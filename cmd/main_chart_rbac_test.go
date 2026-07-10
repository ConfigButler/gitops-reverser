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
	"../charts/gitops-reverser/values.schema.json",
	"../charts/gitops-reverser/templates/rbac.yaml",
	"../charts/gitops-reverser/config/role.yaml",
}

// selectedConfigMapsAndDeployments is the least-privilege example the chart ships commented
// out: the core group's configmaps plus apps/deployments, and nothing else.
var selectedConfigMapsAndDeployments = []string{
	"rbac.watchTypes.mode=selected",
	"rbac.watchTypes.selected[0].apiGroups[0]=", // "" is the core group
	"rbac.watchTypes.selected[0].resources[0]=configmaps",
	"rbac.watchTypes.selected[1].apiGroups[0]=apps",
	"rbac.watchTypes.selected[1].resources[0]=deployments",
}

func helmTemplateRBAC(setValues ...string) ([]byte, error) {
	args := []string{
		"template", "gitops-reverser", "../charts/gitops-reverser",
		"--show-only", "templates/rbac.yaml",
	}
	for _, sv := range setValues {
		args = append(args, "--set", sv)
	}
	return exec.Command("helm", args...).CombinedOutput()
}

// renderClusterRoles renders the chart's RBAC and returns every ClusterRole it produces.
func renderClusterRoles(t *testing.T, setValues ...string) []rbacv1.ClusterRole {
	t.Helper()

	for _, path := range rbacChartInputs {
		_, err := os.ReadFile(path)
		require.NoErrorf(t, err, "%s missing; `task helm-sync` generates it", path)
	}

	out, err := helmTemplateRBAC(setValues...)
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

// The whole point of splitting the wildcard out: `selected` mode must leave a role set that
// cannot enumerate Secrets. A reverser mirroring two types on a management cluster would
// otherwise hold read access to every credential in it.
func TestChartRBAC_SelectedModeCannotEnumerateSecrets(t *testing.T) {
	roles := renderClusterRoles(t, selectedConfigMapsAndDeployments...)

	_, found := roleNamed(t, roles, "-watch-any")
	require.False(t, found, "the wildcard ClusterRole must not render in selected mode")

	for _, role := range roles {
		require.Emptyf(t, grantsSecretListOrWatch(role),
			"ClusterRole %q still grants Secret list/watch", role.Name)
	}
}

// selected grants exactly the listed types, read-only. Anything more is a privilege the user
// did not ask for; anything less and the WatchRule cannot do its job.
func TestChartRBAC_SelectedModeGrantsOnlyTheListedTypes(t *testing.T) {
	roles := renderClusterRoles(t, selectedConfigMapsAndDeployments...)

	selected, found := roleNamed(t, roles, "-watch-selected")
	require.True(t, found, "selected mode must render its ClusterRole")
	require.Len(t, selected.Rules, 2)

	require.Equal(t, []string{""}, selected.Rules[0].APIGroups)
	require.Equal(t, []string{"configmaps"}, selected.Rules[0].Resources)
	require.Equal(t, []string{"apps"}, selected.Rules[1].APIGroups)
	require.Equal(t, []string{"deployments"}, selected.Rules[1].Resources)

	for _, rule := range selected.Rules {
		require.ElementsMatch(t, []string{"get", "list", "watch"}, rule.Verbs,
			"a watched type is only ever read")
		require.NotContains(t, rule.Resources, "*")
		require.NotContains(t, rule.APIGroups, "*")
	}
}

// The manager role is generated from the kubebuilder markers. It must never carry the
// wildcard itself: RBAC is additive, so a wildcard there would re-grant Secret list/watch
// and no chart value could take it away.
func TestChartRBAC_ManagerRoleNeverCarriesTheWildcard(t *testing.T) {
	modes := map[string][]string{
		"any":      {"rbac.watchTypes.mode=any"},
		"selected": selectedConfigMapsAndDeployments,
	}
	for name, values := range modes {
		t.Run(name, func(t *testing.T) {
			roles := renderClusterRoles(t, values...)
			manager, found := roleNamed(t, roles, "-manager-role")
			require.True(t, found, "manager ClusterRole must always render")

			for _, rule := range manager.Rules {
				require.NotContains(t, rule.Resources, "*",
					"manager role must not grant wildcard resources (%v)", rule.APIGroups)
			}
			require.Empty(t, grantsSecretListOrWatch(manager),
				"the manager role must never enumerate Secrets; it reads the ones it is pointed at")
		})
	}
}

// The API-resource catalog and its trigger informers read these three. Without them a
// least-privilege install 403s on every reflector retry, which is why a `selected` user must
// not have to restate them.
func TestChartRBAC_ManagerRoleGrantsTheAPISurfaceReads(t *testing.T) {
	roles := renderClusterRoles(t, selectedConfigMapsAndDeployments...)
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

// Default installs keep working: a WatchRule may name any type, so `any` is the default.
func TestChartRBAC_WildcardRoleShipsByDefault(t *testing.T) {
	roles := renderClusterRoles(t)

	wildcard, found := roleNamed(t, roles, "-watch-any")
	require.True(t, found, "the wildcard ClusterRole must render by default")
	require.Len(t, wildcard.Rules, 1)
	require.Equal(t, []string{"*"}, wildcard.Rules[0].APIGroups)
	require.Equal(t, []string{"*"}, wildcard.Rules[0].Resources)
	require.ElementsMatch(t, []string{"get", "list", "watch"}, wildcard.Rules[0].Verbs)
}

// A mis-set watchTypes must fail the render, not silently grant the wrong thing. Granting
// nothing is as bad as granting everything: one breaks the operator, the other the cluster.
//
// values.schema.json is what rejects these, and helm applies it to template, lint, install
// and upgrade alike. The template carries no equivalent `fail` guards, so these cases are the
// only thing standing between a typo and a ClusterRole nobody meant to create.
func TestChartRBAC_MisconfiguredWatchTypesFailsTheRender(t *testing.T) {
	tests := map[string]struct {
		setValues []string
		wantErr   string
	}{
		"unknown mode": {
			setValues: []string{"rbac.watchTypes.mode=readonly"},
			wantErr:   "at '/rbac/watchTypes/mode': value must be one of 'any', 'selected'",
		},
		"selected with no entries": {
			setValues: []string{"rbac.watchTypes.mode=selected"},
			wantErr:   "at '/rbac/watchTypes/selected': minItems: got 0, want 1",
		},
		"entry without resources": {
			setValues: []string{
				"rbac.watchTypes.mode=selected",
				"rbac.watchTypes.selected[0].apiGroups[0]=apps",
			},
			wantErr: "at '/rbac/watchTypes/selected/0': missing property 'resources'",
		},
		"entry without apiGroups": {
			setValues: []string{
				"rbac.watchTypes.mode=selected",
				"rbac.watchTypes.selected[0].resources[0]=deployments",
			},
			wantErr: "at '/rbac/watchTypes/selected/0': missing property 'apiGroups'",
		},
		// Verbs are not the user's to choose: the reverser mirrors a type, it never writes one.
		"entry setting verbs": {
			setValues: []string{
				"rbac.watchTypes.mode=selected",
				"rbac.watchTypes.selected[0].apiGroups[0]=apps",
				"rbac.watchTypes.selected[0].resources[0]=deployments",
				"rbac.watchTypes.selected[0].verbs[0]=create",
			},
			wantErr: "at '/rbac/watchTypes/selected/0': additional properties 'verbs' not allowed",
		},
		// A typo in a key name is the likeliest way to think you narrowed access when you did not.
		"unknown key under watchTypes": {
			setValues: []string{"rbac.watchTypes.modes=selected"},
			wantErr:   "additional properties 'modes' not allowed",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			out, err := helmTemplateRBAC(tc.setValues...)
			require.Errorf(t, err, "render should have failed, got:\n%s", out)
			require.Contains(t, string(out), tc.wantErr)
		})
	}
}
