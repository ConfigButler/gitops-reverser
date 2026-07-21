// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// retentionLogCapture returns a context whose logger records every default-verbosity line, plus
// the slice the lines land in. V(1) is dropped so the assertions see exactly what an operator
// running at default verbosity sees.
func retentionLogCapture() (context.Context, *[]string) {
	var lines []string
	logger := funcr.New(func(_, args string) {
		lines = append(lines, args)
	}, funcr.Options{})
	return log.IntoContext(context.Background(), logger), &lines
}

func retainingTarget(namespace, name, path string) ResolvedTargetMetadata {
	return ResolvedTargetMetadata{
		Name:      name,
		Namespace: namespace,
		Path:      path,
		PruneMode: v1alpha3.PruneOnEvent,
	}
}

// TestReportRetainedOrphans_NamesTheGitTarget holds the promise both configuration.md and
// UPGRADING.md make to operators: the retention line names the target, so a non-zero retention is
// actionable without correlating a folder back to an object. The folder alone cannot do that —
// see the co-resident case below.
func TestReportRetainedOrphans_NamesTheGitTarget(t *testing.T) {
	ctx, lines := retentionLogCapture()
	w := &BranchWorker{}

	w.reportRetainedOrphans(
		ctx,
		manifestanalyzer.Plan{RetainedOrphans: 3},
		retainingTarget("tenant-acme", "acme", "tenants/acme"),
		"tenants/acme",
		nil,
	)

	require.Len(t, *lines, 1, "a retention is reported once at default verbosity")
	assert.Contains(t, (*lines)[0], `"gitTarget"="tenant-acme/acme"`,
		"the retention line must name the GitTarget, not only its path")
	assert.Contains(t, (*lines)[0], `"retained"=3`)
	assert.Contains(t, (*lines)[0], `"pruneMode"="onEvent"`)
}

// TestReportRetainedOrphans_ThrottlesPerTargetNotPerPath is why the throttle key is the GitTarget
// plus its path rather than the path alone. Two targets in different namespaces may write the same
// spec.path on different branches; if they ever share a worker, keying on the path alone means the
// first one to report silences the second for ten minutes — a retention that is real, configured,
// and invisible.
func TestReportRetainedOrphans_ThrottlesPerTargetNotPerPath(t *testing.T) {
	ctx, lines := retentionLogCapture()
	w := &BranchWorker{}
	plan := manifestanalyzer.Plan{RetainedOrphans: 1}

	w.reportRetainedOrphans(ctx, plan, retainingTarget("tenant-a", "mirror", "shared"), "shared", nil)
	w.reportRetainedOrphans(ctx, plan, retainingTarget("tenant-b", "mirror", "shared"), "shared", nil)
	// The same target again inside the interval is the case the throttle exists for.
	w.reportRetainedOrphans(ctx, plan, retainingTarget("tenant-a", "mirror", "shared"), "shared", nil)

	require.Len(t, *lines, 2, "each target reports once; the repeat is throttled")
	assert.Contains(t, (*lines)[0], `"gitTarget"="tenant-a/mirror"`)
	assert.Contains(t, (*lines)[1], `"gitTarget"="tenant-b/mirror"`)
}

// TestReportRetainedOrphans_SilentWhenNothingIsRetained keeps the signal meaningful: a converged
// mirror under any mode must not log or count. Losing this turns the line into noise operators
// filter out, which defeats the whole point of reporting a retention at all.
func TestReportRetainedOrphans_SilentWhenNothingIsRetained(t *testing.T) {
	ctx, lines := retentionLogCapture()
	w := &BranchWorker{}

	w.reportRetainedOrphans(ctx, manifestanalyzer.Plan{}, retainingTarget("ns", "target", "p"), "p", nil)

	assert.Empty(t, *lines)
}

// TestReportRetainedOrphans_ReportsTheEffectiveModeForALegacyTarget covers the GitTarget created
// before spec.prune existed: it stores no mode at all. Routed through the real apply, because
// that is where the single normalization lives — reporting the raw "" would name a mode that does
// not exist, and one that answers "false" to both deletion predicates while meaning onEvent.
func TestReportRetainedOrphans_ReportsTheEffectiveModeForALegacyTarget(t *testing.T) {
	ctx, lines := retentionLogCapture()
	worktree := newWorktreeForTest(t)
	seedPlacedManifest(t, worktree, "apps/orphan.yaml", cmManifest("orphan", "blue"))
	w := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: configMapMapper()}

	_, _, err := w.applyResyncToWorktree(
		ctx, worktree, "",
		ResolvedTargetMetadata{Name: "acme", Namespace: "tenant-acme"}, // no PruneMode: a legacy target
		nil, nil,
	)
	require.NoError(t, err)

	require.Len(t, *lines, 1, "the orphan is retained under the effective default, and reported")
	assert.Contains(t, (*lines)[0], `"pruneMode"="onEvent"`)
	assert.Contains(t, (*lines)[0], `"gitTarget"="tenant-acme/acme"`)
}
