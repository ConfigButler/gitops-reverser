// SPDX-License-Identifier: Apache-2.0

package git

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// A window's footprint is the set of Git files its writes may change. Two windows may be open side
// by side only while their footprints are proven not to overlap: otherwise the one that closes last
// could put an older value of a file over a newer one. See docs/design/concurrent-author-windows.md.
//
// A footprint is proven only where that is cheap and certain, and is unknown everywhere else; unknown
// overlaps everything, so those writes stay serial, one window at a time, as before. A write is
// proven when all of these hold, on the checkout the next commit is planned on:
//
//   - the GitTarget declares no placement.default and no useKustomize, and no placement.byType
//     template for the object's kind: a template can bundle several objects into one file;
//   - its folder reads nothing outside spec.path, feeds no configMapGenerator, and holds no
//     kustomization the writer does not model;
//   - the object's file is governed by no kustomization (none in its directory or above, none
//     naming it), so the write registers nothing in a kustomization.yaml and creates none;
//   - the object's document is alone in its file, or the object has none and the built-in path is
//     free; the built-in path has no API version, so two versions of one object share it;
//   - the folder's bootstrap files are in place, so the commit adds none;
//   - the write is a whole object (not a field patch) of a type that is not encrypted.
//
// A proof holds for one revision: the checkout, named by how many resets and own commits it has
// seen, and the GitTarget's generation. Before a write is admitted beside other windows, every open
// window is proven again against the current revision (FootprintReproved). And before a proven
// window's write is committed, every path the commit would change must be inside its footprint: a
// commit that changes more is a footprint bug, and is refused (footprintViolationError).

// footprint is the set of Git files a window's writes may change, as proven against one revision.
type footprint struct {
	// paths are the files, slash-separated and relative to the worktree root, sorted and distinct.
	paths []string
	// unknown says why the footprint could not be proven; empty when it is.
	unknown string
	// revision is what the footprint was proven against.
	revision footprintRevision
}

// footprintRevision is what a footprint is proven against: the checkout, named by how many resets
// and own commits the worker has made, and the generation of the window's GitTarget.
type footprintRevision struct {
	resets     uint64
	commits    uint64
	generation int64
}

// known reports whether the footprint is proven.
func (f footprint) known() bool { return f.unknown == "" }

// overlaps reports whether the two footprints may change one file: when either is unknown, or they
// share a path.
func (f footprint) overlaps(other footprint) bool {
	if !f.known() || !other.known() {
		return true
	}
	for _, p := range f.paths {
		if _, found := slices.BinarySearch(other.paths, p); found {
			return true
		}
	}
	return false
}

// union is every path of both, proven against f's revision; unknown when either is.
func (f footprint) union(other footprint) footprint {
	if !f.known() {
		return f
	}
	if !other.known() {
		return footprint{unknown: other.unknown, revision: f.revision}
	}
	paths := slices.Concat(f.paths, other.paths)
	slices.Sort(paths)
	return footprint{paths: slices.Compact(paths), revision: f.revision}
}

// outside returns the paths that are not in the footprint.
func (f footprint) outside(paths []string) []string {
	var out []string
	for _, p := range paths {
		if _, found := slices.BinarySearch(f.paths, p); !found {
			out = append(out, p)
		}
	}
	return out
}

// String is the footprint for a log line.
func (f footprint) String() string {
	if !f.known() {
		return "unknown: " + f.unknown
	}
	return "[" + strings.Join(f.paths, " ") + "]"
}

// String is the revision for a log line.
func (r footprintRevision) String() string {
	return fmt.Sprintf("resets=%d commits=%d generation=%d", r.resets, r.commits, r.generation)
}

// footprintString is a window's footprint for a log line: "unproven" for a window that never had
// company, and so never needed one.
func footprintString(f *footprint) string {
	if f == nil {
		return "unproven"
	}
	return f.String()
}

