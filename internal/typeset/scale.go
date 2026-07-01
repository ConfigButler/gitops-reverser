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

package typeset

import "k8s.io/apimachinery/pkg/runtime/schema"

// ScaleSourceBuiltinRegistry labels a ScaleBinding resolved from the built-in
// registry below, so it reads identically to a CRD-sourced binding for the writer.
const ScaleSourceBuiltinRegistry = "builtin-registry"

// builtinSpecReplicasPath is the parent replica path every currently-served
// built-in scalable resource maps scale.spec.replicas onto. The version is
// intentionally ignored: this path is stable for these resources across their
// served versions.
const builtinSpecReplicasPath = ".spec.replicas"

// builtinScaleResponseGVK is the standardized object Kubernetes returns for a
// built-in /scale call.
func builtinScaleResponseGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "autoscaling", Version: "v1", Kind: "Scale"}
}

// builtinScalable is the closed set of currently-served built-in scalable
// resources, keyed by API group and plural resource. Each maps scale.spec.replicas
// to the parent's .spec.replicas. CRDs draw the path from the CRD definition and
// aggregated APIs have no generic discovery field, so neither is listed here; both
// must resolve their own binding rather than default to .spec.replicas.
func builtinScalable(group, resource string) bool {
	switch {
	case group == "apps" && resource == "deployments",
		group == "apps" && resource == "statefulsets",
		group == "apps" && resource == "replicasets",
		group == "" && resource == "replicationcontrollers":
		return true
	default:
		return false
	}
}

// BuiltinScale returns the /scale binding for a currently-served built-in scalable
// resource identified by API group and plural resource. ok is false for any other
// resource — a CRD, an aggregated API, or a non-scalable built-in — in which case
// the scale event must be resolved elsewhere or dropped, never defaulted to
// .spec.replicas. It is the single source of built-in scale facts, shared by the
// cluster registry (origin/scale enrichment) and the audit consumer (scale write).
func BuiltinScale(group, resource string) (ScaleBinding, bool) {
	if !builtinScalable(group, resource) {
		return ScaleBinding{}, false
	}
	return ScaleBinding{
		Enabled:          true,
		Source:           ScaleSourceBuiltinRegistry,
		ResponseGVK:      builtinScaleResponseGVK(),
		SpecReplicasPath: builtinSpecReplicasPath,
		Usable:           true,
	}, true
}
