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

const (
	// AssertAuthorVerb is the RBAC verb a requester must hold on a GitTarget to set
	// CommitRequest.spec.author. It follows the precedent of `bind`, `escalate` and
	// `impersonate`: a verb on a resource, granted deliberately, that authorizes saying
	// something the API cannot otherwise verify.
	AssertAuthorVerb = "assert-author"
	// gitTargetsResource is the resource the verb is checked against.
	gitTargetsResource = "gittargets"
	// configbutlerGroup is the API group of that resource.
	configbutlerGroup = "configbutler.ai"
)

// AuthorAssertionAuthorizer answers "may this requester assert a commit author on this
// GitTarget?". The real implementation issues a SubjectAccessReview; the interface keeps
// the admission handler unit-testable without an API server.
type AuthorAssertionAuthorizer interface {
	// CanAssertAuthor reports whether user holds AssertAuthorVerb on the named GitTarget.
	// The reason is the authorizer's own explanation, surfaced in the denial message.
	CanAssertAuthor(
		ctx context.Context,
		user authnv1.UserInfo,
		namespace, gitTargetName string,
	) (allowed bool, reason string, err error)
}

// subjectAccessReviewAuthorizer implements AuthorAssertionAuthorizer against the
// apiserver's authorization API, delegating the decision to whatever authorizers the
// cluster has configured (RBAC, webhook, node). It creates SubjectAccessReviews, so the
// operator's ServiceAccount needs `create` on `subjectaccessreviews.authorization.k8s.io`.
type subjectAccessReviewAuthorizer struct {
	client authzv1client.SubjectAccessReviewInterface
}

// NewSubjectAccessReviewAuthorizer builds the production authorizer.
func NewSubjectAccessReviewAuthorizer(client authzv1client.SubjectAccessReviewInterface) AuthorAssertionAuthorizer {
	return &subjectAccessReviewAuthorizer{client: client}
}

func (a *subjectAccessReviewAuthorizer) CanAssertAuthor(
	ctx context.Context,
	user authnv1.UserInfo,
	namespace, gitTargetName string,
) (bool, string, error) {
	// resourceNames scopes the grant to one GitTarget, so a tenant granted assert-author
	// on their own target cannot author commits into someone else's repository.
	review := &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User:   user.Username,
			UID:    user.UID,
			Groups: user.Groups,
			Extra:  extraToSubjectAccessReviewExtra(user.Extra),
			ResourceAttributes: &authzv1.ResourceAttributes{
				Namespace: namespace,
				Verb:      AssertAuthorVerb,
				Group:     configbutlerGroup,
				Resource:  gitTargetsResource,
				Name:      gitTargetName,
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

// extraToSubjectAccessReviewExtra converts admission's user.extra to the authorization
// API's near-identical type, so a webhook authorizer keyed on an OIDC claim sees it.
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

// assertedAuthor is the subset of a CommitRequest an admission request needs to decide
// authorization: the asserted identity and the GitTarget it is asserted against. Like
// commandObjectUID it reads only the fields it needs, so it never depends on the command
// kind being registered in a decoder scheme.
type assertedAuthor struct {
	Name          string
	Email         string
	GitTargetName string
}

// parseAssertedAuthor extracts spec.author and spec.targetRef.name from a raw
// CommitRequest. ok=false means the object asserts no author, which is the common case
// and requires no authorization at all.
func parseAssertedAuthor(raw []byte) (assertedAuthor, bool) {
	if len(raw) == 0 {
		return assertedAuthor{}, false
	}
	var probe struct {
		Spec struct {
			Author *struct {
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"author"`
			TargetRef struct {
				Name string `json:"name"`
			} `json:"targetRef"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return assertedAuthor{}, false
	}
	if probe.Spec.Author == nil || probe.Spec.Author.Name == "" {
		return assertedAuthor{}, false
	}
	return assertedAuthor{
		Name:          probe.Spec.Author.Name,
		Email:         probe.Spec.Author.Email,
		GitTargetName: probe.Spec.TargetRef.Name,
	}, true
}

// denyAuthorAssertion builds the admission denial for an unauthorized spec.author. The
// message names the exact RBAC rule that would grant it, because "forbidden" without the
// remedy is the least useful thing an admission webhook can say.
func denyAuthorAssertion(user, namespace, gitTargetName, reason string) admission.Response {
	msg := fmt.Sprintf(
		"user %q may not set spec.author on a CommitRequest for GitTarget %q: "+
			"asserting a commit author requires the %q verb on gittargets.%s. Grant it with a Role in "+
			"namespace %q: {apiGroups: [%q], resources: [%q], resourceNames: [%q], verbs: [%q]}",
		user, gitTargetName, AssertAuthorVerb, configbutlerGroup, namespace,
		configbutlerGroup, gitTargetsResource, gitTargetName, AssertAuthorVerb)
	if reason != "" {
		msg += " (authorizer: " + reason + ")"
	}
	return admission.Denied(msg)
}
