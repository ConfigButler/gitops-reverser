// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// write creates dir/name with content and returns its full path.
func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func targets(findings []finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.target)
	}
	return out
}

// gitRun runs git in dir. The caller treats any error as "git is unavailable here"
// and skips, so the combined output is folded into the error to make that decision
// debuggable rather than silent.
func gitRun(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return nil
}

func TestCheckMarkdown_ReportsOnlyUnresolvableRelativeLinks(t *testing.T) {
	root := t.TempDir()
	write(t, root, "docs/real.md", "# real\n")
	write(t, root, "docs/dir/nested.md", "# nested\n")

	doc := write(t, root, "docs/index.md", `# index

[resolves](real.md)
[resolves with anchor](real.md#a-heading)
[resolves nested](dir/nested.md)
[external http](http://example.com/missing.md)
[external https](https://example.com/missing.md)
[mailto](mailto:someone@example.com)
[pure anchor](#a-heading)
[broken](gone.md)
[broken with anchor](also-gone.md#x)
`)

	got := targets(checkMarkdown(root, doc))

	want := []string{"gone.md", "also-gone.md#x"}
	if len(got) != len(want) {
		t.Fatalf("expected exactly %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("finding %d: want %q, got %q", i, want[i], got[i])
		}
	}
}

func TestCheckMarkdown_ReportsRepoRelativePathAndLine(t *testing.T) {
	root := t.TempDir()
	doc := write(t, root, "docs/design/plan.md", "line one\n\n[broken](../nope.md)\n")

	findings := checkMarkdown(root, doc)
	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d", len(findings))
	}
	f := findings[0]

	if want := filepath.Join("docs", "design", "plan.md"); f.file != want {
		t.Errorf("file: want %q, got %q", want, f.file)
	}
	if f.line != 3 {
		t.Errorf("line: want 3, got %d", f.line)
	}
	if f.kind != "markdown" {
		t.Errorf("kind: want markdown, got %q", f.kind)
	}
}

func TestCheckMarkdown_UnreadableFileIsNotAFinding(t *testing.T) {
	root := t.TempDir()
	if got := checkMarkdown(root, filepath.Join(root, "absent.md")); got != nil {
		t.Errorf("a file that cannot be read must yield no findings, got %v", got)
	}
}

// The package doc's load-bearing claim: doc paths are read from COMMENTS, never
// from string literals. The gittargetignore tests build in-memory filesystems whose
// entries are named like documentation paths, and those are fixtures, not citations.
func TestCheckGoComments_ReadsCommentsAndIgnoresStringLiterals(t *testing.T) {
	root := t.TempDir()
	write(t, root, "docs/spec/real.md", "# real\n")

	src := write(t, root, "pkg/thing.go", `// Package thing implements the contract in docs/spec/real.md.
package thing

// doThing is specified by docs/spec/missing.md and must stay in sync.
func doThing() string {
	// this citation is also a comment: docs/spec/other-missing.md
	return "docs/spec/not-a-citation.md"
}
`)

	got := targets(checkGoComments(root, src))

	// The path that resolves is absent from the findings, and the one in the string
	// literal is never considered at all. (Spelling that resolvable path out here
	// would make this very comment a citation doccheck then fails to resolve.)
	want := map[string]bool{
		"docs/spec/missing.md":       true,
		"docs/spec/other-missing.md": true,
	}
	if len(got) != len(want) {
		t.Fatalf("want %d findings %v, got %d: %v", len(want), want, len(got), got)
	}
	for _, target := range got {
		if !want[target] {
			t.Errorf("unexpected finding %q — a string literal must never be treated as a citation", target)
		}
	}
}

// A Go comment linking to the published docs site is not a repo-relative citation.
// doccheck's own package doc contains such a URL, and reported itself as broken.
func TestCheckGoComments_IgnoresDocsPathsInsideURLs(t *testing.T) {
	root := t.TempDir()
	src := write(t, root, "pkg/thing.go", `// Package thing is described at
// https://github.com/ConfigButler/gitops-reverser/blob/main/docs/spec/gone.md
// and its contract is docs/spec/gone.md.
package thing
`)

	got := targets(checkGoComments(root, src))
	if len(got) != 1 || got[0] != "docs/spec/gone.md" {
		t.Errorf("the URL must not be reported and the bare citation must be; got %v", got)
	}
}

