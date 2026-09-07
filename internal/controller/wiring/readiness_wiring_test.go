// SPDX-License-Identifier: Apache-2.0

// Package wiring holds integration tests for controller WATCH WIRING: what a controller is woken
// up by, rather than what one reconcile computes.
//
// It runs its own envtest with only the controllers under test registered. That isolation is the
// point rather than tidiness. In internal/controller's suite the GitTarget controller is live and
// owns the GitTarget status these tests write by hand, rewriting it on a 10s cadence, and a second
// manager racing the suite's on rule status produced 100ms retry requeues that masked a removed
// predicate entirely. Both made a wiring assertion here unreliable in opposite directions.
package wiring

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/controller"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

// propagationBudget is shorter than BOTH cadences a rule could otherwise be rescued by:
// RequeueStreamSettleInterval (10s) while converging and RequeueSteadyInterval (5m) once settled.
// Only an event-driven enqueue meets it, which is what makes these tests fail — rather than merely
// run slower — if the GitTarget watch goes back to GenerationChangedPredicate.
const propagationBudget = 4 * time.Second

// settleBudget is the generous wait for a first reconcile, which races manager start and cache sync
// and has no bearing on what is being proven.
const settleBudget = 30 * time.Second

var (
	testEnv   *envtest.Environment
	k8sClient client.Client
	restCfg   *rest.Config
)

// TestMain bootstraps the shared envtest environment the wiring tests run against.
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// run bootstraps envtest, runs the tests, and always tears the environment down.
func run(m *testing.M) int {
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	restCfg, err = testEnv.Start()
	if err != nil {
		panic("start envtest: " + err.Error())
	}
	defer func() { _ = testEnv.Stop() }()

	if err := configbutleraiv1alpha3.AddToScheme(scheme.Scheme); err != nil {
		panic("add scheme: " + err.Error())
	}
	k8sClient, err = client.New(restCfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		panic("build client: " + err.Error())
	}

	return m.Run()
}

// startManager runs a manager carrying only what register installs, and stops it when the test ends.
func startManager(t *testing.T, register func(manager.Manager) error) {
	t.Helper()

	mgr, err := manager.New(restCfg, manager.Options{
		Scheme:         scheme.Scheme,
		Metrics:        metricsserver.Options{BindAddress: "0"},
		LeaderElection: false,
	})
	require.NoError(t, err)
	require.NoError(t, register(mgr))

	mgrCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = mgr.Start(mgrCtx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	require.True(t, mgr.GetCache().WaitForCacheSync(mgrCtx))
}

// seedTarget creates a namespace, a GitProvider and a GitTarget, and returns the namespace. When
// clusterProvider is non-empty it also creates that ClusterProvider admitting every namespace and
// points the GitTarget at it: a ClusterWatchRule is refused (and its GitTargetReady overwritten with
// the refusal) unless the source cluster admits the target's namespace.
func seedTarget(ctx context.Context, t *testing.T, name, clusterProvider string) string {
	t.Helper()

	namespace := name
	require.NoError(t, k8sClient.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}))
	require.NoError(t, k8sClient.Create(ctx, &configbutleraiv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "provider", Namespace: namespace},
		Spec: configbutleraiv1alpha3.GitProviderSpec{
			URL:             "https://github.com/test/repo.git",
			AllowedBranches: []string{"*"},
			SecretRef:       &meta.LocalObjectReference{Name: "git-credentials"},
		},
	}))
	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: namespace},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			GitProviderRef: meta.LocalObjectReference{Name: "provider"},
			Branch:         "main",
			Path:           "mirror",
		},
	}
	if clusterProvider != "" {
		// selector: {} is the "every namespace" form; names: ["*"] would be a literal name.
		require.NoError(t, k8sClient.Create(ctx, &configbutleraiv1alpha3.ClusterProvider{
			ObjectMeta: metav1.ObjectMeta{Name: clusterProvider},
			Spec: configbutleraiv1alpha3.ClusterProviderSpec{
				AccessFrom: &configbutleraiv1alpha3.NamespaceMatcher{
					Selector: &metav1.LabelSelector{},
				},
			},
		}))
		target.Spec.ClusterProviderRef = &meta.LocalObjectReference{Name: clusterProvider}
	}
	require.NoError(t, k8sClient.Create(ctx, target))
	return namespace
}

// setTargetReady writes ONLY the GitTarget's status. The spec, and therefore the generation, is
// untouched: this is exactly the shape of update GenerationChangedPredicate drops.
func setTargetReady(
	ctx context.Context,
	t *testing.T,
	namespace string,
	status metav1.ConditionStatus,
	reason string,
) {
	t.Helper()

	key := types.NamespacedName{Name: "target", Namespace: namespace}
	target := &configbutleraiv1alpha3.GitTarget{}
	require.NoError(t, k8sClient.Get(ctx, key, target))
	generationBefore := target.Generation

	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, key, target); err != nil {
			return err
		}
		apimeta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
			Type:    controller.GitTargetConditionReady,
			Status:  status,
			Reason:  reason,
			Message: "set by the wiring test",
		})
		return k8sClient.Status().Update(ctx, target)
	}))

	require.NoError(t, k8sClient.Get(ctx, key, target))
	require.Equal(t, generationBefore, target.Generation, "the recovery must be a status-only update")
}

