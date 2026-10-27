// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	meta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// CommitRequestSpec defines the desired state of CommitRequest. The spec is
// immutable after creation: a CEL validation rule rejects any update that
// changes it, so a delayed audit event always acts on the spec the object was
// created with.
//
// Immutability is field-by-field rather than `self == oldSelf`, and the reason
// is the one thing to remember when adding a field here: ADD IT TO THIS RULE
// TOO, or it becomes quietly mutable.
//
// A whole-object comparison compares a duration as a STRING, and a duration
// does not round-trip as one: a request created with "1m" reads into Go as a
// time.Duration and serializes back as "1m0s", so every typed update —
// including one that only touches metadata — was rejected as a spec change.
// CommitRequestWindow compares its durations as durations, which compares what
// the fields mean instead of how they were spelled.
// +kubebuilder:validation:XValidation:rule="self.gitTargetRef == oldSelf.gitTargetRef && has(self.message) == has(oldSelf.message) && (!has(self.message) || self.message == oldSelf.message) && has(self.window) == has(oldSelf.window) && has(self.whenNothingToCommit) == has(oldSelf.whenNothingToCommit) && (!has(self.whenNothingToCommit) || self.whenNothingToCommit == oldSelf.whenNothingToCommit) && has(self.catchUp) == has(oldSelf.catchUp) && (!has(self.catchUp) || self.catchUp == oldSelf.catchUp)",message="CommitRequest spec is immutable after creation"
// +kubebuilder:validation:XValidation:rule="!has(self.whenNothingToCommit) || self.whenNothingToCommit != 'CommitEmpty' || has(self.message)",message="whenNothingToCommit: CommitEmpty requires spec.message, because a message is all an empty commit records"
type CommitRequestSpec struct {
	// GitTargetRef names the GitTarget whose commit window this request
	// attaches to: in this namespace, or namespace names another. The
	// GitTarget's spec.allowedCommitRequests must admit this namespace, or the
	// request fails with reason "NotAllowed".
	// +required
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.gitTargetRef.name must not be empty"
	GitTargetRef meta.NamespacedObjectReference `json:"gitTargetRef"`

	// Message is the commit message this request asks for. Omitted, the
	// GitTarget's spec.commit.message.liveTemplate words the commit.
	//
	// The text is never evaluated as a template. If the GitTarget sets
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

	// Window says which commit window this request attaches to, how long it waits
	// for one, and the timers that close the window once it has. They replace the
	// GitTarget's timers for that window. A request never opens a window: only a
	// write does. Omitted, the request attaches to the author's current or next
	// window within 5s and closes it 2s later.
	// +optional
	// +kubebuilder:default={attach: "CurrentOrNext", attachTimeout: "5s", maxDuration: "2s"}
	Window *CommitRequestWindow `json:"window,omitempty"`

	// WhenNothingToCommit decides what happens when the request ends with nothing
	// to commit: attachTimeout ran out with no window, or the window closed
	// without changing Git. "Resolve" finishes without a commit; "CommitEmpty"
	// records spec.message in an empty commit. The Ready reason says which case
	// happened. A window that belonged to another author never gets an empty
	// commit, and a request on a GitTarget that may not be written (suspended, or
	// render fidelity not established) fails either way. Defaults to "Resolve".
	// +optional
	// +kubebuilder:validation:Enum=Resolve;CommitEmpty
	// +kubebuilder:default=Resolve
	WhenNothingToCommit NothingToCommitAction `json:"whenNothingToCommit,omitempty"`

	// CatchUp decides when the GitTarget's applier is asked to apply the branch
	// after this request pushes a commit. "AsDefinedOnClaim" leaves it to the
	// applier claim's spec.catchUpAfter. "AfterPush" asks right after the push.
	// The apply takes the latest branch tip, which may hold later commits, and
	// saves pushed close together can share one apply; the Applied condition
	// reports how it went. The cluster already holds the saved objects:
	// the apply brings the applier's own state, such as a Kustomization's
	// status.lastAppliedRevision, to the tip. Defaults to "AsDefinedOnClaim".
	// +optional
	// +kubebuilder:validation:Enum=AsDefinedOnClaim;AfterPush
	// +kubebuilder:default=AsDefinedOnClaim
	CatchUp CatchUpPolicy `json:"catchUp,omitempty"`
}

// CatchUpPolicy is when a CommitRequest's commit is applied by the GitTarget's
// applier.
type CatchUpPolicy string

