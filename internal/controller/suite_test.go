// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"path/filepath"
	"testing"

	meta "github.com/fluxcd/pkg/apis/meta"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/attribution"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/kubeconfig"
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
	// testWorkerManager is the suite's worker manager. No GitBranch reconciler runs here, so a spec
	// that needs a GitBranch to hold its branch claims it directly (holdGitBranchForTest).
	testWorkerManager *git.WorkerManager
)

// The suite's shared KubeProvider: the operator's own identity, in the "default" namespace, listed
// in the suite reconciler's --in-cluster-kube-providers equivalent and admitting every namespace,
// the way an install that mirrors its own cluster ships one. A spec's GitTargets and applier claims name
// it through testKubeProviderRef.
const (
	testKubeProviderNamespace = "default"
	testKubeProviderName      = "local"
)

// testKubeProviderRef names the suite's shared in-cluster KubeProvider.
func testKubeProviderRef() meta.NamespacedObjectReference {
	return meta.NamespacedObjectReference{Namespace: testKubeProviderNamespace, Name: testKubeProviderName}
}

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

	// Mirror the production client wiring (cmd/main.go): Secrets are NEVER cached. The control
	// plane runs no Secret watch, so a cached read is served by a lazily started cluster-wide
	// Secret informer whose view lags every write the suite makes — and a stale answer here is not
	// a slow test, it is a wrong one. The GitTarget encryption gate reads its age-key Secret by
	// name, and any error or stale hit there parks the target on RequeueSteadyInterval (5 minutes),
	// far past what a spec waits for. That is what made "Should recreate encryption secret when it
	// is deleted while GitTarget still exists" fail roughly one run in ten: the reconcile that
	// should have seen the deletion was served the deleted Secret from cache, or read back a
	// just-created one that the informer had not observed yet.
	//
	// Keep this in step with setupManager in cmd/main.go: a suite whose client is wired
	// differently from the binary tests a controller that does not ship.
	mgr, err = manager.New(cfg, manager.Options{
		Scheme: scheme.Scheme,
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{&corev1.Secret{}},
			},
		},
	})
	Expect(err).NotTo(HaveOccurred())

	Expect(IndexGitBranches(ctx, mgr.GetFieldIndexer())).To(Succeed())

	testRuleStore = rulestore.NewStore()

	// Initialize WorkerManager for new architecture
	testWorkerManager = git.NewWorkerManager(
		mgr.GetClient(), logf.Log.WithName("worker-manager"), git.BranchWorkerLimits{}, types.SensitiveResourcePolicy{},
	)
	workerManager := testWorkerManager
	err = mgr.Add(workerManager)
	Expect(err).NotTo(HaveOccurred())

	err = (&GitProviderReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	err = (&GitReceiverReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		ListenerEnabled: true,
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	// Wired with a live route registry, as the operator is by default (--author-attribution=true):
	// that is what exercises the first-fact channel source and the AuditFactsReceived condition.
	err = (&KubeProviderReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		KubeConfigSafety: kubeconfig.SafetyPolicy{InClusterProviders: map[k8stypes.NamespacedName]struct{}{
			{Namespace: testKubeProviderNamespace, Name: testKubeProviderName}: {},
		}},
		AuditFacts: &attribution.RouteHealth{},
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	err = (&GitTargetReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		WorkerManager: workerManager,
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	// No ApplierClients: an applier claim's applier is never read here, so Configured stays Unknown
	// (NotChecked), which is enough to drive the watches between an applier claim and its GitTargets.
	for _, kind := range []string{
		configbutleraiv1alpha3.ApplierClaimKindFlux, configbutleraiv1alpha3.ApplierClaimKindArgoCD,
	} {
		err = (&ApplierClaimReconciler{Client: mgr.GetClient(), Kind: kind}).SetupWithManager(mgr)
		Expect(err).NotTo(HaveOccurred())
	}

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
	// Progressing state. Neither ever completes, so these specs cover only the
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

	// Ship the suite's shared in-cluster KubeProvider, so a GitTarget or applier claim naming it
	// (testKubeProviderRef) passes the reconcile-time hard gate that REQUIRES the referenced
	// KubeProvider to exist. Its grants admit every namespace the specs create, to mirror its own
	// namespace and every cluster-scoped resource, and to hold appliers anywhere.
	Expect(k8sClient.Create(ctx, &configbutleraiv1alpha3.KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Namespace: testKubeProviderNamespace, Name: testKubeProviderName},
		Spec: configbutleraiv1alpha3.KubeProviderSpec{
			InCluster: &configbutleraiv1alpha3.KubeProviderInCluster{},
			AllowedGitTargets: []configbutleraiv1alpha3.KubeProviderGitTargetGrant{
				{
					ConsumerNamespaces: configbutleraiv1alpha3.ConsumerNamespaces{
						From: configbutleraiv1alpha3.GrantFromAll,
					},
					SourceNamespaces: []configbutleraiv1alpha3.ScopeNamespace{
						configbutleraiv1alpha3.ConsumerNamespaceVariable,
					},
					ClusterResources: []configbutleraiv1alpha3.ClusterResourceIdentity{"*"},
				},
			},
			AllowedApplierClaims: []configbutleraiv1alpha3.KubeProviderApplierClaimGrant{
				{
					ConsumerNamespaces: configbutleraiv1alpha3.ConsumerNamespaces{
						From: configbutleraiv1alpha3.GrantFromAll,
					},
					ApplierNamespaces: []configbutleraiv1alpha3.ScopeNamespace{"*"},
				},
			},
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
