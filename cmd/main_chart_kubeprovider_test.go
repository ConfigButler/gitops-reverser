// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"os/exec"
	"testing"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
)

// kubeProviderChartInputs are read in-process so `go test` treats them as cache inputs.
var kubeProviderChartInputs = []string{
	"../charts/gitops-reverser/values.yaml",
	"../charts/gitops-reverser/values-playground.yaml",
	"../charts/gitops-reverser/values.schema.json",
	"../charts/gitops-reverser/templates/kubeprovider.yaml",
}

// renderLocalKubeProvider renders the chart's `local` KubeProvider in namespace gitops-reverser.
func renderLocalKubeProvider(t *testing.T, args ...string) *configbutleraiv1alpha3.KubeProvider {
	t.Helper()
	for _, path := range kubeProviderChartInputs {
		_, err := os.ReadFile(path)
		require.NoError(t, err)
	}
	command := append([]string{"template", "gitops-reverser", "../charts/gitops-reverser",
		"--namespace", "gitops-reverser", "--show-only", "templates/kubeprovider.yaml"}, args...)
	out, err := exec.Command("helm", command...).CombinedOutput()
	require.NoErrorf(t, err, "helm template failed: %s", out)
	provider := &configbutleraiv1alpha3.KubeProvider{}
	require.NoError(t, yaml.UnmarshalStrict(out, provider))
	return provider
}

// mirrors reports whether a GitTarget in namespace may mirror through provider, and the refusal.
func mirrors(t *testing.T, provider *configbutleraiv1alpha3.KubeProvider, namespace string) authz.Decision {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(provider).Build()
	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "apps"},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			KubeProviderRef: meta.NamespacedObjectReference{Name: provider.Name, Namespace: provider.Namespace},
		},
	}
	decision, err := authz.GitTargetAdmitted(context.Background(), reader, target)
	require.NoError(t, err)
	return decision
}

// TestChartKubeProvider_ADefaultInstallGrantsTheReleaseNamespace: unset values leave both grant
// lists out, so the CRD defaults them to the release namespace's own objects and nobody else.
func TestChartKubeProvider_ADefaultInstallGrantsTheReleaseNamespace(t *testing.T) {
	provider := renderLocalKubeProvider(t)
	assert.Nil(t, provider.Spec.AllowedGitTargets, "omitted, so the API server applies the CRD default")
	assert.Nil(t, provider.Spec.AllowedApplierClaims, "omitted, so the API server applies the CRD default")
	assert.True(t, mirrors(t, provider, "gitops-reverser").Allowed)
	decision := mirrors(t, provider, "team-a")
	assert.False(t, decision.Allowed)
	assert.Contains(t, decision.Message, "KubeProvider gitops-reverser/local admits gitops-reverser")
}

// TestChartKubeProvider_AnEmptyListRendersAsEmpty: [] must reach the API server as [], or the CRD
// default would turn an explicit deny-all back into a grant.
func TestChartKubeProvider_AnEmptyListRendersAsEmpty(t *testing.T) {
	provider := renderLocalKubeProvider(t, "--set-json", "kubeProvider.allowedGitTargets=[]",
		"--set-json", "kubeProvider.allowedApplierClaims=[]")
	assert.NotNil(t, provider.Spec.AllowedGitTargets)
	assert.Empty(t, provider.Spec.AllowedGitTargets)
	assert.NotNil(t, provider.Spec.AllowedApplierClaims)
	assert.Empty(t, provider.Spec.AllowedApplierClaims)
	for _, namespace := range []string{"gitops-reverser", "team-a"} {
		decision := mirrors(t, provider, namespace)
		assert.False(t, decision.Allowed)
		assert.Contains(t, decision.Message,
			"KubeProvider gitops-reverser/local admits no namespace in spec.allowedGitTargets")
	}
}

