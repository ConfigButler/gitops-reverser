// SPDX-License-Identifier: Apache-2.0

package git

// This is the measurement docs/design/push-notification-and-reconcile-trigger.md §1.6 asks for, before any of
// the changes that page argues for. Every connection count in §4 of that page is a prediction
// derived from reading code; this file replaces reading with observing.
//
// The unit is the CONNECTION to the Git host, because that is the scarce resource: a round trip to
// GitHub costs a noticeable fraction of a second before it has transferred anything, and it is in
// front of a person waiting for their edit to land. Local recomputation is not in the same
// category and is not measured here.
//
// Two harness facts make the numbers trustworthy:
//
//   - The remote is canonical git's `http-backend` over real HTTP (startRealGitServer), not
//     `file://`. go-git v6's in-process receive-pack never compares cmd.Old, so a compare-and-swap
//     over `file://` is enforced only by our own client-side check and a rejection cannot be
//     provoked at all. Rows 6 and 7 would be fiction on that transport.
//   - Everything that sets a fixture up — seeding the remote, an out-of-band contending push —
//     goes through the bare repository ON DISK, never over HTTP, so it contributes nothing to the
//     ledger. Each row is a delta across the operation alone.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
)

// ledgerGoldenPath is the checked-in cost table. A change that adds a fetch shows up here as a
// number moving in a review diff, which is the whole reason the file exists rather than a pile of
// assertions.
const ledgerGoldenPath = "testdata/git-roundtrip-ledger.golden"

// ledgerFixture is one operation's world: a real git server over HTTP, the bare repository behind
// it (reachable on disk for out-of-band moves), and a BranchWorker pointed at it.
type ledgerFixture struct {
	t       *testing.T
	sim     *adoSimulator
	repoDir string
	worker  *BranchWorker
	// pending is the retained-write slice the event loop would own. The ops drive commit and push
	// directly, so they carry it here.
	pending []PendingWrite
}

// newLedgerFixture builds that world. seeded chooses between an empty remote and one that already
// has a commit on main, which is the difference between ledger rows 1 and 2.
func newLedgerFixture(t *testing.T, slug string, seeded bool) *ledgerFixture {
	t.Helper()

	projectRoot := t.TempDir()
	repoDir := filepath.Join(projectRoot, "repo.git")
	createBareRepo(t, repoDir)

	// http-backend refuses receive-pack unless the repository opts in.
	out, err := exec.Command("git", "-C", repoDir, "config", "http.receivepack", "true").CombinedOutput()
	require.NoError(t, err, "git config http.receivepack: %s", out)

	if seeded {
		simulateClientCommitOnDisk(t, repoDir, "main", "README.md", "seed\n")
	}

	sim := startRealGitServer(t, projectRoot, repoDir)

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha3.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	ctx := context.Background()
	// A per-operation provider name keeps each row's on-disk clone in its own tree. The worker's
	// repo root is a fixed /tmp path keyed by provider and branch, not a t.TempDir, so two rows
	// sharing a name would share a checkout.
	providerName := "ledger-" + slug
	provider := &configv1alpha3.GitProvider{
		Spec: configv1alpha3.GitProviderSpec{URL: sim.RepoURL},
	}
	provider.Name = providerName
	provider.Namespace = "default"
	require.NoError(t, k8sClient.Create(ctx, provider))

	worker := NewBranchWorker(k8sClient, logr.Discard(), providerName, "default", "main", nil, BranchWorkerLimits{})
	worker.ctx = ctx
	t.Cleanup(func() { _ = os.RemoveAll(worker.repoRootPath()) })

	return &ledgerFixture{t: t, sim: sim, repoDir: repoDir, worker: worker}
}

// mark reads the ledger. Every row is the delta between a mark and the reading after the
// operation, so setup traffic is never attributed to the thing under test.
func (f *ledgerFixture) mark() gitRequestSnapshot { return f.sim.ledger.snapshot() }