const (
	// CatchUpAsDefinedOnClaim leaves the apply to the applier claim's spec.catchUpAfter.
	CatchUpAsDefinedOnClaim CatchUpPolicy = "AsDefinedOnClaim"
	// CatchUpAfterPush asks the applier to apply the tip right after the commit is pushed.
	CatchUpAfterPush CatchUpPolicy = "AfterPush"
)

// NothingToCommitAction is what a CommitRequest does when it ends with nothing
// to commit.
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
	// reaches it afterward; it cannot prove a write was made after the request.
	AttachNext AttachPolicy = "Next"
)

// CommitRequestWindow is how a CommitRequest attaches to a commit window and
// when that window closes. A window is only ever opened by a write; a request
// waits for one and never opens one.
//
// Every duration shares the shape GitTargetCommitSpec documents: a Go duration
// string behind a pattern and a CEL bound. The immutability rule compares them
// as durations, for the reason CommitRequestSpec gives.
// +kubebuilder:validation:XValidation:rule="self.attach == oldSelf.attach && duration(self.attachTimeout) == duration(oldSelf.attachTimeout) && duration(self.maxDuration) == duration(oldSelf.maxDuration) && has(self.idleTimeout) == has(oldSelf.idleTimeout) && (!has(self.idleTimeout) || duration(self.idleTimeout) == duration(oldSelf.idleTimeout))",message="CommitRequest spec is immutable after creation"
// +kubebuilder:validation:XValidation:rule="!has(self.idleTimeout) || duration(self.idleTimeout) <= duration(self.maxDuration)",message="spec.window.idleTimeout must not exceed maxDuration"
// +kubebuilder:validation:XValidation:rule="self.attach != 'Next' || duration(self.attachTimeout) > duration('0s')",message="attach: Next needs a positive attachTimeout; with 0s it would close the author's window and give up in the same instant"
type CommitRequestWindow struct {
	// Attach selects the window. "CurrentOrNext" attaches to the author's open
	// window, or to the next one a write opens. "Next" closes the author's open
	// window under its own message first, and attaches to the next one; it
	// separates earlier writes from later ones, but cannot prove a write was made
	// after the request. Defaults to "CurrentOrNext".
	// +optional
	// +kubebuilder:validation:Enum=CurrentOrNext;Next
	// +kubebuilder:default=CurrentOrNext
	Attach AttachPolicy `json:"attach,omitempty"`

	// AttachTimeout is how long the request waits for a window, as a Go duration,
	// from when GitOps Reverser picks the request up, which is after its own
	// author was found. "0s" attaches only to a window already open. A request
	// whose wait ran out never takes a later window. At most "5m". Defaults to
	// "5s", which covers a write's wait for its audit event.
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+$"
	// +kubebuilder:default="5s"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('5m')",message="spec.window.attachTimeout must not exceed 5m"
	AttachTimeout *metav1.Duration `json:"attachTimeout,omitempty"`

	// IdleTimeout closes the attached window this long after its last write, as a
	// Go duration. Omitted, only maxDuration closes it. At most "5m", and not more
	// than maxDuration.
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+$"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('5m')",message="spec.window.idleTimeout must not exceed 5m"
	IdleTimeout *metav1.Duration `json:"idleTimeout,omitempty"`

	// MaxDuration closes the attached window this long after the request attached,
	// however many writes keep arriving, as a Go duration. "0s" commits right
	// after attaching. At most "5m". Defaults to "2s".
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+$"
	// +kubebuilder:default="2s"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('5m')",message="spec.window.maxDuration must not exceed 5m"
	MaxDuration *metav1.Duration `json:"maxDuration,omitempty"`
}

