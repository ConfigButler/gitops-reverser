// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"strings"
	"testing"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
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

func resolverScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, configv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

// resolverProviderNS is the namespace the test KubeProvider "prod-eu-1" and its kubeconfig Secret
// live in. resolverOtherNS is a namespace that holds neither.
const (
	resolverProviderNS = "team-a"
	resolverOtherNS    = "gitops-reverser-system"
)

// resolverProviderKey is the test KubeProvider's namespace/name.
var resolverProviderKey = k8stypes.NamespacedName{Namespace: resolverProviderNS, Name: "prod-eu-1"}

// kubeProvider builds the remote KubeProvider team-a/prod-eu-1 whose kubeconfig Secret is "kc"
// under the given data key.
func kubeProvider(key string) *configv1alpha3.KubeProvider {
	return &configv1alpha3.KubeProvider{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: resolverProviderNS, Name: "prod-eu-1", UID: "uid-1", Generation: 3,
		},
		Spec: configv1alpha3.KubeProviderSpec{
			SecretRef: &meta.SecretKeyReference{Name: "kc", Key: key},
		},
	}
}

// inClusterKubeProvider builds a KubeProvider that asks for the operator's own identity.
func inClusterKubeProvider(namespace, name string, uid k8stypes.UID) *configv1alpha3.KubeProvider {
	return &configv1alpha3.KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: uid},
		Spec:       configv1alpha3.KubeProviderSpec{InCluster: &configv1alpha3.KubeProviderInCluster{}},
	}
}

// allowInCluster is a SafetyPolicy listing exactly the given KubeProviders as allowed to use
// spec.inCluster.
func allowInCluster(keys ...k8stypes.NamespacedName) kubeconfig.SafetyPolicy {
	allowed := map[k8stypes.NamespacedName]struct{}{}
	for _, key := range keys {
		allowed[key] = struct{}{}
	}
	return kubeconfig.SafetyPolicy{InClusterProviders: allowed}
}

// kubeconfigSecret is the Secret "kc" in the test provider's namespace, at resourceVersion 42.
func kubeconfigSecret(key, body string) *corev1.Secret {
	return kubeconfigSecretIn(resolverProviderNS, key, body)
}

func kubeconfigSecretIn(namespace, key, body string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "kc", ResourceVersion: "42"},
		Data:       map[string][]byte{key: []byte(body)},
	}
}

func newResolver(t *testing.T, safety kubeconfig.SafetyPolicy, objs ...client.Object) SourceClusterResolver {
	t.Helper()
	cl := fake.NewClientBuilder().WithScheme(resolverScheme(t)).WithObjects(objs...).Build()
	return NewSecretSourceClusterResolver(cl, safety)
}

func TestResolveSourceCluster_ValidAppliesThrottleAndVersion(t *testing.T) {
	r := newResolver(t, kubeconfig.SafetyPolicy{},
		kubeProvider("value"), kubeconfigSecret("value", resolverKubeConfig))

	cfg, version, err := r.ResolveSourceCluster(context.Background(), resolverProviderKey)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "https://192.0.2.1:6443", cfg.Host)
	assert.InDelta(t, 20.0, cfg.QPS, 0.001, "the default source-cluster QPS applies")
	assert.Equal(t, 30, cfg.Burst, "the default source-cluster burst applies")
	assert.Equal(t, "uid-1/3/42", version,
		"the provider UID + generation + Secret resourceVersion form the version token")
}

// TestResolveSourceCluster_VersionChangesWithTheProvidersUID: a provider deleted and recreated under
// the same name starts at generation 1 again, and its Secret may be untouched, so only the UID tells
// the two objects apart. Without it the refresh would keep the old object's clients.
func TestResolveSourceCluster_VersionChangesWithTheProvidersUID(t *testing.T) {
	original := kubeProvider("value")
	recreated := kubeProvider("value")
	recreated.UID = "uid-2"

	_, before, err := newResolver(t, kubeconfig.SafetyPolicy{},
		original, kubeconfigSecret("value", resolverKubeConfig)).
		ResolveSourceCluster(context.Background(), resolverProviderKey)
	require.NoError(t, err)
	_, after, err := newResolver(t, kubeconfig.SafetyPolicy{},
		recreated, kubeconfigSecret("value", resolverKubeConfig)).
		ResolveSourceCluster(context.Background(), resolverProviderKey)
	require.NoError(t, err)

	assert.NotEqual(t, before, after)
	assert.True(t, strings.HasPrefix(after, "uid-2/"), after)
}

