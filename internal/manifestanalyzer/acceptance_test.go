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

	"github.com/ConfigButler/gitops-reverser/internal/mapping"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

const (
	plainSecretYAML = "apiVersion: v1\nkind: Secret\nmetadata:\n  name: db\n  namespace: default\n"
	configMapCYAML  = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: c\n  namespace: default\n"
	widgetYAMLDoc   = "apiVersion: example.com/v1\nkind: Widget\nmetadata:\n  name: w\n  namespace: default\n"
	kustomizationY  = "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - deploy.yaml\n"
)

// snapMapper is the ready static snapshot mapper used across acceptance tests.
func snapMapper() mapping.ResourceMapper {
	return mapping.NewStaticSnapshotMapper(sampleClusterSnapshot())
}

// acceptanceOf builds a store with the given allowlist and runs the gate.
func acceptanceOf(
	t *testing.T,
	fsys fstest.MapFS,
	mapper mapping.ResourceMapper,
	policy AcceptancePolicy,
) (*ManifestStore, Acceptance) {
	t.Helper()
	store := buildStoreFS(context.Background(), fsys, mapper, policy.Allowlist)
	return store, Accept(store, policy)
}

// countAcceptance returns the number of issues of a kind.
func countAcceptance(acc Acceptance, kind IssueKind) int {
	n := 0
	for _, is := range acc.Issues {
		if is.Kind == kind {
			n++
		}
	}
	return n
}

// onlyIssue asserts the gate refused with exactly one issue of the expected kind at
// the expected path/index, and returns it.
func onlyIssue(t *testing.T, acc Acceptance, kind IssueKind, path string, index int) AcceptanceIssue {
	t.Helper()
	if acc.Accepted {
		t.Fatalf("expected refusal, got accepted")
	}
	if len(acc.Issues) != 1 {
		t.Fatalf("want exactly one issue, got %+v", acc.Issues)
	}
	is := acc.Issues[0]
	if is.Kind != kind || is.Path != path || is.DocumentIndex != index {
		t.Fatalf("issue = %+v, want kind=%s path=%s#%d", is, kind, path, index)
	}
	return is
}

func TestAccept_CleanFolderPasses(t *testing.T) {
	fsys := fstest.MapFS{
		"deploy.yaml": {Data: []byte(deployYAML)},
		"cm.yaml":     {Data: []byte(configMapsYAML)},
	}
	_, acc := acceptanceOf(t, fsys, snapMapper(), AcceptancePolicy{})
	if !acc.Accepted || len(acc.Issues) != 0 {
		t.Fatalf("clean folder should pass: accepted=%v issues=%+v", acc.Accepted, acc.Issues)
	}
}

func TestAccept_DuplicateRefuses(t *testing.T) {
	fsys := fstest.MapFS{
		"deploy.yaml": {Data: []byte(deployYAML)},
		"dup.yaml":    {Data: []byte(deployYAML)},
	}
	_, acc := acceptanceOf(t, fsys, nil, AcceptancePolicy{})
	is := onlyIssue(t, acc, IssueDuplicate, "dup.yaml", 0)
	if is.Message == "" {
		t.Errorf("duplicate refusal should carry a message naming the winner")
	}
}

func TestAccept_ImpureManagedFileEmptyDocument(t *testing.T) {
	// A managed file with an empty document interspersed between two managed
	// documents: refused, which is exactly what lets the store drop the index.
	impure := deployYAML + "---\n# only a comment\n---\n" + configMapCYAML
	fsys := fstest.MapFS{"app.yaml": {Data: []byte(impure)}}
	_, acc := acceptanceOf(t, fsys, nil, AcceptancePolicy{})
	onlyIssue(t, acc, IssueImpureManagedFile, "app.yaml", 1)
}

func TestAccept_ImpureManagedFileNonKRM(t *testing.T) {
	// A non-KRM passenger document in a managed file is impure, not bucket-2 non-KRM.
	impure := deployYAML + "---\njust: data\n"
	fsys := fstest.MapFS{"app.yaml": {Data: []byte(impure)}}
	_, acc := acceptanceOf(t, fsys, nil, AcceptancePolicy{})
	onlyIssue(t, acc, IssueImpureManagedFile, "app.yaml", 1)
}

