// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// kubeRootCAConfigMap is the ConfigMap kube-controller-manager's root CA publisher writes into every
// namespace. Its content is the cluster's CA bundle: it means nothing outside that one cluster, it
// changes whenever the cluster is rebuilt, and the publisher overwrites any other value at once, so
// a copy in Git can neither be edited nor restored.
const kubeRootCAConfigMap = "kube-root-ca.crt"

// clusterGenerated reports whether an object is one the cluster itself writes and Git never holds,
// whatever a WatchRule selects. Unlike resource_policy.go, which excludes whole types, this
// excludes single objects of a type that is mirrored.
//
// Both gatherings ask it: the snapshot fold (desiredFromObject) leaves the object out of the
// desired set, and the live route drops its events. An object already in Git is therefore treated
// like any document the cluster does not hold: prune mode "Always" sweeps it on the next resync,
// and "OnEvent" leaves it for a human to delete.
func clusterGenerated(gvr schema.GroupVersionResource, u *unstructured.Unstructured) bool {
	if u == nil {
		return false
	}
	return gvr.Group == "" && gvr.Resource == "configmaps" && u.GetName() == kubeRootCAConfigMap
}
