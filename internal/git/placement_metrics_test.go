// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

const (
	placementsMetric              = "gitopsreverser_placements_total"
	placementRefusalsMetric       = "gitopsreverser_placement_refusals_total"
	kustomizationEntriesMetric    = "gitopsreverser_placement_kustomization_entries_total"
	metricsTestGitTargetName      = "prod-mirror"
	metricsTestGitTargetNamespace = "tenant-a"
)

// targetedConfigMapEvent is newConfigMapEvent plus the GitTarget identity the live path
// carries on every event. The identity is what makes the placement counters actionable —
// "which target needs a byType line" — so the tests drive the real path with it set.
func targetedConfigMapEvent(name, namespace string) Event {
	event := newConfigMapEvent(name, namespace)
	event.GitTargetName = metricsTestGitTargetName
	event.GitTargetNamespace = metricsTestGitTargetNamespace
	return event
}

func targetedSecretEvent(name, namespace string) Event {
	return Event{
		Object: &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
		}},
		Identifier:         types.NewResourceIdentifier("", "v1", "secrets", namespace, name),
		Operation:          "CREATE",
		GitTargetName:      metricsTestGitTargetName,
		GitTargetNamespace: metricsTestGitTargetNamespace,
	}
}

// placementLabels is the label set every placement sample must carry: the GitTarget that
// owns the write, and the type key a placement.byType entry would name.
func placementLabels(resource string, extra map[string]string) map[string]string {
	labels := map[string]string{
		"gittarget_namespace": metricsTestGitTargetNamespace,
		"gittarget_name":      metricsTestGitTargetName,
		"group":               "",
		"version":             "v1",
		"resource":            resource,
	}
	for k, v := range extra {
		labels[k] = v
	}
	return labels
}

func flushWithPolicy(
	t *testing.T,
	worktree *gogit.Worktree,
	policy *manifestanalyzer.PlacementPolicy,
	events ...Event,
) {
	t.Helper()
	w := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: configMapMapper()}
	_, err := w.flushEventsToWorktree(context.Background(), worktree, "", events, policy, v1alpha3.PruneOnEvent)
	require.NoError(t, err)
}

