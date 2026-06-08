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
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// structureOnlyStore builds the canonical sample tree with no mapper (structure-only).
func structureOnlyStore() *ManifestStore {
	return BuildStore(context.Background(), sampleFS(), nil)
}

// TestBuildStore_ManagedFilesOnly proves the store carries exactly the managed
// KRM documents the Report projection needs: non-YAML files and YAML files with no
// KRM document never become FileModels.
func TestBuildStore_ManagedFilesOnly(t *testing.T) {
	store := structureOnlyStore()

	// plain.yaml (non-KRM), broken.yaml (invalid), empty.yaml (empty), and
	// docs/notes.txt (non-yaml) hold no managed document, so they are absent.
	managed := []string{"cm.yaml", "deploy.yaml", "dup.yaml", "secret.sops.yaml"}
	if len(store.FilesByPath) != len(managed) {
		t.Fatalf("managed files = %d, want %d: %v", len(store.FilesByPath), len(managed), keysOf(store.FilesByPath))
	}
	for _, p := range managed {
		if store.FilesByPath[p] == nil {
			t.Errorf("expected managed file %q in store", p)
		}
	}
	for _, p := range []string{"plain.yaml", "broken.yaml", "empty.yaml", "docs/notes.txt"} {
		if store.FilesByPath[p] != nil {
			t.Errorf("unmanaged file %q should not be a FileModel", p)
		}
	}
}

// TestBuildStore_DocumentDetail checks the per-document classification carried by
// the store: multi-document order, encryption, and the duplicate loser.
func TestBuildStore_DocumentDetail(t *testing.T) {
	store := structureOnlyStore()

	cm := store.FilesByPath["cm.yaml"]
	if len(cm.Documents) != 2 {
		t.Fatalf("cm.yaml documents = %d, want 2", len(cm.Documents))
	}
	if cm.Documents[0].ManifestIdentity.Name != "a" || cm.Documents[1].ManifestIdentity.Name != "b" {
		t.Errorf(
			"cm.yaml doc order = %q,%q",
			cm.Documents[0].ManifestIdentity.Name,
			cm.Documents[1].ManifestIdentity.Name,
		)
	}
	for _, dm := range cm.Documents {
		if dm.Mapping != MappingNoSource {
			t.Errorf(
				"structure-only analysis should leave Mapping=%v, got %v",
				MappingNoSource,
				dm.Mapping,
			)
		}
		if dm.ResourceIdentity != nil {
			t.Errorf("structure-only analysis should leave ResourceIdentity nil, got %+v", dm.ResourceIdentity)
		}
	}

	if secret := store.FilesByPath["secret.sops.yaml"]; secret.Documents[0].Cause.Kind != CauseEncrypted {
		t.Errorf("secret.sops.yaml document should carry an encrypted cause, got %+v", secret.Documents[0].Cause)
	}
	if dup := store.FilesByPath["dup.yaml"]; !store.IsDuplicate(dup.Documents[0]) {
		t.Errorf("dup.yaml document should be the duplicate loser")
	}
}

// TestBuildStore_EncryptedDuplicate covers the edge the duplicate collapse must
// match manifestedit on: two encrypted documents with the same identity. The first
// occurrence wins; the second is a duplicate even though encrypted documents are
// Editable=false (not patchable in place).
func TestBuildStore_EncryptedDuplicate(t *testing.T) {
	store := BuildStore(context.Background(), fstest.MapFS{
		"a.sops.yaml": {Data: []byte(sopsSecretYAML)},
		"b.sops.yaml": {Data: []byte(sopsSecretYAML)},
	}, nil)
	a := store.FilesByPath["a.sops.yaml"].Documents[0]
	b := store.FilesByPath["b.sops.yaml"].Documents[0]

	if a.Editable || b.Editable {
		t.Fatalf("encrypted documents should be Editable=false")
	}
	if store.IsDuplicate(a) {
		t.Errorf("first encrypted occurrence should win the identity, not be a duplicate")
	}
	if !store.IsDuplicate(b) {
		t.Errorf("second encrypted occurrence should be detected as a duplicate")
	}
}

