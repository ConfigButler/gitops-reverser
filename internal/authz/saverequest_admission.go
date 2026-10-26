// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// SaveRequestAdmitted reports whether request may save the commit windows of target, the GitTarget
// its spec.gitTargetRef names: a grant in target's spec.allowedSaveRequests admits the request's
// namespace, by the labels that namespace carries. The admission also says how much the request may
// claim of a window whose writes name no author. A non-NotFound read of the Namespace is returned as
// err, so the caller retries instead of refusing a request on a transient failure.
func SaveRequestAdmitted(
	ctx context.Context,
	reader client.Reader,
	request *configv1alpha3.SaveRequest,
	target *configv1alpha3.GitTarget,
) (Decision, configv1alpha3.SaveRequestAdmission, error) {
	nsLabels, err := namespaceLabels(ctx, reader, request.Namespace)
	if err != nil {
		return Decision{}, configv1alpha3.SaveRequestAdmission{}, err
	}
	admission, selErr := target.AdmitsSaveRequest(request.Namespace, nsLabels)
	if !admission.Admitted {
		return notAdmitted("GitTarget", client.ObjectKeyFromObject(target), target.DescribeAllowedSaveRequests(),
			"spec.allowedSaveRequests", request.Namespace, selErr), admission, nil
	}
	return Decision{Allowed: true}, admission, nil
}
