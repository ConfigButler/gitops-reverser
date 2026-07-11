// SPDX-License-Identifier: Apache-2.0

/*
Package manifestanalyzer is a runtime-independent analyzer for a folder of
Kubernetes manifests. It is the proof-of-concept core described in
docs/spec/current-manifest-support-review.md: build the manifest model
once, classify every file, and report what we know about it — without any
controller runtime, and without writing anything.

The package is deliberately decoupled from the controller so the same logic can
back both the live writer and a standalone CLI:

  - Filesystem access goes through fs.FS, so it runs against a git worktree, an
    arbitrary directory (os.DirFS), or an in-memory tree (fstest.MapFS).
  - The analysis path is strictly read-only; it produces a Report and never
    mutates the tree.

This first slice is structure-only and needs no cluster: it classifies files,
detects duplicates, and reports the inventory of every GVK found. Comparing those
GVKs against a live API (the "what is in the API" source of truth, which decides
what is watched, unwatched, or orphaned) is a deliberate later step.

It builds on internal/git/manifestedit for the YAML mechanism (splitting,
manifest identity, duplicate detection, SOPS handling) and adds classification,
a bounded summary, and acceptance issues on top.
*/
package manifestanalyzer

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
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
// every valid Kubernetes manifest. Which GVKs those manifests are is reported via
// the GVK inventory (Summary.ByGVK) rather than by sub-classing the bucket;
// comparing them against a live API is a deliberate later step.
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
	// ClassKRM is a valid Kubernetes manifest.
	ClassKRM Class = "krm"
)

