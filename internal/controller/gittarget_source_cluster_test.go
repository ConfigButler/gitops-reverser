// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/kubeconfig"
)

const scValidKubeConfig = `apiVersion: v1
kind: Config
clusters:
- {name: c, cluster: {server: https://192.0.2.1:6443, certificate-authority-data: dGVzdA==}}
contexts:
- {name: c, context: {cluster: c, user: u}}
current-context: c
users:
- {name: u, user: {token: t}}
`

const scExecKubeConfig = `apiVersion: v1
kind: Config
clusters:
- {name: c, cluster: {server: https://192.0.2.1:6443}}
contexts:
- {name: c, context: {cluster: c, user: u}}
current-context: c
users:
- name: u
  user:
    exec: {apiVersion: client.authentication.k8s.io/v1, command: /bin/echo, interactiveMode: Never}
`

const scInsecureKubeConfig = `apiVersion: v1
kind: Config
clusters:
- {name: c, cluster: {server: https://192.0.2.1:6443, insecure-skip-tls-verify: true}}
contexts:
- {name: c, context: {cluster: c, user: u}}
current-context: c
users:
- {name: u, user: {token: t}}
`

func scScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

func gitTargetWithKubeConfig(secretName, key string) *configbutleraiv1alpha3.GitTarget {
	t := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "gt", Namespace: "team-a"},
	}
	if secretName != "" {
		t.Spec.KubeConfig = &meta.KubeConfigReference{
			SecretRef: &meta.SecretKeyReference{Name: secretName, Key: key},
		}
	}
	return t
}

func TestValidateKubeConfig(t *testing.T) {
	tests := []struct {
		name       string
		secretName string
		key        string
		secretData map[string][]byte
		safety     kubeconfig.SafetyPolicy
		wantOK     bool
		wantReason string
	}{
		{name: "omitted is valid (local cluster)", secretName: "", wantOK: true},
		{name: "missing Secret", secretName: "absent", wantOK: false, wantReason: kubeconfig.ReasonSecretNotFound},
		{
			name: "missing key", secretName: "kc", key: "value",
			secretData: map[string][]byte{"elsewhere": []byte(scValidKubeConfig)},
			wantOK:     false, wantReason: kubeconfig.ReasonKeyNotFound,
		},
		{
			name: "unparseable", secretName: "kc",
			secretData: map[string][]byte{"value": []byte("not a kubeconfig")},
			wantOK:     false, wantReason: kubeconfig.ReasonInvalid,
		},
		{
			name: "exec rejected", secretName: "kc",
			secretData: map[string][]byte{"value": []byte(scExecKubeConfig)},
			wantOK:     false, wantReason: kubeconfig.ReasonExecNotAllowed,
		},
		{
			name: "insecure TLS rejected", secretName: "kc",
			secretData: map[string][]byte{"value": []byte(scInsecureKubeConfig)},
			wantOK:     false, wantReason: kubeconfig.ReasonInsecureTLSNotAllowed,
		},
		{
			name: "valid via value.yaml fallback", secretName: "kc",
			secretData: map[string][]byte{"value.yaml": []byte(scValidKubeConfig)},
			wantOK:     true,
		},
		{
			name: "exec allowed when opted in", secretName: "kc",
			secretData: map[string][]byte{"value": []byte(scExecKubeConfig)},
			safety:     kubeconfig.SafetyPolicy{AllowExec: true},
			wantOK:     true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scScheme(t))
			if tc.secretData != nil {
				builder = builder.WithObjects(&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "kc"},
					Data:       tc.secretData,
				})
			}
			r := &GitTargetReconciler{Client: builder.Build(), KubeConfigSafety: tc.safety}
			ok, reason, msg, err := r.validateKubeConfig(
				context.Background(),
				gitTargetWithKubeConfig(tc.secretName, tc.key),
			)
			require.NoError(t, err)
			assert.Equal(t, tc.wantOK, ok)
			if !tc.wantOK {
				assert.Equal(t, tc.wantReason, reason)
				assert.NotEmpty(t, msg)
			}
		})
	}
}

func TestGitProviderReadiness(t *testing.T) {
	provider := func(conds []metav1.Condition) *configbutleraiv1alpha3.GitProvider {
		return &configbutleraiv1alpha3.GitProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "prov", Namespace: "team-a"},
			Status:     configbutleraiv1alpha3.GitProviderStatus{Conditions: conds},
		}
	}
	ready := metav1.Condition{Type: ConditionTypeReady, Status: metav1.ConditionTrue, Reason: "OK"}
	notReady := metav1.Condition{
		Type:    ConditionTypeReady,
		Status:  metav1.ConditionFalse,
		Reason:  "BadRepo",
		Message: "no repo",
	}

	tests := []struct {
		name string
		gp   *configbutleraiv1alpha3.GitProvider
		want metav1.ConditionStatus
	}{
		{"ready", provider([]metav1.Condition{ready}), metav1.ConditionTrue},
		{"not ready -> False (downgrades)", provider([]metav1.Condition{notReady}), metav1.ConditionFalse},
		{"no condition -> Unknown (does not downgrade)", provider(nil), metav1.ConditionUnknown},
		{"absent provider -> Unknown", nil, metav1.ConditionUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scScheme(t))
			if tc.gp != nil {
				builder = builder.WithObjects(tc.gp)
			}
			r := &GitTargetReconciler{Client: builder.Build()}
			target := &configbutleraiv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "gt", Namespace: "team-a"},
				Spec: configbutleraiv1alpha3.GitTargetSpec{
					ProviderRef: configbutleraiv1alpha3.GitProviderReference{Name: "prov"},
				},
			}
			status, _, msg := r.gitProviderReadiness(context.Background(), target, "team-a")
			assert.Equal(t, tc.want, status)
			assert.NotEmpty(t, msg)
		})
	}
}
