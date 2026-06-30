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
	"fmt"

	"github.com/go-git/go-git/v5/plumbing/transport"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/ssh"
)

// SSHHostKeyConfig configures where SSH known_hosts (host-trust material) are sourced and the
// dev-only escape hatch for a host with no pinned key. It is set once at startup and threaded
// to every credentials read. Its zero value fails closed: no install-level default and no
// missing-key opt-out.
type SSHHostKeyConfig struct {
	// ControllerNamespace is the namespace the controller runs in; it scopes the install-level
	// default known-hosts ConfigMap.
	ControllerNamespace string

	// DefaultKnownHostsConfigMap names an optional install-level ConfigMap in ControllerNamespace
	// that supplies known_hosts when neither the credentials Secret nor the GitProvider supplies
	// it. Empty disables this layer.
	DefaultKnownHostsConfigMap string

	// AllowMissingKnownHosts permits SSH only when NO host-key source produced any known_hosts at
	// all (the controller's --insecure-allow-missing-known-hosts flag). A known_hosts that is
	// present but unparseable is always a hard error.
	AllowMissingKnownHosts bool
}

// getAuthFromSecret fetches the credentials Secret named by the GitProvider and resolves it into
// a go-git auth method. A GitProvider with no secretRef authenticates anonymously (public repos).
func getAuthFromSecret(
	ctx context.Context,
	k8sClient client.Client,
	provider *v1alpha3.GitProvider,
	hostKeys SSHHostKeyConfig,
) (transport.AuthMethod, error) {
	if provider.Spec.SecretRef == nil || provider.Spec.SecretRef.Name == "" {
		return nil, nil //nolint:nilnil // Returning nil auth for public repos is semantically correct
	}

	secretName := types.NamespacedName{
		Name:      provider.Spec.SecretRef.Name,
		Namespace: provider.Namespace,
	}

	var secret corev1.Secret
	if err := k8sClient.Get(ctx, secretName, &secret); err != nil {
		return nil, fmt.Errorf("failed to get secret %s: %w", secretName, err)
	}

	return AuthFromSecretData(ctx, k8sClient, provider, &secret, hostKeys)
}

// AuthFromSecretData resolves a go-git auth method from an already-fetched Git credentials Secret,
// accepting the Kubernetes-native, Flux, and Argo CD key dialects (the credentials Secret is the
// one portable artifact across those ecosystems). provider supplies the namespace and the optional
// knownHostsRef for SSH host trust; hostKeys supplies the install-level default and the dev escape
// hatch. Auth precedence is: SSH key (if present) → HTTP basic (username+password) → bearer token.
func AuthFromSecretData(
	ctx context.Context,
	k8sClient client.Client,
	provider *v1alpha3.GitProvider,
	secret *corev1.Secret,
	hostKeys SSHHostKeyConfig,
) (transport.AuthMethod, error) {
	if secret == nil {
		return nil, nil //nolint:nilnil // no secret means anonymous (public repository) access
	}

	// SSH private key: ssh-privatekey (Kubernetes-native) → identity (Flux) → sshPrivateKey (Argo).
	if privateKey, ok := firstSecretValue(secret, "ssh-privatekey", "identity", "sshPrivateKey"); ok {
		knownHosts, err := resolveKnownHosts(ctx, k8sClient, provider, secret, hostKeys)
		if err != nil {
			return nil, err
		}
		return ssh.GetAuthMethod(privateKey, sshPassphrase(secret), knownHosts, hostKeys.AllowMissingKnownHosts)
	}

	// HTTP basic auth: username + password — already identical across all three ecosystems.
	if username, ok := firstSecretValue(secret, "username"); ok {
		password, hasPassword := firstSecretValue(secret, "password")
		if !hasPassword {
			return nil, fmt.Errorf(
				"secret %s/%s contains username but no password for HTTP basic auth", secret.Namespace, secret.Name)
		}
		return GetHTTPAuthMethod(username, password)
	}

	// HTTP bearer token: bearerToken — the common token path in both Flux and Argo.
	if token, ok := firstSecretValue(secret, "bearerToken"); ok {
		return GetHTTPTokenAuthMethod(token)
	}

	return nil, fmt.Errorf(
		"secret %s/%s does not contain valid authentication data "+
			"(an SSH private key, username/password, or bearerToken)",
		secret.Namespace, secret.Name,
	)
}

