// SPDX-License-Identifier: Apache-2.0

package git

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
)

// This file implements render-root scoping's read half: a GitTarget whose spec.path is a
// kustomize overlay reads a base OUTSIDE its subtree (the base/ + overlays/{env} shape
// reached via `../../base`). The operator must READ that base to render the overlay, while
// only ever WRITING inside spec.path.
//
// The mechanism is a re-root: the scan is anchored at renderBase — the lowest common
// ancestor of spec.path and every file the build reads outside it — so the store, the
// attribution, and the render oracle all run in one coordinate system with no `..`-escaping
// paths, exactly as a self-contained render root already does. writeSubdir is spec.path
// expressed relative to renderBase; it is the write jail the flush enforces. When the subtree
// reads no out-of-scope file, renderBase == spec.path and writeSubdir == "", so the scan is
// byte-identical to the plain subtree scan and nothing downstream changes.
//
// Read scope is the EXACT reachable file set of the resources/patches graph, resolved by
// following it transitively — a referenced file or base kustomization, never a whole sibling
// directory. Collecting whole directories would pull in unrelated content a base does not
// reference (rejecting a buildable folder) and re-scan overlay-local files when a base is an
// ancestor of spec.path (a spurious duplicate). See
// docs/design/support-boundary/render-root-scoping.md §4 ("Read scope grows; write scope
// does not").

// renderScopeResult is the outcome of resolving a GitTarget subtree's read scope.
type renderScopeResult struct {
	// scan is the structural view the store is built from, keyed relative to renderBase.
	scan manifestanalyzer.FolderScan
	// renderBase is the scan anchor, slash-relative to the worktree root (equal to base
	// when no out-of-scope file is read).
	renderBase string
	// writeSubdir is spec.path relative to renderBase — the write jail. Empty when
	// renderBase == spec.path.
	writeSubdir string
}

// scanRenderScope resolves the read scope of the GitTarget subtree at base (slash-relative
// to the worktree root) and returns the store's structural view re-rooted at renderBase.
//
// It first scans spec.path exactly as the plain writer does, then resolves every file the
// subtree's kustomizations read from OUTSIDE spec.path — following the resources/patches
// graph transitively, refusing a reference that escapes the repository root — and re-keys the
// whole set relative to their common ancestor. A subtree that reads no out-of-scope file
// returns the plain scan unchanged.
func scanRenderScope(root, base string) (renderScopeResult, error) {
	absBase := filepath.Join(root, filepath.FromSlash(base))
	specScan, err := scanWorktreeSubtree(absBase)
	if err != nil {
		return renderScopeResult{}, err
	}

	readFiles, err := resolveReadScope(root, base, specScan.YAMLFiles)
	if err != nil {
		return renderScopeResult{}, err
	}
	if len(readFiles) == 0 {
		return renderScopeResult{scan: specScan, renderBase: base, writeSubdir: ""}, nil
	}

	// renderBase is the lowest ancestor of spec.path and every out-of-scope file the build
	// reads, so the whole scan re-keys under it with no `..`-escaping paths.
	renderBase := commonAncestor(append([]string{base}, dirsOf(readFiles)...))
	writeSubdir := relUnder(renderBase, base)

	scan := rekeyScan(specScan, writeSubdir)
	seen := map[string]struct{}{}
	for _, f := range scan.YAMLFiles {
		seen[f.Path] = struct{}{}
	}
	for _, wf := range readFiles {
		key := relUnder(renderBase, wf)
		if _, dup := seen[key]; dup {
			continue // already present from the spec.path scan (e.g. a base that is an ancestor)
		}
		content, ok := readFileBytes(root, wf)
		if !ok {
			continue // a vanished/unreadable referenced file: the build refusal reports it, not us
		}
		seen[key] = struct{}{}
		scan.YAMLFiles = append(scan.YAMLFiles, manifestedit.FileContent{Path: key, Content: content})
	}
	sort.Slice(scan.YAMLFiles, func(i, j int) bool { return scan.YAMLFiles[i].Path < scan.YAMLFiles[j].Path })

	return renderScopeResult{scan: scan, renderBase: renderBase, writeSubdir: writeSubdir}, nil
}

