// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

func TestManagerStart_MustSeedRuleStoreFromExistingWatchRules(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha3.AddToScheme(scheme))

	existingWatchRule := &configv1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: "playground-watchrule", Namespace: "tilt-playground"},
		Spec: configv1alpha3.WatchRuleSpec{
			GitTargetRef: meta.LocalObjectReference{Name: "playground-target"},
			Rules: []configv1alpha3.ResourceRule{{
				APIGroups:   []string{""},
				APIVersions: []string{"v1"},
				Resources:   []string{"services"},
			}},
		},
	}
	existingGitTarget := &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "playground-target", Namespace: "tilt-playground"},
		Spec: configv1alpha3.GitTargetSpec{
			GitBranchRef:    meta.NamespacedObjectReference{Name: gitBranchNameForTest("playground-provider", "main")},
			KubeProviderRef: meta.NamespacedObjectReference{Name: "local"},
			Path:            "live-cluster",
		},
	}
	// A WatchRule's own namespace is watched only through a grant, so the fixture ships the provider
	// that gives it.
	localKubeProvider := &configv1alpha3.KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: "tilt-playground"},
		Spec: configv1alpha3.KubeProviderSpec{
			InCluster: &configv1alpha3.KubeProviderInCluster{},
			AllowedGitTargets: []configv1alpha3.SourceGrant{{From: configv1alpha3.GrantFromSame,
				SourceNamespaces: []configv1alpha3.ScopeNamespace{configv1alpha3.ConsumerNamespaceVariable}}},
		},
	}
	existingGitProvider := &configv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "playground-provider", Namespace: "tilt-playground"},
		Spec: configv1alpha3.GitProviderSpec{
			URL: "https://example.invalid/playground.git",
			AllowedGitBranches: []configv1alpha3.GitBranchGrant{
				{From: configv1alpha3.GrantFromSame, Branches: []configv1alpha3.BranchPattern{"main"}},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tilt-playground"}},
			existingWatchRule,
			existingGitTarget,
			existingGitProvider,
			localKubeProvider,
		).
		Build()

	var watchRules configv1alpha3.WatchRuleList
	require.NoError(t, fakeClient.List(context.Background(), &watchRules))
	require.Len(t, watchRules.Items, 1)

	store := rulestore.NewStore()
	manager := &Manager{
		Client:          fakeClient,
		Log:             logr.Discard(),
		RuleStore:       store,
		resourceCatalog: newCommonTestCatalog(t),
		discoveryClient: commonTestDiscoveryClient(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	require.NoError(t, manager.Start(ctx))

	// After startup, the RuleStore must contain the existing WatchRule.
	assert.NotEmpty(t, store.SnapshotWatchRules(),
		"startup must seed rule store from existing WatchRule CRs")
	assert.NotEmpty(t, manager.ComputeRequestedGVRs(),
		"startup must compute GVRs from seeded rules")
}

func TestManagerStart_MustSeedRuleStoreFromExistingClusterWatchRules(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha3.AddToScheme(scheme))

	existingClusterWatchRule := &configv1alpha3.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-namespaces"},
		Spec: configv1alpha3.ClusterWatchRuleSpec{
			GitTargetRef: meta.NamespacedObjectReference{Name: "ops-target", Namespace: "ops"},
			Rules: []configv1alpha3.ClusterResourceRule{{
				APIGroups:   []string{""},
				APIVersions: []string{"v1"},
				Resources:   []string{"namespaces"},
			}},
		},
	}
	existingGitTarget := &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-target", Namespace: "ops"},
		Spec: configv1alpha3.GitTargetSpec{
			GitBranchRef:    meta.NamespacedObjectReference{Name: gitBranchNameForTest("ops-provider", "main")},
			KubeProviderRef: meta.NamespacedObjectReference{Name: "local"},
			Path:            "cluster-state",
		},
	}
	existingGitProvider := &configv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-provider", Namespace: "ops"},
		Spec: configv1alpha3.GitProviderSpec{
			URL: "https://example.invalid/ops.git",
			AllowedGitBranches: []configv1alpha3.GitBranchGrant{
				{From: configv1alpha3.GrantFromSame, Branches: []configv1alpha3.BranchPattern{"main"}},
			},
		},
	}

	// existingGitTarget references the KubeProvider ops/local, in its own namespace. Bootstrap
	// applies that provider's grants before seeding a ClusterWatchRule, so the fixture must ship the
	// provider, granting its own namespace every cluster-scoped resource. Without it the seed is
	// correctly REFUSED, which is the behavior TestBootstrapClusterWatchRule_RefusesMissingKubeProvider
	// covers; this test is about the admitted path.
	localKubeProvider := &configv1alpha3.KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: "ops"},
		Spec: configv1alpha3.KubeProviderSpec{
			InCluster: &configv1alpha3.KubeProviderInCluster{},
			AllowedGitTargets: []configv1alpha3.SourceGrant{{From: configv1alpha3.GrantFromSame,
				ClusterResources: []configv1alpha3.ClusterResourceIdentity{"*"}}},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ops"}},
			existingClusterWatchRule,
			existingGitTarget,
			existingGitProvider,
			localKubeProvider,
		).
		Build()

	store := rulestore.NewStore()
	manager := &Manager{
		Client:          fakeClient,
		Log:             logr.Discard(),
		RuleStore:       store,
		resourceCatalog: newCommonTestCatalog(t),
		discoveryClient: commonTestDiscoveryClient(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	require.NoError(t, manager.Start(ctx))

	// After startup, the RuleStore must contain the existing ClusterWatchRule.
	assert.NotEmpty(t, store.SnapshotClusterWatchRules(),
		"startup must seed rule store from existing ClusterWatchRule CRs")
	assert.NotEmpty(t, manager.ComputeRequestedGVRs(),
		"startup must compute GVRs from seeded cluster rules")
}
