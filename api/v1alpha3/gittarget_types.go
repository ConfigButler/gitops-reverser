// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	meta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GitTargetSpec defines the desired state of GitTarget.
//
// The destination fields — gitProviderRef, branch, and path — are immutable. A
// GitTarget materializes the watched resources at exactly one (provider, branch,
// folder); changing where it writes would orphan the old materialization and require
// migrating manifests between repositories/branches/folders. Instead of reconciling
// that move, the destination is fixed: to relocate a GitTarget, delete it and create a
// new one. This keeps the one-owner-per-folder invariant and the initial-snapshot gate
// simple — a successful snapshot can never be silently invalidated by a destination
// change.
//
// The gitProviderRef rule is guarded on oldSelf rather than written as a plain equality, and the
// guard is a one-way migration door rather than a loosening. A GitTarget stored before this field
// was named gitProviderRef serves NO value for it (a field outside the structural schema is not
// served, measured in TestRenamedRequiredField_StoredObjectCanAdoptIt), so a plain equality would
// reject the very apply that migrates it and force a delete-and-recreate. Because the field is
// REQUIRED, it can never be absent on an object created from this release on, so the door is shut
// for everything except the objects it exists for.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.gitProviderRef) || self.gitProviderRef == oldSelf.gitProviderRef",message="spec.gitProviderRef is immutable; delete and recreate the GitTarget to change its destination"
// +kubebuilder:validation:XValidation:rule="self.branch == oldSelf.branch",message="spec.branch is immutable; delete and recreate the GitTarget to change its destination"
// +kubebuilder:validation:XValidation:rule="self.path == oldSelf.path",message="spec.path is immutable; delete and recreate the GitTarget to change its destination"
//
// spec.clusterProviderRef names the SOURCE cluster a GitTarget mirrors FROM (see its field doc). It
// is immutable — a folder's source cluster is part of what the folder means, like
// gitProviderRef/branch/path above — and defaults to a ClusterProvider named "default", so it is
// always populated (never nil) and always jumpable.
// +kubebuilder:validation:XValidation:rule="self.clusterProviderRef == oldSelf.clusterProviderRef",message="spec.clusterProviderRef is immutable; delete and recreate the GitTarget to change the cluster it mirrors"
type GitTargetSpec struct {
	// GitProviderRef names the GitProvider that backs this target, in this GitTarget's own
	// namespace. Many GitTargets may name the same GitProvider.
	// Immutable: delete and recreate the GitTarget to change its destination.
	// +required
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.gitProviderRef.name must not be empty"
	GitProviderRef meta.LocalObjectReference `json:"gitProviderRef"`

	// Branch to use for this target.
	// Must be one of the allowed branches in the provider.
	// Immutable: delete and recreate the GitTarget to change its destination.
	// +required
	// +kubebuilder:validation:MinLength=1
	Branch string `json:"branch"`

	// Path within the repository to write resources to, relative to the repository
	// root. Required and must be non-empty — there is no default, so a GitTarget can
	// never silently write to the repository root. To deliberately target the
	// repository root, set it to "." (the ArgoCD/Flux convention); an empty string is
	// rejected because it is too easy to leave blank by accident to be a deliberate
	// root choice. Any leading slash (absolute path) and ".." are rejected, and a
	// trailing slash is normalized away.
	// Immutable: delete and recreate the GitTarget to change its destination.
	// +required
	// +kubebuilder:validation:MinLength=1
	Path string `json:"path"`

	// Encryption defines encryption settings for Secret resource writes.
	// +optional
	Encryption *EncryptionSpec `json:"encryption,omitempty"`

	// Why this is an empty commit rather than a direct trigger of the reconciler: naming the
	// Kustomization or Application that renders a folder would mean deriving a mapping neither
	// Flux nor Argo CD derives (Argo refreshes every Application whose repository matches and
	// narrows only on an annotation; a Flux Receiver names its resources by hand), and then
	// writing to somebody else's object on the strength of it. Moving the branch asks each tool
	// instead of modelling it. See docs/design/push-notification-and-reconcile-trigger.md, part 3
	// option 7.

	// OnRefusal decides what happens when a live edit is refused because it has no legal
	// destination in the Git folder. A refusal produces no commit, so nothing normally reverts
	// the edit: with Argo CD selfHeal off it stays OutOfSync until a human acts, and under Flux
	// it survives until the next apply interval.
	//
	// "Ignore", the default, records the refusal in conditions and events and does nothing else.
	//
	// "PushEmptyCommit" pushes a commit that changes no file, which moves the branch. Flux then
	// sees a new artifact revision and re-applies; Argo CD sees a revision it has not synced, so
	// its selfHeal=false skip does not apply and automated sync reverts the edit.
	//
	// It fires only when the refused write was an edit to a document the folder ALREADY holds and
	// is not removing. An object Git does not manage is left alone deliberately: re-applying would
	// do nothing to it (Flux prunes from its inventory and Argo CD from the resources it tracks,
	// and a live-created object is in neither), or, if it was managed and has since been removed
	// from Git, would prune it. That second one is the reconciler's decision to take on its own
	// schedule, not this operator's to hurry. Eligibility is per document, not per file: a write
	// that removes a document from a file other documents keep alive is still a removal, and a
	// flush that removes one anywhere is excluded whole.
	//
	// A standing refusal earns one commit, not one per recheck. The target re-reads its folder and
	// refuses again for as long as nobody corrects the edit, and an observation of the same objects
	// with the same issues is already covered by the commit that was made for it. A different
	// refused edit is a new commit, as is the same edit re-made after the target accepted a write
	// in between.
	//
	// It also fires only for a refusal where the folder itself is accepted and this one write had
	// nowhere to land. A folder-level refusal (unparseable YAML, a foreign file) is left alone: an
	// empty commit cannot repair a folder, and the reconciler may be mid-way through its own
	// corrections there.
	//
	// Two consequences remain to weigh. The commit wakes EVERYTHING watching the branch, not just
	// the refused object, so it can hurry along unrelated work including another target's pending
	// prune. And it can revert allowed live edits that are still waiting in the commit window. An
	// Argo CD Application carrying argocd.argoproj.io/manifest-generate-paths ignores the commit
	// entirely, because no file under its refresh paths changed.
	// +optional
	// +kubebuilder:validation:Enum=Ignore;PushEmptyCommit
	// +kubebuilder:default=Ignore
	OnRefusal RefusalAction `json:"onRefusal,omitempty"`

	// Why a "{label:key|fallback}" may start with "_" where a label value may not: a
	// fallback that is itself label-legal shares its bucket with the resources genuinely
	// labeled it, and nothing downstream can separate the two again. A leading "_" is the
	// only way to name a bucket no label value can reach — the property "_unlabeled" and
	// "_cluster" are already built on. The empty fallback is allowed for the mirror-image
	// reason: the built-in sentinel protects the case where nobody chose, while an empty
	// fallback is a choice spelled out in the spec, visible in review and in `kubectl get`.
	// See docs/layout/new-file-placement-rules.md.

	// Why {namespace} takes the same "|fallback" as {label:key}, under the same rules and no
	// others: they are one problem — a variable with an absent case needs a bucket, and the author
	// should be able to name it — so they share the grammar, the charset and the fence.
	//
	// A namespace fallback naming a real namespace is deliberately allowed. The collision it looks
	// like it could cause cannot occur: scope is a property of the TYPE, so two resources
	// rendering the same {groupPath}/{resource} are both cluster-scoped or both namespaced, and
	// the fallback fires only for the former. What is left is a template that drops the type
	// variables — a bundle such as "{namespace|team-a}/all.yaml" — where cluster-scoped resources
	// join that namespace's bundle. Bundling is a supported, declared layout; documents keep their
	// identity inside a file and the write-time guards still refuse a sensitive document in a
	// shared one.

	// Placement declares where NEW resources are written. It has no effect on a
	// resource that already has a document in Git — that document is always
	// updated in place at its existing location, wherever that is. Mutable: a
	// change only affects resources created after the change.
	//
	// Its byType and default templates share one variable language (docs/configuration.md).
	// Besides the resource's identity, a template may read one of its labels:
	// "{label:app.kubernetes.io/instance}/configmaps.yaml".
	//
	// Two variables can be absent for a resource that is otherwise placeable, and both are
	// still placed rather than skipped: "{label:key}" on a resource that does not set the
	// label (or sets it empty) renders the built-in "_unlabeled" bucket, and "{namespace}" on
	// a cluster-scoped resource renders "_cluster". Neither is a value its kind of source can
	// hold, so neither is ever confused with a real one.
	//
	// Append "|fallback" to either to name that bucket yourself — "{label:team|unassigned}",
	// "{namespace|_global}". A fallback is at most 63 characters of [A-Za-z0-9._-] and is
	// neither "." nor "..", so it can add no directory and escape no path; it may start with
	// "_" to stay collision-proof, or be empty ("{label:team|}", "{namespace|}") to render
	// nothing, collapsing the segment. A {namespace} fallback may name a real namespace
	// ("{namespace|team-a}"), which files cluster-scoped resources in that folder; begin it with
	// "_" instead when you want a bucket no namespace can reach. Only these two variables take a
	// fallback, and one written on any other is rejected rather than silently ignored — not
	// because the others always have a value ({groupPath} renders empty for a core resource) but
	// because an empty group segment collapses by design, which is the canonical path's intent.
	//
	// A label is never identity, so it does not contribute to the identity-completeness a
	// sensitive route requires (a {namespace} fallback does not cost it either). Because
	// placement runs only for a resource with no document yet, labeling one afterwards never
	// moves the file already written.
	// +optional
	Placement *GitTargetPlacementSpec `json:"placement,omitempty"`

	// Top level rather than inside placement because it is retroactive: placement decides where a
	// NEW document goes, this governs the bytes of every write and how a managed document is found.
	// *bool because neither default is safe — false breaks a flat folder, true writes a redundant
	// line into a kustomize folder that already supplies one.

	// SerializeNamespace declares whether a committed document carries its own
	// metadata.namespace. It governs every write this target makes, not just the first one, and it
	// applies to NAMESPACED resources only — a cluster-scoped document has no namespace, so the
	// field is ignored for it rather than being an error.
	//
	// Omitted, the namespace is INFERRED per document, which is the behavior a target that says
	// nothing has always had: metadata.namespace is omitted only when the kustomization governing
	// the document's path sets namespace: to that resource's own namespace, and written explicitly
	// in every other case. Leave it unset for a folder that is legitimately non-uniform — a tree of
	// nested kustomize roots, each supplying its own namespace — because inference resolves each
	// document against the root governing its own path.
	//
	// true always writes it: the setting for a flat folder applied directly, where nothing
	// downstream supplies a namespace and a document without one is ambiguous.
	//
	// false never writes it, and is a claim about the whole folder: something outside this
	// repository — a Flux Kustomization's targetNamespace, an Argo Application's
	// destination.namespace, or a kustomization this target maintains itself — supplies the
	// namespace instead. Because a namespace-less document takes its namespace from a single
	// supplier, an explicit false admits exactly ONE source namespace: a second WatchRule bringing
	// another namespace to this target is refused, with GitPathAccepted=False and reason
	// MultipleSourceNamespaces.
	// +optional
	SerializeNamespace *bool `json:"serializeNamespace,omitempty"`

	// Defaults to a concrete {name: "default"}, not nil, so the persisted object names something a
	// reader can jump to. The operator never creates that provider; a missing one holds the target
	// unready rather than falling back to in-cluster access.

	// ClusterProviderRef names the SOURCE cluster this GitTarget mirrors FROM, by referencing a
	// cluster-scoped ClusterProvider by name. That ClusterProvider owns the cluster's connectivity
	// credential, namespace-access authorization, and author-attribution mode. The default provider
	// name is "default" and must exist; it may be in-cluster or remote.
	// Immutable: a folder's source cluster is part of what the folder means; delete and recreate.
	// +kubebuilder:default={name: "default"}
	// +optional
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.clusterProviderRef.name must not be empty"
	ClusterProviderRef *meta.LocalObjectReference `json:"clusterProviderRef,omitempty"`

	// Mutable, unlike the destination fields above: recovering from a scope mistake must not
	// require recreating the GitTarget, which is the one operation that loses the folder's history.

	// Prune controls which deletion paths may remove documents from this target's folder: an
	// explicit source DELETE event, and the resync mark-and-sweep that infers a deletion from a
	// desired snapshot. Omitted, it is `mode: OnEvent` — observed deletes are mirrored, inferred
	// ones are not — for a stored GitTarget as well as a new one.
	// +optional
	Prune *PrunePolicy `json:"prune,omitempty"`

	// Batching and phrasing describe the folder, so they live here; committer and signing describe
	// the identity talking to the remote and stay on GitProvider. Migration: docs/UPGRADING.md.

	// Commit configures how this target's writes are batched into commits, and how those commits
	// are phrased. Omitted, writes coalesce over a 5s rolling silence window and use the built-in
	// message templates.
	// +optional
	Commit *GitTargetCommitSpec `json:"commit,omitempty"`

	// The scan keeps running while writes are suspended: freezing status.placement too would leave
	// a stale answer at the moment it costs most. A suspended target is Ready=True reason
	// Suspended, because not writing is the configured outcome rather than ill health.

	// Suspend stops this target from writing to Git, without deleting it. It is the knob to turn
	// when something is wrong and the writes have to stop now: watches keep running, events keep
	// arriving, the folder keeps being scanned and status.placement keeps being maintained, and no
	// new commit is planned while it is set.
	//
	// It takes effect at the next planning boundary, not instantly. Work already committed
	// locally when suspend is set is still pushed, so a target suspended during a push cooldown
	// can publish one more commit seconds later. That is deliberate: a local commit that is never
	// pushed would sit in the worker's checkout indefinitely and surface later, out of order, on
	// resume. Suspend is a valve on new work, not an undo — it stops the next write, it does not
	// revert the last one.
	//
	// While it is set, status.retention is not published at all: nothing is swept, so nothing is
	// measured, and reporting zero retained documents would read as "converged" when nothing was
	// counted.
	//
	// Omitted, it is false. Clearing it resumes writing from the current cluster state on the next
	// resync; the writes suppressed while suspended are not replayed.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// GitTargetCommitSpec configures how a GitTarget's writes become commits.
type GitTargetCommitSpec struct {
	// metav1.Duration with the markers below is the shape every duration in this API takes: a Go
	// duration string, unit mandatory, rejected at admission, so nothing downstream has to decide
	// what to do with a stored value it cannot parse.
	//
	// The unit set is Go's OWN, wider than Flux's pattern, because the accepted set has to be
	// CLOSED UNDER SERIALIZATION: a typed client reads this field into a time.Duration and writes
	// it back as Duration.String(), so a value the pattern admits but Go re-spells outside it
	// could never be written again. "0.5ms" round-trips as "500µs", which Flux's pattern rejects.
	//
	// The CEL bound is not a policy about how long a window may be; it is what keeps the value
	// PARSEABLE. The pattern cannot express magnitude, so "999999999h" matches it and then
	// overflows time.ParseDuration — and one stored value that no typed client can decode breaks
	// GET and LIST for the whole kind, taking the GitTarget informer down with it.

	// Window is the rolling silence window used to coalesce this target's events into a single
	// commit per author, as a Go duration string ("5s", "750ms", "1m30s"). The timer resets on
	// every event arrival, and the commit is made after this much silence. "0s" opts into
	// per-event commits. At most "24h". Omitted, it is "5s".
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+$"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('24h')",message="spec.commit.window must be a Go duration of at most 24h"
	Window *metav1.Duration `json:"window,omitempty"`

	// Message configures how this target's commit messages are formatted. Omitted, or with any
	// individual template left empty, the built-in templates are used.
	// +optional
	Message *CommitMessageSpec `json:"message,omitempty"`
}

// GitTargetPlacementSpec declares where NEW resources are written when no document
// for their identity exists yet in Git — one exact-type map plus a fallback
// default template (Option B2 of
// docs/layout/new-file-placement-rules.md). There is
// deliberately no separate "sensitive" placement block: sensitivity is a
// write-safety classification the controller owns (encrypt the content, keep the
// path identity-complete, never append or co-mingle), not a second placement
// namespace the user has to configure. A user routes Secrets the same way they
// route anything else — by naming their type in ByType. When a resource's type
// has no ByType entry and no Default, the new document goes beside the folder's
// kustomization when the whole folder is governed by exactly one supported
// kustomization (so the file is reachable from a render root instead of being
// written where kustomize would never build it), and otherwise at the built-in
// canonical, versionless {namespace}/{group}/{resource}/{name}.yaml path.
// Nothing infers a destination from where the repository keeps other resources of
// the same type: a layout this operator cannot derive from one root is declared
// here or it is canonical. Because the canonical path omits the API version,
// objects that differ only by version share a file; a target that watches several
// versions of the same group/resource and wants them separated must use a
// ByType/Default template that includes {version}.
type GitTargetPlacementSpec struct {
	// ByType maps an exact resource type key ("{group}/{version}/{resource}", e.g.
	// "v1/configmaps", "apps/v1/deployments", or "v1/secrets"; core resources omit
	// the group) to the path template used for a new resource of that type. A path
	// selected for a sensitive resource (Secrets, plus any operator-configured
	// sensitive type) must be identity-complete so it cannot collide two distinct
	// sensitive resources onto one file.
	// +optional
	ByType map[string]string `json:"byType,omitempty"`

	// Default is the path template used for a new resource whose type has no ByType
	// entry. Omitted, it falls through to the folder's one supported kustomization
	// root, if it has exactly one, and then to the built-in canonical path.
	// A bundling default (one that is not identity-complete,
	// such as "all.yaml") is only valid when a sensitive resource can never reach it
	// — give every sensitive type an explicit identity-complete ByType entry.
	// +optional
	Default string `json:"default,omitempty"`

	// The trap: false does NOT mean "leave kustomize alone". Registering a new file with the
	// kustomization already governing its directory is an invariant, not a setting, so it happens
	// either way; this flag only decides what to do when there is no root at all. To leave a
	// folder's root untouched, root the target lower — the ancestor walk stops at the write jail.

	// UseKustomize declares that this folder is a kustomize folder whose root the operator
	// maintains. It controls one thing: what happens when NO kustomization governs the path a new
	// document lands at.
	//
	// Omitted or false, the document is written and nothing else is touched. True, a
	// kustomization.yaml is created at spec.path and the new document is registered in it as part
	// of the same commit.
	//
	// A created root ADOPTS the folder: its resources: lists every managed document already there
	// as well as the new one. Turning a folder into a kustomize folder means the folder, and a
	// root naming one file would leave every other file in Git but out of every render.
	//
	// A created root carries NO namespace:. It holds an apiVersion, a kind and the resources: list
	// and nothing else, so the namespace still comes from the documents (serializeNamespace unset
	// or true) or from whatever installs the folder (serializeNamespace false).
	//
	// A folder that already has a kustomize render root never gains a second one, because two
	// render roots is an ambiguous folder that stops accepting new documents altogether. If a
	// byType or default template places a document outside the existing root, that placement is
	// REFUSED rather than committed as a file no kustomization would render.
	//
	// It has NO bearing on a folder that already has a root. A new file is always registered with
	// the nearest kustomization governing it, whatever chose its path, because a file no
	// kustomization lists is a file kustomize never builds.
	// +optional
	UseKustomize bool `json:"useKustomize,omitempty"`
}

// GitTargetStatus defines the observed state of GitTarget.
type GitTargetStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest available observations of an object's state
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Streams is the bounded data-plane roll-up over this GitTarget's tracked types.
	// Counts, never a per-type list, so it stays bounded however many types are watched.
	// +optional
	Streams *GitTargetStreamsStatus `json:"streams,omitempty"`

	// An observation, not a condition: a sweep suppressed by spec.prune.mode is the configured
	// outcome, and a condition going False for it would train operators to ignore the real ones.

	// Retention reports documents a resync kept because this target's spec.prune.mode suppressed
	// the mark-and-sweep. It covers the INFERRED deletion path only: under `never`, a suppressed
	// source DELETE is not counted here, so a `never` target can report zero while still declining
	// to mirror deletes. It is informational either way — retention is the configured behavior,
	// never a fault, and no condition changes state because of it.
	// +optional
	Retention *GitTargetRetentionStatus `json:"retention,omitempty"`

	// Placement is what the last scan resolved about this folder's layout: whether a kustomize
	// render root governs new documents, which one, and whether the folder renders a base it may
	// not write to. It is an observation, like streams and retention, and the LayoutResolved
	// condition carries the verdict.
	// +optional
	Placement *GitTargetPlacementStatus `json:"placement,omitempty"`

	// Remote is where this target's branch is on the Git remote, and when that was last proved.
	// It is renewed by every confirmed look: a successful push as well as a fetch. Absent means
	// nothing has proved anything yet — either nothing has looked, or the GitProvider was
	// recreated against a different repository and what was published no longer describes the
	// one this target points at.
	// +optional
	Remote *GitTargetRemoteStatus `json:"remote,omitempty"`
}

