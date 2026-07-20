// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

var _ = Describe("ClusterProvider Controller", func() {
	const timeout = 10 * time.Second
	const interval = 200 * time.Millisecond

	readyStatus := func(name string) func() metav1.ConditionStatus {
		return func() metav1.ConditionStatus {
			var got configbutleraiv1alpha3.ClusterProvider
			if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: name}, &got); err != nil {
				return metav1.ConditionUnknown
			}
			c := findCondition(got.Status.Conditions, ConditionTypeReady)
			if c == nil {
				return metav1.ConditionUnknown
			}
			return c.Status
		}
	}

	It("validates the in-cluster 'default' provider and goes Ready", func() {
		// The "default" ClusterProvider is created once in BeforeSuite (as an install ships it — the
		// operator never creates one), so this spec asserts the existing object reconciles to Ready
		// rather than creating its own; "default" is cluster-scoped and name-unique.
		Eventually(readyStatus("default"), timeout, interval).Should(Equal(metav1.ConditionTrue))

		var got configbutleraiv1alpha3.ClusterProvider
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "default"}, &got)).To(Succeed())
		Expect(findCondition(got.Status.Conditions, ClusterProviderConditionValidated).Status).
			To(Equal(metav1.ConditionTrue))
	})

	It("admits any name that omits kubeConfig — in-cluster is not reserved to 'default'", func() {
		// kubeConfig is optional for EVERY provider: omitted means the operator's own cluster,
		// whatever the object is called. "default" only names what an omitted clusterProviderRef
		// points at; it makes no claim about which cluster that is.
		provider := &configbutleraiv1alpha3.ClusterProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "local-extra"},
			Spec: configbutleraiv1alpha3.ClusterProviderSpec{
				AllowedNamespaces: &configbutleraiv1alpha3.NamespaceMatcher{Names: []string{"default"}},
			},
		}
		Expect(k8sClient.Create(context.Background(), provider)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), provider) })

		Eventually(readyStatus("local-extra"), timeout, interval).Should(Equal(metav1.ConditionTrue))
	})

	It("does not forbid a 'default' provider that sets a kubeConfig", func() {
		// The converse of the spec above: a provider named "default" MAY mirror a remote cluster.
		// The existing "default" object makes this create collide on the name, so the assertion is
		// precisely that the rejection is the name being taken — NOT a schema rule tying the name
		// "default" to an absent kubeConfig, which no longer exists.
		provider := &configbutleraiv1alpha3.ClusterProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "default"},
			Spec: configbutleraiv1alpha3.ClusterProviderSpec{
				KubeConfig: &meta.KubeConfigReference{SecretRef: &meta.SecretKeyReference{Name: "kc"}},
			},
		}
		err := k8sClient.Create(context.Background(), provider)
		Expect(apierrors.IsAlreadyExists(err)).To(BeTrue(), "expected a name collision, got %v", err)
		Expect(apierrors.IsInvalid(err)).To(BeFalse(), "the name must not constrain kubeConfig")
	})

	It("validates a remote provider with a valid kubeconfig Secret", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "prod-eu-kc"},
			Data:       map[string][]byte{"value": []byte(scValidKubeConfig)},
		}
		Expect(k8sClient.Create(context.Background(), secret)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), secret) })

		provider := &configbutleraiv1alpha3.ClusterProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "prod-eu-1"},
			Spec: configbutleraiv1alpha3.ClusterProviderSpec{
				KubeConfig: &meta.KubeConfigReference{SecretRef: &meta.SecretKeyReference{Name: "prod-eu-kc"}},
			},
		}
		Expect(k8sClient.Create(context.Background(), provider)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), provider) })

		Eventually(readyStatus("prod-eu-1"), timeout, interval).Should(Equal(metav1.ConditionTrue))
	})

	It("stalls a remote provider whose kubeconfig Secret is missing", func() {
		provider := &configbutleraiv1alpha3.ClusterProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "prod-us-1"},
			Spec: configbutleraiv1alpha3.ClusterProviderSpec{
				KubeConfig: &meta.KubeConfigReference{SecretRef: &meta.SecretKeyReference{Name: "absent-kc"}},
			},
		}
		Expect(k8sClient.Create(context.Background(), provider)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), provider) })

		Eventually(readyStatus("prod-us-1"), timeout, interval).Should(Equal(metav1.ConditionFalse))

		var got configbutleraiv1alpha3.ClusterProvider
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "prod-us-1"}, &got)).To(Succeed())
		Expect(findCondition(got.Status.Conditions, ClusterProviderConditionValidated).Status).
			To(Equal(metav1.ConditionFalse))
		Expect(findCondition(got.Status.Conditions, ConditionTypeStalled).Status).
			To(Equal(metav1.ConditionTrue))
	})
})
