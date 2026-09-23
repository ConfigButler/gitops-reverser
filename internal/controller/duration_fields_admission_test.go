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
		Entry("a leading dot", "window-leading-dot", ".5s"),
		Entry("a negative duration", "window-negative", "-1s"),
		Entry("nonsense", "window-nonsense", "banana"),
		// The pattern cannot express MAGNITUDE, so this one matches it and then overflows
		// time.ParseDuration. Stored, it would be undecodable by every typed client — one object
		// breaking the GET and LIST the GitTarget informer runs on, for the whole kind. The CEL
		// bound is what refuses it, and that is the bound's real job.
		Entry("a duration too large for int64 nanoseconds", "window-overflow", "999999999h"),
		Entry("past the bound, but parseable", "window-too-long", "25h"),
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
		// Go's own units, admitted because the accepted set has to be closed under
		// serialization: see the round-trip spec below.
		Entry("microseconds, as Go spells them", "window-micros", "500µs", 500*time.Microsecond),
		Entry("microseconds, as Go parses them", "window-us", "500us", 500*time.Microsecond),
		Entry("nanoseconds", "window-ns", "10ns", 10*time.Nanosecond),
		Entry("at the bound", "window-at-bound", "24h", 24*time.Hour),
	)

	// The property that makes this API usable from a typed client at all: what the pattern
	// accepts, Go must be able to write back. A client reads the field into a time.Duration and
	// serializes it as Duration.String(), so a value the API server accepted but Go re-spells
	// outside the pattern can never be updated again — the object is stuck, by one field.
	//
	// "0.5ms" is the case that found this: Go writes it as "500µs", which Flux's own pattern
	// rejects.
	DescribeTable("survive a typed read-modify-write",
		func(name string, written string) {
			ctx := context.Background()
			target := newTarget(name, written)
			Expect(k8sClient.Create(ctx, target)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, target) })

			// A metadata-only update through the TYPED client, which re-serializes the whole
			// object — the duration included, in Go's spelling rather than the one written above.
			//
			// Retried because the GitTarget controller is live in this suite and writes status
			// between the read and the write; a resourceVersion conflict says nothing about the
			// property under test, while a rejected duration keeps failing until the timeout.
			key := types.NamespacedName{Name: name, Namespace: "default"}
			Eventually(func() error {
				var stored configbutleraiv1alpha3.GitTarget
				if err := k8sClient.Get(ctx, key, &stored); err != nil {
					return err
				}
				if stored.Labels == nil {
					stored.Labels = map[string]string{}
				}
				stored.Labels["touched"] = "yes"
				return k8sClient.Update(ctx, &stored)
			}, "10s", "200ms").Should(Succeed(),
				"a value this API accepted must survive being written back by a typed client")
		},
		Entry("a spelling Go re-spells", "rt-half-ms", "0.5ms"),
		Entry("a unit Go renames", "rt-us", "500us"),
		Entry("a value Go expands", "rt-minute", "1m"),
		Entry("a fraction Go redistributes", "rt-fraction", "1.5m"),
		Entry("a value Go leaves alone", "rt-stable", "750ms"),
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

	// CommitRequest carries the same hazard with immutability on top: its spec may not change
	// after creation, and a whole-object comparison compares the delay as a STRING. "1m" reads
	// back into Go and serializes as "1m0s", so before the rule compared durations instead of
	// spellings, every typed update — including one that only touches a label — was rejected as
	// an attempt to change an immutable spec.
	It("lets a typed client update a CommitRequest created with a re-spelled duration", func() {
		ctx := context.Background()

		request := &configbutleraiv1alpha3.CommitRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "delay-respelled", Namespace: "default"},
			Spec: configbutleraiv1alpha3.CommitRequestSpec{
				GitTargetRef: meta.LocalObjectReference{Name: "any-target"},
				CloseDelay:   &metav1.Duration{Duration: time.Minute},
			},
		}
		Expect(k8sClient.Create(ctx, request)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, request) })

		key := types.NamespacedName{Name: request.Name, Namespace: request.Namespace}
		Eventually(func() error {
			var stored configbutleraiv1alpha3.CommitRequest
			if err := k8sClient.Get(ctx, key, &stored); err != nil {
				return err
			}
			if stored.Labels == nil {
				stored.Labels = map[string]string{}
			}
			stored.Labels["touched"] = "yes"
			return k8sClient.Update(ctx, &stored)
		}, "10s", "200ms").Should(Succeed(),
			"the same duration spelled differently is not a spec change")
	})

	It("still refuses a real change to an immutable CommitRequest spec", func() {
		ctx := context.Background()

		request := &configbutleraiv1alpha3.CommitRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "delay-immutable", Namespace: "default"},
			Spec: configbutleraiv1alpha3.CommitRequestSpec{
				GitTargetRef: meta.LocalObjectReference{Name: "any-target"},
				CloseDelay:   &metav1.Duration{Duration: time.Minute},
			},
		}
		Expect(k8sClient.Create(ctx, request)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, request) })

		var stored configbutleraiv1alpha3.CommitRequest
		key := types.NamespacedName{Name: request.Name, Namespace: request.Namespace}
		Expect(k8sClient.Get(ctx, key, &stored)).To(Succeed())
		stored.Spec.CloseDelay = &metav1.Duration{Duration: 30 * time.Second}
		Eventually(func() string {
			var current configbutleraiv1alpha3.CommitRequest
			if err := k8sClient.Get(ctx, key, &current); err != nil {
				return err.Error()
			}
			current.Spec.CloseDelay = &metav1.Duration{Duration: 30 * time.Second}
			if err := k8sClient.Update(ctx, &current); err != nil {
				return err.Error()
			}
			return ""
		}, "10s", "200ms").Should(ContainSubstring("immutable"),
			"comparing by value must not become comparing by nothing")
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
