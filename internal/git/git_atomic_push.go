// SPDX-License-Identifier: Apache-2.0

package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/revlist"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// getPushSession opens a single receive-pack session for pushing.
//
// go-git v6 replaced v5's transport.NewEndpoint + client.NewClient + NewReceivePackSession with
// transport.ParseURL + client.New(opts...).Handshake. The property PushAtomic depends on is
// unchanged and now explicit in the interface: one Session serves both GetRemoteRefs (the
// advertisement) and Push, so the remote state we validate against is read on the same connection
// we then write to.
func getPushSession(
	ctx context.Context,
	repo *git.Repository,
	auth []gitclient.Option,
) (transport.Session, error) {
	remote, err := repo.Remote("origin")
	if err != nil {
		return nil, fmt.Errorf("failed to get remote: %w", err)
	}

	// go-git's own config validation rejects a remote with no URL, but a hand-edited or truncated
	// .git/config can still present one, and indexing it would panic rather than fail.
	urls := remote.Config().URLs
	if len(urls) == 0 {
		return nil, errors.New("remote origin has no URL configured")
	}

	endpoint, err := transport.ParseURL(urls[0])
	if err != nil {
		return nil, fmt.Errorf("failed to parse remote URL: %w", err)
	}

	session, err := gitclient.New(auth...).Handshake(ctx, &transport.Request{
		URL:     endpoint,
		Command: transport.ReceivePackService,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create receive-pack session: %w", err)
	}

	return session, nil
}

// advertisedHashes indexes a v6 advertisement by reference name. v5 handed back a
// map[string]plumbing.Hash directly; v6's transport.RemoteRefs carries a []*plumbing.Reference so
// that fields can be added without breaking the interface, so the lookup is built here.
func advertisedHashes(refs *transport.RemoteRefs) map[plumbing.ReferenceName]plumbing.Hash {
	out := make(map[plumbing.ReferenceName]plumbing.Hash, len(refs.References))
	for _, ref := range refs.References {
		if ref.Type() == plumbing.HashReference {
			out[ref.Name()] = ref.Hash()
		}
	}
	return out
}

// RemoteMovedError reports a compare-and-swap that cannot proceed: the cycle's root branch is no
// longer at the hash the local commits were based on.
//
// It carries Advertised, which is the hash the push session's OWN advertisement named. That is the
// point of the type. The caller has to know where the remote is before it can replay, and without
// this it learned the same number a second time over the network — a whole SmartFetch, which is
// two more requests to the Git host, for a fact the rejected push already had in hand.
//
// The message is unchanged from the error this replaces, deliberately: it is the string in the
// logs operators have been reading, and the type is the new information, not the wording.
type RemoteMovedError struct {
	// Branch is the cycle's root branch, the one whose advertised hash was compared.
	Branch plumbing.ReferenceName
	// Expected is the hash the local commits were based on (the cycle's root hash).
	Expected plumbing.Hash
	// Advertised is where the remote says that branch is now.
	Advertised plumbing.Hash
	// Missing records that the branch was absent from the advertisement rather than sitting at a
	// different hash. Advertised is then the zero hash, which is the honest answer: there is
	// nothing there.
	//
	// A deleted branch IS the remote moving, and typing it as one is what lets the worker recover
	// from it. Reported as an untyped error, it fell through to the fallback probe — which reads
	// refs/remotes/origin/<branch>, a ref SmartFetch cannot prune because it builds no refspec for
	// a branch the remote no longer has. The probe therefore answered with the branch's old hash,
	// that hash matched the cycle's root, and the worker concluded the remote had not moved. No
	// replay followed, and the retained writes then made every later cycle skip its fetch as well,
	// so the worker pushed the same doomed commits forever.
	Missing bool
}

// RepositoryNotEmptyError reports a push planned against an empty repository that is no longer
// empty: the cycle's root is the zero hash, the write branch is not advertised, and the
// advertisement carries other refs. Creating the branch now would make an orphan beside them, so
// the push is refused and the worker replays onto what discovery finds.
//
// It is its own type rather than a RemoteMovedError, whose Branch and Advertised mean "the root,
// compared": filling them with another branch's hash would make them lie.
type RepositoryNotEmptyError struct {
	// Refs is how many hash refs the advertisement carried.
	Refs int
}

func (e *RepositoryNotEmptyError) Error() string {
	return fmt.Sprintf("the repository is no longer empty: the advertisement carries %d refs", e.Refs)
}

func (e *RemoteMovedError) Error() string {
	if e.Missing {
		// Unchanged from the untyped error this replaces: it is the string in the logs operators
		// have been reading.
		return "remote went missing"
	}
	return "remote received unknown updates"
}

// PushOutcomeKind names which of the three non-error exits a push session took.
type PushOutcomeKind string

const (
	// PushAccepted means the server took the ref update. The outcome's Head is the new tip.
	PushAccepted PushOutcomeKind = "Accepted"
	// PushUpToDate means the advertisement already carried the branch at our head, so nothing
	// was sent. Head is that hash.
	PushUpToDate PushOutcomeKind = "UpToDate"
	// PushNoBranch means the advertisement did not carry the branch and there was nothing local
	// to send. Head is the zero hash, and that is an observation rather than the absence of one:
	// a branch does not exist without a commit.
	PushNoBranch PushOutcomeKind = "NoBranch"
)

// PushOutcome is what the push session learned about the remote.
//
// Every field is read from the ONE connection the push was making anyway, so none of it costs a
// round trip — and a push that returns without an error proves more than a fetch does. A fetch
// says "the branch was at X when I looked"; an accepted push says "it was at Old when I looked,
// and the server has just moved it to New on my authority", with no window between the read and
// the write for anyone to slip through.
//
// The zero value is not a valid outcome: an error return means nothing was observed at all, and
// the caller must invalidate rather than record.
type PushOutcome struct {
	// Kind is which exit was taken.
	Kind PushOutcomeKind
	// Head is where the branch is on the remote now. Zero only for PushNoBranch.
	Head plumbing.Hash
}

// pushPlan is validatePushState's answer: either a packfile to send, or an outcome the
// advertisement alone already settled.
//
// The two cases used to share one return shape — a zero/zero pair meaning "up to date" — which
// conflated "the remote equals our head" with "there is nothing local and nothing on the remote",
// because GetCurrentBranch returns a zero hash for an unborn local branch. Both are observations;
// only one names a revision.
type pushPlan struct {
	// settled is empty when the push has to be sent, and otherwise carries what the
	// advertisement proved.
	settled PushOutcomeKind
	// head is the settled outcome's revision. Meaningless while settled is empty.
	head plumbing.Hash
	// old and new are the compare-and-swap pair for the push command.
	old plumbing.Hash
	new plumbing.Hash
}

// validatePushState checks if the push can proceed based on remote state.
func validatePushState(
	ctx context.Context,
	session transport.Session,
	repo *git.Repository,
	rootHash plumbing.Hash,
	rootBranch plumbing.ReferenceName,
) (pushPlan, error) {
	logger := log.FromContext(ctx)

	branch, localHash, err := GetCurrentBranch(repo)
	if err != nil {
		return pushPlan{}, fmt.Errorf("failed to get current branch: %w", err)
	}

	branchName := branch.Short()

	// Phase 1: Get advertised references (remote state) on this same session.
	remoteRefs, err := session.GetRemoteRefs(ctx, nil)
	if err != nil {
		return pushPlan{}, fmt.Errorf("failed to get advertised references: %w", err)
	}
	refs := advertisedHashes(remoteRefs)

	// Determine the "old" hash for the push command and validate state
	var oldHash = plumbing.ZeroHash
	remoteHash, found := refs[branch]
	if _, rootFound := refs[rootBranch]; !rootFound && !rootHash.IsZero() {
		return pushPlan{}, &RemoteMovedError{
			Branch:     rootBranch,
			Expected:   rootHash,
			Advertised: plumbing.ZeroHash,
			Missing:    true,
		}
	}

	if found && localHash == remoteHash {
		logger.Info("remote already up2date", "branch", branchName, "hash", localHash)
		return pushPlan{settled: PushUpToDate, head: remoteHash}, nil
	}

	if rootHash.IsZero() && !found {
		if n := countHashRefs(refs); n > 0 {
			logger.Info("Planned against an empty repository that has gained refs", "branch", branchName, "refs", n)
			return pushPlan{}, &RepositoryNotEmptyError{Refs: n}
		}
	}

	if err := remoteMovedSinceBase(branch, rootBranch, rootHash, refs); err != nil {
		logger.Info("Remote branch not in expected state", "branch", branchName, "root", rootBranch.Short())
		return pushPlan{}, err
	}

	if found {
		oldHash = remoteHash
	} else if nothingToPublish(branch, localHash, rootBranch, rootHash) {
		// The advertisement still answered the question the caller asked: this branch is not there.
		logger.Info("Nothing to publish; the branch stays absent", "branch", branchName)
		return pushPlan{settled: PushNoBranch}, nil
	}

	// Old stays zero for a new branch, so the server also refuses it if somebody creates it after
	// this advertisement.
	return pushPlan{old: oldHash, new: localHash}, nil
}

// countHashRefs counts the advertised hash refs other than HEAD; see advertisesNoRefs.
func countHashRefs(refs map[plumbing.ReferenceName]plumbing.Hash) int {
	n := 0
	for name := range refs {
		if name != plumbing.HEAD {
			n++
		}
	}
	return n
}

// nothingToPublish reports, for a branch the remote does not carry, that there is nothing to
// create it with: no local commit at all, or a new branch (rooted on another branch, its parent)
// whose local tip is still the parent's, which the caller has just confirmed. Creating it would
// publish nothing but a name, so it stays absent until a write has something to commit.
func nothingToPublish(branch plumbing.ReferenceName, localHash plumbing.Hash,
	rootBranch plumbing.ReferenceName, rootHash plumbing.Hash,
) bool {
	if localHash.IsZero() {
		return true
	}
	return rootBranch != branch && localHash == rootHash
}

// remoteMovedSinceBase reports a remote that is no longer where the local commits were based, or
// nil. Both checks hold whether or not the pushed branch exists, because the case they guard
// hardest is the one where it does not.
func remoteMovedSinceBase(
	branch, rootBranch plumbing.ReferenceName,
	rootHash plumbing.Hash,
	refs map[plumbing.ReferenceName]plumbing.Hash,
) error {
	// Somebody created the branch we believed absent. Pushing with Old = their commit would
	// overwrite it with a commit that does not descend from it, on any server that accepts a
	// non-fast-forward update; the root check below cannot see it, because the root is the parent.
	if remoteHash, found := refs[branch]; found && rootBranch != branch {
		return &RemoteMovedError{Branch: branch, Expected: plumbing.ZeroHash, Advertised: remoteHash}
	}

	// The commits were built on rootHash. For an existing branch that is the branch itself; for a
	// new one it is the parent, which must not have moved either: a branch born on a stale parent
	// is born behind it.
	if advertised := refs[rootBranch]; advertised != rootHash {
		return &RemoteMovedError{Branch: rootBranch, Expected: rootHash, Advertised: advertised}
	}
	return nil
}

// performPush executes the packfile creation and push operation.
func performPush(
	ctx context.Context,
	session transport.Session,
	repo *git.Repository,
	rootHash, localHash, oldHash plumbing.Hash,
	branch plumbing.ReferenceName,
	logger logr.Logger,
) error {
	// Phase 3: Calculate packfile using revlist and push in same session
	// Use revlist.Objects to calculate objects to send
	// Pass localHash as 'ignore' (start) and parentHash as 'limit' (stop)
	var objectsToSend []plumbing.Hash
	var err error
	if rootHash.IsZero() {
		// Creating new branch - send all reachable objects from localHash
		objectsToSend, err = revlist.Objects(repo.Storer, []plumbing.Hash{localHash}, nil)
	} else {
		// Updating existing branch - send objects between parentHash and localHash
		// revlist.Objects(storer, commits to traverse, commits to stop at)
		objectsToSend, err = revlist.Objects(repo.Storer, []plumbing.Hash{localHash}, []plumbing.Hash{rootHash})
	}
	if err != nil {
		return fmt.Errorf("failed to calculate objects using revlist: %w", err)
	}

	logger.Info(
		"Calculated objects to send using revlist",
		"count",
		len(objectsToSend),
		"from",
		rootHash,
		"to",
		localHash,
	)

	// Create packfile
	packfileData, err := createPackfile(repo, objectsToSend)
	if err != nil {
		return fmt.Errorf("failed to create packfile: %w", err)
	}

	// Build the push request. The compare-and-swap that makes this push atomic is unchanged from
	// v5: packp.Command carries the Old hash we expect the ref to be at, and the server refuses the
	// update if it has moved. v6 takes the same *packp.Command type, and negotiates report-status
	// itself in buildUpdateRequests, so the capability no longer has to be set by hand.
	//
	// Atomic asks for the receive-pack `atomic` capability when the server offers it. We send a
	// single command, so it changes nothing today; it is set because the guarantee this function
	// promises is exactly what the capability names, and it becomes load-bearing the moment a
	// second command is added.
	req := &transport.PushRequest{
		Packfile: packfileData,
		Commands: []*packp.Command{{
			Name: branch,
			Old:  oldHash,
			New:  localHash,
		}},
		Atomic: true,
	}

	// Push on the same session the advertisement was read from.
	logger.Info("Sending packfile via receive-pack", "objects", len(objectsToSend))
	if err := session.Push(ctx, repo.Storer, req); err != nil {
		// v6's SendPack decodes report-status and returns the per-command rejection as this error,
		// so a refused compare-and-swap arrives here rather than in a separate status struct.
		logger.Error(err, "push rejected or failed", "ref", branch)
		return fmt.Errorf("push failed for ref %s: %w", branch, err)
	}

	logger.Info("Push successful via single session", "branch", branch.Short(), "from", oldHash, "to", localHash)
	return nil
}

// PushAtomic performs an atomic push in a single network session.
// It checks if the remote branch is not touched before pushing to prevent creating diverged branches.
//
// The returned PushOutcome is an observation of the remote, made on the connection the push was
// opening anyway. An error means nothing was observed; see PushOutcome for what each kind proves.
// afterPushValidation runs between the advertisement check and the upload. Nil in production; a test
// changes the remote here to make the server, not our client, refuse the upload.
//
//nolint:gochecknoglobals // a test seam, like pushAtomicFn
var afterPushValidation func()

func PushAtomic(
	ctx context.Context,
	repo *git.Repository,
	rootHash plumbing.Hash,
	rootBranch plumbing.ReferenceName, // only pushes if this branch is in exact same state, e.g. refs/heads/main (HEAD not allowed since a ReceivePackSession never returns it)
	auth []gitclient.Option,
) (PushOutcome, error) {
	if !rootBranch.IsBranch() {
		return PushOutcome{}, errors.New("rootBranch is not a branch")
	}

	logger := log.FromContext(ctx)

	session, err := getPushSession(ctx, repo, auth)
	if err != nil {
		return PushOutcome{}, err
	}
	defer session.Close()

	plan, err := validatePushState(ctx, session, repo, rootHash, rootBranch)
	if err != nil {
		return PushOutcome{}, err
	}
	if plan.settled != "" {
		return PushOutcome{Kind: plan.settled, Head: plan.head}, nil
	}

	branch, _, err := GetCurrentBranch(repo)
	if err != nil {
		return PushOutcome{}, fmt.Errorf("failed to get current branch: %w", err)
	}

	if afterPushValidation != nil {
		afterPushValidation()
	}
	if err := performPush(ctx, session, repo, rootHash, plan.new, plan.old, branch, logger); err != nil {
		return PushOutcome{}, err
	}
	// The server accepted the ref update, so the branch is at the hash we sent. go-git v6 decodes
	// report-status inside session.Push, so a per-command rejection would have come back as the
	// error above rather than as a silent no-op.
	return PushOutcome{Kind: PushAccepted, Head: plan.new}, nil
}

// createPackfile creates a packfile containing the specified objects using go-git's encoder.
func createPackfile(repo *git.Repository, objects []plumbing.Hash) (io.ReadCloser, error) {
	var buf bytes.Buffer

	encoder := packfile.NewEncoder(&buf, repo.Storer, false)

	// Encode the list of object hashes
	_, err := encoder.Encode(objects, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to encode packfile: %w", err)
	}

	return io.NopCloser(&buf), nil
}
