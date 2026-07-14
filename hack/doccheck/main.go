// SPDX-License-Identifier: Apache-2.0

// Command doccheck verifies that every documentation reference in the repository
// resolves to a file that exists.
//
// It checks three surfaces that nothing off-the-shelf covers together:
//
//  1. Relative links in Markdown — `[text](../spec/foo.md)`, including links that
//     point at source files. External URLs and pure `#anchor` links are skipped.
//
//  2. Repo-relative Markdown paths cited inside **Go comments** — the
//     `docs/spec/manifest-system.md`-style references the packages use to point at
//     their contract, and the ones that live next to the code they describe, like
//     `internal/git/manifestedit/DECISION.md`. These are the ones that rot silently:
//     a doc gets moved, the comment keeps pointing at the old path, and nothing
//     notices. Seventeen of them were dangling before this check existed.
//
//  3. The same repo-relative paths cited in **YAML and shell** — Taskfiles, CI
//     workflows, chart values, hack scripts. This surface was added after a docs
//     reorg left eight of them dangling: the Markdown and Go citations had all been
//     repointed, and nothing was looking at the Taskfiles.
//
// A citation must contain a slash. A bare `README.md` in prose names no particular
// file — relative to which directory? — so it is not treated as a reference.
//
// The Go side parses the AST and reads only comments, never string literals. That
// distinction matters: the gittargetignore tests build in-memory filesystems whose
// entries are named like documentation paths. Those are fixtures, not citations. A
// regex cannot tell the two apart; the parser can.
//
// YAML and shell have no comparable parser worth carrying, so they are scanned as
// plain text. The one distinction that must be made there is a docs path in a URL
// (`https://github.com/…/docs/spec/foo.md`), which names a rendered page rather
// than a repo-relative file; those are skipped.
//
// Only git-tracked files are scanned, so gitignored scratch notes and the local
// upstream checkouts under external-sources/ are out of scope for free.
//
// A reference must resolve to a git-tracked file or directory, not merely to
// something on disk. Resolving against the filesystem would make this check pass on
// the author's machine and fail in CI: a link into a gitignored path — `.agents/`,
// a local scratch file, an untracked upstream checkout — exists locally and does not
// exist in a fresh clone. That is precisely the reference this check must catch, so
// existence is decided by `git ls-files`, never by os.Stat.
//
// Usage:
//
//	doccheck [-root DIR]
//
// Exits non-zero and prints file:line for every unresolved reference.
package main

import (
	"context"
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	gopath "path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// markdownLink matches [label](target). Reference-style links and bare autolinks
// are deliberately not matched: the former are rare here, the latter are URLs.
var markdownLink = regexp.MustCompile(`\[[^\]\[]*\]\(([^)\s]+)\)`)

// docPath matches a repo-relative Markdown path as written in a comment — `docs/`
// is the common case, but a package that keeps its contract next to its code cites
// something like `internal/git/manifestedit/DECISION.md`, and those rot just the same.
//
// At least one slash is required: a bare `README.md` in prose names no particular
// file (relative to which directory?), and treating it as a citation would report
// half the repository's prose as broken.
var docPath = regexp.MustCompile(`\b[A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]+)+\.md\b`)

type finding struct {
	file   string // repo-relative
	line   int
	target string
	kind   string // "markdown" | "go-comment" | "yaml" | "shell"
}

// exitUsage is the exit code for doccheck failing to run at all, as distinct from
// exit 1, which means it ran and found broken references.
const exitUsage = 2

func main() {
	root := flag.String("root", ".", "repository root to scan")
	flag.Parse()

	abs, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doccheck: %v\n", err)
		os.Exit(exitUsage)
	}

	tracked, err := trackedFiles(context.Background(), abs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doccheck: %v\n", err)
		os.Exit(exitUsage)
	}

	known := newTrackedSet(tracked)

	var findings []finding
	var mdFiles, goFiles, textFiles int

	for _, name := range tracked {
		path := filepath.Join(abs, name)
		switch {
		case strings.HasSuffix(name, ".md"):
			mdFiles++
			findings = append(findings, checkMarkdown(abs, path, known)...)
		case strings.HasSuffix(name, ".go"):
			goFiles++
			findings = append(findings, checkGoComments(abs, path, known)...)
		case strings.HasSuffix(name, ".yml"), strings.HasSuffix(name, ".yaml"):
			textFiles++
			findings = append(findings, checkTextCitations(abs, path, "yaml", known)...)
		case strings.HasSuffix(name, ".sh"):
			textFiles++
			findings = append(findings, checkTextCitations(abs, path, "shell", known)...)
		}
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].file != findings[j].file {
			return findings[i].file < findings[j].file
		}
		return findings[i].line < findings[j].line
	})

	if len(findings) == 0 {
		fmt.Printf("doccheck: OK — %d markdown files, %d Go files, %d YAML/shell files, "+
			"every reference resolves\n", mdFiles, goFiles, textFiles)
		return
	}

	for _, f := range findings {
		fmt.Printf("%s:%d: broken %s reference: %s\n", f.file, f.line, f.kind, f.target)
	}
	fmt.Fprintf(os.Stderr, "\ndoccheck: %d broken reference(s). "+
		"A moved document must have its references fixed in the same commit.\n", len(findings))
	os.Exit(1)
}

