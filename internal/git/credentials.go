// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"errors"
	"fmt"

	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	gogithttp "github.com/go-git/go-git/v6/plumbing/transport/http"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	sshpkg "github.com/ConfigButler/gitops-reverser/internal/ssh"
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
// go-git transport options. A GitProvider with no secretRef authenticates anonymously (public repos).
func getAuthFromSecret(
	ctx context.Context,
	k8sClient client.Client,
	provider *v1alpha3.GitProvider,
	hostKeys SSHHostKeyConfig,
) ([]gitclient.Option, error) {
	cred, err := credentialFromSecret(ctx, k8sClient, provider, hostKeys)
	if err != nil {
		return nil, err
	}
	return cred.Options(), nil
}

// credentialFromSecret is getAuthFromSecret before the options wrapper: it returns the concrete
// credential so the Secret-to-auth mapping stays assertable. See Credential.
func credentialFromSecret(
	ctx context.Context,
	k8sClient client.Client,
	provider *v1alpha3.GitProvider,
	hostKeys SSHHostKeyConfig,
) (Credential, error) {
	if provider.Spec.SecretRef == nil || provider.Spec.SecretRef.Name == "" {
		return Credential{}, nil // anonymous access for public repositories
	}

	secretName := types.NamespacedName{
		Name:      provider.Spec.SecretRef.Name,
		Namespace: provider.Namespace,
	}

	var secret corev1.Secret
	if err := k8sClient.Get(ctx, secretName, &secret); err != nil {
		return Credential{}, fmt.Errorf("failed to get secret %s: %w", secretName, err)
	}

	return CredentialFromSecretData(ctx, k8sClient, provider, &secret, hostKeys)
}

// Credential is the concrete credential a Secret yields, before it is wrapped into go-git v6's
// opaque transport client options. At most one field is non-nil; all nil means anonymous access to a
// public repository.
//
// This type exists because v6 removed transport.AuthMethod: authentication is now supplied as
// functional options, which are closures and therefore cannot be inspected. Keeping the concrete
// value on the way past preserves the ability to assert which Secret key maps to which auth field,
// which is the contract CredentialFromSecretData is actually responsible for.
type Credential struct {
	SSH    *sshpkg.KeyAuth
	Basic  *gogithttp.BasicAuth
	Bearer *gogithttp.TokenAuth
}

// Options renders the credential as go-git v6 transport client options. A zero Credential yields
// nil, which go-git treats as anonymous.
func (c Credential) Options() []gitclient.Option {
	switch {
	case c.SSH != nil:
		return []gitclient.Option{gitclient.WithSSHAuth(c.SSH)}
	case c.Basic != nil:
		return []gitclient.Option{gitclient.WithHTTPAuth(c.Basic)}
	case c.Bearer != nil:
		return []gitclient.Option{gitclient.WithHTTPAuth(c.Bearer)}
	default:
		return nil
	}
}

// AuthFromSecretData resolves go-git transport options from an already-fetched Git credentials
// Secret. It is the thin wrapper over CredentialFromSecretData; callers that need to know what kind
// of credential was produced should use that instead.
func AuthFromSecretData(
	ctx context.Context,
	k8sClient client.Client,
	provider *v1alpha3.GitProvider,
	secret *corev1.Secret,
	hostKeys SSHHostKeyConfig,
) ([]gitclient.Option, error) {
	cred, err := CredentialFromSecretData(ctx, k8sClient, provider, secret, hostKeys)
	if err != nil {
		return nil, err
	}
	return cred.Options(), nil
}

// CredentialFromSecretData resolves a credential from an already-fetched Git credentials Secret,
// accepting the Kubernetes-native, Flux, and Argo CD key dialects (the credentials Secret is the
// one portable artifact across those ecosystems). provider supplies the namespace and the optional
// knownHostsRef for SSH host trust; hostKeys supplies the install-level default and the dev escape
// hatch. Auth precedence is: SSH key (if present) → HTTP basic (username+password) → bearer token.
func CredentialFromSecretData(
	ctx context.Context,
	k8sClient client.Client,
	provider *v1alpha3.GitProvider,
	secret *corev1.Secret,
	hostKeys SSHHostKeyConfig,
) (Credential, error) {
	if secret == nil {
		return Credential{}, nil // no secret means anonymous (public repository) access
	}

	// SSH private key: ssh-privatekey (Kubernetes-native) → identity (Flux) → sshPrivateKey (Argo).
	if privateKey, ok := firstSecretValue(secret, "ssh-privatekey", "identity", "sshPrivateKey"); ok {
		knownHosts, err := resolveKnownHosts(ctx, k8sClient, provider, secret, hostKeys)
		if err != nil {
			return Credential{}, err
		}
		publicKeys, err := sshpkg.NewPublicKeyAuth(
			privateKey, sshPassphrase(secret), knownHosts, hostKeys.AllowMissingKnownHosts)
		if err != nil {
			return Credential{}, err
		}
		return Credential{SSH: publicKeys}, nil
	}

	// HTTP basic auth: username + password — already identical across all three ecosystems.
	//
	// The password is what carries the credential, so it is what we branch on. Azure DevOps
	// documents its Personal Access Tokens as an EMPTY username with the PAT as the password
	// (the https://:PAT@dev.azure.com/... form), and firstSecretValue treats an empty value as
	// an absent key — so keying off the username would refuse the very Secret ADO tells people
	// to create. A username without a password stays an error: that one is a real mistake.
	username, hasUsername := firstSecretValue(secret, "username")
	if password, ok := firstSecretValue(secret, "password"); ok {
		return Credential{Basic: &gogithttp.BasicAuth{Username: username, Password: password}}, nil
	}
	if hasUsername {
		return Credential{}, fmt.Errorf(
			"secret %s/%s contains username but no password for HTTP basic auth", secret.Namespace, secret.Name)
	}

	// HTTP bearer token: bearerToken — the common token path in both Flux and Argo.
	if token, ok := firstSecretValue(secret, "bearerToken"); ok {
		if token == "" {
			return Credential{}, errors.New("bearer token cannot be empty")
		}
		return Credential{Bearer: &gogithttp.TokenAuth{Token: token}}, nil
	}

	return Credential{}, fmt.Errorf(
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
