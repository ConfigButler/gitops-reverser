// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const validKubeConfig = `apiVersion: v1
kind: Config
clusters:
- name: acme
  cluster:
    server: https://acme.example:6443
contexts:
- name: acme
  context:
    cluster: acme
    user: acme
current-context: acme
users:
- name: acme
  user:
    token: abc123
`

func kubeConfigSecret(key, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-kubeconfig", Namespace: "team-a", ResourceVersion: "42"},
		Data:       map[string][]byte{key: []byte(value)},
	}
}

func newResolver(t *testing.T, objects ...runtime.Object) SourceClusterResolver {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()
	return NewSecretSourceClusterResolver(c, 20, 30)
}

func TestParseSourceClusterID(t *testing.T) {
	t.Parallel()

	ref, err := parseSourceClusterID("team-a/acme-kubeconfig/value.yaml")
	require.NoError(t, err)
	assert.Equal(t, sourceClusterRef{Namespace: "team-a", Name: "acme-kubeconfig", Key: "value.yaml"}, ref)

	// A data key may itself contain slashes; only the first two segments are structural.
	ref, err = parseSourceClusterID("team-a/acme/nested/key.yaml")
	require.NoError(t, err)
	assert.Equal(t, "nested/key.yaml", ref.Key)

	for _, bad := range []string{"", "team-a", "team-a/acme", "/acme/key", "team-a//key", "team-a/acme/"} {
		_, err := parseSourceClusterID(bad)
		require.Error(t, err, "id %q must be rejected", bad)
	}
}

func TestSecretSourceClusterResolver_ResolvesAndVersions(t *testing.T) {
	t.Parallel()

	r := newResolver(t, kubeConfigSecret("value.yaml", validKubeConfig))
	cfg, version, err := r.ResolveSourceCluster(context.Background(), "team-a/acme-kubeconfig/value.yaml")

	require.NoError(t, err)
	assert.Equal(t, "https://acme.example:6443", cfg.Host)
	assert.Equal(t, "42", version, "the Secret's resourceVersion is the rotation token")
	assert.InDelta(t, 20.0, float64(cfg.QPS), 0.001, "a remote cluster gets client-side throttling")
	assert.Equal(t, 30, cfg.Burst)
}

func TestSecretSourceClusterResolver_MissingSecret(t *testing.T) {
	t.Parallel()

	r := newResolver(t)
	_, _, err := r.ResolveSourceCluster(context.Background(), "team-a/acme-kubeconfig/value.yaml")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "read kubeconfig Secret team-a/acme-kubeconfig")
}

func TestSecretSourceClusterResolver_MissingKey(t *testing.T) {
	t.Parallel()

	r := newResolver(t, kubeConfigSecret("other.yaml", validKubeConfig))
	_, _, err := r.ResolveSourceCluster(context.Background(), "team-a/acme-kubeconfig/value.yaml")

	require.Error(t, err)
	assert.Contains(t, err.Error(), `no data under key "value.yaml"`)
}

func TestSecretSourceClusterResolver_UnparseableKubeConfig(t *testing.T) {
	t.Parallel()

	r := newResolver(t, kubeConfigSecret("value.yaml", "this is not a kubeconfig"))
	_, _, err := r.ResolveSourceCluster(context.Background(), "team-a/acme-kubeconfig/value.yaml")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse kubeconfig")
}

func TestSecretSourceClusterResolver_MalformedID(t *testing.T) {
	t.Parallel()

	r := newResolver(t)
	_, _, err := r.ResolveSourceCluster(context.Background(), "not-an-id")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed source cluster id")
}
