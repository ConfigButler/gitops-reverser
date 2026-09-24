// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// DefaultBranchBufferMaxBytes is the default cap on a worker's combined event
// buffer + unpushed-events memory. Operators override this via
// --branch-buffer-max-size (8Mi by default).
const DefaultBranchBufferMaxBytes int64 = 8 * 1024 * 1024

// DefaultBranchWorkerQueueDepth is the default depth of each branch worker's event
// queue. Operators override it via --branch-worker-queue-depth.
//
// It is sized so that a bounded administrative burst cannot overrun it, because a burst
// that stays under the queue depth cannot drop at ALL, whatever its arrival shape. The
// sizing input is not an event rate: a slot holds one WriteRequest, so what must fit is
// the number of concurrent write requests a burst can produce, roughly
// (concurrent writers) x (GitTargets sharing this branch worker). Note the second factor:
// workers are keyed by (provider namespace, provider name, branch), so pointing another
// GitTarget at a branch divides the per-target headroom of a worker already in use.
//
// 1000 replaces an earlier 100, which a realistic workload cleared and a scripted one did
// not: a 200-participant demo across two GitTargets peaked at depth 44, and then deleting
// those 203 objects unpaced -- hundreds of requests per second rather than the ballots'
// ~3/s -- dropped 20 writes. Convergence still healed the mirror, but a dropped write is a
// LIVE, ATTRIBUTED commit that never happened, so the history is missing something the
// end state cannot show.
const DefaultBranchWorkerQueueDepth = 1000

// BranchWorkerLimits bounds one branch worker's memory. The two knobs cover different
// stages of the same pipeline and neither substitutes for the other, which is why they
// travel together.
type BranchWorkerLimits struct {
	// MaxBufferBytes caps totalRetainedBytes: the open commit window plus the writes
	// committed locally and retained for replay until a push succeeds. Tripping it
	// finalizes the window early, ignoring the commit cadence.
	MaxBufferBytes int64

	// QueueDepth is the depth of the event queue, and it is a HARD drop boundary: the
	// enqueue is deliberately non-blocking, so a full queue throws the item away and
	// counts git_queue_drops_total rather than stalling the watch path behind a slow
	// remote.
	//
	// MaxBufferBytes does NOT cover this queue. That cap is accounted only once the loop
	// DEQUEUES an item, so whatever is still on the channel is bounded by count alone and
	// costs memory ON TOP of it. The channel itself is trivial (a WorkItem is three
	// pointers); what it retains is not, because each live-event item holds a sanitized
	// object as an unstructured map, measured at roughly SIX times the bytes it serializes
	// to. Budget QueueDepth x serialized size x ~6 per SATURATED worker: ~5MiB at the
	// default depth for ~800-byte resources, but the multiplier rides the payload, so a
	// folder mirroring megabyte-sized ConfigMaps reaches gigabytes at the same depth.
	QueueDepth int
}

// withDefaults fills in the zero value of each knob, so a caller that cares about one
// need not restate the other.
func (l BranchWorkerLimits) withDefaults() BranchWorkerLimits {
	if l.MaxBufferBytes <= 0 {
		l.MaxBufferBytes = DefaultBranchBufferMaxBytes
	}
	if l.QueueDepth <= 0 {
		l.QueueDepth = DefaultBranchWorkerQueueDepth
	}
	return l
}

