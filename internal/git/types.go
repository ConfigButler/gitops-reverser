/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package git

import (
	"fmt"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	v1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

const (
	// DefaultCommitterName matches the default operator identity in Git history.
	DefaultCommitterName = "GitOps Reverser"
	// DefaultCommitterEmail matches the default operator email in Git history.
	DefaultCommitterEmail = "noreply@configbutler.ai"
	// DefaultEventCommitMessageTemplate reproduces the current per-event commit message shape.
	DefaultEventCommitMessageTemplate = "[{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Name}}"
	// DefaultReconcileCommitMessageTemplate is the default reconcile commit message shape.
	// It names the synced type for a per-type splice (e.g. "reconciled 6 secrets (last
	// resourceVersion: 1331)"), so the otherwise-indistinguishable per-type reconciles a single
	// GitTarget produces become self-describing — and the pinned resourceVersion shows exactly
	// how fresh the reconcile is, which is useful for demos and first-user trust. The plural
	// resource alone (no group/version) is chosen for readability; a custom template can add
	// {{.APIVersion}} when cross-group plural collisions matter. The {{if .Resource}} and
	// {{if .Revision}} guards fall back to "reconciled N resources" for a whole-target reconcile
	// (nil ScopeGVR) or the events-based atomic path, where the type/revision fields are empty —
	// so the subject never degrades to a trailing-space, identity-less "reconciled N ".
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
	// ScopeGVR, when set, restricts the resync's mark-and-sweep to one type's
	// (group, resource): the M12 per-type reconcile/sweep. Desired then carries only
	// that type's objects (empty for a pure sweep), and no sibling type's document is
	// ever dropped. Nil is the whole-GitTarget resync.
	ScopeGVR *schema.GroupVersionResource
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
	// resolved Committed (with CommitSHA) once this write is pushed (§6.5 of
	// docs/design/stream/commitrequest-design.md). It rides the write through the
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
	// ScopeGVR, when set, makes this a per-type (M12) reconcile/sweep: the mark-and-sweep
	// is restricted to the named type's (group, resource) and Desired carries only that
	// type's objects (empty = pure sweep of a removed type). Nil is a whole-GitTarget resync.
	ScopeGVR *schema.GroupVersionResource
	// Heal marks a non-urgent drift-correcting resync (a periodic checkpoint re-anchor or a
	// removed-type sweep) that the worker DEFERS while a commit window is open, instead of
	// force-finalizing it. Because one worker serves N GitTargets and the commit window is a
	// worker singleton, a force-finalizing heal can steal a DIFFERENT GitTarget's held
	// CommitRequest window — the 8f2ad84 regression. A heal therefore waits for the worker to be
	// idle (no open window), a boundary that recurs on every silence timeout and identity switch,
	// so it never starves and, when it runs, has no window to steal. A first-sync backfill is NOT
	// a heal: it must establish initial state promptly and is ordered before the audit tail.
	Heal bool
	// Result receives exactly one reply. It is buffered (cap 1) by the emitter so
	// the worker never blocks delivering it.
	Result chan ResyncResult
}

// ResyncResult is the reply to a ResyncRequest: the plan's change counts, or an
// error if the resync could not be applied (in which case nothing was committed).
type ResyncResult struct {
	Stats ResyncStats
	Err   error
}

// ResyncStats summarises what a resync changed, for GitTarget status. Created,
// Updated, and Deleted are the materialised create / patch+replace / managed-drop
// counts; Skipped is documents present but not safely editable (e.g. encrypted or
// disallowed constructs).
type ResyncStats struct {
	Created int
	Updated int
	Deleted int
	Skipped int
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

	// AuditStreamID is the FULL Redis stream position "<rv>-<seq>" this change was recorded at
	// on the per-type audit stream. It is set ONLY on the audit-tail path (ReadTypeAuditChanges)
	// and read by the per-(GitTarget, GVR) coverage-watermark gate in applyAuditChangesForType to
	// decide whether the entry is historical for a target (id <= Hc, suppress) or live (id > Hc,
	// route). The sub-sequence is load-bearing: distinct entries can share an rv (an rv-less
	// DELETE/Status rides the high-water, duplicate/same-rv writes get fresh seqs), so the gate
	// compares full positions, not bare rvs. Empty on the live admission path; not used by the
	// writer. See docs/design/stream/signing-snapshot-tail-replay-failure-investigation.md §7.
	AuditStreamID string

	// UserInfo contains user information for commit messages.
	UserInfo UserInfo

	// Path is the POSIX-like relative path prefix for this event's files.
	// This comes from the GitTarget that triggered this event.
	// Empty string means write to repository root.
	Path string

	// GitTargetName is the target owning this event.
	GitTargetName string

	// GitTargetNamespace is the namespace of the target owning this event.
	GitTargetNamespace string

	// BootstrapOptions controls path-scoped bootstrap file staging for this event.
	BootstrapOptions pathBootstrapOptions
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
// See docs/design/manifest/version2/scale-subresource-audit-rehydration.md.
type FieldPatch struct {
	// Assignments are the (path, value) pairs to set on the parent manifest. Paths
	// are disjoint; each owns only its own subtree, so the patch is additive and
	// leaves every unmentioned field in Git untouched.
	Assignments []manifestedit.FieldAssignment
	// Source is a bounded origin label for commit messages and metrics, e.g.
	// "deployments/scale". Never the request URI.
	//
	// The parent Kind is intentionally NOT carried here. The audit objectRef gives
	// only the GVR (plural resource), and the subresource body's own Kind (e.g.
	// "Scale") is not the parent's. The writer resolves the parent document from the
	// objectRef GVR through the same resource-identity inventory the GVR-only delete
	// uses — it already has the live-catalog mapper — so the consumer never needs
	// GVR->GVK resolution.
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
// Group, Version, Resource, and APIVersion name the synced type, mirroring the per-event
// CommitMessageData fields so a reconcile template can identify its type exactly as a per-event
// template does. They are populated for a per-type splice (M12/R2 per-type reconcile, whose
// ResyncRequest carries a non-nil ScopeGVR) and left empty for a whole-target reconcile or the
// events-based atomic path. Revision is the cluster resourceVersion the desired set was pinned to
// (empty for a pure sweep or the events-based path). Any template that references these fields
// must render cleanly when they are absent — the default guards both with {{if}}.
type ReconcileCommitMessageData struct {
	Count      int
	GitTarget  string
	Group      string
	Version    string
	Resource   string
	APIVersion string
	Revision   string
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
// (see docs/design/commit-window-refactor.md).
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

// ResolveCommitConfig resolves API commit settings into runtime defaults.
func ResolveCommitConfig(spec *v1alpha1.CommitSpec) CommitConfig {
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

	if spec.Message != nil {
		if eventTemplate := strings.TrimSpace(spec.Message.EventTemplate); eventTemplate != "" {
			config.Message.EventTemplate = eventTemplate
		}
		if reconcileTemplate := strings.TrimSpace(spec.Message.ReconcileTemplate); reconcileTemplate != "" {
			config.Message.ReconcileTemplate = reconcileTemplate
		}
		if groupTemplate := strings.TrimSpace(spec.Message.GroupTemplate); groupTemplate != "" {
			config.Message.GroupTemplate = groupTemplate
		}
	}

	return config
}
