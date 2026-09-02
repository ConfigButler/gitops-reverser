// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// Repo discovery (the onboarding scan): walks a WHOLE repository once, enumerates candidate
// GitTarget subtrees, classifies each layout, runs the same acceptance gate the operator runs, and
// emits a machine-readable report. Deliberately reuse-heavy — only the whole-repo pass, candidate
// enumeration, layout classification and the report contract are new here.
//
// It REPORTS, it does not PROPOSE: no GitTarget/WatchRule generation, no repo-level refuse exit.
// See docs/design/support-boundary/repo-discovery-and-onboarding-scan.md.

// Layout is the structural shape of a candidate subtree. Layout and acceptedByOperator are
// two distinct truths: a kustomize-overlay has a well-understood layout and is now adopted
// when its render scope passes the gate, but its editable count can still be 0 when every
// rendered field is base-owned.
type Layout string

const (
	// LayoutPlain is a directory of raw KRM documents with explicit namespaces and no
	// kustomization — the "one plain folder per environment" launch layout. Accepted.
	LayoutPlain Layout = "plain"
	// LayoutKustomizeSingle is a self-contained render root: one kustomization whose
	// resources graph stays within its own subtree (local files, or a base directory
	// nested underneath it). Accepted — the operator can render the whole subtree.
	LayoutKustomizeSingle Layout = "kustomize-single"
	// LayoutKustomizeOverlay is a render root that reaches a base kustomization OUTSIDE
	// its own subtree (the classic base/ + overlays/{env} shape reached via ../../base).
	// Render-root scoping shipped, so the operator adopts it when the adoption gate accepts
	// its render scope; the editable count shows how much of the render the overlay owns (a
	// pure passthrough overlay is adopted yet editable: 0). It is a distinct layout from
	// kustomize-single because it still renders — and cannot edit — an out-of-subtree base.
	LayoutKustomizeOverlay Layout = "kustomize-overlay"
	// LayoutRefusedStructural is a render root whose kustomization uses a feature the
	// contextual-namespace writer cannot map back to editable source (helm inflation,
	// generators, patches, components, name(pre|suf)fix, remote bases, malformed
	// images/replicas). This is the support boundary: the folder cannot be adopted as it
	// stands, and each refusal says whether anyone can solve that.
	LayoutRefusedStructural Layout = "refused-structural"
)

// ReasonRefusedStructural is the support boundary: a render root whose kustomization
// uses a construct the writer cannot map back to editable source. It is the only render-root
// refusal reason now that external-base overlays are adopted through render-root scoping (the
// former forward-looking overlay-fan-out-unsupported reason is retired; the public
// pkg/manifestanalyzer constant is kept for consumers pinning the string, but the classifier no
// longer emits it). An overlay refused for a real structural fault carries that fault's own
// gate-issue code instead.
const ReasonRefusedStructural = "refused-structural"

// RefusalReason is one machine-readable reason a candidate is not accepted, with a
// human detail. A candidate carries none when accepted.
type RefusalReason struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
	// Solvable and Actor carry the refusal's own answer to "can this be solved, and by
	// whom" — projected verbatim from the acceptance issue that raised it, or decided by
	// the raise site for the reasons that are not issue kinds. Empty means unclassified; say
	// nothing about whether it can be solved.
	Solvable bool  `json:"solvable"`
	Actor    Actor `json:"actor,omitempty"`
}

// RenderedTypes is what a folder renders, keeping the PAIRING between a type and the namespace it
// lands in. A set of types beside a set of namespaces loses it: a Deployment into frontend and a
// Service into backend would read as four combinations, and a tool generating one rule per pair
// would authorize two that match nothing.
//
// For a render root the sets come off a real kustomize build, so an outside base is included and a
// `namespace:` transformer is applied. A root that failed to build reports nothing: what it
// renders is not knowable.
type RenderedTypes struct {
	// ByNamespace lists the types that land in each namespace, sorted, keyed by namespace.
	ByNamespace map[string][]string `json:"byNamespace,omitempty"`

	// NamespaceUndeclared lists the types that render WITHOUT a namespace, sorted.
	//
	// NOT a list of cluster-scoped types: it cannot tell a genuinely cluster-scoped type from a
	// namespaced one relying on the applier's default, which needs API discovery. A type can
	// appear here AND under ByNamespace, which is an ordinary folder, not a contradiction.
	NamespaceUndeclared []string `json:"namespaceUndeclared,omitempty"`
}

// ResourceCounts splits the KRM a candidate covers into what it renders versus what it
// can actually edit. For a plain or self-contained kustomize candidate the two are
// equal; for an overlay they diverge — rendered counts the documents pulled from the
// out-of-subtree base, editable counts only the source physically in the candidate's
// own subtree (zero for a pure overlay), making the gap legible at a glance.
type ResourceCounts struct {
	// Rendered is the number of managed KRM documents the candidate renders: its own
	// subtree plus every base it reads (readScope).
	Rendered int `json:"rendered"`
	// Editable is the number of managed KRM documents physically in the candidate's own
	// subtree — the source the operator would own and write in place.
	Editable int `json:"editable"`
	// NonKRM is the number of non-KRM YAML documents and foreign (non-YAML/symlink)
	// entries in the candidate's own subtree. Retained build directives (kustomization
	// files), operator artifacts (README.md), and accepted benign passengers (a license,
	// docs, .gitignore) are neither KRM nor NonKRM and are not counted.
	NonKRM int `json:"nonKrm"`
}

