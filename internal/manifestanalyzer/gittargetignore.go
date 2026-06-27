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
	"fmt"
	"io/fs"
	"strings"

	gitignore "github.com/go-git/go-git/v5/plumbing/format/gitignore"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// GitTargetIgnoreFileName is the basename of the in-repo escape hatch described in
// docs/design/gitpath-foreign-content-stringency.md (§4). Exactly ONE copy is honoured —
// the file at the GitTarget path root — and the patterns it carries name content the
// operator must NEVER read, even when it is YAML. A copy deeper in the subtree is NOT
// honoured and is refused as foreign content (D-foreign-2).
const GitTargetIgnoreFileName = ".gittargetignore"

// gitDirName is the version-control metadata directory. It is never managed content and
// is never descended, so its contents are neither modeled nor refused as foreign.
const gitDirName = ".git"

// ForeignKind classifies a non-managed filesystem entry found under a GitTarget path —
// the foreign role of the five-role model in
// docs/design/gitpath-foreign-content-stringency.md (§3). A foreign entry is refused, not
// ignored: the path is an operator-exclusive subtree.
type ForeignKind string

const (
	// ForeignFile is a non-YAML regular file in no recognized role (notes.txt, deploy.sh,
	// blob.bin, a nested .gittargetignore). YAML that is not managed KRM is already refused
	// as non-KRM, so this names only the non-YAML case.
	ForeignFile ForeignKind = "file"
	// ForeignSymlink is any symlink under the subtree. A writer materialising into a folder
	// with a symlink could follow it out of the subtree, so it is refused rather than skipped.
	ForeignSymlink ForeignKind = "symlink"
	// ForeignSubmodule is a gitlink / git submodule under the subtree — content the operator
	// cannot own or reason about. It is part of the model so a future gitlink-aware scan can
	// surface it; the structural fs.FS walk does not currently detect submodules (a nested
	// .git directory is skipped like any VCS metadata), so this is reserved for that hardening.
	ForeignSubmodule ForeignKind = "submodule"
)

// ForeignEntry is one filesystem entry under spec.path that matches no recognized role
// (managed KRM, active build directive, operator artifact, or .gittargetignore-ignored) and
// is therefore refused. Path is slash-separated and relative to the scanned root.
type ForeignEntry struct {
	Path string      `json:"path"`
	Kind ForeignKind `json:"kind"`
}

// EntryRole is the role a single walked filesystem entry falls into under the
// foreign-content policy. It is the pure verdict shared by every folder walker (the fs.FS
// analyzer scan and the live writer's worktree scan) so the .gittargetignore filter, the
// operator-artifact recognition, and the foreign-content refusal can never drift between
// the two paths.
type EntryRole int

const (
	// RoleIgnored is an entry that is recognized and deliberately NOT modeled: an
	// ignored file/symlink, or the one root .gittargetignore itself. The walker does
	// nothing with it — never reads it, never refuses it.
	RoleIgnored EntryRole = iota
	// RoleSkipDir is a directory the walker must not descend: the .git metadata directory,
	// or a whole subtree the root .gittargetignore matches (the "never read" semantic).
	RoleSkipDir
	// RoleManagedYAML is a YAML file the walker must read into the model (managed KRM, or a
	// retained build directive / operator .sops.yaml the store's allowlist handles).
	RoleManagedYAML
	// RoleOperatorArtifact is an accepted non-YAML operator artifact (README.md). It is
	// listed in the report's non-YAML inventory but is never foreign.
	RoleOperatorArtifact
	// RoleForeignFile is a foreign non-YAML regular file: refused.
	RoleForeignFile
	// RoleForeignSymlink is a foreign symlink: refused.
	RoleForeignSymlink
	// RoleDescend is a normal directory the walker descends into.
	RoleDescend
)

