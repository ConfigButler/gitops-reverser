// SPDX-License-Identifier: Apache-2.0

package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	sigsyaml "sigs.k8s.io/yaml"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// The write path through a configMapGenerator. The corpus (shape 9) pins one diff or one refusal
// per rule; these are the cases a single-event fixture cannot state.

const generatorKustomizationYAML = `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: coffee
resources:
  - deployment.yaml
configMapGenerator:
  - name: coffee-flags
    envs:
      - flags.env
generatorOptions:
  disableNameSuffixHash: true
`

const generatorFlagsEnv = "# Turn off during the conference rush.\nENABLE_VOUCHERS=true\nSHOW_BANNER=true\n"

const generatorDeploymentYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: coffee
spec:
  selector:
    matchLabels:
      app: coffee
  template:
    metadata:
      labels:
        app: coffee
    spec:
      containers:
        - name: coffee
          image: ghcr.io/example/coffee:1.0.0
`

// seedGeneratorWorktree writes a kustomize folder whose ConfigMap a generator renders from an
// env file, beside a Deployment the kustomization lists, and returns the env file's path.
func seedGeneratorWorktree(t *testing.T) (*gogit.Worktree, string) {
	t.Helper()
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem().Root()
	seedFile(t, root, "kustomization.yaml", generatorKustomizationYAML)
	seedFile(t, root, "deployment.yaml", generatorDeploymentYAML)
	seedFile(t, root, "flags.env", generatorFlagsEnv)
	commitCorpusWorktree(t, worktree, "seed a generator folder")
	return worktree, filepath.Join(root, "flags.env")
}

func flagsEvent(operation string, data map[string]interface{}) Event {
	event := Event{
		Identifier: types.NewResourceIdentifier("", "v1", "configmaps", "coffee", "coffee-flags"),
		Operation:  operation,
	}
	if data != nil {
		event.Object = &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "coffee-flags", "namespace": "coffee"},
			"data":       data,
		}}
	}
	return event
}

// Two edits of one generated ConfigMap in one batch: the second plans against the bytes the
// first left, so both land and neither is reverted.
func TestPlanFlush_TwoEditsOfOneGeneratedConfigMapInOneBatch(t *testing.T) {
	worktree, envPath := seedGeneratorWorktree(t)

	changed, err := flushEventsForTest(t, newContentWriter(types.SensitiveResourcePolicy{}), worktree, corpusMapper(),
		flagsEvent("UPDATE", map[string]interface{}{"ENABLE_VOUCHERS": "false", "SHOW_BANNER": "true"}),
		flagsEvent("UPDATE", map[string]interface{}{"ENABLE_VOUCHERS": "false", "SHOW_BANNER": "false"}),
	)
	require.NoError(t, err)
	assert.True(t, changed)
	assertFileBytes(t, envPath,
		"# Turn off during the conference rush.\nENABLE_VOUCHERS=false\nSHOW_BANNER=false\n",
		"both edits land on their own lines")
}

// A live change to the Deployment beside a generator is written as before, with the generated
// ConfigMap in the render the oracle holds to not moving.
func TestPlanFlush_ANeighbourOfAGeneratorStaysWritable(t *testing.T) {
	worktree, envPath := seedGeneratorWorktree(t)
	deployment := sharedImageEvent("coffee", "ghcr.io/example/coffee:1.1.0")
	deployment.Object.SetNamespace("coffee")
	deployment.Identifier.Namespace = "coffee"
	deployment.Object.Object["spec"] = map[string]interface{}{
		"selector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": "coffee"}},
		"template": map[string]interface{}{
			"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": "coffee"}},
			"spec": map[string]interface{}{"containers": []interface{}{
				map[string]interface{}{"name": "coffee", "image": "ghcr.io/example/coffee:1.1.0"},
			}},
		},
	}

	changed, err := flushEventsForTest(t, newContentWriter(types.SensitiveResourcePolicy{}), worktree, corpusMapper(),
		deployment)
	require.NoError(t, err)
	assert.True(t, changed)
	assertFileBytes(
		t,
		filepath.Join(worktree.Filesystem().Root(), "deployment.yaml"),
		strings.Replace(
			generatorDeploymentYAML,
			"coffee:1.0.0",
			"coffee:1.1.0",
			1,
		),
		"the Deployment is edited in place",
	)
	assertFileBytes(t, envPath, generatorFlagsEnv, "the generator's input is untouched")
}

// A deleted generated ConfigMap takes its generator entry and the env file only that entry reads
// with it, whether the event carries the last object or only the identity. With its one entry
// gone, configMapGenerator: goes too.
func TestPlanFlush_ADeletedGeneratedConfigMapRemovesItsEntry(t *testing.T) {
	for name, event := range map[string]Event{
		"with the last object": flagsEvent("DELETE", map[string]interface{}{"ENABLE_VOUCHERS": "true"}),
		"by identity only":     flagsEvent("DELETE", nil),
	} {
		t.Run(name, func(t *testing.T) {
			worktree, envPath := seedGeneratorWorktree(t)
			changed, err := flushEventsForTest(t, newContentWriter(types.SensitiveResourcePolicy{}), worktree,
				corpusMapper(), event)
			require.NoError(t, err)
			assert.True(t, changed)
			assert.NoFileExists(t, envPath)
			assertFileBytes(t, filepath.Join(worktree.Filesystem().Root(), "kustomization.yaml"),
				`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: coffee
