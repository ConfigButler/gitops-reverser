// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// isClusterWatchRuleKind reports whether gr is the cluster-scoped ClusterWatchRule. It reaches this
// endpoint for its objectSelector check only.
func isClusterWatchRuleKind(gr metav1.GroupResource) bool {
	return gr == metav1.GroupResource{Group: "configbutler.ai", Resource: "clusterwatchrules"}
}

// objectSelectorDenial rejects a WatchRule or ClusterWatchRule whose spec.rules[].objectSelector
// does not parse, with the same check and message the rule compiler refuses it with
// (types.ValidateObjectSelectors). The CRD schema already bounds the selector and checks each
// requirement; this covers what the schema cannot afford to, such as matchLabels syntax.
//
// It is feedback, like the rest of this endpoint: the compiler refuses the same rule whether or
// not this ran. An object this cannot decode is allowed and left to the compiler.
func objectSelectorDenial(gr metav1.GroupResource, raw []byte) (string, bool) {
	var selectors []*metav1.LabelSelector
	if isClusterWatchRuleKind(gr) {
		var rule v1alpha3.ClusterWatchRule
		if json.Unmarshal(raw, &rule) != nil {
			return "", false
		}
		selectors = rule.Spec.ObjectSelectors()
	} else {
		var rule v1alpha3.WatchRule
		if json.Unmarshal(raw, &rule) != nil {
			return "", false
		}
		selectors = rule.Spec.ObjectSelectors()
	}
	if err := types.ValidateObjectSelectors(selectors); err != nil {
		return err.Error(), true
	}
	return "", false
}

// validateRule is the rule-kind branch of Handle: the objectSelector check for both kinds, then the
// WatchRule-only source-namespace check.
func (h *ValidateOperatorTypesHandler) validateRule(
	ctx context.Context,
	gr metav1.GroupResource,
	req admission.Request,
) admission.Response {
	if message, denied := objectSelectorDenial(gr, req.Object.Raw); denied {
		return admission.Denied(message)
	}
	if isClusterWatchRuleKind(gr) {
		return admission.Allowed("objectSelector valid")
	}
	return h.validateWatchRuleSourceNamespaces(ctx, req)
}
