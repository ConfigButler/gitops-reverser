// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"context"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// foreignPaths returns the foreign entries of a store keyed by path → kind, for terse
// assertions.
func foreignPaths(store *ManifestStore) map[string]ForeignKind {
	out := map[string]ForeignKind{}
	for _, f := range store.Foreign {
		out[f.Path] = f.Kind
	}
	return out
}

// accHasIssue reports whether the acceptance carries an issue of the given kind at path.
func accHasIssue(acc Acceptance, kind IssueKind, path string) bool {
	for _, iss := range acc.Issues {
		if iss.Kind == kind && iss.Path == path {
			return true
		}
	}
	return false
}

func TestForeignContent_NonYAMLFileRefused(t *testing.T) {
	fsys := fstest.MapFS{
		"deploy.yaml":     {Data: []byte(deployYAML)},
		"secrets.txt":     {Data: []byte("db-password=hunter2")},
		"deploy.sh":       {Data: []byte("#!/bin/sh\n")},
		"blob.bin":        {Data: []byte{0x00, 0x01}},
		"sub/values.json": {Data: []byte(`{"k":"v"}`)},
	}
	store := BuildStore(context.Background(), fsys, nil)

	got := foreignPaths(store)
	for _, want := range []string{"secrets.txt", "deploy.sh", "blob.bin", "sub/values.json"} {
		if got[want] != ForeignFile {
			t.Errorf("path %q: foreign kind = %q, want %q", want, got[want], ForeignFile)
		}
	}
	if len(got) != 4 {
		t.Errorf("foreign entries = %+v, want exactly the four non-YAML files", store.Foreign)
	}

	acc := AcceptStructureOnly(store)
	if acc.Accepted {
		t.Fatal("expected refusal for a folder holding foreign non-YAML files")
	}
	if !accHasIssue(acc, IssueForeignFile, "secrets.txt") {
		t.Errorf("expected an IssueForeignFile for secrets.txt; got %+v", acc.Issues)
	}
}

func TestBenignPassenger_AcceptedByDefault(t *testing.T) {
	// Inert repo-hygiene files — docs, a license, and Git metadata — are accepted without a
	// .gittargetignore, so adopting an existing repo does not refuse the whole folder over a
	// LICENSE, a stray .gitkeep, or documentation that is not the operator's own README.
	fsys := fstest.MapFS{
		"deploy.yaml":         {Data: []byte(deployYAML)},
		"LICENSE":             {Data: []byte("Apache-2.0")},
		"COPYING":             {Data: []byte("legal")},
		"CONTRIBUTING.md":     {Data: []byte("# contributing")},
		"docs/guide.markdown": {Data: []byte("# guide")},
		".gitignore":          {Data: []byte("*.log\n")},
		".gitattributes":      {Data: []byte("*.yaml text\n")},
		"sub/.gitkeep":        {Data: []byte("")},
	}
	store := BuildStore(context.Background(), fsys, nil)

	if len(store.Foreign) != 0 {
		t.Fatalf("benign-passenger hygiene files must not be foreign; got %+v", store.Foreign)
	}
	if acc := AcceptStructureOnly(store); !acc.Accepted {
		t.Errorf("expected acceptance for a folder of hygiene passengers; got %+v", acc.Issues)
	}
	// They are still recorded in the non-YAML inventory (accepted, never managed), and the
	// managed manifest is modeled as usual.
	scan := collectFiles(fsys)
	for _, want := range []string{"LICENSE", "CONTRIBUTING.md", "sub/.gitkeep", ".gitignore"} {
		if !containsString(scan.NonYAML, want) {
			t.Errorf("%q should appear in the non-YAML inventory; got %+v", want, scan.NonYAML)
		}
	}
	if _, ok := store.FilesByPath["deploy.yaml"]; !ok {
		t.Error("the managed manifest must still be modeled alongside benign passengers")
	}
}

