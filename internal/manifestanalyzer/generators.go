// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"path"
	"sort"
	"strings"

	kustypes "sigs.k8s.io/kustomize/api/types"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// This file models the one generator the operator writes back through: a configMapGenerator
// whose name hash is disabled. Its input files are the configuration people maintain, so a live
// ConfigMap edit lands in the file (files:) or on the line (envs:) the value came from, and the
// render oracle proves the write by building the folder again.
//
// A generator with the name hash is refused, and that is not about finding the source. The
// re-render names the object differently, so the object somebody edited is replaced by another
// one on the next apply, and "the render reproduces the edited object" is false by construction.

// featureGeneratorNameHash refuses a configMapGenerator entry whose name carries the content
// hash. The author can disable the hash; see unsupportedKustomizeDetail for the action.
const featureGeneratorNameHash = "configMapGenerator-name-hash"

// featureGeneratorBehavior refuses a configMapGenerator entry with behavior: merge or replace.
// Such an entry rewrites a ConfigMap a base generates, so the object is shared with the base and
// no single file owns it.
const featureGeneratorBehavior = "configMapGenerator-behavior"

// ConfigMapGeneratorEntry is one configMapGenerator entry of a kustomization, with every input
// path resolved against the scan root.
type ConfigMapGeneratorEntry struct {
	// KustomizationPath is the kustomization file declaring the entry (slash).
	KustomizationPath string
	// Name and Namespace are the entry's own fields. Namespace is usually empty: a namespace:
	// transformer supplies it.
	Name, Namespace string
	// Files are the files: sources, in listed order.
	Files []GeneratorFileSource
	// Envs are the envs: files, resolved, in listed order.
	Envs []string
	// Literals are the literals: entries, raw ("KEY=value").
	Literals []string
}

// GeneratorFileSource is one files: source: the data key and the file whose whole content is
// its value.
type GeneratorFileSource struct {
	Key  string
	Path string
}

// ID names the entry in its kustomization: by its name and namespace, which an edit of the file
// leaves as they are, where its position may move.
func (e ConfigMapGeneratorEntry) ID() manifestedit.GeneratorEntryID {
	return manifestedit.GeneratorEntryID{Name: e.Name, Namespace: e.Namespace}
}

// inputPaths lists every file the entry reads.
func (e ConfigMapGeneratorEntry) inputPaths() []string {
	out := make([]string, 0, len(e.Files)+len(e.Envs))
	for _, f := range e.Files {
		out = append(out, f.Path)
	}
	return append(out, e.Envs...)
}

// configMapGenerators converts kustomize's parsed configMapGenerator entries into the writer's
// model and names every feature that refuses the folder. An entry whose sources do not parse is
// left out of the model; kustomize fails the build on it, and that refusal names it.
func configMapGenerators(k kustypes.Kustomization, kustomizationPath string) ([]ConfigMapGeneratorEntry, []string) {
	if len(k.ConfigMapGenerator) == 0 {
		return nil, nil
	}
	dir := slashDir(kustomizationPath)
	globalHashOff := k.GeneratorOptions != nil && k.GeneratorOptions.DisableNameSuffixHash
	features := map[string]struct{}{}
	var out []ConfigMapGeneratorEntry
	for _, g := range k.ConfigMapGenerator {
		switch g.Behavior {
		case "", "create":
			// kustomize ORs the two (types.MergeGlobalOptionsIntoLocal): either one disables it.
			if !globalHashOff && (g.Options == nil || !g.Options.DisableNameSuffixHash) {
				features[featureGeneratorNameHash] = struct{}{}
			}
		default:
			// Refused whatever its hash setting, so the hash is not named beside it: disabling
			// it would not make the entry adoptable.
			features[featureGeneratorBehavior] = struct{}{}
		}
		entry, ok := generatorEntry(g, kustomizationPath, dir)
		if ok {
			out = append(out, entry)
		}
	}
	return out, sortedKeysOfSet(features)
}

// generatorEntry resolves one entry's sources. ok is false when a files: source does not parse
// the way kustomize parses it.
func generatorEntry(g kustypes.ConfigMapArgs, kustomizationPath, dir string) (ConfigMapGeneratorEntry, bool) {
	entry := ConfigMapGeneratorEntry{
		KustomizationPath: kustomizationPath,
		Name:              g.Name,
		Namespace:         g.Namespace,
		Literals:          append([]string(nil), g.LiteralSources...),
	}
	for _, source := range g.FileSources {
		key, file, ok := parseFileSource(source)
		if !ok {
			return ConfigMapGeneratorEntry{}, false
		}
		entry.Files = append(entry.Files, GeneratorFileSource{Key: key, Path: cleanJoin(dir, file)})
	}
	for _, env := range g.EnvSources {
		entry.Envs = append(entry.Envs, cleanJoin(dir, env))
	}
	return entry, true
}

