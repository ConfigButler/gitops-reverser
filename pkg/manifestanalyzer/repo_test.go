// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/pkg/manifestanalyzer"
)

// corpusRoot is the discovery corpus the engine's own golden tests drive. The public
// package reads the same fixtures rather than growing a second copy: the point of this
// package is that it cannot disagree with the engine, and a duplicated corpus is the
// first way that guarantee rots. Tests run with the package directory as cwd.
const corpusRoot = "../../internal/manifestanalyzer/testdata/repo-walker"

func fixture(t *testing.T, group, name string) string {
	t.Helper()
	path := filepath.Join(corpusRoot, group, name)
	info, err := os.Stat(path)
	require.NoError(t, err, "discovery corpus fixture %s must exist", path)
	require.True(t, info.IsDir())
	return path
}

func TestScanRepo_PlainPerEnvironmentFoldersAreCandidates(t *testing.T) {
	t.Parallel()

	report, err := manifestanalyzer.ScanRepo(context.Background(), fixture(t, "supported", "plain-per-env"))
	require.NoError(t, err)

	require.Equal(t, manifestanalyzer.SchemaVersion, report.SchemaVersion)
	require.NotEmpty(t, report.Candidates)
	for _, cand := range report.Candidates {
		require.Equal(t, manifestanalyzer.LayoutPlain, cand.Layout)
		require.True(t, cand.AcceptedByOperator, "plain KRM folders are the launch layout")
		require.Empty(t, cand.RefusalReasons)
		require.False(t, cand.RenderRoot)
	}
	require.Equal(t, len(report.Candidates), report.Summary.Accepted)
	require.Zero(t, report.Summary.Refused)
}

// The two refusal codes mean different things and a consumer must be able to tell them
// apart: one is "not yet, when render-root scoping ships", the other is the permanent
// support boundary. Collapsing them would mislabel an onboardable repository as hopeless.
func TestScanRepo_RefusalCodesStayDistinct(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		group, name string
		wantCode    string
		wantLayout  manifestanalyzer.Layout
	}{
		"overlay fan-out is a not-yet": {
			group: "unsupported", name: "base-overlays",
			wantCode:   manifestanalyzer.ReasonOverlayFanOutNeedsF2,
			wantLayout: manifestanalyzer.LayoutKustomizeOverlay,
		},
		"helm inflation is permanent": {
			group: "unsupported", name: "helm-inflation",
			wantCode:   manifestanalyzer.ReasonRefusedStructural,
			wantLayout: manifestanalyzer.LayoutRefusedStructural,
		},
		"unsupported kustomize is permanent": {
			group: "unsupported", name: "unsupported-kustomize",
			wantCode:   manifestanalyzer.ReasonRefusedStructural,
			wantLayout: manifestanalyzer.LayoutRefusedStructural,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			report, err := manifestanalyzer.ScanRepo(context.Background(), fixture(t, tc.group, tc.name))
			require.NoError(t, err)

			var seen bool
			for _, cand := range report.Candidates {
				if cand.Layout != tc.wantLayout {
					continue
				}
				seen = true
				require.False(t, cand.AcceptedByOperator)
				codes := make([]string, 0, len(cand.RefusalReasons))
				for _, reason := range cand.RefusalReasons {
					codes = append(codes, reason.Code)
				}
				require.Contains(t, codes, tc.wantCode)
			}
			require.True(t, seen, "expected at least one %s candidate", tc.wantLayout)
		})
	}
}

func TestScanRepo_ReportsOverlapConflicts(t *testing.T) {
	t.Parallel()

	report, err := manifestanalyzer.ScanRepo(context.Background(), fixture(t, "supported", "overlapping"))
	require.NoError(t, err)

	require.NotEmpty(t, report.Summary.OverlapConflicts,
		"two nested candidates can never both own a folder; the conflict must surface")
	for _, conflict := range report.Summary.OverlapConflicts {
		require.NotEmpty(t, conflict.Ancestor)
		require.NotEmpty(t, conflict.Descendant)
		require.NotEqual(t, conflict.Ancestor, conflict.Descendant)
	}
}

func TestScanRepo_FleetRootIsNeverACandidate(t *testing.T) {
	t.Parallel()

	root := fixture(t, "supported", "fleet-root")
	report, err := manifestanalyzer.ScanRepo(context.Background(), root)
	require.NoError(t, err)

	require.True(t, report.Summary.FleetRoot)
	for _, cand := range report.Candidates {
		require.NotEqual(t, ".", cand.Path, "a GitTarget points at an app subtree, never the fleet root")
	}
}

func TestScanRepo_MissingRootIsAnError(t *testing.T) {
	t.Parallel()
	_, err := manifestanalyzer.ScanRepo(context.Background(), filepath.Join(t.TempDir(), "absent"))
	require.Error(t, err)
}

func TestRepoReport_WriteJSON_Contract(t *testing.T) {
	t.Parallel()

	report, err := manifestanalyzer.ScanRepo(context.Background(), fixture(t, "supported", "kustomize-single"))
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, report.WriteJSON(&buf))

	var raw map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &raw))
	require.Equal(t, "v1", raw["schemaVersion"])
	require.Contains(t, raw, "candidates")
	require.Contains(t, raw, "summary")

	var decoded manifestanalyzer.RepoReport
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	require.Equal(t, report, decoded)
}

// An empty repository still marshals candidates as an array, so a consumer can iterate it
// without a nil check.
func TestRepoReport_WriteJSON_EmptyCandidatesIsArray(t *testing.T) {
	t.Parallel()

	report, err := manifestanalyzer.ScanRepo(context.Background(), t.TempDir())
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, report.WriteJSON(&buf))
	require.Contains(t, buf.String(), `"candidates": []`)
}