// footprintIndex is what proving a GitTarget's writes needs to know about its folder at one revision.
// The loop keeps one per GitTarget, and builds it again once the revision moves.
type footprintIndex struct {
	revision footprintRevision
	// unknown says why no write of the GitTarget can be proven at this revision; empty when they can.
	unknown string
	// base is the GitTarget's folder, slash-relative to the worktree root.
	base   string
	root   string
	store  *manifestanalyzer.ManifestStore
	docLoc map[*manifestanalyzer.DocumentModel]manifestanalyzer.RecordRef
	policy *manifestanalyzer.PlacementPolicy
	// content is every scanned YAML file's bytes, by its path relative to base.
	content map[string][]byte
	writer  eventContentWriter
}

// currentRevision is the revision a footprint of the GitTarget is proven against now, and whether
// it could be read.
func (l *branchWorkerEventLoop) currentRevision(name, namespace string) (footprintRevision, bool) {
	target, err := l.w.getGitTarget(l.w.ctx, name, namespace)
	if err != nil {
		return footprintRevision{}, false
	}
	return footprintRevision{
		resets:     l.w.checkoutResets.Load(),
		commits:    l.w.checkoutCommits.Load(),
		generation: target.Generation,
	}, true
}

// proveEvents proves the footprint of events, all of one GitTarget, against the current revision, for
// a write the loop admits now: the next commit is planned on the checkout once it holds every decided
// write.
func (l *branchWorkerEventLoop) proveEvents(name, namespace string, events []Event) footprint {
	return l.proveOn(name, namespace, events, l.checkoutCurrent() && !l.w.worktreeDirty())
}

// proveOn proves the footprint of events against the current revision. planned says whether the
// checkout is the tree their commit is planned on; a proof of any other tree is unknown.
func (l *branchWorkerEventLoop) proveOn(name, namespace string, events []Event, planned bool) footprint {
	rev, ok := l.currentRevision(name, namespace)
	if !ok {
		return footprint{unknown: "the GitTarget cannot be read"}
	}
	if !planned {
		return footprint{unknown: "the checkout does not hold every decided write yet", revision: rev}
	}
	ix := l.footprintIndexFor(itypes.NewResourceReference(name, namespace), rev, events)
	f := footprint{revision: rev}
	if ix.unknown != "" {
		f.unknown = ix.unknown
		return f
	}
	for i := range events {
		file, why := ix.fileFor(events[i])
		if why != "" {
			return footprint{unknown: why, revision: rev}
		}
		if file != "" {
			f = f.union(footprint{paths: []string{file}, revision: rev})
		}
	}
	return f
}

// proveWindow proves the footprint of every write window holds, against the current revision.
func (l *branchWorkerEventLoop) proveWindow(window *openWindow) footprint {
	return l.proveEvents(window.GitTarget, window.GitTargetNamespace, window.orderedEvents())
}

// footprintIndexFor returns the GitTarget's index at rev, building it when the one kept is of another
// revision.
func (l *branchWorkerEventLoop) footprintIndexFor(
	target itypes.ResourceReference, rev footprintRevision, events []Event,
) *footprintIndex {
	if ix := l.footprints[target]; ix != nil && ix.revision == rev {
		return ix
	}
	ix := l.buildFootprintIndex(target, rev, events)
	if l.footprints == nil {
		l.footprints = map[itypes.ResourceReference]*footprintIndex{}
	}
	l.footprints[target] = ix
	return ix
}

