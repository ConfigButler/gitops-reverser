// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	// +kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var (
	cfg           *rest.Config
	k8sClient     client.Client
	testEnv       *envtest.Environment
	ctx           context.Context
	cancel        context.CancelFunc
	mgr           manager.Manager
	testRuleStore *rulestore.RuleStore
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	err = configbutleraiv1alpha3.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:scheme

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	mgr, err = manager.New(cfg, manager.Options{
		Scheme: scheme.Scheme,
	})
	Expect(err).NotTo(HaveOccurred())

	testRuleStore = rulestore.NewStore()

	// Initialize WorkerManager for new architecture
	workerManager := git.NewWorkerManager(
		mgr.GetClient(), logf.Log.WithName("worker-manager"), 0, types.SensitiveResourcePolicy{},
	)
	err = mgr.Add(workerManager)
	Expect(err).NotTo(HaveOccurred())

	err = (&GitProviderReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	err = (&ClusterProviderReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		OperatorNamespace: "default",
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	err = (&GitTargetReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		WorkerManager: workerManager,
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	err = (&WatchRuleReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		RuleStore: testRuleStore,
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	err = (&ClusterWatchRuleReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		RuleStore: testRuleStore,
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	// A non-nil AuthorLookup that misses (no admission record) resolves to the
	// committer immediately, and a Finalizer that never resolves keeps requests in the
	// WaitingForCloseDelay wait. Neither ever completes, so these specs cover only the
	// initial in-progress stamp and the terminal short-circuit.
	err = (&CommitRequestReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		Finalizer:    &fakeFinalizer{resolved: false},
		AuthorLookup: &fakeAuthorLookup{},
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	// Note: Old git.Worker has been replaced by WorkerManager + BranchWorker architecture
	// Webhook tests are handled separately in webhook package

	// Ship the reserved "default" ClusterProvider the way a real install (chart / config SUT) does,
	// so a GitTarget that references it (the schema default) passes the reconcile-time hard gate
	// that now REQUIRES the referenced ClusterProvider to exist. Its empty selector admits every
	// namespace, matching the chart default.
	Expect(k8sClient.Create(ctx, &configbutleraiv1alpha3.ClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: configbutleraiv1alpha3.DefaultClusterProviderName},
		Spec: configbutleraiv1alpha3.ClusterProviderSpec{
			AllowedNamespaces: &configbutleraiv1alpha3.NamespaceMatcher{Selector: &metav1.LabelSelector{}},
		},
	})).To(Succeed())

	go func() {
		defer GinkgoRecover()
		err = mgr.Start(ctx)
		Expect(err).ToNot(HaveOccurred(), "failed to run manager")
	}()
})

var _ = AfterSuite(func() {
	cancel()
	By("tearing down the test environment")
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})
