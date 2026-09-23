// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// Every duration this API takes is a Go duration string, validated by the API SERVER rather than
// by a controller reading the object afterwards. These specs run against the generated CRDs, so
// they say what an operator's `kubectl apply` actually does.
//
// The shape is Flux's, verbatim: metav1.Duration typed as a string with
// `^([0-9]+(\.[0-9]+)?(ms|s|m|h))+$`. What that pattern buys is a rejection with the field's name
// in it, at write time, instead of a value nobody can parse sitting in storage and being
// discovered later by whichever component reads it first. What it costs is that `30` is not a
// duration: the unit is mandatory, as it is everywhere else in this project.
var _ = Describe("Duration fields", func() {
	newTarget := func(name string, window any) *unstructured.Unstructured {
		spec := map[string]any{
			"gitProviderRef": map[string]any{"name": "any-provider"},
			"branch":         "main",
			"path":           "clusters/prod",
			"commit":         map[string]any{"window": window},
		}
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "configbutler.ai/v1alpha3",
			"kind":       "GitTarget",
			"metadata":   map[string]any{"name": name, "namespace": "default"},
			"spec":       spec,
		}}
	}

	DescribeTable("reject anything that is not a Go duration",
		func(name string, window string) {
			ctx := context.Background()
			err := k8sClient.Create(ctx, newTarget(name, window))
			Expect(err).To(HaveOccurred(), "the API server must refuse %q, not store it", window)
			Expect(err.Error()).To(ContainSubstring("spec.commit.window"),
				"the rejection must name the field, or an operator has to guess which value it meant")
		},
		Entry("prose", "window-prose", "5 seconds"),
		Entry("a bare number", "window-bare", "30"),
		Entry("a negative duration", "window-negative", "-1s"),
		Entry("a unit Go accepts but this pattern does not", "window-micros", "500us"),
		Entry("nonsense", "window-nonsense", "banana"),
	)

	DescribeTable("accept every Go duration spelling the pattern admits",
		func(name string, window string, want time.Duration) {
			ctx := context.Background()
			target := newTarget(name, window)
			Expect(k8sClient.Create(ctx, target)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, target) })

			var stored configbutleraiv1alpha3.GitTarget
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: name, Namespace: "default"}, &stored)).To(Succeed())
			Expect(stored.Spec.Commit.Window.Duration).To(Equal(want),
				"what round-trips must be the duration that was written")
		},
		Entry("sub-second", "window-ms", "750ms", 750*time.Millisecond),
		Entry("seconds", "window-s", "5s", 5*time.Second),
		Entry("fractional", "window-fraction", "1.5m", 90*time.Second),
		Entry("compound", "window-compound", "1m30s", 90*time.Second),
		Entry("zero, which opts into per-event commits", "window-zero", "0s", time.Duration(0)),
	)

	It("defaults CommitRequest.spec.closeDelay to 2s and preserves an explicit 0s", func() {
		ctx := context.Background()

		omitted := &configbutleraiv1alpha3.CommitRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "delay-omitted", Namespace: "default"},
			Spec: configbutleraiv1alpha3.CommitRequestSpec{
				GitTargetRef: meta.LocalObjectReference{Name: "any-target"},
			},
		}
		Expect(k8sClient.Create(ctx, omitted)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, omitted) })
		Expect(omitted.Spec.CloseDelay).NotTo(BeNil(), "the schema default must be filled in")
		Expect(omitted.Spec.CloseDelay.Duration).To(Equal(2 * time.Second))

		// The pointer exists for exactly this: an explicit "0s" is a request to finalize
		// immediately, and defaulting must not read it as an omission.
		immediate := &configbutleraiv1alpha3.CommitRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "delay-immediate", Namespace: "default"},
			Spec: configbutleraiv1alpha3.CommitRequestSpec{
				GitTargetRef: meta.LocalObjectReference{Name: "any-target"},
				CloseDelay:   &metav1.Duration{},
			},
		}
		Expect(k8sClient.Create(ctx, immediate)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, immediate) })
		Expect(immediate.Spec.CloseDelay).NotTo(BeNil())
		Expect(immediate.Spec.CloseDelay.Duration).To(BeZero())
	})

	// The bound the int field carried as Maximum=300 survives the retype, as CEL: the field is a
	// string to the API server, so the pattern decides whether a value is a duration at all and
	// only CEL can compare two that are.
	It("keeps the upper bound on closeDelay, now as a CEL rule", func() {
		ctx := context.Background()

		tooLong := &configbutleraiv1alpha3.CommitRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "delay-too-long", Namespace: "default"},
			Spec: configbutleraiv1alpha3.CommitRequestSpec{
				GitTargetRef: meta.LocalObjectReference{Name: "any-target"},
				CloseDelay:   &metav1.Duration{Duration: 10 * time.Minute},
			},
		}
		err := k8sClient.Create(ctx, tooLong)
		Expect(err).To(HaveOccurred(), "a request may not hold a window open for ten minutes")
		Expect(err.Error()).To(ContainSubstring("closeDelay must not exceed 5m"))

		atTheBound := &configbutleraiv1alpha3.CommitRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "delay-at-bound", Namespace: "default"},
			Spec: configbutleraiv1alpha3.CommitRequestSpec{
				GitTargetRef: meta.LocalObjectReference{Name: "any-target"},
				CloseDelay:   &metav1.Duration{Duration: 5 * time.Minute},
			},
		}
		Expect(k8sClient.Create(ctx, atTheBound)).To(Succeed(), "the bound itself is allowed")
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, atTheBound) })
	})

	// The int spelling is GONE, and a removed field is pruned on write rather than refused — the
	// same quiet behaviour the superseded-field specs pin. It is quiet in a way that matters here:
	// a legacy `closeDelaySeconds: 30` applies cleanly and finalizes after 2 seconds instead of
	// 30. That is what the UPGRADING.md entry and its inventory command exist for.
	It("prunes a legacy closeDelaySeconds instead of honouring it", func() {
		ctx := context.Background()

		legacy := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "configbutler.ai/v1alpha3",
			"kind":       "CommitRequest",
			"metadata":   map[string]any{"name": "legacy-close-delay", "namespace": "default"},
			"spec": map[string]any{
				"gitTargetRef":      map[string]any{"name": "any-target"},
				"closeDelaySeconds": int64(30),
			},
		}}
		Expect(k8sClient.Create(ctx, legacy)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, legacy) })

		spec, _, err := unstructured.NestedMap(legacy.Object, "spec")
		Expect(err).NotTo(HaveOccurred())
		Expect(spec).NotTo(HaveKey("closeDelaySeconds"), "the int spelling is dropped silently")
		Expect(spec).To(HaveKeyWithValue("closeDelay", "2s"),
			"and the default lands, so the request finalizes in 2s where it asked for 30")
	})
})