// RepoCandidate is one subtree the product could turn into a GitTarget, with its
// layout, current operator acceptance, and the facts a tool built on top needs to decide.
// This cut reports these; it proposes no GitTarget/WatchRule.
type RepoCandidate struct {
	// Path is the candidate directory, slash-separated and relative to the repo root.
	Path string `json:"path"`
	// Layout is the candidate's structural shape.
	Layout Layout `json:"layout"`
	// AcceptedByOperator reports whether the operator would adopt this subtree today.
	AcceptedByOperator bool `json:"acceptedByOperator"`
	// RefusalReasons explains a non-acceptance; empty when accepted.
	RefusalReasons []RefusalReason `json:"refusalReasons,omitempty"`
	// RenderRoot reports whether the candidate is a kustomize render root (versus a
	// plain KRM folder).
	RenderRoot bool `json:"renderRoot"`
	// ReadScope lists the directories outside this candidate's own subtree whose content
	// its build renders: base kustomization directories its resources graph reaches, and
	// the directories holding individual resource files it renders from elsewhere. Empty
	// for plain and self-contained candidates.
	ReadScope []string `json:"readScope,omitempty"`
	// ReadBy is the reverse edge: the candidates whose build reads THIS directory. It is
	// usually empty, and structurally so — a directory another kustomization references is
	// never a render root, hence never a candidate — but not always: a plain folder holding
	// one file some distant kustomization lists in resources: is a candidate that another
	// folder depends on, and adopting it is the disruptive case a consumer must be able to
	// see. The rest of these edges end at directories no candidate list mentions; see
	// [RepoSummary.ReadEdges].
	ReadBy []string `json:"readBy,omitempty"`
	// InferredNamespace is the namespace the candidate resolves to: the kustomization's
	// namespace transformer for a render root, or the single explicit metadata.namespace
	// for a plain folder. Empty when none is set or the folder is ambiguous.
	InferredNamespace string `json:"inferredNamespace,omitempty"`
	// Resources counts the KRM this candidate covers (rendered vs editable) plus non-KRM.
	Resources ResourceCounts `json:"resources"`
	// RenderedTypes says which type lands in which namespace when this candidate renders.
	// It is what a tool must know before it provisions from the folder: the schemas that
	// have to be served where this is applied, and the exact (type, namespace) pairs a
	// watch rule can be written for. Empty for a render root kustomize could not build.
	RenderedTypes RenderedTypes `json:"renderedTypes"`
	// OverlapsWith lists other candidate paths this one nests with. Two overlapping
	// candidates can never both be proposed (one-owner-per-folder); the conflict is
	// reported, not resolved, in this cut.
	OverlapsWith []string `json:"overlapsWith,omitempty"`
}

// OverlapConflict records a nesting conflict between two candidates: ancestor strictly
// contains descendant in the folder tree.
type OverlapConflict struct {
	Ancestor   string `json:"ancestor"`
	Descendant string `json:"descendant"`
}

// ReadEdge is one folder-to-folder read: From's build renders documents that live in To,
// which is outside From's own subtree. From is always a candidate; To is a candidate only
// in the file-reference case described on [RepoCandidate.ReadBy], and is otherwise a
// directory the scan offers to nobody.
type ReadEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// RepoSummary is the repo-level roll-up a product uses to describe onboardability.
type RepoSummary struct {
	// CandidatesByLayout counts candidates per layout class.
	CandidatesByLayout map[Layout]int `json:"candidatesByLayout"`
	// Accepted and Refused count candidates by current operator acceptance.
	Accepted int `json:"accepted"`
	Refused  int `json:"refused"`
	// OverlapConflicts lists every nesting conflict between candidates.
	OverlapConflicts []OverlapConflict `json:"overlapConflicts,omitempty"`
	// ReadEdges is the repo's folder dependency graph, collected in one place so a consumer can
	// draw it without walking the candidates.
	//
	// Most edges end at a directory that is NOT a candidate, and nothing else identifies those
	// nodes: they are not all kustomize bases, since a referenced resource file or an
	// out-of-subtree patch makes its folder one too.
	ReadEdges []ReadEdge `json:"readEdges,omitempty"`
	// UnsupportedConstructs is the sorted, de-duplicated set of unsupported kustomize
	// features seen across refused-structural candidates, so a product can say "this repo
	// uses Helm inflation, which we don't manage".
	UnsupportedConstructs []string `json:"unsupportedConstructs,omitempty"`
}