// trackedFiles lists the repository's git-tracked files. Using git's own index
// rather than a filesystem walk means .gitignore is honoured for free: scratch
// notes and the local upstream checkouts under external-sources/ are never scanned,
// and we never have to maintain a skip-list that drifts.
func trackedFiles(ctx context.Context, root string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-z")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w (is %s a git repository?)", err, root)
	}
	var names []string
	for _, n := range strings.Split(string(out), "\x00") {
		if n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

// checkMarkdown resolves every relative link in a Markdown file against the file's
// own directory. http(s), mailto and pure-anchor links are not our business.
func checkMarkdown(root, path string, known map[string]bool) []finding {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	dir := filepath.Dir(path)
	var out []finding

	for i, line := range strings.Split(string(data), "\n") {
		for _, m := range markdownLink.FindAllStringSubmatch(line, -1) {
			target := m[1]
			if isExternal(target) {
				continue
			}
			// Strip a trailing #fragment; we check the file, not the anchor.
			if idx := strings.IndexByte(target, '#'); idx >= 0 {
				target = target[:idx]
			}
			if target == "" {
				continue
			}
			if !resolves(root, known, filepath.Join(dir, target)) {
				out = append(out, finding{
					file:   rel(root, path),
					line:   i + 1,
					target: m[1],
					kind:   "markdown",
				})
			}
		}
	}
	return out
}

// checkGoComments extracts docs/**.md citations from comments only. String
// literals are ignored on purpose — see the package doc.
func checkGoComments(root, path string, known map[string]bool) []finding {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		// A file that does not parse is golangci-lint's problem, not ours.
		return nil
	}

	var out []finding
	for _, group := range file.Comments {
		for _, c := range group.List {
			for _, target := range citationsIn(c.Text) {
				if !resolves(root, known, filepath.Join(root, target)) {
					out = append(out, finding{
						file:   rel(root, path),
						line:   fset.Position(c.Pos()).Line,
						target: target,
						kind:   "go-comment",
					})
				}
			}
		}
	}
	return out
}

// urlPrefix matches a URL running right up to the end of the text before a match,
// i.e. the match is the tail of that URL rather than a repo-relative path.
var urlPrefix = regexp.MustCompile(`https?://\S*$`)

// citationsIn returns the repo-relative *.md paths cited in text, skipping any that
// are the tail of a URL. `https://github.com/…/docs/spec/foo.md` names a rendered page
// on a website; only a bare repo-relative path is ours to resolve. Both the Go and the
// YAML/shell surfaces need this, so it lives here rather than in either of them.
func citationsIn(text string) []string {
	var out []string
	for _, loc := range docPath.FindAllStringIndex(text, -1) {
		if urlPrefix.MatchString(text[:loc[0]]) {
			continue
		}
		if loc[0] > 0 && isPathTail(text[loc[0]-1]) {
			continue
		}
		out = append(out, text[loc[0]:loc[1]])
	}
	return out
}

// isPathTail reports whether the character immediately before a match makes that match
// the tail of a longer token rather than a citation in its own right. A repo-relative
// citation starts at the beginning of its path; when the regex can only latch onto the
// middle of one, what it found is not a path from the repository root.
//
// Two shapes hit this. An example relative link written out in a comment begins with
// a dot-dot segment, and a path assembled in shell begins with a variable — in both,
// the regex can only start matching after the leading separator, so what it captures
// is a fragment. Reporting either would be a false positive, and the shell one would
// be unfixable: there is no repo-relative path there to correct.
//
// (The fragments are deliberately not spelled out here. This checker reads its own
// comments, and a bare fragment in prose is exactly what it is built to report.)
func isPathTail(b byte) bool {
	switch b {
	case '/', '.', '-', '$':
		return true
	}
	return false
}

// checkTextCitations resolves docs/**.md citations in a file with no parser worth
// carrying — YAML and shell, where these paths live in comments. kind names the
// surface so the report says which one broke.
func checkTextCitations(root, path, kind string, known map[string]bool) []finding {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var out []finding
	for i, line := range strings.Split(string(data), "\n") {
		for _, target := range citationsIn(line) {
			if !resolves(root, known, filepath.Join(root, target)) {
				out = append(out, finding{
					file:   rel(root, path),
					line:   i + 1,
					target: target,
					kind:   kind,
				})
			}
		}
	}
	return out
}

// newTrackedSet is the set of repo-relative paths git tracks, plus every directory
// implied by them (git lists files, but a link may legitimately point at a folder).
func newTrackedSet(tracked []string) map[string]bool {
	// Sized for the files alone; the implied directories grow it a little.
	known := make(map[string]bool, len(tracked))
	for _, name := range tracked {
		name = filepath.ToSlash(name)
		known[name] = true
		for dir := gopath.Dir(name); dir != "." && dir != "/"; dir = gopath.Dir(dir) {
			known[dir] = true
		}
	}
	return known
}

// resolves reports whether abs — an absolute path a reference pointed at — names a
// git-tracked file or directory. A path that escapes the repository never resolves.
func resolves(root string, known map[string]bool, abs string) bool {
	r, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	r = filepath.ToSlash(r)
	if r == "." {
		return true
	}
	if r == ".." || strings.HasPrefix(r, "../") {
		return false
	}
	return known[r]
}

func isExternal(target string) bool {
	return strings.HasPrefix(target, "http://") ||
		strings.HasPrefix(target, "https://") ||
		strings.HasPrefix(target, "mailto:") ||
		strings.HasPrefix(target, "#")
}

func rel(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return r
}
