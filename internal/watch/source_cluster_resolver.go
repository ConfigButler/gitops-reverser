// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// secretSourceClusterResolver resolves a source-cluster id — "<namespace>/<name>/<key>",
// as GitTarget.SourceClusterID() renders it — into a rest.Config, by reading that Secret
// from the CONFIG PLANE: the cluster the operator runs in.
//
// This is the whole point of the split. The credential for a cluster never has to live on
// that cluster, and the watched cluster holds nothing but the watched resources — no
// Secret, no configbutler.ai CRDs at all.
type secretSourceClusterResolver struct {
	// client reads Secrets from the config plane. Secret reads bypass the controller-runtime
	// cache (see newManager), so a rotated kubeconfig is seen without a Secret informer.
	client client.Client

	// qps and burst bound the rate at which the operator talks to a source cluster. A
	// remote cluster is reached over a network the local one is not, so it gets client-side
	// throttling the in-cluster config does not carry by default.
	qps   float32
	burst int
}

// NewSecretSourceClusterResolver builds the production source-cluster resolver.
func NewSecretSourceClusterResolver(c client.Client, qps float32, burst int) SourceClusterResolver {
	return &secretSourceClusterResolver{client: c, qps: qps, burst: burst}
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
// encoding, never a user input: a GitTarget's namespace cannot contain "/" and neither can
// a Secret name, so the first two segments are unambiguous and the rest is the data key.
func parseSourceClusterID(id string) (sourceClusterRef, error) {
	parts := strings.SplitN(id, "/", sourceClusterIDSegments)
	if len(parts) != sourceClusterIDSegments || parts[0] == "" || parts[1] == "" || parts[2] == "" {
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
	raw, ok := secret.Data[ref.Key]
	if !ok || len(raw) == 0 {
		return nil, "", fmt.Errorf("kubeconfig Secret %s/%s has no data under key %q",
			ref.Namespace, ref.Name, ref.Key)
	}

	cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		return nil, "", fmt.Errorf("parse kubeconfig from Secret %s/%s key %q: %w",
			ref.Namespace, ref.Name, ref.Key, err)
	}
	if r.qps > 0 {
		cfg.QPS = r.qps
		cfg.Burst = r.burst
	}

	// The Secret's resourceVersion is the version token: it changes on every rotation, and
	// on nothing else. The kubeconfig bytes themselves are dropped here — only the built
	// rest.Config survives the call.
	return cfg, secret.ResourceVersion, nil
}
