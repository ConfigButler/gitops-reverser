// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// This is a claim about the APISERVER and the wire format together, so it is pinned by execution:
// an omitted grant list on GitBranch and KubeProvider reads back as the CRD default, and [] reads
// back as [] through a create and a full update. The Go fields carry no omitempty, so a nil list
// goes out as null, which the API server prunes and then defaults, and an empty list goes out as
// []. With omitempty, any Update of the object (a finalizer, a label) would send [] as omitted, and
// an explicit deny-all would come back as the default grant.
func TestGrantLists_APIServerDefaultsOmittedAndKeepsEmpty(t *testing.T) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../config/crd/bases"},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest control plane: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	sch := runtime.NewScheme()
	require.NoError(t, scheme.AddToScheme(sch))
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(sch))
	c, err := client.New(cfg, client.Options{Scheme: sch})
	require.NoError(t, err)
	ctx := context.Background()

	branch := func(name string, grants []configbutleraiv1alpha3.GitBranchGitTargetGrant) *configbutleraiv1alpha3.GitBranch {
		b := &configbutleraiv1alpha3.GitBranch{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
			Spec:       configbutleraiv1alpha3.GitBranchSpec{Branch: "main", AllowedGitTargets: grants},
		}
		b.Spec.GitProviderRef.Name = "repo"
		return b
	}
	provider := func(name string) *configbutleraiv1alpha3.KubeProvider {
		return &configbutleraiv1alpha3.KubeProvider{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
			Spec: configbutleraiv1alpha3.KubeProviderSpec{
				InCluster: &configbutleraiv1alpha3.KubeProviderInCluster{},
			},
		}
	}
	relabel := func(t *testing.T, obj client.Object) {
		t.Helper()
		obj.SetLabels(map[string]string{"touched": "true"})
		require.NoError(t, c.Update(ctx, obj))
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), obj))
	}

	t.Run("an omitted GitBranch list reads back as the default", func(t *testing.T) {
		obj := branch("omitted", nil)
		require.NoError(t, c.Create(ctx, obj))
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), obj))
		assert.Equal(t, configbutleraiv1alpha3.DefaultGitBranchGitTargetGrants(), obj.Spec.AllowedGitTargets)
	})

	t.Run("an empty GitBranch list stays empty through an update", func(t *testing.T) {
		obj := branch("deny-all", []configbutleraiv1alpha3.GitBranchGitTargetGrant{})
		require.NoError(t, c.Create(ctx, obj))
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), obj))
		require.NotNil(t, obj.Spec.AllowedGitTargets)
		assert.Empty(t, obj.Spec.AllowedGitTargets)

		relabel(t, obj)
		require.NotNil(t, obj.Spec.AllowedGitTargets, "an update must not turn [] into the default")
		assert.Empty(t, obj.Spec.AllowedGitTargets)
	})

	t.Run("omitted KubeProvider lists read back as the defaults", func(t *testing.T) {
		obj := provider("omitted")
		require.NoError(t, c.Create(ctx, obj))
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), obj))
		assert.Equal(t, configbutleraiv1alpha3.DefaultKubeProviderGitTargetGrants(), obj.Spec.AllowedGitTargets)
		assert.Equal(t, configbutleraiv1alpha3.DefaultKubeProviderApplierClaimGrants(), obj.Spec.AllowedApplierClaims)
	})

	t.Run("empty KubeProvider lists stay empty through an update", func(t *testing.T) {
		obj := provider("deny-all")
		obj.Spec.AllowedGitTargets = []configbutleraiv1alpha3.KubeProviderGitTargetGrant{}
		obj.Spec.AllowedApplierClaims = []configbutleraiv1alpha3.KubeProviderApplierClaimGrant{}
		require.NoError(t, c.Create(ctx, obj))
		relabel(t, obj)
		require.NotNil(t, obj.Spec.AllowedGitTargets)
		assert.Empty(t, obj.Spec.AllowedGitTargets)
		require.NotNil(t, obj.Spec.AllowedApplierClaims)
		assert.Empty(t, obj.Spec.AllowedApplierClaims)
	})

	target := func(name string, grants []configbutleraiv1alpha3.GitTargetCommitRequestGrant) *configbutleraiv1alpha3.GitTarget {
		gt := &configbutleraiv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
			Spec:       configbutleraiv1alpha3.GitTargetSpec{Path: "apps", AllowedCommitRequests: grants},
		}
		gt.Spec.GitBranchRef.Name = "main"
		gt.Spec.KubeProviderRef.Name = "local"
		return gt
	}

	t.Run("an omitted GitTarget allowedCommitRequests reads back as the default", func(t *testing.T) {
		obj := target("omitted", nil)
		require.NoError(t, c.Create(ctx, obj))
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), obj))
		assert.Equal(t, configbutleraiv1alpha3.DefaultGitTargetCommitRequestGrants(), obj.Spec.AllowedCommitRequests)
	})

	t.Run("an empty GitTarget allowedCommitRequests stays empty through an update", func(t *testing.T) {
		obj := target("deny-all", []configbutleraiv1alpha3.GitTargetCommitRequestGrant{})
		require.NoError(t, c.Create(ctx, obj))
		relabel(t, obj)
		require.NotNil(t, obj.Spec.AllowedCommitRequests, "an update must not turn [] into the default")
		assert.Empty(t, obj.Spec.AllowedCommitRequests)
	})
}
