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
const corpusRoot = "../../internal/manifestanalyzer/testdata/scan-repo"

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

// An external-base overlay and a permanent structural refusal are different truths a consumer
// must be able to tell apart: render-root scoping shipped, so the overlay is ADOPTED, while
// helm inflation and unsupported kustomize stay the permanent support boundary. Collapsing the
// two would either mislabel an onboardable overlay as hopeless or an unbuildable folder as fine.
func TestScanRepo_OverlayAdoptedDistinctFromStructural(t *testing.T) {
	t.Parallel()

	t.Run("external-base overlay is adopted", func(t *testing.T) {
		t.Parallel()
		report, err := manifestanalyzer.ScanRepo(context.Background(), fixture(t, "supported", "base-overlays"))
		require.NoError(t, err)

		var seen bool
		for _, cand := range report.Candidates {
			require.Equal(t, manifestanalyzer.LayoutKustomizeOverlay, cand.Layout)
			require.True(t, cand.AcceptedByOperator, "render-root scoping adopts the external-base overlay")
			require.Empty(t, cand.RefusalReasons)
			seen = true
		}
		require.True(t, seen, "expected at least one kustomize-overlay candidate")
	})

	structural := map[string]string{
		"helm inflation is permanent":        "helm-inflation",
		"unsupported kustomize is permanent": "unsupported-kustomize",
	}
	for name, fixtureName := range structural {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			report, err := manifestanalyzer.ScanRepo(context.Background(), fixture(t, "unsupported", fixtureName))
			require.NoError(t, err)

			var seen bool
			for _, cand := range report.Candidates {
				if cand.Layout != manifestanalyzer.LayoutRefusedStructural {
					continue
				}
				seen = true
				require.False(t, cand.AcceptedByOperator)
				codes := make([]string, 0, len(cand.RefusalReasons))
				for _, reason := range cand.RefusalReasons {
					codes = append(codes, reason.Code)
				}
				require.Contains(t, codes, manifestanalyzer.ReasonRefusedStructural)
			}
			require.True(t, seen, "expected at least one refused-structural candidate")
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