// RepoReport is the whole-repo discovery report: the machine-readable contract the
// a tool built on top of the operator consumes.
type RepoReport struct {
	// Root is the scanned repository root as passed to ScanRepo. It is informational.
	Root string `json:"root,omitempty"`
	// Candidates are the enumerated subtrees, sorted by path.
	Candidates []RepoCandidate `json:"candidates"`
	// Summary is the repo-level roll-up.
	Summary RepoSummary `json:"summary"`
}

// ScanRepo is the whole-repo discovery pass (the library entry point; the CLI
// --mode scan-repo is a thin wrapper). It is read-only, writes nothing, needs no
// cluster, and never follows symlinks — the same posture as ScanDir, just over the
// whole tree rather than one subtree. It verifies root is a directory, then walks
// os.DirFS(root).
func ScanRepo(ctx context.Context, root string) (RepoReport, error) {
	info, err := os.Stat(root)
	if err != nil {
		return RepoReport{}, err
	}
	if !info.IsDir() {
		return RepoReport{}, fmt.Errorf("not a directory: %s", root)
	}
	rep := scanRepoFS(ctx, os.DirFS(root))
	rep.Root = root
	return rep, nil
}

// scanRepoFS is ScanRepo over an fs.FS, so it is testable against an in-memory tree.
func scanRepoFS(ctx context.Context, fsys fs.FS) RepoReport {
	scan := collectFiles(fsys)
	kusts := parseKustomizations(scan.YAMLFiles)
	// Structure-only whole-repo store built with the live writer's allowlist (WriterAllowlist:
	// kustomization files + the operator's .sops.yaml bootstrap config), so acceptance and the
	// document counts match what the operator would actually adopt. Acceptance is decided
	// per-candidate against its own subtree, not from this whole-repo store.
	store := buildStore(ctx, scan, nil, WriterAllowlist())
	kustContent := kustomizationContentByDir(scan)
	ownedFiles := reachedResourceFiles(kusts)

	candidates := make([]RepoCandidate, 0)
	for _, rootDir := range renderRoots(kusts) {
		candidates = append(candidates, classifyRenderRoot(ctx, fsys, rootDir, scan, kusts, kustContent, store))
	}
	candidates = append(candidates, plainCandidates(ctx, fsys, store, kusts, ownedFiles)...)

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Path < candidates[j].Path })
	detectOverlaps(candidates)
	edges := buildReadGraph(candidates)

	summary := summarize(candidates, kusts)
	summary.ReadEdges = edges

	return RepoReport{
		Candidates: candidates,
		Summary:    summary,
	}
}

// classifyRenderRoot classifies one kustomize render root into a candidate: refused
// (unsupported kustomization), an external-base kustomize-overlay, or a self-contained
// kustomize-single. An overlay is no longer refused merely for reaching an out-of-subtree
// base — render-root scoping shipped, so the operator adopts it — and instead runs the same
// adoption gate over its render scope.
func classifyRenderRoot(
	ctx context.Context,
	fsys fs.FS,
	rootDir string,
	scan FolderScan,
	kusts map[string]*kustomizationDoc,
	kustContent map[string][]byte,
	store *ManifestStore,
) RepoCandidate {
	c := RepoCandidate{Path: rootDir, RenderRoot: true, InferredNamespace: renderRootNamespace(kusts, rootDir, store)}
	// What the root actually renders to, from the build the scan already ran. A root that
	// failed to build has none, which is honest: we cannot know what it renders.
	if inv, ok := store.RenderedInventory[rootDir]; ok {
		c.RenderedTypes = inv
	}
	// Every folder this root renders from and does not own — reported for a refused root
	// too, since "what does it read" is answerable from the resources graph whether or not
	// the operator would adopt it.
	c.ReadScope = readDirsOutside(rootDir, kusts)
	// rendered/editable count only the documents the kustomization graph actually renders
	// (its resources: entries), never parked YAML a kustomization does not reference.
	rendered := reachedResourceFilesFrom(rootDir, kusts)

	if doc := kusts[rootDir]; doc == nil || doc.unsupported {
		c.Layout = LayoutRefusedStructural
		c.AcceptedByOperator = false
		c.RefusalReasons = []RefusalReason{refusedStructuralReason(kusts[rootDir], kustContent[rootDir])}
		c.Resources = countResources(store, rootDir, rendered)
		return c
	}

	outsideBases := outOfSubtreeBases(rootDir, kusts)
	if len(outsideBases) > 0 {
		// External-base overlay: the operator now renders it through render-root scoping, so it
		// is adopted when the same gate the live writer runs accepts its render scope. The
		// editable count still shows how much the overlay owns — a pure passthrough overlay is
		// adoptable yet editable: 0, since every field is base-owned. A gate refusal (foreign
		// content in the overlay, an unbuildable base, an unsupported nested kustomization)
		// surfaces as its own reason rather than a blanket overlay refusal.
		c.Layout = LayoutKustomizeOverlay
		acc := overlayCandidateAcceptance(ctx, rootDir, scan, kusts)
		c.AcceptedByOperator = acc.Accepted
		if !acc.Accepted {
			c.RefusalReasons = issuesToReasons(acc.Issues)
		}
		c.Resources = countResources(store, rootDir, rendered)
		return c
	}

	// Self-contained render root: run the same gate the operator runs, scoped to the
	// subtree. A within-subtree base is reachable, so acceptance is truthful here; a gate
	// refusal (duplicate, non-KRM, foreign, unsupported nested kustomization, …) is
	// surfaced as refusal reasons rather than a bare false.
	c.Layout = LayoutKustomizeSingle
	acc := candidateAcceptance(ctx, fsys, rootDir)
	c.AcceptedByOperator = acc.Accepted
	if !acc.Accepted {
		c.RefusalReasons = issuesToReasons(acc.Issues)
	}
	c.Resources = countResources(store, rootDir, rendered)
	return c
}

