// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

func inventoryTarget(name, provider, branch string) *configbutleraiv1alpha3.GitTarget {
	return &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop"},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			GitProviderRef: meta.LocalObjectReference{Name: provider},
			Branch:         branch,
			Path:           name,
		},
	}
}

// TestBranchInventory_CountsTheGitTargetsThatConfigureEachBranch is the whole field: what is this
// repository being used for, answered from the configuration and sorted so a status patch is sent
// only when the answer moves.
func TestBranchInventory_CountsTheGitTargetsThatConfigureEachBranch(t *testing.T) {
	suspended := inventoryTarget("archive", "repo1", "main")
	suspended.Spec.Suspend = true
	deleting := inventoryTarget("leaving", "repo1", "retired")
	deleting.DeletionTimestamp = &metav1.Time{Time: time.Now()}

	got := branchInventory("repo1", []configbutleraiv1alpha3.GitTarget{
		*inventoryTarget("release", "repo1", "rel"),
		*inventoryTarget("apps", "repo1", "main"),
		*inventoryTarget("infra", "repo1", "main"),
		*suspended,
		*deleting,
		*inventoryTarget("other", "repo2", "main"),
	})

	assert.Equal(t, []configbutleraiv1alpha3.GitProviderBranchStatus{
		{Name: "main", GitTargetCount: 3},
		{Name: "rel", GitTargetCount: 1},
	}, got,
		"every branch a live GitTarget of THIS provider configures, suspended ones included, sorted by name")
}

// TestBranchInventory_AConfiguredAndUnusedRepositoryReportsAnEmptyList is the state the field
// exists to distinguish. It is not an error, and it must not look like one — and it must not look
// like the third state either: an ABSENT list means nothing has read the GitTargets yet, so the
// measured-and-empty answer is an empty slice rather than nil.
func TestBranchInventory_AConfiguredAndUnusedRepositoryReportsAnEmptyList(t *testing.T) {
	measured := branchInventory("repo1", nil)
	assert.NotNil(t, measured, "configured and unused is an answer, not the absence of one")
	assert.Empty(t, measured)

	measured = branchInventory("repo1", []configbutleraiv1alpha3.GitTarget{
		*inventoryTarget("other", "repo2", "main"),
	})
	assert.NotNil(t, measured)
	assert.Empty(t, measured, "a GitTarget on another provider says nothing about this one")
}

// TestPublishBranchInventory_KeepsTheLastAnswerWhenTheTargetsCannotBeRead. An inventory that
// collapsed to nothing on a cache hiccup would report "configured and unused" about a repository
// with GitTargets all over it, which is exactly the misreading the field is here to prevent.
func TestPublishBranchInventory_KeepsTheLastAnswerWhenTheTargetsCannotBeRead(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))
	k8sClient := interceptor.NewClient(
		fake.NewClientBuilder().WithScheme(scheme).Build(),
		interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New("the cache is not ready")
			},
		})
	r := &GitProviderReconciler{Client: k8sClient}
	provider := &configbutleraiv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "repo1", Namespace: "shop"},
		Status: configbutleraiv1alpha3.GitProviderStatus{
			Branches: []configbutleraiv1alpha3.GitProviderBranchStatus{{Name: "main", GitTargetCount: 2}},
		},
	}

	require.Error(t, r.publishBranchInventory(context.Background(), provider))
	assert.Equal(t, []configbutleraiv1alpha3.GitProviderBranchStatus{{Name: "main", GitTargetCount: 2}},
		provider.Status.Branches)
}

// TestPublishBranchInventory_ReadsOnlyTheProvidersOwnNamespace. A GitProvider is referenced from
// its own namespace, and counting a same-named provider's targets in another tenant's namespace
// would report somebody else's configuration.
func TestPublishBranchInventory_ReadsOnlyTheProvidersOwnNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))
	elsewhere := inventoryTarget("apps", "repo1", "other-tenant-branch")
	elsewhere.Namespace = "warehouse"
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(inventoryTarget("apps", "repo1", "main"), elsewhere).Build()
	r := &GitProviderReconciler{Client: k8sClient}
	provider := &configbutleraiv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "repo1", Namespace: "shop"},
	}

	require.NoError(t, r.publishBranchInventory(context.Background(), provider))
	assert.Equal(t, []configbutleraiv1alpha3.GitProviderBranchStatus{{Name: "main", GitTargetCount: 1}},
		provider.Status.Branches)
}