// GitTargetRemoteStatus is the answer to "where is my branch, and when did we last prove it".
//
// It is written whenever the revision changes — including one we pushed ourselves — and otherwise
// only when the published timestamp is older than one refresh interval: the revision has to move
// with the fact it dates.
type GitTargetRemoteStatus struct {
	// Revision is the commit the branch is at on the remote. EMPTY means the branch is not on
	// the remote at all, which is not an error: a branch does not exist without a commit, and a
	// target that has never written has nothing there yet.
	// +optional
	Revision string `json:"revision,omitempty"`

	// LastVerifiedAt is when the remote was last observed. It answers "has anything looked",
	// which placement.resolvedAtRevision deliberately does not: that one dates the resolution, so
	// an old value there means the layout has not changed rather than that nothing has looked.
	// +optional
	LastVerifiedAt *metav1.Time `json:"lastVerifiedAt,omitempty"`

	// VerifiedBy is what proved it: `Push` means the server accepted a ref update of ours, so
	// this revision is our own work; `Fetch` means we went and looked, and this is what was
	// there. A `Fetch` next to a revision no publication of yours produced is how a foreign push
	// to the branch is read off kubectl.
	// +optional
	// +kubebuilder:validation:Enum=Push;Fetch
	VerifiedBy string `json:"verifiedBy,omitempty"`
}

