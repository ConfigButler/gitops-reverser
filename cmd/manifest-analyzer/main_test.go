/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestRun_Errors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no args", nil, 2},
		{"too many args", []string{"a", "b"}, 2},
		{"bad flag", []string{"--nope", "x"}, 2},
		{"bad format", []string{"--format", "xml", "x"}, 2},
		{"bad policy", []string{"--policy", "delete", "x"}, 2},
		{"missing dir", []string{filepath.Join("definitely", "missing", "dir")}, 2},
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

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
