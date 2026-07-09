// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

// chartInputs are read by the test process itself so `go test` records them as cache
// inputs. helm reads the chart in a subprocess, which the test log cannot see, so without
// these reads a chart-only edit would replay a cached PASS.
var chartInputs = []string{
	"../charts/gitops-reverser/values.yaml",
	"../charts/gitops-reverser/templates/deployment.yaml",
}

// renderManagerArgs renders the chart's Deployment and returns the manager container's
// argv, exactly as the kubelet would hand it to the binary.
func renderManagerArgs(t *testing.T, setValues ...string) []string {
	t.Helper()

	for _, path := range chartInputs {
		_, err := os.ReadFile(path)
		require.NoError(t, err)
	}

	args := []string{
		"template", "gitops-reverser", "../charts/gitops-reverser",
		"--show-only", "templates/deployment.yaml",
	}
	for _, sv := range setValues {
		args = append(args, "--set", sv)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	require.NoErrorf(t, err, "helm template failed: %s", out)

	var deploy appsv1.Deployment
	require.NoError(t, yaml.Unmarshal(out, &deploy))

	for _, c := range deploy.Spec.Template.Spec.Containers {
		if c.Name == "manager" {
			return c.Args
		}
	}
	t.Fatalf("no manager container in rendered Deployment:\n%s", out)
	return nil
}

// The chart writes the manager's argv and the binary parses it; nothing else enforces that
// the two agree. A value interpolated as a bare `{{ . | quote }}` inside a YAML plain scalar
// keeps its double quotes as literal characters, so the flag arrives with them attached and
// a validating flag rejects it — the manager then CrashLoopBackOffs on a chart that renders,
// lints and packages cleanly. Parse what the chart actually emits.
func TestChartRendersArgsTheBinaryAccepts(t *testing.T) {
	tests := map[string]struct {
		setValues      []string
		wantKeyPrefix  string
		wantRedisAddr  string
		wantRedisDBSet int
	}{
		"chart defaults": {
			wantKeyPrefix: "gitops-reverser",
		},
		"nested tenant prefix": {
			setValues:     []string{"queue.redis.keyPrefix=cell-a:tenant-7"},
			wantKeyPrefix: "cell-a:tenant-7",
		},
		"redis enabled with auth": {
			setValues: []string{
				"queue.redis.addr=redis.example.com:6379",
				"queue.redis.auth.username=reverser",
				"queue.redis.db=3",
				"queue.redis.keyPrefix=tenant-7",
			},
			wantKeyPrefix:  "tenant-7",
			wantRedisAddr:  "redis.example.com:6379",
			wantRedisDBSet: 3,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			argv := renderManagerArgs(t, tc.setValues...)

			cfg, err := parseFlagsWithArgs(flag.NewFlagSet("manager", flag.ContinueOnError), argv)
			require.NoError(t, err, "the binary must accept the argv the chart renders")

			require.Equal(t, tc.wantKeyPrefix, cfg.redisKeyPrefix)
			if tc.wantRedisAddr != "" {
				require.Equal(t, tc.wantRedisAddr, cfg.redisAddr)
				require.Equal(t, tc.wantRedisDBSet, cfg.redisDB)
			}
		})
	}
}
