// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/kubeconfig"
)

const resolverOperatorNS = "gitops-reverser-system"

const resolverKubeConfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://192.0.2.1:6443
    certificate-authority-data: dGVzdA==
contexts:
- name: c
  context: {cluster: c, user: u}
current-context: c
users:
- name: u
  user:
    token: dummy-token
`

const resolverExecKubeConfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: https://192.0.2.1:6443}
contexts:
- name: c
  context: {cluster: c, user: u}
current-context: c
users:
- name: u
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: /bin/echo
      interactiveMode: Never
`

func resolverScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, configv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

// clusterProvider builds the remote ClusterProvider "prod-eu-1" whose kubeconfig Secret is "kc"
// under the given data key.
func clusterProvider(key string) *configv1alpha3.ClusterProvider {
	return &configv1alpha3.ClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-eu-1"},
		Spec: configv1alpha3.ClusterProviderSpec{
			KubeConfig: &meta.KubeConfigReference{SecretRef: &meta.SecretKeyReference{Name: "kc", Key: key}},
		},
	}
}

func kubeconfigSecret(key, body string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: resolverOperatorNS, Name: "kc"},
		Data:       map[string][]byte{key: []byte(body)},
	}
}

func newResolver(t *testing.T, safety kubeconfig.SafetyPolicy, objs ...client.Object) SourceClusterResolver {
	t.Helper()
	cl := fake.NewClientBuilder().WithScheme(resolverScheme(t)).WithObjects(objs...).Build()
	return NewSecretSourceClusterResolver(cl, resolverOperatorNS, safety, 20, 30)
}

func TestResolveSourceCluster_ValidAppliesThrottleAndVersion(t *testing.T) {
	r := newResolver(t, kubeconfig.SafetyPolicy{},
		clusterProvider("value"), kubeconfigSecret("value", resolverKubeConfig))

	cfg, version, err := r.ResolveSourceCluster(context.Background(), "prod-eu-1")
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "https://192.0.2.1:6443", cfg.Host)
	assert.InDelta(t, 20.0, cfg.QPS, 0.001, "global source-cluster QPS applied")
	assert.Equal(t, 30, cfg.Burst, "global source-cluster burst applied")
	assert.NotEmpty(t, version, "the provider generation + Secret resourceVersion form the version token")
}

func TestResolveSourceCluster_PerProviderThrottleOverride(t *testing.T) {
	qps := int32(5)
	burst := int32(7)
	provider := clusterProvider("value")
	provider.Spec.QPS = &qps
	provider.Spec.Burst = &burst
	r := newResolver(t, kubeconfig.SafetyPolicy{}, provider, kubeconfigSecret("value", resolverKubeConfig))

	cfg, _, err := r.ResolveSourceCluster(context.Background(), "prod-eu-1")
	require.NoError(t, err)
	assert.InDelta(t, 5.0, cfg.QPS, 0.001, "per-provider QPS overrides the global default")
	assert.Equal(t, 7, cfg.Burst, "per-provider burst overrides the global default")
}

func TestResolveSourceCluster_KeyFallbackValueYaml(t *testing.T) {
	// Secret stores under value.yaml (Flux Kustomization shape); the provider's secretRef.key is empty.
	r := newResolver(t, kubeconfig.SafetyPolicy{},
		clusterProvider(""), kubeconfigSecret("value.yaml", resolverKubeConfig))

	cfg, _, err := r.ResolveSourceCluster(context.Background(), "prod-eu-1")
	require.NoError(t, err)
	assert.Equal(t, "https://192.0.2.1:6443", cfg.Host)
}

func TestResolveSourceCluster_MissingProviderSecretAndKey(t *testing.T) {
	// Absent ClusterProvider -> error.
	r := newResolver(t, kubeconfig.SafetyPolicy{})
	_, _, err := r.ResolveSourceCluster(context.Background(), "absent")
	require.Error(t, err, "an absent ClusterProvider is an error, not a nil config")

	// Provider present, kubeconfig Secret absent -> error.
	r = newResolver(t, kubeconfig.SafetyPolicy{}, clusterProvider("value"))
	_, _, err = r.ResolveSourceCluster(context.Background(), "prod-eu-1")
	require.Error(t, err, "an absent Secret is an error, not a nil config")

	// Secret present but no kubeconfig under the resolved key -> typed KeyNotFound.
	r = newResolver(t, kubeconfig.SafetyPolicy{},
		clusterProvider("value"), kubeconfigSecret("elsewhere", resolverKubeConfig))
	_, _, err = r.ResolveSourceCluster(context.Background(), "prod-eu-1")
	require.Error(t, err)
	rej, ok := kubeconfig.AsRejection(err)
	require.True(t, ok)
	assert.Equal(t, kubeconfig.ReasonKeyNotFound, rej.Reason)
}

// TestDescribeKey covers the hint a "key not found" error carries. An omitted secretRef.key is
// not "no key": the resolver tried both fallbacks, and the message must say so or the human is
// told to look for a key they never wrote.
func TestDescribeKey(t *testing.T) {
	tests := []struct {
		name    string
		specKey string
		want    string
	}{
		{"omitted key names both fallbacks", "", "value or value.yaml"},
		{"explicit key is reported verbatim", "kubeconfig", "kubeconfig"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, describeKey(tc.specKey))
		})
	}
}

func TestResolveSourceCluster_RejectsUnsafe(t *testing.T) {
	r := newResolver(t, kubeconfig.SafetyPolicy{},
		clusterProvider("value"), kubeconfigSecret("value", resolverExecKubeConfig))
	_, _, err := r.ResolveSourceCluster(context.Background(), "prod-eu-1")
	require.Error(t, err)
	rej, ok := kubeconfig.AsRejection(err)
	require.True(t, ok)
	assert.Equal(t, kubeconfig.ReasonExecNotAllowed, rej.Reason)

	// Opting in (a deliberate trust decision) lets it through.
	r = newResolver(t, kubeconfig.SafetyPolicy{AllowExec: true},
		clusterProvider("value"), kubeconfigSecret("value", resolverExecKubeConfig))
	_, _, err = r.ResolveSourceCluster(context.Background(), "prod-eu-1")
	require.NoError(t, err)
}
