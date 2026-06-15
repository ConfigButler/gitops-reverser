/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package git

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	gogithttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gogitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

func credTestSSHKey(t *testing.T) ([]byte, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	privatePEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	pub, err := gossh.NewPublicKey(&key.PublicKey)
	require.NoError(t, err)
	return privatePEM, knownhosts.Line([]string{"github.com"}, pub)
}

func credTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// AuthFromSecretData must read the SSH private key under any of the three ecosystem dialects
// (Kubernetes-native ssh-privatekey, Flux identity, Argo CD sshPrivateKey).
func TestAuthFromSecretData_SSHKeyDialects(t *testing.T) {
	privateKey, knownHosts := credTestSSHKey(t)
	c := credTestClient(t)
	for _, keyName := range []string{"ssh-privatekey", "identity", "sshPrivateKey"} {
		t.Run(keyName, func(t *testing.T) {
			secret := &corev1.Secret{Data: map[string][]byte{
				keyName:       privateKey,
				"known_hosts": []byte(knownHosts),
			}}
			auth, err := AuthFromSecretData(
				context.Background(), c, &configv1alpha1.GitProvider{}, secret, SSHHostKeyConfig{})
			require.NoError(t, err)
			assert.IsType(t, &gogitssh.PublicKeys{}, auth)
		})
	}
}

// A password presented alongside an SSH key is its passphrase (Flux's shared-key convention), not
// HTTP basic auth — so the result is still SSH public-key auth, never BasicAuth.
func TestAuthFromSecretData_PasswordIsSSHPassphraseWhenKeyPresent(t *testing.T) {
	privateKey, knownHosts := credTestSSHKey(t)
	c := credTestClient(t)
	secret := &corev1.Secret{Data: map[string][]byte{
		"ssh-privatekey": privateKey,
		"password":       []byte(""), // unencrypted key: ignored, but must not divert to basic auth
		"known_hosts":    []byte(knownHosts),
	}}
	auth, err := AuthFromSecretData(context.Background(), c, &configv1alpha1.GitProvider{}, secret, SSHHostKeyConfig{})
	require.NoError(t, err)
	assert.IsType(t, &gogitssh.PublicKeys{}, auth)
}

