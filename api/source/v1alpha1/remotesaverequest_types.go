// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A RemoteSaveRequest is the source-cluster twin of a configbutler.ai SaveRequest, for the people who edit
// a source cluster and cannot reach the cluster GitOps Reverser runs in. It is its own kind, in its own
// group, rather than a SaveRequest with another schema: it names no GitTarget, because its creator
// cannot see them, its author comes from the source cluster's audit events rather than from admission,
// and it is installed alone. docs/future/source-side-save-plan.md has the reasoning.
//
// The spec is immutable, compared as SaveRequestSpec compares it: durations as durations, so a typed
// client's re-spelling of "1m" as "1m0s" is not a change.

// RemoteSaveRequestSpec defines the desired state of RemoteSaveRequest. It
// is immutable.
// +kubebuilder:validation:XValidation:rule="has(self.message) == has(oldSelf.message) && (!has(self.message) || self.message == oldSelf.message) && has(self.window) == has(oldSelf.window) && has(self.catchUp) == has(oldSelf.catchUp) && (!has(self.catchUp) || self.catchUp == oldSelf.catchUp)",message="RemoteSaveRequest spec is immutable after creation"
type RemoteSaveRequestSpec struct {
	// Message is the commit message this request asks for. Omitted, each
	// GitTarget's spec.commit.message.liveTemplate words the commit.
	//
	// The text is never evaluated as a template. If a GitTarget sets
	// spec.commit.message.requestTemplate, that template wraps this text;
	// otherwise the text is committed as written.
	//
	// It is 1–1024 characters and must contain a non-whitespace character.
	// Newlines are allowed, for a subject and body; tabs and other control
	// characters are rejected.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^[^\x00-\x09\x0B-\x1F\x7F]*$`
	// +kubebuilder:validation:XValidation:rule="self.matches(r'[^\\s\\x{0085}\\x{00A0}\\x{1680}\\x{2000}-\\x{200A}\\x{2028}\\x{2029}\\x{202F}\\x{205F}\\x{3000}]')",message="message must contain a non-whitespace character"
	Message string `json:"message,omitempty"`

	// The default is spelled out rather than {} because the API server checks a default against
	// the block's CEL rules without applying the nested field defaults first.

	// Window says how long the request waits for a commit window with the
	// edits it saves, and when that window closes once it has. Omitted, the
	// request waits 5s and closes the window 2s after it attached.
	// +optional
	// +kubebuilder:default={attachTimeout: "5s", maxDuration: "2s"}
	Window *RemoteSaveRequestWindow `json:"window,omitempty"`

	// CatchUp decides when the applier of each GitTarget is asked to apply
	// the branch after this request pushes a commit. "AsDefinedOnClaim"
	// leaves it to the applier claim's spec.catchUpAfter. "AfterPush" asks
	// right after the push, and status.targets reports how it went. The
	// cluster already holds the saved objects. Defaults to
	// "AsDefinedOnClaim".
	// +optional
	// +kubebuilder:validation:Enum=AsDefinedOnClaim;AfterPush
	// +kubebuilder:default=AsDefinedOnClaim
	CatchUp CatchUpPolicy `json:"catchUp,omitempty"`
}

// CatchUpPolicy is when a RemoteSaveRequest's commit is applied by a
// GitTarget's applier.
type CatchUpPolicy string

const (
	// CatchUpAsDefinedOnClaim leaves the apply to the applier claim's spec.catchUpAfter.
	CatchUpAsDefinedOnClaim CatchUpPolicy = "AsDefinedOnClaim"
	// CatchUpAfterPush asks the applier to apply the tip right after the commit is pushed.
	CatchUpAfterPush CatchUpPolicy = "AfterPush"
)

// RemoteSaveRequestWindow is how long a RemoteSaveRequest waits for a commit
// window, and when that window closes. A window is only ever opened by an
// edit; a request waits for one and never opens one.
// +kubebuilder:validation:XValidation:rule="duration(self.attachTimeout) == duration(oldSelf.attachTimeout) && duration(self.maxDuration) == duration(oldSelf.maxDuration) && has(self.idleTimeout) == has(oldSelf.idleTimeout) && (!has(self.idleTimeout) || duration(self.idleTimeout) == duration(oldSelf.idleTimeout))",message="RemoteSaveRequest spec is immutable after creation"
// +kubebuilder:validation:XValidation:rule="!has(self.idleTimeout) || duration(self.idleTimeout) <= duration(self.maxDuration)",message="spec.window.idleTimeout must not exceed maxDuration"
type RemoteSaveRequestWindow struct {
	// AttachTimeout is how long the request waits for a commit window, as a
	// Go duration, from when GitOps Reverser picks the request up, which is
	// after its own author was found. A request whose wait ran out never
	// takes a later window. At most "5m". Defaults to "5s".
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+$"
	// +kubebuilder:default="5s"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('5m')",message="spec.window.attachTimeout must not exceed 5m"
	AttachTimeout *metav1.Duration `json:"attachTimeout,omitempty"`

	// IdleTimeout closes the attached window this long after its last edit,
	// as a Go duration. Omitted, only maxDuration closes it. At most "5m",
	// and not more than maxDuration.
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+$"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('5m')",message="spec.window.idleTimeout must not exceed 5m"
	IdleTimeout *metav1.Duration `json:"idleTimeout,omitempty"`

	// MaxDuration closes the attached window this long after the request
	// attached, however many edits keep arriving, as a Go duration. At most
	// "5m". Defaults to "2s".
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+$"
	// +kubebuilder:default="2s"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('5m')",message="spec.window.maxDuration must not exceed 5m"
	MaxDuration *metav1.Duration `json:"maxDuration,omitempty"`
}

