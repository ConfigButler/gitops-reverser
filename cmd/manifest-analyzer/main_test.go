// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

const deployYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: default
`

// fixtureDir creates a temp dir containing a watched-clean manifest plus a
// non-KRM YAML file (which is an acceptance issue).
func fixtureDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "deploy.yaml", deployYAML)
	write(t, dir, "values.yaml", "just: data\n")
	return dir
}

func TestRun_TextReport(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{fixtureDir(t)}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "Manifest analysis:") {
		t.Errorf("missing header: %s", out.String())
	}
}

func TestRun_JSONReport(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{"--format", "json", fixtureDir(t)}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errBuf.String())
	}
	var parsed map[string]any
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	if _, ok := parsed["summary"]; !ok {
		t.Errorf("json output missing summary key")
	}
}

func TestRun_RefusePolicy(t *testing.T) {
	dir := fixtureDir(t) // contains a non-KRM YAML, so there is an issue

	var out, errBuf bytes.Buffer
	if code := run([]string{"--policy", "refuse", dir}, &out, &errBuf); code != 1 {
		t.Errorf("refuse with issues: exit = %d, want 1", code)
	}

	// A clean tree under refuse should pass.
	clean := t.TempDir()
	write(t, clean, "deploy.yaml", deployYAML)
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"--policy", "refuse", clean}, &out, &errBuf); code != 0 {
		t.Errorf("refuse on clean tree: exit = %d, want 0\nstderr=%s", code, errBuf.String())
	}
}

func TestRun_GVKInventory(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "deploy.yaml", deployYAML)
	write(t, dir, "cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: c\n  namespace: default\n")

	var out, errBuf bytes.Buffer
	if code := run([]string{dir}, &out, &errBuf); code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr=%s", code, errBuf.String())
	}
	// Every GVK found is reported in the inventory, with no cluster involved.
	for _, want := range []string{"apps/v1/Deployment", "v1/ConfigMap"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("expected %q in GVK inventory: %s", want, out.String())
		}
	}
}

func TestRun_ScanTextRefuses(t *testing.T) {
	// fixtureDir contains a non-KRM values.yaml, so scan mode refuses under --policy
	// refuse and prints the acceptance verdict and the (empty, structure-only) plan.
	var out, errBuf bytes.Buffer
	if code := run([]string{"--mode", "scan-folder", "--policy", "refuse", fixtureDir(t)}, &out, &errBuf); code != 1 {
		t.Fatalf("scan refuse with issues: exit = %d, want 1 (stderr=%s)", code, errBuf.String())
	}
	for _, want := range []string{"Acceptance: REFUSED", "Plan: no changes"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("scan output missing %q:\n%s", want, out.String())
		}
	}
}

func TestRun_ScanCleanPasses(t *testing.T) {
	clean := t.TempDir()
	write(t, clean, "deploy.yaml", deployYAML)

	var out, errBuf bytes.Buffer
	if code := run([]string{"--mode", "scan-folder", "--policy", "refuse", clean}, &out, &errBuf); code != 0 {
		t.Fatalf("scan refuse on clean tree: exit = %d, want 0 (stderr=%s)", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "Acceptance: accepted") {
		t.Errorf("expected accepted verdict: %s", out.String())
	}
}

func TestRun_ScanJSON(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{"--mode", "scan-folder", "--format", "json", fixtureDir(t)}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errBuf.String())
	}
	var parsed map[string]any
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("scan JSON invalid: %v\n%s", err, out.String())
	}
	if _, ok := parsed["accepted"]; !ok {
		t.Errorf("scan JSON missing accepted key: %s", out.String())
	}
}

// scanRepoFixture builds a tiny two-folder repo: a plain KRM app folder and a kustomize
// overlay reaching an out-of-subtree base, so scan-repo reports both an accepted plain
// candidate and a refused kustomize-overlay one.
func scanRepoFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, mkdir(t, root, "plain"), "deploy.yaml", deployYAML)
	base := mkdir(t, root, "base")
	write(t, base, "kustomization.yaml", "resources:\n- deploy.yaml\n")
	write(t, base, "deploy.yaml", deployYAML)
	overlay := mkdir(t, root, filepath.Join("overlays", "test"))
	write(t, overlay, "kustomization.yaml", "namespace: test\nresources:\n- ../../base\n")
	return root
}

// mkdir creates dir/sub (and parents) and returns the full path.
func mkdir(t *testing.T, dir, sub string) string {
	t.Helper()
	full := filepath.Join(dir, sub)
	if err := os.MkdirAll(full, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", full, err)
	}
	return full
}

func TestRun_ScanRepoText(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"--mode", "scan-repo", scanRepoFixture(t)}, &out, &errBuf); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%s)", code, errBuf.String())
	}
	for _, want := range []string{"candidates:", "kustomize-overlay", "overlay-fan-out-unsupported"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("scan-repo text missing %q:\n%s", want, out.String())
		}
	}
}

func TestRun_ScanRepoJSON(t *testing.T) {
	var out, errBuf bytes.Buffer
	args := []string{"--mode", "scan-repo", "--format", "json", scanRepoFixture(t)}
	if code := run(args, &out, &errBuf); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%s)", code, errBuf.String())
	}
	var parsed struct {
		Candidates []struct {
			Path   string `json:"path"`
			Layout string `json:"layout"`
		} `json:"candidates"`
		Summary map[string]any `json:"summary"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("scan-repo JSON invalid: %v\n%s", err, out.String())
	}
	if len(parsed.Candidates) != 2 {
		t.Fatalf("want 2 candidates, got %d: %s", len(parsed.Candidates), out.String())
	}
	if parsed.Summary == nil {
		t.Errorf("scan-repo JSON missing summary: %s", out.String())
	}
}

