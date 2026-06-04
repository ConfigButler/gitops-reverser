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

/*
Package manifestanalyzer is a runtime-independent analyzer for a folder of
Kubernetes manifests. It is the proof-of-concept core described in
docs/design/manifest/current-manifest-support-review.md: build the manifest model
once, classify every file, and report what we know about it — without any
controller runtime, and without writing anything.

The package is deliberately decoupled from the controller so the same logic can
back both the live writer and a standalone CLI:

  - Filesystem access goes through fs.FS, so it runs against a git worktree, an
    arbitrary directory (os.DirFS), or an in-memory tree (fstest.MapFS).
  - "What is in the API" is an injected WatchSource, not an ambient global. With
    no source the analyzer simply reports KRM as "unknown" rather than guessing.
  - The analysis path is strictly read-only; it produces a Report and never
    mutates the tree.

It builds on internal/git/manifestedit for the YAML mechanism (splitting,
manifest identity, duplicate detection, SOPS handling) and adds classification,
a bounded summary, and acceptance issues on top.
*/
package manifestanalyzer

import (
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// GVK is a parsed group/version/kind. Group is empty for core resources.
type GVK struct {
	Group   string `json:"group"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
}

// ParseGVK derives a GVK from a manifest's apiVersion and kind. An apiVersion of
// "apps/v1" yields group "apps"; a bare "v1" yields an empty group.
func ParseGVK(apiVersion, kind string) GVK {
	g := GVK{Kind: kind}
	if i := strings.LastIndex(apiVersion, "/"); i >= 0 {
		g.Group = apiVersion[:i]
		g.Version = apiVersion[i+1:]
	} else {
		g.Version = apiVersion
	}
	return g
}

// String renders a GVK as "group/version/kind", or "version/kind" for core.
func (g GVK) String() string {
	if g.Group == "" {
		return g.Version + "/" + g.Kind
	}
	return g.Group + "/" + g.Version + "/" + g.Kind
}

// Empty reports whether the GVK carries no information (a non-KRM document).
func (g GVK) Empty() bool {
	return g.Group == "" && g.Version == "" && g.Kind == ""
}

// Class is the bucket a file or document falls into, mirroring the design doc:
// non-YAML files are ignored, non-KRM YAML is the dangerous unknown, and KRM is
// split by whether it maps to a watched API resource.
type Class string

const (
	// ClassNonYAML is a file that is not YAML by extension. Always ignored.
	ClassNonYAML Class = "non-yaml"
	// ClassEmpty is a YAML document that is empty or comment-only.
	ClassEmpty Class = "empty"
	// ClassInvalidYAML is a document that does not parse as YAML.
	ClassInvalidYAML Class = "invalid-yaml"
	// ClassNonKRM is valid YAML that is not a Kubernetes manifest.
	ClassNonKRM Class = "non-krm"
	// ClassWatchedKRM is a manifest whose GVK is a watched API resource.
	ClassWatchedKRM Class = "watched-krm"
	// ClassUnwatchedKRM is a manifest whose GVK is not a watched API resource.
	ClassUnwatchedKRM Class = "unwatched-krm"
	// ClassUnknownKRM is a manifest classified with no WatchSource available.
	ClassUnknownKRM Class = "unknown-krm"
)

// DocumentReport describes one YAML document inside a file.
type DocumentReport struct {
	Index      int                   `json:"index"`
	Class      Class                 `json:"class"`
	GVK        GVK                   `json:"gvk"`
	Identity   manifestedit.Identity `json:"identity"`
	WatchState WatchState            `json:"watchState"`
	Editable   bool                  `json:"editable"`
	Encrypted  bool                  `json:"encrypted"`
	Duplicate  bool                  `json:"duplicate"`
	// Reason explains a non-editable, non-KRM, empty, or invalid document.
	Reason string `json:"reason,omitempty"`
}

// FileReport describes one file under the scanned root. Non-YAML files carry no
// documents; YAML files carry one DocumentReport per document.
type FileReport struct {
	Path      string           `json:"path"`
	IsYAML    bool             `json:"isYaml"`
	Documents []DocumentReport `json:"documents,omitempty"`
}

// Summary is a bounded, status-friendly overview of a Report. It never grows with
// the number of resources beyond the small set of class and GVK keys.
type Summary struct {
	FilesTotal   int                                  `json:"filesTotal"`
	YAMLFiles    int                                  `json:"yamlFiles"`
	NonYAMLFiles int                                  `json:"nonYamlFiles"`
	Documents    int                                  `json:"documents"`
	Duplicates   int                                  `json:"duplicates"`
	Encrypted    int                                  `json:"encrypted"`
	ByClass      map[Class]int                        `json:"byClass"`
	ByGVK        map[string]int                       `json:"byGvk"`
	Diagnostics  map[manifestedit.DiagnosticLevel]int `json:"diagnostics"`
}

// IssueKind classifies an acceptance issue.
type IssueKind string

const (
	// IssueDuplicate marks a document that duplicates an earlier manifest identity.
	IssueDuplicate IssueKind = "duplicate-identity"
	// IssueNonKRM marks YAML that does not parse as a Kubernetes manifest.
	IssueNonKRM IssueKind = "non-krm-yaml"
	// IssueInvalidYAML marks a document that does not parse as YAML.
	IssueInvalidYAML IssueKind = "invalid-yaml"
	// IssueUnwatched marks KRM with no matching watched API resource.
	IssueUnwatched IssueKind = "unwatched-krm"
)

// AcceptanceIssue is a fact about the tree that a stricter adoption policy may
// treat as blocking. The analyzer always reports issues; deciding whether they
// block (the refuse/scan/prune policy) is left to the caller.
type AcceptanceIssue struct {
	Kind          IssueKind `json:"kind"`
	Path          string    `json:"path"`
	DocumentIndex int       `json:"documentIndex"`
	Message       string    `json:"message"`
}

// Report is the full result of analyzing a tree.
type Report struct {
	Root        string                    `json:"root"`
	Files       []FileReport              `json:"files"`
	Summary     Summary                   `json:"summary"`
	Issues      []AcceptanceIssue         `json:"issues"`
	Diagnostics []manifestedit.Diagnostic `json:"diagnostics"`
}

// Options configures an analysis run.
type Options struct {
	// Watch resolves a GVK to a watch state. Nil means NoWatchSource (no API
	// truth available), so all KRM is reported as ClassUnknownKRM.
	Watch WatchSource
}

// AnalyzeDir analyzes the directory at root. It verifies root is a directory,
// then runs Analyze over os.DirFS(root). Symlinks are never followed.
func AnalyzeDir(root string, opts Options) (Report, error) {
	info, err := os.Stat(root)
	if err != nil {
		return Report{}, err
	}
	if !info.IsDir() {
		return Report{}, fmt.Errorf("not a directory: %s", root)
	}
	rep := Analyze(os.DirFS(root), opts)
	rep.Root = root
	return rep, nil
}

// Analyze scans fsys and returns a Report. It is read-only and never fails: any
// per-entry problem (unreadable file, walk error, invalid YAML) becomes a
// diagnostic rather than an error.
func Analyze(fsys fs.FS, opts Options) Report {
	ws := opts.Watch
	if ws == nil {
		ws = NoWatchSource{}
	}

	yamlFiles, nonYAML, scanDiags := collectFiles(fsys)
	inv, indexDiags := manifestedit.IndexFiles(yamlFiles)

	recordsByPath := map[string][]manifestedit.DocumentRecord{}
	for _, r := range inv.Records {
		recordsByPath[r.Location.Path] = append(recordsByPath[r.Location.Path], r)
	}
	dupSet := map[manifestedit.Location]bool{}
	for _, d := range inv.Duplicates() {
		dupSet[d.Location] = true
	}
	diagsByPath := map[string][]manifestedit.Diagnostic{}
	for _, d := range indexDiags {
		diagsByPath[d.Path] = append(diagsByPath[d.Path], d)
	}

	files := make([]FileReport, 0, len(yamlFiles)+len(nonYAML))
	for _, f := range yamlFiles {
		files = append(files, buildYAMLFileReport(f.Path, recordsByPath[f.Path], diagsByPath[f.Path], dupSet, ws))
	}
	for _, p := range nonYAML {
		files = append(files, FileReport{Path: p, IsYAML: false})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	allDiags := append(append([]manifestedit.Diagnostic(nil), scanDiags...), indexDiags...)
	return Report{
		Files:       files,
		Summary:     buildSummary(files, allDiags),
		Issues:      buildIssues(files),
		Diagnostics: allDiags,
	}
}

// collectFiles walks fsys, returning YAML files (path + content), the paths of
// non-YAML files, and scan-level diagnostics. Symlinks are skipped.
func collectFiles(fsys fs.FS) ([]manifestedit.FileContent, []string, []manifestedit.Diagnostic) {
	var (
		yamlFiles []manifestedit.FileContent
		nonYAML   []string
		diags     []manifestedit.Diagnostic
	)

	walkErr := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			diags = append(
				diags,
				manifestedit.Diagnostic{Level: manifestedit.DiagWarning, Path: path, Message: err.Error()},
			)
			return nil //nolint:nilerr // a per-entry error must not abort the whole scan
		}
		if path == "." {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			diags = append(
				diags,
				manifestedit.Diagnostic{Level: manifestedit.DiagInfo, Path: path, Message: "symlink skipped"},
			)
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !isYAMLFile(path) {
			nonYAML = append(nonYAML, path)
			return nil
		}
		content, readErr := fs.ReadFile(fsys, path)
		if readErr != nil {
			diags = append(
				diags,
				manifestedit.Diagnostic{Level: manifestedit.DiagWarning, Path: path, Message: readErr.Error()},
			)
			return nil //nolint:nilerr // an unreadable file must not abort the whole scan
		}
		yamlFiles = append(yamlFiles, manifestedit.FileContent{Path: path, Content: content})
		return nil
	})
	if walkErr != nil {
		diags = append(
			diags,
			manifestedit.Diagnostic{Level: manifestedit.DiagError, Path: ".", Message: walkErr.Error()},
		)
	}

	sort.Slice(yamlFiles, func(i, j int) bool { return yamlFiles[i].Path < yamlFiles[j].Path })
	sort.Strings(nonYAML)
	return yamlFiles, nonYAML, diags
}

// buildYAMLFileReport assembles per-document classification for one YAML file by
// merging KRM records (the authoritative manifest documents) with diagnostics
// (which cover empty, invalid, and non-KRM documents) on document index.
func buildYAMLFileReport(
	path string,
	records []manifestedit.DocumentRecord,
	diags []manifestedit.Diagnostic,
	dupSet map[manifestedit.Location]bool,
	ws WatchSource,
) FileReport {
	recByIdx := map[int]manifestedit.DocumentRecord{}
	for _, r := range records {
		recByIdx[r.Location.DocumentIndex] = r
	}
	diagByIdx := map[int]manifestedit.Diagnostic{}
	for _, d := range diags {
		if _, ok := diagByIdx[d.DocumentIndex]; !ok {
			diagByIdx[d.DocumentIndex] = d
		}
	}

	idxSet := map[int]bool{}
	for i := range recByIdx {
		idxSet[i] = true
	}
	for i := range diagByIdx {
		idxSet[i] = true
	}
	idxs := make([]int, 0, len(idxSet))
	for i := range idxSet {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)

	fr := FileReport{Path: path, IsYAML: true}
	for _, i := range idxs {
		// A KRM record always wins over a co-located diagnostic (for example a
		// non-editable record paired with a warning about anchors).
		if r, ok := recByIdx[i]; ok {
			fr.Documents = append(fr.Documents, krmDocReport(i, r, dupSet, ws))
			continue
		}
		d := diagByIdx[i]
		fr.Documents = append(fr.Documents, DocumentReport{Index: i, Class: classFromDiag(d), Reason: d.Message})
	}
	return fr
}

// krmDocReport builds a DocumentReport for an indexed KRM record.
func krmDocReport(
	i int,
	r manifestedit.DocumentRecord,
	dupSet map[manifestedit.Location]bool,
	ws WatchSource,
) DocumentReport {
	gvk := ParseGVK(r.Identity.APIVersion, r.Identity.Kind)
	state := ws.WatchStateFor(gvk)
	return DocumentReport{
		Index:      i,
		Class:      krmClass(state),
		GVK:        gvk,
		Identity:   r.Identity,
		WatchState: state,
		Editable:   r.Editable,
		Encrypted:  r.Encrypted,
		Duplicate:  dupSet[r.Location],
		Reason:     r.Reason,
	}
}

// krmClass maps a watch state to the class used for a KRM document.
func krmClass(state WatchState) Class {
	switch state {
	case WatchWatched:
		return ClassWatchedKRM
	case WatchUnwatched:
		return ClassUnwatchedKRM
	case WatchUnknown:
		return ClassUnknownKRM
	}
	return ClassUnknownKRM
}

// classFromDiag classifies a non-KRM document from its diagnostic. It couples to
// manifestedit's diagnostic wording; promoting manifestedit to emit structured
// reasons is a documented follow-up. An error-level diagnostic is invalid YAML;
// an "empty" message is an empty document; anything else is non-KRM YAML.
func classFromDiag(d manifestedit.Diagnostic) Class {
	if d.Level == manifestedit.DiagError {
		return ClassInvalidYAML
	}
	if strings.Contains(d.Message, "empty") {
		return ClassEmpty
	}
	return ClassNonKRM
}

// buildSummary produces the bounded overview.
func buildSummary(files []FileReport, diags []manifestedit.Diagnostic) Summary {
	s := Summary{
		ByClass:     map[Class]int{},
		ByGVK:       map[string]int{},
		Diagnostics: map[manifestedit.DiagnosticLevel]int{},
	}
	for _, f := range files {
		s.FilesTotal++
		if !f.IsYAML {
			s.NonYAMLFiles++
			continue
		}
		s.YAMLFiles++
		for _, d := range f.Documents {
			s.Documents++
			s.ByClass[d.Class]++
			if !d.GVK.Empty() {
				s.ByGVK[d.GVK.String()]++
			}
			if d.Duplicate {
				s.Duplicates++
			}
			if d.Encrypted {
				s.Encrypted++
			}
		}
	}
	for _, d := range diags {
		s.Diagnostics[d.Level]++
	}
	return s
}

// buildIssues derives acceptance issues from the classified documents. With no
// WatchSource there are no unwatched issues, which is the intended behavior: we
// do not flag what we cannot judge.
func buildIssues(files []FileReport) []AcceptanceIssue {
	var issues []AcceptanceIssue
	for _, f := range files {
		for _, d := range f.Documents {
			if d.Duplicate {
				issues = append(issues, AcceptanceIssue{
					Kind: IssueDuplicate, Path: f.Path, DocumentIndex: d.Index,
					Message: "duplicate of " + identityRef(d.Identity),
				})
			}
			switch d.Class {
			case ClassNonKRM:
				issues = append(issues, AcceptanceIssue{
					Kind: IssueNonKRM, Path: f.Path, DocumentIndex: d.Index,
					Message: "YAML is not a Kubernetes manifest",
				})
			case ClassInvalidYAML:
				issues = append(issues, AcceptanceIssue{
					Kind: IssueInvalidYAML, Path: f.Path, DocumentIndex: d.Index, Message: d.Reason,
				})
			case ClassUnwatchedKRM:
				issues = append(issues, AcceptanceIssue{
					Kind: IssueUnwatched, Path: f.Path, DocumentIndex: d.Index,
					Message: identityRef(d.Identity) + " has no matching watched API resource",
				})
			case ClassNonYAML, ClassEmpty, ClassWatchedKRM, ClassUnknownKRM:
				// Not acceptance issues: ignored files, empty documents, and KRM
				// that is watched or whose watch state is unknown.
			}
		}
	}
	return issues
}

// identityRef renders a manifest identity like "apps/v1/Deployment/default/web",
// using "_cluster" for cluster-scoped objects.
func identityRef(id manifestedit.Identity) string {
	ns := id.Namespace
	if ns == "" {
		ns = "_cluster"
	}
	return ParseGVK(id.APIVersion, id.Kind).String() + "/" + ns + "/" + id.Name
}

// isYAMLFile reports whether a path is a YAML file by extension.
func isYAMLFile(path string) bool {
	return strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")
}