// plainFolderInventory is the rendered-type map of a folder with no kustomization: the
// documents ARE the render, so the types and the namespaces come straight off them.
// Nothing transforms them on the way to the cluster, which is what makes a plain folder
// plain.
func plainFolderInventory(store *ManifestStore, dir string) RenderedTypes {
	byNamespace := map[string]map[string]struct{}{}
	undeclared := map[string]struct{}{}
	for filePath, fm := range store.FilesByPath {
		if !pathWithin(filePath, dir) {
			continue
		}
		for _, dm := range fm.Documents {
			id := dm.ManifestIdentity
			name := ParseGVK(id.APIVersion, id.Kind).String()
			if id.Namespace == "" {
				undeclared[name] = struct{}{}
				continue
			}
			if byNamespace[id.Namespace] == nil {
				byNamespace[id.Namespace] = map[string]struct{}{}
			}
			byNamespace[id.Namespace][name] = struct{}{}
		}
	}
	return RenderedTypes{
		ByNamespace:         sortedTypesByNamespace(byNamespace),
		NamespaceUndeclared: sortedKeysOfSet(undeclared),
	}
}

// plainCandidates enumerates plain KRM leaf folders: directories that directly hold a
// managed KRM document, carry no kustomization, and are not already owned by a
// kustomization's resources graph (so a base a kustomization renders is not also
// proposed as a bare folder).
func plainCandidates(
	ctx context.Context,
	fsys fs.FS,
	store *ManifestStore,
	kusts map[string]*kustomizationDoc,
	ownedFiles map[string]struct{},
) []RepoCandidate {
	dirs := map[string]struct{}{}
	for filePath, fm := range store.FilesByPath {
		if len(fm.Documents) == 0 {
			continue
		}
		dir := slashDir(filePath)
		if _, isKust := kusts[dir]; isKust {
			continue // a kustomization directory is a render root, not a plain folder
		}
		if _, owned := ownedFiles[filePath]; owned {
			continue // a resource file some kustomization already renders
		}
		dirs[dir] = struct{}{}
	}

	out := make([]RepoCandidate, 0, len(dirs))
	for dir := range dirs {
		acc := candidateAcceptance(ctx, fsys, dir)
		renderedTypes := plainFolderInventory(store, dir)
		cand := RepoCandidate{
			Path:               dir,
			Layout:             LayoutPlain,
			AcceptedByOperator: acc.Accepted,
			InferredNamespace:  singleExplicitNamespace(store, dir),
			RenderedTypes:      renderedTypes,
			// A plain folder is applied directory-wise, so it renders its whole subtree
			// (renderedFiles nil); no kustomization graph scopes it.
			Resources: countResources(store, dir, nil),
		}
		if !acc.Accepted {
			cand.RefusalReasons = issuesToReasons(acc.Issues)
		}
		out = append(out, cand)
	}
	return out
}

// overlayCandidateAcceptance runs the adoption gate over an overlay's RENDER SCOPE: the subtree
// plus the exact base files its graph reaches, matching the live writer's render-root scoping.
// Only reached files enter the scope, never a whole base directory, so parked YAML a base does not
// reference can never refuse the overlay.
//
// The scoped store keeps repo-relative paths, so `../../base` resolves exactly as kustomize
// resolves it. This is folder adoption only; the write half is out of scope for a read-only report.
func overlayCandidateAcceptance(
	ctx context.Context,
	rootDir string,
	scan FolderScan,
	kusts map[string]*kustomizationDoc,
) Acceptance {
	reached := renderScopePaths(rootDir, kusts)
	scoped := FolderScan{}
	for _, f := range scan.YAMLFiles {
		if pathWithin(f.Path, rootDir) || setContains(reached, f.Path) {
			scoped.YAMLFiles = append(scoped.YAMLFiles, f)
		}
	}
	// Only the overlay's OWN non-YAML/foreign content bears on its acceptance; a base's loose
	// files are never read (the render scope pulls in referenced files only), just as at runtime.
	for _, p := range scan.NonYAML {
		if pathWithin(p, rootDir) {
			scoped.NonYAML = append(scoped.NonYAML, p)
		}
	}
	for _, fe := range scan.Foreign {
		if pathWithin(fe.Path, rootDir) {
			scoped.Foreign = append(scoped.Foreign, fe)
		}
	}
	store := buildStore(ctx, scoped, nil, WriterAllowlist())
	return Accept(store, AcceptancePolicy{Allowlist: WriterAllowlist()})
}

