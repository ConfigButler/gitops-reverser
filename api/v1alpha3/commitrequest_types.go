// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CommitRequestSpec defines the desired state of CommitRequest. The spec is
// immutable after creation: a CEL validation rule rejects any update that
// changes it, so a delayed audit event always acts on the spec the object was
// created with.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="CommitRequest spec is immutable after creation"
type CommitRequestSpec struct {
	// TargetRef names the GitTarget whose open commit window to finalize.
	// The GitTarget must be in the same namespace as this CommitRequest.
	// +required
	TargetRef LocalTargetReference `json:"targetRef"`

	// Message is an optional commit message for the finalized commit. When
	// omitted, the generated grouped-commit message is used.
	//
	// When present it is limited to 1-1024 Unicode characters and used
	// verbatim as the commit message. Newlines are allowed so a subject and
	// body can be supplied; all other ASCII control characters (including tab
	// and carriage return) are rejected.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^[^\x00-\x09\x0B-\x1F\x7F]*$`
	Message string `json:"message,omitempty"`

	// CloseDelaySeconds optionally delays closing the open commit window for this
	// many seconds after the CommitRequest is attributed, acting as an extra collect
	// window: changes the author makes in the meantime still join the open commit
	// window and are included in the resulting commit. Omitted or 0 closes the window
	// as soon as the CommitRequest is attributed to its author. The window can still
	// be closed earlier by another author's change or by the provider's commit window
	// timer, exactly as without a CommitRequest.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=300
	CloseDelaySeconds int32 `json:"closeDelaySeconds,omitempty"`

	// Author names the human this commit is for, instead of deriving them from an
	// apiserver audit fact. It exists for an authenticated control plane that already
	// knows who the user is — one that verified their token before impersonating them —
	// and for any cluster whose apiserver audit flags the operator cannot set.
	//
	// Asserting an author is a privilege, not a field anyone may set. It is honored only
	// when the requester holds the `assert-author` verb on the named GitTarget, in the
	// style of `bind`, `escalate` and `impersonate`:
	//
	//	rules:
	//	  - apiGroups: ["configbutler.ai"]
	//	    resources: ["gittargets"]
	//	    resourceNames: ["tenants"]
	//	    verbs: ["assert-author"]
	//
	// The check runs in the validate-operator-types admission webhook, which denies an
	// unauthorized create. Because that webhook is failurePolicy: Ignore by design, the
	// controller is the real gate: it honors this field only when an admission record
	// exists for the object AND that record carries the authorized verdict. Without one —
	// the webhook is off, was bypassed, or Redis is not configured — the assertion is
	// ignored, the commit is authored by the configured committer, and the request reports
	// AuthorAttributed=False with reason AuthorAssertionUnverified.
	//
	// An asserted author attaches to any open commit window for the target, not only to
	// one whose audit-derived author matches: the assertion is a statement about the
	// commit being made, not a claim to be the actor the audit stream recorded. It becomes
	// the commit's author signature; the committer stays the operator's configured
	// identity.
	// +optional
	Author *CommitAuthor `json:"author,omitempty"`
}

// CommitAuthor is the Git author identity a privileged client asserts for a commit.
// Neither field is verified to correspond to a real identity: they are what the trusted
// control plane says they are. Granting `assert-author` grants the ability to write any
// author into the repository's history — treat it exactly like granting `impersonate`.
type CommitAuthor struct {
	// Name is the Git author name, used verbatim in the commit's author header.
	// ASCII control characters, angle brackets and newlines are rejected because they
	// would let a name forge a signature header.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1F\x7F<>]*$`
	Name string `json:"name"`

	// Email is the Git author email. When omitted, a stable synthetic address is derived
	// from Name, exactly as it is for an audit-attributed author with no email claim.
	// +optional
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=254
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`
	Email string `json:"email,omitempty"`
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
//     (AuthorAsserted) when spec.author named the commit author and the requester was
//     authorized to assert it; True (AttributedFromAdmission) when the submitter captured
//     at admission named the commit author; False (CommitterFallback) when no admission
//     record exists — the validate-operator-types webhook is not configured — and the
//     commit is authored by the configured committer; False (AuthorAssertionUnverified)
//     when spec.author was set but no authorized admission record backs it, so the
//     assertion was ignored. False is not a failure and does not affect Ready.
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

	// SHA is the resulting commit SHA. Set when the commit was pushed (Pushed=True).
	// +optional
	SHA string `json:"sha,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="GitTarget",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="SHA",type=string,JSONPath=`.status.sha`
// +kubebuilder:printcolumn:name="AuthorAttributed",type=string,JSONPath=`.status.conditions[?(@.type=="AuthorAttributed")].status`,priority=1
// +kubebuilder:printcolumn:name="Pushed",type=string,JSONPath=`.status.conditions[?(@.type=="Pushed")].status`,priority=1
// +kubebuilder:printcolumn:name="Branch",type=string,JSONPath=`.status.branch`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// CommitRequest is a one-shot "save" signal: creating one finalizes the open
// commit window for the referenced GitTarget instead of waiting for the
// silence timer. The resulting commit SHA is reported back in status.
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
