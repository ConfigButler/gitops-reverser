// SPDX-License-Identifier: Apache-2.0

// Package kubeconfig resolves and safety-checks a source-cluster kubeconfig held in a Secret,
// shared by the watch data plane's SourceClusterResolver and the GitTarget controller's
// Validated gate so both apply exactly one contract: Flux's value→value.yaml key order, and a
// REJECT — not Flux's silent strip — of exec auth providers and insecure-skip-tls-verify.
//
// It depends only on client-go's clientcmd (which this repo already has) and never dials: the
// safety check is a few lines over the parsed kubeconfig, deliberately not a Flux import.
package kubeconfig

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Reason strings map one-to-one onto the GitTarget Validated=False reasons the controller
// reports and the e2e plan asserts. They live here so the resolver's errors and the
// controller's condition reasons can never drift.
const (
	// ReasonSecretNotFound: spec.kubeConfig.secretRef names a Secret that does not exist.
	ReasonSecretNotFound = "KubeConfigSecretNotFound"
	// ReasonKeyNotFound: the Secret exists but has no kubeconfig under the resolved key.
	ReasonKeyNotFound = "KubeConfigKeyNotFound"
	// ReasonInvalid: the bytes are not a parseable kubeconfig.
	ReasonInvalid = "KubeConfigInvalid"
	// ReasonExecNotAllowed: the kubeconfig carries an exec auth provider (runs a binary in
	// the operator Pod) and --insecure-kubeconfig-exec is not set.
	ReasonExecNotAllowed = "KubeConfigExecNotAllowed"
	// ReasonInsecureTLSNotAllowed: the kubeconfig sets insecure-skip-tls-verify and
	// --insecure-kubeconfig-tls is not set.
	ReasonInsecureTLSNotAllowed = "KubeConfigInsecureTLSNotAllowed"
	// ReasonFileReferenceNotAllowed: the kubeconfig names a credential or CA by file PATH
	// (tokenFile, client-certificate, client-key, or certificate-authority) instead of embedding
	// it. client-go reads those paths from the operator Pod's OWN filesystem when it builds the
	// REST config, so an operator-supplied (attacker-adjacent) kubeconfig could point at in-Pod
	// Secrets and ship them to a remote API server it names. Always rejected — there is no safe
	// opt-in, unlike exec/insecure-TLS: require the embedded *-data (or inline token) forms.
	ReasonFileReferenceNotAllowed = "KubeConfigFileReferenceNotAllowed"
	// ReasonInClusterNotAuthorized: a KubeProvider sets spec.inCluster, the operator's own
	// identity, and --in-cluster-kube-providers does not list it.
	ReasonInClusterNotAuthorized = "InClusterNotAuthorized"
)

// SafetyPolicy is the operator's opt-in to what this package rejects by default: the two kubeconfig
// footguns, and the operator's own identity. All default off — the safe posture, diverging from
// Flux's silent strip.
type SafetyPolicy struct {
	// AllowExec permits exec auth providers (--insecure-kubeconfig-exec).
	AllowExec bool
	// AllowInsecureTLS permits insecure-skip-tls-verify (--insecure-kubeconfig-tls).
	AllowInsecureTLS bool
	// InClusterProviders are the KubeProviders allowed to use spec.inCluster
	// (--in-cluster-kube-providers). Empty, none is.
	InClusterProviders map[types.NamespacedName]struct{}
}

// The allow-list is an exact set of names on purpose. A wildcard, a prefix or a namespace-wide
// grant would let anyone who can create a KubeProvider in that scope pick up the operator's own
// identity, which is exactly the path this list exists to close.

// ParseInClusterProviders parses the --in-cluster-kube-providers value: a comma-separated list of
// exact namespace/name entries. Empty entries are ignored; anything else that is not one
// namespace/name pair is an error naming the entry.
func ParseInClusterProviders(value string) (map[types.NamespacedName]struct{}, error) {
	out := map[types.NamespacedName]struct{}{}
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		namespace, name, ok := strings.Cut(entry, "/")
		if !ok || namespace == "" || name == "" || strings.Contains(name, "/") || strings.Contains(entry, "*") {
			return nil, fmt.Errorf(
				"in-cluster-kube-providers entry %q is not namespace/name; list each KubeProvider allowed to "+
					"use spec.inCluster exactly, for example gitops-reverser/local", entry)
		}
		out[types.NamespacedName{Namespace: namespace, Name: name}] = struct{}{}
	}
	return out, nil
}

// AllowsInCluster reports whether the KubeProvider named provider may use the operator's own
// identity.
func (p SafetyPolicy) AllowsInCluster(provider types.NamespacedName) bool {
	_, ok := p.InClusterProviders[provider]
	return ok
}

// CheckInCluster is the consent check for a KubeProvider that sets spec.inCluster: nil when the
// installation lists it, and a RejectionError naming the flag when it does not.
func CheckInCluster(provider types.NamespacedName, policy SafetyPolicy) *RejectionError {
	if policy.AllowsInCluster(provider) {
		return nil
	}
	allowed := make([]string, 0, len(policy.InClusterProviders))
	for key := range policy.InClusterProviders {
		allowed = append(allowed, key.String())
	}
	sort.Strings(allowed)
	listed := "lists none"
	if len(allowed) > 0 {
		listed = "lists only " + strings.Join(allowed, ", ")
	}
	return &RejectionError{
		Reason: ReasonInClusterNotAuthorized,
		Message: fmt.Sprintf(
			"KubeProvider %s sets spec.inCluster, the operator's own identity, and the installation does not "+
				"allow it: --in-cluster-kube-providers %s. It is never used; set spec.secretRef instead, or ask "+
				"the installation's owner to list it", provider, listed),
	}
}