// sshPassphrase returns the SSH private-key passphrase. It prefers our own ssh-password key and
// falls back to password, mirroring Flux's disambiguation: Flux stores the passphrase under
// password and tells it apart by the presence of an SSH key. This fallback is reached only when an
// SSH key is present, so a bare password is never mistaken for a passphrase.
func sshPassphrase(secret *corev1.Secret) string {
	if v, ok := firstSecretValue(secret, "ssh-password", "password"); ok {
		return v
	}
	return ""
}

// resolveKnownHosts resolves SSH host-trust material in priority order: the credentials Secret's
// own known_hosts, then the GitProvider's knownHostsRef, then the install-level default ConfigMap.
// It returns "" when no source yields host keys; the caller (ssh.GetAuthMethod) then fails closed
// unless the missing-key opt-out is set.
func resolveKnownHosts(
	ctx context.Context,
	k8sClient client.Client,
	provider *v1alpha3.GitProvider,
	secret *corev1.Secret,
	hostKeys SSHHostKeyConfig,
) (string, error) {
	// 1. Secret-level known_hosts — highest priority; keeps Flux-authored SSH Secrets working.
	if v, ok := firstSecretValue(secret, "known_hosts"); ok {
		return v, nil
	}

	// 2. GitProvider.spec.knownHostsRef — a namespace-local ConfigMap or Secret. A reference that
	//    is set but unreadable is the user's error and surfaces.
	if provider != nil && provider.Spec.KnownHostsRef != nil {
		v, found, err := readKnownHostsFromRef(ctx, k8sClient, provider.Namespace, provider.Spec.KnownHostsRef)
		if err != nil {
			return "", err
		}
		if found {
			return v, nil
		}
	}

	// 3. Install-level default known-hosts ConfigMap in the controller's namespace. Optional infra:
	//    a configured-but-absent ConfigMap is skipped, leaving the caller to fail closed.
	if hostKeys.DefaultKnownHostsConfigMap != "" && hostKeys.ControllerNamespace != "" {
		v, found, err := readKnownHostsFromConfigMap(
			ctx, k8sClient, hostKeys.ControllerNamespace, hostKeys.DefaultKnownHostsConfigMap, true)
		if err != nil {
			return "", err
		}
		if found {
			return v, nil
		}
	}

	return "", nil
}

// readKnownHostsFromRef reads known_hosts from a namespace-local ConfigMap or Secret. It accepts
// the known_hosts key, falling back to ssh_known_hosts (the key used by Argo CD's
// argocd-ssh-known-hosts-cm ConfigMap, for data copied out of it).
func readKnownHostsFromRef(
	ctx context.Context,
	k8sClient client.Client,
	namespace string,
	ref *v1alpha3.KnownHostsReference,
) (string, bool, error) {
	switch ref.Kind {
	case "", "ConfigMap":
		return readKnownHostsFromConfigMap(ctx, k8sClient, namespace, ref.Name, false)
	case "Secret":
		var secret corev1.Secret
		key := types.NamespacedName{Name: ref.Name, Namespace: namespace}
		if err := k8sClient.Get(ctx, key, &secret); err != nil {
			return "", false, fmt.Errorf("read knownHostsRef Secret %s: %w", key, err)
		}
		if v, ok := firstSecretValue(&secret, "known_hosts", "ssh_known_hosts"); ok {
			return v, true, nil
		}
		return "", false, nil
	default:
		return "", false, fmt.Errorf("unsupported knownHostsRef kind %q (must be ConfigMap or Secret)", ref.Kind)
	}
}

// readKnownHostsFromConfigMap reads known_hosts (or ssh_known_hosts) from a ConfigMap. When
// optionalMissing is set, a NotFound ConfigMap is reported as "no host keys" rather than an error,
// so an install-level default that has not been created yet simply falls through to fail-closed.
func readKnownHostsFromConfigMap(
	ctx context.Context,
	k8sClient client.Client,
	namespace, name string,
	optionalMissing bool,
) (string, bool, error) {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Name: name, Namespace: namespace}
	if err := k8sClient.Get(ctx, key, &cm); err != nil {
		if optionalMissing && apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read known-hosts ConfigMap %s: %w", key, err)
	}
	for _, k := range []string{"known_hosts", "ssh_known_hosts"} {
		if v, ok := cm.Data[k]; ok && v != "" {
			return v, true, nil
		}
	}
	return "", false, nil
}

// firstSecretValue returns the first non-empty value among the given Secret data keys, in order.
func firstSecretValue(secret *corev1.Secret, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := secret.Data[k]; ok && len(v) > 0 {
			return string(v), true
		}
	}
	return "", false
}