// TestBuildStore_Indexes checks the collapsed manifest-identity index and the
// multi-valued GVK index.
func TestBuildStore_Indexes(t *testing.T) {
	store := structureOnlyStore()

	// The Deployment appears in deploy.yaml and dup.yaml; the index collapses to the
	// first occurrence (deploy.yaml sorts first) and dup.yaml's copy is the loser.
	dep := manifestedit.Identity{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "default", Name: "web"}
	winner := store.ByManifestIdentity[dep]
	if winner == nil {
		t.Fatalf("Deployment identity missing from ByManifestIdentity")
	}
	if winner != store.FilesByPath["deploy.yaml"].Documents[0] {
		t.Errorf("ByManifestIdentity winner should be deploy.yaml's document")
	}

	// ByGVK groups the two Deployments (winner + loser) under one key.
	gvk := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	if got := len(store.ByGVK[gvk]); got != 2 {
		t.Errorf("ByGVK[%s] = %d documents, want 2", gvk, got)
	}

	// Structure-only analysis resolves no resource identities.
	if len(store.ByResourceIdentity) != 0 {
		t.Errorf("structure-only ByResourceIdentity should be empty, got %d", len(store.ByResourceIdentity))
	}
}

// sampleClusterSnapshot is a ready static snapshot that matches sampleFS: apps/v1
// Deployment and core v1 ConfigMap are served and allowed; core v1 Secret is served
// but excluded by policy, so it must resolve to Disallowed rather than Resolved.
func sampleClusterSnapshot() typeset.Snapshot {
	return typeset.Snapshot{
		Generation: 1,
		Entries: []typeset.Entry{
			{
				GVK:        schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				GVR:        schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
				Namespaced: true,
				Allowed:    true,
			},
			{
				GVK:        schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
				GVR:        schema.GroupVersionResource{Version: "v1", Resource: "configmaps"},
				Namespaced: true,
				Allowed:    true,
			},
			{
				GVK:        schema.GroupVersionKind{Version: "v1", Kind: "Secret"},
				GVR:        schema.GroupVersionResource{Version: "v1", Resource: "secrets"},
				Namespaced: true,
				Allowed:    false,
			},
		},
	}
}

// TestBuildStore_StaticSnapshotMapper is the B3 milestone check: with a
// static-snapshot mapper, resolved documents carry a ResourceIdentity + Resolved
// status and populate ByResourceIdentity, while a disallowed kind stays unresolved
// and surfaces an unresolved-mapping diagnostic.
func TestBuildStore_StaticSnapshotMapper(t *testing.T) {
	mapper := typeset.NewSnapshotRegistry(sampleClusterSnapshot())
	store := BuildStore(context.Background(), sampleFS(), mapper)

	// The Deployment resolves to apps/v1/deployments and carries both identities.
	dep := store.FilesByPath["deploy.yaml"].Documents[0]
	if dep.Mapping != MappingFollowable {
		t.Fatalf("deploy.yaml Mapping = %q, want Resolved", dep.Mapping)
	}
	wantRI := types.NewResourceIdentifier("apps", "v1", "deployments", "default", "web")
	if dep.ResourceIdentity == nil || *dep.ResourceIdentity != wantRI {
		t.Fatalf("deploy.yaml ResourceIdentity = %+v, want %+v", dep.ResourceIdentity, wantRI)
	}
	if store.ByResourceIdentity[wantRI] != dep {
		t.Errorf("ByResourceIdentity[%s] should point at deploy.yaml's document", wantRI.Key())
	}

	// Both ConfigMaps resolve; the resource index collapses on the same winners as
	// the manifest-identity index, so the three resolved winners are all present
	// (Deployment web, ConfigMap a, ConfigMap b) and the disallowed Secret is not.
	if got := len(store.ByResourceIdentity); got != 3 {
		t.Errorf("ByResourceIdentity = %d resolved winners, want 3", got)
	}

	// The Secret is served but policy-denied: no ResourceIdentity, not followable.
	secret := store.FilesByPath["secret.sops.yaml"].Documents[0]
	if secret.Mapping != MappingNotFollowable {
		t.Errorf("secret Mapping = %v, want NotFollowable", secret.Mapping)
	}
	if secret.ResourceIdentity != nil {
		t.Errorf("disallowed secret should have no ResourceIdentity, got %+v", secret.ResourceIdentity)
	}

	// Exactly one unresolved-mapping diagnostic (the disallowed Secret).
	var unresolved int
	for _, d := range store.Diagnostics {
		if d.Reason == reasonUnresolvedMapping {
			unresolved++
			if d.Path != "secret.sops.yaml" {
				t.Errorf("unresolved-mapping diagnostic on %q, want secret.sops.yaml", d.Path)
			}
		}
	}
	if unresolved != 1 {
		t.Errorf("unresolved-mapping diagnostics = %d, want 1", unresolved)
	}
}

