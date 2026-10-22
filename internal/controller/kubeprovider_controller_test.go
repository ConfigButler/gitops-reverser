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
	"sigs.k8s.io/controller-runtime/pkg/client"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
	"github.com/ConfigButler/gitops-reverser/internal/kubeconfig"
)

var _ = Describe("KubeProvider Controller", func() {
	const timeout = 10 * time.Second
	const interval = 200 * time.Millisecond

	condition := func(key types.NamespacedName, conditionType string) func() *metav1.Condition {
		return func() *metav1.Condition {
			var got configbutleraiv1alpha3.KubeProvider
			if err := k8sClient.Get(context.Background(), key, &got); err != nil {
				return nil
			}
			return findCondition(got.Status.Conditions, conditionType)
		}
	}
	readyStatus := func(key types.NamespacedName) func() metav1.ConditionStatus {
		return func() metav1.ConditionStatus {
			c := condition(key, ConditionTypeReady)()
			if c == nil {
				return metav1.ConditionUnknown
			}
			return c.Status
		}
	}
	reasonOf := func(key types.NamespacedName, conditionType string) func() string {
		return func() string {
			c := condition(key, conditionType)()
			if c == nil {
				return ""
			}
			return c.Reason
		}
	}
	create := func(obj client.Object) {
		Expect(k8sClient.Create(context.Background(), obj)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), obj) })
	}

	It("validates the suite's listed inCluster provider and goes Ready", func() {
		key := types.NamespacedName{Namespace: testKubeProviderNamespace, Name: testKubeProviderName}
		Eventually(readyStatus(key), timeout, interval).Should(Equal(metav1.ConditionTrue))
		Expect(reasonOf(key, KubeProviderConditionValidated)()).To(Equal(ReasonInCluster))
	})

	It("stalls an inCluster provider the installation does not list", func() {
		provider := &configbutleraiv1alpha3.KubeProvider{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "unlisted"},
			Spec: configbutleraiv1alpha3.KubeProviderSpec{
				InCluster: &configbutleraiv1alpha3.KubeProviderInCluster{},
			},
		}
		create(provider)
		key := provider.Key()

		Eventually(reasonOf(key, ConditionTypeStalled), timeout, interval).
			Should(Equal(kubeconfig.ReasonInClusterNotAuthorized))
		Expect(condition(key, KubeProviderConditionValidated)().Status).To(Equal(metav1.ConditionFalse))
		Expect(readyStatus(key)()).To(Equal(metav1.ConditionFalse))
	})

	It("validates a remote provider whose kubeconfig Secret is in its own namespace", func() {
		create(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "prod-eu-kc"},
			Data:       map[string][]byte{"value": []byte(scValidKubeConfig)},
		})
		provider := &configbutleraiv1alpha3.KubeProvider{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "prod-eu-1"},
			Spec: configbutleraiv1alpha3.KubeProviderSpec{
				SecretRef: &meta.SecretKeyReference{Name: "prod-eu-kc"},
			},
		}
		create(provider)

		Eventually(readyStatus(provider.Key()), timeout, interval).Should(Equal(metav1.ConditionTrue))
	})

	It("stalls a remote provider whose kubeconfig Secret is missing", func() {
		provider := &configbutleraiv1alpha3.KubeProvider{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "prod-us-1"},
			Spec: configbutleraiv1alpha3.KubeProviderSpec{
				SecretRef: &meta.SecretKeyReference{Name: "absent-kc"},
			},
		}
		create(provider)
		key := provider.Key()

		Eventually(readyStatus(key), timeout, interval).Should(Equal(metav1.ConditionFalse))
		Expect(reasonOf(key, KubeProviderConditionValidated)()).To(Equal(kubeconfig.ReasonSecretNotFound))
		Expect(condition(key, ConditionTypeStalled)().Status).To(Equal(metav1.ConditionTrue))
	})

	It("holds the credential to exactly one immutable arm", func() {
		ctx := context.Background()
		spec := func(name string, s configbutleraiv1alpha3.KubeProviderSpec) *configbutleraiv1alpha3.KubeProvider {
			return &configbutleraiv1alpha3.KubeProvider{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}, Spec: s,
			}
		}
		qps := int32(5)

		Expect(k8sClient.Create(ctx, spec("no-arm", configbutleraiv1alpha3.KubeProviderSpec{}))).
			To(MatchError(ContainSubstring("set exactly one of spec.secretRef and spec.inCluster")))
		Expect(k8sClient.Create(ctx, spec("both-arms", configbutleraiv1alpha3.KubeProviderSpec{
			SecretRef: &meta.SecretKeyReference{Name: "kc"},
			InCluster: &configbutleraiv1alpha3.KubeProviderInCluster{},
		}))).To(MatchError(ContainSubstring("set exactly one of spec.secretRef and spec.inCluster")))
		Expect(k8sClient.Create(ctx, spec("throttled-in-cluster", configbutleraiv1alpha3.KubeProviderSpec{
			InCluster: &configbutleraiv1alpha3.KubeProviderInCluster{},
			Client:    &configbutleraiv1alpha3.KubeProviderClient{QPS: &qps},
		}))).To(MatchError(ContainSubstring("spec.client is refused beside spec.inCluster")))

		provider := spec("immutable-credential", configbutleraiv1alpha3.KubeProviderSpec{
			SecretRef: &meta.SecretKeyReference{Name: "kc"},
		})
		create(provider)
		Eventually(func() error {
			current := &configbutleraiv1alpha3.KubeProvider{}
			if err := k8sClient.Get(ctx, provider.Key(), current); err != nil {
				return err
			}
			current.Spec.SecretRef.Name = "other-kc"
			return k8sClient.Update(ctx, current)
		}, timeout, interval).Should(MatchError(ContainSubstring("spec.secretRef and spec.inCluster are immutable")))
	})

	It("stalls the younger of two providers in different namespaces on one route, until the holder goes", func() {
		// Through the real manager this also drives auditRouteSiblingsHandler: deleting the holder
		// is the only event that can clear the conflict, and the sibling watch is what delivers it
		// rather than the five-minute steady requeue.
		const route = "envtest-shared-route"
		otherNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kp-route-conflict"}}
		create(otherNS)

		listed := func(namespace, name string) *configbutleraiv1alpha3.KubeProvider {
			return &configbutleraiv1alpha3.KubeProvider{
				ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
				Spec: configbutleraiv1alpha3.KubeProviderSpec{
					SecretRef:   &meta.SecretKeyReference{Name: "route-kc"},
					Attribution: &configbutleraiv1alpha3.KubeProviderAttribution{AuditRoute: route},
				},
			}
		}
		for _, ns := range []string{"default", otherNS.Name} {
			create(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "route-kc"},
				Data:       map[string][]byte{"value": []byte(scValidKubeConfig)},
			})
		}

		holder := listed("default", "route-holder")
		Expect(k8sClient.Create(context.Background(), holder)).To(Succeed())
		Eventually(readyStatus(holder.Key()), timeout, interval).Should(Equal(metav1.ConditionTrue))
		// creationTimestamp has one-second resolution: make the second provider unambiguously
		// younger so the oldest-carrier rule, not the UID tie-break, decides.
		time.Sleep(1100 * time.Millisecond)

		younger := listed(otherNS.Name, "route-loser")
		create(younger)
		Eventually(reasonOf(younger.Key(), ConditionTypeStalled), timeout, interval).
			Should(Equal(authz.ReasonAuditRouteConflict))
		Expect(condition(younger.Key(), KubeProviderConditionValidated)().Status).To(Equal(metav1.ConditionTrue))
		Consistently(readyStatus(holder.Key()), time.Second, interval).Should(Equal(metav1.ConditionTrue))

		Expect(k8sClient.Delete(context.Background(), holder)).To(Succeed())
		Eventually(readyStatus(younger.Key()), timeout, interval).Should(Equal(metav1.ConditionTrue))
	})
})