func TestAccept_StandaloneNonKRMRefuses(t *testing.T) {
	fsys := fstest.MapFS{"values.yaml": {Data: []byte(plainYAML)}}
	_, acc := acceptanceOf(t, fsys, nil, AcceptancePolicy{})
	onlyIssue(t, acc, IssueNonKRM, "values.yaml", 0)
}

func TestAccept_StandaloneInvalidRefuses(t *testing.T) {
	fsys := fstest.MapFS{"broken.yaml": {Data: []byte(brokenYAML)}}
	_, acc := acceptanceOf(t, fsys, nil, AcceptancePolicy{})
	is := onlyIssue(t, acc, IssueInvalidYAML, "broken.yaml", 0)
	if is.Message == "" {
		t.Errorf("invalid-yaml refusal should carry the parse error detail")
	}
}

func TestAccept_StandaloneEmptyIgnored(t *testing.T) {
	fsys := fstest.MapFS{"empty.yaml": {Data: []byte(emptyYAML)}}
	_, acc := acceptanceOf(t, fsys, nil, AcceptancePolicy{})
	if !acc.Accepted || len(acc.Issues) != 0 {
		t.Fatalf("a standalone empty document is ignored, got %+v", acc.Issues)
	}
}

func TestAccept_UnwatchedAPIKRMRefuses(t *testing.T) {
	// A Secret is served but disallowed by the sample snapshot's resource policy:
	// unwatched API-backed KRM, refused rather than pruned.
	fsys := fstest.MapFS{"secret.yaml": {Data: []byte(plainSecretYAML)}}
	_, acc := acceptanceOf(t, fsys, snapMapper(), AcceptancePolicy{})
	onlyIssue(t, acc, IssueUnwatchedAPIKRM, "secret.yaml", 0)
}

func TestAccept_UnresolvedKRMRefuses(t *testing.T) {
	// An unserved kind (no snapshot entry) is recognised KRM the mapper cannot tie to
	// a watched resource, and is not allowlisted.
	fsys := fstest.MapFS{"w.yaml": {Data: []byte(widgetYAMLDoc)}}
	mapper := mapping.NewStaticSnapshotMapper(mapping.Snapshot{Generation: 1})
	_, acc := acceptanceOf(t, fsys, mapper, AcceptancePolicy{})
	onlyIssue(t, acc, IssueUnresolvedKRM, "w.yaml", 0)
}

func TestAccept_OutOfScopeRefuses(t *testing.T) {
	fsys := fstest.MapFS{"deploy.yaml": {Data: []byte(deployYAML)}}
	policy := AcceptancePolicy{
		InScope: func(ri types.ResourceIdentifier) bool { return ri.Namespace == "kube-system" },
	}
	_, acc := acceptanceOf(t, fsys, snapMapper(), policy)
	onlyIssue(t, acc, IssueOutOfScope, "deploy.yaml", 0)
}

func TestAccept_StructureOnlySkipsMappingChecks(t *testing.T) {
	// Structure-only: a Secret cannot be judged unwatched without an API source, so
	// the mapping refusals are skipped and the folder passes.
	fsys := fstest.MapFS{"secret.yaml": {Data: []byte(plainSecretYAML)}}
	_, acc := acceptanceOf(t, fsys, nil, AcceptancePolicy{})
	if !acc.Accepted {
		t.Fatalf("structure-only should not refuse on mapping grounds, got %+v", acc.Issues)
	}
}

