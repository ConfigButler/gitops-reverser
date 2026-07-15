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
// ancestor of spec.path and every base it reaches — so the store, the attribution, and the
// render oracle all run in one coordinate system with no `..`-escaping paths, exactly as a
// self-contained render root already does. writeSubdir is spec.path expressed relative to
// renderBase; it is the write jail the flush enforces. When the subtree reads no
// out-of-scope base, renderBase == spec.path and writeSubdir == "", so the scan is
// byte-identical to the plain subtree scan and nothing downstream changes.
//
// See docs/design/support-boundary/render-root-scoping.md §4 ("Read scope grows; write
// scope does not").

// renderScopeResult is the outcome of resolving a GitTarget subtree's read scope.
type renderScopeResult struct {
	// scan is the structural view the store is built from, keyed relative to renderBase.
	scan manifestanalyzer.FolderScan
	// renderBase is the scan anchor, slash-relative to the worktree root (equal to base
	// when no out-of-scope base is read).
	renderBase string
	// writeSubdir is spec.path relative to renderBase — the write jail. Empty when
	// renderBase == spec.path.
	writeSubdir string
}

// scanRenderScope resolves the read scope of the GitTarget subtree at base (slash-relative
// to the worktree root) and returns the store's structural view re-rooted at renderBase.
//
// It first scans spec.path exactly as the plain writer does, then follows every kustomize
// `../` base reference that escapes spec.path — transitively, and refusing a reference that
// escapes the repository root — pulls those bases in as read-only render context, and
// re-keys the whole set relative to their common ancestor. A subtree that reads no
// out-of-scope base returns the plain scan unchanged.
func scanRenderScope(root, base string) (renderScopeResult, error) {
	absBase := filepath.Join(root, filepath.FromSlash(base))
	specScan, err := scanWorktreeSubtree(absBase)
	if err != nil {
		return renderScopeResult{}, err
	}

	bases, err := resolveOutOfScopeBases(root, base, specScan.YAMLFiles)
	if err != nil {
		return renderScopeResult{}, err
	}
	if len(bases) == 0 {
		return renderScopeResult{scan: specScan, renderBase: base, writeSubdir: ""}, nil
	}

	renderBase := commonAncestor(append([]string{base}, bases...))
	writeSubdir := relUnder(renderBase, base)

	scan := rekeyScan(specScan, writeSubdir)
	for _, dir := range bases {
		files, walkErr := walkReadOnlyBase(root, dir, renderBase)
		if walkErr != nil {
			return renderScopeResult{}, walkErr
		}
		scan.YAMLFiles = append(scan.YAMLFiles, files...)
	}
	sort.Slice(scan.YAMLFiles, func(i, j int) bool { return scan.YAMLFiles[i].Path < scan.YAMLFiles[j].Path })

	return renderScopeResult{scan: scan, renderBase: renderBase, writeSubdir: writeSubdir}, nil
}

// resolveOutOfScopeBases returns the sorted, distinct set of base directories (slash,
// worktree-relative) a subtree's kustomizations reach OUTSIDE spec.path, following the
// resources graph transitively. It reads the kustomization of each out-of-scope base from
// disk to follow its own `../` references. A reference that escapes the repository root is
// refused — the operator never reads outside the repository.
func resolveOutOfScopeBases(root, base string, specFiles []manifestedit.FileContent) ([]string, error) {
	kustContent := map[string][]byte{} // worktree-relative dir -> kustomization bytes
	for _, f := range specFiles {
		if isKustomizationFileName(f.Path) {
			kustContent[cleanSlash(path.Join(base, path.Dir(f.Path)))] = f.Content
		}
	}

	outOfScope := map[string]struct{}{}
	visited := map[string]struct{}{}
	queue := make([]string, 0, len(kustContent))
	for dir := range kustContent {
		queue = append(queue, dir)
	}
	sort.Strings(queue)

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
		targets, err := outOfScopeTargets(root, base, dir, content)
		if err != nil {
			return nil, err
		}
		for _, target := range targets {
			outOfScope[target] = struct{}{}
			queue = append(queue, target)
		}
	}

	out := make([]string, 0, len(outOfScope))
	for dir := range outOfScope {
		out = append(out, dir)
	}
	out = minimalDirs(out)
	sort.Strings(out)
	return out, nil
}