// buildFootprintIndex scans the GitTarget's folder as the write plan does, and records what decides
// whether a write there can be proven.
func (l *branchWorkerEventLoop) buildFootprintIndex(
	target itypes.ResourceReference, rev footprintRevision, events []Event,
) *footprintIndex {
	ix := &footprintIndex{revision: rev, writer: l.w.contentWriter}
	md, err := l.w.resolveTargetMetadata(l.w.ctx, target.Name, target.Namespace)
	if err != nil {
		ix.unknown = "the GitTarget cannot be resolved"
		return ix
	}
	ix.policy = md.Placement
	if why := wholeTargetUnproven(md.Placement); why != "" {
		ix.unknown = why
		return ix
	}
	ix.root, ix.base = l.w.repoPath(), sanitizePath(md.Path)
	if _, err := os.Stat(filepath.Join(ix.root, ".git")); err != nil {
		ix.unknown = "the repository is not checked out"
		return ix
	}
	if missing := missingBootstrapFile(ix.root, ix.base, md.BootstrapOptions); missing != "" {
		ix.unknown = "the folder's bootstrap file " + missing + " is not in Git yet"
		return ix
	}

	l.w.repoMu.Lock()
	scoped, err := scanRenderScope(ix.root, ix.base)
	l.w.repoMu.Unlock()
	switch {
	case err != nil:
		ix.unknown = "the folder cannot be scanned"
		return ix
	case scoped.writeSubdir != "":
		ix.unknown = "the folder reads files outside spec.path"
		return ix
	case len(scoped.scan.InputFiles) > 0:
		ix.unknown = "the folder feeds a configMapGenerator"
		return ix
	}
	mapper := l.w.mapperForCluster(clusterIDForEvents(events))
	ix.store = manifestanalyzer.BuildStoreFromScan(l.w.ctx, scoped.scan, mapper, manifestanalyzer.WriterAllowlist(),
		manifestanalyzer.WithDeclaredNamespace(md.Namespaces.declaredNamespace()))
	for _, k := range ix.store.Kustomizations {
		if k.Unsupported {
			ix.unknown = "the folder holds a kustomization the writer does not model"
			return ix
		}
	}
	ix.docLoc = ix.store.DocumentLocations()
	ix.content = make(map[string][]byte, len(scoped.scan.YAMLFiles))
	for _, f := range scoped.scan.YAMLFiles {
		ix.content[f.Path] = f.Content
	}
	return ix
}

// wholeTargetUnproven says why no write of a GitTarget with this placement can be proven, or "".
func wholeTargetUnproven(policy *manifestanalyzer.PlacementPolicy) string {
	switch {
	case policy == nil:
		return ""
	case policy.UseKustomize:
		return "placement.useKustomize is on"
	case policy.Default != "":
		return "placement.default is set"
	default:
		return ""
	}
}

// missingBootstrapFile names a bootstrap file the next commit to the folder would add, or "". It reads
// the commit HEAD names, not the disk: a write that committed nothing leaves its bootstrap files
// staged, and the next commit adds them.
func missingBootstrapFile(root, base string, options pathBootstrapOptions) string {
	if !options.Enabled {
		return ""
	}
	entries, err := bootstrapTemplateFS.ReadDir(bootstrapTemplateDir)
	if err != nil {
		return bootstrapTemplateDir
	}
	tree, err := headTree(root)
	if err != nil {
		return "(HEAD cannot be read)"
	}
	for _, entry := range entries {
		if shouldSkipBootstrapEntry(entry, options) {
			continue
		}
		if _, err := tree.File(path.Join(base, entry.Name())); err != nil {
			return entry.Name()
		}
	}
	return ""
}

// headTree is the tree of the commit HEAD names in the repository at root.
func headTree(root string) (*object.Tree, error) {
	repo, err := gogit.PlainOpen(root)
	if err != nil {
		return nil, err
	}
	head, err := repo.Head()
	if err != nil {
		return nil, err
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil, err
	}
	return commit.Tree()
}

// fileFor is the one file, slash-relative to the worktree root, a write of the event may change: ""
// when it changes none, with why it cannot be proven when it cannot.
func (ix *footprintIndex) fileFor(event Event) (string, string) {
	switch {
	case event.IsFieldPatch():
		return "", "a field patch"
	case ix.writer.isSensitiveIdentifier(event.Identifier):
		return "", "an encrypted type"
	case event.Operation == string(itypes.OperationDelete):
		return ix.deleteFile(event)
	default:
		return ix.upsertFile(event)
	}
}

