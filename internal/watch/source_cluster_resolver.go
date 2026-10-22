// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"fmt"
	"net"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/kubeconfig"
)

// sourceClusterDialTimeout bounds CONNECTION ESTABLISHMENT to a remote source cluster (the TCP
// dial + TLS handshake), not the whole request. A remote is reached over a network the in-cluster
// config is not, so an unreachable API server must surface as SourceClusterReachable=False
// promptly instead of hanging the catalog-refresh loop. It is applied as a dialer timeout (not
// rest.Config.Timeout), so a long-lived watch built from the same config is NEVER cut off after
// this interval; finite discovery calls get it as a request timeout on a separate config copy
// (see clusterDiscovery).
const sourceClusterDialTimeout = 15 * time.Second

// sourceClusterKeepAlive is the TCP keep-alive on the dialer above, so an idle watch connection
// to a remote is kept healthy rather than silently half-open.
const sourceClusterKeepAlive = 30 * time.Second

// defaultSourceClusterQPS / -Burst are the client-side throttle for a remote source cluster whose
// KubeProvider sets no spec.client: a remote is reached over a network the in-cluster config is
// not, and is only read (list/watch/get).
const (
	defaultSourceClusterQPS   = 20
	defaultSourceClusterBurst = 30
)

// secretSourceClusterResolver resolves a source cluster — a KubeProvider's namespace and name, as
// (api/v1alpha3).GitTarget.KubeProviderKey() carries it — into a rest.Config, reading the
// kubeconfig Secret the provider names from the PROVIDER'S OWN namespace. A tenant's connection
// keeps its credential with the tenant, and the watched cluster holds nothing but the watched
// resources.
//
// It is also the one place the operator's own identity is handed out: a provider that sets
// spec.inCluster resolves to it only while the installation lists it, so every credential use (the
// watches, discovery, and the appliers) passes the same consent check.
type secretSourceClusterResolver struct {
	// client reads the KubeProvider and its kubeconfig Secret. Secret reads bypass the
	// controller-runtime cache (the manager client sets Cache.DisableFor on Secrets), so a rotated
	// kubeconfig is seen without a Secret informer — the same reasoning as the SOPS age-key reads.
	client client.Client

	// safety is the operator's exec / insecure-TLS opt-in and its inCluster allow-list. All default
	// off: an operator-supplied kubeconfig is attacker-adjacent input, so we REJECT rather than
	// silently strip (diverging from Flux) unless a flag opts in.
	safety kubeconfig.SafetyPolicy
}

// NewSecretSourceClusterResolver builds the production source-cluster resolver.
func NewSecretSourceClusterResolver(c client.Client, safety kubeconfig.SafetyPolicy) SourceClusterResolver {
	return &secretSourceClusterResolver{client: c, safety: safety}
}

func (r *secretSourceClusterResolver) ResolveSourceCluster(
	ctx context.Context,
	key k8stypes.NamespacedName,
) (*rest.Config, string, error) {
	var provider configv1alpha3.KubeProvider
	if err := r.client.Get(ctx, key, &provider); err != nil {
		return nil, "", fmt.Errorf("read KubeProvider %s: %w", key, err)
	}
	if provider.IsInCluster() {
		if rej := kubeconfig.CheckInCluster(key, r.safety); rej != nil {
			return nil, "", rej
		}
		// The UID is the whole version: there is no Secret to rotate, and a provider deleted and
		// recreated under the same name is a different object whose arm may differ.
		return nil, inClusterConfigVersion + "/" + string(provider.UID), nil
	}
	if provider.Spec.SecretRef == nil {
		return nil, "", &kubeconfig.RejectionError{
			Reason:  kubeconfig.ReasonInvalid,
			Message: fmt.Sprintf("KubeProvider %s sets neither spec.secretRef nor spec.inCluster", key),
		}
	}
	ref := provider.Spec.SecretRef

	var secret corev1.Secret
	secretKey := client.ObjectKey{Namespace: provider.Namespace, Name: ref.Name}
	if err := r.client.Get(ctx, secretKey, &secret); err != nil {
		return nil, "", fmt.Errorf("read kubeconfig Secret %s for KubeProvider %s: %w", secretKey, key, err)
	}
	raw, usedKey, ok := kubeconfig.ResolveKey(secret.Data, ref.Key)
	if !ok {
		return nil, "", &kubeconfig.RejectionError{
			Reason: kubeconfig.ReasonKeyNotFound,
			Message: fmt.Sprintf("kubeconfig Secret %s has no kubeconfig under key %q",
				secretKey, describeKey(ref.Key)),
		}
	}

	// Parse and REJECT unsafe kubeconfigs before building the config — a legible failure that the
	// KubeProvider's Validated gate reports with the same typed reason. Never dials.
	cfg, err := kubeconfig.BuildRESTConfig(raw, r.safety)
	if err != nil {
		return nil, "", fmt.Errorf("kubeconfig Secret %s key %q: %w", secretKey, usedKey, err)
	}
	cfg.QPS, cfg.Burst = r.throttleFor(&provider)
	// Bound CONNECTION SETUP so an unreachable remote surfaces as SourceClusterReachable=False
	// promptly — but do NOT set rest.Config.Timeout, which applies to the full HTTP request and
	// would cut off a long-lived watch every interval and churn reconnects. A dialer timeout bounds
	// the TCP/TLS handshake only; the established watch stream then stays open indefinitely. Finite
	// discovery/list calls are bounded separately (clusterDiscovery copies the config with a request
	// timeout; list callers pass a context deadline).
	cfg.Dial = (&net.Dialer{Timeout: sourceClusterDialTimeout, KeepAlive: sourceClusterKeepAlive}).DialContext

	// The version token changes exactly when the resolved config could change: on a kubeconfig
	// Secret rotation (its resourceVersion), on a provider spec change that alters qps/burst (its
	// generation), and on a provider deleted and recreated under the same name (its UID, since a new
	// object starts at generation 1 again). A change in any re-resolves and rebuilds the clients.
	version := fmt.Sprintf("%s/%d/%s", provider.UID, provider.Generation, secret.ResourceVersion)
	return cfg, version, nil
}

// throttleFor returns the effective client QPS/burst for a provider: its spec.client override
// when set, else the built-in default.
func (r *secretSourceClusterResolver) throttleFor(provider *configv1alpha3.KubeProvider) (float32, int) {
	qps := float32(defaultSourceClusterQPS)
	burst := defaultSourceClusterBurst
	client := provider.Spec.Client
	if client == nil {
		return qps, burst
	}
	if client.QPS != nil {
		qps = float32(*client.QPS)
	}
	if client.Burst != nil {
		burst = int(*client.Burst)
	}
	return qps, burst
}

// describeKey renders the resolved-key hint for a "key not found" message: an omitted spec key
// tried the value→value.yaml fallback.
func describeKey(specKey string) string {
	if specKey == "" {
		return "value or value.yaml"
	}
	return specKey
}