func TestBenignPassenger_StillUserSuppressible(t *testing.T) {
	// A benign passenger is USER content, matched after the ignore filter, so a user can still
	// .gittargetignore it to drop it from the inventory entirely (unlike an operator artifact,
	// which survives an ignore rule).
	fsys := fstest.MapFS{
		"deploy.yaml":      {Data: []byte(deployYAML)},
		"NOTES.md":         {Data: []byte("# notes")},
		".gittargetignore": {Data: []byte("NOTES.md\n")},
	}
	scan := collectFiles(fsys)
	if containsString(scan.NonYAML, "NOTES.md") {
		t.Error("a benign passenger named in .gittargetignore must be dropped (never read)")
	}
	if len(scan.Foreign) != 0 {
		t.Errorf("an ignored passenger is not foreign; got %+v", scan.Foreign)
	}
}

func TestIsBenignPassenger(t *testing.T) {
	accepted := []string{
		"LICENSE", "LICENSE.txt", "LICENCE", "COPYING", "NOTICE",
		".gitignore", ".gitattributes", ".gitkeep", ".keep",
		"README.md", "notes.md", "a/b/guide.markdown",
	}
	for _, p := range accepted {
		if !isBenignPassenger(p) {
			t.Errorf("isBenignPassenger(%q) = false, want true", p)
		}
	}
	refused := []string{
		"notes.txt", "values.json", "deploy.sh", "Chart.yaml", "blob.bin",
		"license", "readme", "MD", "sub.markdown.tar",
	}
	for _, p := range refused {
		if isBenignPassenger(p) {
			t.Errorf("isBenignPassenger(%q) = true, want false", p)
		}
	}
}

func TestForeignContent_OperatorArtifactsAccepted(t *testing.T) {
	// README.md is an operator artifact (role 3); the root .gittargetignore is recognized
	// positionally; a deeply nested README.md is still basename-matched as an artifact.
	fsys := fstest.MapFS{
		"deploy.yaml":      {Data: []byte(deployYAML)},
		"README.md":        {Data: []byte("# bootstrap")},
		".gittargetignore": {Data: []byte("# nothing ignored\n")},
		"nested/README.md": {Data: []byte("# sub readme")},
	}
	store := BuildStore(context.Background(), fsys, nil)
	if len(store.Foreign) != 0 {
		t.Fatalf("operator artifacts must not be foreign; got %+v", store.Foreign)
	}
	if acc := AcceptStructureOnly(store); !acc.Accepted {
		t.Errorf("expected acceptance with only operator artifacts; got %+v", acc.Issues)
	}
}

func TestForeignContent_SymlinkRefused(t *testing.T) {
	fsys := fstest.MapFS{
		"deploy.yaml": {Data: []byte(deployYAML)},
		"link":        {Mode: fs.ModeSymlink | 0o777, Data: []byte("deploy.yaml")},
		"escape":      {Mode: fs.ModeSymlink | 0o777, Data: []byte("/etc/passwd")},
	}
	store := BuildStore(context.Background(), fsys, nil)

	got := foreignPaths(store)
	if got["link"] != ForeignSymlink || got["escape"] != ForeignSymlink {
		t.Fatalf("symlinks must be foreign; got %+v", store.Foreign)
	}
	acc := AcceptStructureOnly(store)
	if !accHasIssue(acc, IssueForeignSymlink, "escape") {
		t.Errorf("expected an IssueForeignSymlink for the escaping symlink; got %+v", acc.Issues)
	}
}

func TestForeignContent_NestedGitTargetIgnoreRefused(t *testing.T) {
	// A .gittargetignore deeper than the root is NOT honoured and is refused as foreign,
	// unless the root file ignores it (D-foreign-2).
	fsys := fstest.MapFS{
		"deploy.yaml":          {Data: []byte(deployYAML)},
		"sub/.gittargetignore": {Data: []byte("*.yaml\n")},
	}
	store := BuildStore(context.Background(), fsys, nil)
	if foreignPaths(store)["sub/.gittargetignore"] != ForeignFile {
		t.Fatalf("a nested .gittargetignore must be foreign; got %+v", store.Foreign)
	}
	if acc := AcceptStructureOnly(store); acc.Accepted {
		t.Error("expected refusal for a nested .gittargetignore")
	}
}