// renderScopePaths returns the files an overlay render root reaches through its resources and
// patches graph: every referenced resource file, plus each reached kustomization's own file
// and strategic-merge patch inputs. These are the exact build inputs kustomize loads, so the
// scoped acceptance store can render `../../base` without importing a base's unreferenced
// content. Paths are scan-root-relative slash paths, matching the store's file keys.
func renderScopePaths(rootDir string, kusts map[string]*kustomizationDoc) map[string]struct{} {
	paths := map[string]struct{}{}
	for f := range reachedResourceFilesFrom(rootDir, kusts) {
		paths[f] = struct{}{}
	}
	dirs := reachedKustomizationDirs(rootDir, kusts)
	dirs[rootDir] = struct{}{}
	for dir := range dirs {
		doc := kusts[dir]
		if doc == nil {
			continue
		}
		paths[doc.path] = struct{}{}
		for _, patch := range doc.patches {
			paths[patch] = struct{}{}
		}
	}
	return paths
}

// setContains reports membership in a set-like map.
func setContains(set map[string]struct{}, key string) bool {
	_, ok := set[key]
	return ok
}

// candidateAcceptance runs the structure-only adoption gate over the candidate subtree —
// the exact gate the operator's live writer runs (Scan with WriterAllowlist, which retains
// the kustomize build directives and the operator's .sops.yaml bootstrap config). It
// returns the full acceptance so a refused candidate can carry the gate's issues as
// refusal reasons rather than collapsing them to a bare boolean.
func candidateAcceptance(ctx context.Context, fsys fs.FS, dir string) Acceptance {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		return Acceptance{Issues: []AcceptanceIssue{{
			Kind: IssueForeignFile, Path: dir, Message: err.Error(),
			Solvable: true, Actor: ActorRepositoryAuthor,
		}}}
	}
	policy := ScanPolicy{Acceptance: AcceptancePolicy{Allowlist: WriterAllowlist()}}
	return Scan(ctx, sub, nil, nil, policy).Acceptance
}

// issuesToReasons projects acceptance-gate issues into refusal reasons, so a refused candidate
// reports WHY rather than just acceptedByOperator: false. It is the single choke point for that
// mapping, so a check that classifies itself reaches a consumer without a second table. The issue
// Kind is the machine code; the path-qualified message is the detail.
func issuesToReasons(issues []AcceptanceIssue) []RefusalReason {
	out := make([]RefusalReason, 0, len(issues))
	for _, iss := range issues {
		detail := iss.Message
		if iss.Path != "" {
			detail = iss.Path + ": " + iss.Message
		}
		out = append(out, RefusalReason{
			Code:     string(iss.Kind),
			Detail:   detail,
			Solvable: iss.Solvable,
			Actor:    iss.Actor,
		})
	}
	return out
}

// readDirsOutside returns the sorted, MINIMAL set of directories a render root reads that
// lie outside its own subtree — every folder whose content the root renders yet does not
// own.
//
// The directory projection of [renderScopePaths], deliberately: that function already answers
// which files a build loads, and a second enumeration here would drift from it. A base directory,
// a `resources: ../shared/x.yaml` and a `patches: [{path: ../../shared/p.yaml}]` are the same
// fact, and only one of the three is a kustomize base.
//
// Minimal: a directory nested under another in the set is dropped. This is the ONLY relation the
// read graph is built from, so its three projections cannot disagree.
func readDirsOutside(rootDir string, kusts map[string]*kustomizationDoc) []string {
	dirs := map[string]struct{}{}
	for file := range renderScopePaths(rootDir, kusts) {
		if dir := slashDir(file); !pathWithin(dir, rootDir) {
			dirs[dir] = struct{}{}
		}
	}
	if len(dirs) == 0 {
		return nil // a self-contained root reads nothing outside itself; say so with an absent key
	}
	out := minimalDirs(sortedKeysOf(dirs))
	sort.Strings(out)
	return out
}

// buildReadGraph turns the per-candidate read scopes into the repo-level edge list and
// fills in the reverse direction on each candidate. The edges are the transpose source:
// ReadBy is derived here rather than recomputed from the kustomization graph, so a folder
// listed as read by X always has X listing it under readScope.
func buildReadGraph(candidates []RepoCandidate) []ReadEdge {
	edges := make([]ReadEdge, 0)
	readers := map[string][]string{}
	for _, c := range candidates {
		for _, dir := range c.ReadScope {
			edges = append(edges, ReadEdge{From: c.Path, To: dir})
			readers[dir] = append(readers[dir], c.Path)
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})
	for i := range candidates {
		if from := readers[candidates[i].Path]; len(from) > 0 {
			sort.Strings(from)
			candidates[i].ReadBy = from
		}
	}
	return edges
}

