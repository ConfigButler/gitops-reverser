// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	k8stypes "k8s.io/apimachinery/pkg/types"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
)

// supersededFieldRefusal reports why this GitTarget may not run, considering both its OWN stored
// superseded fields and those of the GitProvider it writes through, or "" when neither carries any.
//
// The provider is checked HERE, on the target, and not only on the provider's own conditions. A
// GitProvider reporting Stalled is a remark about the provider; it does not stop a GitTarget wiring
// a worker and declaring, so a provider still carrying spec.push or spec.commit.message would keep
// writing — at the default cadence and under the default wording, since neither value is read any
// more. That is exactly the silent reinterpretation the retained-and-refused pattern exists to
// prevent, and the only place it can be prevented is the gate that runs before the data plane.
//
// A provider that cannot be read is NOT a refusal: validateProviderAndBranch owns the missing and
// unreadable cases and reports them with their own reasons, and duplicating that here would give
// one fault two vocabularies.
func (r *GitTargetReconciler) supersededFieldRefusal(
	ctx context.Context,
	target *configbutleraiv1alpha3.GitTarget,
	providerNS string,
) string {
	if refusal := authz.SupersededFieldRefusal(target); refusal != "" {
		return refusal
	}

	var provider configbutleraiv1alpha3.GitProvider
	key := k8stypes.NamespacedName{Name: target.Spec.ProviderRef.Name, Namespace: providerNS}
	if err := r.Get(ctx, key, &provider); err != nil {
		return ""
	}
	return authz.SupersededFieldRefusal(&provider)
}
