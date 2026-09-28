// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// quickstartChartInputs are read in-process so `go test` treats them as cache inputs; helm reads
// the chart in a subprocess the test log cannot see.
var quickstartChartInputs = []string{
	"../charts/gitops-reverser/values.yaml",
	"../charts/gitops-reverser/values.schema.json",
	"../charts/gitops-reverser/templates/quickstart.yaml",
}

func helmTemplateQuickstart(t *testing.T, setValues ...string) ([]byte, error) {
	t.Helper()
	for _, path := range quickstartChartInputs {
		_, err := os.ReadFile(path)
		require.NoError(t, err)
	}
	args := []string{
		"template", "gitops-reverser", "../charts/gitops-reverser",
		"--show-only", "templates/quickstart.yaml",
		"--set", "quickstart.enabled=true",
		"--set", "quickstart.gitProvider.url=https://example.com/repo.git",
	}
	for _, sv := range setValues {
		args = append(args, "--set", sv)
	}
	return exec.Command("helm", args...).CombinedOutput()
}

// The starter WatchRule renders without a rules[].operations key: a rule selects a resource
// collection, which is observed through its whole lifecycle.
func TestChartQuickstart_StarterWatchRuleRendersWithoutOperations(t *testing.T) {
	out, err := helmTemplateQuickstart(t)
	require.NoErrorf(t, err, "helm template failed: %s", out)

	rendered := string(out)
	require.Contains(t, rendered, "kind: WatchRule")
	require.Contains(t, rendered, "- configmaps")
	require.NotContains(t, rendered, "operations")
}

// rules[].operations is gone from the API. A values file that still sets it must fail the render
// rather than install a rule the user believes suppresses deletions: the removed key is not
// deletion protection, the GitTarget's prune mode is.
func TestChartQuickstart_RemovedOperationsKeyFailsTheRender(t *testing.T) {
	out, err := helmTemplateQuickstart(t,
		"quickstart.watchRule.rules[0].resources[0]=configmaps",
		"quickstart.watchRule.rules[0].operations[0]=CREATE",
	)
	require.Errorf(t, err, "render should have failed, got:\n%s", out)
	require.Contains(t, string(out),
		"at '/quickstart/watchRule/rules/0': additional properties 'operations' not allowed")
}