// RemoteSaveRequestStatus defines the observed state of RemoteSaveRequest.
type RemoteSaveRequestStatus struct {
	// ObservedGeneration is the latest generation observed by GitOps Reverser.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Author is who created this request, as this cluster's audit events name
	// them. Empty when they name nobody: then the request saves only edits
	// that name nobody either.
	// +optional
	Author string `json:"author,omitempty"`

	// GitTargets is the save in each GitTarget that admitted this request.
	// +optional
	// +listType=map
	// +listMapKey=namespace
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	GitTargets []RemoteSaveRequestGitTarget `json:"gitTargets,omitempty"`

	// Conditions are the latest observations of this RemoteSaveRequest's state.
	// Ready is True once the request ended without error in every GitTarget,
	// with reason "Committed" when one of them committed, and otherwise the
	// reason of the first: "NoWindow", "AlreadyPresent", "WindowMismatch",
	// "NoGitTarget" when no GitTarget mirrors this namespace, or
	// "AuthorUnresolved" when this cluster sends audit events and none named
	// the request's creator. Ready=False with Stalled=True is a
	// failure, with reason "NotAllowed" when GitTargets mirror this namespace
	// and none admits the request.
	//
	// Applied, with spec.catchUp "AfterPush", is True once every GitTarget's
	// applier applied a revision that holds its commit, or needed none, and
	// False only when a branch lost the commit. It never changes Ready.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// RemoteSaveRequestGitTarget is the save in one GitTarget.
type RemoteSaveRequestGitTarget struct {
	// Namespace is the GitTarget's namespace, on the cluster GitOps Reverser
	// runs in.
	// +required
	Namespace string `json:"namespace"`

	// Name is the GitTarget's name.
	// +required
	Name string `json:"name"`

	// Reason is how the save ended in this GitTarget, as the Ready reasons
	// above: "Committed", "NoWindow", "AlreadyPresent", "WindowMismatch", or
	// "Failed". Empty while it runs.
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message says more about the reason.
	// +optional
	Message string `json:"message,omitempty"`

	// Branch is the branch the save was committed on.
	// +optional
	Branch string `json:"branch,omitempty"`

	// Commit is the commit the save made, once it is on the remote.
	// +optional
	Commit string `json:"commit,omitempty"`

	// Applied is how the apply the save asked for went, with spec.catchUp
	// "AfterPush": "Applying", "Retrying", "Applied", "NothingToApply",
	// "NoApplierClaim", "ApplierGone", "CommitDropped" or "Lost".
	// +optional
	Applied string `json:"applied,omitempty"`

	// AppliedRevision is the revision the applier applied.
	// +optional
	AppliedRevision string `json:"appliedRevision,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Author",type=string,JSONPath=`.status.author`
// +kubebuilder:printcolumn:name="Applied",type=string,JSONPath=`.status.conditions[?(@.type=="Applied")].status`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RemoteSaveRequest asks GitOps Reverser to save its creator's edits: the
// commit window of each GitTarget that mirrors this namespace and admits the
// request, in spec.allowedRemoteSaveRequests, gets its message and closes on
// its timers. The window holds the creator's edits anywhere that GitTarget
// mirrors, not only in this namespace. Create it on the cluster the edits
// were made on, as JSON. Its creator is found from this cluster's audit
// events, as the edits' authors are, so it saves only its creator's edits; on
// a cluster with no audit it names nobody, and saves edits that name nobody.
// Its spec is immutable, and status reports each GitTarget's commit.
type RemoteSaveRequest struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of RemoteSaveRequest. It is immutable after
	// creation.
	// +required
	Spec RemoteSaveRequestSpec `json:"spec"`

	// status defines the observed state of RemoteSaveRequest.
	// +optional
	Status RemoteSaveRequestStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// RemoteSaveRequestList contains a list of RemoteSaveRequest.
type RemoteSaveRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []RemoteSaveRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RemoteSaveRequest{}, &RemoteSaveRequestList{})
}
