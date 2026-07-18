// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"fmt"
	"net"
	"time"

	corev1 "k8s.io/api/core/v1"
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

// secretSourceClusterResolver resolves a source-cluster NAME — a ClusterProvider's name, as
// (api/v1alpha3).GitTarget.SourceCluster() carries it — into a rest.Config by looking up the
// cluster-scoped ClusterProvider and reading its kubeConfig Secret from the OPERATOR NAMESPACE
// (the config plane): the cluster the operator runs in.
//
// This is the whole point of the split. The credential for a cluster never has to live on that
// cluster, and the watched cluster holds nothing but the watched resources — no Secret, no
// configbutler.ai CRDs at all. The resolver is only ever asked to resolve a REMOTE provider;
// the in-cluster "default" provider is served by the manager's own in-cluster config and never
// reaches here.
type secretSourceClusterResolver struct {
	// client reads the ClusterProvider (cluster-scoped) and its kubeConfig Secret. Secret reads
	// bypass the controller-runtime cache (the manager client sets Cache.DisableFor on Secrets),
	// so a rotated kubeconfig is seen without a Secret informer — the same reasoning as the SOPS
	// age-key reads.
	client client.Client

	// operatorNamespace is where a ClusterProvider's kubeConfig Secret is pinned. A cluster-scoped
	// provider has no namespace of its own, so the credential for a cluster is always read from
	// here — never from the source cluster.
	operatorNamespace string

	// safety is the operator's exec / insecure-TLS opt-in. Both default off: an
	// operator-supplied kubeconfig is attacker-adjacent input, so we REJECT rather than
	// silently strip (diverging from Flux) unless a flag opts in.
	safety kubeconfig.SafetyPolicy

	// qps and burst are the GLOBAL defaults (--source-cluster-qps/-burst) bounding the rate at
	// which the operator talks to a source cluster. A ClusterProvider may override them per
	// cluster via spec.qps/spec.burst.
	qps   float32
	burst int
}

// NewSecretSourceClusterResolver builds the production source-cluster resolver.
func NewSecretSourceClusterResolver(
	c client.Client,
	operatorNamespace string,
	safety kubeconfig.SafetyPolicy,
	qps float32,
	burst int,
) SourceClusterResolver {
	return &secretSourceClusterResolver{
		client:            c,
		operatorNamespace: operatorNamespace,
		safety:            safety,
		qps:               qps,
		burst:             burst,
	}
}

func (r *secretSourceClusterResolver) ResolveSourceCluster(
	ctx context.Context,
	providerName string,
) (*rest.Config, string, error) {
	var provider configv1alpha3.ClusterProvider
	if err := r.client.Get(ctx, client.ObjectKey{Name: providerName}, &provider); err != nil {
		return nil, "", fmt.Errorf("read ClusterProvider %q: %w", providerName, err)
	}
	if provider.Spec.KubeConfig == nil {
		// No kubeConfig means the operator's OWN cluster — legal for every provider name, so this
		// is the in-cluster answer (nil config), never an error. The name is irrelevant: what makes
		// a provider local is the absent kubeConfig, not being called "default".
		return nil, inClusterConfigVersion, nil
	}
	if provider.Spec.KubeConfig.SecretRef == nil {
		return nil, "", &kubeconfig.RejectionError{
			Reason:  kubeconfig.ReasonInvalid,
			Message: fmt.Sprintf("ClusterProvider %q sets kubeConfig without a secretRef", providerName),
		}
	}
	ref := provider.Spec.KubeConfig.SecretRef

	var secret corev1.Secret
	secretKey := client.ObjectKey{Namespace: r.operatorNamespace, Name: ref.Name}
	if err := r.client.Get(ctx, secretKey, &secret); err != nil {
		return nil, "", fmt.Errorf("read kubeconfig Secret %s for ClusterProvider %q: %w", secretKey, providerName, err)
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
	// ClusterProvider's Validated gate reports with the same typed reason. Never dials.
	cfg, err := kubeconfig.BuildRESTConfig(raw, r.safety)
	if err != nil {
		return nil, "", fmt.Errorf("kubeconfig Secret %s key %q: %w", secretKey, usedKey, err)
	}
	if qps, burst := r.throttleFor(&provider); qps > 0 {
		cfg.QPS = qps
		cfg.Burst = burst
	}
	// Bound CONNECTION SETUP so an unreachable remote surfaces as SourceClusterReachable=False
	// promptly — but do NOT set rest.Config.Timeout, which applies to the full HTTP request and
	// would cut off a long-lived watch every interval and churn reconnects. A dialer timeout bounds
	// the TCP/TLS handshake only; the established watch stream then stays open indefinitely. Finite
	// discovery/list calls are bounded separately (clusterDiscovery copies the config with a request
	// timeout; list callers pass a context deadline).
	cfg.Dial = (&net.Dialer{Timeout: sourceClusterDialTimeout, KeepAlive: sourceClusterKeepAlive}).DialContext

	// The version token changes exactly when the resolved config could change: on a kubeconfig
	// Secret rotation (its resourceVersion) OR on a provider spec change that alters qps/burst
	// (its generation). A change in either re-resolves and rebuilds the clients.
	version := fmt.Sprintf("%d/%s", provider.Generation, secret.ResourceVersion)
	return cfg, version, nil
}

// throttleFor returns the effective client QPS/burst for a provider: its per-provider override
// when set, else the operator-wide default. A zero global default leaves the rest.Config defaults
// untouched (the same behavior as before per-provider overrides existed).
func (r *secretSourceClusterResolver) throttleFor(provider *configv1alpha3.ClusterProvider) (float32, int) {
	qps := r.qps
	burst := r.burst
	if provider.Spec.QPS != nil {
		qps = float32(*provider.Spec.QPS)
	}
	if provider.Spec.Burst != nil {
		burst = int(*provider.Spec.Burst)
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
