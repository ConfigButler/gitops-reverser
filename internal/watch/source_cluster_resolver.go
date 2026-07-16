// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ConfigButler/gitops-reverser/internal/kubeconfig"
)

// sourceClusterDialTimeout bounds every call the operator makes to a remote source cluster
// (discovery, list, watch establishment). A remote is reached over a network the in-cluster
// config is not, so without a timeout an unreachable API server would hang the catalog-refresh
// loop indefinitely — exactly the SourceClusterReachable=False case the reachability split
// exists to surface promptly. It bounds the "controller never blocks forever on a dial" promise.
const sourceClusterDialTimeout = 15 * time.Second

// secretSourceClusterResolver resolves a source-cluster id — "<namespace>/<name>/<key>", as
// (api/v1alpha3).GitTarget.SourceClusterID() renders it — into a rest.Config, by reading that
// Secret from the CONFIG PLANE: the cluster the operator runs in.
//
// This is the whole point of the split. The credential for a cluster never has to live on
// that cluster, and the watched cluster holds nothing but the watched resources — no Secret,
// no configbutler.ai CRDs at all.
type secretSourceClusterResolver struct {
	// client reads Secrets from the config plane. Secret reads bypass the controller-runtime
	// cache (the manager client sets Cache.DisableFor on Secrets), so a rotated kubeconfig is
	// seen without a Secret informer — the same reasoning as the SOPS age-key reads.
	client client.Client

	// safety is the operator's exec / insecure-TLS opt-in. Both default off: an
	// operator-supplied kubeconfig is attacker-adjacent input, so we REJECT rather than
	// silently strip (diverging from Flux) unless a flag opts in.
	safety kubeconfig.SafetyPolicy

	// qps and burst bound the rate at which the operator talks to a source cluster. A remote
	// cluster is reached over a network the local one is not, so it gets client-side
	// throttling the in-cluster config does not carry by default.
	qps   float32
	burst int
}

// NewSecretSourceClusterResolver builds the production source-cluster resolver.
func NewSecretSourceClusterResolver(
	c client.Client,
	safety kubeconfig.SafetyPolicy,
	qps float32,
	burst int,
) SourceClusterResolver {
	return &secretSourceClusterResolver{client: c, safety: safety, qps: qps, burst: burst}
}

// sourceClusterRef is a source-cluster id parsed back into the Secret it names.
type sourceClusterRef struct {
	Namespace string
	Name      string
	Key       string
}

// sourceClusterIDSegments is the number of "/"-separated parts in a source-cluster id.
const sourceClusterIDSegments = 3

// parseSourceClusterID splits the id GitTarget.SourceClusterID() produces. It is a private
// encoding, never a user input: a GitTarget's namespace cannot contain "/" and neither can a
// Secret name, so the first two segments are unambiguous and the rest is the data key. The KEY
// segment MAY be empty — an omitted spec key is its own identity, and the resolver then falls
// back value→value.yaml.
func parseSourceClusterID(id string) (sourceClusterRef, error) {
	parts := strings.SplitN(id, "/", sourceClusterIDSegments)
	if len(parts) != sourceClusterIDSegments || parts[0] == "" || parts[1] == "" {
		return sourceClusterRef{}, fmt.Errorf("malformed source cluster id %q, want <namespace>/<name>/<key>", id)
	}
	return sourceClusterRef{Namespace: parts[0], Name: parts[1], Key: parts[2]}, nil
}

func (r *secretSourceClusterResolver) ResolveSourceCluster(
	ctx context.Context,
	clusterID string,
) (*rest.Config, string, error) {
	ref, err := parseSourceClusterID(clusterID)
	if err != nil {
		return nil, "", err
	}

	var secret corev1.Secret
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &secret); err != nil {
		return nil, "", fmt.Errorf("read kubeconfig Secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	raw, usedKey, ok := kubeconfig.ResolveKey(secret.Data, ref.Key)
	if !ok {
		return nil, "", &kubeconfig.RejectionError{
			Reason: kubeconfig.ReasonKeyNotFound,
			Message: fmt.Sprintf("kubeconfig Secret %s/%s has no kubeconfig under key %q",
				ref.Namespace, ref.Name, describeKey(ref.Key)),
		}
	}

	// Parse and REJECT unsafe kubeconfigs before building the config — a legible failure that
	// the controller's Validated gate reports with the same typed reason. Never dials.
	cfg, err := kubeconfig.BuildRESTConfig(raw, r.safety)
	if err != nil {
		return nil, "", fmt.Errorf("kubeconfig Secret %s/%s key %q: %w",
			ref.Namespace, ref.Name, usedKey, err)
	}
	if r.qps > 0 {
		cfg.QPS = r.qps
		cfg.Burst = r.burst
	}
	// Bound every dial so an unreachable remote surfaces as SourceClusterReachable=False
	// promptly instead of hanging the refresh loop.
	cfg.Timeout = sourceClusterDialTimeout

	// The Secret's resourceVersion is the version token: it changes on every rotation, and on
	// nothing else. The kubeconfig bytes themselves are dropped here — only the built
	// rest.Config survives the call.
	return cfg, secret.ResourceVersion, nil
}

// describeKey renders the resolved-key hint for a "key not found" message: an omitted spec key
// tried the value→value.yaml fallback.
func describeKey(specKey string) string {
	if specKey == "" {
		return "value or value.yaml"
	}
	return specKey
}