// commit runs one local commit for the named ConfigMaps and retains its pending write.
// hasPendingCommits is the flag commitPendingWrites gates its head-of-cycle fetch on: false means
// "first commit of a cycle".
func (f *ledgerFixture) commit(hasPendingCommits bool, names ...string) {
	f.t.Helper()
	events := make([]Event, 0, len(names))
	for _, name := range names {
		events = append(events, configMapEvent(name, "alice", "team-a"))
	}
	pendingWrite, err := f.worker.buildGroupedPendingWrite(f.worker.ctx, events)
	require.NoError(f.t, err)
	require.NoError(f.t, f.worker.commitPendingWrites([]PendingWrite{*pendingWrite}, hasPendingCommits))
	f.pending = append(f.pending, *pendingWrite)
}

// push publishes whatever is retained and clears it, as the event loop does on success.
func (f *ledgerFixture) push() {
	f.t.Helper()
	require.NoError(f.t, f.worker.pushPendingCommits(f.pending))
	f.pending = nil
}

// publish is the whole steady-state cycle: one commit, then the push.
func (f *ledgerFixture) publish(names ...string) {
	f.t.Helper()
	f.commit(false, names...)
	f.push()
}

// contend moves the remote branch out from under us, the way another writer would. It goes
// through the bare repository on disk, so the contention itself costs the ledger nothing and the
// row measures only what OUR worker spent discovering it.
func (f *ledgerFixture) contend(file, content string) {
	f.t.Helper()
	simulateClientCommitOnDisk(f.t, f.repoDir, "main", file, content)
}

// createLedgerTarget declares the GitTarget the operations that need one write through. There is
// only ever one: these rows are about the cost of talking to the remote, and a second target on
// the same branch shares the same worker and the same connections.
const ledgerTargetName = "target-a"

func (f *ledgerFixture) createLedgerTarget(path string, prune *configv1alpha3.PrunePolicy) {
	f.t.Helper()
	require.NoError(f.t, f.worker.Client.Create(f.worker.ctx, &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: ledgerTargetName, Namespace: "default"},
		Spec: configv1alpha3.GitTargetSpec{
			GitProviderRef: meta.LocalObjectReference{Name: f.worker.GitProviderRef},
			Branch:         f.worker.Branch,
			Path:           path,
			Prune:          prune,
		},
	}))
}

// ledgerOp is one row of §4.1's table.
type ledgerOp struct {
	// name is the row label, and must match the golden file.
	name string
	// slug keys the on-disk clone.
	slug string
	// seeded starts the remote with a commit on main.
	seeded bool
	// prime puts the fixture in the state the row describes. It runs BEFORE the measurement
	// starts, so a row about steady-state publication is not charged for the initial clone.
	prime func(f *ledgerFixture)
	// run is the operation being measured.
	run func(f *ledgerFixture)
}

