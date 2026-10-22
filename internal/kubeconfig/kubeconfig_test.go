// SPDX-License-Identifier: Apache-2.0

package kubeconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

const validKubeConfig = `apiVersion: v1
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

const execKubeConfig = `apiVersion: v1
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
`

const insecureKubeConfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://192.0.2.1:6443
    insecure-skip-tls-verify: true
contexts:
- name: c
  context: {cluster: c, user: u}
current-context: c
users:
- name: u
  user:
    token: dummy-token
`

// tokenFileKubeConfig names a token by file path — client-go would read it from the operator
// Pod's filesystem. The CA is embedded so ONLY the file reference is the fault under test.
const tokenFileKubeConfig = `apiVersion: v1
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
    tokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
`

// caFileKubeConfig names the cluster CA by file path — another in-Pod read vector.
const caFileKubeConfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://192.0.2.1:6443
    certificate-authority: /etc/ssl/certs/ca.crt
contexts:
- name: c
  context: {cluster: c, user: u}
current-context: c
users:
- name: u
  user:
    token: dummy-token
`

func TestResolveKey_ExplicitKeyWins(t *testing.T) {
	data := map[string][]byte{"value": []byte("a"), "custom": []byte("b")}
	raw, used, ok := ResolveKey(data, "custom")
	require.True(t, ok)
	assert.Equal(t, "custom", used)
	assert.Equal(t, "b", string(raw))
}

func TestResolveKey_FallsBackValueThenValueYaml(t *testing.T) {
	// value wins over value.yaml.
	raw, used, ok := ResolveKey(map[string][]byte{"value": []byte("a"), "value.yaml": []byte("b")}, "")
	require.True(t, ok)
	assert.Equal(t, "value", used)
	assert.Equal(t, "a", string(raw))

	// value absent -> value.yaml (Flux's Kustomization Secret shape).
	raw, used, ok = ResolveKey(map[string][]byte{"value.yaml": []byte("b")}, "")
	require.True(t, ok)
	assert.Equal(t, "value.yaml", used)
	assert.Equal(t, "b", string(raw))
}

func TestResolveKey_MissingAndEmptyAreNotOK(t *testing.T) {
	_, _, ok := ResolveKey(map[string][]byte{"elsewhere": []byte("x")}, "value")
	assert.False(t, ok, "explicit key absent")

	_, _, ok = ResolveKey(map[string][]byte{"value": {}}, "")
	assert.False(t, ok, "empty value is treated as absent")

	_, _, ok = ResolveKey(nil, "")
	assert.False(t, ok, "no data")
}

func TestCheck_RejectsUnsafeByDefault(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantReason string
	}{
		{"garbage", "this is not a kubeconfig", ReasonInvalid},
		{"exec", execKubeConfig, ReasonExecNotAllowed},
		{"insecureTLS", insecureKubeConfig, ReasonInsecureTLSNotAllowed},
		{"tokenFile", tokenFileKubeConfig, ReasonFileReferenceNotAllowed},
		{"caFile", caFileKubeConfig, ReasonFileReferenceNotAllowed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rej := Check([]byte(tc.raw), SafetyPolicy{})
			require.NotNil(t, rej, "must reject")
			assert.Equal(t, tc.wantReason, rej.Reason)
			assert.NotEmpty(t, rej.Error())
		})
	}
}

func TestCheck_AllowsSafeAndOptedIn(t *testing.T) {
	assert.Nil(t, Check([]byte(validKubeConfig), SafetyPolicy{}), "a token kubeconfig is safe")
	assert.Nil(t, Check([]byte(execKubeConfig), SafetyPolicy{AllowExec: true}), "exec opted in")
	assert.Nil(t, Check([]byte(insecureKubeConfig), SafetyPolicy{AllowInsecureTLS: true}), "insecure TLS opted in")
}

// A file-backed reference is a pod-filesystem read vector with no legitimate use for a remote
// kubeconfig, so it is rejected UNCONDITIONALLY — even with every safety opt-in enabled.
func TestCheck_FileReferencesRejectedEvenWhenOptedIn(t *testing.T) {
	all := SafetyPolicy{AllowExec: true, AllowInsecureTLS: true}
	for name, raw := range map[string]string{"tokenFile": tokenFileKubeConfig, "caFile": caFileKubeConfig} {
		rej := Check([]byte(raw), all)
		require.NotNil(t, rej, "%s must be rejected regardless of policy", name)
		assert.Equal(t, ReasonFileReferenceNotAllowed, rej.Reason)
	}
}

func TestBuildRESTConfig(t *testing.T) {
	cfg, err := BuildRESTConfig([]byte(validKubeConfig), SafetyPolicy{})
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "https://192.0.2.1:6443", cfg.Host)

	// A rejected kubeconfig surfaces the typed Rejection through the error chain.
	_, err = BuildRESTConfig([]byte(execKubeConfig), SafetyPolicy{})
	require.Error(t, err)
	rej, ok := AsRejection(err)
	require.True(t, ok)
	assert.Equal(t, ReasonExecNotAllowed, rej.Reason)
}

func TestParseInClusterProviders(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    []types.NamespacedName
		wantErr string
	}{
		{name: "empty lists none", value: ""},
		{name: "blank entries are ignored", value: " , ,"},
		{
			name:  "exact entries, trimmed",
			value: "gitops-reverser/local, platform/ops",
			want: []types.NamespacedName{
				{Namespace: "gitops-reverser", Name: "local"}, {Namespace: "platform", Name: "ops"},
			},
		},
		{name: "a bare name is refused", value: "local", wantErr: `"local" is not namespace/name`},
		{name: "an empty namespace is refused", value: "/local", wantErr: `"/local"`},
		{name: "an empty name is refused", value: "platform/", wantErr: `"platform/"`},
		{name: "an extra segment is refused", value: "a/b/c", wantErr: `"a/b/c"`},
		{name: "a wildcard is refused", value: "platform/*", wantErr: `"platform/*"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseInClusterProviders(tt.value)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Len(t, got, len(tt.want))
			for _, key := range tt.want {
				assert.Contains(t, got, key)
			}
		})
	}
}

func TestCheckInCluster(t *testing.T) {
	local := types.NamespacedName{Namespace: "gitops-reverser", Name: "local"}
	other := types.NamespacedName{Namespace: "tenant-a", Name: "local"}

	rej := CheckInCluster(local, SafetyPolicy{})
	require.NotNil(t, rej, "an empty allow-list allows nobody")
	assert.Equal(t, ReasonInClusterNotAuthorized, rej.Reason)
	assert.Contains(t, rej.Message, "lists none")

	policy := SafetyPolicy{InClusterProviders: map[types.NamespacedName]struct{}{local: {}}}
	assert.Nil(t, CheckInCluster(local, policy))
	assert.True(t, policy.AllowsInCluster(local))

	rej = CheckInCluster(other, policy)
	require.NotNil(t, rej, "the same name in another namespace is another provider")
	assert.Contains(t, rej.Message, "tenant-a/local")
	assert.Contains(t, rej.Message, "lists only gitops-reverser/local")

	_, isRejection := AsRejection(rej)
	assert.True(t, isRejection)
}