func TestAuthFromSecretData_HTTPBasicAndBearer(t *testing.T) {
	c := credTestClient(t)

	t.Run("basic", func(t *testing.T) {
		secret := &corev1.Secret{Data: map[string][]byte{
			"username": []byte("u"),
			"password": []byte("p"),
		}}
		auth, err := AuthFromSecretData(
			context.Background(), c, &configv1alpha1.GitProvider{}, secret, SSHHostKeyConfig{})
		require.NoError(t, err)
		basic, ok := auth.(*gogithttp.BasicAuth)
		require.True(t, ok)
		assert.Equal(t, "u", basic.Username)
		assert.Equal(t, "p", basic.Password)
	})

	t.Run("bearer token", func(t *testing.T) {
		secret := &corev1.Secret{Data: map[string][]byte{"bearerToken": []byte("gho_token")}}
		auth, err := AuthFromSecretData(
			context.Background(), c, &configv1alpha1.GitProvider{}, secret, SSHHostKeyConfig{})
		require.NoError(t, err)
		token, ok := auth.(*gogithttp.TokenAuth)
		require.True(t, ok)
		assert.Equal(t, "gho_token", token.Token)
	})

	t.Run("username without password", func(t *testing.T) {
		secret := &corev1.Secret{Data: map[string][]byte{"username": []byte("u")}}
		_, err := AuthFromSecretData(
			context.Background(), c, &configv1alpha1.GitProvider{}, secret, SSHHostKeyConfig{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no password")
	})

	t.Run("no recognizable credentials", func(t *testing.T) {
		secret := &corev1.Secret{Data: map[string][]byte{"random": []byte("x")}}
		_, err := AuthFromSecretData(
			context.Background(), c, &configv1alpha1.GitProvider{}, secret, SSHHostKeyConfig{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not contain valid authentication data")
	})

	t.Run("nil secret is anonymous", func(t *testing.T) {
		auth, err := AuthFromSecretData(
			context.Background(), c, &configv1alpha1.GitProvider{}, nil, SSHHostKeyConfig{})
		require.NoError(t, err)
		assert.Nil(t, auth)
	})
}

// Host keys resolve in priority order: Secret → GitProvider.knownHostsRef → install-level default,
// then fail closed. These cases exercise each tier with a valid SSH key whose only variable is
// where the known_hosts come from.
func TestResolveKnownHosts_Priority(t *testing.T) {
	privateKey, knownHosts := credTestSSHKey(t)
	khConfigMap := func(name string, key string) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
			Data:       map[string]string{key: knownHosts},
		}
	}

	t.Run("knownHostsRef ConfigMap (known_hosts key)", func(t *testing.T) {
		c := credTestClient(t, khConfigMap("hosts", "known_hosts"))
		provider := &configv1alpha1.GitProvider{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns"},
			Spec: configv1alpha1.GitProviderSpec{
				KnownHostsRef: &configv1alpha1.KnownHostsReference{Name: "hosts"},
			},
		}
		secret := &corev1.Secret{Data: map[string][]byte{"ssh-privatekey": privateKey}}
		auth, err := AuthFromSecretData(context.Background(), c, provider, secret, SSHHostKeyConfig{})
		require.NoError(t, err)
		assert.IsType(t, &gogitssh.PublicKeys{}, auth)
	})

	t.Run("knownHostsRef ConfigMap (Argo ssh_known_hosts key)", func(t *testing.T) {
		c := credTestClient(t, khConfigMap("hosts", "ssh_known_hosts"))
		provider := &configv1alpha1.GitProvider{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns"},
			Spec: configv1alpha1.GitProviderSpec{
				KnownHostsRef: &configv1alpha1.KnownHostsReference{Name: "hosts"},
			},
		}
		secret := &corev1.Secret{Data: map[string][]byte{"ssh-privatekey": privateKey}}
		auth, err := AuthFromSecretData(context.Background(), c, provider, secret, SSHHostKeyConfig{})
		require.NoError(t, err)
		assert.IsType(t, &gogitssh.PublicKeys{}, auth)
	})

	t.Run("knownHostsRef Secret", func(t *testing.T) {
		refSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "host-secret", Namespace: "ns"},
			Data:       map[string][]byte{"known_hosts": []byte(knownHosts)},
		}
		c := credTestClient(t, refSecret)
		provider := &configv1alpha1.GitProvider{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns"},
			Spec: configv1alpha1.GitProviderSpec{
				KnownHostsRef: &configv1alpha1.KnownHostsReference{Kind: "Secret", Name: "host-secret"},
			},
		}
		secret := &corev1.Secret{Data: map[string][]byte{"ssh-privatekey": privateKey}}
		auth, err := AuthFromSecretData(context.Background(), c, provider, secret, SSHHostKeyConfig{})
		require.NoError(t, err)
		assert.IsType(t, &gogitssh.PublicKeys{}, auth)
	})

	t.Run("knownHostsRef missing object is an error", func(t *testing.T) {
		c := credTestClient(t)
		provider := &configv1alpha1.GitProvider{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns"},
			Spec: configv1alpha1.GitProviderSpec{
				KnownHostsRef: &configv1alpha1.KnownHostsReference{Name: "absent"},
			},
		}
		secret := &corev1.Secret{Data: map[string][]byte{"ssh-privatekey": privateKey}}
		_, err := AuthFromSecretData(context.Background(), c, provider, secret, SSHHostKeyConfig{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "absent")
	})

	t.Run("install-level default ConfigMap", func(t *testing.T) {
		c := credTestClient(t, khConfigMap("cluster-hosts", "known_hosts"))
		secret := &corev1.Secret{Data: map[string][]byte{"ssh-privatekey": privateKey}}
		hostKeys := SSHHostKeyConfig{ControllerNamespace: "ns", DefaultKnownHostsConfigMap: "cluster-hosts"}
		auth, err := AuthFromSecretData(context.Background(), c, &configv1alpha1.GitProvider{}, secret, hostKeys)
		require.NoError(t, err)
		assert.IsType(t, &gogitssh.PublicKeys{}, auth)
	})

	t.Run("absent install-level default falls through to fail-closed", func(t *testing.T) {
		c := credTestClient(t)
		secret := &corev1.Secret{Data: map[string][]byte{"ssh-privatekey": privateKey}}
		hostKeys := SSHHostKeyConfig{ControllerNamespace: "ns", DefaultKnownHostsConfigMap: "missing"}
		_, err := AuthFromSecretData(context.Background(), c, &configv1alpha1.GitProvider{}, secret, hostKeys)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "known_hosts is required")
	})

	t.Run("no source and no opt-out fails closed", func(t *testing.T) {
		c := credTestClient(t)
		secret := &corev1.Secret{Data: map[string][]byte{"ssh-privatekey": privateKey}}
		_, err := AuthFromSecretData(context.Background(), c, &configv1alpha1.GitProvider{}, secret, SSHHostKeyConfig{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "known_hosts is required")
	})

	t.Run("opt-out permits missing known_hosts", func(t *testing.T) {
		c := credTestClient(t)
		secret := &corev1.Secret{Data: map[string][]byte{"ssh-privatekey": privateKey}}
		auth, err := AuthFromSecretData(
			context.Background(), c, &configv1alpha1.GitProvider{}, secret,
			SSHHostKeyConfig{AllowMissingKnownHosts: true})
		require.NoError(t, err)
		assert.IsType(t, &gogitssh.PublicKeys{}, auth)
	})
}

// getAuthFromSecret fetches the GitProvider's referenced Secret; a provider with no secretRef is
// anonymous, and a missing referenced Secret is an error.
func TestGetAuthFromSecret_FetchPaths(t *testing.T) {
	t.Run("no secretRef is anonymous", func(t *testing.T) {
		c := credTestClient(t)
		provider := &configv1alpha1.GitProvider{ObjectMeta: metav1.ObjectMeta{Namespace: "ns"}}
		auth, err := getAuthFromSecret(context.Background(), c, provider, SSHHostKeyConfig{})
		require.NoError(t, err)
		assert.Nil(t, auth)
	})

	t.Run("present secret resolves", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "ns"},
			Data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
		}
		c := credTestClient(t, secret)
		provider := &configv1alpha1.GitProvider{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns"},
			Spec:       configv1alpha1.GitProviderSpec{SecretRef: &configv1alpha1.LocalSecretReference{Name: "creds"}},
		}
		auth, err := getAuthFromSecret(context.Background(), c, provider, SSHHostKeyConfig{})
		require.NoError(t, err)
		assert.IsType(t, &gogithttp.BasicAuth{}, auth)
	})

	t.Run("missing referenced secret errors", func(t *testing.T) {
		c := credTestClient(t)
		provider := &configv1alpha1.GitProvider{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns"},
			Spec:       configv1alpha1.GitProviderSpec{SecretRef: &configv1alpha1.LocalSecretReference{Name: "absent"}},
		}
		_, err := getAuthFromSecret(context.Background(), c, provider, SSHHostKeyConfig{})
		require.Error(t, err)
	})
}

func TestGetHTTPTokenAuthMethod(t *testing.T) {
	auth, err := GetHTTPTokenAuthMethod("abc")
	require.NoError(t, err)
	assert.Equal(t, "abc", auth.(*gogithttp.TokenAuth).Token)

	_, err = GetHTTPTokenAuthMethod("")
	require.Error(t, err)
}
