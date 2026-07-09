// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// updateGolden regenerates the golden reports when UPDATE_GOLDEN=1 is set, the standard
// golden-file workflow: run once to (re)write the expectations, then review the diff.
var updateGolden = os.Getenv("UPDATE_GOLDEN") == "1"

// TestWalkRepo_Golden drives the whole discovery corpus under testdata/repo-walker.
// Each fixture is a self-contained repo with a sibling <fixture>.golden.json pinning its
// report, so layout classification, refusal codes, overlap detection, namespace
// inference, and the rendered/editable split are all fixed by real layouts rather than
// prose. The corpus is split supported/ vs unsupported/ mirroring the
// contextual-namespace corpus. See
// docs/design/gitops-api/f8-repo-discovery-and-onboarding-scan.md.
func TestWalkRepo_Golden(t *testing.T) {
	for _, group := range []string{"supported", "unsupported"} {
		base := filepath.Join("testdata", "repo-walker", group)
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
	rep, err := WalkRepo(context.Background(), fixture)
	if err != nil {
		t.Fatalf("WalkRepo(%s): %v", fixture, err)
	}
	if rep.Root != fixture {
		t.Errorf("Root = %q, want %q", rep.Root, fixture)
	}
	rep.Root = ""

	var buf bytes.Buffer
	if err := RenderRepoJSON(&buf, rep); err != nil {
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

// TestWalkRepo_RefusalCodesStayDistinct pins the load-bearing distinction the design
// calls out: a forward-looking overlay-fan-out-needs-f2 must never collapse into the
// permanent refused-structural boundary. base-overlays is the former; helm-inflation and
// unsupported-kustomize are the latter.
func TestWalkRepo_RefusalCodesStayDistinct(t *testing.T) {
	cases := []struct {
		fixture  string
		wantCode string
		layout   Layout
	}{
		{"unsupported/base-overlays", ReasonOverlayFanOutNeedsF2, LayoutKustomizeOverlay},
		{"unsupported/helm-inflation", ReasonRefusedStructural, LayoutRefusedStructural},
		{"unsupported/unsupported-kustomize", ReasonRefusedStructural, LayoutRefusedStructural},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			rep, err := WalkRepo(context.Background(), filepath.Join("testdata", "repo-walker", tc.fixture))
			if err != nil {
				t.Fatalf("WalkRepo: %v", err)
			}
			if len(rep.Candidates) == 0 {
				t.Fatalf("expected at least one candidate")
			}
			for _, c := range rep.Candidates {
				assertRefused(t, tc.fixture, c, tc.layout, tc.wantCode)
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

// TestWalkRepo_RenderedCountDedupesNestedBase guards the double-count: an overlay whose
// out-of-subtree base pulls in a nested base must count each rendered document exactly
// once (rendered=2 here: base/deployment + base/common/configmap), and readScope must be
// the minimal [base] rather than [base, base/common]. A regression would report 3.
func TestWalkRepo_RenderedCountDedupesNestedBase(t *testing.T) {
	fixture := filepath.Join("testdata", "repo-walker", "unsupported", "overlay-nested-base")
	rep, err := WalkRepo(context.Background(), fixture)
	if err != nil {
		t.Fatalf("WalkRepo: %v", err)
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

// TestWalkRepo_NotADirectory covers the usage error path: a missing or non-directory
// root is an error, not an empty report.
func TestWalkRepo_NotADirectory(t *testing.T) {
	if _, err := WalkRepo(context.Background(), filepath.Join("testdata", "does-not-exist")); err == nil {
		t.Fatal("expected an error for a missing root")
	}
}
