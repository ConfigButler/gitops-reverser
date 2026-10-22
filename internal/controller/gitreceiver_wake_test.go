// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// GitReceiverReady clears only when another object changes, so it must not wait out the provider's
// steady requeue (5 minutes). These specs drive the manager's own controllers and touch nothing but
// the objects: every transition below can only arrive through a watch.
var _ = Describe("GitReceiverReady wake paths", func() {
	const namespace = "default"

	receiverReadyOf := func(key types.NamespacedName) func(g Gomega) string {
		return func(g Gomega) string {
			provider := &configbutleraiv1alpha3.GitProvider{}
			g.Expect(k8sClient.Get(context.Background(), key, provider)).To(Succeed())
			cond := findCondition(provider.Status.Conditions, GitProviderConditionGitReceiverReady)
			if cond == nil {
				return ""
			}
			return string(cond.Status) + "/" + cond.Reason
		}
	}

	It("follows the GitReceiver appearing, getting its token, and going", func() {
		ctx := context.Background()
		provider := &configbutleraiv1alpha3.GitProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "woken-provider", Namespace: namespace},
			Spec: configbutleraiv1alpha3.GitProviderSpec{
				URL:             "https://example.invalid/unreachable.git",
				AllowedBranches: []string{"main"},
				GitReceiverRef:  &meta.NamespacedObjectReference{Name: "woken-receiver"},
			},
		}
		Expect(k8sClient.Create(ctx, provider)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, provider) })
		key := types.NamespacedName{Namespace: namespace, Name: provider.Name}

		Eventually(receiverReadyOf(key), "30s", "250ms").Should(Equal("False/GitReceiverNotFound"))

		receiver := &configbutleraiv1alpha3.GitReceiver{
			ObjectMeta: metav1.ObjectMeta{Name: "woken-receiver", Namespace: namespace},
			Spec: configbutleraiv1alpha3.GitReceiverSpec{
				Type:      configbutleraiv1alpha3.ReceiverTypeGitea,
				SecretRef: meta.LocalObjectReference{Name: "woken-receiver-token"},
			},
		}
		Expect(k8sClient.Create(ctx, receiver)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, receiver) })
		Eventually(receiverReadyOf(key), "30s", "250ms").Should(Equal("False/"+ReasonGitReceiverNotReady),
			"the receiver appearing wakes the provider; it has no token yet")

		token := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "woken-receiver-token", Namespace: namespace},
			Data:       map[string][]byte{configbutleraiv1alpha3.ReceiverTokenKey: []byte("wake-token")},
		}
		Expect(k8sClient.Create(ctx, token)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, token) })
		// Nothing watches the Secret: the receiver finds it on its token poll, and its Ready turning
		// True is what wakes the provider.
		Eventually(receiverReadyOf(key), RequeueReceiverTokenInterval+30*time.Second, "500ms").
			Should(Equal("True/" + ReasonSucceeded))

		Expect(k8sClient.Delete(ctx, receiver)).To(Succeed())
		Eventually(receiverReadyOf(key), "30s", "250ms").Should(Equal("False/GitReceiverNotFound"),
			"the receiver going wakes the provider too")
	})
})