func TestCheckGoComments_UnparseableFileIsNotAFinding(t *testing.T) {
	root := t.TempDir()
	src := write(t, root, "broken.go", "this is not go source at all, and cites docs/spec/missing.md\n")

	if got := checkGoComments(root, src); got != nil {
		t.Errorf("a file that does not parse is golangci-lint's problem, not ours; got %v", got)
	}
}

// The surface that was missing: a docs reorg repointed every Markdown link and Go
// comment, and left eight citations dangling in Taskfiles, workflows and scripts.
func TestCheckTextCitations_ResolvesYAMLAndShellCitations(t *testing.T) {
	root := t.TempDir()
	write(t, root, "docs/spec/real.md", "# real\n")

	yml := write(t, root, "Taskfile.yml", `version: '3'
tasks:
  build:
    # The contract lives in docs/spec/real.md.
    # This one moved: docs/design/moved.md
    cmds:
      - echo docs/spec/also-missing.md
`)

	got := targets(checkTextCitations(root, yml, "yaml"))
	want := []string{"docs/design/moved.md", "docs/spec/also-missing.md"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i, target := range got {
		if target != want[i] {
			t.Errorf("finding %d: want %q, got %q", i, want[i], target)
		}
	}
}

// A docs path at the tail of a URL is a rendered page, not a file in this repo.
// Without this, every link to the published docs would be reported as dangling.
func TestCheckTextCitations_IgnoresDocsPathsInsideURLs(t *testing.T) {
	root := t.TempDir()
	sh := write(t, root, "hack/thing.sh", `#!/usr/bin/env bash
# See https://github.com/ConfigButler/gitops-reverser/blob/main/docs/spec/gone.md
# but this one is a real citation: docs/spec/gone.md
`)

	got := targets(checkTextCitations(root, sh, "shell"))
	if len(got) != 1 || got[0] != "docs/spec/gone.md" {
		t.Errorf("the URL must not be reported and the bare path must be; got %v", got)
	}
}

func TestTrackedFiles_ListsGitTrackedFilesOnly(t *testing.T) {
	root := t.TempDir()
	write(t, root, "tracked.md", "# tracked\n")
	write(t, root, "untracked.md", "# untracked\n")

	ctx := context.Background()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "doccheck@example.com"},
		{"config", "user.name", "doccheck"},
		{"add", "tracked.md"},
	} {
		if err := gitRun(ctx, root, args...); err != nil {
			t.Skipf("git unavailable in this environment: %v", err)
		}
	}

	names, err := trackedFiles(ctx, root)
	if err != nil {
		t.Fatalf("trackedFiles: %v", err)
	}

	if len(names) != 1 || names[0] != "tracked.md" {
		t.Errorf("want [tracked.md], got %v — untracked files must never be scanned", names)
	}
}

func TestTrackedFiles_NonRepoIsAnError(t *testing.T) {
	if _, err := trackedFiles(context.Background(), t.TempDir()); err == nil {
		t.Error("a directory that is not a git repository must be an error, not an empty list")
	}
}

func TestIsExternal(t *testing.T) {
	for target, want := range map[string]bool{
		"http://example.com":       true,
		"https://example.com":      true,
		"mailto:a@example.com":     true,
		"#anchor":                  true,
		"docs/spec/thing.md":       false,
		"../README.md":             false,
		"./relative.md":            false,
		"httpsnot-a-scheme.md":     false,
		"notes.md#https://not-url": false,
	} {
		if got := isExternal(target); got != want {
			t.Errorf("isExternal(%q): want %v, got %v", target, want, got)
		}
	}
}

func TestRel_FallsBackToTheAbsolutePath(t *testing.T) {
	if got := rel("/a/b", "/a/b/c.md"); got != "c.md" {
		t.Errorf("want c.md, got %q", got)
	}
	// A relative path cannot be computed from an absolute root to a relative target.
	if got := rel("/a/b", "c.md"); got != "c.md" {
		t.Errorf("want the path back unchanged, got %q", got)
	}
}