// Two rules for anything added here. A field earns its place only if a reader cannot get it from
// the spec AND it varies with this folder. And nothing may depend on a placement having HAPPENED:
// every field is a fact from the last scan, available before the target has written a byte.
// No counters — those are metrics, and a counter in status is a status write per event.

// GitTargetPlacementStatus is what the last scan resolved about a GitTarget folder's layout.
type GitTargetPlacementStatus struct {
	// Mode is how this folder is written, and it is the field that predicts the surprises.
	//
	// `Plain` — no kustomization governs the folder. A new document is written and nothing else
	// is touched; a delete removes it and nothing else is touched.
	//
	// `KustomizeRoot` — exactly one kustomization governs the folder and the folder is
	// self-contained. A new document is also registered in that root's `resources:`; a delete
	// also drops its entry; and every write is proved by re-rendering, so kustomize itself can
	// refuse one.
	//
	// `KustomizeOverlay` — as KustomizeRoot, and the folder additionally renders a base outside
	// it (ReadOnlyBases). The base is read-only input: an edit to a field the base owns is
	// authored into the overlay when it is expressible there (`images:`, `replicas:`) and refused
	// with WriteBoundaryRefused otherwise, and DELETING an object the overlay inherits authors a
	// `$patch: delete` into the overlay rather than removing anything.
	//
	// Empty when the folder covers more than one render root, which is LayoutResolved=Ambiguous:
	// there is no single answer, and no new document is placed until the target is pointed at one
	// of them.
	// +optional
	// +kubebuilder:validation:Enum=Plain;KustomizeRoot;KustomizeOverlay
	Mode PlacementMode `json:"mode,omitempty"`

	// RenderRoot is the kustomization directory that governs new documents in this folder,
	// relative to spec.path; "." is the folder itself. Empty for Plain (there is no root) and
	// under LayoutResolved=Ambiguous (there is no single one).
	// +optional
	RenderRoot string `json:"renderRoot,omitempty"`

	// ReadOnlyBases are the kustomization directories this folder renders but may never write to,
	// relative to spec.path — so they lead with "../". Non-empty exactly when Mode is
	// KustomizeOverlay, and it is what makes a WriteBoundaryRefused message predictable instead of
	// surprising: an edit that lands on a document under one of these paths cannot be written
	// where it lives.
	// +optional
	ReadOnlyBases []string `json:"readOnlyBases,omitempty"`

	// ResolvedAtRevision is the Git revision this resolution was first observed at. It is not
	// re-stamped on every scan: a resolution that has not changed is not republished, because
	// doing so would write status once per commit to the branch, whichever target caused the
	// commit. So it dates the RESOLUTION, not the last scan — a revision older than the branch
	// head means the folder's layout has not changed since, not that nothing has looked.
	//
	// It is empty when the branch had no commit at the time (a folder nothing has written to yet)
	// and is filled in by the first scan that finds one.
	// +optional
	ResolvedAtRevision string `json:"resolvedAtRevision,omitempty"`

	// ResolvedAt is when this resolution was computed. Like ResolvedAtRevision it dates the
	// resolution rather than the last scan, so a timestamp well in the past means the folder's
	// shape has been stable, not that scanning stopped.
	// +optional
	ResolvedAt *metav1.Time `json:"resolvedAt,omitempty"`
}