// awaitMirroredReady polls the rule's mirrored GitTargetReady until it reads want, or fails.
func awaitMirroredReady(
	ctx context.Context,
	t *testing.T,
	key types.NamespacedName,
	fresh func() client.Object,
	want metav1.ConditionStatus,
	budget time.Duration,
) {
	t.Helper()

	deadline := time.Now().Add(budget)
	last := "absent"
	for time.Now().Before(deadline) {
		obj := fresh()
		if err := k8sClient.Get(ctx, key, obj); err == nil {
			var conditions []metav1.Condition
			switch typed := obj.(type) {
			case *configbutleraiv1alpha3.WatchRule:
				conditions = typed.Status.Conditions
			case *configbutleraiv1alpha3.ClusterWatchRule:
				conditions = typed.Status.Conditions
			}
			if c := apimeta.FindStatusCondition(conditions, controller.ConditionTypeGitTargetReady); c != nil {
				if c.Status == want {
					return
				}
				last = string(c.Status)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("GitTargetReady never reached %q within %s (last seen %q)", want, budget, last)
}

// TestWatchRule_FollowsGitTargetStatusOnlyRecovery is the regression test for a rule whose mirrored
// GitTargetReady lagged its target by a full requeue. It fails if the GitTarget watch stops
// reacting to a status-only readiness move.
func TestWatchRule_FollowsGitTargetStatusOnlyRecovery(t *testing.T) {
	ctx := context.Background()
	startManager(t, func(m manager.Manager) error {
		return (&controller.WatchRuleReconciler{
			Client:    m.GetClient(),
			Scheme:    m.GetScheme(),
			RuleStore: rulestore.NewStore(),
		}).SetupWithManager(m)
	})

	namespace := seedTarget(ctx, t, "wiring-watchrule", "")
	rule := &configbutleraiv1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: "rule", Namespace: namespace},
		Spec: configbutleraiv1alpha3.WatchRuleSpec{
			GitTargetRef: meta.LocalObjectReference{Name: "target"},
			Rules:        []configbutleraiv1alpha3.ResourceRule{{Resources: []string{"configmaps"}}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, rule))
	key := client.ObjectKeyFromObject(rule)

	// Drive the target to a known FAILING readiness and let the rule settle on it.
	setTargetReady(ctx, t, namespace, metav1.ConditionFalse, "WatchError")
	awaitMirroredReady(ctx, t, key, func() client.Object {
		return &configbutleraiv1alpha3.WatchRule{}
	}, metav1.ConditionFalse, settleBudget)

	// Heal it, status-only, and require the rule to follow inside the budget.
	setTargetReady(ctx, t, namespace, metav1.ConditionTrue, "Succeeded")
	awaitMirroredReady(ctx, t, key, func() client.Object {
		return &configbutleraiv1alpha3.WatchRule{}
	}, metav1.ConditionTrue, propagationBudget)
}

// TestClusterWatchRule_FollowsGitTargetStatusOnlyRecovery is the same regression for the
// cluster-scoped rule, which carried the identical wiring.
func TestClusterWatchRule_FollowsGitTargetStatusOnlyRecovery(t *testing.T) {
	ctx := context.Background()
	startManager(t, func(m manager.Manager) error {
		return (&controller.ClusterWatchRuleReconciler{
			Client:    m.GetClient(),
			Scheme:    m.GetScheme(),
			RuleStore: rulestore.NewStore(),
		}).SetupWithManager(m)
	})

	namespace := seedTarget(ctx, t, "wiring-clusterwatchrule", "wiring-source")
	rule := &configbutleraiv1alpha3.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-rule"},
		Spec: configbutleraiv1alpha3.ClusterWatchRuleSpec{
			GitTargetRef: meta.NamespacedObjectReference{Name: "target", Namespace: namespace},
			Rules:        []configbutleraiv1alpha3.ClusterResourceRule{{Resources: []string{"namespaces"}}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, rule))
	key := client.ObjectKeyFromObject(rule)

	setTargetReady(ctx, t, namespace, metav1.ConditionFalse, "WatchError")
	awaitMirroredReady(ctx, t, key, func() client.Object {
		return &configbutleraiv1alpha3.ClusterWatchRule{}
	}, metav1.ConditionFalse, settleBudget)

	setTargetReady(ctx, t, namespace, metav1.ConditionTrue, "Succeeded")
	awaitMirroredReady(ctx, t, key, func() client.Object {
		return &configbutleraiv1alpha3.ClusterWatchRule{}
	}, metav1.ConditionTrue, propagationBudget)
}
