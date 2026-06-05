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

package manifestanalyzer

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/mapping"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// deployResource / configMapResource are the GVR-based identities a DELETE watch event
// carries (no object body), mirroring desiredDeployWeb / desiredConfigMap's Resource.
func deployResource() types.ResourceIdentifier {
	return types.NewResourceIdentifier("apps", "v1", "deployments", "default", "web")
}

func configMapResource(name string) types.ResourceIdentifier {
	return types.NewResourceIdentifier("", "v1", "configmaps", "default", name)
}

// TestPlanDelete_ByResourceIdentity: a DELETE event carrying only the GVR/name resolves
// through the resource-identity index to the right document and emits one
// delete-document action carrying the manifest (GVK) identity the writer needs.
func TestPlanDelete_ByResourceIdentity(t *testing.T) {
	store := planStore(t)
	action, emitted := PlanDelete(store, deployResource())
	if !emitted {
		t.Fatalf("a resource that exists in Git should emit a delete action")
	}
	if action.Kind != PlanDeleteDocument {
		t.Errorf("kind = %q, want delete-document", action.Kind)
	}
	if action.Ref != (RecordRef{FilePath: "deploy.yaml", DocumentIndex: 0}) {
		t.Errorf("ref = %+v, want deploy.yaml#0", action.Ref)
	}
	wantID := manifestedit.Identity{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "default", Name: "web"}
	if action.Identity != wantID {
		t.Errorf("identity = %+v, want %+v", action.Identity, wantID)
	}
	if action.Resource != deployResource() {
		t.Errorf("resource = %+v, want %+v", action.Resource, deployResource())
	}
	if action.Desired != nil {
		t.Errorf("a delete action carries no desired object, got %+v", action.Desired)
	}
}

// TestPlanDelete_MovedManifest is the headline M6 case: the Deployment lives at a
// NON-canonical path (legacy/foo.yaml, not apps/v1/deployments/default/web.yaml), and a
// delete event with only GVR/name still finds it — because location is content-derived,
// not path-derived. This is the gap the old path scan left (a moved manifest was
// invisible to the canonical-path lookup).
func TestPlanDelete_MovedManifest(t *testing.T) {
	fsys := fstest.MapFS{"legacy/foo.yaml": {Data: []byte(deployYAML)}}
	store := BuildStore(context.Background(), fsys, mapping.NewStaticSnapshotMapper(sampleClusterSnapshot()))

	action, emitted := PlanDelete(store, deployResource())
	if !emitted {
		t.Fatalf("moved manifest should resolve")
	}
	if action.Ref.FilePath != "legacy/foo.yaml" {
		t.Errorf("ref path = %q, want the actual (moved) file legacy/foo.yaml", action.Ref.FilePath)
	}
}

// TestPlanDelete_MultiDocIndex: deleting one document of a multi-document file targets
// the right document index (cm.yaml holds ConfigMap a at #0 and b at #1).
func TestPlanDelete_MultiDocIndex(t *testing.T) {
	store := planStore(t)
	action, emitted := PlanDelete(store, configMapResource("b"))
	if !emitted {
		t.Fatalf("ConfigMap b should resolve")
	}
	if action.Ref != (RecordRef{FilePath: "cm.yaml", DocumentIndex: 1}) {
		t.Errorf("ref = %+v, want cm.yaml#1", action.Ref)
	}
}

// TestPlanDelete_NotInGit: a delete for a resource Git never materialised is a no-op —
// no action, no error. The cluster dropped something we do not track; already converged.
func TestPlanDelete_NotInGit(t *testing.T) {
	store := planStore(t)
	action, emitted := PlanDelete(store, configMapResource("ghost"))
	if emitted {
		t.Errorf("a resource absent from Git should emit no action, got %+v", action)
	}
}

// TestPlanDelete_EncryptedStillDeletes: deletion is content-agnostic, so an encrypted
// (non-patchable) document is still removed when its resource leaves the cluster.
// Editability gates patches, not removals.
func TestPlanDelete_EncryptedStillDeletes(t *testing.T) {
	const encConfigMap = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n" +
		"  name: enc\n  namespace: default\nsops:\n  version: \"3\"\n"
	fsys := fstest.MapFS{"cm.sops.yaml": {Data: []byte(encConfigMap)}}
	store := BuildStore(context.Background(), fsys, mapping.NewStaticSnapshotMapper(sampleClusterSnapshot()))

	dm := store.FilesByPath["cm.sops.yaml"].Documents[0]
	if dm.Editable || dm.Cause.Kind != CauseEncrypted {
		t.Fatalf("fixture should be an encrypted, non-editable ConfigMap, got editable=%v cause=%+v",
			dm.Editable, dm.Cause)
	}

	action, emitted := PlanDelete(store, configMapResource("enc"))
	if !emitted {
		t.Fatalf("an encrypted resource should still delete")
	}
	if action.Kind != PlanDeleteDocument {
		t.Errorf("kind = %q, want delete-document even for an encrypted document", action.Kind)
	}
}

func TestPlanDelete_StructureOnlyStoreHasNoDeleteTarget(t *testing.T) {
	store := BuildStore(context.Background(), planFS(), nil)
	if len(store.ByResourceIdentity) != 0 {
		t.Fatalf("structure-only store should have an empty resource index, got %d", len(store.ByResourceIdentity))
	}

	_, emitted := PlanDelete(store, deployResource())
	if emitted {
		t.Errorf("a store without resource inventory must not emit a delete")
	}
}

// TestPlanDelete_DuplicateSuppressed: a collided manifest identity refuses the whole
// GitTarget at acceptance, so a steady-state delete for it produces no action — deleting
// one arbitrary copy of an ambiguous identity is exactly what the design refuses to do.
func TestPlanDelete_DuplicateSuppressed(t *testing.T) {
	fsys := fstest.MapFS{
		"deploy.yaml": {Data: []byte(deployYAML)},
		"dup.yaml":    {Data: []byte(deployYAML)},
	}
	store := BuildStore(context.Background(), fsys, mapping.NewStaticSnapshotMapper(sampleClusterSnapshot()))

	_, emitted := PlanDelete(store, deployResource())
	if emitted {
		t.Errorf("a collided identity must produce no delete action")
	}
}
