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
// A whole-object comparison compares a duration as a STRING, and a duration does not round-trip as
// one: a request created with "1m" reads into Go as a time.Duration and serializes back as "1m0s",
// so every typed update — including one that only touches metadata — was rejected as a spec
// change. CommitRequestWindow compares its durations as durations, which compares what the fields
// mean instead of how they were spelled.
// +kubebuilder:validation:XValidation:rule="self.gitTargetRef == oldSelf.gitTargetRef && has(self.message) == has(oldSelf.message) && (!has(self.message) || self.message == oldSelf.message) && has(self.window) == has(oldSelf.window) && has(self.whenNothingToCommit) == has(oldSelf.whenNothingToCommit) && (!has(self.whenNothingToCommit) || self.whenNothingToCommit == oldSelf.whenNothingToCommit)",message="CommitRequest spec is immutable after creation"
// +kubebuilder:validation:XValidation:rule="!has(self.whenNothingToCommit) || self.whenNothingToCommit != 'CommitEmpty' || has(self.message)",message="whenNothingToCommit: CommitEmpty requires spec.message, because a message is all an empty commit records"
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

	// The default is spelled out rather than {} because the API server checks a default against
	// the block's CEL rules without applying the nested field defaults first.

	// Window says which commit window this request attaches to, how long it waits for one, and
	// the timers that close the window once it has attached. They replace the GitTarget's timers
	// for that window, shorter or longer. Omitted, it is the defaults of every field below: attach
	// to the author's current or next window within 2s, then close it 2s later.
	// +optional
	// +kubebuilder:default={attach: "CurrentOrNext", attachTimeout: "2s", maxDuration: "2s"}
	Window *CommitRequestWindow `json:"window,omitempty"`

	// WhenNothingToCommit decides what the request does when it ends with nothing to commit:
	// because attachTimeout ran out with no eligible window, or because the window it attached to
	// closed, for any reason, without changing Git. Resolve finishes without a commit; CommitEmpty
	// records spec.message in an empty commit, and the Ready reason still says which of the two
	// happened. A window that belonged to another author never falls back to an empty commit, and
	// neither does a suspended GitTarget. Defaults to Resolve.
	// +optional
	// +kubebuilder:validation:Enum=Resolve;CommitEmpty
	// +kubebuilder:default=Resolve
	WhenNothingToCommit NothingToCommitAction `json:"whenNothingToCommit,omitempty"`
}

// NothingToCommitAction is what a CommitRequest does when it ends with nothing to commit.
type NothingToCommitAction string

const (
	// NothingToCommitResolve finishes the request without a commit.
	NothingToCommitResolve NothingToCommitAction = "Resolve"
	// NothingToCommitCommitEmpty records the request's message in an empty commit.
	NothingToCommitCommitEmpty NothingToCommitAction = "CommitEmpty"
)

// AttachPolicy selects the commit window a CommitRequest attaches to.
type AttachPolicy string

const (
	// AttachCurrentOrNext attaches to the author's window if one is open when the worker
	// registers the request, and otherwise to the next one a write opens.
	AttachCurrentOrNext AttachPolicy = "CurrentOrNext"
	// AttachNext closes the author's open window, under its own message, and attaches to the
	// next one a write opens. It separates work the worker already collected from work that
	// reaches it afterwards; it cannot prove a write was made after the request.
	AttachNext AttachPolicy = "Next"
)

// CommitRequestWindow is how a CommitRequest attaches to a commit window and when that window
// closes. A window is only ever opened by a write; a request waits for one and never opens one.
//
// Every duration shares the shape GitTargetCommitSpec documents: a Go duration string behind a
// pattern and a CEL bound. The immutability rule compares them as durations, for the reason
// CommitRequestSpec gives.
// +kubebuilder:validation:XValidation:rule="self.attach == oldSelf.attach && duration(self.attachTimeout) == duration(oldSelf.attachTimeout) && duration(self.maxDuration) == duration(oldSelf.maxDuration) && has(self.idleTimeout) == has(oldSelf.idleTimeout) && (!has(self.idleTimeout) || duration(self.idleTimeout) == duration(oldSelf.idleTimeout))",message="CommitRequest spec is immutable after creation"
// +kubebuilder:validation:XValidation:rule="!has(self.idleTimeout) || duration(self.idleTimeout) <= duration(self.maxDuration)",message="spec.window.idleTimeout must not exceed maxDuration"
// +kubebuilder:validation:XValidation:rule="self.attach != 'Next' || duration(self.attachTimeout) > duration('0s')",message="attach: Next needs a positive attachTimeout; with 0s it would close the author's window and give up in the same instant"
type CommitRequestWindow struct {
	// Attach selects the window: CurrentOrNext attaches to the author's window if one is open
	// when the worker registers the request, and otherwise to the next one; Next first closes the
	// author's open window, under its own message, and attaches to the next one. Defaults to
	// CurrentOrNext.
	// +optional
	// +kubebuilder:validation:Enum=CurrentOrNext;Next
	// +kubebuilder:default=CurrentOrNext
	Attach AttachPolicy `json:"attach,omitempty"`

	// AttachTimeout is how long the request waits for a window to attach to, counted from when
	// the branch worker registers it, as a Go duration string. "0s" attaches to a window already
	// open, or gives up at once. A request whose wait ran out never takes a later window. At most
	// "5m". Defaults to "2s", which covers a write's wait for its audit fact.
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+$"
	// +kubebuilder:default="2s"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('5m')",message="spec.window.attachTimeout must not exceed 5m"
	AttachTimeout *metav1.Duration `json:"attachTimeout,omitempty"`

	// IdleTimeout closes the attached window after this much silence; every write it collects
	// restarts it. Omitted, the window has no idle close and maxDuration alone ends it. At most
	// "5m".
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+$"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('5m')",message="spec.window.idleTimeout must not exceed 5m"
	IdleTimeout *metav1.Duration `json:"idleTimeout,omitempty"`

	// MaxDuration closes the attached window this long after the request attached, however much
	// keeps arriving. "0s" finalizes right after the attach. At most "5m". Defaults to "2s".
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+$"
	// +kubebuilder:default="2s"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('5m')",message="spec.window.maxDuration must not exceed 5m"
	MaxDuration *metav1.Duration `json:"maxDuration,omitempty"`
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
