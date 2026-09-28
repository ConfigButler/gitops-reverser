// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	meta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CommitRequestSpec defines the desired state of CommitRequest. The spec is
// immutable after creation: a CEL validation rule rejects any update that
// changes it, so a delayed audit event always acts on the spec the object was
// created with.
//
// Immutability is field-by-field rather than `self == oldSelf`, and the reason is the one thing to
// remember when adding a field here: ADD IT TO THIS RULE TOO, or it becomes quietly mutable.
//
// A whole-object comparison compares closeDelay as a STRING, and this field does not round-trip as
// one: a request created with "1m" reads into Go as a time.Duration and serializes back as "1m0s",
// so every typed update — including one that only touches metadata — was rejected as a spec
// change. Comparing it as a duration compares what the field means instead of how it was spelled.
// +kubebuilder:validation:XValidation:rule="self.gitTargetRef == oldSelf.gitTargetRef && has(self.message) == has(oldSelf.message) && (!has(self.message) || self.message == oldSelf.message) && has(self.closeDelay) == has(oldSelf.closeDelay) && (!has(self.closeDelay) || duration(self.closeDelay) == duration(oldSelf.closeDelay))",message="CommitRequest spec is immutable after creation"
type CommitRequestSpec struct {
	// GitTargetRef names the GitTarget whose open commit window to finalize.
	// The GitTarget must be in the same namespace as this CommitRequest.
	// +required
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.gitTargetRef.name must not be empty"
	GitTargetRef meta.LocalObjectReference `json:"gitTargetRef"`

	// Message is an optional commit message for the finalized commit. When
	// omitted, the target's liveTemplate is used. Template-like text remains literal.
	//
	// When present it is limited to 1-1024 Unicode characters and used
	// verbatim as the commit message. Newlines are allowed so a subject and
	// body can be supplied; all other ASCII control characters (including tab
	// and carriage return) are rejected. Whitespace-only messages are rejected.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^[^\x00-\x09\x0B-\x1F\x7F]*$`
	// +kubebuilder:validation:XValidation:rule="self.matches(r'[^\\s\\x{0085}\\x{00A0}\\x{1680}\\x{2000}-\\x{200A}\\x{2028}\\x{2029}\\x{202F}\\x{205F}\\x{3000}]')",message="message must contain a non-whitespace character"
	Message string `json:"message,omitempty"`

	// A pointer, not a bare metav1.Duration, so that an omitted field and an explicit "0s" stay
	// distinguishable once the default is stored: a schema default on a bare value would make
	// "finalize immediately" inexpressible from a typed Go client, whose zero value is not
	// serialized, and would erase the distinction a cluster-level default needs.
	//
	// The pattern, the CEL bound and the wider-than-Flux unit set are the shape every duration in
	// this API takes, and GitTargetCommitSpec.Window carries the reasoning for all three.

	// CloseDelay sets the finalize deadline from the worker's first receipt, as a Go duration
	// string ("2s", "750ms", "1m"). Time waiting for a matching window consumes this delay;
	// repeated receipt keeps the deadline. Normal flush triggers can close an attached window
	// early, carrying its message. A request claims at most one open window and cannot rename a
	// finalized commit. At most "5m".
	// Defaults to "2s", which covers the time a write spends waiting for its audit fact before
	// the commit window opens; an explicit "0s" requests immediate finalization and will usually
	// find nothing pending. A delay does not reserve a transaction.
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+$"
	// +kubebuilder:default="2s"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('5m')",message="closeDelay must not exceed 5m"
	CloseDelay *metav1.Duration `json:"closeDelay,omitempty"`
}

// CommitRequestStatus defines the observed state of CommitRequest. Progress and
// outcome are reported entirely through conditions (kstatus-compatible), so the
// object carries no lifecycle phase string:
//
//   - Ready (summary): True once the request reached a terminal outcome that is not
//     an error — a pushed commit, or a benign no-commit (nothing to save, already
//     present, or a foreign open window). False while in progress or when it failed.
//   - Reconciling / Stalled: the kstatus progress / blocked pair. Reconciling=True
//     while finalizing; Stalled=True when the finalize failed and needs attention.
//   - AuthorAttributed (domain): binary and settled immediately. True
//     (AttributedFromAdmission) when the submitter captured at admission named the
//     commit author; False (CommitterFallback) when capture ran but no admission record
//     exists, or False (AuthorCaptureDisabled) when capture is disabled. In either False
//     case the request claims no actor. False is not a failure and does not affect Ready;
//     the final Git author remains the matching watch window's author.
//   - Pushed (domain): True once the commit is in the remote repository.
type CommitRequestStatus struct {
	// ObservedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions report the request's progress and terminal outcome: the Ready
	// summary, the kstatus Reconciling/Stalled pair, and the domain conditions
	// AuthorAttributed and Pushed.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Branch is the Git branch the GitTarget commits to. Populated once the
	// finalize resolves.
	// +optional
	Branch string `json:"branch,omitempty"`

	// Commit is the resulting commit hash. Set when the commit was pushed (Pushed=True).
	// +optional
	Commit string `json:"commit,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="GitTarget",type=string,JSONPath=`.spec.gitTargetRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Commit",type=string,JSONPath=`.status.commit`
// +kubebuilder:printcolumn:name="AuthorAttributed",type=string,JSONPath=`.status.conditions[?(@.type=="AuthorAttributed")].status`,priority=1
// +kubebuilder:printcolumn:name="Pushed",type=string,JSONPath=`.status.conditions[?(@.type=="Pushed")].status`,priority=1
// +kubebuilder:printcolumn:name="Branch",type=string,JSONPath=`.status.branch`,priority=1
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// CommitRequest is a one-shot "save" signal: creating one finalizes the open
// commit window for the referenced GitTarget instead of waiting for the
// silence timer. The resulting commit hash is reported back in status.
type CommitRequest struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of CommitRequest
	// +required
	Spec CommitRequestSpec `json:"spec"`

	// status defines the observed state of CommitRequest
	// +optional
	Status CommitRequestStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// CommitRequestList contains a list of CommitRequest.
type CommitRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []CommitRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CommitRequest{}, &CommitRequestList{})
}
