// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ConfigButler/gitops-reverser/internal/kubeconfig"
)

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

func TestParseSourceClusterID(t *testing.T) {
	ref, err := parseSourceClusterID("team-a/kc/value")
	require.NoError(t, err)
	assert.Equal(t, sourceClusterRef{Namespace: "team-a", Name: "kc", Key: "value"}, ref)

	// An empty key segment is valid — an omitted spec key is its own identity.
	ref, err = parseSourceClusterID("team-a/kc/")
	require.NoError(t, err)
	assert.Equal(t, sourceClusterRef{Namespace: "team-a", Name: "kc", Key: ""}, ref)

	for _, bad := range []string{"", "onlyone", "ns/name", "/name/key", "ns//key"} {
		_, err := parseSourceClusterID(bad)
		assert.Error(t, err, "malformed id %q must error", bad)
	}
}

func newResolver(t *testing.T, secret *corev1.Secret, safety kubeconfig.SafetyPolicy) SourceClusterResolver {
	t.Helper()
	builder := fake.NewClientBuilder()
	if secret != nil {
		builder = builder.WithObjects(secret)
	}
	return NewSecretSourceClusterResolver(builder.Build(), safety, 20, 30)
}

func kubeconfigSecret(key, body string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "kc"},
		Data:       map[string][]byte{key: []byte(body)},
	}
}

func TestResolveSourceCluster_ValidAppliesThrottleAndVersion(t *testing.T) {
	secret := kubeconfigSecret("value", resolverKubeConfig)
	r := newResolver(t, secret, kubeconfig.SafetyPolicy{})

	cfg, version, err := r.ResolveSourceCluster(context.Background(), "team-a/kc/value")
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "https://192.0.2.1:6443", cfg.Host)
	assert.InDelta(t, 20.0, cfg.QPS, 0.001, "source-cluster QPS applied")
	assert.Equal(t, 30, cfg.Burst, "source-cluster burst applied")
	assert.NotEmpty(t, version, "the Secret resourceVersion is the version token")
}

func TestResolveSourceCluster_KeyFallbackValueYaml(t *testing.T) {
	// Secret stores under value.yaml (Flux Kustomization shape); id has an empty key segment.
	secret := kubeconfigSecret("value.yaml", resolverKubeConfig)
	r := newResolver(t, secret, kubeconfig.SafetyPolicy{})

	cfg, _, err := r.ResolveSourceCluster(context.Background(), "team-a/kc/")
	require.NoError(t, err)
	assert.Equal(t, "https://192.0.2.1:6443", cfg.Host)
}

func TestResolveSourceCluster_MissingSecretAndKey(t *testing.T) {
	r := newResolver(t, nil, kubeconfig.SafetyPolicy{})
	_, _, err := r.ResolveSourceCluster(context.Background(), "team-a/absent/value")
	require.Error(t, err, "an absent Secret is an error, not a nil config")

	// Secret present but no kubeconfig under the resolved key -> typed KeyNotFound.
	secret := kubeconfigSecret("elsewhere", resolverKubeConfig)
	r = newResolver(t, secret, kubeconfig.SafetyPolicy{})
	_, _, err = r.ResolveSourceCluster(context.Background(), "team-a/kc/value")
	require.Error(t, err)
	rej, ok := kubeconfig.AsRejection(err)
	require.True(t, ok)
	assert.Equal(t, kubeconfig.ReasonKeyNotFound, rej.Reason)
}

func TestResolveSourceCluster_RejectsUnsafe(t *testing.T) {
	secret := kubeconfigSecret("value", resolverExecKubeConfig)
	r := newResolver(t, secret, kubeconfig.SafetyPolicy{})
	_, _, err := r.ResolveSourceCluster(context.Background(), "team-a/kc/value")
	require.Error(t, err)
	rej, ok := kubeconfig.AsRejection(err)
	require.True(t, ok)
	assert.Equal(t, kubeconfig.ReasonExecNotAllowed, rej.Reason)

	// Opting in (a deliberate trust decision) lets it through.
	r = newResolver(t, secret, kubeconfig.SafetyPolicy{AllowExec: true})
	_, _, err = r.ResolveSourceCluster(context.Background(), "team-a/kc/value")
	require.NoError(t, err)
}
