// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// Render the real NOTES template as ConfigMap data: helm template normally hides release notes.
// Read all inputs in-process so Go's test cache tracks chart-only changes.
func renderAuditNotes(t *testing.T, values ...string) string {
	t.Helper()
	chart := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(chart, "templates"), 0o750))
	for _, name := range []string{"Chart.yaml", "values.yaml", "values.schema.json", "templates/_helpers.tpl"} {
		data, err := os.ReadFile(filepath.Join("../charts/gitops-reverser", name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(chart, name), data, 0o600))
	}
	notes, err := os.ReadFile("../charts/gitops-reverser/templates/NOTES.txt")
	require.NoError(t, err)
	wrapper := "{{- define \"test.notes\" -}}" + string(notes) + "{{- end -}}\n" +
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: notes\ndata:\n  notes: {{ include \"test.notes\" . | quote }}\n"
	require.NoError(t, os.WriteFile(filepath.Join(chart, "templates/notes.yaml"), []byte(wrapper), 0o600))
	args := []string{"template", "gitops-reverser", chart, "--namespace", "gitops-reverser",
		"--set", "attribution.enabled=true", "--set", "queue.redis.addr=valkey:6379"}
	for _, value := range values {
		args = append(args, "--set", value)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	require.NoErrorf(t, err, "helm template: %s", out)
	var rendered struct {
		Data map[string]string `json:"data"`
	}
	require.NoError(t, yaml.Unmarshal(out, &rendered))
	return rendered.Data["notes"]
}

func TestChartAuditNotesUseConfiguredRouteMode(t *testing.T) {
	tests := map[string]struct {
		values []string
		url    string
	}{
		"default nodeport": {url: "https://127.0.0.1:30444/audit-webhook/default"},
		"clusterip": {values: []string{"auditService.type=ClusterIP", "auditService.clusterIP=10.96.0.44"},
			url: "https://10.96.0.44:9444/audit-webhook/default"},
		"loadbalancer": {values: []string{"auditService.type=LoadBalancer"},
			url: "\"https://<reachable-address>:9444/audit-webhook/default\""},
		"shared stream": {values: []string{"attribution.auditRouteAnnotationKey=example.com/route"},
			url: "https://127.0.0.1:30444/audit-webhook"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			notes := renderAuditNotes(t, tt.values...)
			require.Contains(t, notes, "export AUDIT_WEBHOOK_SERVER_URL="+tt.url+"\n")
			require.Contains(t, notes, "certificate-authority-data:")
			require.Contains(t, notes, "tls-server-name: ${AUDIT_TLS_SERVER_NAME}")
			require.Contains(t, notes, "umask 077\ncat > audit-webhook.kubeconfig")
			if name == "shared stream" {
				require.NotContains(t, notes, "/audit-webhook/default")
				require.Contains(t, notes, "example.com/route")
			}
		})
	}
}
