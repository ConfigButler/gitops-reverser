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
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// The feedback half of the one-source-namespace rule. Everything asserted here is a rejection or a
// deliberate refusal to judge; the rule itself is enforced at the write, and
// internal/git asserts that half.

func namespaceFreeTarget(serialize *bool) *v1alpha3.GitTarget {
	return &v1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout-artifact", Namespace: "shop"},
		Spec: v1alpha3.GitTargetSpec{
			Path:               "apps/checkout",
			SerializeNamespace: serialize,
		},
	}
}

func watchRuleFor(name, sourceNamespace string) *v1alpha3.WatchRule {
	return &v1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop"},
		Spec: v1alpha3.WatchRuleSpec{
			GitTargetRef: meta.LocalObjectReference{Name: "checkout-artifact"},
			Rules: []v1alpha3.ResourceRule{{
				Resources:       []string{"configmaps"},
				SourceNamespace: sourceNamespace,
			}},
		},
	}
}

func watchRuleReview(t *testing.T, rule *v1alpha3.WatchRule, operation admissionv1.Operation) ctrladmission.Request {
	t.Helper()
	raw, err := json.Marshal(rule)
	require.NoError(t, err)
	return ctrladmission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Resource: metav1.GroupVersionResource{
				Group: "configbutler.ai", Version: "v1alpha3", Resource: "watchrules"},
			Operation: operation,
			Namespace: rule.Namespace,
			Name:      rule.Name,
			Object:    runtime.RawExtension{Raw: raw},
		},
	}
}

func watchRuleHandler(t *testing.T, objects ...client.Object) *ValidateOperatorTypesHandler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha3.AddToScheme(scheme))
	return &ValidateOperatorTypesHandler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
	}
}

func TestWatchRuleAdmission_SecondSourceNamespaceIsRejected(t *testing.T) {
	no := false
	handler := watchRuleHandler(t, namespaceFreeTarget(&no), watchRuleFor("content", ""))

	response := handler.Handle(t.Context(),
		watchRuleReview(t, watchRuleFor("content-billing", "billing"), admissionv1.Create))

	require.False(t, response.Allowed)
	assert.Contains(t, response.Result.Message, "admits exactly one source namespace")
	assert.Contains(t, response.Result.Message, "[billing shop]")
}

func TestWatchRuleAdmission_FirstSourceNamespaceIsAdmitted(t *testing.T) {
	no := false
	handler := watchRuleHandler(t, namespaceFreeTarget(&no))

	response := handler.Handle(t.Context(), watchRuleReview(t, watchRuleFor("content", ""), admissionv1.Create))

	assert.True(t, response.Allowed, "the target's own namespace is the one namespace it admits")
}

// A wildcard is rejected statically: neither reading of "*" can be shown to be one namespace, so
// nothing is enumerated to find out.
func TestWatchRuleAdmission_WildcardIsRejectedWithoutEnumerating(t *testing.T) {
	no := false
	handler := watchRuleHandler(t, namespaceFreeTarget(&no))

	response := handler.Handle(t.Context(),
		watchRuleReview(t, watchRuleFor("everything", v1alpha3.SourceNamespaceWildcard), admissionv1.Create))

	require.False(t, response.Allowed)
	assert.Contains(t, response.Result.Message, "cannot be shown to be one namespace")
}

// An UPDATE is judged on what the rule would BECOME. Re-submitting the rule that already holds the
// second namespace must not be rejected for its own existing value, or the only edit that could fix
// it would be blocked by it.
func TestWatchRuleAdmission_UpdateExcludesTheRuleUnderReview(t *testing.T) {
	no := false
	existing := watchRuleFor("content", "billing")
	handler := watchRuleHandler(t, namespaceFreeTarget(&no), existing)

	narrowed := watchRuleFor("content", "shop")
	response := handler.Handle(t.Context(), watchRuleReview(t, narrowed, admissionv1.Update))

	assert.True(t, response.Allowed, "an edit that narrows the rule back to one namespace must land")
}

func TestWatchRuleAdmission_UnfencedTargetsAreNotJudged(t *testing.T) {
	yes := true
	for _, tc := range []struct {
		name      string
		serialize *bool
	}{
		{"unset infers per document and is never fenced", nil},
		{"true means every document carries its own namespace", &yes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := watchRuleHandler(t, namespaceFreeTarget(tc.serialize), watchRuleFor("content", ""))

			response := handler.Handle(t.Context(),
				watchRuleReview(t, watchRuleFor("content-billing", "billing"), admissionv1.Create))

			assert.True(t, response.Allowed)
		})
	}
}

// Every way of failing to evaluate ALLOWS. A rejection this handler cannot justify is a rejection
// of the user's object on the strength of a GitTarget nobody read.
func TestWatchRuleAdmission_UnevaluatableRequestsAreAllowed(t *testing.T) {
	no := false

	t.Run("no client", func(t *testing.T) {
		handler := &ValidateOperatorTypesHandler{}
		response := handler.Handle(t.Context(),
			watchRuleReview(t, watchRuleFor("content-billing", "billing"), admissionv1.Create))
		assert.True(t, response.Allowed)
	})

	t.Run("the GitTarget does not exist yet", func(t *testing.T) {
		handler := watchRuleHandler(t)
		response := handler.Handle(t.Context(),
			watchRuleReview(t, watchRuleFor("content-billing", "billing"), admissionv1.Create))
		assert.True(t, response.Allowed, "rules and targets are applied in whatever order the manifest lists them")
	})

	t.Run("an undecodable object", func(t *testing.T) {
		handler := watchRuleHandler(t, namespaceFreeTarget(&no))
		review := watchRuleReview(t, watchRuleFor("content", ""), admissionv1.Create)
		review.Object.Raw = []byte("{not json")
		assert.True(t, handler.Handle(t.Context(), review).Allowed)
	})
}

// A rule pointing at a different GitTarget contributes nothing, so a target fenced to one namespace
// is not rejected because of somebody else's rule.
func TestWatchRuleAdmission_OtherTargetsRulesAreNotCounted(t *testing.T) {
	no := false
	elsewhere := watchRuleFor("elsewhere", "billing")
	elsewhere.Spec.GitTargetRef.Name = "other-artifact"
	handler := watchRuleHandler(t, namespaceFreeTarget(&no), elsewhere)

	response := handler.Handle(t.Context(), watchRuleReview(t, watchRuleFor("content", ""), admissionv1.Create))

	assert.True(t, response.Allowed)
}
