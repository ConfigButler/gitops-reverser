// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// rbacChartInputs are read in-process so `go test` treats them as cache inputs; helm reads
// the chart in a subprocess the test log cannot see.
var rbacChartInputs = []string{
	"../charts/gitops-reverser/values.yaml",
	"../charts/gitops-reverser/templates/rbac.yaml",
	"../charts/gitops-reverser/config/role.yaml",
}

// renderClusterRoles renders the chart's RBAC and returns every ClusterRole it produces.
func renderClusterRoles(t *testing.T, setValues ...string) []rbacv1.ClusterRole {
	t.Helper()

	for _, path := range rbacChartInputs {
		_, err := os.ReadFile(path)
		require.NoErrorf(t, err, "%s missing; `task helm-sync` generates it", path)
	}

	args := []string{
		"template", "gitops-reverser", "../charts/gitops-reverser",
		"--show-only", "templates/rbac.yaml",
	}
	for _, sv := range setValues {
		args = append(args, "--set", sv)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	require.NoErrorf(t, err, "helm template failed: %s", out)

	var roles []rbacv1.ClusterRole
	for _, doc := range strings.Split(string(out), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var obj rbacv1.ClusterRole
		require.NoError(t, yaml.Unmarshal([]byte(doc), &obj))
		if obj.Kind == "ClusterRole" {
			roles = append(roles, obj)
		}
	}
	require.NotEmpty(t, roles, "chart rendered no ClusterRole")
	return roles
}

func covers(values []string, want string) bool {
	for _, v := range values {
		if v == want || v == "*" {
			return true
		}
	}
	return false
}

// grantsSecretListOrWatch reports the rules that let the holder enumerate Secrets cluster-wide.
// `get` is fine: the operator reads only the Secrets a GitProvider or GitTarget names.
func grantsSecretListOrWatch(role rbacv1.ClusterRole) []rbacv1.PolicyRule {
	var bad []rbacv1.PolicyRule
	for _, rule := range role.Rules {
		if !covers(rule.APIGroups, "") || !covers(rule.Resources, "secrets") {
			continue
		}
		if covers(rule.Verbs, "list") || covers(rule.Verbs, "watch") {
			bad = append(bad, rule)
		}
	}
	return bad
}

func roleNamed(t *testing.T, roles []rbacv1.ClusterRole, suffix string) (rbacv1.ClusterRole, bool) {
	t.Helper()
	for _, r := range roles {
		if strings.HasSuffix(r.Name, suffix) {
			return r, true
		}
	}
	return rbacv1.ClusterRole{}, false
}

// The whole point of splitting the wildcard out: turning it off must leave a role that
// cannot enumerate Secrets. A reverser mirroring two CRDs on a management cluster would
// otherwise hold read access to every credential in it.
func TestChartRBAC_WithoutWildcardTheOperatorCannotEnumerateSecrets(t *testing.T) {
	roles := renderClusterRoles(t, "rbac.watchAnyResource=false")

	_, found := roleNamed(t, roles, "-watch-any-resource")
	require.False(t, found, "the wildcard ClusterRole must not render when disabled")

	for _, role := range roles {
		require.Emptyf(t, grantsSecretListOrWatch(role),
			"ClusterRole %q still grants Secret list/watch", role.Name)
	}
}

// The manager role is generated from the kubebuilder markers. It must never carry the
// wildcard itself: RBAC is additive, so a wildcard there would re-grant Secret list/watch
// and no chart value could take it away.
func TestChartRBAC_ManagerRoleNeverCarriesTheWildcard(t *testing.T) {
	for _, values := range [][]string{{"rbac.watchAnyResource=true"}, {"rbac.watchAnyResource=false"}} {
		roles := renderClusterRoles(t, values...)
		manager, found := roleNamed(t, roles, "-manager-role")
		require.True(t, found, "manager ClusterRole must always render")

		for _, rule := range manager.Rules {
			require.NotContains(t, rule.Resources, "*",
				"manager role must not grant wildcard resources (%v)", rule.APIGroups)
		}
		require.Empty(t, grantsSecretListOrWatch(manager),
			"the manager role must never enumerate Secrets; it reads the ones it is pointed at")
	}
}

// The API-resource catalog and its trigger informers read these two. Without them a
// least-privilege install 403s on every reflector retry.
func TestChartRBAC_ManagerRoleGrantsTheAPISurfaceReads(t *testing.T) {
	roles := renderClusterRoles(t, "rbac.watchAnyResource=false")
	manager, found := roleNamed(t, roles, "-manager-role")
	require.True(t, found)

	for _, want := range []struct{ group, resource string }{
		{"apiextensions.k8s.io", "customresourcedefinitions"},
		{"apiregistration.k8s.io", "apiservices"},
		{"", "namespaces"},
	} {
		granted := false
		for _, rule := range manager.Rules {
			if covers(rule.APIGroups, want.group) && covers(rule.Resources, want.resource) &&
				covers(rule.Verbs, "list") && covers(rule.Verbs, "watch") {
				granted = true
				break
			}
		}
		require.Truef(t, granted, "manager role must grant list+watch on %s/%s", want.group, want.resource)
	}
}

// Default installs keep working: a WatchRule may name any type, so the wildcard ships on.
func TestChartRBAC_WildcardRoleShipsByDefault(t *testing.T) {
	roles := renderClusterRoles(t)

	wildcard, found := roleNamed(t, roles, "-watch-any-resource")
	require.True(t, found, "the wildcard ClusterRole must render by default")
	require.Len(t, wildcard.Rules, 1)
	require.Equal(t, []string{"*"}, wildcard.Rules[0].APIGroups)
	require.Equal(t, []string{"*"}, wildcard.Rules[0].Resources)
	require.ElementsMatch(t, []string{"get", "list", "watch"}, wildcard.Rules[0].Verbs)
}
