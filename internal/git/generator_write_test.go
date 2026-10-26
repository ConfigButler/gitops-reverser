// SPDX-License-Identifier: Apache-2.0

package git

import (
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

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

// A deleted generated ConfigMap has no document to remove, and the generator would bring it
// back: the delete is refused, naming what a person can do.
func TestPlanFlush_ADeletedGeneratedConfigMapIsRefused(t *testing.T) {
	for name, event := range map[string]Event{
		"with the last object": flagsEvent("DELETE", map[string]interface{}{"ENABLE_VOUCHERS": "true"}),
		"by identity only":     flagsEvent("DELETE", nil),
	} {
		t.Run(name, func(t *testing.T) {
			worktree, envPath := seedGeneratorWorktree(t)
			_, err := flushEventsForTest(t, newContentWriter(types.SensitiveResourcePolicy{}), worktree,
				corpusMapper(), event)

			var refused *manifestanalyzer.AcceptanceRefusedError
			require.ErrorAs(t, err, &refused)
			assert.True(t, refused.AllIssuesOfKinds(manifestanalyzer.IssueUnplaceableEdit))
			assert.Contains(t, refused.Error(), "remove the generator entry to delete it")
			assertFileBytes(t, envPath, generatorFlagsEnv, "a refused flush writes nothing")
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
