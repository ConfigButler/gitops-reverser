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

package queue

import (
	"k8s.io/apimachinery/pkg/runtime"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	"github.com/ConfigButler/gitops-reverser/internal/auditutil"
	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// Scale translation turns a built-in */scale audit event (e.g. deployments/scale) into a
// single parent-manifest replicas assignment, without hydrating the parent object. It is
// the only subresource translation GitOps Reverser performs: /scale is the standardized
// view that writes a parent's desired replica state, and Kubernetes returns the accepted
// value in responseObject.spec.replicas. Everything else — every other subresource, and
// every scale on a CRD or aggregated API whose parent replica path is unknown — is
// dropped with a scale-specific metric, never guessed. See
// docs/design/manifest/version2/subresource-scope-reduction.md.

// translateScaleToAssignments turns a */scale audit event for parent (group, resource)
// into a single replicas assignment at the parent's known replica path. It returns
// (assignments, dropOutcome, ok): on success ok is true, dropOutcome is "", and
// assignments holds exactly one (replicaPath -> responseObject.spec.replicas) pair; on a
// drop ok is false and dropOutcome is the scale-specific metric outcome the caller must
// record. Only the post-mutation responseObject is read — a request body is
// pre-admission intent, not confirmed accepted state — and only spec.replicas is read,
// never status, the body's own apiVersion/kind, or any other spec leaf.
func translateScaleToAssignments(
	event auditv1.Event,
	group, resource string,
) ([]manifestedit.FieldAssignment, string, bool) {
	if event.ObjectRef == nil || !auditutil.IsScaleSubresource(event.ObjectRef.Subresource) {
		return nil, pipelineOutcomeDroppedNonScale, false
	}
	replicaPath, known := auditutil.BuiltinScaleReplicasPath(group, resource)
	if !known {
		return nil, pipelineOutcomeDroppedScalePathUnresolved, false
	}
	replicas, ok := scaleReplicasFromResponse(event.ResponseObject)
	if !ok {
		return nil, pipelineOutcomeDroppedScaleMissingReplicas, false
	}
	return []manifestedit.FieldAssignment{{Path: replicaPath, Value: replicas}}, "", true
}

// scaleReplicasFromResponse reads responseObject.spec.replicas as an int64. It uses the
// apimachinery JSON decoder so a JSON integer becomes int64 (matching how a manifest is
// rendered), and reads nothing else: never status, never the requestObject, never the
// Scale body's own apiVersion/kind. ok is false when the body is absent, undecodable, or
// carries no integral spec.replicas.
func scaleReplicasFromResponse(raw *runtime.Unknown) (int64, bool) {
	if raw == nil || len(raw.Raw) == 0 {
		return 0, false
	}
	var decoded map[string]interface{}
	if err := utiljson.Unmarshal(raw.Raw, &decoded); err != nil {
		return 0, false
	}
	spec, ok := decoded["spec"].(map[string]interface{})
	if !ok {
		return 0, false
	}
	switch replicas := spec["replicas"].(type) {
	case int64:
		return replicas, true
	case float64:
		return int64(replicas), true
	default:
		return 0, false
	}
}
