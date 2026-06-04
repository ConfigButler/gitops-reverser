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
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// TestBuildStore_ManagedFilesOnly proves the store carries exactly the managed
// KRM documents the Report projection needs: non-YAML files and YAML files with no
// KRM document never become FileModels.
func TestBuildStore_ManagedFilesOnly(t *testing.T) {
	store := BuildStore(sampleFS())

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
	store := BuildStore(sampleFS())

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
		if dm.Mapping != MappingStructureOnly {
			t.Errorf("structure-only analysis should leave Mapping=%q, got %q", MappingStructureOnly, dm.Mapping)
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

// TestBuildStore_Indexes checks the collapsed manifest-identity index and the
// multi-valued GVK index.
func TestBuildStore_Indexes(t *testing.T) {
	store := BuildStore(sampleFS())

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
