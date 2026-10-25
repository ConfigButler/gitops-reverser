// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	meta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// GitTargetSpec defines the desired state of GitTarget.
//
// The destination fields — gitBranchRef and path — are immutable. A GitTarget
// materializes the watched resources at exactly one (repository branch,
// folder); changing where it writes would orphan the old materialization and
// require migrating manifests between repositories/branches/folders. Instead
// of reconciling that move, the destination is fixed: to relocate a GitTarget,
// delete it and create a new one. This keeps the one-owner-per-folder
// invariant and the initial-snapshot gate simple — a successful snapshot can
// never be silently invalidated by a destination change.
//
// +kubebuilder:validation:XValidation:rule="self.gitBranchRef == oldSelf.gitBranchRef",message="spec.gitBranchRef is immutable; delete and recreate the GitTarget to change its destination"
// +kubebuilder:validation:XValidation:rule="self.path == oldSelf.path",message="spec.path is immutable; delete and recreate the GitTarget to change its destination"
// +kubebuilder:validation:XValidation:rule="!has(self.onRefusal) || self.onRefusal != 'Reapply' || has(self.applierClaimRef)",message="spec.onRefusal: Reapply needs spec.applierClaimRef, the applier claim asked to reapply"
type GitTargetSpec struct {
	// GitBranchRef names the GitBranch this GitTarget writes to: the repository
	// branch and the credential that writes it. An omitted namespace means this
	// GitTarget's namespace. The GitBranch's spec.allowedGitTargets must admit
	// this namespace. Several GitTargets may share one GitBranch. Immutable:
	// delete and recreate the GitTarget to change it.
	// +required
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.gitBranchRef.name must not be empty"
	GitBranchRef meta.NamespacedObjectReference `json:"gitBranchRef"`

	// No default, and "" is rejected: a path left blank by accident must not mean the repository
	// root. "." is the Argo CD and Flux spelling of a deliberate root.

	// Path is the folder this GitTarget writes, relative to the repository root.
	// Use "." for the root itself. A leading "/" and ".." are rejected, and a
	// trailing "/" is dropped. Immutable: delete and recreate the GitTarget to
	// change it.
	// +required
	// +kubebuilder:validation:MinLength=1
	Path string `json:"path"`

	// Encryption configures how Secrets are encrypted before they are written to
	// Git.
	// +optional
	Encryption *EncryptionSpec `json:"encryption,omitempty"`

	// Why a reference rather than an inference: deriving which Kustomization applies a folder would
	// be guessing on Flux's behalf, and the guess would drive a write. A reference the user declares
	// claims nothing. See docs/design/apply-trigger.md.

	// ApplierClaimRef names the FluxClaim or ArgoCDClaim, in this namespace, that
	// holds the applier of this folder: the Flux Kustomization or Argo CD
	// Application that applies it. With it set, GitOps Reverser decides when that
	// applier runs, and runs it only after the edits it holds are published.
	// Objects another applier owns are not mirrored into the folder. The applier
	// must apply this GitTarget's repository and branch; the ApplierClaimReady
	// condition reports whether it does. Several GitTargets may name one applier
	// claim.
	// +optional
	ApplierClaimRef *ApplierClaimReference `json:"applierClaimRef,omitempty"`

	// Reapply is limited to edits of documents the folder already holds, in writes that remove no
	// document: re-applying otherwise prunes, or puts back, an object somebody else is deleting. A
	// folder-level refusal never fires it, because re-applying Git cannot repair a folder.

	// OnRefusal decides what happens when a live edit is refused because it has no
	// valid place in the folder. A refusal makes no commit, so the edit stays on
	// the cluster until something applies Git over it.
	//
	// "Ignore" (the default) reports the refusal in conditions and events and does
	// nothing else.
	//
	// "Reapply" asks the applier in spec.applierClaimRef, which it requires, to
	// apply the branch once the pending edits are published, which reverts the
	// refused edit. It fires only for an edit to a document the folder already
	// holds, in a write that removes no document, and never for a refusal of the
	// whole folder, such as unparseable YAML. A refusal that stands is reapplied
	// once, not on every recheck.
	// +optional
	// +kubebuilder:validation:Enum=Ignore;Reapply
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

	// Placement decides where a new resource is written: one with no document
	// in the folder yet. A resource that already has a document is updated
	// where that document is. So a change here affects only resources created
	// afterward, and labeling a resource later never moves its file.
	//
	// byType and default are path templates over the resource's identity:
	// {namespace}, {name}, {group}, {version}, {resource}, {kind} and more. They
	// may read a label: "{label:app.kubernetes.io/instance}/configmaps.yaml".
	//
	// Two variables can be absent, and each then renders a built-in value:
	//
	// - "{label:key}" renders "_unlabeled" for a resource without the label.
	// - "{namespace}" renders "_cluster" for a cluster-scoped resource.
	//
	// Append "|fallback" to either to choose that value instead:
	// "{label:team|unassigned}", "{namespace|_global}". A fallback is up to 63
	// characters of [A-Za-z0-9._-], is not "." or "..", and may be empty to
	// drop the path segment. A {namespace} fallback that names a real namespace
	// files cluster-scoped resources in that namespace's folder. A fallback on
	// any other variable is rejected.
	// +optional
	Placement *GitTargetPlacementSpec `json:"placement,omitempty"`

	// Top level rather than inside placement because it is retroactive: placement decides where a
	// NEW document goes, this governs the bytes of every write and how a managed document is found.
	// *bool because neither default is safe — false breaks a flat folder, true writes a redundant
	// line into a kustomize folder that already supplies one.

	// SerializeNamespace decides whether a namespaced resource's document carries
	// metadata.namespace. It applies to every write, not just the first, and is
	// ignored for cluster-scoped resources.
	//
	// Omitted, it is decided per document: the namespace is left out only when the
	// kustomization governing the document's path sets namespace: to that same
	// namespace. This suits a folder of nested kustomize roots that each set their
	// own namespace.
	//
	// true always writes it. Use it for a flat folder applied as is.
	//
	// false never writes it, for a folder whose namespace something else supplies:
	// a Flux Kustomization's targetNamespace, an Argo CD Application's
	// destination.namespace, or a kustomization. Such a folder holds one source
	// namespace; a WatchRule bringing a second one is refused with
	// GitPathAccepted=False, reason MultipleSourceNamespaces.
	// +optional
	SerializeNamespace *bool `json:"serializeNamespace,omitempty"`

	// KubeProviderRef names the KubeProvider this GitTarget mirrors from: the
	// source cluster and the credential that reads it. An omitted namespace means
	// this GitTarget's namespace. The KubeProvider's spec.allowedGitTargets must
	// admit this namespace. Immutable: delete and recreate the GitTarget to change
	// it.
	// +required
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.kubeProviderRef.name must not be empty"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.kubeProviderRef is immutable; delete and recreate the GitTarget to change the cluster it mirrors"
	KubeProviderRef meta.NamespacedObjectReference `json:"kubeProviderRef"`

	// Mutable, unlike the destination fields above: recovering from a scope mistake must not
	// require recreating the GitTarget, which is the one operation that loses the folder's history.

	// Prune decides which deletions remove documents from this folder: a delete
	// observed on the source cluster, and a document a resync finds no resource
	// for. Omitted, mode is "OnEvent": observed deletes are mirrored, and a resync
	// removes nothing.
	// +optional
	Prune *PrunePolicy `json:"prune,omitempty"`

	// Batching and phrasing describe the folder, so they live here; committer and signing describe
	// the identity talking to the remote and stay on GitProvider. Migration: docs/UPGRADING.md.

	// Commit configures how this GitTarget's writes are batched into commits and
	// how the commits are worded. Omitted, a commit window closes 5s after its
	// last write or 1m after it opened, and the built-in message templates are
	// used.
	// +optional
	Commit *GitTargetCommitSpec `json:"commit,omitempty"`

	// The scan keeps running while writes are suspended: freezing status.placement too would leave
	// a stale answer at the moment it costs most. A suspended target is Ready=True reason
	// Suspended, because not writing is the configured outcome rather than ill health. A local
	// commit is still pushed because one never pushed would sit in the checkout and surface later,
	// out of order, on resume. status.retention is withheld because nothing is swept, and zero
	// would read as converged.

	// Suspend stops this GitTarget from making new commits, without deleting
	// it. Defaults to false.
	//
	// While it is set, watches keep running and status.placement stays current,
	// but status.retention is not published.
	//
	// A commit already made locally is still pushed, so one more commit can
	// appear seconds after it is set: suspend stops the next write and does not
	// undo the last one.
	//
	// Clearing it resumes from the current cluster state at the next resync.
	// Writes skipped while suspended are not replayed.
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

	// Window sets when this GitTarget's commit windows close. A window opens on an
	// author's first write and collects that author's writes until idleTimeout
	// passes without one, or maxDuration after it opened, whichever comes first. A
	// change applies to windows opened afterward. A CommitRequest attached to a
	// window replaces both timers for that window.
	// +optional
	Window *CommitWindow `json:"window,omitempty"`

	// Message sets the commit message templates. A template left empty uses the
	// built-in one.
	// +optional
	Message *CommitMessageSpec `json:"message,omitempty"`
}