// resolveReadScope returns the sorted, distinct set of files (slash, worktree-relative) the
// subtree's kustomizations read from OUTSIDE spec.path — the exact files kustomize loads, not
// whole directories. It follows the resources/patches graph transitively: a referenced file
// is added directly, a directory base contributes its kustomization file and, recursively,
// that kustomization's own reachable files. A reference that escapes the repository root is
// refused — the operator never reads outside the repository.
func resolveReadScope(root, base string, specFiles []manifestedit.FileContent) ([]string, error) {
	kustContent := map[string][]byte{} // worktree-relative dir -> kustomization bytes
	for _, f := range specFiles {
		if isKustomizationFileName(f.Path) {
			kustContent[cleanSlash(path.Join(base, path.Dir(f.Path)))] = f.Content
		}
	}

	readSet := map[string]struct{}{}
	visited := map[string]struct{}{}
	queue := sortedKeys(kustContent)

	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if _, seen := visited[dir]; seen {
			continue
		}
		visited[dir] = struct{}{}

		content, ok := kustContentOrDisk(root, dir, kustContent)
		if !ok {
			continue // a referenced directory with no readable kustomization: nothing to follow
		}
		// An out-of-scope base's own kustomization file(s) are a build input the render FS
		// needs. Import EVERY recognized kustomization file the directory holds, not just the
		// first: real kustomize refuses a directory with more than one, so carrying them all
		// lets the render reach the same refusal instead of masking the conflict.
		if !pathWithin(dir, base) {
			for _, kf := range kustomizationFiles(root, dir) {
				readSet[kf] = struct{}{}
			}
		}
		found, err := reachableTargets(root, base, dir, content)
		if err != nil {
			return nil, err
		}
		queue = append(queue, found.dirs...)
		for _, t := range found.files {
			readSet[t] = struct{}{}
		}
	}

	return sortedKeys(readSet), nil
}

// reachableSplit is the two kinds of thing one kustomization's graph reaches out of scope:
// directory bases to follow, and files to read.
type reachableSplit struct {
	dirs  []string
	files []string
}

// reachableTargets classifies one kustomization's resources + patch references. A directory
// target is a base to follow (its own files come from recursing into it); a file target
// outside spec.path is read directly. In-scope targets are already covered by the spec.path
// scan, and remote entries name no local file. A reference climbing above the repository root
// is an error.
func reachableTargets(root, base, dir string, content []byte) (reachableSplit, error) {
	resources, patches, ok := manifestanalyzer.KustomizationBuildRefs(content)
	if !ok {
		return reachableSplit{}, nil // unparseable: the acceptance gate refuses it, not us
	}
	var out reachableSplit
	entries := make([]string, 0, len(resources)+len(patches))
	entries = append(entries, resources...)
	entries = append(entries, patches...)
	for _, entry := range entries {
		if manifestanalyzer.IsRemoteBaseEntry(entry) {
			continue
		}
		target := cleanSlash(path.Join(dir, entry))
		if escapesRoot(target) {
			return reachableSplit{}, fmt.Errorf(
				"kustomization in %q references %q which escapes the repository root; refusing to read outside it",
				dir, entry)
		}
		switch {
		case isDir(root, target):
			out.dirs = append(out.dirs, target) // a directory base: follow its own graph
		case !pathWithin(target, base):
			out.files = append(out.files, target) // an out-of-scope file the build loads
		}
	}
	return out, nil
}

// rekeyScan lifts a spec.path-relative scan into render coordinates by prefixing every
// managed path with writeSubdir. The ignore matcher is deliberately left spec.path-relative
// — it is a matcher, not a path, and the flush translates a planned write back to spec.path
// coordinates before consulting it.
func rekeyScan(scan manifestanalyzer.FolderScan, writeSubdir string) manifestanalyzer.FolderScan {
	if writeSubdir == "" {
		return scan
	}
	out := scan
	out.YAMLFiles = make([]manifestedit.FileContent, len(scan.YAMLFiles))
	for i, f := range scan.YAMLFiles {
		out.YAMLFiles[i] = manifestedit.FileContent{
			Path:    cleanSlash(path.Join(writeSubdir, f.Path)),
			Content: f.Content,
		}
	}
	out.NonYAML = make([]string, len(scan.NonYAML))
	for i, p := range scan.NonYAML {
		out.NonYAML[i] = cleanSlash(path.Join(writeSubdir, p))
	}
	out.Foreign = make([]manifestanalyzer.ForeignEntry, len(scan.Foreign))
	for i, fe := range scan.Foreign {
		fe.Path = cleanSlash(path.Join(writeSubdir, fe.Path))
		out.Foreign[i] = fe
	}
	return out
}