// CommitRequestStatus defines the observed state of CommitRequest. Progress
// and outcome are reported entirely through conditions (kstatus-compatible),
// so the object carries no lifecycle phase string:
//
//   - Ready (summary): True once the request reached a terminal outcome that
//     is not an error — a pushed commit, or a benign no-commit (nothing to
//     save, already present, or a foreign open window). Unknown while in
//     progress, from the first status write, which records the author. False
//     when it failed.
//   - Reconciling / Stalled: the kstatus progress / blocked pair.
//     Reconciling=True while finalizing; Stalled=True when the finalize failed
//     and needs attention.
//   - Pushed (domain): True once the commit is in the remote repository.
//   - Applied (domain): with spec.catchUp "AfterPush", whether the applier
//     caught up with the commit. It never affects Ready.
type CommitRequestStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Author is who created this request, as the audit events of the cluster
	// it was created on name them. Empty when they name nobody, or when author
	// attribution is off: then the request saves only writes that name nobody
	// either.
	// +optional
	Author string `json:"author,omitempty"`

	// Conditions are the latest observations of this CommitRequest's state.
	// Reconciling and Stalled follow kstatus.
	//
	// Three conditions say how far the save got:
	//
	// - Ready (kstatus): Unknown while the save runs. True once the request
	//   ended without error, with a pushed commit or with nothing to save; it
	//   never waits for an apply. False when it failed: reason NotAllowed,
	//   GitTargetNotFound, SaveInProgress or FinalizeFailed.
	// - Pushed: True once the commit is on the remote.
	// - Applied: set only with spec.catchUp "AfterPush", once Ready is True.
	//
	// Applied says whether the cluster's applier caught up with the commit:
	//
	// - Unknown, reason Applying or Retrying: the apply runs, or did not
	//   complete and is asked again; the message says why.
	// - True, reason Applied: the applier applied a revision that holds the
	//   commit, named in status.appliedRevision.
	// - True, reason NothingToApply, NoApplierClaim or ApplierGone: no apply
	//   was needed. The request pushed no commit, or the GitTarget names no
	//   applier claim, or stopped naming the one asked; the cluster already
	//   holds the saved objects.
	// - False, reason CommitDropped: the branch no longer holds the commit;
	//   its history was rewritten after the push. Only this is False.
	// - Unknown, reason Lost: GitOps Reverser restarted before it knew.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// GitTargetUID is the UID of the GitTarget whose spec.allowedCommitRequests
	// admitted this request. A GitTarget with another UID, such as one created
	// after the request under the same name, must admit it again.
	// +optional
	GitTargetUID string `json:"gitTargetUID,omitempty"`

	// Branch is the branch the commit is made on. Set once the request ends.
	// +optional
	Branch string `json:"branch,omitempty"`

	// Commit is the commit hash. Set once the commit is pushed (Pushed=True).
	// +optional
	Commit string `json:"commit,omitempty"`

	// AppliedRevision is the revision the GitTarget's applier reported applying
	// for this request, as in a Kustomization's status.lastAppliedRevision. It
	// holds the request's commit, and may be a later one. Set once Applied is
	// True with reason Applied.
	// +optional
	AppliedRevision string `json:"appliedRevision,omitempty"`
}

// The last line of the kind's description is its schema revision, which a cluster that installed the
// CRD on its own, as a source cluster does, can be judged by. It ships in the CRD, so the same
// published schema reads it back: CommitRequestSchemaRevision and CommitRequestSchemaFloor below
// must say the same, and TestCommitRequestSchemaRevision fails when the generated CRD changes and
// they do not move. Raise the revision with every change to the generated CRD, and the floor ("compatible
// from") with a change that breaks an older reader or writer: a new required field, a removed field,
// or a changed meaning.

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="GitTarget",type=string,JSONPath=`.spec.gitTargetRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Author",type=string,JSONPath=`.status.author`
// +kubebuilder:printcolumn:name="Commit",type=string,JSONPath=`.status.commit`
// +kubebuilder:printcolumn:name="Pushed",type=string,JSONPath=`.status.conditions[?(@.type=="Pushed")].status`,priority=1
// +kubebuilder:printcolumn:name="Applied",type=string,JSONPath=`.status.conditions[?(@.type=="Applied")].status`,priority=1
// +kubebuilder:printcolumn:name="Branch",type=string,JSONPath=`.status.branch`,priority=1
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// CommitRequest asks for a save: it attaches to its author's current or next
// commit window on a GitTarget, closes that window on its own timers, and
// gives the commit its message. Create it on the cluster GitOps Reverser runs
// in, or on a source cluster whose KubeProvider sets spec.commitRequests. Its
// spec is immutable, and status.commit reports the commit once it is on the
// remote.
//
// Schema revision 1, compatible from revision 1.
type CommitRequest struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of CommitRequest. It is immutable after
	// creation.
	// +required
	Spec CommitRequestSpec `json:"spec"`

	// status defines the observed state of CommitRequest.
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

// GitTargetKey is the GitTarget spec.gitTargetRef names: in the request's namespace unless the ref
// names another.
func (s *CommitRequest) GitTargetKey() types.NamespacedName {
	namespace := s.Spec.GitTargetRef.Namespace
	if namespace == "" {
		namespace = s.Namespace
	}
	return types.NamespacedName{Namespace: namespace, Name: s.Spec.GitTargetRef.Name}
}

// CommitRequestSchemaRevision and CommitRequestSchemaFloor are the revision of the CommitRequest
// schema this operator was built with, and the oldest revision it can serve: the last line of the
// kind's description.
const (
	CommitRequestSchemaRevision = 1
	CommitRequestSchemaFloor    = 1
)

func init() {
	SchemeBuilder.Register(&CommitRequest{}, &CommitRequestList{})
}