// PlacementMode is how a GitTarget folder is written: as plain files, as a kustomize root, or as
// an overlay over a base it may not write to.
type PlacementMode string

const (
	// PlacementModePlain is a folder no kustomization governs.
	PlacementModePlain PlacementMode = "Plain"
	// PlacementModeKustomizeRoot is a self-contained folder governed by exactly one kustomization.
	PlacementModeKustomizeRoot PlacementMode = "KustomizeRoot"
	// PlacementModeKustomizeOverlay is a folder governed by one kustomization that renders a base
	// outside the write scope.
	PlacementModeKustomizeOverlay PlacementMode = "KustomizeOverlay"
)

// GitTargetStreamsStatus is a bounded roll-up of the stream readiness state for the
// types this GitTarget tracks.
type GitTargetStreamsStatus struct {
	// Summary is the display-only ready/total ratio, e.g. "3/4".
	//
	// It restates Ready and Total, which the API conventions would normally rule out. It exists
	// solely to feed the Streams printer column: a column can read one JSONPath, not format two.
	// Do not compute anything from it — read ready and total.
	// +optional
	Summary string `json:"summary,omitempty"`

	// Total is how many types this target tracks.
	Total int32 `json:"total"`

	// Ready is how many tracked types are Streaming.
	Ready int32 `json:"ready"`

	// Replaying is how many tracked types are still replaying their initial events.
	Replaying int32 `json:"replaying"`

	// Blocked is how many tracked types cannot currently be watched.
	Blocked int32 `json:"blocked"`
}