// The signal the Option C deletion owes its users: a repository whose layout this operator
// cannot derive gets the canonical path, and `source="canonical"` on placements_total names
// the GitTarget and the type that needs one `placement.byType` line. Without the labels the
// counter would only say a fall-back happened somewhere, which is not a fix anybody can act
// on — see docs/design/open-asks-priority.md.
func TestPlacementMetrics_CanonicalFallbackNamesTargetAndType(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	worktree := newWorktreeForTest(t)
	seedPlacedManifest(t, worktree, "overlays/test/configmap-existing.yaml",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: existing\n  namespace: app\ndata:\n  k: v\n")

	flushWithPolicy(t, worktree, nil, targetedConfigMapEvent("cache", "app"))

	got, ok := telemetry.CollectInt64Sum(reader, placementsMetric, placementLabels("configmaps", map[string]string{
		"source":      "canonical",
		"disposition": "new_file",
	}))
	require.True(t, ok, "expected a canonical placement sample labelled by target and type")
	assert.Equal(t, int64(1), got)
}

// A declared template is the answer to a canonical fall-back, so the two must be
// distinguishable in the same series: `source="declared"` is how an operator confirms the
// byType line they added is actually the one being used.
func TestPlacementMetrics_DeclaredPlacementIsCountedAsDeclared(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	worktree := newWorktreeForTest(t)
	policy := &manifestanalyzer.PlacementPolicy{
		ByType: map[string]string{"v1/configmaps": "{namespace}/{name}.yaml"},
	}

	flushWithPolicy(t, worktree, policy, targetedConfigMapEvent("cache", "app"))

	got, ok := telemetry.CollectInt64Sum(reader, placementsMetric, placementLabels("configmaps", map[string]string{
		"source":      "declared",
		"disposition": "new_file",
	}))
	require.True(t, ok, "expected a declared placement sample")
	assert.Equal(t, int64(1), got)
}

// Appending to a bundle is only reachable by declaring one now, and `disposition="appended"`
// is what proves the bundle is being grown rather than a new file written beside it —
// the difference between a bundling policy that works and one whose path is subtly wrong.
func TestPlacementMetrics_DeclaredBundleRecordsAppendedDisposition(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	worktree := newWorktreeForTest(t)
	seedPlacedManifest(t, worktree, "all.yaml",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n  namespace: app\ndata:\n  k: v\n")
	policy := &manifestanalyzer.PlacementPolicy{ByType: map[string]string{"v1/configmaps": "all.yaml"}}

	flushWithPolicy(t, worktree, policy, targetedConfigMapEvent("cache", "app"))

	got, ok := telemetry.CollectInt64Sum(reader, placementsMetric, placementLabels("configmaps", map[string]string{
		"source":      "declared",
		"disposition": "appended",
	}))
	require.True(t, ok, "expected an appended placement sample")
	assert.Equal(t, int64(1), got)
}

// The structural fallback has its own source value, and it is the one that must NOT be read
// as a missing rule: a folder with one kustomize root is placing files where they render,
// which is the correct answer with no declaration at all. A dashboard that lumped it in with
// canonical would report every well-formed overlay as misconfigured. The successful
// resources: entry is counted too — it is the half that makes the file build.
func TestPlacementMetrics_KustomizeRootSourceAndEntryAdded(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	worktree := newWorktreeForTest(t)
	seedPlacedManifest(t, worktree, "overlays/test/kustomization.yaml",
		"namespace: app\nresources:\n  - deployment.yaml\n")
	seedPlacedManifest(t, worktree, "overlays/test/deployment.yaml",
		"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: app\n")

	flushWithPolicy(t, worktree, nil, targetedConfigMapEvent("cache", "app"))

	got, ok := telemetry.CollectInt64Sum(reader, placementsMetric, placementLabels("configmaps", map[string]string{
		"source":      "kustomize_root",
		"disposition": "new_file",
	}))
	require.True(t, ok, "expected a kustomize_root placement sample")
	assert.Equal(t, int64(1), got)

	entries, ok := telemetry.CollectInt64Sum(reader, kustomizationEntriesMetric, map[string]string{
		"gittarget_namespace": metricsTestGitTargetNamespace,
		"gittarget_name":      metricsTestGitTargetName,
		"outcome":             "added",
	})
	require.True(t, ok, "expected the resources: entry to be counted")
	assert.Equal(t, int64(1), entries)
}

// The invisible failure this counter exists for: the document is committed and its
// resources: entry is not, so kustomize never builds the file. The object is in Git, looks
// mirrored, and nothing applies it. A kustomization with no resources: sequence to append to
// is the reproducible shape of that.
func TestPlacementMetrics_KustomizationEntryFailureIsCounted(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	worktree := newWorktreeForTest(t)
	seedPlacedManifest(t, worktree, "overlays/test/kustomization.yaml", "namespace: app\n")

	flushWithPolicy(t, worktree, nil, targetedConfigMapEvent("cache", "app"))

	failed, ok := telemetry.CollectInt64Sum(reader, kustomizationEntriesMetric, map[string]string{
		"gittarget_namespace": metricsTestGitTargetNamespace,
		"gittarget_name":      metricsTestGitTargetName,
		"outcome":             "failed",
	})
	require.True(t, ok, "a resources: entry that could not be added must be counted")
	assert.Equal(t, int64(1), failed)
}

// A refused resource is one the mirror does NOT hold, so it is a counter of its own with a
// bounded reason — never a `source` value on placements_total, which would report a skipped
// Secret as a successful placement. Before this it left a log line and, on the resync path
// only, one integer in a summary.
func TestPlacementMetrics_SensitiveCollisionCountsARefusal(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	worktree := newWorktreeForTest(t)
	seedPlacedManifest(t, worktree, "secrets/app.sops.yaml",
		"apiVersion: v1\nkind: Secret\nmetadata:\n  name: other\n  namespace: app\nsops:\n  version: \"3\"\n")
	// Not identity-complete: every Secret in a namespace renders the same path, so the
	// second one collides with the first.
	policy := &manifestanalyzer.PlacementPolicy{
		ByType: map[string]string{"v1/secrets": "secrets/{namespace}.sops.yaml"},
	}

	flushWithPolicy(t, worktree, policy, targetedSecretEvent("api-token", "app"))

	refusals, ok := telemetry.CollectInt64Sum(reader, placementRefusalsMetric,
		placementLabels("secrets", map[string]string{"reason": "sensitive_append"}))
	require.True(t, ok, "expected a refusal sample naming the sensitive-append reason")
	assert.Equal(t, int64(1), refusals)

	placements, _ := telemetry.CollectInt64Sum(reader, placementsMetric, placementLabels("secrets", nil))
	assert.Zero(t, placements, "a refused resource must never also count as a placement")
}

// A declared template that escapes spec.path is refused by the runtime path gate, and it is
// worth its own reason: unlike the sensitivity refusals it is a template that can never work
// for any resource, so one series says "fix the policy" rather than "resolve a collision".
func TestPlacementMetrics_EscapingTemplateCountsAnInvalidPathRefusal(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	worktree := newWorktreeForTest(t)
	policy := &manifestanalyzer.PlacementPolicy{Default: "../../outside.yaml"}

	flushWithPolicy(t, worktree, policy, targetedConfigMapEvent("cache", "app"))

	refusals, ok := telemetry.CollectInt64Sum(reader, placementRefusalsMetric,
		placementLabels("configmaps", map[string]string{"reason": "invalid_path"}))
	require.True(t, ok, "expected an invalid_path refusal sample")
	assert.Equal(t, int64(1), refusals)
}

// Two resources of different sensitivity routed by one bundling default onto the SAME
// brand-new file: LocateNew cannot see the collision (it resolves each resource against the
// pre-batch snapshot), so the writer refuses the second one. That refusal is counted from the
// same closed reason set as the analyzer's, so "resources we declined to place" is one series.
func TestPlacementMetrics_MixedSensitivityNewFileCountsARefusal(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	worktree := newWorktreeForTest(t)
	policy := &manifestanalyzer.PlacementPolicy{Default: "{namespace}/all.yaml"}

	w := &BranchWorker{
		contentWriter: newContentWriter(types.SensitiveResourcePolicy{}),
		mapper:        configMapMapper(),
	}
	_, err = w.flushEventsToWorktree(
		context.Background(),
		worktree,
		"",
		[]Event{targetedConfigMapEvent("cache", "app"), targetedSecretEvent("api-token", "app")},
		policy,
		v1alpha3.PruneOnEvent,
	)
	require.NoError(t, err, "a co-mingling refusal is skipped, not returned as a batch error")

	refusals, ok := telemetry.CollectInt64Sum(reader, placementRefusalsMetric,
		map[string]string{
			"gittarget_namespace": metricsTestGitTargetNamespace,
			"gittarget_name":      metricsTestGitTargetName,
			"reason":              "mixed_sensitivity_new_file",
		})
	require.True(t, ok, "expected a mixed-sensitivity refusal sample")
	assert.Equal(t, int64(1), refusals)
}

// The resync path synthesises its events from the desired snapshot, so they carry no
// GitTarget identity of their own. Its placements must still land in the labelled series:
// otherwise a fall-back to canonical would be visible for a live create and invisible for
// the reconcile that produced the same file, and which of the two ran is not something the
// operator chose.
func TestPlacementMetrics_ResyncPlacementsCarryTheTargetLabels(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	w := &BranchWorker{contentWriter: writer, mapper: configMapMapper()}

	_, _, err = w.applyResyncToWorktree(
		context.Background(),
		worktree,
		"",
		ResolvedTargetMetadata{Name: metricsTestGitTargetName, Namespace: metricsTestGitTargetNamespace},
		[]manifestanalyzer.DesiredResource{desiredCM("fresh", "red")},
		nil,
	)
	require.NoError(t, err)

	got, ok := telemetry.CollectInt64Sum(reader, placementsMetric, map[string]string{
		"gittarget_namespace": metricsTestGitTargetNamespace,
		"gittarget_name":      metricsTestGitTargetName,
		"source":              "canonical",
	})
	require.True(t, ok, "a resync's create must be counted with the target labels")
	assert.Equal(t, int64(1), got)
}