// DocumentReport describes one YAML document inside a file.
type DocumentReport struct {
	Index    int                   `json:"index"`
	Class    Class                 `json:"class"`
	GVK      GVK                   `json:"gvk"`
	Identity manifestedit.Identity `json:"identity"`
	Editable bool                  `json:"editable"`
	// Cause is the structured reason a KRM document is not cleanly editable
	// (encrypted, non-editable construct). It is nil for an editable document and
	// for non-KRM/empty/invalid rows. Duplicate identity is no longer a per-document
	// attribute — it surfaces as an acceptance issue and a diagnostic instead.
	Cause *DocumentCause `json:"cause,omitempty"`
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

// AnalyzeDir analyzes the directory at root. It verifies root is a directory,
// then runs Analyze over os.DirFS(root). Symlinks are never followed.
func AnalyzeDir(root string) (Report, error) {
	info, err := os.Stat(root)
	if err != nil {
		return Report{}, err
	}
	if !info.IsDir() {
		return Report{}, fmt.Errorf("not a directory: %s", root)
	}
	rep := Analyze(os.DirFS(root))
	rep.Root = root
	return rep, nil
}

// BuildStore walks fsys and returns the byte-free ManifestStore: the managed
// FileModels and the scan/index diagnostics. It is the structure spine the Report
// is projected from, and the entry point downstream layers (planner, live writer)
// will consume directly. It is read-only and never fails.
//
// lookup resolves each managed document's GVK to a served resource identity; pass
// nil (or an un-ready registry) to keep the no-cluster, structure-only mode.
//
// BuildStore materialises every KRM document (the empty-allowlist case). Scan mode
// passes the acceptance policy's allowlist through buildStoreFS so non-API KRM such
// as kustomization.yaml is retained outside the model rather than materialised.
func BuildStore(ctx context.Context, fsys fs.FS, lookup typeset.Lookup) *ManifestStore {
	return buildStoreFS(ctx, fsys, lookup, Allowlist{})
}

// buildStoreFS is BuildStore with an explicit allowlist: a record whose GVK the
// allowlist matches is retained (kept out of FilesByPath) instead of materialised.
func buildStoreFS(
	ctx context.Context,
	fsys fs.FS,
	lookup typeset.Lookup,
	allowlist Allowlist,
) *ManifestStore {
	return buildStore(ctx, collectFiles(fsys), lookup, allowlist)
}

// Analyze scans fsys and returns a Report. It is read-only and never fails: any
// per-entry problem (unreadable file, walk error, invalid YAML) becomes a
// diagnostic rather than an error. The Report is a projection rendered from the
// ManifestStore built by buildStore.
func Analyze(fsys fs.FS) Report {
	scan := collectFiles(fsys)
	// Analyze is the no-cluster default: a nil mapper keeps it structure-only, so the
	// resource index stays empty and no mapping diagnostics are emitted. It
	// materialises every KRM document (the empty allowlist), since the legacy report
	// classifies the whole tree rather than adopting it.
	store := buildStore(context.Background(), scan, nil, Allowlist{})
	return projectReport(store, scan.YAMLFiles, scan.NonYAML)
}

// projectReport renders the analyzer Report from the store plus the scan's file
// skeleton (which YAML and non-YAML files exist). Managed KRM documents come from
// the store; non-KRM, empty, and invalid documents are reconstructed from the
// store's diagnostics, exactly as the pre-store analyzer derived them.
func projectReport(store *ManifestStore, yamlFiles []manifestedit.FileContent, nonYAML []string) Report {
	// Non-YAML scan diagnostics (read errors, skipped symlinks, walk errors) never
	// share a path with an indexed YAML file, so grouping every diagnostic by path
	// yields exactly the per-document index diagnostics for each YAML file.
	diagsByPath := diagnosticsByPath(store.Diagnostics)

	files := make([]FileReport, 0, len(yamlFiles)+len(nonYAML))
	// Duplicate identities are acceptance facts derived from the store's collapsed
	// index. Their position is reconstructed per file (DocumentModel no longer stores
	// its index), so the duplicate set is collected alongside the file reports.
	duplicates := map[RecordRef]bool{}
	for _, f := range yamlFiles {
		fr, dups := projectFileReport(store, f.Path, store.FilesByPath[f.Path], diagsByPath[f.Path])
		files = append(files, fr)
		for ref := range dups {
			duplicates[ref] = true
		}
	}
	for _, p := range nonYAML {
		files = append(files, FileReport{Path: p, IsYAML: false})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	// Capture the detail for every diagnostic classFromDiag maps to invalid-YAML
	// (a parse failure or a .sops.yaml missing its sops stanza), so the resulting
	// IssueInvalidYAML carries a message rather than an empty string.
	invalidMsgs := map[RecordRef]string{}
	for _, d := range store.Diagnostics {
		if d.Reason == manifestedit.ReasonInvalidYAML || d.Reason == manifestedit.ReasonMissingSopsKey {
			invalidMsgs[RecordRef{FilePath: d.Path, DocumentIndex: d.DocumentIndex}] = d.Message
		}
	}

	return Report{
		Root:        store.Root,
		Files:       files,
		Summary:     buildSummary(files, store.Diagnostics, len(duplicates)),
		Issues:      buildIssues(files, duplicates, invalidMsgs),
		Diagnostics: store.Diagnostics,
	}
}

// collectFiles walks fsys into a FolderScan: the YAML files to model, the non-YAML
// inventory, the foreign entries to refuse, and the active root .gittargetignore matcher.
// The matcher is loaded once up front (order-independent, never relying on walk order) and
// every entry is classified through the shared ClassifyEntry policy, so the analyzer scan
// and the live writer's worktree scan apply the foreign-content and ignore rules
// identically. An ignored entry — file, symlink, or whole subtree — is never read.
func collectFiles(fsys fs.FS) FolderScan {
	ignore, ignoreIssues := loadRootGitTargetIgnore(fsys)
	scan := FolderScan{Ignore: ignore, IgnoreIssues: ignoreIssues}

	walkErr := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			scan.Diagnostics = append(
				scan.Diagnostics,
				manifestedit.Diagnostic{Level: manifestedit.DiagWarning, Path: path, Message: err.Error()},
			)
			return nil //nolint:nilerr // a per-entry error must not abort the whole scan
		}
		if path == "." {
			return nil
		}
		switch ClassifyEntry(path, d, ignore) {
		case RoleSkipDir:
			return fs.SkipDir
		case RoleManagedYAML:
			content, readErr := fs.ReadFile(fsys, path)
			if readErr != nil {
				scan.Diagnostics = append(
					scan.Diagnostics,
					manifestedit.Diagnostic{Level: manifestedit.DiagWarning, Path: path, Message: readErr.Error()},
				)
				return nil //nolint:nilerr // an unreadable file must not abort the whole scan
			}
			scan.YAMLFiles = append(scan.YAMLFiles, manifestedit.FileContent{Path: path, Content: content})
		case RoleOperatorArtifact:
			scan.NonYAML = append(scan.NonYAML, path)
		case RoleForeignFile:
			scan.NonYAML = append(scan.NonYAML, path)
			scan.Foreign = append(scan.Foreign, ForeignEntry{Path: path, Kind: ForeignFile})
		case RoleForeignSymlink:
			scan.Foreign = append(scan.Foreign, ForeignEntry{Path: path, Kind: ForeignSymlink})
		case RoleIgnored, RoleDescend:
			// Ignored content is never read; a normal directory is simply descended.
		}
		return nil
	})
	if walkErr != nil {
		scan.Diagnostics = append(
			scan.Diagnostics,
			manifestedit.Diagnostic{Level: manifestedit.DiagError, Path: ".", Message: walkErr.Error()},
		)
	}

	sort.Slice(scan.YAMLFiles, func(i, j int) bool { return scan.YAMLFiles[i].Path < scan.YAMLFiles[j].Path })
	sort.Strings(scan.NonYAML)
	sort.Slice(scan.Foreign, func(i, j int) bool { return scan.Foreign[i].Path < scan.Foreign[j].Path })
	return scan
}