func ledgerOperations() []ledgerOp {
	return []ledgerOp{
		{
			name:   "1. bootstrap helper, empty remote",
			slug:   "start-empty",
			seeded: false,
			run: func(f *ledgerFixture) {
				f.createLedgerTarget("team-a", nil)
				require.NoError(f.t, f.worker.EnsurePathBootstrapped("team-a", "target-a", "default"))
			},
		},
		{
			name:   "2. bootstrap helper, populated remote",
			slug:   "start-populated",
			seeded: true,
			run: func(f *ledgerFixture) {
				f.createLedgerTarget("team-a", nil)
				require.NoError(f.t, f.worker.EnsurePathBootstrapped("team-a", "target-a", "default"))
			},
		},
		{
			name:   "3. one publication, one commit",
			slug:   "publish-one",
			seeded: true,
			prime:  func(f *ledgerFixture) { f.publish("prime") },
			run:    func(f *ledgerFixture) { f.publish("first") },
		},
		{
			name:   "4. one publication, several commits in one cycle",
			slug:   "publish-many",
			seeded: true,
			prime:  func(f *ledgerFixture) { f.publish("prime") },
			run: func(f *ledgerFixture) {
				// Only the first commit of a cycle may fetch: that is the claim this row checks.
				f.commit(false, "first")
				f.commit(true, "second")
				f.commit(true, "third")
				f.push()
			},
		},
		{
			name:   "5. a publication that commits nothing",
			slug:   "publish-noop",
			seeded: true,
			prime:  func(f *ledgerFixture) { f.publish("same") },
			run: func(f *ledgerFixture) {
				// The identical object plans to no change, so commitPendingWrites creates no
				// commit — and the write is still retained and still reaches PushAtomic, which is
				// the property §2 rests on.
				f.commit(false, "same")
				require.True(f.t, f.pending[0].CommitSHA.IsZero(),
					"the no-diff write must be retained with no commit of its own")
				f.push()
			},
		},
		{
			name:   "6. a publication whose push is rejected once, then succeeds",
			slug:   "rejected-once",
			seeded: true,
			prime:  func(f *ledgerFixture) { f.publish("prime") },
			run: func(f *ledgerFixture) {
				f.commit(false, "mine")
				f.contend("OUTSIDE.md", "from-another-writer\n")
				f.push()
			},
		},
		{
			name:   "7. a publication rejected twice",
			slug:   "rejected-twice",
			seeded: true,
			prime:  func(f *ledgerFixture) { f.publish("prime") },
			run: func(f *ledgerFixture) {
				f.commit(false, "mine")
				f.contend("OUTSIDE-1.md", "first-other-writer\n")
				// Move the remote again just before the SECOND push attempt, so the replay is
				// rejected too. Hooking the push is the only way to land a write inside the retry
				// loop; the contending commit itself is made on disk and costs the ledger nothing.
				restore := interceptPushAttempt(f.t, 2, func() {
					f.contend("OUTSIDE-2.md", "second-other-writer\n")
				})
				defer restore()
				f.push()
			},
		},
		{
			name:   "8. forced recheck, no retained writes",
			slug:   "recheck-clean",
			seeded: true,
			prime:  func(f *ledgerFixture) { f.publish("prime") },
			run: func(f *ledgerFixture) {
				err := f.worker.syncWithRemote(f.worker.ctx, fetchReasonForcedRecheck)
				require.NoError(f.t, err)
			},
		},
		{
			name:   "9. forced recheck, with retained writes",
			slug:   "recheck-retained",
			seeded: true,
			prime: func(f *ledgerFixture) {
				f.publish("prime")
				f.commit(false, "retained")
			},
			run: func(f *ledgerFixture) {
				require.NoError(f.t,
					f.worker.refreshRemoteAndRebuildPendingWrites(f.worker.ctx, f.pending, fetchReasonForcedRecheck))
			},
		},
		{
			name:   "10. a resync (snapshot)",
			slug:   "resync",
			seeded: true,
			prime: func(f *ledgerFixture) {
				f.worker.mapper = configMapMapper()
				f.createLedgerTarget("live", &configv1alpha3.PrunePolicy{
					Mode: configv1alpha3.PruneAlways,
				})
				f.publish("prime")
			},
			run: func(f *ledgerFixture) {
				loop := newBranchWorkerEventLoop(f.worker, 0)
				defer loop.stopTimers()
				req := &ResyncRequest{
					Desired:            []manifestanalyzer.DesiredResource{desiredCM("keep", "blue")},
					ResourceVersion:    "42",
					GitTargetName:      "target-a",
					GitTargetNamespace: "default",
					Result:             make(chan ResyncResult, 1),
				}
				// handleQueueItem commits AND pushes: applyResync ends in maybeSchedulePush,
				// and a worker that has not pushed before has no cooldown to wait out. So this
				// one call is the whole resync cycle, which is what the row is meant to cost.
				loop.handleQueueItem(WorkItem{Resync: req})
				result := <-req.Result
				require.NoError(f.t, result.Err)
				require.Equal(f.t, 1, result.Stats.Created, "the resync must have written something")
				require.Empty(f.t, loop.pendingWrites, "the resync's commit must have reached the remote")
			},
		},
		{
			// The remote has no branch of ours to fetch, and cannot grow one while we are the
			// only writer, so a second cycle must cost exactly what a normal one does. This row
			// is the measurement behind "an absent branch is trusted right away": before that
			// change every cycle here paid for a fetch that could only re-learn the absence.
			name:   "11. publication onto a branch the remote does not have",
			slug:   "publish-absent-branch",
			seeded: false,
			prime:  func(f *ledgerFixture) { f.publish("prime") },
			run:    func(f *ledgerFixture) { f.publish("second") },
		},
		{
			name:   "12. an idle target held across several commit windows",
			slug:   "idle",
			seeded: true,
			prime:  func(f *ledgerFixture) { f.publish("prime") },
			run: func(f *ledgerFixture) {
				// The real event loop, with a short window, running over nothing. An idle target
				// must be silent, and this row asserts it strictly rather than golden-matching it.
				ctx, cancel := context.WithCancel(context.Background())
				f.worker.ctx = ctx
				loop := newBranchWorkerEventLoop(f.worker, idleLedgerCommitWindow)
				done := make(chan struct{})
				go func() {
					defer close(done)
					loop.run()
				}()
				time.Sleep(idleLedgerCommitWindow * idleLedgerWindows)
				cancel()
				<-done
			},
		},
	}
}