// outOfScopeTargets returns the base directories one kustomization reaches outside spec.path.
// An unparseable kustomization contributes none (the acceptance gate refuses it); a remote
// base is skipped (refused before any build); an out-of-scope raw file is left to the base
// walk. A reference climbing above the repository root is an error.
func outOfScopeTargets(root, base, dir string, content []byte) ([]string, error) {
	entries, ok := manifestanalyzer.KustomizationResourceEntries(content)
	if !ok {
		return nil, nil
	}
	var out []string
	for _, entry := range entries {
		if manifestanalyzer.IsRemoteBaseEntry(entry) {
			continue
		}
		target := cleanSlash(path.Join(dir, entry))
		if escapesRoot(target) {
			return nil, fmt.Errorf(
				"kustomization in %q references %q which escapes the repository root; refusing to read outside it",
				dir, entry)
		}
		if pathWithin(target, base) || !isDir(root, target) {
			continue
		}
		out = append(out, target)
	}
	return out, nil
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

// walkReadOnlyBase walks an out-of-scope base directory (slash, worktree-relative) and
// returns its managed YAML files, keyed relative to renderBase. Only YAML is collected —
// the base is read-only render context, never materialised as a foreign-content refusal —
// and symlinks are never followed. The same ClassifyEntry policy the writer's subtree scan
// uses decides what counts as managed YAML, so the two agree on kustomization and resource
// files.
func walkReadOnlyBase(root, dir, renderBase string) ([]manifestedit.FileContent, error) {
	absDir := filepath.Join(root, filepath.FromSlash(dir))
	var files []manifestedit.FileContent
	walkErr := filepath.WalkDir(absDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == absDir {
			return nil
		}
		rel, relErr := filepath.Rel(absDir, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		switch manifestanalyzer.ClassifyEntry(rel, d, nil) {
		case manifestanalyzer.RoleSkipDir:
			return filepath.SkipDir
		case manifestanalyzer.RoleManagedYAML:
			//nolint:gosec // reading a referenced base as render context is the feature
			content, readErr := os.ReadFile(p)
			if readErr != nil {
				return readErr
			}
			key := cleanSlash(path.Join(relUnder(renderBase, dir), rel))
			files = append(files, manifestedit.FileContent{Path: key, Content: content})
		case manifestanalyzer.RoleOperatorArtifact, manifestanalyzer.RoleForeignFile,
			manifestanalyzer.RoleForeignSymlink, manifestanalyzer.RoleIgnored, manifestanalyzer.RoleDescend:
			// Non-YAML, foreign, ignored, or a plain directory to descend: a base contributes
			// only its manifests to the render, so everything else is skipped.
		}
		return nil
	})
	if walkErr != nil && !os.IsNotExist(walkErr) {
		return nil, walkErr
	}
	return files, nil
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

// readKustomization reads the kustomization.yaml (or .yml) of a worktree-relative directory
// from disk, for following an out-of-scope base's own `../` references. ok is false when the
// directory holds no readable kustomization.
func readKustomization(root, dir string) ([]byte, bool) {
	for _, name := range []string{"kustomization.yaml", "kustomization.yml"} {
		p := filepath.Join(root, filepath.FromSlash(dir), name)
		if content, err := os.ReadFile(p); err == nil {
			return content, true
		}
	}
	return nil, false
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

// minimalDirs drops any directory nested under another in the set, leaving only the
// top-level roots — so a base and its own nested base are walked once, through the parent.
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

// cleanSlash cleans a slash path, mapping "." (the root) to "".
func cleanSlash(p string) string {
	c := path.Clean(p)
	if c == "." {
		return ""
	}
	return c
}
