// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"encoding/json"
	"testing"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

func clusterWatchRuleReview(t *testing.T, rule *v1alpha3.ClusterWatchRule) ctrladmission.Request {
	t.Helper()
	raw, err := json.Marshal(rule)
	require.NoError(t, err)
	return ctrladmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Resource: metav1.GroupVersionResource{
			Group: "configbutler.ai", Version: "v1alpha3", Resource: "clusterwatchrules"},
		Operation: admissionv1.Create,
		Name:      rule.Name,
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

// The schema cannot afford to check matchLabels syntax, so the webhook applies the compiler's own
// check to both rule kinds.
func TestObjectSelectorAdmission_RejectsWhatTheCompilerRefuses(t *testing.T) {
	invalid := &v1alpha3.ObjectSelector{MatchLabels: map[string]string{"bad key": "x"}}
	valid := &v1alpha3.ObjectSelector{MatchLabels: map[string]string{"team": "a"}}
	handler := watchRuleHandler(t)

	for name, selector := range map[string]*v1alpha3.ObjectSelector{"invalid": invalid, "valid": valid} {
		t.Run("WatchRule/"+name, func(t *testing.T) {
			rule := watchRuleFor("selected", "")
			rule.Spec.Rules[0].ObjectSelector = selector
			response := handler.Handle(t.Context(), watchRuleReview(t, rule, admissionv1.Create))
			assert.Equal(t, name == "valid", response.Allowed, response.Result.Message)
			if name == "invalid" {
				assert.Contains(t, response.Result.Message, "spec.rules[0].objectSelector")
			}
		})
		t.Run("ClusterWatchRule/"+name, func(t *testing.T) {
			rule := &v1alpha3.ClusterWatchRule{
				ObjectMeta: metav1.ObjectMeta{Name: "selected"},
				Spec: v1alpha3.ClusterWatchRuleSpec{
					GitTargetRef: meta.NamespacedObjectReference{Name: "t", Namespace: "shop"},
					Rules: []v1alpha3.ClusterResourceRule{{
						Resources: []string{"namespaces"}, ObjectSelector: selector,
					}},
				},
			}
			response := handler.Handle(t.Context(), clusterWatchRuleReview(t, rule))
			assert.Equal(t, name == "valid", response.Allowed, response.Result.Message)
		})
	}
}