const (
	// idleLedgerCommitWindow is short so "several commit windows" is several, not a wall-clock
	// wait. The row asserts zero, so a window that elapses early can only make the test weaker in
	// coverage, never flaky in outcome.
	idleLedgerCommitWindow = 20 * time.Millisecond
	idleLedgerWindows      = 15
)

// interceptPushAttempt runs before the nth call to pushAtomicFn and then restores it. It exists
// for the twice-rejected row: the second rejection has to be provoked from inside runPushCycle's
// retry loop, where a test has no other seam.
func interceptPushAttempt(t *testing.T, attempt int, before func()) func() {
	t.Helper()
	original := pushAtomicFn
	calls := 0
	pushAtomicFn = func(
		ctx context.Context,
		repo *gogit.Repository,
		rootHash plumbing.Hash,
		rootBranch plumbing.ReferenceName,
		auth []gitclient.Option,
	) error {
		calls++
		if calls == attempt {
			before()
		}
		return original(ctx, repo, rootHash, rootBranch, auth)
	}
	return func() { pushAtomicFn = original }
}

// TestGitRoundTripLedger measures every operation in §4.1's table and compares the result with the
// checked-in golden file.
//
// It is deliberately ONE test with eleven subtests rather than eleven tests: the golden file is a
// single table, and writing it from a partial run would silently drop rows.
func TestGitRoundTripLedger(t *testing.T) {
	// Resolve the backend up front so a machine without it skips the WHOLE table.
	//
	// Per-row skipping would be worse than useless here: t.Run reports a skipped subtest as
	// success, so every row would be recorded at zero and the golden comparison would fail with
	// a table of zeros — or, run with -update, would quietly overwrite the real measurements
	// with them.
	gitHTTPBackend(t)

	ops := ledgerOperations()
	rows := make([]ledgerRow, 0, len(ops))

	for _, op := range ops {
		row := ledgerRow{Operation: op.name}
		ok := t.Run(op.name, func(t *testing.T) {
			f := newLedgerFixture(t, op.slug, op.seeded)
			if op.prime != nil {
				op.prime(f)
			}

			base := f.mark()
			op.run(f)
			measured := f.mark().since(base)

			// The exact bytes are logged and not asserted: they are the number a human wants when
			// a row surprises them, and the number a golden file cannot hold. See sizeClass.
			t.Logf("%s: %d connections | %s", op.name, measured.connections(), measured.exactBytes())
			row.Snapshot = measured
		})
		if !ok {
			t.Fatalf("%s failed; the golden table would be incomplete", op.name)
		}
		rows = append(rows, row)
	}

	// A harness that measures nothing measures nothing consistently, and a golden file is happy
	// to record that. Row 12 is meant to be zero; everything else is not.
	for _, row := range rows[:len(rows)-1] {
		require.Positive(t, row.Snapshot.connections(),
			"%s recorded no traffic at all, so the harness — not the code — is what this row is "+
				"describing", row.Operation)
	}

	// Row 12 is a property, not a measurement: an idle target is silent. It is asserted here
	// directly so it can never be "accepted" by regenerating the golden file.
	idle := rows[len(rows)-1]
	require.Equal(t, "12. an idle target held across several commit windows", idle.Operation)
	require.Zero(t, idle.Snapshot.connections(),
		"an idle target must not talk to the Git host at all; it opened %s", idle.Snapshot.exactBytes())

	assertLedgerGolden(t, rows)
}