// WorkerManager manages BranchWorkers.
// Creates workers per (repo, branch), shared by multiple GitDestinations.
// Implements controller-runtime's Runnable interface for lifecycle management.
type WorkerManager struct {
	Client client.Client
	Log    logr.Logger

	limits             BranchWorkerLimits
	sensitiveResources types.SensitiveResourcePolicy

	mu sync.RWMutex
	// lifecycleMu serialises creating and stopping workers, so one BranchKey never has two live
	// workers: they share an on-disk clone keyed by remote URL. It is separate from mu so a
	// shutdown never blocks readers of the map, one of which is the queue-depth gauge source on
	// the scrape goroutine. Take it BEFORE mu, never the other way round.
	lifecycleMu sync.Mutex
	workers     map[BranchKey]*BranchWorker
	ctx         context.Context
	// mapper is the GVK->GVR resolver injected into every worker so store scans build a
	// resource-identity inventory. It is set once at startup (SetMapper) before any
	// worker is created; a nil mapper keeps workers structure-only. It is the LOCAL cluster's
	// resolver and the fallback when a GitTarget names no source cluster.
	mapper typeset.Lookup
	// clusterMapper resolves the GVK->GVR lookup for a NAMED source cluster, so a worker
	// serving a GitTarget that mirrors a remote resolves that folder against the remote's
	// registry — never a union. Set once at startup (SetClusterMapper); nil in the CLI and
	// in tests, which fall back to `mapper`.
	clusterMapper func(clusterID string) typeset.Lookup

	// sshHostKeys configures SSH host-key resolution for every worker's credential reads. Set
	// once at startup (SetSSHHostKeyConfig) before any worker is created.
	sshHostKeys SSHHostKeyConfig
	// credentialPolicy controls explicit insecure opt-ins for Git credential transports. Set
	// once at startup (SetCredentialTransportPolicy) before any worker is created.
	credentialPolicy CredentialTransportPolicy

	// pathRefusal reports a refused live write plan to the GitTarget status surface. Set
	// once at startup (SetPathRefusalReporter) before any worker is created; nil in the
	// CLI and in tests that do not assert on the status transition.
	pathRefusal PathRefusalReporter

	// layoutReporter publishes each scan's resolved folder layout to the GitTarget status
	// surface. Set once at startup (SetLayoutReporter) before any worker is created; nil in the
	// CLI and in tests that do not assert on status.placement.
	layoutReporter LayoutReporter

	// remoteReporter publishes each confirmed observation of a branch's remote state to the
	// GitTarget status surface. Set once at startup (SetRemoteReporter) before any worker is
	// created; nil in the CLI and in tests that do not assert on status.remote.
	remoteReporter RemoteReporter

	// scanAcceptance publishes a read-only folder scan's verdict to the GitTarget status surface.
	// Set once at startup (SetScanAcceptanceReporter) before any worker is created; nil in the CLI
	// and in tests that do not assert on GitPathAccepted.
	scanAcceptance ScanAcceptanceReporter

	// renderFidelityGate is shared by every worker and the watch manager. It is created with the
	// manager so a target's state survives workers being recreated for the same branch.
	renderFidelityGate *RenderFidelityGate
}

// NewWorkerManager creates a new worker manager. limits bounds every worker this manager
// creates; a zero value of either knob takes that knob's default.
func NewWorkerManager(
	client client.Client,
	log logr.Logger,
	limits BranchWorkerLimits,
	sensitiveResources types.SensitiveResourcePolicy,
) *WorkerManager {
	return &WorkerManager{
		Client:             client,
		Log:                log,
		limits:             limits.withDefaults(),
		sensitiveResources: sensitiveResources,
		workers:            make(map[BranchKey]*BranchWorker),
		renderFidelityGate: NewRenderFidelityGate(),
	}
}

// RenderFidelityGate returns the manager-wide target gate used by branch workers. The gate is
// safe for concurrent watch and worker access.
func (m *WorkerManager) RenderFidelityGate() *RenderFidelityGate {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.renderFidelityGate
}

// SetMapper injects the GVK->GVR resolver used by every worker's store scan. It is
// called once at startup, before any GitTarget registers a worker, so each worker
// created by EnsureWorker carries it.
func (m *WorkerManager) SetMapper(mapper typeset.Lookup) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mapper = mapper
}

// SetClusterMapper injects the per-source-cluster GVK->GVR resolver used by every worker's
// store scan when a GitTarget names a source cluster. Like SetMapper, it is called once at
// startup before any worker is created, so each worker created by EnsureWorker carries it.
func (m *WorkerManager) SetClusterMapper(resolver func(clusterID string) typeset.Lookup) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clusterMapper = resolver
}

// SetSSHHostKeyConfig injects the SSH host-key resolution config used by every worker's credential
// reads. Like SetMapper, it is called once at startup before any worker is created.
func (m *WorkerManager) SetSSHHostKeyConfig(cfg SSHHostKeyConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sshHostKeys = cfg
}

// SetCredentialTransportPolicy injects Git credential transport policy into every worker.
func (m *WorkerManager) SetCredentialTransportPolicy(policy CredentialTransportPolicy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.credentialPolicy = policy
}

// SetPathRefusalReporter injects the hook every worker calls when a live write plan is
// refused, so the refusal reaches GitTarget status instead of being logged and dropped. Like
// SetMapper, it is called once at startup before any worker is created.
func (m *WorkerManager) SetPathRefusalReporter(reporter PathRefusalReporter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pathRefusal = reporter
}

// SetLayoutReporter injects the hook every worker calls after a scan resolves a target's
// folder layout, so status.placement and LayoutResolved reflect the folder rather than being
// computed and dropped. Like SetPathRefusalReporter, it is called once at startup before any
// worker is created.
func (m *WorkerManager) SetLayoutReporter(reporter LayoutReporter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.layoutReporter = reporter
}

