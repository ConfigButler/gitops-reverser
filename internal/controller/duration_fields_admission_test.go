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
	// newTargetWindow builds a GitTarget with the given commit.window block.
	newTargetWindow := func(name string, window map[string]any) *unstructured.Unstructured {
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
	// newTarget sets idleTimeout under a maxDuration raised to its own bound, so the shape tables
	// below test the duration alone and not the idleTimeout <= maxDuration rule.
	newTarget := func(name string, idle any) *unstructured.Unstructured {
		return newTargetWindow(name, map[string]any{"idleTimeout": idle, "maxDuration": "24h"})
	}

	DescribeTable("reject anything that is not a Go duration",
		func(name string, window string) {
			ctx := context.Background()
			err := k8sClient.Create(ctx, newTarget(name, window))
			Expect(err).To(HaveOccurred(), "the API server must refuse %q, not store it", window)
			Expect(err.Error()).To(ContainSubstring("spec.commit.window.idleTimeout"),
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
			Expect(stored.Spec.Commit.Window.IdleTimeout.Duration).To(Equal(want),
				"what round-trips must be the duration that was written")
		},
		Entry("sub-second", "window-ms", "750ms", 750*time.Millisecond),
		Entry("seconds", "window-s", "5s", 5*time.Second),
		Entry("fractional", "window-fraction", "1.5m", 90*time.Second),
		Entry("compound", "window-compound", "1m30s", 90*time.Second),
		Entry("zero, which commits every write on its own", "window-zero", "0s", time.Duration(0)),
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

	It("fills a GitTarget's window block field by field, and refuses an idleTimeout past maxDuration", func() {
		ctx := context.Background()

		empty := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "configbutler.ai/v1alpha3",
			"kind":       "GitTarget",
			"metadata":   map[string]any{"name": "window-empty-block", "namespace": "default"},
			"spec": map[string]any{
				"gitProviderRef": map[string]any{"name": "any-provider"},
				"branch":         "main",
				"path":           "clusters/prod",
				"commit":         map[string]any{"window": map[string]any{}},
			},
		}}
		Expect(k8sClient.Create(ctx, empty)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, empty) })
		window, _, err := unstructured.NestedStringMap(empty.Object, "spec", "commit", "window")
		Expect(err).NotTo(HaveOccurred())
		Expect(window).To(Equal(map[string]string{"idleTimeout": "5s", "maxDuration": "1m"}),
			"an empty block takes both defaults")

		err = k8sClient.Create(ctx, newTargetWindow("window-idle-past-max", map[string]any{"idleTimeout": "2m"}))
		Expect(err).To(HaveOccurred(), "an idleTimeout past the default 1m maxDuration can never fire")
		Expect(err.Error()).To(ContainSubstring("idleTimeout must not exceed maxDuration"))
	})

	// The one upgrade hazard, pinned: before this release commit.window was a duration string.
	// The new schema refuses that shape at admission rather than storing something no typed client
	// can decode — which is why UPGRADING removes it from stored targets before the upgrade.
	It("refuses the old string commit.window", func() {
		ctx := context.Background()
		legacy := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "configbutler.ai/v1alpha3",
			"kind":       "GitTarget",
			"metadata":   map[string]any{"name": "window-legacy-string", "namespace": "default"},
			"spec": map[string]any{
				"gitProviderRef": map[string]any{"name": "any-provider"},
				"branch":         "main",
				"path":           "clusters/prod",
				"commit":         map[string]any{"window": "5s"},
			},
		}}
		err := k8sClient.Create(ctx, legacy)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.commit.window"))
	})

	newRequest := func(name string, spec map[string]any) *unstructured.Unstructured {
		spec["gitTargetRef"] = map[string]any{"name": "any-target"}
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "configbutler.ai/v1alpha3",
			"kind":       "CommitRequest",
			"metadata":   map[string]any{"name": name, "namespace": "default"},
			"spec":       spec,
		}}
	}

	It("defaults an omitted CommitRequest window and whenNothingToCommit, and keeps explicit zeros", func() {
		ctx := context.Background()

		omitted := &configbutleraiv1alpha3.CommitRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "window-omitted", Namespace: "default"},
			Spec: configbutleraiv1alpha3.CommitRequestSpec{
				GitTargetRef: meta.LocalObjectReference{Name: "any-target"},
			},
		}
		Expect(k8sClient.Create(ctx, omitted)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, omitted) })
		Expect(omitted.Spec.Window).NotTo(BeNil(), "the block default must be filled in")
		Expect(omitted.Spec.Window.Attach).To(Equal(configbutleraiv1alpha3.AttachCurrentOrNext))
		Expect(omitted.Spec.Window.AttachTimeout.Duration).To(Equal(2 * time.Second))
		Expect(omitted.Spec.Window.MaxDuration.Duration).To(Equal(2 * time.Second))
		Expect(omitted.Spec.Window.IdleTimeout).To(BeNil(), "no idle close unless one is asked for")
		Expect(omitted.Spec.WhenNothingToCommit).To(Equal(configbutleraiv1alpha3.NothingToCommitResolve))

		// The pointers exist for exactly this: an explicit "0s" is a value, and defaulting must
		// not read it as an omission.
		zero := &configbutleraiv1alpha3.CommitRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "window-zeros", Namespace: "default"},
			Spec: configbutleraiv1alpha3.CommitRequestSpec{
				GitTargetRef: meta.LocalObjectReference{Name: "any-target"},
				Window: &configbutleraiv1alpha3.CommitRequestWindow{
					AttachTimeout: &metav1.Duration{},
					IdleTimeout:   &metav1.Duration{},
					MaxDuration:   &metav1.Duration{},
				},
			},
		}
		Expect(k8sClient.Create(ctx, zero)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, zero) })
		Expect(zero.Spec.Window.AttachTimeout.Duration).To(BeZero())
		Expect(zero.Spec.Window.IdleTimeout.Duration).To(BeZero())
		Expect(zero.Spec.Window.MaxDuration.Duration).To(BeZero())
	})

	DescribeTable("refuse a CommitRequest window the rules forbid",
		func(name string, spec map[string]any, message string) {
			err := k8sClient.Create(context.Background(), newRequest(name, spec))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(message))
		},
		Entry("a maxDuration past its bound", "window-max-too-long",
			map[string]any{"window": map[string]any{"maxDuration": "10m"}}, "maxDuration must not exceed 5m"),
		Entry("an attachTimeout past its bound", "window-attach-too-long",
			map[string]any{"window": map[string]any{"attachTimeout": "6m"}}, "attachTimeout must not exceed 5m"),
		Entry("an idleTimeout past maxDuration", "window-idle-past-max",
			map[string]any{"window": map[string]any{"idleTimeout": "5s"}}, "idleTimeout must not exceed maxDuration"),
		Entry("Next with no time to wait", "window-next-zero",
			map[string]any{"window": map[string]any{"attach": "Next", "attachTimeout": "0s"}},
			"attach: Next needs a positive attachTimeout"),
		Entry("an attach value that does not exist", "window-attach-bogus",
			map[string]any{"window": map[string]any{"attach": "Current"}}, "spec.window.attach"),
		Entry("CommitEmpty with no message to record", "empty-no-message",
			map[string]any{"whenNothingToCommit": "CommitEmpty"}, "CommitEmpty requires spec.message"),
	)

	It("accepts the documented rolling shape and the bounds themselves", func() {
		ctx := context.Background()
		for name, spec := range map[string]map[string]any{
			"window-rolling": {"window": map[string]any{"idleTimeout": "1s", "maxDuration": "10s"}},
			"window-bounds":  {"window": map[string]any{"attachTimeout": "5m", "maxDuration": "5m"}},
			"window-next":    {"window": map[string]any{"attach": "Next", "attachTimeout": "10s"}},
			"empty-recorded": {"whenNothingToCommit": "CommitEmpty", "message": "save"},
		} {
			request := newRequest(name, spec)
			Expect(k8sClient.Create(ctx, request)).To(Succeed(), name)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, request) })
		}
	})

	// CommitRequest carries the same hazard with immutability on top: its spec may not change
	// after creation, and a whole-object comparison compares a duration as a STRING. "1m" reads
	// back into Go and serializes as "1m0s", so before the rule compared durations instead of
	// spellings, every typed update — including one that only touches a label — was rejected as
	// an attempt to change an immutable spec.
	It("lets a typed client update a CommitRequest created with a re-spelled duration", func() {
		ctx := context.Background()

		// Created as UNSTRUCTURED, carrying the spelling a HUMAN writes. That is the whole
		// reproduction: a typed create serializes metav1.Duration{time.Minute} as "1m0s", so the
		// stored value would already be canonical and the update below would send back a
		// byte-identical spec, testing nothing.
		request := newRequest("window-respelled", map[string]any{
			"window": map[string]any{"attachTimeout": "1m", "idleTimeout": "0.5m", "maxDuration": "1m"},
		})
		Expect(k8sClient.Create(ctx, request)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, request) })

		stored, _, err := unstructured.NestedString(request.Object, "spec", "window", "maxDuration")
		Expect(err).NotTo(HaveOccurred())
		Expect(stored).To(Equal("1m"),
			"the API server stores what was written; nothing normalizes it on the way in")

		key := types.NamespacedName{Name: "window-respelled", Namespace: "default"}
		Eventually(func() error {
			var typed configbutleraiv1alpha3.CommitRequest
			if err := k8sClient.Get(ctx, key, &typed); err != nil {
				return err
			}
			if typed.Labels == nil {
				typed.Labels = map[string]string{}
			}
			typed.Labels["touched"] = "yes"
			return k8sClient.Update(ctx, &typed)
		}, "10s", "200ms").Should(Succeed(),
			"the same duration spelled differently is not a spec change")
	})

	DescribeTable("still refuse a real change to an immutable CommitRequest spec",
		func(name string, mutate func(*configbutleraiv1alpha3.CommitRequestSpec)) {
			ctx := context.Background()
			request := &configbutleraiv1alpha3.CommitRequest{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: configbutleraiv1alpha3.CommitRequestSpec{
					GitTargetRef: meta.LocalObjectReference{Name: "any-target"},
					Message:      "save",
					Window: &configbutleraiv1alpha3.CommitRequestWindow{
						MaxDuration: &metav1.Duration{Duration: time.Minute},
					},
				},
			}
			Expect(k8sClient.Create(ctx, request)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, request) })

			key := types.NamespacedName{Name: request.Name, Namespace: request.Namespace}
			Eventually(func() string {
				var current configbutleraiv1alpha3.CommitRequest
				if err := k8sClient.Get(ctx, key, &current); err != nil {
					return err.Error()
				}
				mutate(&current.Spec)
				if err := k8sClient.Update(ctx, &current); err != nil {
					return err.Error()
				}
				return ""
			}, "10s", "200ms").Should(ContainSubstring("immutable"),
				"comparing by value must not become comparing by nothing")
		},
		Entry("maxDuration", "immutable-max", func(spec *configbutleraiv1alpha3.CommitRequestSpec) {
			spec.Window.MaxDuration = &metav1.Duration{Duration: 30 * time.Second}
		}),
		Entry("attach", "immutable-attach", func(spec *configbutleraiv1alpha3.CommitRequestSpec) {
			spec.Window.Attach = configbutleraiv1alpha3.AttachNext
		}),
		Entry("an idleTimeout added later", "immutable-idle", func(spec *configbutleraiv1alpha3.CommitRequestSpec) {
			spec.Window.IdleTimeout = &metav1.Duration{Duration: time.Second}
		}),
		Entry("whenNothingToCommit", "immutable-empty", func(spec *configbutleraiv1alpha3.CommitRequestSpec) {
			spec.WhenNothingToCommit = configbutleraiv1alpha3.NothingToCommitCommitEmpty
		}),
	)

	// closeDelay is GONE, and a removed field is pruned on write rather than refused by a client
	// that does not validate fields strictly, which is how a controller writes. The UPGRADING entry
	// exists for exactly this: a request that still sets closeDelay: 30s now collects for 2s.
	It("prunes a removed closeDelay instead of honouring it", func() {
		ctx := context.Background()

		legacy := newRequest("legacy-close-delay", map[string]any{"closeDelay": "30s"})
		Expect(k8sClient.Create(ctx, legacy)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, legacy) })

		spec, _, err := unstructured.NestedMap(legacy.Object, "spec")
		Expect(err).NotTo(HaveOccurred())
		Expect(spec).NotTo(HaveKey("closeDelay"), "the removed field is dropped")
		maxDuration, _, err := unstructured.NestedString(legacy.Object, "spec", "window", "maxDuration")
		Expect(err).NotTo(HaveOccurred())
		Expect(maxDuration).To(Equal("2s"), "and the default lands in its place")
	})
})