// RejectionError is a typed, legible reason a kubeconfig was not accepted. It carries both the
// stable Reason (for a condition) and a human Message (for its text).
type RejectionError struct {
	Reason  string
	Message string
}

// Error implements error so a RejectionError can flow through the resolver's error return.
func (r *RejectionError) Error() string { return r.Message }

// ResolveKey selects the kubeconfig bytes from a Secret's data, following Flux's order:
// an explicit spec key wins; otherwise "value" then "value.yaml". ok is false when no
// candidate key holds a non-empty value — the caller reports ReasonKeyNotFound. usedKey is
// the key the bytes came from, for legible messages.
func ResolveKey(data map[string][]byte, specKey string) ([]byte, string, bool) {
	candidates := []string{specKey}
	if specKey == "" {
		candidates = []string{"value", "value.yaml"}
	}
	for _, key := range candidates {
		if v, present := data[key]; present && len(v) > 0 {
			return v, key, true
		}
	}
	return nil, "", false
}

// Check parses raw kubeconfig bytes and applies the safety policy WITHOUT dialing. A nil
// return means the kubeconfig is well-formed and permitted; a non-nil *RejectionError names exactly
// which input was wrong (Invalid / ExecNotAllowed / InsecureTLSNotAllowed).
func Check(raw []byte, policy SafetyPolicy) *RejectionError {
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return &RejectionError{Reason: ReasonInvalid, Message: fmt.Sprintf("not a parseable kubeconfig: %v", err)}
	}
	return checkParsed(cfg, policy)
}

// checkParsed is the safety half of Check, over an already-parsed config.
func checkParsed(cfg *clientcmdapi.Config, policy SafetyPolicy) *RejectionError {
	if !policy.AllowExec {
		for name, auth := range cfg.AuthInfos {
			if auth != nil && auth.Exec != nil {
				return &RejectionError{
					Reason: ReasonExecNotAllowed,
					Message: fmt.Sprintf(
						"kubeconfig user %q uses an exec auth provider, which runs a binary in the operator "+
							"Pod; rejected. Set --insecure-kubeconfig-exec to allow it, or use a token/cert credential.",
						name),
				}
			}
		}
	}
	if !policy.AllowInsecureTLS {
		for name, cluster := range cfg.Clusters {
			if cluster != nil && cluster.InsecureSkipTLSVerify {
				return &RejectionError{
					Reason: ReasonInsecureTLSNotAllowed,
					Message: fmt.Sprintf(
						"kubeconfig cluster %q sets insecure-skip-tls-verify, which disables server certificate "+
							"validation; rejected. Set --insecure-kubeconfig-tls to allow it, or embed the CA.",
						name),
				}
			}
		}
	}
	if rej := checkNoFileReferences(cfg); rej != nil {
		return rej
	}
	return nil
}

// checkNoFileReferences rejects any credential or CA named by file PATH. client-go resolves
// tokenFile / client-certificate / client-key / certificate-authority against the process's own
// filesystem when it builds the REST config, so a remote kubeconfig that named an in-Pod path
// would make the operator read its own Secrets and send them to the server the kubeconfig points
// at. There is no legitimate reason for an operator-supplied remote kubeconfig to reference the
// operator Pod's files, and no safe opt-in — so this is unconditional (unlike exec/insecure-TLS).
func checkNoFileReferences(cfg *clientcmdapi.Config) *RejectionError {
	for name, auth := range cfg.AuthInfos {
		if auth == nil {
			continue
		}
		if field := fileBackedAuthField(auth); field != "" {
			return &RejectionError{
				Reason: ReasonFileReferenceNotAllowed,
				Message: fmt.Sprintf(
					"kubeconfig user %q names %s by file path, which client-go would read from the operator "+
						"Pod's filesystem; rejected. Embed the credential with its *-data field (client-"+
						"certificate-data / client-key-data) or use an inline token.", name, field),
			}
		}
	}
	for name, cluster := range cfg.Clusters {
		if cluster != nil && cluster.CertificateAuthority != "" {
			return &RejectionError{
				Reason: ReasonFileReferenceNotAllowed,
				Message: fmt.Sprintf(
					"kubeconfig cluster %q names certificate-authority by file path, which client-go would "+
						"read from the operator Pod's filesystem; rejected. Embed it with "+
						"certificate-authority-data.", name),
			}
		}
	}
	return nil
}

// fileBackedAuthField reports the first file-path credential field an AuthInfo sets, or "".
func fileBackedAuthField(auth *clientcmdapi.AuthInfo) string {
	switch {
	case auth.ClientCertificate != "":
		return "client-certificate"
	case auth.ClientKey != "":
		return "client-key"
	case auth.TokenFile != "":
		return "tokenFile"
	}
	return ""
}

// BuildRESTConfig parses raw kubeconfig bytes, applies the safety policy, and returns the
// rest.Config to reach the cluster. It is the resolver's one call: parse → reject-unsafe →
// build. A *RejectionError is returned (as error) when the bytes are unusable, so the caller can
// surface the same typed reason the controller's Validated gate does. It never dials.
func BuildRESTConfig(raw []byte, policy SafetyPolicy) (*rest.Config, error) {
	if rej := Check(raw, policy); rej != nil {
		return nil, rej
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		// Check already parsed successfully, so this is unusual; classify it as Invalid anyway.
		return nil, &RejectionError{
			Reason:  ReasonInvalid,
			Message: fmt.Sprintf("build REST config from kubeconfig: %v", err),
		}
	}
	return cfg, nil
}

// AsRejection extracts the *RejectionError from an error chain, so a caller can read the typed
// Reason. It returns false for any other error.
func AsRejection(err error) (*RejectionError, bool) {
	var rej *RejectionError
	if errors.As(err, &rej) {
		return rej, true
	}
	return nil, false
}