// projectFileReport assembles per-document classification for one YAML file by
// merging the store's managed KRM documents (the authoritative manifest documents)
// with diagnostics (which cover empty, invalid, and non-KRM documents) on document
// index. fm is nil for a YAML file that holds no KRM document.
//
// DocumentModel no longer stores its position, so each managed document's true file
// index is reconstructed from the record-less diagnostic gaps: empty, non-KRM, and
// invalid documents leave a diagnostic at their position, and the managed documents
// fill the remaining positions in document order. It also returns the duplicate
// losers of this file keyed by their reconstructed RecordRef.
func projectFileReport(
	store *ManifestStore,
	path string,
	fm *FileModel,
	diags []manifestedit.Diagnostic,
) (FileReport, map[RecordRef]bool) {
	managedIdx := reconstructManagedIndices(fm, gapIndices(diags))

	docByIdx := map[int]*DocumentModel{}
	dups := map[RecordRef]bool{}
	if fm != nil {
		for i, dm := range fm.Documents {
			docByIdx[managedIdx[i]] = dm
			if store.IsDuplicate(dm) {
				dups[RecordRef{FilePath: path, DocumentIndex: managedIdx[i]}] = true
			}
		}
	}
	diagByIdx := map[int]manifestedit.Diagnostic{}
	for _, d := range diags {
		if _, ok := diagByIdx[d.DocumentIndex]; !ok {
			diagByIdx[d.DocumentIndex] = d
		}
	}

	fr := FileReport{Path: path, IsYAML: true}
	for _, i := range mergedIndices(docByIdx, diagByIdx) {
		// A managed KRM document always wins over a co-located diagnostic (for
		// example a non-editable document paired with a warning about anchors).
		if dm, ok := docByIdx[i]; ok {
			fr.Documents = append(fr.Documents, krmDocReport(i, dm))
			continue
		}
		fr.Documents = append(fr.Documents, DocumentReport{Index: i, Class: classFromDiag(diagByIdx[i])})
	}
	return fr, dups
}

// diagnosticsByPath groups diagnostics by their file path, preserving order. It is
// the shared input for position reconstruction (the report and the planner both
// recover a managed document's true file index from the record-less gaps).
func diagnosticsByPath(diags []manifestedit.Diagnostic) map[string][]manifestedit.Diagnostic {
	out := map[string][]manifestedit.Diagnostic{}
	for _, d := range diags {
		out[d.Path] = append(out[d.Path], d)
	}
	return out
}

// gapIndices collects the file positions held by a record-less document — empty,
// non-KRM, or invalid YAML — each of which leaves exactly one diagnostic at its
// position. The non-editable and duplicate reasons accompany a managed record and
// are therefore NOT gaps. mapping diagnostics (a different DiagReason value) also
// accompany a managed record and fall through.
func gapIndices(diags []manifestedit.Diagnostic) map[int]bool {
	gaps := map[int]bool{}
	for _, d := range diags {
		switch d.Reason {
		case manifestedit.ReasonEmptyDocument, manifestedit.ReasonNotKRM,
			manifestedit.ReasonInvalidYAML, manifestedit.ReasonMissingSopsKey:
			gaps[d.DocumentIndex] = true
		case manifestedit.ReasonNonEditable, manifestedit.ReasonDuplicateIdentity:
			// Accompany a managed record, so the record holds the position, not a gap.
		}
	}
	return gaps
}