// Counts, never a per-document list, so the field stays bounded however many documents are
// retained. Pull-based: only as fresh as the last reconcile, which is fine for an observation and
// is another reason this must not become a condition.

// GitTargetRetentionStatus is a bounded roll-up of what this GitTarget's prune policy kept.
type GitTargetRetentionStatus struct {
	// Mode is the EFFECTIVE spec.prune.mode this roll-up was produced under. It is reported here
	// rather than left to be read from the spec because a GitTarget that predates spec.prune has
	// no stored value at all, so the spec alone cannot explain why documents are being kept.
	// +optional
	Mode PruneMode `json:"mode,omitempty"`

	// RetainedDocuments is how many managed documents the policy kept that a converged mirror
	// would not hold. Zero means a resync ran and found nothing to retain — the mirror is
	// converged. An ABSENT retention block means something different: no resync has reported yet.
	RetainedDocuments int32 `json:"retainedDocuments"`

	// LastChangedTime records when the reported retention count or effective prune mode last
	// changed. It does NOT indicate when retention was last evaluated: a resync that re-reports the
	// same count leaves it untouched, so an old timestamp is equally consistent with stable
	// retention and with nothing having measured it since.
	// +optional
	LastChangedTime *metav1.Time `json:"lastChangedTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// Three default columns; seven wrapped `kubectl get gittargets` on a normal terminal.
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Streams",type=string,JSONPath=`.status.streams.summary`
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspend`,priority=1
// +kubebuilder:printcolumn:name="Verified",type=date,JSONPath=`.status.remote.lastVerifiedAt`,priority=1
// +kubebuilder:printcolumn:name="Layout",type=string,JSONPath=`.status.placement.mode`,priority=1
// +kubebuilder:printcolumn:name="RenderRoot",type=string,JSONPath=`.status.placement.renderRoot`,priority=1
// +kubebuilder:printcolumn:name="LayoutResolved",type=string,JSONPath=`.status.conditions[?(@.type=="LayoutResolved")].reason`,priority=1
// +kubebuilder:printcolumn:name="GitPathAccepted",type=string,JSONPath=`.status.conditions[?(@.type=="GitPathAccepted")].status`,priority=1
// +kubebuilder:printcolumn:name="RenderMatchesLive",type=string,JSONPath=`.status.conditions[?(@.type=="RenderMatchesLive")].status`,priority=1
// +kubebuilder:printcolumn:name="StreamsRunning",type=string,JSONPath=`.status.conditions[?(@.type=="StreamsRunning")].status`,priority=1
// +kubebuilder:printcolumn:name="SourceReachable",type=string,JSONPath=`.status.conditions[?(@.type=="SourceClusterReachable")].reason`,priority=1
// +kubebuilder:printcolumn:name="ProviderReady",type=string,JSONPath=`.status.conditions[?(@.type=="GitProviderReady")].status`,priority=1
// +kubebuilder:printcolumn:name="ClusterProviderReady",type=string,JSONPath=`.status.conditions[?(@.type=="ClusterProviderReady")].status`,priority=1
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Encryption",type=string,JSONPath=`.spec.encryption.provider`,priority=1
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.gitProviderRef.name`,priority=1
// +kubebuilder:printcolumn:name="Branch",type=string,JSONPath=`.spec.branch`,priority=1
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.spec.path`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GitTarget is the Schema for the gittargets API.
type GitTarget struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of GitTarget
	// +required
	Spec GitTargetSpec `json:"spec"`

	// status defines the observed state of GitTarget
	// +optional
	Status GitTargetStatus `json:"status,omitempty,omitzero"`
}

