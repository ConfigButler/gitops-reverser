// SPDX-License-Identifier: Apache-2.0

package kubeconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