// nonCandidateTargets returns the sorted directories the read edges point at that are not
// themselves candidates — the other half of the graph's nodes, offered to nobody while
// several candidates render from them. It is a set difference over the report's own two
// lists, computed here for the text view; the JSON contract publishes the edges and the
// candidates and lets a consumer do the same subtraction, rather than shipping an index
// that could fall out of step with either.
func nonCandidateTargets(candidates []RepoCandidate, edges []ReadEdge) []string {
	isCandidate := map[string]struct{}{}
	for _, c := range candidates {
		isCandidate[c.Path] = struct{}{}
	}
	bases := map[string]struct{}{}
	for _, e := range edges {
		if _, ok := isCandidate[e.To]; !ok {
			bases[e.To] = struct{}{}
		}
	}
	if len(bases) == 0 {
		return nil
	}
	return sortedKeysOf(bases)
}

// outOfSubtreeBases returns the sorted, MINIMAL base kustomization directories a render
// root reaches that lie OUTSIDE its own subtree — the escaping-subtree fact that makes an
// overlay unrenderable by the operator today. Bases nested within the subtree do not
// count (the operator can render them, so the root stays kustomize-single). The set is
// minimal: a reached base nested under another reached base is dropped, since it is read
// transitively through its parent — this keeps readScope non-overlapping so the rendered
// document count never double-counts a shared nested base.
func outOfSubtreeBases(rootDir string, kusts map[string]*kustomizationDoc) []string {
	var out []string
	for base := range reachedKustomizationDirs(rootDir, kusts) {
		if !pathWithin(base, rootDir) {
			out = append(out, base)
		}
	}
	out = minimalDirs(out)
	sort.Strings(out)
	return out
}

// minimalDirs drops any directory nested under another directory in the set, leaving only
// the top-level roots. Used so readScope reports (and counts) a base and its own nested
// base once, through the parent, rather than twice.
func minimalDirs(dirs []string) []string {
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		nested := false
		for _, other := range dirs {
			if other != d && pathWithin(d, other) {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, d)
		}
	}
	return out
}

// reachedKustomizationDirs returns every kustomization directory reachable from
// rootDir through the resources graph (excluding rootDir itself), following the same
// cleanJoin resolution the store uses. The on-path set bounds cycles.
func reachedKustomizationDirs(rootDir string, kusts map[string]*kustomizationDoc) map[string]struct{} {
	reached := map[string]struct{}{}
	onPath := map[string]struct{}{}
	var walk func(dir string)
	walk = func(dir string) {
		cur := kusts[dir]
		if cur == nil {
			return
		}
		if _, cycling := onPath[dir]; cycling {
			return
		}
		onPath[dir] = struct{}{}
		for _, entry := range cur.resources {
			target := cleanJoin(dir, entry)
			if target == "" {
				continue
			}
			if _, isKust := kusts[target]; isKust {
				reached[target] = struct{}{}
				walk(target)
			}
		}
		delete(onPath, dir)
	}
	walk(rootDir)
	return reached
}

// reachedResourceFiles is the set of resource-file paths (non-kustomization targets)
// any kustomization in the repo references. A plain folder whose file is in this set is
// already owned by a render and is not proposed as a bare candidate.
func reachedResourceFiles(kusts map[string]*kustomizationDoc) map[string]struct{} {
	out := map[string]struct{}{}
	for dir, k := range kusts {
		for _, entry := range k.resources {
			target := cleanJoin(dir, entry)
			if target == "" {
				continue
			}
			if _, isKust := kusts[target]; !isKust {
				out[target] = struct{}{}
			}
		}
	}
	return out
}

// refusedStructuralReason builds the render-root refusal, classified by the constructs
// that caused it rather than by the code.
//
// The code MEANS "the writer cannot map this root back to editable source", so "no" is the answer
// when the constructs are unknown. But the gate judges the same folder construct by construct when
// refusing a NESTED kustomization, and the two surfaces must not give a consumer two answers about
// one directory: a root refused only because its kustomization does not parse is one commit from
// adoptable. Classifying both from the same feature set keeps them agreeing.
func refusedStructuralReason(doc *kustomizationDoc, content []byte) RefusalReason {
	var features []string
	if doc != nil {
		features = doc.features
	}
	// One classification, used twice: it sets Solvable and it picks the prose stem. The
	// detail cannot contradict the field when neither is computed independently of the other.
	class := classifyKustomizeFeatures(features)
	if doc == nil {
		class = Classification{}
	}
	return RefusalReason{
		Code:     ReasonRefusedStructural,
		Detail:   unsupportedKustomizeDetail(class, features, kustomizationDecodeError(content)),
		Solvable: class.Solvable,
		Actor:    class.Actor,
	}
}

// renderRootNamespace resolves the namespace a render root renders under: the
// kustomization's own namespace transformer, falling back to a single explicit
// namespace on its resources when the kustomization sets none.
func renderRootNamespace(kusts map[string]*kustomizationDoc, rootDir string, store *ManifestStore) string {
	if doc := kusts[rootDir]; doc != nil && doc.namespace != "" {
		return doc.namespace
	}
	return singleExplicitNamespace(store, rootDir)
}