func TestForeignContent_GitDirSkipped(t *testing.T) {
	// The VCS metadata directory is never descended, so none of its contents are foreign.
	fsys := fstest.MapFS{
		"deploy.yaml":     {Data: []byte(deployYAML)},
		".git/config":     {Data: []byte("[core]\n")},
		".git/objects/ab": {Data: []byte("blob")},
		".git/HEAD":       {Data: []byte("ref: refs/heads/main")},
	}
	store := BuildStore(context.Background(), fsys, nil)
	if len(store.Foreign) != 0 {
		t.Fatalf(".git contents must be skipped, not refused; got %+v", store.Foreign)
	}
	if acc := AcceptStructureOnly(store); !acc.Accepted {
		t.Errorf("expected acceptance; .git should be invisible. issues=%+v", acc.Issues)
	}
}

func TestGitTargetIgnore_FileNeverRead(t *testing.T) {
	// An ignored file — even a YAML one — is never read: it does not become managed, is not
	// classified, and cannot trigger a foreign refusal.
	fsys := fstest.MapFS{
		"deploy.yaml":      {Data: []byte(deployYAML)},
		"notes.txt":        {Data: []byte("loose notes")},
		"legacy/old.yaml":  {Data: []byte("not: even: valid: krm")},
		".gittargetignore": {Data: []byte("*.txt\nlegacy/\n")},
	}
	store := BuildStore(context.Background(), fsys, nil)

	if len(store.Foreign) != 0 {
		t.Errorf("ignored entries must not be foreign; got %+v", store.Foreign)
	}
	if _, ok := store.FilesByPath["legacy/old.yaml"]; ok {
		t.Error("an ignored YAML file must never be read into the model")
	}
	if _, ok := store.FilesByPath["deploy.yaml"]; !ok {
		t.Error("a non-ignored managed YAML file must still be modeled")
	}
	if acc := AcceptStructureOnly(store); !acc.Accepted {
		t.Errorf("expected acceptance once foreign files are ignored; got %+v", acc.Issues)
	}
}

func TestGitTargetIgnore_SubtreeNeverRead(t *testing.T) {
	// A directory pattern prunes the whole subtree at the walk, so nothing under it is read.
	fsys := fstest.MapFS{
		"deploy.yaml":      {Data: []byte(deployYAML)},
		"docs/guide.md":    {Data: []byte("# guide")},
		"docs/img/a.bin":   {Data: []byte{0x00}},
		".gittargetignore": {Data: []byte("docs/\n")},
	}
	store := BuildStore(context.Background(), fsys, nil)
	if len(store.Foreign) != 0 {
		t.Fatalf("a pruned subtree yields no foreign entries; got %+v", store.Foreign)
	}
	if acc := AcceptStructureOnly(store); !acc.Accepted {
		t.Errorf("expected acceptance with the docs subtree ignored; got %+v", acc.Issues)
	}
}

func TestGitTargetIgnore_RootFileRecognized(t *testing.T) {
	// The one root .gittargetignore is recognized at its exact path: never modeled, never
	// foreign, and its patterns build the matcher.
	fsys := fstest.MapFS{
		"deploy.yaml":      {Data: []byte(deployYAML)},
		".gittargetignore": {Data: []byte("*.txt\n")},
	}
	store := BuildStore(context.Background(), fsys, nil)
	if store.Ignore == nil {
		t.Fatal("the root .gittargetignore should have produced a matcher")
	}
	if _, ok := store.FilesByPath[".gittargetignore"]; ok {
		t.Error(".gittargetignore must not be modeled as managed content")
	}
	if len(store.Foreign) != 0 {
		t.Errorf(".gittargetignore must not be foreign; got %+v", store.Foreign)
	}
}

