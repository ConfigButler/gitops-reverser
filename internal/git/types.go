// SPDX-License-Identifier: Apache-2.0

package git

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"

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
	// DefaultReconcileCommitMessageTemplate names the synced type AND, when the run covered one,
	// the namespace, so the otherwise indistinguishable per-cell reconciles one GitTarget
	// produces are self-describing: a target watching configmaps in team-a and in team-b would
	// otherwise write two byte-identical subjects. Plural resource alone for readability; add
	// {{.APIVersion}} when plural collisions matter.
	//
	// The {{if}} guards fall back to "chore: reconcile N resources" for a whole-target reconcile,
	// so the subject never degrades to an identity-less "chore: reconcile N ". The namespace guard
	// in particular must stay an {{if}} rather than a sentinel: an empty Namespace here means the
	// run was not namespace-scoped, which covers BOTH an all-namespaces sweep of a namespaced type
	// and a cluster-scoped type having no namespaces at all. No single word is true of both, so
	// the honest rendering of "no namespace to name" is to say nothing. (Contrast ResourceRef,
	// which describes ONE resource, where empty has exactly one meaning and carries the
	// types.ClusterScopeSegment sentinel.)
	DefaultReconcileCommitMessageTemplate = "chore: reconcile {{.Count}} " +
		"{{if .Resource}}{{.Resource}}{{else}}resources{{end}}" +
		"{{if .Namespace}} in {{.Namespace}}{{end}}" +
		"{{if .ResourceVersion}} (last resourceVersion: {{.ResourceVersion}}){{end}}"
	// DefaultLiveCommitMessageTemplate describes retained input resources.
	DefaultLiveCommitMessageTemplate = "chore: sync {{.Count}} resource{{if ne .Count 1}}s{{end}}\n\n" +
		"{{range .Resources -}}" +
		"- [{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Namespace}}/{{.Name}}\n" +
		"{{end -}}"

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

// RepoIdentity is WHICH REPOSITORY a branch worker's clone, base trust and observations are
// about. It is what BranchKey does not say: every field of that key comes off the GitTarget, and
// none of them mentions the repository.
//
// The UID carries the identity because spec.url is immutable, so within the lifetime of one
// GitProvider object the repository cannot change: a new object is the only way to reach a new
// one, and that mints a new UID. The URL rides along as a safety net for an immutability rule
// that was bypassed — a CRD reinstalled without the CEL rule, or a write straight to etcd. It is
// compared EXACTLY, with no normalization, because a re-spelled URL can only arrive with a
// recreate, which the UID has already caught.
//
// It is deliberately NOT part of BranchKey. That key is derivable from the GitTarget alone, which
// is what lets the event router resolve a worker with a map read and no GitProvider in hand; a key
// naming the repository would put a provider read on the per-event path, and would let two workers
// for one (provider, branch) report under one set of metric labels while both were live.
type RepoIdentity struct {
	// ProviderUID is the GitProvider object's metadata.uid.
	ProviderUID k8stypes.UID
	// URL is the GitProvider's spec.url, exactly as it is written there.
	URL string
}

// IsZero reports that nothing is known about the repository. The CLI and tests that never touch a
// remote build workers this way, and a comparison against an unknown identity proves nothing.
func (r RepoIdentity) IsZero() bool { return r.ProviderUID == "" && r.URL == "" }

// String is for logs: enough to tell two repositories apart without printing a credential.
func (r RepoIdentity) String() string { return fmt.Sprintf("%s (uid %s)", r.URL, r.ProviderUID) }

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
	// PendingWriteRefusalTouch carries no content at all. It exists to move the branch after a
	// refused write, so the reconciler re-applies the desired state and reverts the live edit a
	// refusal left in place. It is the only write kind that deliberately produces a commit with
	// an empty tree diff, and it is created only for a GitTarget with
	// spec.onRefusal: PushEmptyCommit.
	PendingWriteRefusalTouch PendingWriteKind = "refusal_touch"
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
	// ResourceVersion is the cluster snapshot's resourceVersion the desired set is pinned to
	// (the joined streaming-watch bookmark) — the COLLECTION's version, not any one object's.
	// Carried for diagnostics, logging, and the reconcile commit message.
	//
	// Not "Revision": in this codebase a revision is a Git commit (see LayoutReport.Revision).
	ResourceVersion string
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
	// committedMessageSource is the message source this write actually committed under, stamped by
	// executePendingWrites the way CommitSHA is and for the same reason: publishCommitsForPush runs
	// after the push, and recomputing the source from the write cannot know that a requestTemplate
	// render failed at commit time. Zero means "not stamped", so messageSource() recomputes.
	committedMessageSource messageResolution

	// CommitSHA is the hash of the commit this write created, captured in
	// executePendingWrite and refreshed when the write is re-executed on a
	// rebase-replay (so it is never a stale pre-rebase hash). Zero when the write
	// produced no commit (no diff).
	CommitSHA plumbing.Hash
}