// singleExplicitNamespace returns the one explicit metadata.namespace shared by every
// managed document under dir, or "" when there is none or they disagree.
func singleExplicitNamespace(store *ManifestStore, dir string) string {
	seen := map[string]struct{}{}
	for filePath, fm := range store.FilesByPath {
		if !pathWithin(filePath, dir) {
			continue
		}
		for _, dm := range fm.Documents {
			if ns := dm.ManifestIdentity.Namespace; ns != "" {
				seen[ns] = struct{}{}
			}
		}
	}
	if len(seen) != 1 {
		return ""
	}
	for ns := range seen {
		return ns
	}
	return ""
}

// countResources counts the KRM a candidate renders and can edit, plus non-KRM noise in
// its own subtree. renderedFiles is the exact set of resource files the candidate renders —
// the kustomization resources graph for a render root; a nil set means "every managed file
// in the candidate's own subtree", the plain-folder case applied directory-wise. rendered
// counts documents in the rendered files; editable counts the subset physically in the
// candidate's own subtree (the source the operator would own and write) — a pure overlay
// renders its base but edits nothing locally (editable = 0). nonKRM counts non-KRM YAML
// documents and foreign entries under dir.
func countResources(store *ManifestStore, dir string, renderedFiles map[string]struct{}) ResourceCounts {
	var rendered, editable int
	for filePath, fm := range store.FilesByPath {
		if !fileIsRendered(filePath, dir, renderedFiles) {
			continue
		}
		rendered += len(fm.Documents)
		if pathWithin(filePath, dir) {
			editable += len(fm.Documents)
		}
	}
	return ResourceCounts{Rendered: rendered, Editable: editable, NonKRM: nonKRMUnder(store, dir)}
}

// fileIsRendered reports whether a managed file counts toward a candidate's rendered set:
// membership in renderedFiles for a kustomize candidate, or presence in the candidate's own
// subtree when renderedFiles is nil (a plain folder renders its whole directory).
func fileIsRendered(filePath, dir string, renderedFiles map[string]struct{}) bool {
	if renderedFiles == nil {
		return pathWithin(filePath, dir)
	}
	_, ok := renderedFiles[filePath]
	return ok
}

// reachedResourceFilesFrom returns the set of resource-file paths a render root actually
// renders: the non-kustomization targets reached by following the resources graph from
// rootDir. Each kustomization contributes only the entries it lists, so parked YAML a
// kustomization does not reference is never counted. The on-path set bounds cycles.
func reachedResourceFilesFrom(rootDir string, kusts map[string]*kustomizationDoc) map[string]struct{} {
	files := map[string]struct{}{}
	onPath := map[string]struct{}{}
	var walk func(dir string)
	walk = func(dir string) {
		cur := kusts[dir]
		if cur == nil {
			return
		}
		if _, cycling := onPath[dir]; cycling {
			return
		}
		onPath[dir] = struct{}{}
		for _, entry := range cur.resources {
			target := cleanJoin(dir, entry)
			if target == "" {
				continue
			}
			if _, isKust := kusts[target]; isKust {
				walk(target)
			} else {
				files[target] = struct{}{}
			}
		}
		delete(onPath, dir)
	}
	walk(rootDir)
	return files
}

// nonKRMUnder counts non-KRM YAML documents and foreign entries under dir. Retained
// build directives, operator artifacts, accepted benign passengers, and values files a
// release names as read-only context are excluded (they are neither KRM nor noise).
func nonKRMUnder(store *ManifestStore, dir string) int {
	n := 0
	for _, d := range store.Diagnostics {
		if d.Reason == manifestedit.ReasonNotKRM && pathWithin(d.Path, dir) &&
			!store.isReferencedValuesFile(d.Path) {
			n++
		}
	}
	for _, f := range store.Foreign {
		if pathWithin(f.Path, dir) {
			n++
		}
	}
	return n
}

// detectOverlaps fills OverlapsWith on each candidate and returns nothing; the summary
// collects the conflicts separately. Two candidates overlap when one strictly contains
// the other — the one-owner-per-folder invariant mirrored from gittarget_path_overlap.
func detectOverlaps(candidates []RepoCandidate) {
	for i := range candidates {
		for j := i + 1; j < len(candidates); j++ {
			a, b := candidates[i].Path, candidates[j].Path
			if pathWithin(a, b) || pathWithin(b, a) {
				candidates[i].OverlapsWith = append(candidates[i].OverlapsWith, b)
				candidates[j].OverlapsWith = append(candidates[j].OverlapsWith, a)
			}
		}
	}
}