func TestGitTargetIgnore_CatastrophicPatternRefused(t *testing.T) {
	for _, pattern := range []string{"*", "**", "/", "*.yaml", "*.yml", "**/*"} {
		t.Run(pattern, func(t *testing.T) {
			fsys := fstest.MapFS{
				"deploy.yaml":      {Data: []byte(deployYAML)},
				".gittargetignore": {Data: []byte(pattern + "\n")},
			}
			store := BuildStore(context.Background(), fsys, nil)
			acc := AcceptStructureOnly(store)
			if acc.Accepted {
				t.Fatalf("catastrophic pattern %q must fail the GitTarget", pattern)
			}
			if !accHasIssue(acc, IssueIgnoreShadowsManaged, GitTargetIgnoreFileName) {
				t.Errorf("expected IssueIgnoreShadowsManaged for %q; got %+v", pattern, acc.Issues)
			}
		})
	}
}

func TestGitTargetIgnore_ImmuneFilesNotHidden(t *testing.T) {
	// Operator artifacts and build directives are matched before the ignore filter, so a
	// user cannot use .gittargetignore to hide them. A managed manifest, by contrast, CAN be
	// ignored (it is matched after the filter).
	fsys := fstest.MapFS{
		"deploy.yaml":      {Data: []byte(deployYAML)},
		"README.md":        {Data: []byte("# readme")},
		".gittargetignore": {Data: []byte("README.md\ndeploy.yaml\n")},
	}
	scan := collectFiles(fsys)

	if !containsString(scan.NonYAML, "README.md") {
		t.Error("README.md is an operator artifact and must survive an ignore rule")
	}
	if containsFileContent(scan.YAMLFiles, "deploy.yaml") {
		t.Error("a managed manifest named in .gittargetignore must be dropped (never read)")
	}
}

func TestIgnoreMatcher_MatchAndPattern(t *testing.T) {
	matcher, issues := LoadGitTargetIgnore([]byte(
		"# a comment\n\n*.md\ndocs/\n!docs/keep.md\nlegacy/old.yaml\n"))
	if len(issues) != 0 {
		t.Fatalf("no catastrophic patterns here; got %+v", issues)
	}
	cases := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"notes.md", false, true},
		{"deploy.yaml", false, false},
		{"docs", true, true},
		{"docs/guide.txt", false, true},
		{"docs/keep.md", false, false}, // negation wins as the last matching rule
		{"legacy/old.yaml", false, true},
	}
	for _, c := range cases {
		if got := matcher.Match(c.path, c.isDir); got != c.want {
			t.Errorf("Match(%q, %v) = %v, want %v", c.path, c.isDir, got, c.want)
		}
	}
	if pat := matcher.MatchingPattern("notes.md", false); pat != "*.md" {
		t.Errorf("MatchingPattern(notes.md) = %q, want *.md", pat)
	}
	if pat := matcher.MatchingPattern("deploy.yaml", false); pat != "" {
		t.Errorf("MatchingPattern(deploy.yaml) = %q, want empty (not ignored)", pat)
	}

	// A nil matcher matches nothing and need no nil guard at the call site.
	var nilMatcher *IgnoreMatcher
	if nilMatcher.Match("anything", false) {
		t.Error("a nil matcher must never match")
	}
}

func TestLoadGitTargetIgnore_EmptyAndCommentOnly(t *testing.T) {
	matcher, issues := LoadGitTargetIgnore([]byte("# only comments\n\n   \n"))
	if matcher != nil {
		t.Error("a comment-only file should yield a nil matcher (nothing ignored)")
	}
	if len(issues) != 0 {
		t.Errorf("a comment-only file earns no issues; got %+v", issues)
	}
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func containsFileContent(files []manifestedit.FileContent, want string) bool {
	for _, f := range files {
		if f.Path == want {
			return true
		}
	}
	return false
}