// ClassifyEntry decides the role of one walked entry. rel is the entry's slash-separated
// path relative to the scanned root; d is its directory entry; ignore is the active root
// matcher (nil when the path carries no .gittargetignore). It is a pure function — the
// single source of truth for the precedence in §4.1 of the design:
//
//	operator artifacts + build directives  →  root .gittargetignore filter  →  managed KRM / foreign
//
// so a user cannot use .gittargetignore to hide the operator's own files (README.md,
// .sops.yaml) or to silence a hard-kustomize refusal (kustomization.yaml), while every
// other unknown non-YAML entry is refused unless an ignore pattern names it.
func ClassifyEntry(rel string, d fs.DirEntry, ignore *IgnoreMatcher) EntryRole {
	// Symlinks are foreign wherever they appear and whatever they are named — a writer
	// could follow one out of the subtree. The only way to keep one is to ignore it.
	if d.Type()&fs.ModeSymlink != 0 {
		if ignore.Match(rel, d.IsDir()) {
			return RoleIgnored
		}
		return RoleForeignSymlink
	}
	if d.IsDir() {
		if filepathBase(rel) == gitDirName {
			return RoleSkipDir
		}
		if ignore.Match(rel, true) {
			return RoleSkipDir
		}
		return RoleDescend
	}
	// The one honoured .gittargetignore lives at exactly this path; its contents already
	// built the matcher. It is itself never modeled and never refused. A nested copy has a
	// different rel ("dir/.gittargetignore") and falls through to the foreign role below.
	if rel == GitTargetIgnoreFileName {
		return RoleIgnored
	}
	// Operator artifacts and build directives are matched before the ignore filter.
	if isRecognizedArtifact(rel) {
		if isYAMLFile(rel) {
			return RoleManagedYAML
		}
		return RoleOperatorArtifact
	}
	if ignore.Match(rel, false) {
		return RoleIgnored
	}
	if isYAMLFile(rel) {
		return RoleManagedYAML
	}
	return RoleForeignFile
}

// isRecognizedArtifact reports whether a file is an operator artifact or build directive
// the foreign-content gate always accepts and the ignore filter must never hide: the
// operator's own README.md and .sops.yaml config, and the kustomize build directives.
// Matching is by basename (any depth), as the design's role 3 / role 2 specify. The
// per-resource encrypted "<name>.sops.yaml" payloads are not matched here — only the bare
// ".sops.yaml" config is — so encrypted Secrets remain managed KRM.
func isRecognizedArtifact(path string) bool {
	switch filepathBase(path) {
	case "README.md", sopsConfigBasename, "kustomization.yaml", "kustomization.yml":
		return true
	default:
		return false
	}
}

// sopsConfigBasename is the operator's SOPS creation-rules config basename, recognized as
// an operator artifact (role 3). It mirrors the constant the bootstrap template uses.
const sopsConfigBasename = ".sops.yaml"

// IgnoreMatcher is the parsed, active root .gittargetignore: a go-git gitignore matcher
// plus the raw patterns it was built from. It is reused git's own matching semantics
// rather than reinventing glob handling. A nil *IgnoreMatcher matches nothing, so callers
// need no nil guard around Match.
type IgnoreMatcher struct {
	patterns []gitignore.Pattern
	raw      []string
}

// Match reports whether the slash-separated path is ignored (isDir distinguishes a
// directory match such as "docs/"). A nil matcher never matches.
func (m *IgnoreMatcher) Match(path string, isDir bool) bool {
	if m == nil || len(m.patterns) == 0 {
		return false
	}
	parts := strings.Split(path, "/")
	for i := len(m.patterns) - 1; i >= 0; i-- {
		switch m.patterns[i].Match(parts, isDir) {
		case gitignore.Exclude:
			return true
		case gitignore.Include:
			return false
		case gitignore.NoMatch:
		}
	}
	return false
}

// MatchingPattern returns the raw pattern that causes path to be ignored, or "" when the
// path is not ignored. It mirrors Match's last-wins priority so a shadowing diagnostic can
// name the exact pattern at fault (§4.3). A nil matcher returns "".
func (m *IgnoreMatcher) MatchingPattern(path string, isDir bool) string {
	if m == nil {
		return ""
	}
	parts := strings.Split(path, "/")
	for i := len(m.patterns) - 1; i >= 0; i-- {
		switch m.patterns[i].Match(parts, isDir) {
		case gitignore.Exclude:
			return m.raw[i]
		case gitignore.Include:
			return ""
		case gitignore.NoMatch:
		}
	}
	return ""
}

// isCatastrophicIgnorePattern reports whether a pattern is in the tiny parse-time denylist
// of whole-space patterns that would shadow essentially every managed write path (§4.3). It
// is deliberately NOT a collision prover — proving the general case is infeasible because
// write paths are dynamic and templated — only a guardrail against the obvious footgun.
func isCatastrophicIgnorePattern(pattern string) bool {
	switch pattern {
	case "*", "**", "/", "*.yaml", "*.yml", "**/*", "**/*.yaml", "**/*.yml":
		return true
	default:
		return false
	}
}