// CommitWindow holds the two timers that close a GitTarget's commit windows. A
// change to them applies to windows that open afterward; a window already open
// keeps the timers it opened with.
// +kubebuilder:validation:XValidation:rule="!has(self.idleTimeout) || !has(self.maxDuration) || duration(self.idleTimeout) <= duration(self.maxDuration)",message="spec.commit.window.idleTimeout must not exceed maxDuration"
type CommitWindow struct {
	// IdleTimeout closes the window this long after its last write, as a Go
	// duration ("5s", "750ms"). "0s" makes one commit per write. At most "24h".
	// Defaults to "5s".
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+$"
	// +kubebuilder:default="5s"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('24h')",message="spec.commit.window.idleTimeout must be a Go duration of at most 24h"
	IdleTimeout *metav1.Duration `json:"idleTimeout,omitempty"`

	// MaxDuration closes the window this long after it opened, however many writes
	// keep arriving, as a Go duration ("1m", "90s"). "0s" closes it right after
	// the write that opened it. At most "24h". Defaults to "1m".
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+$"
	// +kubebuilder:default="1m"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('24h')",message="spec.commit.window.maxDuration must be a Go duration of at most 24h"
	MaxDuration *metav1.Duration `json:"maxDuration,omitempty"`
}

// GitTargetPlacementSpec declares where NEW resources are written when no
// document for their identity exists yet in Git — one exact-type map plus a
// fallback default template (Option B2 of
// docs/layout/new-file-placement-rules.md). There is deliberately no separate
// "sensitive" placement block: sensitivity is a write-safety classification
// the controller owns (encrypt the content, keep the path identity-complete,
// never append or co-mingle), not a second placement namespace the user has to
// configure. A user routes Secrets the same way they route anything else — by
// naming their type in ByType. When a resource's type has no ByType entry and
// no Default, the new document goes beside the folder's kustomization when the
// whole folder is governed by exactly one supported kustomization (so the file
// is reachable from a render root instead of being written where kustomize
// would never build it), and otherwise at the built-in versionless
// {namespace}/{group}/{resource}/{name}.yaml path. Nothing infers a
// destination from where the repository keeps other resources of the same
// type: a layout this operator cannot derive from one root is declared here or
// it takes the built-in path. Because the built-in path omits the API version,
// objects that differ only by version share a file; a target that watches
// several versions of the same group/resource and wants them separated must
// use a ByType/Default template that includes {version}.
type GitTargetPlacementSpec struct {
	// ByType maps a resource type, written "{group}/{version}/{resource}"
	// ("apps/v1/deployments", or "v1/configmaps" for the core group), to the path
	// template for new resources of that type. The template of a sensitive type,
	// such as Secrets, must contain the resource's whole identity, so two of them
	// never share a file.
	// +optional
	ByType map[string]string `json:"byType,omitempty"`

	// Default is the path template for a new resource whose type has no byType
	// entry. Omitted, a new document goes next to the folder's kustomization when
	// exactly one governs the folder, and otherwise to
	// {namespace}/{group}/{resource}/{name}.yaml. That path has no version, so add
	// {version} to a template to keep two versions of one resource apart. A
	// bundling template such as "all.yaml" is valid only when every sensitive type
	// has its own byType entry.
	// +optional
	Default string `json:"default,omitempty"`

	// A created root adopts the whole folder, because a root naming one file would leave every
	// other file out of every render. A second root is never created, because two roots make an
	// ambiguous folder that stops accepting new documents.
	//
	// The trap: false does NOT mean "leave kustomize alone". Registering a new file with the
	// kustomization already governing its directory is an invariant, not a setting, so it happens
	// either way; this flag only decides what to do when there is no root at all. To leave a
	// folder's root untouched, root the target lower — the ancestor walk stops at the write jail.

	// UseKustomize makes GitOps Reverser create a kustomization.yaml at spec.path
	// when no kustomization governs the folder yet. The first new document creates
	// it, listing every managed document already in the folder and the new one,
	// and nothing else: no namespace:. It has no effect on a folder that already
	// has a kustomization. A new file is always added to the kustomization
	// governing it, and a template that places a document outside that
	// kustomization is refused. Defaults to false.
	// +optional
	UseKustomize bool `json:"useKustomize,omitempty"`
}

