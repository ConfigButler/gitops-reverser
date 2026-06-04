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

package manifestanalyzer

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

const (
	deployYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: default
spec:
  replicas: 1
`
	configMapsYAML = `apiVersion: v1
kind: ConfigMap
metadata:
  name: a
  namespace: default
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: b
  namespace: default
`
	sopsSecretYAML = `apiVersion: v1
kind: Secret
metadata:
  name: db
  namespace: default
sops:
  version: "3"
`
	plainYAML  = "foo: bar\nbaz: qux\n"
	brokenYAML = "foo: [bar\n"
	emptyYAML  = "# only a comment\n"
	notesText  = "just some notes\n"
)

// sampleFS builds the canonical mixed tree used across tests. deploy.yaml sorts
// before dup.yaml, so the Deployment in dup.yaml is the duplicate loser.
func sampleFS() fstest.MapFS {
	return fstest.MapFS{
		"deploy.yaml":      {Data: []byte(deployYAML)},
		"cm.yaml":          {Data: []byte(configMapsYAML)},
		"dup.yaml":         {Data: []byte(deployYAML)},
		"plain.yaml":       {Data: []byte(plainYAML)},
		"broken.yaml":      {Data: []byte(brokenYAML)},
		"empty.yaml":       {Data: []byte(emptyYAML)},
		"secret.sops.yaml": {Data: []byte(sopsSecretYAML)},
		"docs/notes.txt":   {Data: []byte(notesText)},
	}
}

func TestAnalyze_Summary(t *testing.T) {
	s := Analyze(sampleFS()).Summary

	if s.FilesTotal != 8 || s.YAMLFiles != 7 || s.NonYAMLFiles != 1 {
		t.Fatalf("file counts: total=%d yaml=%d nonyaml=%d", s.FilesTotal, s.YAMLFiles, s.NonYAMLFiles)
	}
	if s.Documents != 8 {
		t.Fatalf("documents = %d, want 8", s.Documents)
	}
	if s.Duplicates != 1 || s.Encrypted != 1 {
		t.Fatalf("duplicates=%d encrypted=%d, want 1 and 1", s.Duplicates, s.Encrypted)
	}

	// Every enum key is listed so the exhaustive linter guards future additions.
	wantClass := map[Class]int{
		ClassKRM:         5, // deploy, cm a, cm b, secret, dup
		ClassNonKRM:      1,
		ClassInvalidYAML: 1,
		ClassEmpty:       1,
		ClassNonYAML:     0,
	}
	for c, n := range wantClass {
		if s.ByClass[c] != n {
			t.Errorf("class %s = %d, want %d", c, s.ByClass[c], n)
		}
	}

	// The GVK inventory reports every GVK found, regardless of any API.
	wantGVK := map[string]int{"apps/v1/Deployment": 2, "v1/ConfigMap": 2, "v1/Secret": 1}
	for g, n := range wantGVK {
		if s.ByGVK[g] != n {
			t.Errorf("gvk %s = %d, want %d", g, s.ByGVK[g], n)
		}
	}
}

func TestAnalyze_Issues(t *testing.T) {
	rep := Analyze(sampleFS())
	want := map[IssueKind]int{
		IssueDuplicate:   1,
		IssueNonKRM:      1,
		IssueInvalidYAML: 1,
	}
	for kind, n := range want {
		if got := countIssues(rep, kind); got != n {
			t.Errorf("%s issues = %d, want %d", kind, got, n)
		}
	}
}

func TestAnalyze_DocumentDetail(t *testing.T) {
	rep := Analyze(sampleFS())
	cm := findFile(t, rep, "cm.yaml")
	if len(cm.Documents) != 2 {
		t.Fatalf("cm.yaml documents = %d, want 2", len(cm.Documents))
	}
	if cm.Documents[0].Identity.Name != "a" || cm.Documents[1].Identity.Name != "b" {
		t.Errorf("cm.yaml doc names = %q,%q", cm.Documents[0].Identity.Name, cm.Documents[1].Identity.Name)
	}

	secret := findFile(t, rep, "secret.sops.yaml")
	if len(secret.Documents) != 1 || !secret.Documents[0].Encrypted {
		t.Errorf("secret.sops.yaml should be a single encrypted document: %+v", secret.Documents)
	}

	dup := findFile(t, rep, "dup.yaml")
	if len(dup.Documents) != 1 || !dup.Documents[0].Duplicate {
		t.Errorf("dup.yaml document should be marked duplicate: %+v", dup.Documents)
	}

	notes := findFile(t, rep, "docs/notes.txt")
	if notes.IsYAML || len(notes.Documents) != 0 {
		t.Errorf("notes.txt should be non-yaml with no documents: %+v", notes)
	}

	plain := findFile(t, rep, "plain.yaml")
	if len(plain.Documents) != 1 || plain.Documents[0].Class != ClassNonKRM {
		t.Errorf("plain.yaml should be a single non-krm document: %+v", plain.Documents)
	}
}

func TestAnalyze_NonEditableRecord(t *testing.T) {
	const anchored = `apiVersion: v1
kind: ConfigMap
metadata:
  name: anchored
  namespace: default
data: &a
  k: v
`
	rep := Analyze(fstest.MapFS{"anchored.yaml": {Data: []byte(anchored)}})
	doc := findFile(t, rep, "anchored.yaml").Documents[0]
	if doc.Editable {
		t.Errorf("anchored document should be non-editable")
	}
	if doc.Class != ClassKRM {
		t.Errorf("anchored document class = %s, want krm", doc.Class)
	}
	if doc.Reason == "" {
		t.Errorf("non-editable document should carry a reason")
	}
}

func TestAnalyze_EmptyTree(t *testing.T) {
	rep := Analyze(fstest.MapFS{})
	if rep.Summary.FilesTotal != 0 || len(rep.Issues) != 0 {
		t.Errorf("empty tree should yield no files and no issues: %+v", rep.Summary)
	}
}

func TestAnalyzeDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "deploy.yaml", deployYAML)
	writeFile(t, dir, "notes.txt", notesText)

	rep, err := AnalyzeDir(dir)
	if err != nil {
		t.Fatalf("AnalyzeDir: %v", err)
	}
	if rep.Root != dir {
		t.Errorf("root = %q, want %q", rep.Root, dir)
	}
	if rep.Summary.Documents != 1 || rep.Summary.NonYAMLFiles != 1 {
		t.Errorf("unexpected summary: %+v", rep.Summary)
	}
}

func TestAnalyzeDir_SymlinkSkipped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "deploy.yaml", deployYAML)
	if err := os.Symlink(filepath.Join(dir, "deploy.yaml"), filepath.Join(dir, "link.yaml")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	rep, err := AnalyzeDir(dir)
	if err != nil {
		t.Fatalf("AnalyzeDir: %v", err)
	}
	if !hasDiag(rep, "link.yaml", "symlink skipped") {
		t.Errorf("expected a symlink-skipped diagnostic for link.yaml: %+v", rep.Diagnostics)
	}
	if rep.Summary.YAMLFiles != 1 {
		t.Errorf("yaml files = %d, want 1 (symlink not counted)", rep.Summary.YAMLFiles)
	}
}

func TestAnalyzeDir_Errors(t *testing.T) {
	if _, err := AnalyzeDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("expected error for missing directory")
	}

	file := filepath.Join(t.TempDir(), "f.yaml")
	writeFile(t, filepath.Dir(file), "f.yaml", deployYAML)
	if _, err := AnalyzeDir(file); err == nil {
		t.Error("expected error when root is not a directory")
	}
}

// faultyFS injects read/walk failures to exercise the diagnostic branches.
type faultyFS struct {
	fstest.MapFS

	failReadFile string
	failReadDir  string
}

func (f faultyFS) ReadFile(name string) ([]byte, error) {
	if name == f.failReadFile {
		return nil, errors.New("synthetic read error")
	}
	return f.MapFS.ReadFile(name)
}

func (f faultyFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == f.failReadDir {
		return nil, errors.New("synthetic readdir error")
	}
	return f.MapFS.ReadDir(name)
}

func TestAnalyze_ReadFileError(t *testing.T) {
	fsys := faultyFS{
		MapFS:        fstest.MapFS{"bad.yaml": {Data: []byte(deployYAML)}},
		failReadFile: "bad.yaml",
	}
	rep := Analyze(fsys)
	if rep.Summary.YAMLFiles != 0 {
		t.Errorf("unreadable file should not be indexed: %+v", rep.Summary)
	}
	if !hasDiag(rep, "bad.yaml", "synthetic read error") {
		t.Errorf("expected read-error diagnostic: %+v", rep.Diagnostics)
	}
}

func TestAnalyze_WalkDirError(t *testing.T) {
	fsys := faultyFS{
		MapFS:       fstest.MapFS{"sub/deploy.yaml": {Data: []byte(deployYAML)}},
		failReadDir: "sub",
	}
	rep := Analyze(fsys)
	if !hasDiag(rep, "sub", "synthetic readdir error") {
		t.Errorf("expected walk-error diagnostic for sub: %+v", rep.Diagnostics)
	}
}

func TestParseGVK(t *testing.T) {
	cases := []struct {
		apiVersion, kind string
		want             GVK
		str              string
	}{
		{"apps/v1", "Deployment", GVK{"apps", "v1", "Deployment"}, "apps/v1/Deployment"},
		{"v1", "ConfigMap", GVK{"", "v1", "ConfigMap"}, "v1/ConfigMap"},
	}
	for _, c := range cases {
		got := ParseGVK(c.apiVersion, c.kind)
		if got != c.want {
			t.Errorf("ParseGVK(%q,%q) = %+v, want %+v", c.apiVersion, c.kind, got, c.want)
		}
		if got.String() != c.str {
			t.Errorf("String() = %q, want %q", got.String(), c.str)
		}
		if got.Empty() {
			t.Errorf("%+v should not be Empty", got)
		}
	}
	if !(GVK{}).Empty() {
		t.Error("zero GVK should be Empty")
	}
}

// --- helpers ---

func countIssues(rep Report, kind IssueKind) int {
	n := 0
	for _, is := range rep.Issues {
		if is.Kind == kind {
			n++
		}
	}
	return n
}

func findFile(t *testing.T, rep Report, path string) FileReport {
	t.Helper()
	for _, f := range rep.Files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("file %q not found in report", path)
	return FileReport{}
}

func hasDiag(rep Report, path, substr string) bool {
	for _, d := range rep.Diagnostics {
		if d.Path == path && strings.Contains(d.Message, substr) {
			return true
		}
	}
	return false
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