// LoadGitTargetIgnore parses the bytes of a root .gittargetignore into a matcher and the
// parse-time refusals its catastrophic patterns earn. Comments (#) and blank lines are
// skipped; every other line is a gitignore pattern. A file with no effective patterns
// yields a nil matcher (nothing ignored) and no issues. The matcher is always returned
// even when a catastrophic pattern is present — acceptance refuses on the issues, so the
// matcher is never consulted for a write in that case.
func LoadGitTargetIgnore(content []byte) (*IgnoreMatcher, []AcceptanceIssue) {
	var (
		patterns []gitignore.Pattern
		raw      []string
		issues   []AcceptanceIssue
	)
	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if isCatastrophicIgnorePattern(trimmed) {
			issues = append(issues, AcceptanceIssue{
				Kind: IssueIgnoreShadowsManaged,
				Path: GitTargetIgnoreFileName,
				Message: fmt.Sprintf(
					"%s pattern %q matches essentially every managed write path and would blind the "+
						"operator to its own files; remove it and name only specific passengers",
					GitTargetIgnoreFileName, trimmed),
			})
		}
		patterns = append(patterns, gitignore.ParsePattern(trimmed, nil))
		raw = append(raw, trimmed)
	}
	if len(patterns) == 0 {
		return nil, issues
	}
	return &IgnoreMatcher{patterns: patterns, raw: raw}, issues
}

// loadRootGitTargetIgnore reads the one honoured ignore file at the scanned root of fsys
// and parses it. A missing file is the common case and yields a nil matcher with no issues.
// Only the exact root path is consulted — a nested copy is never read here (it is refused
// as foreign by the walk).
func loadRootGitTargetIgnore(fsys fs.FS) (*IgnoreMatcher, []AcceptanceIssue) {
	content, err := fs.ReadFile(fsys, GitTargetIgnoreFileName)
	if err != nil {
		return nil, nil
	}
	return LoadGitTargetIgnore(content)
}

// foreignContentRefusals turns every surviving foreign entry into an acceptance refusal,
// naming the offending path so a human (and GitTarget status) can resolve it — git rm the
// file or name it in the root .gittargetignore. Symlinks and submodules carry their own
// kinds; a non-YAML regular file is IssueForeignFile (foreign YAML is already IssueNonKRM).
func foreignContentRefusals(store *ManifestStore) []AcceptanceIssue {
	out := make([]AcceptanceIssue, 0, len(store.Foreign))
	for _, f := range store.Foreign {
		out = append(out, AcceptanceIssue{
			Kind:    foreignIssueKind(f.Kind),
			Path:    f.Path,
			Message: foreignMessage(f),
		})
	}
	return out
}

// foreignIssueKind maps a ForeignKind to its acceptance IssueKind.
func foreignIssueKind(k ForeignKind) IssueKind {
	switch k {
	case ForeignSymlink:
		return IssueForeignSymlink
	case ForeignSubmodule:
		return IssueForeignSubmodule
	case ForeignFile:
		return IssueForeignFile
	default:
		return IssueForeignFile
	}
}

// foreignMessage builds the human-facing refusal text for a foreign entry. It always names
// the path and points at the escape hatch.
func foreignMessage(f ForeignEntry) string {
	switch f.Kind {
	case ForeignSymlink:
		return "symlink " + f.Path + " is not managed content; remove it or name it in " + GitTargetIgnoreFileName
	case ForeignSubmodule:
		return "git submodule " + f.Path + " cannot be managed by the operator; remove it or name it in " +
			GitTargetIgnoreFileName
	case ForeignFile:
		return "foreign file " + f.Path + " is not a managed manifest; remove it or name it in " +
			GitTargetIgnoreFileName
	default:
		return "foreign content " + f.Path + " is not managed; remove it or name it in " + GitTargetIgnoreFileName
	}
}

// FolderScan is the structural view of a scanned GitTarget subtree: the YAML files to
// model, the non-YAML inventory for the report, the foreign entries to refuse, and the
// active root .gittargetignore matcher with any parse-time refusals. It is the one shape
// every folder walker produces, so the analyzer scan and the live writer feed the store
// and the acceptance gate identically.
type FolderScan struct {
	YAMLFiles    []manifestedit.FileContent
	NonYAML      []string
	Foreign      []ForeignEntry
	Ignore       *IgnoreMatcher
	IgnoreIssues []AcceptanceIssue
	Diagnostics  []manifestedit.Diagnostic
}