// GitTargetStatus defines the observed state of GitTarget.
type GitTargetStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions are the latest observations of this GitTarget's state. Ready,
	// Reconciling and Stalled follow kstatus; the others say which check holds the
	// GitTarget back.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Streams counts the types this GitTarget watches, by watch state.
	// +optional
	Streams *StreamsStatus `json:"streams,omitempty"`

	// An observation, not a condition: a sweep suppressed by spec.prune.mode is the configured
	// outcome, and a condition going False for it would train operators to ignore the real ones.

	// Retention counts the documents a resync kept because spec.prune.mode did not
	// allow removing them. A delete observed on the cluster and not mirrored under
	// "Never" is not counted, so a "Never" folder can report zero. It is
	// information only and changes no condition.
	// +optional
	Retention *GitTargetRetentionStatus `json:"retention,omitempty"`

	// Placement is this folder's layout as the last scan resolved it: whether a
	// kustomization governs new documents, which one, and which bases the folder
	// renders but may not write. The LayoutResolved condition gives the verdict.
	// +optional
	Placement *GitTargetPlacementStatus `json:"placement,omitempty"`
}

// resolvedAtCommit and resolvedAt are not re-stamped on every scan: republishing an unchanged
// resolution would write status once per commit to the branch, whichever target made the commit.
//
// Two rules for anything added here. A field earns its place only if a reader cannot get it from
// the spec AND it varies with this folder. And nothing may depend on a placement having HAPPENED:
// every field is a fact from the last scan, available before the target has written a byte.
// No counters — those are metrics, and a counter in status is a status write per event.