// parseFileSource splits a files: source the way kustomize does (generators.ParseFileSource,
// which is internal): "path" keys the value by the file's base name, "key=path" names the key,
// and anything else is an error.
func parseFileSource(source string) (string, string, bool) {
	switch strings.Count(source, "=") {
	case 0:
		return path.Base(source), source, true
	case 1:
		key, file, _ := strings.Cut(source, "=")
		return key, file, key != "" && file != ""
	default:
		return "", "", false
	}
}

// KustomizationGeneratorInputs returns the local files a kustomization's configMapGenerator
// entries read, relative to its own directory. The live writer follows them alongside
// KustomizationBuildRefs, so a base outside spec.path is rendered with its generator inputs.
func KustomizationGeneratorInputs(content []byte) []string {
	var k kustypes.Kustomization
	if err := k.Unmarshal(content); err != nil {
		return nil
	}
	k.FixKustomization()
	var out []string
	for _, g := range k.ConfigMapGenerator {
		for _, source := range g.FileSources {
			if _, file, ok := parseFileSource(source); ok {
				out = append(out, file)
			}
		}
		out = append(out, g.EnvSources...)
	}
	return out
}

// AttachGeneratorInputs moves every file a configMapGenerator reads into scan.InputFiles: out of
// YAMLFiles, so a settings file is never indexed as a manifest or refused as non-KRM YAML, and
// out of Foreign, so a .env or .properties file is not refused as foreign content. read returns a
// scanned path's bytes; it is called only for files the walk did not already read.
//
// The inputs are named context, kept in the scan so the render can read them and the writer can
// edit them, and never modelled as documents. A file that a kustomization also lists under
// resources: stays a resource: kustomize renders it as an object, so it must be mirrored.
func AttachGeneratorInputs(scan *FolderScan, read func(path string) ([]byte, bool)) {
	kusts := parseKustomizations(scan.YAMLFiles)
	inputs := generatorInputSet(kusts)
	if len(inputs) == 0 {
		return
	}
	resources := reachedResourceFiles(kusts)
	a := inputAttacher{scan: scan, read: read, have: map[string]struct{}{}}
	a.isInput = func(p string) bool {
		p = filepathToSlash(p)
		_, input := inputs[p]
		_, resource := resources[p]
		return input && !resource
	}
	for _, f := range scan.InputFiles {
		a.have[filepathToSlash(f.Path)] = struct{}{}
	}
	a.fromYAML()
	a.fromForeign()
	for _, p := range scan.NonYAML {
		if a.isInput(p) {
			a.readInput(p)
		}
	}
	sort.Slice(scan.InputFiles, func(i, j int) bool { return scan.InputFiles[i].Path < scan.InputFiles[j].Path })
}

// generatorInputSet is every file a configMapGenerator entry of the scan reads.
func generatorInputSet(kusts map[string]*kustomizationDoc) map[string]struct{} {
	inputs := map[string]struct{}{}
	for _, doc := range kusts {
		for _, entry := range doc.configMapGenerators {
			for _, p := range entry.inputPaths() {
				if p != "" {
					inputs[p] = struct{}{}
				}
			}
		}
	}
	return inputs
}

// inputAttacher moves generator inputs into scan.InputFiles, each at most once.
type inputAttacher struct {
	scan    *FolderScan
	read    func(string) ([]byte, bool)
	isInput func(string) bool
	have    map[string]struct{}
}

// fromYAML moves the YAML files that are inputs, already read by the walk.
func (a *inputAttacher) fromYAML() {
	kept := a.scan.YAMLFiles[:0:0]
	for _, f := range a.scan.YAMLFiles {
		if !a.isInput(f.Path) {
			kept = append(kept, f)
			continue
		}
		if _, dup := a.have[filepathToSlash(f.Path)]; !dup {
			a.scan.InputFiles = append(a.scan.InputFiles, f)
			a.have[filepathToSlash(f.Path)] = struct{}{}
		}
	}
	a.scan.YAMLFiles = kept
}

// fromForeign reads the foreign files that are inputs and stops refusing them.
func (a *inputAttacher) fromForeign() {
	kept := a.scan.Foreign[:0:0]
	for _, f := range a.scan.Foreign {
		if f.Kind == ForeignFile && a.isInput(f.Path) && a.readInput(f.Path) {
			continue
		}
		kept = append(kept, f)
	}
	a.scan.Foreign = kept
}

// readInput reads one input the walk did not, reporting whether it is now attached.
func (a *inputAttacher) readInput(p string) bool {
	if _, dup := a.have[filepathToSlash(p)]; dup {
		return true
	}
	content, ok := a.read(p)
	if !ok {
		return false
	}
	a.scan.InputFiles = append(a.scan.InputFiles, manifestedit.FileContent{Path: p, Content: content})
	a.have[filepathToSlash(p)] = struct{}{}
	return true
}

// RenderFiles is every file a render reads: the YAML files and the generator inputs.
func (s FolderScan) RenderFiles() []manifestedit.FileContent {
	if len(s.InputFiles) == 0 {
		return s.YAMLFiles
	}
	out := make([]manifestedit.FileContent, 0, len(s.YAMLFiles)+len(s.InputFiles))
	out = append(out, s.YAMLFiles...)
	return append(out, s.InputFiles...)
}
