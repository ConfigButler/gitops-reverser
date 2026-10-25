// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"

	meta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// The names are Flux's Receiver's, type values included, so a Flux user already knows them. The
// kind is not called Receiver because Flux's Receiver sits in the same clusters.

// GitReceiverSpec defines the desired state of GitReceiver.
type GitReceiverSpec struct {
	// Type is the payload and signature the Git host sends: "github" (X-Hub-Signature-256),
	// "gitlab" (X-Gitlab-Token), "gitea" (X-Gitea-Signature), or "generic-hmac" for a relay of
	// your own (X-Signature: sha256=<hex HMAC of the body>, over a {"repository", "branch",
	// "after"} body).
	// +required
	// +kubebuilder:validation:Enum=github;gitlab;gitea;generic-hmac
	Type ReceiverType `json:"type"`

	// SecretRef names the Secret, in this namespace, whose "token" key is the webhook secret. The
	// token also determines status.webhookPath, so rotating it moves the path: register the new
	// path with the new secret.
	// +required
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.secretRef.name must not be empty"
	SecretRef meta.LocalObjectReference `json:"secretRef"`
}

// ReceiverType is the payload format a receiver accepts. The values are Flux's Receiver types.
type ReceiverType string

const (
	// ReceiverTypeGitHub is a GitHub push event.
	ReceiverTypeGitHub ReceiverType = "github"
	// ReceiverTypeGitLab is a GitLab push event.
	ReceiverTypeGitLab ReceiverType = "gitlab"
	// ReceiverTypeGitea is a Gitea push event.
	ReceiverTypeGitea ReceiverType = "gitea"
	// ReceiverTypeGenericHMAC is a {"repository", "branch", "after"} body signed as Flux's
	// generic-hmac Receiver expects.
	ReceiverTypeGenericHMAC ReceiverType = "generic-hmac"
)

// ReceiverTokenKey is the Secret key holding a receiver's token, the key a Flux Receiver Secret uses.
const ReceiverTokenKey = "token"

// ReceiverPathPrefix is the prefix every receiver path starts with.
const ReceiverPathPrefix = "/git-push/"

// A path that names the GitReceiver would let anybody who can reach the listener list which
// namespaces and receivers exist. Like Flux's Receiver, the path is derived from the token, so
// only whoever holds the token can find it; unlike Flux's sha256(token+name+namespace), it is an
// HMAC over "namespace/name", which no two receivers sharing a token can collide on.

// ReceiverPath is the path a GitReceiver is served on: the prefix and the hex HMAC-SHA256 of
// "<namespace>/<name>", keyed by the receiver token.
func ReceiverPath(token []byte, namespace, name string) string {
	mac := hmac.New(sha256.New, token)
	mac.Write([]byte(namespace + "/" + name))
	return ReceiverPathPrefix + hex.EncodeToString(mac.Sum(nil))
}

// GitReceiverStatus defines the observed state of GitReceiver.
type GitReceiverStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions are the latest observations of this GitReceiver's state. Ready, Reconciling and
	// Stalled follow kstatus; Ready is True once the token is readable and the receiver listener
	// is on.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// WebhookPath is the path this GitReceiver is served on, under the receiver listener: /git-push/
	// and an HMAC of its namespace and name, keyed by the token. Point the Git host's push webhook
	// at the listener's address plus this path. Empty while the token is missing.
	// +optional
	WebhookPath string `json:"webhookPath,omitempty"`
}

// A delivery is routed by the repository it names, not by a reference from each GitProvider: a push
// event carries little more than a repository and a branch, and a reference bought nothing but a
// line of wiring per provider and, for a GitHub App, an admission in every namespace. Waking across
// namespaces grants nothing, because a woken worker reads the remote with its own provider's
// credential, and it holds at most one pending look however many deliveries arrive. That bounds the
// queue, not the rate: a stream of deliveries can keep a worker reading continuously, an
// availability cost accepted without a per-worker cooldown.

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.status.webhookPath`,priority=1
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GitReceiver is the endpoint a Git host notifies when it receives a push. A delivery wakes every
// GitBranch of every GitProvider, in any namespace, whose spec.url names the delivery's
// repository, so pushes are picked up at once instead of at the next refresh. A GitProvider that
// reaches the host through another host name or port is not matched. A delivery is only a hint:
// the branch is then read with its own GitProvider's credential, so a delivery cannot choose a
// URL, a credential or a commit. GitBranch.status.lastPushNoticeAt shows deliveries arriving.
type GitReceiver struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of GitReceiver.
	// +required
	Spec GitReceiverSpec `json:"spec"`

	// status defines the observed state of GitReceiver.
	// +optional
	Status GitReceiverStatus `json:"status,omitempty,omitzero"`
}

// Key is this GitReceiver's namespace and name.
func (r *GitReceiver) Key() types.NamespacedName {
	return types.NamespacedName{Namespace: r.Namespace, Name: r.Name}
}

// +kubebuilder:object:root=true

// GitReceiverList contains a list of GitReceiver.
type GitReceiverList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []GitReceiver `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GitReceiver{}, &GitReceiverList{})
}
