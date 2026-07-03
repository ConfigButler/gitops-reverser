// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"
	"time"

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
			TargetRef: configv1alpha3.LocalTargetReference{Name: "playground-target"},
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
			ProviderRef: configv1alpha3.GitProviderReference{Name: "playground-provider"},
			Branch:      "main",
			Path:        "live-cluster",
		},
	}
	existingGitProvider := &configv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "playground-provider", Namespace: "tilt-playground"},
		Spec: configv1alpha3.GitProviderSpec{
			URL:             "https://example.invalid/playground.git",
			AllowedBranches: []string{"main"},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tilt-playground"}},
			existingWatchRule,
			existingGitTarget,
			existingGitProvider,
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
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-services"},
		Spec: configv1alpha3.ClusterWatchRuleSpec{
			TargetRef: configv1alpha3.NamespacedTargetReference{Name: "ops-target", Namespace: "ops"},
			Rules: []configv1alpha3.ClusterResourceRule{{
				Scope:       configv1alpha3.ResourceScopeNamespaced,
				APIGroups:   []string{""},
				APIVersions: []string{"v1"},
				Resources:   []string{"services"},
			}},
		},
	}
	existingGitTarget := &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-target", Namespace: "ops"},
		Spec: configv1alpha3.GitTargetSpec{
			ProviderRef: configv1alpha3.GitProviderReference{Name: "ops-provider"},
			Branch:      "main",
			Path:        "cluster-state",
		},
	}
	existingGitProvider := &configv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-provider", Namespace: "ops"},
		Spec: configv1alpha3.GitProviderSpec{
			URL:             "https://example.invalid/ops.git",
			AllowedBranches: []string{"main"},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ops"}},
			existingClusterWatchRule,
			existingGitTarget,
			existingGitProvider,
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
