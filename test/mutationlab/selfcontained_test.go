// SPDX-License-Identifier: Apache-2.0

// Package mutationlab holds the mutation-capture lab's cluster wiring. This test keeps that
// wiring self-contained: the lab once borrowed the product's admission registration, certificate,
// and Deployment, and lost its admission source when the product removed them, with every check
// still green.
package mutationlab

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const repoRoot = "../.."

// sharedPaths are the only files outside the lab's own directories that the lab's wiring may use.
// Each is shared on purpose, and changing one changes both clusters the same way.
var sharedPaths = []string{
	"test/e2e/cluster/start-cluster.sh",                // creates both clusters
	"test/e2e/cluster/audit/policy.yaml",               // one audit policy, so the corpus shows what e2e audits
	"test/e2e/setup/manifests/sample-apiserver",        // the same aggregated API server
	"test/mutationlab",                                 // the lab's wiring
	"cmd/mutation-capture-lab", "internal/mutationlab", // the lab binary
}

// labFiles are the files the check reads: the lab binary and its wiring, without the corpus
// (data the API server wrote) and the Markdown (prose about the product is fine).
func labFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, dir := range []string{"test/mutationlab", "cmd/mutation-capture-lab", "internal/mutationlab"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "corpus" {
				return filepath.SkipDir
			}
			if d.IsDir() || strings.HasSuffix(path, ".md") || strings.HasSuffix(path, "selfcontained_test.go") {
				return nil
			}
			files = append(files, path)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	return files
}

// TestLabTasksDependOnLabTasksOnly fails when a lab task runs or depends on a task the lab's
// Taskfile does not define, which is how the lab came to depend on the product's install.
func TestLabTasksDependOnLabTasksOnly(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot, "test/mutationlab/Taskfile-lab.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var tf struct {
		Tasks map[string]struct {
			Deps []any `json:"deps"`
			Cmds []any `json:"cmds"`
		} `json:"tasks"`
	}
	if err := yaml.Unmarshal(raw, &tf); err != nil {
		t.Fatalf("parse Taskfile-lab.yml: %v", err)
	}
	for name, task := range tf.Tasks {
		for _, ref := range append(taskRefs(task.Deps, true), taskRefs(task.Cmds, false)...) {
			if _, ok := tf.Tasks[ref]; !ok {
				t.Errorf("lab task %s calls %s, which Taskfile-lab.yml does not define", name, ref)
			}
		}
	}
}

// taskRefs returns the task names a deps or cmds list calls. A deps entry is a name or a
// {task: name} map; a cmds entry calls a task only in the map form.
func taskRefs(entries []any, bareNames bool) []string {
	var refs []string
	for _, e := range entries {
		switch v := e.(type) {
		case string:
			if bareNames {
				refs = append(refs, v)
			}
		case map[string]any:
			if name, ok := v["task"].(string); ok {
				refs = append(refs, name)
			}
		}
	}
	return refs
}

var repoPath = regexp.MustCompile(`\b(?:test|hack|config|charts|cmd|internal|api)/[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*`)

// TestLabUsesOnlySharedPaths fails when the lab's wiring (its Taskfile, manifests, and scripts)
// reads a repository path outside its own directories and the few it shares on purpose.
func TestLabUsesOnlySharedPaths(t *testing.T) {
	for _, file := range labFiles(t) {
		if strings.HasSuffix(file, ".go") {
			// Go names packages, not files; the compiler and the name check below cover it.
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range uniq(repoPath.FindAllString(string(raw), -1)) {
			if !shared(p) {
				t.Errorf("%s uses %s, outside the lab and the paths it shares (sharedPaths)", rel(file), p)
			}
		}
	}
}

func shared(p string) bool {
	for _, s := range sharedPaths {
		if p == s || strings.HasPrefix(p, s+"/") {
			return true
		}
	}
	return false
}

// TestLabNamesNoProductResource fails when the lab names something the product installs: its
// namespace, Deployment, Services, certificates, webhook registrations, or audit routes all carry
// the gitops-reverser name. Two spellings of it are the lab's own: the Go module path, and the lab
// cluster's name.
func TestLabNamesNoProductResource(t *testing.T) {
	allowed := strings.NewReplacer(
		"github.com/ConfigButler/gitops-reverser", "",
		"gitops-reverser-capture-lab", "",
	)
	for _, file := range labFiles(t) {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(allowed.Replace(line), "gitops-reverser") {
				t.Errorf("%s:%d names a product resource: %s", rel(file), i+1, strings.TrimSpace(line))
			}
		}
	}
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func rel(path string) string {
	r, err := filepath.Rel(repoRoot, path)
	if err != nil {
		return path
	}
	return r
}