// summarize rolls the candidates up into the repo-level summary. Unsupported constructs
// are recomputed from each refused-structural candidate's kustomization bytes, so the
// summary shares one source of truth with the per-candidate detail.
func summarize(candidates []RepoCandidate, kusts map[string]*kustomizationDoc) RepoSummary {
	s := RepoSummary{CandidatesByLayout: map[Layout]int{}}
	constructs := map[string]struct{}{}
	for _, c := range candidates {
		s.CandidatesByLayout[c.Layout]++
		if c.AcceptedByOperator {
			s.Accepted++
		} else {
			s.Refused++
		}
		if doc := kusts[c.Path]; c.Layout == LayoutRefusedStructural && doc != nil {
			for _, f := range doc.features {
				constructs[f] = struct{}{}
			}
		}
		for _, other := range c.OverlapsWith {
			if pathWithin(other, c.Path) { // c is the ancestor of other
				s.OverlapConflicts = append(s.OverlapConflicts, OverlapConflict{Ancestor: c.Path, Descendant: other})
			}
		}
	}
	if len(constructs) > 0 {
		s.UnsupportedConstructs = sortedKeysOf(constructs)
	}
	sort.Slice(s.OverlapConflicts, func(i, j int) bool {
		if s.OverlapConflicts[i].Ancestor != s.OverlapConflicts[j].Ancestor {
			return s.OverlapConflicts[i].Ancestor < s.OverlapConflicts[j].Ancestor
		}
		return s.OverlapConflicts[i].Descendant < s.OverlapConflicts[j].Descendant
	})
	return s
}

// kustomizationContentByDir maps each kustomization directory to its raw bytes, so the
// refused-structural detail can name the specific unsupported features.
func kustomizationContentByDir(scan FolderScan) map[string][]byte {
	out := map[string][]byte{}
	for _, f := range scan.YAMLFiles {
		if isKustomizationFile(f.Path) {
			out[slashDir(f.Path)] = f.Content
		}
	}
	return out
}

// pathWithin reports whether the slash path p is within dir: equal to it, or nested
// under it on a segment boundary ("a/b" is within "a" but "ab" is not).
func pathWithin(p, dir string) bool {
	p = path.Clean(p)
	dir = path.Clean(dir)
	if dir == "." {
		return true // the repo root contains every path
	}
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// sortedKeysOf returns the sorted keys of a string-keyed map, so a walk over it emits
// in deterministic order.
func sortedKeysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// readersOf returns the candidates that read dir, read off the edge list so the text view
// and the JSON contract answer from the same place.
func readersOf(edges []ReadEdge, dir string) []string {
	out := make([]string, 0, 1)
	for _, e := range edges {
		if e.To == dir {
			out = append(out, e.From)
		}
	}
	return out
}

// RenderRepoText writes a compact human summary of the repo report: one line per
// candidate, then the roll-up. It is a convenience view; JSON is the contract.
func RenderRepoText(w io.Writer, rep RepoReport) {
	fmt.Fprintf(w, "repo: %s\n", rep.Root)
	fmt.Fprintf(w, "candidates: %d\n", len(rep.Candidates))
	for _, c := range rep.Candidates {
		status := "accepted"
		if !c.AcceptedByOperator {
			status = "refused"
			if len(c.RefusalReasons) > 0 {
				status = c.RefusalReasons[0].Code
			}
		}
		ns := c.InferredNamespace
		if ns == "" {
			ns = "-"
		}
		fmt.Fprintf(w, "  %-40s %-18s %-10s ns=%-16s rendered=%d editable=%d\n",
			c.Path, c.Layout, status, ns, c.Resources.Rendered, c.Resources.Editable)
		if len(c.ReadScope) > 0 {
			fmt.Fprintf(w, "      reads: %s\n", strings.Join(c.ReadScope, ", "))
		}
		if len(c.ReadBy) > 0 {
			fmt.Fprintf(w, "      read by: %s\n", strings.Join(c.ReadBy, ", "))
		}
		if len(c.OverlapsWith) > 0 {
			fmt.Fprintf(w, "      overlaps: %s\n", strings.Join(c.OverlapsWith, ", "))
		}
	}
	// The other half of the graph never appears above: candidates render from these folders
	// and none of them is offered, so a reader who sees only the candidate list cannot tell
	// that adopting one of those readers buys a folder whose documents live elsewhere.
	if targets := nonCandidateTargets(rep.Candidates, rep.Summary.ReadEdges); len(targets) > 0 {
		fmt.Fprintf(w, "not a candidate: %d\n", len(targets))
		for _, dir := range targets {
			fmt.Fprintf(w, "  %-40s read by: %s\n", dir, strings.Join(readersOf(rep.Summary.ReadEdges, dir), ", "))
		}
	}
	fmt.Fprintf(w, "summary: accepted=%d refused=%d", rep.Summary.Accepted, rep.Summary.Refused)
	if len(rep.Summary.OverlapConflicts) > 0 {
		fmt.Fprintf(w, " overlap-conflicts=%d", len(rep.Summary.OverlapConflicts))
	}
	if len(rep.Summary.UnsupportedConstructs) > 0 {
		fmt.Fprintf(w, " unsupported=[%s]", strings.Join(rep.Summary.UnsupportedConstructs, ", "))
	}
	fmt.Fprintln(w)
}
