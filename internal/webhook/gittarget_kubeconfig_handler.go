// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"fmt"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authzv1client "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// ValidateGitTargetKubeConfigPath is the fail-closed validating admission endpoint that guards
// spec.kubeConfig.secretRef against the confused-deputy escalation the config-plane split opens:
// a tenant granted create on GitTarget but NOT Secret-read could otherwise name a privileged
// kubeconfig Secret they cannot themselves read, and the operator — which can — would mirror
// that remote cluster's state into a Git destination the tenant controls. See
// docs/design/config-plane-split.md "Credential-reference authorization".
const ValidateGitTargetKubeConfigPath = "/validate-gittarget-kubeconfig"

// secretsResource is the resource the SubjectAccessReview is checked against.
const secretsResource = "secrets"

// SecretAccessAuthorizer answers "may this requester `get` the named Secret?". The real
// implementation issues a SubjectAccessReview; the interface keeps the admission handler
// unit-testable without an API server.
type SecretAccessAuthorizer interface {
	// CanGetSecret reports whether user holds `get` on the named Secret in namespace. The
	// reason is the authorizer's own explanation, surfaced in the denial message.
	CanGetSecret(
		ctx context.Context,
		user authnv1.UserInfo,
		namespace, name string,
	) (allowed bool, reason string, err error)
}

// subjectAccessReviewSecretAuthorizer implements SecretAccessAuthorizer against the apiserver's
// authorization API, delegating the decision to whatever authorizers the cluster has configured
// (RBAC, webhook, node). It creates SubjectAccessReviews, so the operator's ServiceAccount needs
// `create` on `subjectaccessreviews.authorization.k8s.io`.
type subjectAccessReviewSecretAuthorizer struct {
	client authzv1client.SubjectAccessReviewInterface
}

// NewSubjectAccessReviewSecretAuthorizer builds the production authorizer.
func NewSubjectAccessReviewSecretAuthorizer(
	client authzv1client.SubjectAccessReviewInterface,
) SecretAccessAuthorizer {
	return &subjectAccessReviewSecretAuthorizer{client: client}
}

func (a *subjectAccessReviewSecretAuthorizer) CanGetSecret(
	ctx context.Context,
	user authnv1.UserInfo,
	namespace, name string,
) (bool, string, error) {
	review := &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User:   user.Username,
			UID:    user.UID,
			Groups: user.Groups,
			Extra:  extraToSubjectAccessReviewExtra(user.Extra),
			ResourceAttributes: &authzv1.ResourceAttributes{
				Namespace: namespace,
				Verb:      "get",
				Group:     "", // core group
				Resource:  secretsResource,
				Name:      name,
			},
		},
	}
	result, err := a.client.Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, "", fmt.Errorf("create SubjectAccessReview: %w", err)
	}
	if result.Status.EvaluationError != "" && !result.Status.Allowed {
		return false, result.Status.EvaluationError, nil
	}
	return result.Status.Allowed, result.Status.Reason, nil
}

// extraToSubjectAccessReviewExtra converts admission's user.extra to the authorization API's
// near-identical type, so a webhook authorizer keyed on an OIDC claim sees it.
func extraToSubjectAccessReviewExtra(extra map[string]authnv1.ExtraValue) map[string]authzv1.ExtraValue {
	if len(extra) == 0 {
		return nil
	}
	out := make(map[string]authzv1.ExtraValue, len(extra))
	for key, values := range extra {
		out[key] = authzv1.ExtraValue(values)
	}
	return out
}

// ValidateGitTargetKubeConfigHandler denies a GitTarget whose spec.kubeConfig.secretRef the
// requester cannot themselves `get`. It is FAIL-CLOSED: any authorizer error denies, so a
// missing verdict never silently admits an escalation. A GitTarget without kubeConfig.secretRef
// (the single-cluster default) is admitted with no check.
type ValidateGitTargetKubeConfigHandler struct {
	// Authorizer is nil when the operator has no SubjectAccessReview client (e.g. a build
	// without RBAC to create SARs); the handler then fails closed on any GitTarget that names a
	// kubeConfig Secret, because it cannot prove the requester may read it.
	Authorizer SecretAccessAuthorizer
}

// Handle implements admission.Handler.
func (h *ValidateGitTargetKubeConfigHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	secretName, ok := parseKubeConfigSecretRef(req.Object.Raw)
	if !ok {
		return admission.Allowed("no spec.kubeConfig.secretRef; nothing to authorize")
	}
	if h.Authorizer == nil {
		return admission.Denied(fmt.Sprintf(
			"cannot verify access to spec.kubeConfig.secretRef Secret %q: no SubjectAccessReview "+
				"authorizer configured (fail-closed)", secretName))
	}
	allowed, reason, err := h.Authorizer.CanGetSecret(ctx, req.UserInfo, req.Namespace, secretName)
	if err != nil {
		// Fail closed: an authorization backend error must not admit an unverified reference.
		return admission.Denied(fmt.Sprintf(
			"could not verify access to spec.kubeConfig.secretRef Secret %s/%s (fail-closed): %v",
			req.Namespace, secretName, err))
	}
	if !allowed {
		return denyKubeConfigSecretAccess(req.UserInfo.Username, req.Namespace, secretName, reason)
	}
	return admission.Allowed("requester may read the referenced kubeconfig Secret")
}

// kubeConfigSecretRef is the subset of a GitTarget an admission request needs: the Secret its
// spec.kubeConfig.secretRef names. Reading only these fields means the handler never depends on
// the GitTarget kind being registered in a decoder scheme.
func parseKubeConfigSecretRef(raw []byte) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var probe struct {
		Spec struct {
			KubeConfig *struct {
				SecretRef *struct {
					Name string `json:"name"`
				} `json:"secretRef"`
			} `json:"kubeConfig"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", false
	}
	if probe.Spec.KubeConfig == nil || probe.Spec.KubeConfig.SecretRef == nil {
		return "", false
	}
	name := probe.Spec.KubeConfig.SecretRef.Name
	if name == "" {
		return "", false
	}
	return name, true
}

// denyKubeConfigSecretAccess builds the admission denial for an unauthorized secretRef. The
// message names the exact grant that would allow it, because "forbidden" without the remedy is
// the least useful thing an admission webhook can say.
func denyKubeConfigSecretAccess(user, namespace, secretName, reason string) admission.Response {
	msg := fmt.Sprintf(
		"user %q may not reference kubeconfig Secret %s/%s in spec.kubeConfig.secretRef: it names a "+
			"Secret they cannot `get`, which would let the operator read a remote cluster on their behalf "+
			"(confused-deputy). Grant get on that Secret with a Role in namespace %q: "+
			"{apiGroups: [\"\"], resources: [\"secrets\"], resourceNames: [%q], verbs: [\"get\"]}",
		user, namespace, secretName, namespace, secretName)
	if reason != "" {
		msg += " (authorizer: " + reason + ")"
	}
	return admission.Denied(msg)
}