// TestBuildStore_DuplicateLoserResolves proves mapping is per-document: the
// duplicate Deployment in dup.yaml still resolves to a ResourceIdentity, but it lost
// the first-occurrence contest, so it is the IsDuplicate loser and never the
// ByResourceIdentity winner (deploy.yaml's document holds that slot).
func TestBuildStore_DuplicateLoserResolves(t *testing.T) {
	store := BuildStore(context.Background(), sampleFS(), typeset.NewSnapshotRegistry(sampleClusterSnapshot()))

	wantRI := types.NewResourceIdentifier("apps", "v1", "deployments", "default", "web")
	dupDep := store.FilesByPath["dup.yaml"].Documents[0]
	if dupDep.Mapping != MappingFollowable || dupDep.ResourceIdentity == nil {
		t.Fatalf(
			"dup.yaml Deployment should still resolve, got Mapping=%q RI=%+v",
			dupDep.Mapping,
			dupDep.ResourceIdentity,
		)
	}
	if !store.IsDuplicate(dupDep) {
		t.Errorf("dup.yaml Deployment should be the duplicate loser")
	}
	if store.ByResourceIdentity[wantRI] == dupDep {
		t.Errorf("ByResourceIdentity winner should be deploy.yaml's document, not dup.yaml's")
	}
}

// TestBuildStore_ClusterScopedResolution covers the scope-correct ResourceIdentity:
// a cluster-scoped resource is keyed with no namespace, and a manifest that
// accidentally carries metadata.namespace has it dropped for indexing plus a
// scope-mismatch diagnostic — never indexed under the wrong, namespaced key.
func TestBuildStore_ClusterScopedResolution(t *testing.T) {
	snap := typeset.Snapshot{
		Generation: 1,
		Entries: []typeset.Entry{
			{
				GVK: schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"},
				GVR: schema.GroupVersionResource{
					Group:    "rbac.authorization.k8s.io",
					Version:  "v1",
					Resource: "clusterroles",
				},
				Namespaced: false,
				Allowed:    true,
			},
		},
	}
	const clusterRoleYAML = "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: viewer\n"
	const clusterRoleWithNS = "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\n" +
		"metadata:\n  name: editor\n  namespace: oops\n"

	store := BuildStore(context.Background(), fstest.MapFS{
		"cr.yaml":    {Data: []byte(clusterRoleYAML)},
		"cr-ns.yaml": {Data: []byte(clusterRoleWithNS)},
	}, typeset.NewSnapshotRegistry(snap))

	// Clean cluster-scoped: indexed with an empty namespace, no scope diagnostic.
	clean := store.FilesByPath["cr.yaml"].Documents[0]
	wantClean := types.NewResourceIdentifier("rbac.authorization.k8s.io", "v1", "clusterroles", "", "viewer")
	if clean.ResourceIdentity == nil || *clean.ResourceIdentity != wantClean {
		t.Fatalf("clean ClusterRole RI = %+v, want %+v", clean.ResourceIdentity, wantClean)
	}
	if store.ByResourceIdentity[wantClean] != clean {
		t.Errorf("ByResourceIdentity should key the clean ClusterRole under an empty namespace")
	}

	// Accidental namespace: dropped for indexing (so it is NOT keyed under "oops"),
	// and exactly one scope-mismatch diagnostic is emitted, naming the file.
	dirty := store.FilesByPath["cr-ns.yaml"].Documents[0]
	wantDirty := types.NewResourceIdentifier("rbac.authorization.k8s.io", "v1", "clusterroles", "", "editor")
	if dirty.ResourceIdentity == nil || *dirty.ResourceIdentity != wantDirty {
		t.Fatalf(
			"accidentally-namespaced ClusterRole RI = %+v, want %+v (namespace dropped)",
			dirty.ResourceIdentity,
			wantDirty,
		)
	}
	var scopeMismatch int
	for _, d := range store.Diagnostics {
		if d.Reason == reasonScopeMismatch {
			scopeMismatch++
			if d.Path != "cr-ns.yaml" {
				t.Errorf("scope-mismatch diagnostic on %q, want cr-ns.yaml", d.Path)
			}
		}
	}
	if scopeMismatch != 1 {
		t.Errorf("scope-mismatch diagnostics = %d, want 1", scopeMismatch)
	}
}