func TestAccept_AllowlistedFileRetained(t *testing.T) {
	fsys := fstest.MapFS{
		"kustomization.yaml": {Data: []byte(kustomizationY)},
		"deploy.yaml":        {Data: []byte(deployYAML)},
	}
	store, acc := acceptanceOf(t, fsys, snapMapper(), AcceptancePolicy{Allowlist: DefaultAllowlist()})

	if store.FilesByPath["kustomization.yaml"] != nil {
		t.Errorf("allowlisted file must never enter FilesByPath")
	}
	if !acc.Accepted {
		t.Fatalf("allowlisted build directive beside a clean resource should pass, got %+v", acc.Issues)
	}
	if len(acc.Retained) != 1 || acc.Retained[0].Location.Path != "kustomization.yaml" {
		t.Fatalf("retained = %+v, want one whole-file kustomization.yaml entry", acc.Retained)
	}
	if acc.Retained[0].Identity.Name != "" {
		t.Errorf("a nameless build directive should be a whole-file retention, got %+v", acc.Retained[0])
	}
}

func TestAccept_MixedManagedInAllowlistedFileRefuses(t *testing.T) {
	// A named Deployment hiding inside kustomization.yaml: refused, never silently
	// un-managed.
	mixed := kustomizationY + "---\n" + deployYAML
	fsys := fstest.MapFS{"kustomization.yaml": {Data: []byte(mixed)}}
	store, acc := acceptanceOf(t, fsys, snapMapper(), AcceptancePolicy{Allowlist: DefaultAllowlist()})

	if store.FilesByPath["kustomization.yaml"] != nil {
		t.Errorf("an allowlisted file is never materialised, even with a managed passenger")
	}
	if countAcceptance(acc, IssueMixedFile) != 1 {
		t.Fatalf("want one mixed-file refusal, got %+v", acc.Issues)
	}
}

func TestAccept_MultipleRefusalsSorted(t *testing.T) {
	fsys := fstest.MapFS{
		"a-bad.yaml": {Data: []byte(plainYAML)},  // non-KRM
		"b-dup.yaml": {Data: []byte(deployYAML)}, // duplicate winner
		"c-dup.yaml": {Data: []byte(deployYAML)}, // duplicate loser
	}
	_, acc := acceptanceOf(t, fsys, nil, AcceptancePolicy{})
	if acc.Accepted {
		t.Fatalf("expected refusal")
	}
	// Sorted by path: a-bad.yaml (non-krm) before c-dup.yaml (duplicate).
	if acc.Issues[0].Path != "a-bad.yaml" || acc.Issues[len(acc.Issues)-1].Path != "c-dup.yaml" {
		t.Errorf("issues not sorted by path: %+v", acc.Issues)
	}
}

func TestAccept_MultipleRetainedSorted(t *testing.T) {
	fsys := fstest.MapFS{
		"b/kustomization.yaml": {Data: []byte(kustomizationY)},
		"a/kustomization.yaml": {Data: []byte(kustomizationY)},
		"deploy.yaml":          {Data: []byte(deployYAML)},
	}
	store, acc := acceptanceOf(t, fsys, snapMapper(), AcceptancePolicy{Allowlist: DefaultAllowlist()})
	if !acc.Accepted {
		t.Fatalf("two clean build directives beside a resource should pass: %+v", acc.Issues)
	}
	if len(store.Retained) != 2 {
		t.Fatalf("retained = %+v, want two whole-file entries", store.Retained)
	}
	if store.Retained[0].Location.Path != "a/kustomization.yaml" ||
		store.Retained[1].Location.Path != "b/kustomization.yaml" {
		t.Errorf("retained entries should be sorted by path, got %+v", store.Retained)
	}
}

func TestAllowlist(t *testing.T) {
	def := DefaultAllowlist()
	if !def.Allows("kustomization.yaml") || !def.Allows("base/kustomization.yaml") {
		t.Errorf("DefaultAllowlist should match kustomization.yaml by basename")
	}
	if def.Allows("deploy.yaml") || def.Allows("kustomization.yaml.bak") {
		t.Errorf("DefaultAllowlist should not match unrelated files")
	}
	if (Allowlist{}).Allows("kustomization.yaml") {
		t.Errorf("the zero allowlist should allow nothing")
	}
	if NewAllowlist().Allows("kustomization.yaml") {
		t.Errorf("an empty NewAllowlist should allow nothing")
	}
}