resources:
  - deployment.yaml
generatorOptions:
  disableNameSuffixHash: true
`, "the entry and the emptied configMapGenerator: are gone")
		})
	}
}

// A ConfigMap the target treats as sensitive is never written into a plaintext input file.
func TestPlanFlush_ASensitiveGeneratedConfigMapIsRefused(t *testing.T) {
	policy, err := types.ParseSensitiveResourcePolicy("configmaps")
	require.NoError(t, err)
	worktree, envPath := seedGeneratorWorktree(t)

	_, err = flushEventsForTest(t, newContentWriter(policy), worktree, corpusMapper(),
		flagsEvent("UPDATE", map[string]interface{}{"ENABLE_VOUCHERS": "false", "SHOW_BANNER": "true"}))

	var refused *manifestanalyzer.AcceptanceRefusedError
	require.ErrorAs(t, err, &refused)
	assert.Contains(t, refused.Error(), "a generator input is a plaintext file")
	assertFileBytes(t, envPath, generatorFlagsEnv, "no plaintext reaches the input")
}

// A ${...} token in an input that the live object carries expanded was substituted downstream, by
// Flux postBuild. Writing the expansion back would match the re-render and still corrupt the
// input, so the write is refused as the render-fidelity divergence it is.
func TestPlanFlush_AGeneratedConfigMapWithAnExpandedTokenIsRefused(t *testing.T) {
	worktree, envPath := seedGeneratorWorktree(t)
	tokenEnv := "HOST=shop.${cluster_domain}\nENABLE_VOUCHERS=true\n"
	seedFile(t, worktree.Filesystem().Root(), "flags.env", tokenEnv)

	_, err := flushEventsForTest(t, newContentWriter(types.SensitiveResourcePolicy{}), worktree, corpusMapper(),
		flagsEvent("UPDATE", map[string]interface{}{"HOST": "shop.example.com", "ENABLE_VOUCHERS": "false"}))

	var refused *manifestanalyzer.AcceptanceRefusedError
	require.ErrorAs(t, err, &refused)
	assert.True(t, refused.AllIssuesOfKinds(manifestanalyzer.IssueRenderDoesNotMatchLive))
	assert.Contains(t, refused.Error(), "${cluster_domain}")
	assertFileBytes(t, envPath, tokenEnv, "the token stays in the input")
}

// resyncGenerator folds a complete desired snapshot over the generator folder under one prune
// mode. The Deployment is always in it, so only the generated ConfigMap can be an orphan.
func resyncGenerator(
	t *testing.T,
	worktree *gogit.Worktree,
	mode v1alpha3.PruneMode,
	withFlags bool,
) ResyncStats {
	t.Helper()
	deployment := &unstructured.Unstructured{}
	require.NoError(t, sigsyaml.Unmarshal([]byte(generatorDeploymentYAML), &deployment.Object))
	deployment.SetNamespace("coffee")
	desired := []manifestanalyzer.DesiredResource{{
		Resource: types.NewResourceIdentifier("apps", "v1", "deployments", "coffee", "coffee"),
		Object:   deployment,
	}}
	if withFlags {
		flags := flagsEvent("UPDATE", map[string]interface{}{"ENABLE_VOUCHERS": "true", "SHOW_BANNER": "true"})
		desired = append(desired, manifestanalyzer.DesiredResource{Resource: flags.Identifier, Object: flags.Object})
	}
	w := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: corpusMapper()}
	stats, _, err := w.applyResyncToWorktree(
		t.Context(), worktree, "", ResolvedTargetMetadata{PruneMode: mode}, desired, nil)
	require.NoError(t, err)
	return stats
}

// A generated ConfigMap is as much the folder's content as a document is, so a resync that sweeps
// removes the generator entry of one the cluster no longer has, keeps it while the cluster has
// it, and only counts it as retained under a mode that does not sweep.
func TestResync_SweepsAGeneratedConfigMapTheClusterNoLongerHas(t *testing.T) {
	t.Run("always, absent: the entry and its env file go", func(t *testing.T) {
		worktree, envPath := seedGeneratorWorktree(t)
		stats := resyncGenerator(t, worktree, v1alpha3.PruneAlways, false)
		assert.Equal(t, 1, stats.Deleted)
		assert.NoFileExists(t, envPath)
		kustomization, err := os.ReadFile(filepath.Join(worktree.Filesystem().Root(), "kustomization.yaml"))
		require.NoError(t, err)
		assert.NotContains(t, string(kustomization), "coffee-flags")
	})
	t.Run("always, present: kept", func(t *testing.T) {
		worktree, envPath := seedGeneratorWorktree(t)
		stats := resyncGenerator(t, worktree, v1alpha3.PruneAlways, true)
		assert.Zero(t, stats.Deleted)
		assertFileBytes(t, envPath, generatorFlagsEnv, "a generated object the cluster has is matched")
	})
	t.Run("onEvent, absent: retained and counted", func(t *testing.T) {
		worktree, envPath := seedGeneratorWorktree(t)
		stats := resyncGenerator(t, worktree, v1alpha3.PruneOnEvent, false)
		assert.Zero(t, stats.Deleted)
		assert.Equal(t, 1, stats.Retained)
		assertFileBytes(t, envPath, generatorFlagsEnv, "onEvent never sweeps")
	})
}
