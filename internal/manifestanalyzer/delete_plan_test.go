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

	"k8s.io/apimachinery/pkg/runtime/schema"

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
	action, emitted, err := PlanDelete(context.Background(), store, nil, deployResource())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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

	action, emitted, err := PlanDelete(context.Background(), store, nil, deployResource())
	if err != nil || !emitted {
		t.Fatalf("moved manifest should resolve: emitted=%v err=%v", emitted, err)
	}
	if action.Ref.FilePath != "legacy/foo.yaml" {
		t.Errorf("ref path = %q, want the actual (moved) file legacy/foo.yaml", action.Ref.FilePath)
	}
}

// TestPlanDelete_MultiDocIndex: deleting one document of a multi-document file targets
// the right document index (cm.yaml holds ConfigMap a at #0 and b at #1).
func TestPlanDelete_MultiDocIndex(t *testing.T) {
	store := planStore(t)
	action, emitted, err := PlanDelete(context.Background(), store, nil, configMapResource("b"))
	if err != nil || !emitted {
		t.Fatalf("ConfigMap b should resolve: emitted=%v err=%v", emitted, err)
	}
	if action.Ref != (RecordRef{FilePath: "cm.yaml", DocumentIndex: 1}) {
		t.Errorf("ref = %+v, want cm.yaml#1", action.Ref)
	}
}

// TestPlanDelete_NotInGit: a delete for a resource Git never materialised is a no-op —
// no action, no error. The cluster dropped something we do not track; already converged.
func TestPlanDelete_NotInGit(t *testing.T) {
	store := planStore(t)
	action, emitted, err := PlanDelete(context.Background(), store, nil, configMapResource("ghost"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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

	action, emitted, err := PlanDelete(context.Background(), store, nil, configMapResource("enc"))
	if err != nil || !emitted {
		t.Fatalf("an encrypted resource should still delete: emitted=%v err=%v", emitted, err)
	}
	if action.Kind != PlanDeleteDocument {
		t.Errorf("kind = %q, want delete-document even for an encrypted document", action.Kind)
	}
}

// TestPlanDelete_FallbackByManifestIdentity exercises the reverse-map fallback: a
// structure-only store leaves ByResourceIdentity empty, so resolution falls back to the
// mapper's GVR->GVK reverse map and matches by manifest identity instead.
func TestPlanDelete_FallbackByManifestIdentity(t *testing.T) {
	store := BuildStore(context.Background(), planFS(), nil) // structure-only: no resource index
	if len(store.ByResourceIdentity) != 0 {
		t.Fatalf("structure-only store should have an empty resource index, got %d", len(store.ByResourceIdentity))
	}

	mapper := mapping.NewStaticSnapshotMapper(sampleClusterSnapshot())
	action, emitted, err := PlanDelete(context.Background(), store, mapper, deployResource())
	if err != nil || !emitted {
		t.Fatalf("fallback should resolve via the mapper: emitted=%v err=%v", emitted, err)
	}
	if action.Ref != (RecordRef{FilePath: "deploy.yaml", DocumentIndex: 0}) {
		t.Errorf("fallback ref = %+v, want deploy.yaml#0", action.Ref)
	}
}

// TestPlanDelete_FallbackUnservedIsNoOp: when the resource index misses and the event
// GVR does not reverse-map to a served GVK, there is no manifest identity we can trust,
// so resolution yields "no managed document" — not a guess and not an error.
func TestPlanDelete_FallbackUnservedIsNoOp(t *testing.T) {
	store := BuildStore(context.Background(), planFS(), nil) // structure-only forces the fallback

	mapper := mapping.NewStaticSnapshotMapper(sampleClusterSnapshot())
	// apps/v1/statefulsets is not in the snapshot, so GVKForGVR returns Unserved.
	unserved := types.NewResourceIdentifier("apps", "v1", "statefulsets", "default", "web")
	_, emitted, err := PlanDelete(context.Background(), store, mapper, unserved)
	if err != nil {
		t.Fatalf("an unserved reverse-map is an expected outcome, not an error: %v", err)
	}
	if emitted {
		t.Errorf("an unservable delete GVR should resolve to no action")
	}
}

// TestPlanDelete_FallbackUnobservableFailsClosed: when the resource index misses and the
// fallback reverse-map cannot OBSERVE the API surface (CatalogUnavailable /
// DiscoveryDegraded), PlanDelete fails closed — it returns an error so M7 holds and
// retries, rather than silently dropping the delete and leaving a stale manifest. The
// contrast with TestPlanDelete_FallbackUnservedIsNoOp (trusted absence -> no-op) is the
// whole point: only an UNOBSERVABLE surface fails closed, a TRUSTED "no served GVK" does not.
func TestPlanDelete_FallbackUnobservableFailsClosed(t *testing.T) {
	store := BuildStore(context.Background(), planFS(), nil) // structure-only forces the fallback

	cases := []struct {
		name     string
		snapshot mapping.Snapshot
	}{
		{name: "catalog unavailable", snapshot: mapping.Snapshot{NotReady: true}},
		{name: "discovery degraded", snapshot: mapping.Snapshot{
			Generation:            1,
			DegradedGroupVersions: []schema.GroupVersion{{Group: "apps", Version: "v1"}},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mapper := mapping.NewStaticSnapshotMapper(c.snapshot)
			_, emitted, err := PlanDelete(context.Background(), store, mapper, deployResource())
			if err == nil {
				t.Fatalf("an unobservable API surface must fail closed (error), got nil")
			}
			if emitted {
				t.Errorf("no delete action may be emitted when the surface is unobservable")
			}
		})
	}
}

// TestPlanDelete_MapperErrorFailsClosed: when the resource index misses and the mapper
// returns a Go error (a cancelled context here), PlanDelete fails closed — it returns
// the error and emits nothing. An unobservable API surface is never evidence to delete.
func TestPlanDelete_MapperErrorFailsClosed(t *testing.T) {
	store := BuildStore(context.Background(), planFS(), nil) // structure-only forces the fallback

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mapper := mapping.NewStaticSnapshotMapper(sampleClusterSnapshot())
	_, emitted, err := PlanDelete(ctx, store, mapper, deployResource())
	if err == nil {
		t.Fatalf("a mapper error must surface (fail closed), got nil")
	}
	if emitted {
		t.Errorf("no delete action may be emitted when resolution failed closed")
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

	_, emitted, err := PlanDelete(context.Background(), store, nil, deployResource())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if emitted {
		t.Errorf("a collided identity must produce no delete action")
	}
}