// WorkItem is the unit of work in the BranchWorker queue. Exactly one of
// Request, Attach, Resync, or Refresh is set.
type WorkItem struct {
	// Request is a resource-write request.
	Request *WriteRequest
	// Attach is a CommitRequest attach: bind a message to the author's window and
	// finalize it after the grace.
	Attach *AttachCommitRequest
	// Resync is a streaming-snapshot resync request (M8): a synchronous
	// request/reply that materialises a GitTarget's complete desired set.
	Resync *ResyncRequest
	// Refresh asks the worker to re-prove where its branch is on the remote. It is the only
	// work item that never writes anything: see RefreshRequest.
	Refresh *RefreshRequest
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
// version-pinned desired snapshot (M8). It rides the worker queue so the single
// git-mutating goroutine applies it in order with live events, and replies on
// Result once the local commit is created. The desired set is the whole watched
// resource state at ResourceVersion; the worker's content-derived mark-and-sweep drops
// any managed document the snapshot did not contain.
type ResyncRequest struct {
	Desired            []manifestanalyzer.DesiredResource
	ResourceVersion    string
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
	// RefreshRemote asks the worker to fetch/reset to the remote tip before evaluating the
	// acceptance gate. Forced GitTarget rechecks use it because their trigger is often "I changed
	// Git; look again", and the local checkout may still hold the refused revision.
	RefreshRemote bool
	// SourceCell names the target-watch cell that gathered this snapshot. Zero for a
	// whole-GitTarget resync, which speaks for no single cell. Diagnostic only: nothing
	// filters the queue on it. See source_cell.go.
	SourceCell types.CellKey
	// Result receives exactly one reply. It is buffered (cap 1) by the emitter so
	// the worker never blocks delivering it.
	Result chan ResyncResult
}

// refusalCell is the watched cell this request speaks for: its scope's cell for a per-type
// reconcile, and the ZERO cell for a whole-GitTarget resync, which speaks for every cell the
// target holds rather than for one of them. It is what keys a refusal's dedupe memory and its
// queued commit, so that one watched type's success neither clears nor re-arms another's.
func (r *ResyncRequest) refusalCell() types.CellKey {
	if r == nil || r.Scope == nil {
		return types.CellKey{}
	}
	return r.Scope.Cell
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

	// ResourceVersion is the metadata.resourceVersion of the observed state this event
	// describes. It is PROVENANCE, never content: sanitize strips resourceVersion from Object
	// on purpose (a version in a committed manifest makes every observation a byte change), so
	// this field carries the fact BESIDE the object rather than inside it. Never write it back
	// onto Object.
	//
	// Stamped where the object is observed — the live watch holds the unsanitized object there
	// and nowhere downstream does. Producers with no observed object (reconcile, bootstrap)
	// leave it empty.
	//
	// Unlike the object-derived fields, a DELETE CAN carry one: the watch Deleted frame
	// delivers the final object, so this names the last version that existed.
	ResourceVersion string

	// Generation is the metadata.generation of the observed state — the DESIRED state's counter,
	// where ResourceVersion counts every write. Same provenance-not-content rule: sanitize strips
	// generation from Object one line after resourceVersion, and for the same reason.
	//
	// 0 means "no generation", which covers both "this producer observed nothing" and "this kind
	// has none": a ConfigMap or a Secret never carries one, only types with a spec do.
	Generation int64

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

// CommitMessageConfig contains the resolved live and reconcile templates.
type CommitMessageConfig struct {
	LiveTemplate      string
	ReconcileTemplate string
	// RequestTemplate frames a CommitRequest's message. Empty — the default — commits that
	// message verbatim, which is the behaviour every target had before the field existed.
	RequestTemplate string
}

// ReconcileCommitMessageData is the template context for reconcile commit messages.
//
// Group, Version, Resource and APIVersion describe the reconciled type, and are
// populated only for a per-type reconcile. ResourceVersion is the version the desired set was
// pinned to. Any template referencing these must render cleanly when absent; the default guards
// both with {{if}}.
type ReconcileCommitMessageData struct {
	Count      int
	GitTarget  string
	Group      string
	Version    string
	Resource   string
	APIVersion string
	// ResourceVersion is THE COLLECTION'S metadata.resourceVersion — the LIST the desired set
	// was folded from, not any one object's. A reconcile describes a type, so the version it
	// can name is the snapshot's. (The per-object counterpart is
	// LiveCommitMessageData.Resources[i].ResourceVersion, which a live commit carries because
	// it names n resources.)
	//
	// Empty for a pure sweep. It was called Revision until v0.48.0; see the Revision tombstone
	// below for why the word moved.
	ResourceVersion string
	// Namespace is the single source namespace a namespace-scoped reconcile covered, and
	// is empty for a whole-target or all-namespaces reconcile.
	Namespace string
}

// Revision is the retired name of ResourceVersion, kept ONLY so a stored template that still
// says {{.Revision}} is refused with a sentence instead of a text/template internal
// ("can't evaluate field Revision in type git.ReconcileCommitMessageData").
//
// It is a method rather than a scan of the template source because a method catches every
// spelling — {{with .Revision}}, {{.Revision | printf "%s"}}, assignment to a variable — which a
// search for the literal string would not. Same argument validateRequestTemplate makes for
// probing behaviour rather than matching template text.
//
// It deliberately does NOT return the value. Silently honouring the old name would keep the
// wrong word alive indefinitely, and "accepted, and quietly carried on" is the upgrade failure
// mode docs/UPGRADING.md is written against. Reject, do not prune.
//
// Delete one minor release after v0.48.0. By then {{.Revision}} can go back to failing as a
// plain unknown field, which is all an unknown field deserves.
func (ReconcileCommitMessageData) Revision() (string, error) {
	return "", errors.New(
		"reconcileTemplate: {{.Revision}} was renamed to {{.ResourceVersion}} in v0.48.0 " +
			"(it is the snapshot's resourceVersion; \"revision\" now only ever means a Git commit)")
}

// ResourceRef is the lightweight resource identifier emitted to grouped commit
// templates via LiveCommitMessageData.Resources.
//
// Its fields are deliberately the placement template language's variables under Go template
// casing: {namespace} is .Namespace, {kind} is .Kind, and so on.
// The two renderers are separate on purpose (a path must be statically checkable, a message
// must be able to range over n resources), but a reader should not have to learn two
// vocabularies to describe the same resource. See docs/configuration.md.
type ResourceRef struct {
	Operation  string
	APIVersion string
	Group      string
	Version    string
	Resource   string
	// Kind is the manifest kind, the commit-message spelling of the {kind} placement
	// variable. It is read off the event's object, so it is EMPTY for a DELETE: the watcher
	// carries no object for one, because by then the object is gone from the cluster.
	Kind string
	// Namespace is the resource's namespace, or the literal "_cluster" when it is
	// cluster-scoped — types.ClusterScopeSegment, the same word the canonical Git path and the
	// {namespace} placement variable use. A cluster-scoped resource HAS a scope name; it is
	// simply not a namespace, and "_cluster" is a name no real namespace can collide with
	// (DNS-1123 forbids "_"). A template therefore never has to guard this field.
	Namespace string
	Name      string
	// Labels is the resource's metadata.labels as the writer commits them: the SANITIZED
	// object's labels, so one internal/sanitize strips is already absent, exactly as for the
	// "{label:key}" placement variable. It is nil for a DELETE (no object) and for a resource
	// carrying none.
	//
	// Read it with .Label, not with .Labels.key — see Label.
	Labels map[string]string
	// ResourceVersion is the metadata.resourceVersion of the observed state this commit wrote,
	// and "" for a producer that observed none (reconcile, bootstrap). Guard it with
	// {{with .ResourceVersion}} — empty here has one meaning, "this producer observed no
	// version", and no sentinel states that better than silence.
	//
	// It is NOT the object's current version, and the gap is deliberate: an update whose
	// git-writable content is unchanged (a /status-only write) is never routed, so this value
	// lags the cluster's. Compare it for EQUALITY — equal means nothing is pending — never by
	// subtraction: resourceVersion is opaque by contract and is the global store revision in
	// practice, so a gap measures other objects' writes, not our own drops. The census
	// counts those: watch_events_total{outcome="unchanged"}.
	//
	// Unlike Kind and Labels, a DELETE DOES carry one — see Event.ResourceVersion.
	ResourceVersion string
	// Generation is the metadata.generation of the state this commit wrote, and 0 when there is
	// none. It moves only when the DESIRED state changes, so unlike ResourceVersion it is worth
	// comparing: it is per object, starts at 1, and advances once per spec write, so a gap
	// between two commits really does mean spec changes that were not committed separately.
	//
	// Two blind spots keep it from replacing ResourceVersion. A ConfigMap, a Secret, and any
	// other kind without a spec never carry one, so this stays 0 for much of what a target
	// mirrors. And a label- or annotation-only edit changes what gets committed WITHOUT moving
	// it, so an unchanged Generation does not mean an unchanged commit.
	//
	// Guard it with {{with .Generation}}, which renders nothing for 0.
	Generation int64
}

// Label is the value of one label on this resource, and "" when it does not carry the label.
//
// Use it rather than indexing Labels directly. These templates render with
// missingkey=error, so "{{.Labels.team}}" does not render empty for a resource that has no
// "team" label: it fails the render, and a failed render fails the whole commit (the window's
// events are lost until the next resync). "{{.Label \"team\"}}" renders empty instead, which
// is this language's counterpart of the "_unlabeled" bucket a path falls back to.
func (r ResourceRef) Label(key string) string { return r.Labels[key] }

// String renders the ref as group/version/resource/namespace/name, where the namespace segment
// is "_cluster" for a cluster-scoped resource (Namespace never renders blank). Templates that
// {{range}} over Resources and print each entry get that form.
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

// LiveCommitMessageData is the template context for every live commit message, whatever the
// window retained: one resource, many, or a 0s window. Each live commit covers exactly one
// (author, gitTarget) tuple (see docs/spec/commit-window-refactor.md).
type LiveCommitMessageData struct {
	// Author is the verbatim event.UserInfo.Username for the group.
	Author string
	// GitTarget is the single target this commit is bound to.
	GitTarget string
	// Count is the number of retained resource entries before comparison with Git.
	Count int
	// Operations counts retained entries by operation kind (CREATE/UPDATE/DELETE).
	Operations map[string]int
	// Resources is the per-resource list, deduplicated by file path so the
	// final state is what's being committed.
	Resources []ResourceRef
	// RequestMessage is the message an attached CommitRequest supplied, unaltered. It is empty for
	// every window no request attached to, which is the ordinary case and why liveTemplate may
	// reference it freely.
	//
	// It lives on this struct rather than on a parallel request-only context for the reason
	// ResourceRef's own comment gives about the placement vocabulary: a reader should not have to
	// learn two vocabularies to describe one commit. One sample builder then serves both renders.
	RequestMessage string
}

// LabelValues is the sorted, distinct set of values this commit's resources carry for one
// label key, skipping every resource that does not set it (so the result is empty, never a
// list with a blank in it).
//
// A label is single-valued for a path and set-valued for a commit message: placement asks the
// question of one resource, a commit asks it of the n resources in the window. Printing the
// slice renders Go's "[a b]" form, so a template usually ranges over it:
//
//	{{range .LabelValues "team"}}{{.}} {{end}}
func (d LiveCommitMessageData) LabelValues(key string) []string {
	seen := make(map[string]struct{}, len(d.Resources))
	values := make([]string, 0, len(d.Resources))
	for _, r := range d.Resources {
		value := r.Labels[key]
		if value == "" {
			continue
		}
		if _, dup := seen[value]; dup {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

// LabelValue is the one value every resource in this commit agrees on for key, and "" unless
// all of them carry it with that value. It is what a SUBJECT line wants: naming the team a
// commit belongs to is only honest when the commit is one team's.
//
//	chore: sync {{.Count}} resources{{with .LabelValue "team"}} for {{.}}{{end}}
//
// A resource that does not carry the label DISAGREES; it does not abstain. That is why this
// cannot be LabelValues with a length check: that set skips the unlabeled, so one
// "team: payments" resource committed next to an unlabeled one would name the whole commit
// "for payments" and hide the resource nobody can attribute. The same rule leaves a commit
// containing a DELETE unnamed, since a DELETE carries no object and so no labels (see
// ResourceRef.Labels) — the deleted resource's team is not something the window can know.
func (d LiveCommitMessageData) LabelValue(key string) string {
	shared := ""
	for _, r := range d.Resources {
		value := r.Label(key)
		if value == "" || (shared != "" && value != shared) {
			return ""
		}
		shared = value
	}
	return shared
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
			LiveTemplate:      DefaultLiveCommitMessageTemplate,
			ReconcileTemplate: DefaultReconcileCommitMessageTemplate,
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
	if liveTemplate := strings.TrimSpace(spec.LiveTemplate); liveTemplate != "" {
		c.Message.LiveTemplate = liveTemplate
	}
	if reconcileTemplate := strings.TrimSpace(spec.ReconcileTemplate); reconcileTemplate != "" {
		c.Message.ReconcileTemplate = reconcileTemplate
	}
	// No built-in default to fall back to, unlike the two above: an unset requestTemplate is not
	// "use the standard framing", it is "do not frame at all", and that has to stay distinguishable
	// from a configured one or every existing target would start reformatting its save messages.
	if requestTemplate := strings.TrimSpace(spec.RequestTemplate); requestTemplate != "" {
		c.Message.RequestTemplate = requestTemplate
	}
	return c
}