// upsertFile is fileFor for a create or an update: the file holding the object's document, or the
// built-in path for an object with none.
func (ix *footprintIndex) upsertFile(event Event) (string, string) {
	id, ok := manifestIdentity(event.Object)
	if !ok {
		return "", "an object without a manifest identity"
	}
	if dm := ix.store.ByManifestIdentity[id]; dm != nil {
		return ix.aloneIn(ix.docLoc[dm].FilePath)
	}
	if len(ix.store.GeneratedObjectsFor(event.Object)) > 0 {
		return "", "an object a configMapGenerator renders"
	}
	kind := event.Object.GetKind()
	if ix.policy != nil && ix.policy.ByType[kind] != "" {
		return "", "a placement.byType template for " + kind
	}
	placement, err := manifestanalyzer.LocateNew(ix.store, ix.policy, manifestanalyzer.PlacementRequest{
		Identifier: event.Identifier,
		Kind:       kind,
		Labels:     event.Object.GetLabels(),
	})
	switch {
	case err != nil:
		return "", "a placement that cannot be resolved"
	case placement.Source != manifestanalyzer.PlacementSourceBuiltin || placement.Append ||
		placement.Kustomization != nil:
		return "", "a new document placed beside others"
	case ix.governed(placement.Path):
		return "", "a new document under a kustomization"
	}
	destination := filepath.Join(ix.root, filepath.FromSlash(ix.base), filepath.FromSlash(placement.Path))
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		return "", "a new document at a path that is taken"
	}
	return ix.repoPath(placement.Path), ""
}

// deleteFile is fileFor for a delete: the file holding the object's document, or none when Git holds
// no document of it.
func (ix *footprintIndex) deleteFile(event Event) (string, string) {
	if id, ok := manifestIdentity(event.Object); ok {
		if dm := ix.store.ByManifestIdentity[id]; dm != nil {
			return ix.aloneIn(ix.docLoc[dm].FilePath)
		}
	}
	if action, found := manifestanalyzer.PlanDelete(ix.store, event.Identifier); found {
		return ix.aloneIn(action.Ref.FilePath)
	}
	if len(ix.store.Kustomizations) > 0 {
		// A delete of an object no document holds may still remove a generator's entry.
		return "", "a delete of an object no document holds, in a folder with a kustomization"
	}
	return "", ""
}

// aloneIn is file, which holds the object's document, when that document is alone in it and no
// kustomization governs it.
func (ix *footprintIndex) aloneIn(file string) (string, string) {
	if ix.governed(file) {
		return "", "a document under a kustomization"
	}
	inventory, diags := manifestedit.IndexFile(file, ix.content[file])
	if len(inventory.Records) != 1 {
		return "", "a document in a file with others"
	}
	for _, d := range diags {
		if d.Reason == manifestedit.ReasonNotKRM || d.Reason == manifestedit.ReasonInvalidYAML {
			return "", "a document in a file with others"
		}
	}
	return ix.repoPath(file), ""
}

// governed reports whether a kustomization governs file, relative to base: one in its directory or
// above, or one naming it, or a directory above it, among its resources or patches.
func (ix *footprintIndex) governed(file string) bool {
	within := func(dir string) bool { return dir == "." || dir == "" || strings.HasPrefix(file, dir+"/") }
	for dir, k := range ix.store.Kustomizations {
		if within(dir) || slices.Contains(k.Patches, file) {
			return true
		}
		for _, entry := range k.Resources {
			if named := path.Clean(path.Join(path.Dir(k.Path), entry)); named == file || within(named) {
				return true
			}
		}
	}
	return false
}

// repoPath is file, relative to base, relative to the worktree root.
func (ix *footprintIndex) repoPath(file string) string {
	if ix.base == "" {
		return file
	}
	return ix.base + "/" + file
}

// footprintViolationError refuses a commit that would change files outside its window's footprint.
type footprintViolationError struct {
	outside   []string
	footprint footprint
}

func (e *footprintViolationError) Error() string {
	return fmt.Sprintf("the commit would change %s, outside its window's footprint %s",
		strings.Join(e.outside, ", "), e.footprint)
}