// kustContentOrDisk returns a directory's kustomization bytes, reading from disk (and caching)
// when the directory is not one the spec.path scan already loaded.
func kustContentOrDisk(root, dir string, cache map[string][]byte) ([]byte, bool) {
	if content, ok := cache[dir]; ok {
		return content, true
	}
	content, ok := readKustomization(root, dir)
	if ok {
		cache[dir] = content
	}
	return content, ok
}

// kustomizationFiles returns the worktree-relative path of every recognized kustomization
// file (kustomization.yaml and/or kustomization.yml) a directory holds as a REGULAR file — a
// symlink is skipped, so a base reference can never leave the tree through one. Both are
// returned when both exist: real kustomize refuses that directory, and importing both lets the
// render reach the same verdict rather than masking it.
func kustomizationFiles(root, dir string) []string {
	var out []string
	for _, name := range []string{"kustomization.yaml", "kustomization.yml"} {
		rel := cleanSlash(path.Join(dir, name))
		if info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel))); err == nil && info.Mode().IsRegular() {
			out = append(out, rel)
		}
	}
	return out
}

// readKustomization reads a directory's kustomization file from disk, for following an
// out-of-scope base's own `../` references. It goes through the same guarded reader as every
// other referenced file (readFileBytes: Lstat + regular-file), so a symlinked kustomization is
// never followed outside the worktree. ok is false when the directory holds no readable
// regular kustomization.
func readKustomization(root, dir string) ([]byte, bool) {
	for _, name := range []string{"kustomization.yaml", "kustomization.yml"} {
		if content, ok := readFileBytes(root, cleanSlash(path.Join(dir, name))); ok {
			return content, true
		}
	}
	return nil, false
}

// readFileBytes reads a worktree-relative file from disk, never following a symlink out of the
// tree (Lstat guards the type). ok is false when the path is missing, a symlink, or a
// directory.
func readFileBytes(root, rel string) ([]byte, bool) {
	full := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Lstat(full)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	content, err := os.ReadFile(full)
	if err != nil {
		return nil, false
	}
	return content, true
}

// isDir reports whether a worktree-relative slash path is a directory. A symlink is never
// treated as a directory (Lstat, not Stat), so a base reference can never leave the tree
// through one.
func isDir(root, rel string) bool {
	info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil && info.IsDir()
}

// isKustomizationFileName reports whether a slash path's base name is a kustomization file.
func isKustomizationFileName(p string) bool {
	switch path.Base(p) {
	case "kustomization.yaml", "kustomization.yml":
		return true
	default:
		return false
	}
}

// dirsOf returns the containing directory of each slash file path.
func dirsOf(files []string) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = cleanSlash(path.Dir(f))
	}
	return out
}

// commonAncestor returns the lowest common directory (slash) of a set of slash paths,
// segment by segment. The inputs are all in-repo directories, so the result is at worst the
// repository root ("").
func commonAncestor(dirs []string) string {
	if len(dirs) == 0 {
		return ""
	}
	parts := strings.Split(cleanSlash(dirs[0]), "/")
	for _, d := range dirs[1:] {
		cur := strings.Split(cleanSlash(d), "/")
		n := 0
		for n < len(parts) && n < len(cur) && parts[n] == cur[n] {
			n++
		}
		parts = parts[:n]
	}
	return strings.Join(parts, "/")
}

// relUnder returns child expressed relative to ancestor, where ancestor is known to contain
// child. An empty ancestor (the repository root) returns child unchanged.
func relUnder(ancestor, child string) string {
	ancestor = cleanSlash(ancestor)
	child = cleanSlash(child)
	if ancestor == "" {
		return child
	}
	if ancestor == child {
		return ""
	}
	return strings.TrimPrefix(child, ancestor+"/")
}

// pathWithin reports whether the slash path p is within dir: equal to it, or nested under it
// on a segment boundary. An empty dir (the repository root) contains every path.
func pathWithin(p, dir string) bool {
	p = cleanSlash(p)
	dir = cleanSlash(dir)
	if dir == "" {
		return true
	}
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// escapesRoot reports whether a cleaned slash path climbs above the repository root.
func escapesRoot(p string) bool {
	return p == ".." || strings.HasPrefix(p, "../")
}

// sortedKeys returns the sorted keys of a set-like map, so the walk order is deterministic.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// cleanSlash cleans a slash path, mapping "." (the root) to "".
func cleanSlash(p string) string {
	c := path.Clean(p)
	if c == "." {
		return ""
	}
	return c
}
