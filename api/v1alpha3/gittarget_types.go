// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GitProviderReference references the GitProvider that backs a GitTarget. Many GitTargets may
// reference the same GitProvider; the reference is always to a GitProvider in the GitTarget's own
// namespace. Group and Kind are typed (with defaults) for consistency with the project's other
// local references and so the schema is explicit about what it accepts — currently only
// configbutler.ai/GitProvider.
type GitProviderReference struct {
	// API Group of the referent.
	// +kubebuilder:default=configbutler.ai
	// +kubebuilder:validation:Enum=configbutler.ai
	Group string `json:"group,omitempty"`

	// Kind of the referent.
	// Optional because this reference currently only supports a single kind (GitProvider).
	// Keeping it optional allows users to omit it while still benefiting from CRD defaulting.
	// +optional
	// +kubebuilder:validation:Enum=GitProvider
	// +kubebuilder:default=GitProvider
	Kind string `json:"kind,omitempty"`

	// Name of the referent.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// GitTargetSpec defines the desired state of GitTarget.
//
// The destination fields — providerRef, branch, and path — are immutable. A
// GitTarget materializes the watched resources at exactly one (provider, branch,
// folder); changing where it writes would orphan the old materialization and require
// migrating manifests between repositories/branches/folders. Instead of reconciling
// that move, the destination is fixed: to relocate a GitTarget, delete it and create a
// new one. This keeps the one-owner-per-folder invariant and the initial-snapshot gate
// simple — a successful snapshot can never be silently invalidated by a destination
// change.
//
// +kubebuilder:validation:XValidation:rule="self.providerRef == oldSelf.providerRef",message="spec.providerRef is immutable; delete and recreate the GitTarget to change its destination"
// +kubebuilder:validation:XValidation:rule="self.branch == oldSelf.branch",message="spec.branch is immutable; delete and recreate the GitTarget to change its destination"
// +kubebuilder:validation:XValidation:rule="self.path == oldSelf.path",message="spec.path is immutable; delete and recreate the GitTarget to change its destination"
//
// spec.clusterProviderRef names the SOURCE cluster a GitTarget mirrors FROM (see its field doc). It
// is immutable — a folder's source cluster is part of what the folder means, like
// providerRef/branch/path above — and defaults to a ClusterProvider named "default", so it is
// always populated (never nil) and always jumpable.
// +kubebuilder:validation:XValidation:rule="self.clusterProviderRef == oldSelf.clusterProviderRef",message="spec.clusterProviderRef is immutable; delete and recreate the GitTarget to change the cluster it mirrors"
type GitTargetSpec struct {
	// ProviderRef references the GitProvider that backs this target.
	// Immutable: delete and recreate the GitTarget to change its destination.
	// +required
	ProviderRef GitProviderReference `json:"providerRef"`

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

	// Placement declares where NEW resources are written. It has no effect on a
	// resource that already has a document in Git — that document is always
	// updated in place at its existing location, wherever that is. Mutable: a
	// change only affects resources created after the change.
	// +optional
	Placement *GitTargetPlacementSpec `json:"placement,omitempty"`

	// Design rationale, kept out of the generated CRD description by the blank line below.
	//
	// It sits at the TOP LEVEL rather than inside placement, and the line between the two is
	// retroactivity. spec.placement decides where a NEW document goes and never moves one already
	// written; this governs the bytes of EVERY write, and it also decides how a managed document
	// is FOUND — a document whose namespace is inherited is located in the file bytes by a
	// namespace-less identity. A field with that blast radius nested inside a struct documented as
	// "new files only" would be a trap.
	//
	// It is a *bool because no plain default preserves today's behavior: false breaks a flat
	// folder, whose documents must carry their own namespace or they are ambiguous, and true
	// writes a redundant line into every kustomize folder that already supplies one. nil means
	// infer, which is what the operator has always done.
	//
	// The name deliberately avoids writeNamespace. "Write" is the most loaded word in this API —
	// the write boundary, the write jail, WriteBoundaryRefused — so writeNamespace: false invites
	// the reading "never write to this namespace", a permission, which is precisely what the
	// neighbouring sourceNamespace fields are.
	//
	// See docs/layout/model.md § "serializeNamespace".

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

	// Design rationale, kept out of the generated CRD description by the blank line below.
	//
	// It defaults to a concrete {name: "default"} rather than an implicit nil so a target that omits
	// it persists with a ref a reader can jump to. The operator never creates that provider: a
	// GitTarget naming one that does not exist is held unready rather than silently defaulting to
	// in-cluster access.

	// ClusterProviderRef names the SOURCE cluster this GitTarget mirrors FROM, by referencing a
	// cluster-scoped ClusterProvider by name. That ClusterProvider owns the cluster's connectivity
	// credential, namespace-access authorization, and author-attribution mode. The default provider
	// name is "default" and must exist; it may be in-cluster or remote.
	// Immutable: a folder's source cluster is part of what the folder means; delete and recreate.
	// +kubebuilder:default={name: "default"}
	// +optional
	ClusterProviderRef *ClusterProviderReference `json:"clusterProviderRef,omitempty"`

	// Design rationale, kept out of the generated CRD description by the blank line below.
	//
	// There is deliberately NO self-namespace exception. An implicit carve-out would mean the field
	// does not actually bound what arrives here, so a reader auditing it would be wrong about the
	// target's contents — which is the whole reason the field exists. The resulting authoring
	// footgun (adding a policy for one override silently denies co-resident legacy rules) is
	// mitigated by being LOUD: SourceNamespaceAuthorized=False, Stalled=True, and a message naming
	// the exact fix. `selector: {}` is the replacement for the removed cluster-wide namespaced
	// ClusterWatchRule — declared by the destination owner rather than the rule author, and
	// self-updating as namespaces come and go. The exact-names half stays answerable without any
	// source-cluster Namespace access; that degradation path is deliberate, and it is the half most
	// likely to regress unnoticed.

	// AllowedSourceNamespaces bounds which SOURCE-cluster namespaces may be mirrored INTO this
	// target. It belongs to the DESTINATION, not to any requesting rule: once declared it is
	// exhaustive for every WatchRule that writes here, with no exception for a rule's own namespace.
	//
	// Omitted and empty differ. Omitted declares no policy, and a WatchRule keeps its own namespace;
	// a declared-but-empty policy admits nothing; `selector: {}` admits every source namespace.
	// Selector labels are read in the SOURCE cluster, so evaluating one needs Namespace
	// get/list/watch for that cluster's credential, while exact names need no such access. This is
	// also what a rules[].sourceNamespace of "*" resolves through. Naming any namespace other than
	// the WatchRule's own — including "*" — additionally requires the ClusterProvider to set
	// spec.allowSourceNamespaceOverride. It does NOT bound ClusterWatchRule, whose cluster-scoped
	// objects have no namespace. Full resolution table: docs/configuration.md.
	// +optional
	AllowedSourceNamespaces *NamespaceMatcher `json:"allowedSourceNamespaces,omitempty"`

	// Design rationale, kept out of the generated CRD description by the blank line below.
	//
	// Deliberately MUTABLE, unlike the destination fields above. The whole point of the safe
	// default is that a target keeps its documents while a scope mistake is diagnosed; turning
	// convergence back on afterwards must not require deleting and recreating the GitTarget, which
	// would be the one operation guaranteed to lose the folder's history.

	// Prune controls which deletion paths may remove documents from this target's folder: an
	// explicit source DELETE event, and the resync mark-and-sweep that infers a deletion from a
	// desired snapshot. Omitted, it is `mode: OnEvent` — observed deletes are mirrored, inferred
	// ones are not — for a stored GitTarget as well as a new one.
	// +optional
	Prune *PrunePolicy `json:"prune,omitempty"`

	// Design rationale, kept out of the generated CRD description by the blank line below.
	//
	// Suspend is a PANIC KNOB: one field that stops this target writing, reachable without
	// deleting anything and without unpicking the watch configuration that would have to be
	// rebuilt afterwards. That is the whole justification, and it is enough on its own.
	//
	// It is deliberately NOT a preview mechanism. A target that writes nothing has nothing to
	// show, and the honest way to see what a target would do is to point one at a scratch branch
	// and read the commits — real bytes, real registrations, real deletes, diffable. The
	// manifest-analyzer CLI is the other half of that answer. Neither is something status should
	// grow a second, worse copy of; see docs/layout/model.md § "Previewing a target: point it at a
	// scratch branch".
	//
	// The scan is deliberately NOT suspended with the write, and the reason is incident response
	// rather than preview: a stopped valve that also stopped looking would freeze status.placement
	// at whatever the folder looked like the moment someone panicked, which is exactly when a
	// stale answer costs the most.
	//
	// Deliberately MUTABLE and deliberately not a fault: a suspended target reports Ready=True with
	// reason Suspended, because not writing is the configured outcome. The precedent is
	// status.retention — a condition asserts health, and suppressing a write on request is not ill
	// health.

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
// canonical, versionless {namespaceOrCluster}/{group}/{resource}/{name}.yaml path.
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

	// Design rationale, kept out of the generated CRD description by the blank line below.
	//
	// It has exactly ONE job, and the name says less than the field does. Registering a new file
	// with the kustomization that already governs its directory is an INVARIANT rather than a
	// setting (#295, fixed by #319): a file no kustomization lists is a file nothing renders, so
	// that happens in both columns. What this flag decides is only what to do when there is no
	// root at all.
	//
	// So useKustomize: false does not mean "leave kustomize alone". If a folder's root must not be
	// touched, do not point a GitTarget at that folder: the ancestor walk is bounded by the write
	// jail, so a kustomization ABOVE spec.path is never edited, and rooting the target lower is
	// the existing, better-tested way to say it.
	//
	// It belongs inside placement, unlike spec.serializeNamespace, because it is retroactive in
	// the same way the rest of this struct is: it decides whether a NEW file's directory has a
	// root to join, and creates one if not. Nothing already written moves or changes.
	//
	// See docs/layout/model.md § "useKustomize".

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

	// LastPushTime is the timestamp of the last successful push.
	// +optional
	LastPushTime *metav1.Time `json:"lastPushTime,omitempty"`

	// Streams is the bounded data-plane roll-up over this GitTarget's tracked types.
	// Counts, never a per-type list, so it stays bounded however many types are watched.
	// +optional
	Streams *GitTargetStreamsStatus `json:"streams,omitempty"`

	// Design rationale, kept out of the generated CRD description by the blank line below.
	//
	// An observation, not a condition. A sweep suppressed by spec.prune.mode is the configured
	// outcome and a healthy reconciliation, so no condition may go False for it — doing so would
	// train operators to ignore the conditions that mean the mirror is genuinely broken. The
	// distinction this field rests on: a condition asserts health, an observation reports a fact.
	// status.streams is the precedent for the second kind.

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
}

// Design rationale, kept out of the generated CRD description by the blank line below.
//
// This stanza answers ONE question: why did a write take the shape it did, or why was it refused.
// It is deliberately not a preview of what the target would do, and three fields were removed for
// trying to be one — examples (a fabricated object at a fabricated path), byTypeEntries (a count
// of a spec map the same GET already returns), and serializeNamespace (a copy of a spec field).
//
// The rule that kept them out is worth stating, because it is what to hold new fields against: a
// field earns its place only if a reader cannot get it from the spec, AND it varies with this
// folder. The write behaviours that follow from Mode — registration into resources:, the
// deregistration a delete performs, the $patch: delete an inherited object needs — are constants
// of the mode rather than facts about the folder, so they are documented on Mode and not
// enumerated here.
//
// To PREVIEW what a target would do, point one at a scratch branch and read the commits it makes.
// That is complete, reviewable, and real, where any status stanza is a summary; see
// docs/layout/model.md § "Previewing a target: point it at a scratch branch".
//
// The resolution REASON is a condition reason (LayoutResolved), not a field, because every
// consumer in this ecosystem already reads reasons from conditions.
//
// There are NO counters. placedResources, overriddenTypes and refusedResources are metrics, and
// placements_total carries them with better labels; a counter in status is a status write per
// event, which re-creates the self-triggering reconcile edge the status work already fixed once.
//
// Nothing here may depend on a placement having HAPPENED. Every field is a fact about the folder
// from the last scan, so the whole stanza is available before the target has ever written a byte.

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

// Design rationale, kept out of the generated CRD description by the blank line below.
//
// Counts, never a per-document list, for the same reason GitTargetStreamsStatus is counts: the
// field must stay bounded however many documents are retained. An operator who needs to know WHICH
// documents reads the retention log line or the folder; status answers "how many, under what
// policy, as of when".
//
// The projection is pull-based — the GitTarget controller reads it from the watch manager on each
// reconcile — so it is only as fresh as the last reconcile, exactly like GitPathAccepted. That is
// acceptable for an observation and would not be for a gate, which is a further reason this must
// not become a condition.

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

	// ObservedTime is when this roll-up was last computed. A retention that begins just after a
	// reconcile is not visible until the next one, so read this before treating a zero as live.
	// +optional
	ObservedTime *metav1.Time `json:"observedTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// Seven default-priority columns wrapped `kubectl get gittargets` on any normal terminal.
// Flux ships three or four; the identity fields stay one `-o wide` away.
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Streams",type=string,JSONPath=`.status.streams.summary`
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspend`,priority=1
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
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.providerRef.name`,priority=1
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

// DeclaresSourceNamespacePolicy reports whether this target declares spec.allowedSourceNamespaces
// at all. A declared policy is EXHAUSTIVE — it bounds every WatchRule item writing here, with no
// self-namespace exception — while an absent one leaves a WatchRule its own namespace. Callers
// must branch on this rather than on emptiness: a declared-but-empty policy admits nothing.
func (g *GitTarget) DeclaresSourceNamespacePolicy() bool {
	return g.Spec.AllowedSourceNamespaces.Declared()
}

// AllowsSourceNamespace reports whether a SOURCE-cluster namespace (by name and by the labels it
// carries IN THE SOURCE CLUSTER) may be mirrored into this target, per spec.allowedSourceNamespaces.
//
// It is the source-side twin of ClusterProvider.AllowsNamespace, and both are thin wrappers over
// NamespaceMatcher.Matches so the two policies cannot drift. It answers only the POLICY question:
// the delegation flag, the provider's own admission of this target's namespace, and the
// three-valued "can the labels be read at all" question are the caller's (see internal/authz).
// An undeclared policy admits nothing here — callers apply the legacy rule themselves.
func (g *GitTarget) AllowsSourceNamespace(nsName string, nsLabels map[string]string) (bool, error) {
	return g.Spec.AllowedSourceNamespaces.Matches(nsName, nsLabels)
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
