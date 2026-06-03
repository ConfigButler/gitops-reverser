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

/*
Package manifestreport is the integration layer that drives the cluster-free
manifestedit library against a real repository and cluster state. It supplies the
two pieces of policy manifestedit deliberately refuses to own — the Git
projection and the canonical renderer — and provides a read-only reconcile that
reports what it would add, remove, or update.

It is the seam described in step 6 of
docs/future/manifestedit-abstraction-plan.md and detailed in
docs/future/manifestedit-integration-readonly-reconcile.md. It depends on
internal/sanitize (the projection/renderer) and internal/git/manifestedit (the
mechanism); manifestedit itself stays free of both.
*/
package manifestreport

import (
	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Project computes the Git projection of a live API object: the clean desired
// state the reverser would store. This is the "what does clean mean" policy that
// manifestedit does not own; the integration layer supplies it, and it is exactly
// the projection the live writer path uses (internal/sanitize).
func Project(obj *unstructured.Unstructured) *unstructured.Unstructured {
	return sanitize.Sanitize(obj)
}

// Render is the house canonical renderer injected into manifestedit for
// whole-document replacement and new files. It is the same renderer the Git
// writer uses (sanitize.MarshalToOrderedYAML, see
// internal/git/content_writer.go buildContentForWrite), so whole-replace and
// new-file output cannot drift from committed content. The object passed in is
// the already-projected desired state.
func Render(obj *unstructured.Unstructured) ([]byte, error) {
	return sanitize.MarshalToOrderedYAML(obj)
}

// EditOptions returns the production manifestedit options:
//   - Render: the house renderer above (so canonical output never drifts);
//   - ListMatch: zero value = index-based, deliberately not a global keyed
//     strategy — keyed matching needs a path/GVK-aware policy that does not exist
//     yet, and a blanket KeyField would change every named list's behavior;
//   - Owns: nil = whole-object truth (API-first), the only supported policy
//     (docs/future/manifestedit-field-ownership-spike.md).
func EditOptions() manifestedit.EditOptions {
	return manifestedit.EditOptions{Render: Render}
}

// identityOf reads the manifest identity from a live API object, matching how
// manifestedit derives identity from YAML.
func identityOf(obj *unstructured.Unstructured) manifestedit.Identity {
	return manifestedit.Identity{
		APIVersion: obj.GetAPIVersion(),
		Kind:       obj.GetKind(),
		Namespace:  obj.GetNamespace(),
		Name:       obj.GetName(),
	}
}