func TestResolveSourceCluster_PerProviderThrottleOverride(t *testing.T) {
	qps := int32(5)
	burst := int32(7)
	provider := kubeProvider("value")
	provider.Spec.Client = &configv1alpha3.KubeProviderClient{QPS: &qps, Burst: &burst}
	r := newResolver(t, kubeconfig.SafetyPolicy{}, provider, kubeconfigSecret("value", resolverKubeConfig))

	cfg, _, err := r.ResolveSourceCluster(context.Background(), resolverProviderKey)
	require.NoError(t, err)
	assert.InDelta(t, 5.0, cfg.QPS, 0.001, "spec.client.qps overrides the default")
	assert.Equal(t, 7, cfg.Burst, "spec.client.burst overrides the default")
}

func TestResolveSourceCluster_KeyFallbackValueYaml(t *testing.T) {
	// Secret stores under value.yaml (Flux Kustomization shape); the provider's secretRef.key is empty.
	r := newResolver(t, kubeconfig.SafetyPolicy{},
		kubeProvider(""), kubeconfigSecret("value.yaml", resolverKubeConfig))

	cfg, _, err := r.ResolveSourceCluster(context.Background(), resolverProviderKey)
	require.NoError(t, err)
	assert.Equal(t, "https://192.0.2.1:6443", cfg.Host)
}

func TestResolveSourceCluster_MissingProviderSecretAndKey(t *testing.T) {
	// Absent KubeProvider -> error.
	r := newResolver(t, kubeconfig.SafetyPolicy{})
	absent := k8stypes.NamespacedName{Namespace: resolverProviderNS, Name: "absent"}
	_, _, err := r.ResolveSourceCluster(context.Background(), absent)
	require.Error(t, err, "an absent KubeProvider is an error, not a nil config")

	// Provider present, kubeconfig Secret absent -> error.
	r = newResolver(t, kubeconfig.SafetyPolicy{}, kubeProvider("value"))
	_, _, err = r.ResolveSourceCluster(context.Background(), resolverProviderKey)
	require.Error(t, err, "an absent Secret is an error, not a nil config")

	// Secret present but no kubeconfig under the resolved key -> typed KeyNotFound.
	r = newResolver(t, kubeconfig.SafetyPolicy{},
		kubeProvider("value"), kubeconfigSecret("elsewhere", resolverKubeConfig))
	_, _, err = r.ResolveSourceCluster(context.Background(), resolverProviderKey)
	require.Error(t, err)
	rej, ok := kubeconfig.AsRejection(err)
	require.True(t, ok)
	assert.Equal(t, kubeconfig.ReasonKeyNotFound, rej.Reason)
}

// TestResolveSourceCluster_ReadsTheSecretFromTheProvidersNamespace: the credential stays with the
// tenant that declared the provider. A Secret of the same name anywhere else, the operator's own
// namespace included, is never read, and its absence where it belongs is a definitive loss.
func TestResolveSourceCluster_ReadsTheSecretFromTheProvidersNamespace(t *testing.T) {
	r := newResolver(t, kubeconfig.SafetyPolicy{},
		kubeProvider("value"), kubeconfigSecretIn(resolverOtherNS, "value", resolverKubeConfig))
	_, _, err := r.ResolveSourceCluster(context.Background(), resolverProviderKey)
	require.Error(t, err, "a Secret only in another namespace is not the provider's")
	assert.True(t, apierrors.IsNotFound(err), "the Secret is NotFound in the provider's namespace: %v", err)
	assert.True(t, isDefinitiveCredentialFailure(err))

	r = newResolver(t, kubeconfig.SafetyPolicy{},
		kubeProvider("value"), kubeconfigSecretIn(resolverProviderNS, "value", resolverKubeConfig))
	cfg, _, err := r.ResolveSourceCluster(context.Background(), resolverProviderKey)
	require.NoError(t, err)
	assert.Equal(t, "https://192.0.2.1:6443", cfg.Host)
}

