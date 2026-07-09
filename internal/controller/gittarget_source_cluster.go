// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// GitTargetReasonSourceClusterUnreachable is the Validated=False reason for a GitTarget
// whose spec.sourceCluster names a Secret that is missing, empty at its key, or does not
// hold a parseable kubeconfig. It is a control-plane fault, not a data-plane one: nothing
// has been watched or written, and the human fix is on the Secret.
const GitTargetReasonSourceClusterUnreachable = "SourceClusterUnreachable"

// sourceClusterRulesCaughtUp reports whether the compiled WatchRule/ClusterWatchRule set
// already names the GitTarget's CURRENT source cluster.
//
// The watch data plane learns a GitTarget's source cluster from the rules that point at it,
// because rules resolve their GitTarget when they compile. A rule recompiles when its
// GitTarget's generation bumps — but this reconcile can win that race, and declaring in that
// window would open watches against the previous cluster and write its objects into this
// GitTarget's folder. Waiting one requeue is free; mirroring the wrong cluster is not.
//
// A GitTarget with no rules yet has nothing to watch and nothing to disagree about.
func (r *GitTargetReconciler) sourceClusterRulesCaughtUp(
	target *configbutleraiv1alpha3.GitTarget,
	gitDest types.ResourceReference,
) bool {
	compiled := r.EventRouter.WatchManager.CompiledSourceClusters(gitDest)
	switch len(compiled) {
	case 0:
		// Nothing points at this GitTarget, so there is nothing to watch and nothing to
		// disagree about.
		return true
	case 1:
		return compiled[0] == target.SourceClusterID()
	default:
		// The rules disagree with each other: some have recompiled and some have not.
		return false
	}
}

// describeSourceCluster renders a source-cluster id for logs and messages.
func describeSourceCluster(id string) string {
	if id == "" {
		return "local"
	}
	return id
}

// validateSourceCluster checks that a GitTarget's source-cluster kubeconfig can be read and
// parsed from the config plane, before any watch is opened against it.
//
// This is a *legibility* gate, not a security one. Without it, a typo'd Secret name would
// surface only as a stalled data plane and a repeating log line; with it, the GitTarget says
// exactly which Secret it could not read. It deliberately does NOT dial the cluster: an
// unreachable-right-now cluster is a transient the data plane retries, and a controller that
// blocked on a network round trip would stall every other GitTarget behind it.
//
// The kubeconfig bytes are parsed and dropped. Nothing is retained.
func (r *GitTargetReconciler) validateSourceCluster(
	ctx context.Context,
	target *configbutleraiv1alpha3.GitTarget,
) (bool, string) {
	if target.Spec.SourceCluster == nil {
		return true, ""
	}

	ref := target.Spec.SourceCluster.KubeConfigSecretRef
	key := ref.Key
	if key == "" {
		key = configbutleraiv1alpha3.DefaultKubeConfigSecretKey
	}
	secretKey := k8stypes.NamespacedName{Namespace: target.Namespace, Name: ref.Name}

	var secret corev1.Secret
	if err := r.Get(ctx, secretKey, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Sprintf(
				"spec.sourceCluster names Secret %s, which does not exist in this namespace",
				secretKey.String())
		}
		return false, fmt.Sprintf("cannot read source-cluster Secret %s: %v", secretKey.String(), err)
	}

	raw, present := secret.Data[key]
	if !present || len(raw) == 0 {
		return false, fmt.Sprintf(
			"source-cluster Secret %s has no data under key %q (set spec.sourceCluster.kubeConfigSecretRef.key)",
			secretKey.String(), key)
	}
	if _, err := clientcmd.RESTConfigFromKubeConfig(raw); err != nil {
		return false, fmt.Sprintf("source-cluster Secret %s key %q is not a usable kubeconfig: %v",
			secretKey.String(), key, err)
	}
	return true, ""
}
