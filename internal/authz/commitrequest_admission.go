// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// CommitRequestAdmitted reports whether request may save the commit windows of target, the GitTarget
// its spec.gitTargetRef names: a grant in target's spec.allowedCommitRequests admits the request's
// namespace, by the labels that namespace carries. A non-NotFound read of the Namespace is returned as
// err, so the caller retries instead of refusing a request on a transient failure.
func CommitRequestAdmitted(
	ctx context.Context,
	reader client.Reader,
	request *configv1alpha3.CommitRequest,
	target *configv1alpha3.GitTarget,
) (Decision, error) {
	nsLabels, err := namespaceLabels(ctx, reader, request.Namespace)
	if err != nil {
		return Decision{}, err
	}
	admitted, selErr := target.AdmitsCommitRequest(request.Namespace, nsLabels)
	if !admitted {
		return notAdmitted("GitTarget", client.ObjectKeyFromObject(target), target.DescribeAllowedCommitRequests(),
			"spec.allowedCommitRequests", request.Namespace, selErr), nil
	}
	return Decision{Allowed: true}, nil
}