// SourceCluster is the identity the watch data plane keys a GitTarget's source cluster on: the
// referenced ClusterProvider's NAME. It defaults to "default" when clusterProviderRef is unset —
// so a source-cluster-unaware caller still gets a concrete, non-empty name, and there is no ""
// sentinel. That name is a convention, not a claim about which physical cluster it is. The name
// keys the GVK→GVR registry and the watch context. It is NOT the attribution partition: facts are
// keyed by the referenced ClusterProvider's AuditRoute(), which defaults to this name but may
// differ when several providers share one cluster's single audit stream.
func (g *GitTarget) SourceCluster() string {
	if g.Spec.ClusterProviderRef == nil || g.Spec.ClusterProviderRef.Name == "" {
		return DefaultClusterProviderName
	}
	return g.Spec.ClusterProviderRef.Name
}

// IsLocalSource reports whether this GitTarget references the "default" ClusterProvider, which the
// watch data plane maps to its local cluster context. It is a NAME test, not a claim about the
// physical cluster: a "default" provider may carry a kubeConfig. It only supplies the pre-discovery
// default for SourceClusterReachable, which the watch manager overwrites as soon as it is wired.
func (g *GitTarget) IsLocalSource() bool {
	return g.SourceCluster() == DefaultClusterProviderName
}

// +kubebuilder:object:root=true

// GitTargetList contains a list of GitTarget.
type GitTargetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []GitTarget `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GitTarget{}, &GitTargetList{})
}

// RefusalAction is the action taken when a live write is refused. See GitTargetSpec.OnRefusal.
type RefusalAction string

const (
	// RefusalActionIgnore records the refusal and does nothing else. It is the default.
	RefusalActionIgnore RefusalAction = "Ignore"
	// RefusalActionPushEmptyCommit pushes a commit that changes no file, so the branch moves and
	// the reconciler re-applies the desired state, reverting the refused edit.
	RefusalActionPushEmptyCommit RefusalAction = "PushEmptyCommit"
)

// EffectiveOnRefusal returns the refusal action, resolving an omitted field to the default. The
// CRD default makes this redundant for objects that went through the API server, and not for a
// literal built in a test or by an older client.
func (g *GitTarget) EffectiveOnRefusal() RefusalAction {
	if g.Spec.OnRefusal == "" {
		return RefusalActionIgnore
	}
	return g.Spec.OnRefusal
}