// ledgerRow pairs an operation with what it cost.
type ledgerRow struct {
	Operation string
	Snapshot  gitRequestSnapshot
}

// renderLedger formats the whole table. The format is chosen for a review diff: fixed columns, one
// line per operation, and the total connections first because that is the number under discussion.
func renderLedger(rows []ledgerRow) string {
	var b strings.Builder
	b.WriteString("# Round trips to the Git host, per operation.\n")
	b.WriteString("# Generated by TestGitRoundTripLedger; specified by\n")
	b.WriteString("# docs/design/push-notification-and-reconcile-trigger.md §1.6.\n")
	b.WriteString("#\n")
	b.WriteString("# Regenerate with:  go test ./internal/git -run TestGitRoundTripLedger -update\n")
	b.WriteString("#\n")
	b.WriteString("# conn is the total number of requests to the Git host, which is the cost this\n")
	b.WriteString("# design optimizes. Each request column reads  <count> <request>/<response>,\n")
	b.WriteString("# where the two sizes are classes rather than byte counts: a packfile is\n")
	b.WriteString("# compressed content carrying timestamps and hashes, so its exact length is not\n")
	b.WriteString("# reproducible, while \"did this conversation move a tree\" is. Run the test with\n")
	b.WriteString("# -v for the exact bytes.\n")
	b.WriteString("#\n")
	b.WriteString("# fetch-open = GET /info/refs?service=git-upload-pack   (a fetch conversation)\n")
	b.WriteString("# fetch-xfer = POST /git-upload-pack                    (objects requested)\n")
	b.WriteString("# push-open  = GET /info/refs?service=git-receive-pack  (the CAS advertisement)\n")
	b.WriteString("# push-xfer  = POST /git-receive-pack                   (a packfile sent)\n")
	b.WriteString("\n")

	const opWidth = 58
	fmt.Fprintf(&b, "%-*s  %4s  %-12s  %-12s  %-12s  %-12s\n",
		opWidth, "operation", "conn", "fetch-open", "fetch-xfer", "push-open", "push-xfer")
	b.WriteString(strings.Repeat("-", opWidth+4+4*14+8) + "\n")

	for _, row := range rows {
		fmt.Fprintf(&b, "%-*s  %4d  %-12s  %-12s  %-12s  %-12s\n",
			opWidth, row.Operation, row.Snapshot.connections(),
			ledgerCell(row.Snapshot[infoRefsUploadPack]),
			ledgerCell(row.Snapshot[uploadPackPost]),
			ledgerCell(row.Snapshot[infoRefsReceivePack]),
			ledgerCell(row.Snapshot[receivePackPost]),
		)
	}
	return b.String()
}

// ledgerCell renders one request kind: how many, and how much moved each way.
func ledgerCell(stat gitRequestStat) string {
	if stat.Count == 0 {
		return "0"
	}
	return fmt.Sprintf("%d %s/%s", stat.Count, sizeClass(stat.ReqBytes), sizeClass(stat.RespBytes))
}

// assertLedgerGolden compares the measured table with the checked-in one, or rewrites it under
// -update. The flag is the same one the layout corpus uses, so one `-update` run regenerates every
// golden in this package.
func assertLedgerGolden(t *testing.T, rows []ledgerRow) {
	t.Helper()

	got := renderLedger(rows)
	if *updateGoldens {
		require.NoError(t, os.MkdirAll(filepath.Dir(ledgerGoldenPath), 0o750))
		require.NoError(t, os.WriteFile(ledgerGoldenPath, []byte(got), 0o600))
		return
	}

	want, err := os.ReadFile(ledgerGoldenPath)
	require.NoError(t, err, "read %s (run with -update to create it)", ledgerGoldenPath)
	require.Equal(t, string(want), got,
		"the cost of talking to the Git host changed.\n"+
			"If that is the intended effect of this change, re-run with -update and put the diff in "+
			"the review: a fetch appearing or disappearing is exactly what %s exists to show.",
		ledgerGoldenPath)
}