// SetScanAcceptanceReporter injects the hook the refresher calls after it re-reads a target's
// folder, so a structural refusal that arrived in Git surfaces before anything tries to write it.
// Like SetLayoutReporter, it is called once at startup before any worker is created.
func (m *WorkerManager) SetScanAcceptanceReporter(reporter ScanAcceptanceReporter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scanAcceptance = reporter
}

// SetRemoteReporter injects the hook every worker calls after it proves where its branch is on
// the remote, so status.remote reflects the last confirmed look rather than being learned and
// dropped. Like SetLayoutReporter, it is called once at startup before any worker is created.
func (m *WorkerManager) SetRemoteReporter(reporter RemoteReporter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.remoteReporter = reporter
}

// EnsureWorker ensures a worker exists for the given (provider, branch), and that it is the
// worker for the repository the GitProvider names NOW.
//
// repo is the caller's answer to "which repository is this"; the reconcile has already read the
// GitProvider one gate earlier, so it costs no API call. A slot holding a worker for a DIFFERENT
// repository is not corrected field by field — the clone, the base trust, the observation and the
// retained writes are all statements about the old one — so that worker is stopped, its checkout
// reclaimed, and a fresh one takes the slot.
//
// It reports whether it REPLACED a worker, because a replacement is not complete when it returns:
// the new worker has no clone and no queue, so every GitTarget on that branch has to re-establish
// its folder against the new repository. Arranging that is the caller's job; see the GitTarget
// reconcile's worker wiring gate.
//
// Worker creation/start is protected by the manager lock.
func (m *WorkerManager) EnsureWorker(
	_ context.Context,
	providerName, providerNamespace string,
	branch string,
	repo RepoIdentity,
) (bool, error) {
	// Held across the whole check-and-create so a replacement cannot start while the worker it
	// replaces is still stopping; they would share a clone.
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	key := BranchKey{
		RepoNamespace: providerNamespace,
		RepoName:      providerName,
		Branch:        branch,
	}

	m.mu.RLock()
	existing, exists := m.workers[key]
	m.mu.RUnlock()

	replaced := false
	if exists {
		if existing.repo == repo {
			return false, nil
		}
		// Both identities, at default verbosity: a comparison that is not stable across steady
		// reconciles is a restart loop, and this line is what makes one visible.
		m.Log.Info("GitProvider now names a different repository; replacing the branch worker",
			"key", key.String(),
			"was", existing.repo.String(),
			"now", repo.String())
		// lifecycleMu is already held, so the detach-and-stop must not take it again.
		m.removeWorkersLocked([]BranchKey{key}, "the GitProvider names a different repository")
		replaced = true
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, live := m.workers[key]; !live {
		m.Log.Info("Creating new branch worker", "key", key.String(), "repository", repo.String())
		worker := NewBranchWorker(
			m.Client,
			m.Log.WithName("branch-worker"),
			providerName,
			providerNamespace,
			branch,
			repo,
			newContentWriter(m.sensitiveResources),
			m.limits,
		)
		// Inject the resolver before Start: the field is read only by the event-loop
		// goroutine Start spawns, so setting it here (under m.mu, before that goroutine
		// exists) is race-free.
		worker.mapper = m.mapper
		worker.clusterMapper = m.clusterMapper
		worker.sshHostKeys = m.sshHostKeys
		worker.credentialPolicy = m.credentialPolicy
		worker.pathRefusal = m.pathRefusal
		worker.layoutReporter = m.layoutReporter
		worker.remoteReporter = m.remoteReporter
		worker.scanAcceptance = m.scanAcceptance
		worker.renderFidelityGate = m.renderFidelityGate

		if err := worker.Start(m.ctx); err != nil {
			return replaced, fmt.Errorf("failed to start worker for %s: %w", key.String(), err)
		}

		m.workers[key] = worker
	}

	return replaced, nil
}

// removeWorkers detaches the named workers and stops them.
//
// The lock discipline is the whole reason this is one function rather than a loop at each call
// site. lifecycleMu is held across the Stops so EnsureWorker cannot start a replacement while the
// worker it replaces is still draining — they would share an on-disk clone. m.mu is held only to
// detach: queueDepthSamples reads it on the metric SDK's collection goroutine, so holding THAT
// across Stop(), which waits for the loop goroutine, stalls collection for the whole shutdown.
//
// A key that names no worker is skipped, so a caller may pass a key it is not sure about.
func (m *WorkerManager) removeWorkers(keys []BranchKey, reason string) {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	m.removeWorkersLocked(keys, reason)
}

// removeWorkersLocked is removeWorkers for a caller that already holds lifecycleMu: a sweep holding
// it across choosing the workers to retire and retiring them, and EnsureWorker holding it across
// its whole check-and-create, where a second acquisition would deadlock.
func (m *WorkerManager) removeWorkersLocked(keys []BranchKey, reason string) {
	if len(keys) == 0 {
		return
	}

	m.mu.Lock()
	detached := make(map[BranchKey]*BranchWorker, len(keys))
	for _, key := range keys {
		if worker, exists := m.workers[key]; exists {
			detached[key] = worker
			delete(m.workers, key)
		}
	}
	m.mu.Unlock()

	for key, worker := range detached {
		m.Log.Info("Stopping branch worker", "key", key.String(), "reason", reason)
		worker.Stop()
		// After Stop, never before: Stop waits for the loop goroutine, so until it returns the
		// worktree can still be written. A worker being retired takes its clone with it — the
		// goroutine was only half the leak, and the checkout is the half that survives a
		// restart.
		worker.removeLocalState()
	}
}

// GetWorkerForTarget finds the worker for a target's (provider, branch).
// Returns the worker and true if found, nil and false otherwise.
// This is used by EventRouter to dispatch events to the correct worker.
func (m *WorkerManager) GetWorkerForTarget(
	providerName, providerNamespace string,
	branch string,
) (*BranchWorker, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	key := BranchKey{
		RepoNamespace: providerNamespace,
		RepoName:      providerName,
		Branch:        branch,
	}

	worker, exists := m.workers[key]
	return worker, exists
}

// afterOrphanSelection runs between choosing the workers to retire and retiring them. It is nil in
// production and exists because that gap is the ONLY place the lock discipline in ReconcileWorkers
// can be observed: with lifecycleMu held across both steps a concurrent EnsureWorker blocks here,
// and without it that EnsureWorker hands a caller the very worker about to be stopped. The seam
// follows pushAtomicFn, which the push path already uses for the same reason.
//
//nolint:gochecknoglobals // a test seam, like pushAtomicFn
var afterOrphanSelection func()

// ReconcileWorkers stops every worker no live GitTarget still needs.
//
// It is the ONLY thing that removes a worker before shutdown, and it is driven from the GitTarget
// reconcile's deleted-object path: that reconcile is what the delete watch event produces, so the
// sweep runs exactly when the set of needed workers can have shrunk. A worker that outlives its
// last GitTarget is not free — it holds a goroutine, an event queue and an on-disk clone until the
// process restarts.
//
// It decides from the API rather than from a count of registrations, which is what makes it
// correct for a branch SHARED by several GitTargets: deleting one of them leaves the others
// listed, so the worker they share is still needed and is left alone.
//
// It fails safe. A List that errors stops nothing: the alternative — treating "I could not read
// the targets" as "there are no targets" — would take down every live worker in the process.
func (m *WorkerManager) ReconcileWorkers(ctx context.Context) error {
	// lifecycleMu is held across the whole decision — the List, the selection and the removal —
	// and the three must not be separated.
	//
	// EnsureWorker takes this lock, finds the key present and returns "already there" without
	// creating anything, so a selection made outside it would let this sweep stop a worker a
	// GitTarget had just been told it has. The LIST is inside for the same reason read the other
	// way round: a target created after the snapshot is taken is absent from it, and its worker —
	// created under this lock moments later — then looks like an orphan and is stopped with its
	// queue and its checkout. The client is cached, so the read costs no round trip.
	//
	// m.mu is still taken only for the map itself, and the Stops still run outside it, for the
	// reason in removeWorkers.
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	// Get all GitTargets
	var targetList configv1alpha3.GitTargetList
	if err := m.Client.List(ctx, &targetList); err != nil {
		return fmt.Errorf("failed to list GitTargets: %w", err)
	}

	// Build set of needed workers from active GitTargets
	neededWorkers := make(map[BranchKey]bool)
	for _, target := range targetList.Items {
		// Skip deleted targets
		if !target.DeletionTimestamp.IsZero() {
			continue
		}

		// Determine namespace (Provider is always in same namespace as Target)
		providerNS := target.Namespace

		key := BranchKey{
			RepoNamespace: providerNS,
			RepoName:      target.Spec.GitProviderRef.Name,
			Branch:        target.Spec.Branch,
		}
		neededWorkers[key] = true
	}

	m.mu.RLock()
	orphans := make([]BranchKey, 0, len(m.workers))
	for key := range m.workers {
		if !neededWorkers[key] {
			orphans = append(orphans, key)
		}
	}
	m.mu.RUnlock()

	// The window the lock above closes, opened on request so a test can stand in it. Nil in
	// production; see afterOrphanSelection.
	if afterOrphanSelection != nil {
		afterOrphanSelection()
	}

	m.removeWorkersLocked(orphans, "no GitTarget needs this worker any more")

	m.mu.RLock()
	remaining := len(m.workers)
	m.mu.RUnlock()

	m.Log.V(1).Info("Worker reconciliation complete",
		"activeWorkers", remaining,
		"neededWorkers", len(neededWorkers),
		"stopped", len(orphans))

	return nil
}

// Start implements manager.Runnable interface.
// This is called by controller-runtime when the manager starts.
func (m *WorkerManager) Start(ctx context.Context) error {
	// Publish the context under m.mu: EnsureWorker reads m.ctx under the same lock,
	// so guarding the write makes that read race-free regardless of call ordering.
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	telemetry.SetGaugeSource(telemetry.GaugeGitQueueDepth, m.queueDepthSamples)
	m.Log.Info("WorkerManager started")

	m.sweepPeriodically(ctx)

	// Clear the source before the workers go, so the callback cannot outlive them.
	telemetry.SetGaugeSource(telemetry.GaugeGitQueueDepth, nil)
	m.Log.Info("WorkerManager shutting down")
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	m.mu.Lock()
	workers := m.workers
	m.workers = make(map[BranchKey]*BranchWorker)
	m.mu.Unlock()

	// Stop all workers gracefully
	for key, worker := range workers {
		m.Log.Info("Stopping worker for shutdown", "key", key.String())
		worker.Stop()
	}
	m.Log.Info("WorkerManager stopped")
	return nil
}

// WorkerSweepInterval is the floor under the orphan sweep. It is a FLOOR and not the mechanism: a
// deleted GitTarget's reconcile sweeps immediately, and this catches what that path cannot see.
const WorkerSweepInterval = time.Minute

// sweepPeriodically runs the orphan sweep until the context ends.
//
// The delete-triggered sweep is prompt but not complete, because it runs only where a reconcile
// READ NotFound. Delete a GitTarget and recreate it under the same name on another branch before
// that reconcile runs, and the controller sees the successor, wires its worker, and never learns
// that the predecessor existed: the old branch's worker, its goroutine and its clone are then held
// until the process restarts. Kubernetes reconciliation cannot be built on observing every
// intermediate state, so the sweep needs a trigger that does not depend on seeing the delete.
//
// It is a separate goroutine rather than a step in the GitTarget reconcile on purpose: the sweep
// takes lifecycleMu and holds it across Stop, and GitTarget reconciles are serialized, so a slow
// stop on the reconcile path would stall every target's reconcile behind it.
func (m *WorkerManager) sweepPeriodically(ctx context.Context) {
	ticker := time.NewTicker(WorkerSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.ReconcileWorkers(ctx); err != nil {
				// Failing safe: a List that errors stops nothing, so the only cost is that the
				// sweep is late.
				m.Log.V(1).Info("Periodic worker sweep could not read the GitTargets",
					"error", err.Error())
			}
		}
	}
}

// queueDepthSamples is the git_queue_depth source: one sample per live worker, read when
// Prometheus scrapes.
//
// It takes m.mu only, and only to copy the worker pointers out; each worker's own depth is two
// atomic loads. That matters because the callback runs inside the metric SDK's collection path: a
// source that waited on the lock a wedged worker holds across its slow work would reintroduce the
// staleness the observable gauge exists to remove.
func (m *WorkerManager) queueDepthSamples() []telemetry.GaugeSample {
	m.mu.RLock()
	workers := make([]*BranchWorker, 0, len(m.workers))
	for _, worker := range m.workers {
		workers = append(workers, worker)
	}
	m.mu.RUnlock()

	samples := make([]telemetry.GaugeSample, 0, len(workers))
	for _, worker := range workers {
		samples = append(samples, telemetry.GaugeSample{
			Value: worker.queueDepth(),
			Attrs: worker.providerAttrs(),
		})
	}
	return samples
}

// NeedLeaderElection ensures only the elected leader manages workers.
// This prevents multiple pods from managing the same workers.
func (m *WorkerManager) NeedLeaderElection() bool {
	return true
}