// TestBuildStore_CancelledContextIsNoSource covers a cancelled build context: every
// document is recorded as MappingNoSource (no API source was consulted) — like
// structure-only analysis — resolves no identity, and emits no mapping diagnostic.
func TestBuildStore_CancelledContextIsNoSource(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	store := BuildStore(ctx, sampleFS(), typeset.NewSnapshotRegistry(sampleClusterSnapshot()))

	for path, fm := range store.FilesByPath {
		for _, dm := range fm.Documents {
			if dm.Mapping != MappingNoSource {
				t.Errorf("%s: Mapping = %v, want NoSource after cancel", path, dm.Mapping)
			}
			if dm.ResourceIdentity != nil {
				t.Errorf("%s: cancelled lookup should resolve no identity, got %+v", path, dm.ResourceIdentity)
			}
		}
	}
	if len(store.ByResourceIdentity) != 0 {
		t.Errorf("cancelled lookups should populate no resource index, got %d", len(store.ByResourceIdentity))
	}
	for _, d := range store.Diagnostics {
		if d.Reason == reasonUnresolvedMapping {
			t.Errorf("a cancelled (no-source) build should emit no mapping diagnostics, got %+v", d)
		}
	}
}

// TestBuildStore_NotFollowableDiagnoses proves the store records a not-followable
// outcome for every kind the registry refuses (unserved, denied, ambiguous,
// subresource-only, degraded) and emits exactly one unresolved-mapping diagnostic,
// resolving no identity. An unready registry is the no-source case: it judges
// nothing and emits no diagnostic.
func TestBuildStore_NotFollowableDiagnoses(t *testing.T) {
	const widgetYAML = "apiVersion: example.com/v1\nkind: Widget\nmetadata:\n  name: w\n  namespace: default\n"
	widgetGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}
	widgetGVR := schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"}
	widgetGV := schema.GroupVersion{Group: "example.com", Version: "v1"}
	allowed := typeset.Entry{GVK: widgetGVK, GVR: widgetGVR, Namespaced: true, Allowed: true}

	cases := []struct {
		name      string
		snap      typeset.Snapshot
		want      MappingOutcome
		wantDiags int
	}{
		{"unserved", typeset.Snapshot{}, MappingNotFollowable, 1},
		{
			"denied",
			typeset.Snapshot{
				Entries: []typeset.Entry{{GVK: widgetGVK, GVR: widgetGVR, Namespaced: true, Allowed: false}},
			},
			MappingNotFollowable, 1,
		},
		{
			"ambiguous",
			typeset.Snapshot{Entries: []typeset.Entry{allowed, {
				GVK:        widgetGVK,
				GVR:        schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgetz"},
				Namespaced: true,
				Allowed:    true,
			}}},
			MappingNotFollowable, 1,
		},
		{
			"subresource-only",
			typeset.Snapshot{Entries: []typeset.Entry{
				{
					GVK: widgetGVK,
					GVR: schema.GroupVersionResource{
						Group:    "example.com",
						Version:  "v1",
						Resource: "widgets/status",
					},
					Subresource: true,
					Allowed:     true,
				},
			}},
			MappingNotFollowable, 1,
		},
		{
			"degraded",
			typeset.Snapshot{DegradedGroupVersions: []schema.GroupVersion{widgetGV}},
			MappingNotFollowable, 1,
		},
		{"no-source", typeset.Snapshot{NotReady: true}, MappingNoSource, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := BuildStore(
				context.Background(),
				fstest.MapFS{"w.yaml": {Data: []byte(widgetYAML)}},
				typeset.NewSnapshotRegistry(c.snap),
			)
			dm := store.FilesByPath["w.yaml"].Documents[0]
			if dm.Mapping != c.want {
				t.Errorf("Mapping = %v, want %v", dm.Mapping, c.want)
			}
			if dm.ResourceIdentity != nil {
				t.Errorf("a refused kind should leave ResourceIdentity nil, got %+v", dm.ResourceIdentity)
			}
			var diags int
			for _, d := range store.Diagnostics {
				if d.Reason == reasonUnresolvedMapping {
					diags++
				}
			}
			if diags != c.wantDiags {
				t.Errorf("unresolved-mapping diagnostics = %d, want %d", diags, c.wantDiags)
			}
		})
	}
}

// TestFileModel_DirtyDeleted exercises the derived byte-state machine.
func TestFileModel_DirtyDeleted(t *testing.T) {
	cases := []struct {
		name             string
		orig, cur        []byte
		dirty, isDeleted bool
	}{
		{"resident", nil, nil, false, false},
		{"new file", nil, []byte("x"), true, false},
		{"deleted", []byte("x"), nil, false, true},
		{"changed", []byte("x"), []byte("y"), true, false},
		{"unchanged", []byte("x"), []byte("x"), false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &FileModel{Original: c.orig, Current: c.cur}
			if f.Dirty() != c.dirty || f.Deleted() != c.isDeleted {
				t.Errorf("Dirty=%v Deleted=%v, want %v/%v", f.Dirty(), f.Deleted(), c.dirty, c.isDeleted)
			}
		})
	}
}

func keysOf(m map[string]*FileModel) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
