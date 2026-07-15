// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

// updateGolden regenerates the golden reports when UPDATE_GOLDEN=1 is set, the standard
// golden-file workflow: run once to (re)write the expectations, then review the diff.
var updateGolden = os.Getenv("UPDATE_GOLDEN") == "1"

// deployYAMLNS is a minimal namespaced manifest for in-memory (fstest) repo-walk tests.
const deployYAMLNS = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: demo
`

// TestScanRepo_Golden drives the whole discovery corpus under testdata/scan-repo.
// Each fixture is a self-contained repo with a sibling <fixture>.golden.json pinning its
// report, so layout classification, refusal codes, overlap detection, namespace
// inference, and the rendered/editable split are all fixed by real layouts rather than
// prose. The corpus is split supported/ vs unsupported/ mirroring the
// contextual-namespace corpus. See
// docs/design/support-boundary/repo-discovery-and-onboarding-scan.md.
func TestScanRepo_Golden(t *testing.T) {
	for _, group := range []string{"supported", "unsupported"} {
		base := filepath.Join("testdata", "scan-repo", group)
		entries, err := os.ReadDir(base)
		if err != nil {
			t.Fatalf("read corpus %s: %v", base, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue // the sibling .golden.json files
			}
			fixture := filepath.Join(base, e.Name())
			t.Run(filepath.Join(group, e.Name()), func(t *testing.T) { checkGoldenFixture(t, fixture) })
		}
	}
}

// checkGoldenFixture walks one fixture repo and compares (or, under UPDATE_GOLDEN,
// rewrites) its sibling golden report. The machine-specific root is blanked so the
// golden is path-independent.
func checkGoldenFixture(t *testing.T, fixture string) {
	t.Helper()
	rep, err := ScanRepo(context.Background(), fixture)
	if err != nil {
		t.Fatalf("ScanRepo(%s): %v", fixture, err)
	}
	if rep.Root != fixture {
		t.Errorf("Root = %q, want %q", rep.Root, fixture)
	}
	rep.Root = ""

	// The engine's own report shape, not the published one. pkg/manifestanalyzer owns the
	// machine-readable contract and pins it separately; this corpus pins the classification
	// facts (layout, refusal codes, overlaps, namespace inference, rendered/editable split).
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		t.Fatalf("render json: %v", err)
	}

	golden := fixture + ".golden.json"
	if updateGolden {
		if err := os.WriteFile(golden, buf.Bytes(), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run UPDATE_GOLDEN=1 to create): %v", err)
	}
	if !bytes.Equal(want, buf.Bytes()) {
		t.Errorf("report mismatch for %s\n--- want ---\n%s\n--- got ---\n%s", fixture, want, buf.Bytes())
	}
}

// scanFixtureCandidates scans one corpus fixture and returns its candidates, failing the
// test on a scan error or an empty candidate set.
func scanFixtureCandidates(t *testing.T, fixture string) []RepoCandidate {
	t.Helper()
	rep, err := ScanRepo(context.Background(), filepath.Join("testdata", "scan-repo", fixture))
	if err != nil {
		t.Fatalf("ScanRepo(%s): %v", fixture, err)
	}
	if len(rep.Candidates) == 0 {
		t.Fatalf("%s: expected at least one candidate", fixture)
	}
	return rep.Candidates
}

// TestScanRepo_ExternalBaseOverlayAdopted pins that render-root scoping's discovery half
// shipped: an external-base overlay is ADOPTED (its render scope passes the same gate the
// live writer runs), no longer refused with the retired overlay-fan-out reason.
func TestScanRepo_ExternalBaseOverlayAdopted(t *testing.T) {
	for _, fixture := range []string{"supported/base-overlays", "supported/overlay-parked-base"} {
		t.Run(fixture, func(t *testing.T) {
			for _, c := range scanFixtureCandidates(t, fixture) {
				if c.Layout != LayoutKustomizeOverlay {
					t.Errorf("%s: candidate %s layout = %q, want %q", fixture, c.Path, c.Layout, LayoutKustomizeOverlay)
				}
				if !c.AcceptedByOperator {
					t.Errorf("%s: external-base overlay %s should now be adopted; reasons=%+v",
						fixture, c.Path, c.RefusalReasons)
				}
			}
		})
	}
}

// TestScanRepo_StructuralStaysRefused pins the other half of the load-bearing distinction:
// the permanent refused-structural boundary must never collapse into an adoptable overlay.
func TestScanRepo_StructuralStaysRefused(t *testing.T) {
	for _, fixture := range []string{"unsupported/helm-inflation", "unsupported/unsupported-kustomize"} {
		t.Run(fixture, func(t *testing.T) {
			for _, c := range scanFixtureCandidates(t, fixture) {
				assertRefused(t, fixture, c, LayoutRefusedStructural, ReasonRefusedStructural)
			}
		})
	}
}

// assertRefused checks one candidate is refused with the expected layout and reason code.
func assertRefused(t *testing.T, fixture string, c RepoCandidate, layout Layout, code string) {
	t.Helper()
	if c.AcceptedByOperator {
		t.Errorf("%s: candidate %s should be refused", fixture, c.Path)
	}
	if c.Layout != layout {
		t.Errorf("%s: candidate %s layout = %q, want %q", fixture, c.Path, c.Layout, layout)
	}
	if len(c.RefusalReasons) == 0 || c.RefusalReasons[0].Code != code {
		t.Errorf("%s: candidate %s reasons = %+v, want code %q", fixture, c.Path, c.RefusalReasons, code)
	}
}

// TestScanRepo_RenderedCountDedupesNestedBase guards the double-count: an overlay whose
// out-of-subtree base pulls in a nested base must count each rendered document exactly
// once (rendered=2 here: base/deployment + base/common/configmap), and readScope must be
// the minimal [base] rather than [base, base/common]. A regression would report 3.
func TestScanRepo_RenderedCountDedupesNestedBase(t *testing.T) {
	fixture := filepath.Join("testdata", "scan-repo", "supported", "overlay-nested-base")
	rep, err := ScanRepo(context.Background(), fixture)
	if err != nil {
		t.Fatalf("ScanRepo: %v", err)
	}
	if len(rep.Candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(rep.Candidates), rep.Candidates)
	}
	c := rep.Candidates[0]
	if c.Resources.Rendered != 2 {
		t.Errorf("rendered = %d, want 2 (deduped; a double-count reports 3)", c.Resources.Rendered)
	}
	if len(c.ReadScope) != 1 || c.ReadScope[0] != "base" {
		t.Errorf("readScope = %v, want [base] (minimal; nested base/common folded in)", c.ReadScope)
	}
}

// TestScanRepo_RenderedExcludesParkedYAML guards that rendered counts only what the
// kustomization graph reaches: a base holding a parked.yaml its kustomization does not
// list must not inflate rendered. overlay-parked-base renders 1 (base/deployment), not 2.
func TestScanRepo_RenderedExcludesParkedYAML(t *testing.T) {
	fixture := filepath.Join("testdata", "scan-repo", "supported", "overlay-parked-base")
	rep, err := ScanRepo(context.Background(), fixture)
	if err != nil {
		t.Fatalf("ScanRepo: %v", err)
	}
	if len(rep.Candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(rep.Candidates), rep.Candidates)
	}
	if got := rep.Candidates[0].Resources.Rendered; got != 1 {
		t.Errorf("rendered = %d, want 1 (parked.yaml is not in the resources graph)", got)
	}
}

// TestScanRepo_RefusedPlainSurfacesGateReasons guards that a plain candidate the
// acceptance gate refuses reports WHY (the gate issues) rather than a bare
// acceptedByOperator: false. plain-nonkrm holds a non-KRM values.yaml.
func TestScanRepo_RefusedPlainSurfacesGateReasons(t *testing.T) {
	fixture := filepath.Join("testdata", "scan-repo", "unsupported", "plain-nonkrm")
	rep, err := ScanRepo(context.Background(), fixture)
	if err != nil {
		t.Fatalf("ScanRepo: %v", err)
	}
	if len(rep.Candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(rep.Candidates))
	}
	c := rep.Candidates[0]
	if c.AcceptedByOperator {
		t.Fatal("candidate with a non-KRM file should be refused")
	}
	if len(c.RefusalReasons) == 0 {
		t.Fatalf("a gate refusal must surface reasons, got none (acceptedByOperator=false with no why)")
	}
	if c.RefusalReasons[0].Code == "" || c.RefusalReasons[0].Detail == "" {
		t.Errorf("refusal reason should carry a code and detail, got %+v", c.RefusalReasons[0])
	}
}

// TestScanRepo_SopsBootstrapAcceptedLikeWriter guards that scan-repo's acceptance
// matches the live writer's allowlist: a folder holding the operator's .sops.yaml
// bootstrap config is accepted (the writer retains it), not refused as non-KRM, and the
// .sops.yaml is not counted as non-KRM noise.
func TestScanRepo_SopsBootstrapAcceptedLikeWriter(t *testing.T) {
	fsys := fstest.MapFS{
		"app/deployment.yaml": {Data: []byte(deployYAMLNS)},
		"app/.sops.yaml": {Data: []byte(
			"creation_rules:\n  - path_regex: .*\n    age: age1exampleexampleexampleexampleexampleexampleexample\n")},
	}
	rep := scanRepoFS(context.Background(), fsys)
	if len(rep.Candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(rep.Candidates), rep.Candidates)
	}
	c := rep.Candidates[0]
	if !c.AcceptedByOperator {
		t.Errorf("a folder with the operator .sops.yaml bootstrap should be accepted like the writer, reasons=%+v",
			c.RefusalReasons)
	}
	if c.Resources.NonKRM != 0 {
		t.Errorf(".sops.yaml is a retained build directive, not non-KRM noise; nonKrm=%d", c.Resources.NonKRM)
	}
}

// TestScanRepo_NotADirectory covers the usage error path: a missing or non-directory
// root is an error, not an empty report.
func TestScanRepo_NotADirectory(t *testing.T) {
	if _, err := ScanRepo(context.Background(), filepath.Join("testdata", "does-not-exist")); err == nil {
		t.Fatal("expected an error for a missing root")
	}
}
