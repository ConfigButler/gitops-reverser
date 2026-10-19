// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

// receiverChartInputs are read in-process so `go test` treats them as cache inputs; helm reads
// the chart in a subprocess the test log cannot see.
var receiverChartInputs = []string{
	"../charts/gitops-reverser/values.yaml",
	"../charts/gitops-reverser/values.schema.json",
	"../charts/gitops-reverser/templates/_helpers.tpl",
	"../charts/gitops-reverser/templates/deployment.yaml",
	"../charts/gitops-reverser/templates/services.yaml",
	"../charts/gitops-reverser/templates/applier-rbac.yaml",
}

// helmTemplateDocs renders one chart template and splits it into its YAML documents. A template
// that renders nothing is an empty list, not an error: helm refuses --show-only for a template with
// no output, so that refusal is read as "nothing rendered".
func helmTemplateDocs(t *testing.T, template string, setValues ...string) []string {
	t.Helper()
	for _, path := range receiverChartInputs {
		_, err := os.ReadFile(path)
		require.NoError(t, err)
	}

	args := []string{"template", "gitops-reverser", "../charts/gitops-reverser", "--show-only", template}
	for _, sv := range setValues {
		args = append(args, "--set", sv)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil && strings.Contains(string(out), "could not find template") {
		return nil
	}
	require.NoErrorf(t, err, "helm template failed: %s", out)

	var docs []string
	for _, doc := range strings.Split(string(out), "\n---") {
		if strings.Contains(doc, "kind:") {
			docs = append(docs, doc)
		}
	}
	return docs
}

func renderManager(t *testing.T, setValues ...string) corev1.Container {
	t.Helper()
	docs := helmTemplateDocs(t, "templates/deployment.yaml", setValues...)
	require.Len(t, docs, 1)
	var deploy appsv1.Deployment
	require.NoError(t, yaml.Unmarshal([]byte(docs[0]), &deploy))
	for _, c := range deploy.Spec.Template.Spec.Containers {
		if c.Name == "manager" {
			return c
		}
	}
	t.Fatal("no manager container in rendered Deployment")
	return corev1.Container{}
}

func renderReceiverService(t *testing.T, setValues ...string) (corev1.Service, bool) {
	t.Helper()
	for _, doc := range helmTemplateDocs(t, "templates/services.yaml", setValues...) {
		var svc corev1.Service
		require.NoError(t, yaml.Unmarshal([]byte(doc), &svc))
		if strings.HasSuffix(svc.Name, "-receiver") {
			return svc, true
		}
	}
	return corev1.Service{}, false
}

func containerPort(c corev1.Container, name string) (int32, bool) {
	for _, p := range c.Ports {
		if p.Name == name {
			return p.ContainerPort, true
		}
	}
	return 0, false
}

// The receiver is off by default and the chart must say nothing about it: no flag (the binary's
// own default is off), no port, no Service. A bind address alone does not switch it on.
func TestChartReceiver_OffByDefault(t *testing.T) {
	for name, setValues := range map[string][]string{
		"default":              nil,
		"explicit off":         {"servers.receiver.enabled=false"},
		"bind address but off": {"servers.receiver.bindAddress=0.0.0.0:9393"},
	} {
		t.Run(name, func(t *testing.T) {
			manager := renderManager(t, setValues...)
			cfg, err := parseFlagsWithArgs(flag.NewFlagSet("manager", flag.ContinueOnError), manager.Args)
			require.NoError(t, err)
			assert.False(t, receiverEnabled(cfg))
			for _, arg := range manager.Args {
				assert.NotContains(t, arg, "--receiver-bind-address")
			}
			_, hasPort := containerPort(manager, "receiver")
			assert.False(t, hasPort)
			_, hasService := renderReceiverService(t, setValues...)
			assert.False(t, hasService)
		})
	}
}

// Enabled, the address reaches the binary unquoted, the containerPort is servers.receiver.port, and
// the Service targets it by name, as the other servers are wired.
func TestChartReceiver_BindAddressReachesTheBinaryAndTheService(t *testing.T) {
	for name, tc := range map[string]struct {
		setValues   []string
		bindAddress string
		wantPort    int32
	}{
		"defaults": {nil, ":9292", 9292},
		"host and port": {
			[]string{"servers.receiver.bindAddress=0.0.0.0:9393", "servers.receiver.port=9393"},
			"0.0.0.0:9393", 9393,
		},
	} {
		t.Run(name, func(t *testing.T) {
			setValues := append([]string{"servers.receiver.enabled=true"}, tc.setValues...)
			manager := renderManager(t, setValues...)

			cfg, err := parseFlagsWithArgs(flag.NewFlagSet("manager", flag.ContinueOnError), manager.Args)
			require.NoError(t, err, "the binary must accept the argv the chart renders")
			assert.Equal(t, tc.bindAddress, cfg.receiverBindAddress, "a quoted render would bind nothing")

			port, ok := containerPort(manager, "receiver")
			require.True(t, ok, "the receiver needs a named containerPort for the Service to target")
			assert.Equal(t, tc.wantPort, port)

			svc, ok := renderReceiverService(t, setValues...)
			require.True(t, ok)
			assert.Equal(t, "gitops-reverser-receiver", svc.Name)
			assert.Equal(t, corev1.ServiceTypeClusterIP, svc.Spec.Type)
			require.Len(t, svc.Spec.Ports, 1)
			assert.Equal(t, int32(80), svc.Spec.Ports[0].Port)
			assert.Equal(t, intstr.FromString("receiver"), svc.Spec.Ports[0].TargetPort)
		})
	}
}

func TestChartReceiver_MalformedBindAddressFailsTheRender(t *testing.T) {
	out, err := exec.Command("helm", "template", "gitops-reverser", "../charts/gitops-reverser",
		"--show-only", "templates/deployment.yaml", "--set", "servers.receiver.enabled=true",
		"--set", "servers.receiver.bindAddress=localhost").CombinedOutput()
	require.Error(t, err)
	assert.Contains(t, string(out), "/servers/receiver/bindAddress")
}

// applierAccess.flux is off by default, and on it grants exactly what holding a Flux Kustomization
// needs (suspend and resume it, fetch its source, list the source's consumers), to the operator's own
// ServiceAccount.
func TestChartApplierAccess_Flux(t *testing.T) {
	assert.Empty(t, helmTemplateDocs(t, "templates/applier-rbac.yaml"), "off unless asked for")
	assert.Empty(t, helmTemplateDocs(t, "templates/applier-rbac.yaml",
		"applierAccess.flux=true", "rbac.create=false"), "rbac.create=false renders no RBAC at all")

	docs := helmTemplateDocs(t, "templates/applier-rbac.yaml", "applierAccess.flux=true")
	require.Len(t, docs, 2)

	var role rbacv1.ClusterRole
	require.NoError(t, yaml.Unmarshal([]byte(docs[0]), &role))
	assert.Equal(t, "ClusterRole", role.Kind)
	assert.Equal(t, []rbacv1.PolicyRule{
		{
			APIGroups: []string{"kustomize.toolkit.fluxcd.io"},
			Resources: []string{"kustomizations"},
			Verbs:     []string{"get", "list", "patch"},
		},
		{
			APIGroups: []string{"helm.toolkit.fluxcd.io"},
			Resources: []string{"helmreleases"},
			Verbs:     []string{"list"},
		},
		{
			APIGroups: []string{"source.toolkit.fluxcd.io"},
			Resources: []string{"gitrepositories"},
			Verbs:     []string{"get", "patch"},
		},
		{
			APIGroups: []string{"source.toolkit.fluxcd.io"},
			Resources: []string{"helmcharts"},
			Verbs:     []string{"list"},
		},
		{
			APIGroups: []string{"authorization.k8s.io"},
			Resources: []string{"selfsubjectaccessreviews"},
			Verbs:     []string{"create"},
		},
	}, role.Rules)

	var binding rbacv1.ClusterRoleBinding
	require.NoError(t, yaml.Unmarshal([]byte(docs[1]), &binding))
	assert.Equal(t, role.Name, binding.RoleRef.Name)
	require.Len(t, binding.Subjects, 1)
	assert.Equal(t, "ServiceAccount", binding.Subjects[0].Kind)
	assert.Equal(t, "gitops-reverser", binding.Subjects[0].Name)
}

// applierAccess.argocd is off by default, and on it grants exactly what starting an Argo CD sync
// needs (get, and update to set the Application's operation), apart from the Flux grant.
func TestChartApplierAccess_ArgoCD(t *testing.T) {
	assert.Empty(t, helmTemplateDocs(t, "templates/applier-rbac.yaml",
		"applierAccess.argocd=true", "rbac.create=false"), "rbac.create=false renders no RBAC at all")

	docs := helmTemplateDocs(t, "templates/applier-rbac.yaml", "applierAccess.argocd=true")
	require.Len(t, docs, 2, "the Flux grant stays off")

	var role rbacv1.ClusterRole
	require.NoError(t, yaml.Unmarshal([]byte(docs[0]), &role))
	assert.Equal(t, "gitops-reverser-applier-argocd", role.Name)
	assert.Equal(t, []rbacv1.PolicyRule{
		{
			APIGroups: []string{"argoproj.io"},
			Resources: []string{"applications"},
			Verbs:     []string{"get", "update"},
		},
		{
			APIGroups: []string{"authorization.k8s.io"},
			Resources: []string{"selfsubjectaccessreviews"},
			Verbs:     []string{"create"},
		},
	}, role.Rules)

	var binding rbacv1.ClusterRoleBinding
	require.NoError(t, yaml.Unmarshal([]byte(docs[1]), &binding))
	assert.Equal(t, role.Name, binding.RoleRef.Name)
	require.Len(t, binding.Subjects, 1)
	assert.Equal(t, "gitops-reverser", binding.Subjects[0].Name)

	assert.Len(t, helmTemplateDocs(t, "templates/applier-rbac.yaml",
		"applierAccess.argocd=true", "applierAccess.flux=true"), 4, "both grants together")
}