// TestResolveSourceCluster_InClusterNeedsTheInstallationsConsent: spec.inCluster is the operator's
// own identity, so a provider gets it only while --in-cluster-kube-providers lists it exactly. An
// unlisted one is a typed rejection, never a silent fall through to that identity, and a refresh
// treats it as a definitive loss so cached clients are dropped.
func TestResolveSourceCluster_InClusterNeedsTheInstallationsConsent(t *testing.T) {
	local := k8stypes.NamespacedName{Namespace: "platform", Name: "local"}
	provider := inClusterKubeProvider(local.Namespace, local.Name, "uid-local")

	tests := []struct {
		name   string
		safety kubeconfig.SafetyPolicy
	}{
		{name: "the allow-list is empty", safety: kubeconfig.SafetyPolicy{}},
		{
			name:   "the same name is listed in another namespace",
			safety: allowInCluster(k8stypes.NamespacedName{Namespace: "team-a", Name: "local"}),
		},
		{
			name:   "another name is listed in the same namespace",
			safety: allowInCluster(k8stypes.NamespacedName{Namespace: "platform", Name: "other"}),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, version, err := newResolver(t, tc.safety, provider).
				ResolveSourceCluster(context.Background(), local)
			require.Error(t, err)
			assert.Nil(t, cfg)
			assert.Empty(t, version)
			rej, ok := kubeconfig.AsRejection(err)
			require.True(t, ok, "an unlisted inCluster provider is a typed rejection: %v", err)
			assert.Equal(t, kubeconfig.ReasonInClusterNotAuthorized, rej.Reason)
			assert.True(t, isDefinitiveCredentialFailure(err), "retrying cannot make an unlisted provider listed")
		})
	}

	t.Run("listed", func(t *testing.T) {
		cfg, version, err := newResolver(t, allowInCluster(local), provider).
			ResolveSourceCluster(context.Background(), local)
		require.NoError(t, err)
		assert.Nil(t, cfg, "a nil config is the in-cluster answer")
		assert.Equal(t, inClusterConfigVersion+"/uid-local", version, "the provider's UID is the whole version")
	})
}

// TestResolveSourceCluster_NeitherArmIsRefused: an unvalidated provider that sets neither
// spec.secretRef nor spec.inCluster must never fall through to the operator's own identity.
func TestResolveSourceCluster_NeitherArmIsRefused(t *testing.T) {
	provider := kubeProvider("value")
	provider.Spec.SecretRef = nil
	r := newResolver(t, allowInCluster(resolverProviderKey), provider)

	cfg, _, err := r.ResolveSourceCluster(context.Background(), resolverProviderKey)
	require.Error(t, err)
	assert.Nil(t, cfg)
	rej, ok := kubeconfig.AsRejection(err)
	require.True(t, ok)
	assert.Equal(t, kubeconfig.ReasonInvalid, rej.Reason)
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
		kubeProvider("value"), kubeconfigSecret("value", resolverExecKubeConfig))
	_, _, err := r.ResolveSourceCluster(context.Background(), resolverProviderKey)
	require.Error(t, err)
	rej, ok := kubeconfig.AsRejection(err)
	require.True(t, ok)
	assert.Equal(t, kubeconfig.ReasonExecNotAllowed, rej.Reason)

	// Opting in (a deliberate trust decision) lets it through.
	r = newResolver(t, kubeconfig.SafetyPolicy{AllowExec: true},
		kubeProvider("value"), kubeconfigSecret("value", resolverExecKubeConfig))
	_, _, err = r.ResolveSourceCluster(context.Background(), resolverProviderKey)
	require.NoError(t, err)
}
