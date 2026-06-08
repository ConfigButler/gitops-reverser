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
	"sort"

	"k8s.io/apimachinery/pkg/runtime"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// Subresource translation turns a mutating subresource audit event (e.g.
// deployments/scale) into a bounded set of parent-manifest field assignments,
// without hydrating the parent object. It implements the generic spec-passthrough
// rule from
// docs/design/manifest/version2/scale-subresource-audit-rehydration.md: the
// subresource body's spec subtree IS the desired-state delta, so each leaf under it
// becomes one assignment rooted at the parent's spec. Taking only spec is the
// structural sanitization — the body's status (e.g. a Scale's status.replicas), its
// own apiVersion/kind (autoscaling/v1 Scale, not the parent), and its server
// metadata never enter the patch.

// translateSubresourceToAssignments produces the field assignments for a subresource
// audit event. It prefers the post-mutation responseObject and falls back to the
// requestObject. ok is false when the body carries no usable spec, so the caller drops
// the event with a metric rather than guessing.
func translateSubresourceToAssignments(event auditv1.Event) ([]manifestedit.FieldAssignment, bool) {
	spec, ok := subresourceSpec(event)
	if !ok {
		return nil, false
	}
	assignments := assignmentsFromSpec(spec)
	if len(assignments) == 0 {
		return nil, false
	}
	return assignments, true
}

// subresourceSpec returns the desired spec subtree of a subresource body, preferring
// the authoritative post-mutation responseObject and falling back to the requestObject.
func subresourceSpec(event auditv1.Event) (map[string]interface{}, bool) {
	if spec, ok := specFromAuditObject(event.ResponseObject); ok {
		return spec, true
	}
	return specFromAuditObject(event.RequestObject)
}

// specFromAuditObject decodes a raw audit body and returns its spec map. It uses the
// apimachinery JSON decoder so integral numbers become int64 (matching how a manifest is
// rendered), and reads only spec — never status or any other top-level field. The body's
// own apiVersion/kind are irrelevant (a Scale, not the parent) and need not be present.
func specFromAuditObject(raw *runtime.Unknown) (map[string]interface{}, bool) {
	if raw == nil || len(raw.Raw) == 0 {
		return nil, false
	}
	var decoded map[string]interface{}
	if err := utiljson.Unmarshal(raw.Raw, &decoded); err != nil {
		return nil, false
	}
	spec, ok := decoded["spec"].(map[string]interface{})
	if !ok || len(spec) == 0 {
		return nil, false
	}
	return spec, true
}

// assignmentsFromSpec walks a spec subtree to its leaves, emitting one assignment per
// leaf rooted at the parent's spec. Leaf-level (not subtree-level) emission keeps the
// patch additive: each assignment owns only its own leaf, so a field the subresource
// did not mention is never deleted from the parent. Assignments are returned in stable
// path order so the event's dedup hash does not depend on map iteration order.
func assignmentsFromSpec(spec map[string]interface{}) []manifestedit.FieldAssignment {
	var out []manifestedit.FieldAssignment
	walkSpecLeaves([]string{"spec"}, spec, &out)
	sort.Slice(out, func(i, j int) bool {
		return pathLess(out[i].Path, out[j].Path)
	})
	return out
}

// walkSpecLeaves appends one assignment per leaf under node. A non-empty nested map is
// recursed into; everything else (scalars, slices, and empty maps) is a leaf.
func walkSpecLeaves(prefix []string, node map[string]interface{}, out *[]manifestedit.FieldAssignment) {
	for key, value := range node {
		path := append(append([]string(nil), prefix...), key)
		if child, ok := value.(map[string]interface{}); ok && len(child) > 0 {
			walkSpecLeaves(path, child, out)
			continue
		}
		*out = append(*out, manifestedit.FieldAssignment{Path: path, Value: value})
	}
}

// pathLess orders two field paths lexicographically, segment by segment.
func pathLess(a, b []string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}
