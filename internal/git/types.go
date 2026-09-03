// SPDX-License-Identifier: Apache-2.0

package git

import (
	"fmt"
	"strings"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// AttributionOutcome records what happened when the operator tried to name the actor behind a
// change. It is carried EXPLICITLY rather than inferred from the author identity, because the
// author string is load-bearing in several places (window grouping, commit-message templates,
// the author_kind metric) and overloading it to also mean "attribution failed" made a silent
// failure indistinguishable from correct configured-author behaviour. See
// docs/architecture.md#author-and-committer-identity-in-git.
type AttributionOutcome string

const (
	// AttributionNotAttempted is configured-author mode: attribution is switched off, so the
	// committer legitimately IS the author and no actor was ever sought.
	//
	// Deliberately the EMPTY string so it is also the ZERO VALUE: the many paths that never assign
	// Attribution (reconcile, resync, bootstrap) all mean exactly this. Any other value makes the
	// zero value a silent fourth state, which is the bug that stopped every CommitRequest
	// attaching. TestAttributionZeroValueIsNotAttempted pins it.
	AttributionNotAttempted AttributionOutcome = ""
	// AttributionResolved means an audit fact named the actor.
	AttributionResolved AttributionOutcome = "resolved"
	// AttributionUnresolved means attribution ran and did not arrive at an actor.
	//
	// "Unresolved", not "failed": no fact produced (correct; not every change has a human actor),
	// a cancelled wait, a read error and a malformed value all return the same not-found, so
	// calling it a failure would assert a fault the operator cannot prove.
	AttributionUnresolved AttributionOutcome = "unresolved"
)

// NamesActor reports whether the outcome carries an actor to compare against.
//
// The ONLY distinction that survives across subsystem boundaries: --author-attribution and
// --admission-webhook are configured independently, so two producers can disagree about the enum
// while agreeing about whether there is an actor. Comparing enums across that boundary couples the
// two flags; comparing NamesActor does not. Within one subsystem the enum IS compared directly.
func (o AttributionOutcome) NamesActor() bool {
	return o == AttributionResolved
}

// UnresolvedAuthor is the identity written to the Git AUTHOR HEADER when attribution ran and
// did not resolve an actor. It exists so an unresolved attribution is visible in `git log`
// instead of being indistinguishable from a configured-author commit.
//
// Scope: the git author header and nothing else. DERIVED at the write path from the carried
// outcome, never stamped onto an Event, so it does NOT reach window grouping, message bodies, or
// {{.Username}} templates — pushing a magic token there would change commit text in every existing
// deployment and force user templates to handle a value they never had.
//
// Three strings because the header needs all three: Username is the greppable machine token,
// DisplayName is what a human reads, Email uses the RFC 2606 .invalid TLD so it never routes mail.
func UnresolvedAuthor() UserInfo {
	return UserInfo{
		Username:    UnresolvedAuthorUsername,
		DisplayName: UnresolvedAuthorDisplayName,
		Email:       UnresolvedAuthorEmail,
	}
}

const (
	// UnresolvedAuthorUsername is the stable machine token for an unresolved attribution.
	UnresolvedAuthorUsername = "attribution-unresolved"
	// UnresolvedAuthorDisplayName is the human-facing git author name.
	UnresolvedAuthorDisplayName = "unknown (attribution unresolved)"
	// UnresolvedAuthorEmail is a reserved-invalid address (RFC 2606).
	UnresolvedAuthorEmail = "attribution-unresolved@gitops-reverser.invalid"
)

const (
	// DefaultCommitterName matches the default operator identity in Git history.
	DefaultCommitterName = "GitOps Reverser"
	// DefaultCommitterEmail matches the default operator email in Git history.
	DefaultCommitterEmail = "noreply@configbutler.ai"
	// DefaultEventCommitMessageTemplate reproduces the current per-event commit message shape.
	DefaultEventCommitMessageTemplate = "[{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Name}}"
	// DefaultReconcileCommitMessageTemplate names the synced type, so the otherwise
	// indistinguishable per-type reconciles one GitTarget produces are self-describing. Plural
	// resource alone for readability; add {{.APIVersion}} when plural collisions matter. The
	// {{if}} guards fall back to "reconciled N resources" for a whole-target reconcile, so the
	// subject never degrades to an identity-less "reconciled N ".
	DefaultReconcileCommitMessageTemplate = "reconciled {{.Count}} " +
		"{{if .Resource}}{{.Resource}}{{else}}resources{{end}}" +
		"{{if .Revision}} (last resourceVersion: {{.Revision}}){{end}}"
	// DefaultGroupCommitMessageTemplate is the default message shape for
	// finalized commit-window commits that contain multiple events.
	DefaultGroupCommitMessageTemplate = "{{.Author}} on {{.GitTarget}}: {{.Count}} resource(s)"

	resourceRefStringPartCap = 5
)

// CommitFile represents a single file to be committed.
type CommitFile struct {
	Path    string
	Content []byte
}

// BranchInfo contains information about a Git branch.
type BranchInfo struct {
	ShortName string // e.g., "main"
	Sha       string // commit hash, normally the tip of the default branch. But will be empty ("") for an unborn branch that is going to be orphaned branch (if the default branch does not exist)
	Unborn    bool   // Is true for branches that don't have commits yet: only HEAD is configured to it
}

// RepoInfo represents high-level repository information.
type RepoInfo struct {
	DefaultBranch     *BranchInfo
	RemoteBranchCount int
}

// PullReport provides detailed pull operation results.
type PullReport struct {
	ExistsOnRemote  bool // Branch exists on remote
	HEAD            BranchInfo
	IncomingChanges bool // SHA changed, requiring resource-level reconcile
}

// BranchKey uniquely identifies a (GitProvider, Branch) combination.
// This is the unit of worker ownership to prevent merge conflicts.
// Multiple GitTargets can share the same BranchKey (same provider+branch)
// but write to different paths within that branch.
type BranchKey struct {
	// RepoNamespace is the namespace containing the GitProvider.
	RepoNamespace string
	// RepoName is the name of the GitProvider.
	RepoName string
	// Branch is the Git branch name.
	Branch string
}

// String returns a string representation for logging and debugging.
// Format: "namespace/provider-name/branch".
func (k BranchKey) String() string {
	return fmt.Sprintf("%s/%s/%s", k.RepoNamespace, k.RepoName, k.Branch)
}

// UserInfo contains relevant user information for commit messages.
type UserInfo struct {
	Username string
	UID      string
	// DisplayName is the human-readable name from the OIDC "name" claim, when
	// the audit event carries it. Empty means "fall back to Username".
	DisplayName string
	// Email is the address from the OIDC "email" claim, when the audit event
	// carries it. Empty means "fall back to ConstructSafeEmail(Username)".
	Email string
}

// CommitMode defines how a write request should be committed.
type CommitMode string

const (
	// CommitModePerEvent streams request events through the live commit window.
	// With commitWindow=0 each event finalizes immediately; otherwise events
	// coalesce by author, target, and quiet-window boundaries.
	CommitModePerEvent CommitMode = "per_event"
	// CommitModeAtomic creates one commit for all events in the request.
	CommitModeAtomic CommitMode = "atomic"
)

// WriteRequest is the unit of work queued and written by the BranchWorker.
type WriteRequest struct {
	Events             []Event
	CommitMessage      string
	CommitConfig       *CommitConfig
	Signer             gogit.Signer
	GitTargetName      string
	GitTargetNamespace string
	BootstrapOptions   pathBootstrapOptions
	CommitMode         CommitMode
}

// PendingWriteKind distinguishes the durable write shapes retained until push.
type PendingWriteKind string

const (
	// PendingWriteCommit is one finalized commit-shaped live-event window.
	PendingWriteCommit PendingWriteKind = "grouped_window"
	// PendingWriteAtomic is a caller-defined atomic request, typically from
	// reconciliation.
	PendingWriteAtomic PendingWriteKind = "atomic"
	// PendingWriteResync is a streaming-snapshot resync (M8): it carries the COMPLETE
	// desired resource set for one GitTarget, and the worker materialises it with a
	// content-derived mark-and-sweep against the worktree (upsert every desired
	// resource, drop every watched managed document the snapshot did not contain).
	PendingWriteResync PendingWriteKind = "resync"
)

type pendingTargetKey struct {
	Name      string
	Namespace string
}

// ResolvedTargetMetadata is the target-scoped planning data retained with a
// pending write so replay does not re-fetch mutable GitTarget state.
type ResolvedTargetMetadata struct {
	Name             string
	Namespace        string
	Path             string
	BootstrapOptions pathBootstrapOptions
	EncryptionConfig *ResolvedEncryptionConfig
	// Placement is the GitTarget's declared new-file placement policy, resolved
	// from spec.placement. Nil when the GitTarget declares none, in which case new
	// resources are placed beside the folder's one kustomize root, if it has exactly one,
	// and otherwise at the canonical path.
	Placement *manifestanalyzer.PlacementPolicy
	// Namespaces is the GitTarget's declared namespace behavior — spec.serializeNamespace and the
	// source namespaces reaching the target — which decides whether the documents this target
	// writes carry metadata.namespace, and which namespace a namespace-free one belongs to. It is
	// resolved with the rest of the metadata so a replayed write honours the policy it was planned
	// under, exactly as PruneMode and Suspend are.
	Namespaces namespacePolicy
	// CommitMessage is the GitTarget's spec.commit.message, verbatim and possibly nil. It is
	// overlaid onto the provider-resolved CommitConfig so a commit is phrased by the folder it
	// writes to rather than by the connection it travels over. Resolved with the rest of the
	// target's mutable state, so a write replayed after a rebase is phrased by the policy it was
	// planned under.
	CommitMessage *v1alpha3.CommitMessageSpec

	// PruneMode is the GitTarget's EFFECTIVE spec.prune.mode — always a concrete value,
	// because it is resolved through EffectivePruneMode and an omitted policy is onEvent.
	// It gates both deletion paths: the resync mark-and-sweep (through the planner's
	// SweepMode) and the steady-state DELETE-event writer.
	//
	// Retained on the pending write so a replay after a rebase is not re-planned under a LOOSER
	// policy than it was planned against. Not frozen, though: tightenPendingPruneModes lowers it
	// when the current policy is stricter, because tightening exists to stop deletions that have
	// not landed yet.
	PruneMode v1alpha3.PruneMode
	// SourceCluster is the NAME of the source cluster the GitTarget mirrors from —
	// (api/v1alpha3).GitTarget.SourceCluster(), the referenced ClusterProvider's name
	// ("default" for the in-cluster provider). The resync mark-and-sweep resolves this subtree's
	// documents' GVK->GVR against that cluster's registry, so a folder mirroring a remote is swept
	// against the right cluster's mapping.
	SourceCluster string

	// Suspend suppresses the WRITE only; the scan still runs, which keeps a suspended target's
	// status fresh. Being CAPTURED defines the cutover: a suspension arriving after this write was
	// planned does not retract it, and a commit already made locally is still pushed. Reading the
	// live GitTarget at push time would strand that commit, to resurface out of order on resume.
	Suspend bool
}

// PendingWrite is the unit retained until a push succeeds.
type PendingWrite struct {
	Kind               PendingWriteKind
	Events             []Event
	CommitMessage      string
	CommitConfig       CommitConfig
	Signer             gogit.Signer
	GitTargetName      string
	GitTargetNamespace string
	Targets            map[pendingTargetKey]ResolvedTargetMetadata
	ByteSize           int64

	// Desired is the complete desired resource snapshot, set only for a
	// PendingWriteResync. The worker folds it over the worktree's content-derived
	// store to produce the resync plan (upserts + mark-and-sweep drops).
	Desired []manifestanalyzer.DesiredResource
	// Scope, when set, restricts the resync's mark-and-sweep to one type's
	// (group, resource) and optionally to one namespace: the M12 per-type
	// reconcile/sweep. Desired then carries only that scope's objects (empty for a pure
	// sweep), and no sibling type's — nor, for a namespace-scoped resync, any sibling
	// namespace's — document is ever dropped. Nil is the whole-GitTarget resync.
	Scope *ResyncScope
	// Revision is the cluster snapshot resourceVersion the desired set is pinned to
	// (the joined streaming-watch bookmark). Carried for diagnostics and logging.
	Revision string
	// ResyncStats, when non-nil, is populated during apply with the plan's
	// create/update/delete/skip counts so a synchronous caller can report them.
	ResyncStats *ResyncStats
	// Committed, when non-nil, is set true during apply iff the resync produced a
	// commit. A no-op resync (e.g. an empty initial snapshot) must not be retained or
	// pushed: doing so would advance the push cooldown and delay the next real
	// snapshot's push past its window.
	Committed *bool

	// CommitRequest, when set, is the CommitRequest claiming this write: it is
	// resolved Committed (with CommitSHA) once this write is pushed. It rides the write through the
	// push cooldown and the conflict rebase-replay, so the result follows the data.
	CommitRequest *commitRequestID
	// CommitSHA is the hash of the commit this write created, captured in
	// executePendingWrite and refreshed when the write is re-executed on a
	// rebase-replay (so it is never a stale pre-rebase hash). Zero when the write
	// produced no commit (no diff).
	CommitSHA plumbing.Hash
}

// CommitMessageKind determines which message/authorship path the executor uses.
type CommitMessageKind string

const (
	CommitMessagePerEvent  CommitMessageKind = "event"
	CommitMessageReconcile CommitMessageKind = "reconcile"
	CommitMessageGrouped   CommitMessageKind = "group"
)

// WorkItem is the unit of work in the BranchWorker queue. Exactly one of
// Request, Attach, or Resync is set.
type WorkItem struct {
	// Request is a resource-write request.
	Request *WriteRequest
	// Attach is a CommitRequest attach: bind a message to the author's window and
	// finalize it after the grace.
	Attach *AttachCommitRequest
	// Resync is a streaming-snapshot resync request (M8): a synchronous
	// request/reply that materialises a GitTarget's complete desired set.
	Resync *ResyncRequest
}

// ResyncScope restricts a resync's mark-and-sweep to the slice of the mirror the desired
// snapshot was actually gathered over: one cell, and the served version that cell was
// gathered at.
//
// The invariant: THE SWEEP SCOPE MUST BE EXACTLY THE SCOPE THE DESIRED SET WAS GATHERED OVER.
// Narrower deletes documents that were never in scope; wider silently leaves documents unmanaged.
// The namespace lives inside the cell so a per-namespace replay cannot reach the sweep carrying
// only its type — a replay of one namespace once swept every other namespace's documents.
type ResyncScope struct {
	// Cell is the sweep boundary and the scope's identity: group, resource, namespace.
	Cell types.CellKey
	// Version is the served version the desired set was gathered at. It is DATA, not
	// identity: it renders the reconcile commit message's {{.APIVersion}} and names the
	// version a snapshot came from in logs, and it is deliberately absent from the cell
	// key, so a scope always round-trips to the boundary it sweeps (types.CellKey).
	Version string
}

// ResyncScopeFor builds a scope from the served GVR a snapshot was gathered with and the
// namespace it was gathered in. It is the only constructor: going through it is what keeps
// the version on the data side of the type and out of the identity.
func ResyncScopeFor(gvr schema.GroupVersionResource, namespace string) ResyncScope {
	return ResyncScope{Cell: types.CellKeyFor(gvr, namespace), Version: gvr.Version}
}

// GVR reconstructs the served GroupVersionResource this scope was gathered with, for the
// callers that must talk to the API machinery in its own terms.
func (s *ResyncScope) GVR() schema.GroupVersionResource {
	if s == nil {
		return schema.GroupVersionResource{}
	}
	return schema.GroupVersionResource{Group: s.Cell.Group, Version: s.Version, Resource: s.Cell.Resource}
}

// String renders the scope for logs and for the deferred-heal key. It is nil-safe: a nil
// scope is the whole-GitTarget resync and renders empty.
func (s *ResyncScope) String() string {
	if s == nil {
		return ""
	}
	return s.Cell.String()
}

// Matches reports whether a resolved resource identity falls inside this scope. A nil scope
// matches everything (whole-GitTarget resync). An empty namespace matches every namespace
// for the type.
func (s *ResyncScope) Matches(ri types.ResourceIdentifier) bool {
	if s == nil {
		return true
	}
	return s.Cell.Matches(ri)
}

// ResyncRequest is a synchronous resync of one GitTarget against a complete,
// revision-pinned desired snapshot (M8). It rides the worker queue so the single
// git-mutating goroutine applies it in order with live events, and replies on
// Result once the local commit is created. The desired set is the whole watched
// resource state at Revision; the worker's content-derived mark-and-sweep drops
// any managed document the snapshot did not contain.
type ResyncRequest struct {
	Desired            []manifestanalyzer.DesiredResource
	Revision           string
	GitTargetName      string
	GitTargetNamespace string
	// Scope, when set, makes this a per-type (M12) reconcile/sweep: the mark-and-sweep is
	// restricted to the named type — and, when the scope names a namespace, to that
	// namespace — while Desired carries only that scope's objects (empty = pure sweep of a
	// removed type). Nil is a whole-GitTarget resync. See ResyncScope for the invariant
	// binding this to Desired.
	Scope *ResyncScope
	// Heal marks a non-urgent drift-correcting resync the worker DEFERS while a commit window is
	// open. One worker serves N GitTargets and the window is a worker singleton, so a
	// force-finalizing heal can steal a DIFFERENT GitTarget's held CommitRequest window. Waiting
	// for idle recurs on every silence timeout, so it never starves. A first-sync backfill is NOT
	// a heal: it must establish initial state promptly.
	Heal bool
	// SourceCell names the target-watch cell that gathered this snapshot. Zero for a
	// whole-GitTarget resync, which speaks for no single cell. Diagnostic only: nothing
	// filters the queue on it. See source_cell.go.
	SourceCell types.CellKey
	// Result receives exactly one reply. It is buffered (cap 1) by the emitter so
	// the worker never blocks delivering it.
	Result chan ResyncResult
}

// resyncKey identifies the slice of a mirror a resync reconciles: one GitTarget,
// and the scope within it. Two requests sharing a key are interchangeable in the
// sense that matters — the newer one's desired set wholly supersedes the older's —
// which is what makes coalescing them safe.
type resyncKey struct {
	namespace string
	name      string
	scope     string
}

// pendingResync is the coalescing entry for one resyncKey: the current request for
// that key, and whether anything for its scope has been queued behind the marker
// that represents it in the FIFO. Once tailPassed is set the marker's position is
// no longer a safe place to run a newer snapshot — see the pendingResyncs field on
// BranchWorker, and "Queue ordering and coalescing" in docs/design/target-watch-plan.md.
type pendingResync struct {
	// marker is the request whose pointer sits on the FIFO for this key. It is fixed
	// for the entry's life: coalescing swaps request, never marker. Identifying the
	// entry by its marker is what keeps a released key unambiguous — once a later
	// request re-inserts the same key, the older marker must run the payload it
	// carried rather than pick up the newer entry.
	marker     *ResyncRequest
	request    *ResyncRequest
	tailPassed bool
}

func resyncKeyFor(request *ResyncRequest) resyncKey {
	key := resyncKey{namespace: request.GitTargetNamespace, name: request.GitTargetName}
	if request.Scope != nil {
		key.scope = request.Scope.Cell.String()
	}
	return key
}

// ResyncResult is the reply to a ResyncRequest: the plan's change counts, or an
// error if the resync could not be applied (in which case nothing was committed).
type ResyncResult struct {
	Stats ResyncStats
	Err   error
}

// ResyncStats summarises what a resync changed. Skipped is documents present but not safely
// editable; PlacementSkipped is new resources the writer refused to place fail-safe. Both are
// counted and logged per-resource rather than swallowed, so a not-mirrored resource is visible in
// the summary. Neither has a dedicated status condition yet.
type ResyncStats struct {
	Created          int
	Updated          int
	Deleted          int
	Skipped          int
	PlacementSkipped int
	// Retained is how many managed documents this resync's prune policy kept that a converged
	// mirror would have dropped. It is the ONE count here that does not describe something the
	// resync did: a suppressed drop produces no action, no commit, and no other stat, so without
	// it nothing downstream can tell a converged mirror from a deliberately retaining one. It
	// rides the reply channel to the drain, which rolls it up onto GitTarget status.
	Retained int
	// PruneMode stamps Retained with the effective policy that produced it, so the count and the
	// reason for it travel together. Reading the mode from the spec at projection time instead
	// would let a target that has just been switched publish a new mode beside a count the old
	// one produced.
	PruneMode v1alpha3.PruneMode
}

// reply delivers a result on the request's buffered channel without blocking, so a
// caller that already gave up (timeout/ctx cancel) never wedges the worker loop.
func (r *ResyncRequest) reply(result ResyncResult) {
	if r.Result == nil {
		return
	}
	select {
	case r.Result <- result:
	default:
	}
}

// Event represents a resource change event to be processed by a branch worker.
// Branch comes from the worker context (not stored in event).
// Path comes from the GitTarget that created this event.
type Event struct {
	// Object is the sanitized Kubernetes object. Exactly one of Object or
	// FieldPatch is set for a resource mutation; a control or DELETE event may
	// carry neither.
	Object *unstructured.Unstructured

	// FieldPatch, when set, replaces Object with a bounded in-place edit of an
	// existing parent manifest (subresource audit resolution). It is mutually
	// exclusive with Object.
	FieldPatch *FieldPatch

	// Identifier contains resource identification information.
	Identifier types.ResourceIdentifier

	// Operation is the admission operation (CREATE, UPDATE, DELETE).
	Operation string

	// UserInfo contains user information for commit messages.
	UserInfo UserInfo

	// Attribution is the authority for author rendering, the author_kind metric, and
	// CommitRequest window matching. None may infer the outcome from UserInfo: an empty username
	// cannot distinguish "attribution is off" from "it ran and found nothing". attachAuthor is
	// the only assignment outside tests.
	Attribution AttributionOutcome

	// Path is the POSIX-like relative path prefix for this event's files.
	// This comes from the GitTarget that triggered this event.
	// Empty string means write to repository root.
	Path string

	// GitTargetName is the target owning this event.
	GitTargetName string

	// GitTargetNamespace is the namespace of the target owning this event.
	GitTargetNamespace string

	// SourceCluster is the NAME of the source cluster this object was watched on —
	// (api/v1alpha3).GitTarget.SourceCluster(), the referenced ClusterProvider's name
	// ("default" for the in-cluster provider). The writer resolves this document's GVK->GVR
	// against that cluster's type registry, so a folder mirroring a remote is never indexed
	// against the local cluster's mapping.
	SourceCluster string

	// BootstrapOptions controls path-scoped bootstrap file staging for this event.
	BootstrapOptions pathBootstrapOptions

	// SourceCell names the target-watch cell that produced this event. Zero for every
	// non-stream producer (reconcile, bootstrap, the admission path). Diagnostic only:
	// nothing filters the queue on it. See source_cell.go.
	SourceCell types.CellKey
}

// IsFieldPatch reports whether the event carries a bounded field patch instead of
// a full object. It is the single predicate the pipeline branches on to route a
// patch to the in-place writer rather than the object writer.
func (e Event) IsFieldPatch() bool {
	return e.FieldPatch != nil
}

// FieldPatch is a bounded set of field assignments to an existing parent manifest,
// carried in place of a full Object. It is how an author-preserving subresource
// mutation (e.g. deployments/scale) reaches Git: set exactly the audited field
// paths on the already committed parent, never reconstructing the whole object.
// See docs/spec/scale-subresource-audit-rehydration.md.
type FieldPatch struct {
	// Assignments are the (path, value) pairs to set on the parent manifest. Paths
	// are disjoint; each owns only its own subtree, so the patch is additive and
	// leaves every unmentioned field in Git untouched.
	Assignments []manifestedit.FieldAssignment
	// Source is a bounded origin label for commit messages and metrics, e.g.
	// "deployments/scale". Never the request URI.
	//
	// The parent Kind is NOT carried: the audit objectRef gives only the GVR, and the subresource
	// body's own Kind ("Scale") is not the parent's. The writer resolves the parent through the
	// same resource-identity inventory the GVR-only delete uses.
	Source string
}

// CommitConfig is the resolved commit behavior used by the git writer.
type CommitConfig struct {
	Committer CommitterConfig
	Message   CommitMessageConfig
}

// CommitterConfig defines the operator identity used as the git committer.
type CommitterConfig struct {
	Name  string
	Email string
}

// CommitMessageConfig contains the resolved per-event, reconcile, and grouped templates.
type CommitMessageConfig struct {
	EventTemplate     string
	ReconcileTemplate string
	GroupTemplate     string
}

// CommitMessageData is the template context for per-event commit messages.
type CommitMessageData struct {
	Operation  string
	Group      string
	Version    string
	Resource   string
	Namespace  string
	Name       string
	APIVersion string
	Username   string
	GitTarget  string
}

// ReconcileCommitMessageData is the template context for reconcile commit messages.
//
// Group, Version, Resource and APIVersion mirror the per-event CommitMessageData fields, and are
// populated only for a per-type reconcile. Revision is the resourceVersion the desired set was
// pinned to. Any template referencing these must render cleanly when absent; the default guards
// both with {{if}}.
type ReconcileCommitMessageData struct {
	Count      int
	GitTarget  string
	Group      string
	Version    string
	Resource   string
	APIVersion string
	Revision   string
	// Namespace is the single source namespace a namespace-scoped reconcile covered, and
	// is empty for a whole-target or all-namespaces reconcile.
	Namespace string
}

// ResourceRef is the lightweight resource identifier emitted to grouped commit
// templates via GroupedCommitMessageData.Resources.
type ResourceRef struct {
	Group     string
	Version   string
	Resource  string
	Namespace string
	Name      string
}

// String renders the ref as group/version/resource[/namespace]/name.
// The format mirrors ResourceIdentifier.String for templates that want to
// {{range}} over Resources and just print each entry.
func (r ResourceRef) String() string {
	parts := make([]string, 0, resourceRefStringPartCap)
	if r.Group != "" {
		parts = append(parts, r.Group)
	}
	if r.Version != "" {
		parts = append(parts, r.Version)
	}
	if r.Resource != "" {
		parts = append(parts, r.Resource)
	}
	if r.Namespace != "" {
		parts = append(parts, r.Namespace)
	}
	if r.Name != "" {
		parts = append(parts, r.Name)
	}
	return strings.Join(parts, "/")
}

// GroupedCommitMessageData is the template context for grouped commit
// messages. Each grouped commit covers exactly one (author, gitTarget) tuple
// (see docs/spec/commit-window-refactor.md).
type GroupedCommitMessageData struct {
	// Author is the verbatim event.UserInfo.Username for the group.
	Author string
	// GitTarget is the single target this commit is bound to.
	GitTarget string
	// Count is the number of distinct resources committed.
	Count int
	// Operations counts events by operation kind (CREATE/UPDATE/DELETE).
	Operations map[string]int
	// Resources is the per-resource list, deduplicated by file path so the
	// final state is what's being committed.
	Resources []ResourceRef
}

// ResolveCommitConfig resolves a GitProvider's commit settings into runtime defaults.
//
// It reads the COMMITTER only. Message templates are a GitTarget concern
// (GitTarget.spec.commit.message) and are overlaid by WithTargetMessage, so a provider that still
// carries a stored spec.commit.message has no effect here — that provider is refused outright by
// its own reconciler rather than half-honoured.
func ResolveCommitConfig(spec *v1alpha3.CommitSpec) CommitConfig {
	config := CommitConfig{
		Committer: CommitterConfig{
			Name:  DefaultCommitterName,
			Email: DefaultCommitterEmail,
		},
		Message: CommitMessageConfig{
			EventTemplate:     DefaultEventCommitMessageTemplate,
			ReconcileTemplate: DefaultReconcileCommitMessageTemplate,
			GroupTemplate:     DefaultGroupCommitMessageTemplate,
		},
	}

	if spec == nil {
		return config
	}

	if spec.Committer != nil {
		if name := strings.TrimSpace(spec.Committer.Name); name != "" {
			config.Committer.Name = name
		}
		if email := strings.TrimSpace(spec.Committer.Email); email != "" {
			config.Committer.Email = email
		}
	}

	return config
}

// WithTargetMessage overlays a GitTarget's spec.commit.message onto a resolved config, leaving any
// template the target does not set at its built-in default. A nil spec changes nothing, so a
// target that configures no messages commits under the same wording it always did.
func (c CommitConfig) WithTargetMessage(spec *v1alpha3.CommitMessageSpec) CommitConfig {
	if spec == nil {
		return c
	}
	if eventTemplate := strings.TrimSpace(spec.EventTemplate); eventTemplate != "" {
		c.Message.EventTemplate = eventTemplate
	}
	if reconcileTemplate := strings.TrimSpace(spec.ReconcileTemplate); reconcileTemplate != "" {
		c.Message.ReconcileTemplate = reconcileTemplate
	}
	if groupTemplate := strings.TrimSpace(spec.GroupTemplate); groupTemplate != "" {
		c.Message.GroupTemplate = groupTemplate
	}
	return c
}