// proveBeforeCommit proves the footprint of the decided write at i again on the tree its first commit
// is planned on, once the checkout holds every write before it: the windows a write's admission
// closed commit before it, and can move what it changes (a delete committed first turns its update
// into a create). The check before the commit is then of a proof of that very tree.
func (l *branchWorkerEventLoop) proveBeforeCommit(i int) {
	pendingWrite := &l.pendingWrites[i]
	if pendingWrite.footprint == nil || pendingWrite.committedOnce || len(pendingWrite.Events) == 0 {
		return
	}
	name, namespace := pendingWrite.windowTarget()
	planned := l.w.checkoutHolds(i) && !l.w.rootParentStale() && !l.w.worktreeDirty()
	proven := l.proveOn(name, namespace, pendingWrite.Events, planned)
	pendingWrite.footprint = &proven
}

// checkFootprint refuses the commit about to be made from the staged changes when it changes a file
// outside the write's footprint. It checks a write's first commit only, against a footprint proven
// on the tree the commit is planned on (proveBeforeCommit). A proof of another tree, after a reset
// or a change of the GitTarget, says nothing about this one, and the commit is not checked
// (FootprintCheckSkipped). It reports whether it checked the commit: the caller logs
// FootprintChecked once the commit is made.
func (w *BranchWorker) checkFootprint(
	worktree *gogit.Worktree, pendingWrite *PendingWrite, first bool,
) (bool, error) {
	f := pendingWrite.footprint
	if f == nil || !f.known() || !first {
		return false, nil
	}
	if !w.provenOnCheckout(pendingWrite.Target(), f.revision) {
		w.Log.V(1).Info("Not checking a commit against its window's footprint: the proof is of another tree",
			"branch", w.Branch, "footprint", f.String(), "revision", f.revision.String(),
			"decision", decisionFootprintCheckSkipped)
		return false, nil
	}
	changed, err := stagedPaths(worktree)
	if err != nil {
		return false, fmt.Errorf("read the staged changes: %w", err)
	}
	if outside := f.outside(changed); len(outside) > 0 {
		return false, &footprintViolationError{outside: outside, footprint: *f}
	}
	return true, nil
}

// logFootprintChecked logs the commit checkFootprint checked, once it is made. The tree and its
// parent's tree name the change that was checked across a rebuild of the commit onto the same
// parent, which makes a new commit of it and checks it no more: its actions were decided already.
// The tree alone is not enough: a later commit can return to the same tree from another parent, by
// a change nobody checked.
func (w *BranchWorker) logFootprintChecked(
	repo *gogit.Repository, hash plumbing.Hash, pendingWrite *PendingWrite, checked bool,
) {
	if !checked {
		return
	}
	var tree, parentTree plumbing.Hash
	if commit, err := repo.CommitObject(hash); err == nil {
		tree = commit.TreeHash
		if parent, err := commit.Parent(0); err == nil {
			parentTree = parent.TreeHash
		}
	}
	w.Log.Info("Checked a commit against its window's footprint", "branch", w.Branch,
		"commit", hash.String(), "tree", tree.String(), "parentTree", parentTree.String(),
		"footprint", pendingWrite.footprint.String(), "decision", decisionFootprintChecked)
}

// provenOnCheckout reports whether rev is the revision the checkout is at now.
func (w *BranchWorker) provenOnCheckout(target ResolvedTargetMetadata, rev footprintRevision) bool {
	current, err := w.getGitTarget(w.ctx, target.Name, target.Namespace)
	return err == nil && current.Generation == rev.generation && w.checkoutResets.Load() == rev.resets &&
		w.checkoutCommits.Load() == rev.commits
}

// stagedPaths is every path the next commit changes: the index against HEAD, sorted.
func stagedPaths(worktree *gogit.Worktree) ([]string, error) {
	status, err := worktree.Status()
	if err != nil {
		return nil, err
	}
	var paths []string
	for p, s := range status {
		if s.Staging != gogit.Unmodified && s.Staging != gogit.Untracked {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	return paths, nil
}