// GitTargetPlacementStatus is what the last scan resolved about a GitTarget
// folder's layout.
type GitTargetPlacementStatus struct {
	// Mode is how this folder is written.
	//
	// "Plain": no kustomization governs the folder. Documents are written and
	// removed, and nothing else is touched.
	//
	// "KustomizeRoot": one kustomization governs the folder and renders nothing
	// outside it. A new document is also added to its resources:, a removed one is
	// dropped from it, and every write is checked by rendering, so kustomize can
	// refuse one.
	//
	// "KustomizeOverlay": as KustomizeRoot, and the folder also renders a base
	// outside it (readOnlyBases). An edit to a field the base sets is written into
	// the overlay where it can be (images:, replicas:) and refused with
	// WriteBoundaryRefused otherwise. Deleting an object the base supplies writes
	// a $patch: delete into the overlay.
	//
	// Empty when the folder holds more than one kustomize root
	// (LayoutResolved=Ambiguous): no new document is placed until spec.path points
	// at one of them.
	// +optional
	// +kubebuilder:validation:Enum=Plain;KustomizeRoot;KustomizeOverlay
	Mode PlacementMode `json:"mode,omitempty"`

	// RenderRoot is the kustomization directory that governs new documents,
	// relative to spec.path ("." is the folder itself). Empty for Plain, and when
	// the layout is Ambiguous.
	// +optional
	RenderRoot string `json:"renderRoot,omitempty"`

	// ReadOnlyBases are the kustomization directories this folder renders but may
	// not write, relative to spec.path, so they start with "../". Set only for
	// KustomizeOverlay. An edit to a document under one of them is refused with
	// WriteBoundaryRefused unless the overlay can express it.
	// +optional
	ReadOnlyBases []string `json:"readOnlyBases,omitempty"`

	// ResolvedAtCommit is the commit at which this layout was first seen. It does
	// not move while the layout stays the same, so a commit older than the branch
	// head means the layout has not changed since. Empty until the branch has a
	// commit.
	// +optional
	ResolvedAtCommit string `json:"resolvedAtCommit,omitempty"`

	// ResolvedAt is when this layout was first seen. Like resolvedAtCommit, it
	// does not move while the layout stays the same.
	// +optional
	ResolvedAt *metav1.Time `json:"resolvedAt,omitempty"`
}

