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

package auditutil

// Subresource handling policy. GitOps Reverser mirrors exactly one Kubernetes
// subresource: the built-in /scale. It is the single case where a subresource writes
// the parent's desired state (the accepted replica count) AND Kubernetes exposes that
// value in a standardized response object. Every other subresource is observed state, a
// runtime stream, a credential, lifecycle control, a proxy, or an imperative action, so
// it is ignored. The rule is deliberately narrow: only `scale` can route, and only when
// the parent's replica path is known by built-in policy. See
// docs/design/manifest/version2/subresource-scope-reduction.md.

// IsScaleSubresource reports whether subresource is the built-in scale subresource. The
// webhook forwards only scale subresource events; every other subresource is dropped
// before Redis. A CRD or aggregated-API scale still passes this gate — it is the
// consumer that drops it for an unresolved parent replica path.
func IsScaleSubresource(subresource string) bool {
	return subresource == "scale"
}

// BuiltinScaleReplicasPath returns the parent replica field path for a currently
// served built-in scalable resource identified by API group and plural resource.
// ok is false when the resource is not a known built-in scalable type — for
// example a CRD or aggregated API resource — in which case the scale event must
// be dropped, never defaulted to .spec.replicas.
//
// Kubernetes' currently served built-in scalable resources all map scale.spec.replicas
// to the parent .spec.replicas field. The version is intentionally ignored because
// this path is stable for these resources across their served versions.
//
// CRD scale needs the CRD definition, because the path comes from
// spec.versions[*].subresources.scale.specReplicasPath. Aggregated APIs have no
// generic discovery field that reveals the parent write path. Each call returns
// a fresh slice the caller owns.
func BuiltinScaleReplicasPath(group, resource string) ([]string, bool) {
	switch {
	case group == "apps" && resource == "deployments",
		group == "apps" && resource == "statefulsets",
		group == "apps" && resource == "replicasets",
		group == "" && resource == "replicationcontrollers":
		return []string{"spec", "replicas"}, true
	default:
		return nil, false
	}
}