// TestChartKubeProvider_TheQuickstartGrantsItsNamespaceOnly: quickstart.enabled renders a grant for
// exactly the quickstart namespace, to mirror and to hold appliers in its own namespace.
func TestChartKubeProvider_TheQuickstartGrantsItsNamespaceOnly(t *testing.T) {
	provider := renderLocalKubeProvider(t, "--set", "quickstart.enabled=true",
		"--set", "quickstart.gitProvider.url=https://example.com/repo.git")

	scope, err := provider.AdmitsGitTarget(
		types.NamespacedName{Namespace: "gitops-reverser-quickstart-demo", Name: "mirror"},
		nil,
	)
	require.NoError(t, err)
	assert.True(t, scope.AllowsSourceNamespace("gitops-reverser-quickstart-demo"))
	assert.False(t, scope.AllowsSourceNamespace("kube-system"))
	assert.Empty(t, scope.ClusterResources)
	appliers, err := provider.AdmitsApplierClaim(
		types.NamespacedName{Namespace: "gitops-reverser-quickstart-demo", Name: "checkout"},
		nil,
	)
	require.NoError(t, err)
	assert.True(t, appliers.AllowsApplierNamespace("gitops-reverser-quickstart-demo"))
	assert.False(t, appliers.AllowsApplierNamespace("flux-system"))

	assert.True(t, mirrors(t, provider, "gitops-reverser-quickstart-demo").Allowed)
	assert.False(t, mirrors(t, provider, "team-a").Allowed, "and nothing else")
}

// TestChartKubeProvider_ThePlaygroundGrantsEveryNamespaceItsOwn: values-playground.yaml is the open
// default, chosen visibly: every namespace mirrors its own namespace, no other, and no
// cluster-scoped resource.
func TestChartKubeProvider_ThePlaygroundGrantsEveryNamespaceItsOwn(t *testing.T) {
	provider := renderLocalKubeProvider(t, "-f", "../charts/gitops-reverser/values-playground.yaml")
	for _, namespace := range []string{"team-a", "gitops-reverser"} {
		scope, err := provider.AdmitsGitTarget(types.NamespacedName{Namespace: namespace, Name: "mirror"}, nil)
		require.NoError(t, err)
		assert.True(t, scope.AllowsSourceNamespace(namespace))
		assert.False(t, scope.AllowsSourceNamespace("kube-system"), "no other namespace")
		assert.False(t, scope.AllowsSourceNamespace("*"))
		assert.Empty(t, scope.ClusterResources, "no default grants clusterResources")
		appliers, err := provider.AdmitsApplierClaim(types.NamespacedName{Namespace: namespace, Name: "checkout"}, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{namespace}, appliers.ApplierNamespaces)
	}
}

// TestChartKubeProvider_GrantsRenderAsWritten: values grants reach the provider unchanged, and the
// quickstart grant is added beside them.
func TestChartKubeProvider_GrantsRenderAsWritten(t *testing.T) {
	values := t.TempDir() + "/values.yaml"
	require.NoError(t, os.WriteFile(values, []byte(`kubeProvider:
  allowedGitTargets:
    - consumerNamespaces:
        from: Selector
        selector: {matchLabels: {platform.example.com/tenant: "true"}}
      sourceNamespaces: ["{consumer.namespace}"]
  allowedApplierClaims:
    - consumerNamespaces:
        from: Names
        names: [team-a]
      applierNamespaces: [flux-system]
quickstart:
  enabled: true
  gitProvider: {url: "https://example.com/repo.git"}
`), 0o600))
	provider := renderLocalKubeProvider(t, "-f", values)
	require.Len(t, provider.Spec.AllowedGitTargets, 2)
	assert.Equal(
		t,
		configbutleraiv1alpha3.GrantFromSelector,
		provider.Spec.AllowedGitTargets[0].ConsumerNamespaces.From,
	)
	assert.Equal(t, configbutleraiv1alpha3.GrantFromNames, provider.Spec.AllowedGitTargets[1].ConsumerNamespaces.From)
	require.Len(t, provider.Spec.AllowedApplierClaims, 2)
	assert.Equal(t, []configbutleraiv1alpha3.ScopeNamespace{"flux-system"},
		provider.Spec.AllowedApplierClaims[0].ApplierNamespaces)
}