func TestRun_DiscoveryJSON(t *testing.T) {
	client := fakeDiscovery{
		groups: []*metav1.APIGroup{
			{
				Name: "apps",
				Versions: []metav1.GroupVersionForDiscovery{
					{GroupVersion: "apps/v1", Version: "v1"},
				},
				PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "apps/v1", Version: "v1"},
			},
		},
		resources: []*metav1.APIResourceList{
			{
				GroupVersion: "apps/v1",
				APIResources: []metav1.APIResource{
					{Name: "deployments", SingularName: "deployment", Namespaced: true, Kind: "Deployment"},
				},
			},
		},
	}

	var out, errBuf bytes.Buffer
	if code := runWithDiscoveryClient([]string{"--mode", "discovery"}, &out, &errBuf, client); code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr=%s", code, errBuf.String())
	}

	var parsed discoveryDump
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("discovery JSON invalid: %v\n%s", err, out.String())
	}
	if got := parsed.Resources[0].APIResources[0].Name; got != "deployments" {
		t.Fatalf("resource name = %q, want deployments", got)
	}
}

func TestRun_DiscoveryPartialFailureStillDumps(t *testing.T) {
	failedGV := schema.GroupVersion{Group: "wardle.example.com", Version: "v1alpha1"}
	client := fakeDiscovery{
		resources: []*metav1.APIResourceList{{GroupVersion: "v1"}},
		err: &discovery.ErrGroupDiscoveryFailed{
			Groups: map[schema.GroupVersion]error{failedGV: errors.New("aggregated API unavailable")},
		},
	}

	var out, errBuf bytes.Buffer
	if code := runWithDiscoveryClient([]string{"--mode", "discovery"}, &out, &errBuf, client); code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr=%s", code, errBuf.String())
	}

	var parsed discoveryDump
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("discovery JSON invalid: %v\n%s", err, out.String())
	}
	got := parsed.FailedGroupVersions[failedGV.String()]
	if got != "aggregated API unavailable" {
		t.Fatalf("failed group/version = %q, want aggregated API unavailable", got)
	}
	if parsed.Error == "" {
		t.Fatal("expected partial discovery error to be included")
	}
}

func TestRun_Errors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no args", nil, 2},
		{"too many args", []string{"a", "b"}, 2},
		{"bad flag", []string{"--nope", "x"}, 2},
		{"bad mode", []string{"--mode", "delete", "x"}, 2},
		{"bad format", []string{"--format", "xml", "x"}, 2},
		{"bad policy", []string{"--policy", "delete", "x"}, 2},
		{"discovery rejects dir", []string{"--mode", "discovery", "x"}, 2},
		{"missing dir", []string{filepath.Join("definitely", "missing", "dir")}, 2},
		{"scan-folder missing dir", []string{"--mode", "scan-folder", filepath.Join("definitely", "missing")}, 2},
		{"scan-repo missing dir", []string{"--mode", "scan-repo", filepath.Join("definitely", "missing")}, 2},
		{"scan-repo no args", []string{"--mode", "scan-repo"}, 2},
		// The pre-freeze names carry no back-compat alias: they are usage errors, not
		// silent fallbacks onto the default analyze mode.
		{"retired scan mode", []string{"--mode", "scan", "x"}, 2},
		{"retired repo-walker mode", []string{"--mode", "repo-walker", "x"}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errBuf bytes.Buffer
			if code := run(c.args, &out, &errBuf); code != c.want {
				t.Errorf("exit = %d, want %d (stderr=%s)", code, c.want, errBuf.String())
			}
		})
	}
}

type fakeDiscovery struct {
	groups    []*metav1.APIGroup
	resources []*metav1.APIResourceList
	err       error
}

func (f fakeDiscovery) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	return f.groups, f.resources, f.err
}

func runWithDiscoveryClient(args []string, stdout, stderr io.Writer, client discoveryClient) int {
	return runWithDiscoveryClientFactory(args, stdout, stderr, func(_, _ string) (discoveryClient, error) {
		return client, nil
	})
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
