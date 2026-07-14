// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// hardKustomizationY is a kustomization.yaml that uses an unsupported feature (a patches
// block): the operator cannot map its output back to editable source documents.
const hardKustomizationY = "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
	"kind: Kustomization\n" +
	"resources:\n  - deploy.yaml\n" +
	"patches:\n  - path: patch.yaml\n"

const (
	plainSecretYAML = "apiVersion: v1\nkind: Secret\nmetadata:\n  name: db\n  namespace: default\n"
	configMapCYAML  = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: c\n  namespace: default\n"
	widgetYAMLDoc   = "apiVersion: example.com/v1\nkind: Widget\nmetadata:\n  name: w\n  namespace: default\n"
	kustomizationY  = "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - deploy.yaml\n"
)

// snapMapper is the ready static snapshot mapper used across acceptance tests.
func snapMapper() typeset.Lookup {
	return typeset.NewSnapshotRegistry(sampleClusterSnapshot())
}

// acceptanceOf builds a store with the given allowlist and runs the gate.
func acceptanceOf(
	t *testing.T,
	fsys fs.FS,
	mapper typeset.Lookup,
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

func TestAccept_PolicyDeniedKRMRefuses(t *testing.T) {
	// A Secret is served but denied by the sample snapshot's resource policy, so it is
	// not followable and the folder is refused (never pruned).
	fsys := fstest.MapFS{"secret.yaml": {Data: []byte(plainSecretYAML)}}
	_, acc := acceptanceOf(t, fsys, snapMapper(), AcceptancePolicy{})
	onlyIssue(t, acc, IssueUnresolvedKRM, "secret.yaml", 0)
}

func TestAccept_UnresolvedKRMRefuses(t *testing.T) {
	// An unserved kind (no snapshot entry) is recognised KRM the mapper cannot tie to
	// a watched resource, and is not allowlisted.
	fsys := fstest.MapFS{"w.yaml": {Data: []byte(widgetYAMLDoc)}}
	mapper := typeset.NewSnapshotRegistry(typeset.Snapshot{Generation: 1})
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
	// Both kustomizations must actually BUILD: a render root whose resources: entry
	// does not resolve is refused now (kustomize cannot build it, so neither can
	// Flux), where the old structural parse simply never looked.
	sharedKust := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - ../deploy.yaml\n"
	fsys := fstest.MapFS{
		"b/kustomization.yaml": {Data: []byte(sharedKust)},
		"a/kustomization.yaml": {Data: []byte(sharedKust)},
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

func TestAccept_MappingRefusalIndexAfterGap(t *testing.T) {
	// An empty document at file index 0, then a denied Secret at file index 1. The
	// Secret is the only managed record (loop index 0), but its mapping refusal must
	// name its TRUE file index 1 — proving mappingRefusals uses reconstructed
	// positions, not the loop index.
	src := "# only a comment\n---\n" + plainSecretYAML
	fsys := fstest.MapFS{"app.yaml": {Data: []byte(src)}}
	_, acc := acceptanceOf(t, fsys, snapMapper(), AcceptancePolicy{})

	if acc.Accepted {
		t.Fatalf("expected refusal")
	}
	var unresolved *AcceptanceIssue
	for i := range acc.Issues {
		if acc.Issues[i].Kind == IssueUnresolvedKRM {
			unresolved = &acc.Issues[i]
		}
	}
	if unresolved == nil {
		t.Fatalf("expected an unresolved-krm refusal, got %+v", acc.Issues)
	}
	if unresolved.DocumentIndex != 1 {
		t.Errorf("unresolved refusal should carry the true file index 1, got %d", unresolved.DocumentIndex)
	}
	// The empty document also makes the file impure, named at its own true index 0.
	if countAcceptance(acc, IssueImpureManagedFile) != 1 {
		t.Errorf("the empty document should also make the file impure, got %+v", acc.Issues)
	}
}

// TestReconstructManagedIndices_RecordlessGapsLeaveDiagnostics makes the
// reconstruction's load-bearing invariant explicit: every record-less document kind
// (non-KRM, invalid YAML, empty) leaves a diagnostic at its position, so the managed
// documents always reconstruct to their true file indices. Managed docs sit at file
// indices 0, 2, 4 here, interleaved with a non-KRM doc, an invalid doc, and a
// trailing empty doc.
func TestReconstructManagedIndices_RecordlessGapsLeaveDiagnostics(t *testing.T) {
	src := deployYAML + // web @0
		"---\njust: data\n" + // non-KRM @1
		"---\n" + configMapCYAML + // c @2
		"---\nfoo: [bar\n" + // invalid YAML @3
		"---\n" + plainSecretYAML + // db @4
		"---\n# trailing comment\n" // empty @5
	fsys := fstest.MapFS{"app.yaml": {Data: []byte(src)}}
	store := buildStoreFS(context.Background(), fsys, nil, Allowlist{})

	loc := documentLocations(store)
	got := map[string]int{}
	for _, dm := range store.FilesByPath["app.yaml"].Documents {
		got[dm.ManifestIdentity.Name] = loc[dm].DocumentIndex
	}
	want := map[string]int{"web": 0, "c": 2, "db": 4}
	for name, idx := range want {
		if got[name] != idx {
			t.Errorf("%s reconstructed to #%d, want #%d (all=%+v)", name, got[name], idx, got)
		}
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

func TestAccept_UnsupportedKustomizeRefuses(t *testing.T) {
	// A kustomization.yaml that uses a hard feature (patches) is retained, flagged
	// Unsupported, and refused — the operator will not write into a folder it cannot map
	// back to editable source documents.
	fsys := fstest.MapFS{
		"kustomization.yaml": {Data: []byte(hardKustomizationY)},
		"deploy.yaml":        {Data: []byte(deployYAML)},
	}
	store, acc := acceptanceOf(t, fsys, snapMapper(), AcceptancePolicy{Allowlist: DefaultAllowlist()})

	if acc.Accepted {
		t.Fatalf("a hard-kustomize folder must be refused, got accepted")
	}
	if countAcceptance(acc, IssueUnsupportedKustomize) != 1 {
		t.Fatalf("want one unsupported-kustomize refusal, got %+v", acc.Issues)
	}
	if len(store.Retained) != 1 || !store.Retained[0].Unsupported {
		t.Fatalf("the kustomization should be retained and flagged Unsupported, got %+v", store.Retained)
	}
	// The refusal must name the offending file so a human can act on it.
	for _, is := range acc.Issues {
		if is.Kind == IssueUnsupportedKustomize && is.Path != "kustomization.yaml" {
			t.Errorf("refusal should name kustomization.yaml, got %q", is.Path)
		}
	}
}

func TestAccept_CleanKustomizationNotFlaggedUnsupported(t *testing.T) {
	// A plain kustomization (namespace/resources only) must NOT be flagged or refused —
	// no false refusal on a legitimate build directive.
	fsys := fstest.MapFS{
		"kustomization.yaml": {Data: []byte(kustomizationY)},
		"deploy.yaml":        {Data: []byte(deployYAML)},
	}
	store, acc := acceptanceOf(t, fsys, snapMapper(), AcceptancePolicy{Allowlist: DefaultAllowlist()})
	if !acc.Accepted {
		t.Fatalf("a clean kustomization folder must pass, got %+v", acc.Issues)
	}
	if len(store.Retained) != 1 || store.Retained[0].Unsupported {
		t.Fatalf("a clean kustomization must not be flagged Unsupported, got %+v", store.Retained)
	}
}

func TestAcceptStructureOnly_RefusesUnsupportedKustomize(t *testing.T) {
	// The writer entrypoint (structure-only) still refuses hard-kustomize — the
	// kustomize refusal is a structural fact, not a mapping one.
	fsys := fstest.MapFS{
		"kustomization.yaml": {Data: []byte(hardKustomizationY)},
		"deploy.yaml":        {Data: []byte(deployYAML)},
	}
	store := buildStoreFS(context.Background(), fsys, snapMapper(), DefaultAllowlist())
	acc := AcceptStructureOnly(store)
	if acc.Accepted || countAcceptance(acc, IssueUnsupportedKustomize) != 1 {
		t.Fatalf("AcceptStructureOnly should refuse hard-kustomize, got accepted=%v issues=%+v",
			acc.Accepted, acc.Issues)
	}
}

func TestAcceptStructureOnly_SkipsMappingRefusals(t *testing.T) {
	// A served-but-empty registry makes a Widget NotFollowable. Plain Accept refuses it
	// (IssueUnresolvedKRM); AcceptStructureOnly must NOT — the writer never refuses on a
	// discovery-derived fact that can blink on a wobble.
	fsys := fstest.MapFS{"w.yaml": {Data: []byte(widgetYAMLDoc)}}
	mapper := typeset.NewSnapshotRegistry(typeset.Snapshot{Generation: 1})

	store := buildStoreFS(context.Background(), fsys, mapper, Allowlist{})
	if acc := Accept(store, AcceptancePolicy{}); acc.Accepted {
		t.Fatalf("plain Accept should refuse an unwatched Widget, got accepted")
	}
	if acc := AcceptStructureOnly(store); !acc.Accepted {
		t.Fatalf("AcceptStructureOnly must skip mapping refusals, got %+v", acc.Issues)
	}
}

func TestRefusalError(t *testing.T) {
	if err := RefusalError(Acceptance{Accepted: true}); err != nil {
		t.Fatalf("RefusalError on an accepted result should be nil, got %v", err)
	}
	refused := Acceptance{
		Accepted: false,
		Issues: []AcceptanceIssue{
			{Kind: IssueUnsupportedKustomize, Path: "team-a/kustomization.yaml", Message: "uses patches"},
		},
	}
	err := RefusalError(refused)
	var are *AcceptanceRefusedError
	if !errors.As(err, &are) {
		t.Fatalf("RefusalError should return *AcceptanceRefusedError, got %T", err)
	}
	if are.BlockMessage() == "" || !strings.Contains(are.Error(), "team-a/kustomization.yaml") {
		t.Errorf("refusal error should name the offending file, got %q", are.Error())
	}
}