// reconstructManagedIndices returns the true file index of each managed document in
// fm.Documents. The managed documents fill the file positions not taken by a gap
// (record-less) document, in document order, so the i-th managed document gets the
// i-th non-gap position. It returns nil for a file with no managed documents.
//
// LOAD-BEARING INVARIANT: every record-less document — empty/comment-only, non-KRM,
// invalid YAML, and a .sops.yaml missing its sops key — emits exactly one structured
// manifestedit diagnostic at its position (see indexOneFile), and no managed record
// shares a position with such a diagnostic. If a record-less document ever produced
// no diagnostic, its position would be wrongly handed to a managed document and every
// later managed index would shift. The mixed-gap test
// (TestReconstructManagedIndices_RecordlessGapsLeaveDiagnostics) guards this; the M4
// acceptance gate's impure-managed-file refusal means an accepted file has no gaps at
// all, so the reconstruction only ever matters on a tree that is being refused.
func reconstructManagedIndices(fm *FileModel, gaps map[int]bool) []int {
	if fm == nil {
		return nil
	}
	out := make([]int, len(fm.Documents))
	pos := 0
	for i := range fm.Documents {
		for gaps[pos] {
			pos++
		}
		out[i] = pos
		pos++
	}
	return out
}

// mergedIndices returns the sorted union of the managed-document and diagnostic
// positions, so the per-document report lists every position in file order.
func mergedIndices(docByIdx map[int]*DocumentModel, diagByIdx map[int]manifestedit.Diagnostic) []int {
	set := map[int]bool{}
	for i := range docByIdx {
		set[i] = true
	}
	for i := range diagByIdx {
		set[i] = true
	}
	out := make([]int, 0, len(set))
	for i := range set {
		out = append(out, i)
	}
	sort.Ints(out)
	return out
}

// krmDocReport builds a DocumentReport for one managed KRM document.
func krmDocReport(i int, dm *DocumentModel) DocumentReport {
	return DocumentReport{
		Index:    i,
		Class:    ClassKRM,
		GVK:      ParseGVK(dm.ManifestIdentity.APIVersion, dm.ManifestIdentity.Kind),
		Identity: dm.ManifestIdentity,
		Editable: dm.Editable,
		Cause:    causePtr(dm.Cause),
	}
}

// causePtr returns a pointer to a non-empty cause, or nil for a cleanly editable
// document, so the JSON omits "cause" entirely in the common case.
func causePtr(c DocumentCause) *DocumentCause {
	if c.Kind == CauseNone {
		return nil
	}
	return &c
}

// classFromDiag classifies a record-less document from its structured diagnostic
// reason. Classification reads the reason code, never the message text.
func classFromDiag(d manifestedit.Diagnostic) Class {
	switch d.Reason {
	case manifestedit.ReasonEmptyDocument:
		return ClassEmpty
	case manifestedit.ReasonInvalidYAML, manifestedit.ReasonMissingSopsKey:
		return ClassInvalidYAML
	case manifestedit.ReasonNotKRM:
		return ClassNonKRM
	case manifestedit.ReasonNonEditable, manifestedit.ReasonDuplicateIdentity:
		// These reasons always accompany a record, which wins the merge, so they are
		// never classified here; fall through to the non-KRM default for safety.
		return ClassNonKRM
	default:
		return ClassNonKRM
	}
}

// buildSummary produces the bounded overview. dupCount is the number of duplicate
// identities, derived by the caller from the store's collapsed index.
func buildSummary(files []FileReport, diags []manifestedit.Diagnostic, dupCount int) Summary {
	s := Summary{
		Duplicates:  dupCount,
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
			if d.Cause != nil && d.Cause.Kind == CauseEncrypted {
				s.Encrypted++
			}
		}
	}
	for _, d := range diags {
		s.Diagnostics[d.Level]++
	}
	return s
}

// buildIssues derives acceptance issues from the classified documents. These are
// the structural facts a stricter adoption policy may treat as blocking; whether
// each manifest belongs (the watched/unwatched comparison against a live API) is
// a deliberate later step.
// duplicates marks the (file, index) of every duplicate-identity loser; invalidMsgs
// carries the parse-error detail for invalid-YAML documents, both keyed by RecordRef.
func buildIssues(
	files []FileReport,
	duplicates map[RecordRef]bool,
	invalidMsgs map[RecordRef]string,
) []AcceptanceIssue {
	var issues []AcceptanceIssue
	for _, f := range files {
		for _, d := range f.Documents {
			ref := RecordRef{FilePath: f.Path, DocumentIndex: d.Index}
			if duplicates[ref] {
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
					Kind: IssueInvalidYAML, Path: f.Path, DocumentIndex: d.Index, Message: invalidMsgs[ref],
				})
			case ClassNonYAML, ClassEmpty, ClassKRM:
				// Not acceptance issues: ignored files, empty documents, and valid KRM.
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
