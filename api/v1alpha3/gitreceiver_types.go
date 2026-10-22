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
	// Type is the payload and signature the Git host sends. "github" checks X-Hub-Signature-256,
	// "gitlab" checks X-Gitlab-Token, "gitea" checks X-Gitea-Signature, and "generic-hmac" checks
	// X-Signature: sha256=<hex HMAC of the body> over a {"repository", "branch", "after"} body,
	// for a relay of your own.
	// +required
	// +kubebuilder:validation:Enum=github;gitlab;gitea;generic-hmac
	Type ReceiverType `json:"type"`

	// SecretRef names a Secret in this GitReceiver's namespace whose "token" key is the webhook
	// secret. The token also derives status.webhookPath, so rotating it moves the path: register the
	// new path with the new secret.
	// +required
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.secretRef.name must not be empty"
	SecretRef meta.LocalObjectReference `json:"secretRef"`

	// A delivery only wakes a branch worker, which then reads the remote with its own provider's
	// credential, so admitting a namespace hands it no credential and no say over what is written.
	// The policy exists so a namespace cannot attach its providers to an endpoint whose owner did
	// not agree to serve them.

	// AccessFrom admits GitProviders in OTHER namespaces to reference this GitReceiver from
	// spec.gitReceiverRef. Its own namespace is always admitted; omitted, no other namespace is.
	// One GitReceiver can then serve a GitHub App or an organization webhook, whose one endpoint
	// delivers for repositories whose GitProviders live in many namespaces.
	// +optional
	AccessFrom *NamespaceMatcher `json:"accessFrom,omitempty"`
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

	// Conditions report whether this GitReceiver serves deliveries: the Ready summary plus the
	// kstatus Reconciling/Stalled pair. Ready is True once the token Secret is readable and the
	// receiver listener is on.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// WebhookPath is the path this GitReceiver is served on, under the receiver listener:
	// /git-push/ and an HMAC of this GitReceiver's namespace and name, keyed by the token. Point the
	// Git host's push webhook at the listener plus this path. Empty while the token Secret or its
	// "token" key is missing.
	// +optional
	WebhookPath string `json:"webhookPath,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.status.webhookPath`,priority=1
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GitReceiver is the endpoint a Git host notifies on a push. A delivery to it authenticates against
// its token, names a repository and a branch, and wakes the branch worker of each GitProvider that
// references this GitReceiver for that repository and branch. A delivery is only a hint: the branch
// worker reads the remote with its own provider's credential to find out what happened, so nothing
// a delivery says can choose a URL, a credential or a commit.
type GitReceiver struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of GitReceiver
	// +required
	Spec GitReceiverSpec `json:"spec"`

	// status defines the observed state of GitReceiver
	// +optional
	Status GitReceiverStatus `json:"status,omitempty,omitzero"`
}

// Key is this GitReceiver's namespace and name.
func (r *GitReceiver) Key() types.NamespacedName {
	return types.NamespacedName{Namespace: r.Namespace, Name: r.Name}
}

// AdmitsNamespace reports whether a GitProvider in namespace (by name and labels) may reference this
// GitReceiver: its own namespace always, another one only as spec.accessFrom admits it. A malformed
// selector is an error, never a silent allow.
func (r *GitReceiver) AdmitsNamespace(nsName string, nsLabels map[string]string) (bool, error) {
	if nsName == r.Namespace {
		return true, nil
	}
	return r.Spec.AccessFrom.Matches(nsName, nsLabels)
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