// PlacementMode is how a GitTarget folder is written: as plain files, as a
// kustomize root, or as an overlay over a base it may not write to.
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

// Counts, never a per-document list, so the field stays bounded however many documents are
// retained. Pull-based: only as fresh as the last reconcile, which is fine for an observation and
// is another reason this must not become a condition.

// GitTargetRetentionStatus is a bounded roll-up of what this GitTarget's prune
// policy kept.
type GitTargetRetentionStatus struct {
	// Mode is the spec.prune.mode these counts were taken under, with the default
	// applied.
	// +optional
	// +kubebuilder:validation:Enum=Never;OnEvent;Always
	Mode PruneMode `json:"mode,omitempty"`

	// RetainedDocuments is how many documents the prune policy kept that have no
	// resource on the cluster. Zero means a resync found nothing to keep. An
	// absent retention block means no resync has reported yet.
	RetainedDocuments int32 `json:"retainedDocuments"`

	// LastChangedAt is when the count or the mode last changed. A resync that
	// reports the same count leaves it alone, so it does not say when retention
	// was last measured.
	// +optional
	LastChangedAt *metav1.Time `json:"lastChangedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// Three default columns; seven wrapped `kubectl get gittargets` on a normal terminal.
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Streams",type=string,JSONPath=`.status.streams.summary`
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspend`,priority=1
// +kubebuilder:printcolumn:name="Layout",type=string,JSONPath=`.status.placement.mode`,priority=1
// +kubebuilder:printcolumn:name="RenderRoot",type=string,JSONPath=`.status.placement.renderRoot`,priority=1
// +kubebuilder:printcolumn:name="LayoutResolved",type=string,JSONPath=`.status.conditions[?(@.type=="LayoutResolved")].reason`,priority=1
// +kubebuilder:printcolumn:name="GitPathAccepted",type=string,JSONPath=`.status.conditions[?(@.type=="GitPathAccepted")].status`,priority=1
// +kubebuilder:printcolumn:name="RenderMatchesLive",type=string,JSONPath=`.status.conditions[?(@.type=="RenderMatchesLive")].status`,priority=1
// +kubebuilder:printcolumn:name="ApplierClaimReady",type=string,JSONPath=`.status.conditions[?(@.type=="ApplierClaimReady")].status`,priority=1
// +kubebuilder:printcolumn:name="StreamsRunning",type=string,JSONPath=`.status.conditions[?(@.type=="StreamsRunning")].status`,priority=1
// +kubebuilder:printcolumn:name="SourceReachable",type=string,JSONPath=`.status.conditions[?(@.type=="SourceClusterReachable")].reason`,priority=1
// +kubebuilder:printcolumn:name="Encryption",type=string,JSONPath=`.spec.encryption.provider`,priority=1
// +kubebuilder:printcolumn:name="GitBranch",type=string,JSONPath=`.spec.gitBranchRef.name`,priority=1
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.spec.path`,priority=1
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GitTarget mirrors objects from a source cluster into one folder of a Git
// branch. Its WatchRules and ClusterWatchRules select the objects, its
// KubeProvider reads them, and its GitBranch writes them.
type GitTarget struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of GitTarget.
	// +required
	Spec GitTargetSpec `json:"spec"`

	// status defines the observed state of GitTarget.
	// +optional
	Status GitTargetStatus `json:"status,omitempty,omitzero"`
}

// SourceCluster is the identity the watch data plane keys a GitTarget's source cluster on: the
// referenced KubeProvider's namespace/name, so two providers with one name in two namespaces are
// two clusters. It keys the GVK→GVR registry and the watch context. It is NOT the attribution
// partition: facts are keyed by the referenced KubeProvider's AuditRoute().
func (g *GitTarget) SourceCluster() string {
	return g.KubeProviderKey().String()
}

// GitBranchKey is the GitBranch this GitTarget writes: spec.gitBranchRef, in this GitTarget's own
// namespace unless the reference names another one. An unset reference names nothing, and the zero
// key says exactly that.
func (g *GitTarget) GitBranchKey() types.NamespacedName {
	if g.Spec.GitBranchRef.Name == "" {
		return types.NamespacedName{}
	}
	namespace := g.Spec.GitBranchRef.Namespace
	if namespace == "" {
		namespace = g.Namespace
	}
	return types.NamespacedName{Namespace: namespace, Name: g.Spec.GitBranchRef.Name}
}

// KubeProviderKey is the KubeProvider this GitTarget mirrors through: spec.kubeProviderRef, in this
// GitTarget's own namespace unless the reference names another one. An unset reference names
// nothing, and the zero key says exactly that.
func (g *GitTarget) KubeProviderKey() types.NamespacedName {
	return kubeProviderKey(g.Spec.KubeProviderRef, g.Namespace)
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

// RefusalAction is the action taken when a live write is refused. See
// GitTargetSpec.OnRefusal.
type RefusalAction string

const (
	// RefusalActionIgnore records the refusal and does nothing else. It is the default.
	RefusalActionIgnore RefusalAction = "Ignore"
	// RefusalActionReapply asks the target's applier to apply the current tip, reverting the
	// refused edit.
	RefusalActionReapply RefusalAction = "Reapply"
)

// EffectiveOnRefusal returns the refusal action, resolving an omitted field to the default. The
// CRD default makes this redundant for objects that went through the API server, and not for a
// literal built in a test or by an older client. A value this release does not know, such as the
// retired "PushEmptyCommit" still stored on an object, is Ignore: an action nobody can name now is
// not consent to one.
func (g *GitTarget) EffectiveOnRefusal() RefusalAction {
	if g.Spec.OnRefusal == RefusalActionReapply {
		return RefusalActionReapply
	}
	return RefusalActionIgnore
}
