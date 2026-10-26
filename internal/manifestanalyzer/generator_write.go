// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// This file plans the write of a live ConfigMap back into the configMapGenerator entry that
// renders it. The plan is an attribution, and like every attribution it may be wrong: the oracle
// re-renders the folder with the planned bytes and refuses the write unless kustomize then
// produces exactly the live object.
//
// The rules, per data key:
//
//   - a files: key whose value changed: the file's whole content becomes the live value;
//   - an envs: key whose value changed: its line is edited in place, so comments, order and
//     every other line keep their bytes;
//   - a literals: key whose value changed: its KEY=value item in kustomization.yaml is edited in
//     place, as an images: entry is;
//   - a key the live object added: appended to the entry's env file when it has exactly one, and
//     otherwise to its literals: when it has some and no env file;
//   - a key the live object removed: its env line or literal goes; a files: key, whose value is
//     a whole file, has nothing to remove;
//   - binaryData, and anything outside data, have no input to land in.
//
// A live delete removes the entry from configMapGenerator: and the input files only it reads; see
// PlanGeneratedDelete.

// GeneratedObject is one object a configMapGenerator entry renders, as kustomize renders it.
type GeneratedObject struct {
	// GeneratedIn is the kustomization whose generator produced it (slash).
	GeneratedIn string
	// Entry is the configMapGenerator entry that produced it. Nil when the kustomization holds
	// no entry of the rendered name, or more than one: the object then has no single source.
	Entry *ConfigMapGeneratorEntry
	// Rendered is the object as the render produces it.
	Rendered *unstructured.Unstructured
	// Roots are the render roots that render this entry, in any namespace, sorted.
	Roots []string
	// SharedInputs are the entry's input files something else also reads: another generator
	// entry, a resources: entry or a patch. A delete of the object keeps them.
	SharedInputs map[string]struct{}
}

// generatedRender is one generated object one render root produced.
type generatedRender struct {
	root   string
	in     string
	object *unstructured.Unstructured
}

// generatedObjects groups the generated renders of a scan into one GeneratedObject per entry and
// namespace, each naming every root that renders the entry.
func generatedObjects(renders []generatedRender, kusts map[string]*kustomizationDoc) []*GeneratedObject {
	type entryKey struct{ in, kind, name string }
	type objectKey struct {
		entry     entryKey
		namespace string
	}
	roots := map[entryKey]map[string]struct{}{}
	byObject := map[objectKey]*GeneratedObject{}
	inputUses := inputUseCounts(kusts)
	var order []objectKey
	for _, r := range renders {
		ek := entryKey{in: r.in, kind: r.object.GetKind(), name: r.object.GetName()}
		if roots[ek] == nil {
			roots[ek] = map[string]struct{}{}
		}
		roots[ek][r.root] = struct{}{}
		ok := objectKey{entry: ek, namespace: r.object.GetNamespace()}
		if _, seen := byObject[ok]; seen {
			continue
		}
		entry := generatorEntryFor(kusts[slashDir(r.in)], r.object)
		byObject[ok] = &GeneratedObject{
			GeneratedIn:  r.in,
			Entry:        entry,
			Rendered:     r.object,
			SharedInputs: sharedInputsOf(entry, inputUses),
		}
		order = append(order, ok)
	}
	out := make([]*GeneratedObject, 0, len(order))
	for _, ok := range order {
		g := byObject[ok]
		g.Roots = sortedKeysOfSet(roots[ok.entry])
		out = append(out, g)
	}
	return out
}

// inputUseCounts counts, per file, how many generator entries, resources: entries and patches of
// the scan read it.
func inputUseCounts(kusts map[string]*kustomizationDoc) map[string]int {
	uses := map[string]int{}
	for _, doc := range kusts {
		for _, entry := range doc.configMapGenerators {
			seen := map[string]struct{}{}
			for _, p := range entry.inputPaths() {
				if _, dup := seen[p]; !dup {
					seen[p] = struct{}{}
					uses[p]++
				}
			}
		}
		for _, p := range doc.patches {
			uses[p]++
		}
	}
	for p := range reachedResourceFiles(kusts) {
		uses[p]++
	}
	return uses
}

// sharedInputsOf is the entry's inputs that more than one reader reads.
func sharedInputsOf(entry *ConfigMapGeneratorEntry, uses map[string]int) map[string]struct{} {
	if entry == nil {
		return nil
	}
	out := map[string]struct{}{}
	for _, p := range entry.inputPaths() {
		if uses[p] > 1 {
			out[p] = struct{}{}
		}
	}
	return out
}

// generatorEntryFor finds the entry that rendered obj: the one entry of its name, or, when two
// entries share a name, the one whose own namespace is the object's.
func generatorEntryFor(doc *kustomizationDoc, obj *unstructured.Unstructured) *ConfigMapGeneratorEntry {
	if doc == nil {
		return nil
	}
	var named []*ConfigMapGeneratorEntry
	for i := range doc.configMapGenerators {
		if doc.configMapGenerators[i].Name == obj.GetName() {
			named = append(named, &doc.configMapGenerators[i])
		}
	}
	if len(named) == 1 {
		return named[0]
	}
	var match *ConfigMapGeneratorEntry
	for _, e := range named {
		if e.Namespace == obj.GetNamespace() {
			if match != nil {
				return nil
			}
			match = e
		}
	}
	return match
}

// GeneratedObjectsFor returns every generated object the live object can be: same apiVersion,
// kind and name, and either the same namespace or none, which is the render of a folder whose
// namespace is supplied downstream. More than one result is an object with no single source.
func (s *ManifestStore) GeneratedObjectsFor(live *unstructured.Unstructured) []*GeneratedObject {
	if live == nil {
		return nil
	}
	var out []*GeneratedObject
	for _, g := range s.generated {
		r := g.Rendered
		if r.GetAPIVersion() != live.GetAPIVersion() || r.GetKind() != live.GetKind() ||
			r.GetName() != live.GetName() {
			continue
		}
		if r.GetNamespace() != live.GetNamespace() && r.GetNamespace() != "" {
			continue
		}
		out = append(out, g)
	}
	return out
}

// GeneratedWrite is one file a generator write rewrites: an input file, or the kustomization for
// a literal or a removed entry. A nil Content deletes the file.
type GeneratedWrite struct {
	Path    string
	Content []byte
}

// errGeneratedWrite marks a generated object the writer cannot write back. The message says why;
// the caller names the object and the kustomization.
var errGeneratedWrite = errors.New("no destination for the change")

// PlanGeneratedWrite plans the write of desired, the live object as the writer projects it, into
// the inputs of the generator that renders gen. writeScope is the write jail (empty: everything
// is writable), and read returns an input file's current bytes. An error is a refusal and names
// the reason; nothing has been written.
func PlanGeneratedWrite(
	gen *GeneratedObject,
	desired *unstructured.Unstructured,
	writeScope string,
	read func(path string) ([]byte, bool),
) ([]GeneratedWrite, error) {
	entry, err := writableEntry(gen, writeScope)
	if err != nil {
		return nil, err
	}
	if fields := changedOutsideData(desired, gen.Rendered); len(fields) > 0 {
		return nil, fmt.Errorf("%w: %s changed, and a generated ConfigMap takes it from the kustomization, "+
			"not from an input file", errGeneratedWrite, strings.Join(fields, ", "))
	}

	current, err := readGeneratorSources(entry, read)
	if err != nil {
		return nil, err
	}
	liveBinary, _, _ := unstructured.NestedStringMap(desired.Object, "binaryData")
	if !equalStringMaps(liveBinary, current.binary) {
		return nil, fmt.Errorf("%w: binaryData changed, and the operator writes no binary input file",
			errGeneratedWrite)
	}

	plan := generatorPlan{entry: entry, writeScope: writeScope, read: read, files: map[string][]byte{}}
	liveData, _, _ := unstructured.NestedStringMap(desired.Object, "data")
	for _, key := range sortedKeysOfSet(unionStringKeys(liveData, current.data)) {
		if err := plan.key(key, liveData, current); err != nil {
			return nil, err
		}
	}
	if err := plan.applyLiterals(); err != nil {
		return nil, err
	}
	return plan.writes(read), nil
}

// writableEntry is the entry behind gen, or the refusal that says why its inputs are not this
// target's to write: no single entry, an entry two render roots share, or one in a read-only base.
func writableEntry(gen *GeneratedObject, writeScope string) (*ConfigMapGeneratorEntry, error) {
	entry := gen.Entry
	if entry == nil {
		return nil, fmt.Errorf("%w: %s holds no single configMapGenerator entry named %q",
			errGeneratedWrite, gen.GeneratedIn, gen.Rendered.GetName())
	}
	if len(gen.Roots) > 1 {
		return nil, fmt.Errorf("%w: the generator is rendered by %d render roots (%s), so its inputs are shared",
			errGeneratedWrite, len(gen.Roots), strings.Join(gen.Roots, ", "))
	}
	if writeScope != "" && !pathWithin(entry.KustomizationPath, writeScope) {
		return nil, fmt.Errorf("%w: the generator lives in %s, which this target reads and never writes",
			errGeneratedWrite, entry.KustomizationPath)
	}
	return entry, nil
}

// PlanGeneratedDelete plans the removal of the object gen renders: its entry leaves
// configMapGenerator:, and every input file only that entry reads is deleted. An input something
// else also reads stays, and so does one outside the write jail. read returns a file's current
// bytes. An error is a refusal and names the reason.
func PlanGeneratedDelete(
	gen *GeneratedObject,
	writeScope string,
	read func(path string) ([]byte, bool),
) ([]GeneratedWrite, error) {
	entry, err := writableEntry(gen, writeScope)
	if err != nil {
		return nil, err
	}
	kustomization, ok := read(entry.KustomizationPath)
	if !ok {
		return nil, fmt.Errorf("%w: cannot read %s", errGeneratedWrite, entry.KustomizationPath)
	}
	res, diags := manifestedit.RemoveConfigMapGeneratorEntry(
		entry.KustomizationPath, kustomization, entry.Index, entry.Name)
	if res.Mode != manifestedit.EditPatched {
		return nil, fmt.Errorf("%w: the entry cannot be removed from %s%s",
			errGeneratedWrite, entry.KustomizationPath, diagnosticSuffix(diags))
	}
	writes := []GeneratedWrite{{Path: entry.KustomizationPath, Content: res.Content}}
	for _, p := range sortedKeysOfSet(pathsSet(entry.inputPaths())) {
		_, shared := gen.SharedInputs[p]
		_, present := read(p)
		if shared || !present || (writeScope != "" && !pathWithin(p, writeScope)) {
			continue
		}
		writes = append(writes, GeneratedWrite{Path: p})
	}
	return writes, nil
}

func pathsSet(paths []string) map[string]struct{} {
	out := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		out[p] = struct{}{}
	}
	return out
}

// diagnosticSuffix renders an editor's diagnostics after a refusal's sentence.
func diagnosticSuffix(diags []manifestedit.Diagnostic) string {
	if len(diags) == 0 {
		return ""
	}
	parts := make([]string, 0, len(diags))
	for _, d := range diags {
		parts = append(parts, d.Message)
	}
	return ": " + strings.Join(parts, "; ")
}

// generatorPlan accumulates the new content of each input file a write touches.
type generatorPlan struct {
	entry      *ConfigMapGeneratorEntry
	writeScope string
	read       func(string) ([]byte, bool)
	files      map[string][]byte
	// literals are the literals: edits, applied to the kustomization in one call at the end.
	literals []manifestedit.LiteralEdit
}

// key routes one data key's difference between the live object and the inputs.
func (p *generatorPlan) key(key string, live map[string]string, current generatorSources) error {
	value, inLive := live[key]
	was, inCurrent := current.data[key]
	switch {
	case inLive && inCurrent && value == was:
		return nil
	case !inCurrent:
		return p.added(key, value)
	}
	src, known := current.source[key]
	switch {
	case !known:
		return fmt.Errorf("%w: key %q has no input file", errGeneratedWrite, key)
	case src.kind == sourceLiteral && !inLive:
		p.literals = append(p.literals, manifestedit.LiteralEdit{Key: key, Remove: true})
		return nil
	case src.kind == sourceLiteral:
		p.literals = append(p.literals, manifestedit.LiteralEdit{Key: key, Value: value})
		return nil
	case !inLive:
		return p.removed(key, src)
	case src.kind == sourceFile:
		if err := p.writable(src.path); err != nil {
			return err
		}
		p.files[src.path] = []byte(value)
		return nil
	default:
		return p.setEnv(src.path, key, value)
	}
}

// added routes a key the live object has and the inputs do not: to the entry's one env file, or,
// for an entry with no env file, to its literals:.
func (p *generatorPlan) added(key, value string) error {
	switch {
	case len(p.entry.Envs) == 1:
		return p.setEnv(p.entry.Envs[0], key, value)
	case len(p.entry.Envs) > 1:
		return fmt.Errorf("%w: key %q is new, and the entry has %d envs: files, so no single one to add it to",
			errGeneratedWrite, key, len(p.entry.Envs))
	case len(p.entry.Literals) > 0:
		p.literals = append(p.literals, manifestedit.LiteralEdit{Key: key, Value: value})
		return nil
	default:
		return fmt.Errorf("%w: key %q is new, and the entry has no envs: file or literals: to add it to",
			errGeneratedWrite, key)
	}
}

// applyLiterals folds the literal edits into the kustomization, in one edit of the entry.
func (p *generatorPlan) applyLiterals() error {
	if len(p.literals) == 0 {
		return nil
	}
	path := p.entry.KustomizationPath
	res, diags := manifestedit.EditConfigMapGeneratorLiterals(
		path, p.content(path), p.entry.Index, p.entry.Name, p.literals)
	switch res.Mode {
	case manifestedit.EditPatched:
		p.files[path] = res.Content
		return nil
	case manifestedit.EditNoChange:
		return nil
	case manifestedit.EditSkipped, manifestedit.EditDeleted, manifestedit.EditWholeReplace:
	}
	return fmt.Errorf("%w: the literals: of the entry cannot be edited%s",
		errGeneratedWrite, diagnosticSuffix(diags))
}

// removed routes a key the inputs have and the live object does not: its env line goes, and a
// files: key, whose value is a whole file, has no line to remove.
func (p *generatorPlan) removed(key string, src generatorSource) error {
	if src.kind == sourceFile {
		return fmt.Errorf("%w: key %q was removed, and its value is the whole of %s", errGeneratedWrite, key, src.path)
	}
	if err := p.writable(src.path); err != nil {
		return err
	}
	p.files[src.path] = removeEnvKey(p.content(src.path), key)
	return nil
}

// setEnv sets key on its line of the env file at path, appending a line when there is none.
func (p *generatorPlan) setEnv(path, key, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%w: the value of key %q spans lines, which a line of %s cannot hold",
			errGeneratedWrite, key, path)
	}
	if err := p.writable(path); err != nil {
		return err
	}
	p.files[path] = setEnvValue(p.content(path), key, value)
	return nil
}

// writable refuses an input file outside the write jail.
func (p *generatorPlan) writable(path string) error {
	if p.writeScope != "" && !pathWithin(path, p.writeScope) {
		return fmt.Errorf("%w: %s is outside spec.path", errGeneratedWrite, path)
	}
	return nil
}

// content is an input file's bytes as the plan has left them so far.
func (p *generatorPlan) content(path string) []byte {
	if c, ok := p.files[path]; ok {
		return c
	}
	c, _ := p.read(path)
	return c
}

// writes returns the planned files that differ from what read holds, sorted by path.
func (p *generatorPlan) writes(read func(string) ([]byte, bool)) []GeneratedWrite {
	paths := make([]string, 0, len(p.files))
	for path := range p.files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var out []GeneratedWrite
	for _, path := range paths {
		if before, ok := read(path); ok && bytes.Equal(before, p.files[path]) {
			continue
		}
		out = append(out, GeneratedWrite{Path: path, Content: p.files[path]})
	}
	return out
}

// changedOutsideData names the top-level fields, and the metadata fields, in which desired and
// rendered differ once data, binaryData and the namespace are set aside. The namespace is the
// oracle's to compare: it knows when a target supplies it downstream.
func changedOutsideData(desired, rendered *unstructured.Unstructured) []string {
	strip := func(o *unstructured.Unstructured) map[string]interface{} {
		c := o.DeepCopy().Object
		delete(c, "data")
		delete(c, "binaryData")
		unstructured.RemoveNestedField(c, "metadata", "namespace")
		return c
	}
	a, b := strip(desired), strip(rendered)
	var out []string
	for _, key := range sortedKeysOfSet(unionAnyKeys(a, b)) {
		if key != "metadata" {
			if !sameValue(a[key], b[key]) {
				out = append(out, key)
			}
			continue
		}
		am, _ := a[key].(map[string]interface{})
		bm, _ := b[key].(map[string]interface{})
		for _, field := range sortedKeysOfSet(unionAnyKeys(am, bm)) {
			if !sameValue(am[field], bm[field]) {
				out = append(out, "metadata."+field)
			}
		}
	}
	return out
}

func sameValue(a, b interface{}) bool {
	return sameObject(&unstructured.Unstructured{Object: map[string]interface{}{"v": a}},
		&unstructured.Unstructured{Object: map[string]interface{}{"v": b}})
}

// sourceKind is where a generated key's value comes from.
type sourceKind int

const (
	sourceFile sourceKind = iota
	sourceEnv
	sourceLiteral
)

type generatorSource struct {
	kind sourceKind
	path string
}

// generatorSources is the data an entry's inputs produce, read the way kustomize reads them
// (api/kv), and where each key came from.
type generatorSources struct {
	data   map[string]string
	binary map[string]string
	source map[string]generatorSource
}

// readGeneratorSources reads an entry's inputs as they stand. kustomize loads envs:, then
// literals:, then files:, and a key repeated across them fails the build, so the order only
// decides which source a refusal names.
func readGeneratorSources(
	entry *ConfigMapGeneratorEntry,
	read func(string) ([]byte, bool),
) (generatorSources, error) {
	out := generatorSources{
		data:   map[string]string{},
		binary: map[string]string{},
		source: map[string]generatorSource{},
	}
	for _, env := range entry.Envs {
		if err := out.readEnv(env, read); err != nil {
			return generatorSources{}, err
		}
	}
	for _, literal := range entry.Literals {
		key, value, ok := parseLiteralSource(literal)
		if !ok {
			return generatorSources{}, fmt.Errorf("%w: literal %q does not parse", errGeneratedWrite, literal)
		}
		if err := out.add(key, value, generatorSource{kind: sourceLiteral}); err != nil {
			return generatorSources{}, err
		}
	}
	for _, f := range entry.Files {
		if err := out.readFile(f, read); err != nil {
			return generatorSources{}, err
		}
	}
	return out, nil
}

// add records one key, refusing a key a second input repeats.
func (g generatorSources) add(key, value string, src generatorSource) error {
	if _, dup := g.source[key]; dup {
		return fmt.Errorf("%w: key %q is set by more than one input of the entry", errGeneratedWrite, key)
	}
	g.source[key] = src
	g.data[key] = value
	return nil
}

// readEnv records every pair of one envs: file.
func (g generatorSources) readEnv(env string, read func(string) ([]byte, bool)) error {
	content, ok := read(env)
	if !ok {
		return fmt.Errorf("%w: cannot read %s", errGeneratedWrite, env)
	}
	for _, kv := range envPairs(content) {
		if err := g.add(kv.key, kv.value, generatorSource{kind: sourceEnv, path: env}); err != nil {
			return err
		}
	}
	return nil
}

// readFile records one files: source. kustomize puts a file that is not UTF-8 into binaryData,
// base64-encoded.
func (g generatorSources) readFile(f GeneratorFileSource, read func(string) ([]byte, bool)) error {
	content, ok := read(f.Path)
	if !ok {
		return fmt.Errorf("%w: cannot read %s", errGeneratedWrite, f.Path)
	}
	if err := g.add(f.Key, string(content), generatorSource{kind: sourceFile, path: f.Path}); err != nil {
		return err
	}
	if !utf8.Valid(content) {
		delete(g.data, f.Key)
		g.binary[f.Key] = base64.StdEncoding.EncodeToString(content)
	}
	return nil
}

// parseLiteralSource splits a literals: entry the way kustomize does (kv.parseLiteralSource):
// at the first "=", with one pair of matching surrounding quotes removed from the value.
func parseLiteralSource(source string) (string, string, bool) {
	key, value, found := strings.Cut(source, "=")
	if !found || key == "" {
		return "", "", false
	}
	if len(value) >= 2 && value[0] == value[len(value)-1] && (value[0] == '"' || value[0] == '\'') {
		value = value[1 : len(value)-1]
	}
	return key, value, true
}

// utf8BOM is the byte-order mark kustomize strips from an env file's first line.
const utf8BOM = "\xEF\xBB\xBF"

// envLine is one line of an env file, as kustomize reads it (kv.keyValuesFromLine).
type envLine struct {
	// raw is the line with its terminator.
	raw []byte
	// prefix is what precedes the key: a byte-order mark and leading whitespace.
	prefix []byte
	// key is empty for a blank line or a comment.
	key, value string
	// terminator is "\n", "\r\n", or empty on a last line without one.
	terminator string
}

type envPair struct{ key, value string }

// envLines splits an env file into lines and parses each as kustomize does: leading whitespace
// is trimmed, a line starting with "#" is a comment, the key is everything before the first "="
// and the value everything after it, verbatim. A line with no "=" is a key with an empty value.
func envLines(content []byte) []envLine {
	var out []envLine
	for i := 0; len(content) > 0; i++ {
		end := bytes.IndexByte(content, '\n')
		raw := content
		if end >= 0 {
			raw = content[:end+1]
		}
		content = content[len(raw):]

		line := envLine{raw: raw}
		body := raw
		switch {
		case bytes.HasSuffix(body, []byte("\r\n")):
			body, line.terminator = body[:len(body)-2], "\r\n"
		case bytes.HasSuffix(body, []byte("\n")):
			body, line.terminator = body[:len(body)-1], "\n"
		}
		start := 0
		if i == 0 && bytes.HasPrefix(body, []byte(utf8BOM)) {
			start = len(utf8BOM)
		}
		trimmed := bytes.TrimLeftFunc(body[start:], unicode.IsSpace)
		line.prefix = body[:len(body)-len(trimmed)]
		if len(trimmed) > 0 && trimmed[0] != '#' {
			key, value, _ := strings.Cut(string(trimmed), "=")
			line.key, line.value = key, value
		}
		out = append(out, line)
	}
	return out
}

// envPairs is the key/value pairs of an env file, in order.
func envPairs(content []byte) []envPair {
	var out []envPair
	for _, l := range envLines(content) {
		if l.key != "" {
			out = append(out, envPair{key: l.key, value: l.value})
		}
	}
	return out
}

// setEnvValue sets key to value on the first line that defines it, keeping the line's indent and
// terminator, or appends "key=value" when no line does.
func setEnvValue(content []byte, key, value string) []byte {
	var out []byte
	set := false
	for _, l := range envLines(content) {
		if !set && l.key == key {
			out = append(out, l.prefix...)
			out = append(out, key+"="+value+l.terminator...)
			set = true
			continue
		}
		out = append(out, l.raw...)
	}
	if set {
		return out
	}
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return append(out, key+"="+value+"\n"...)
}

// removeEnvKey drops every line that defines key.
func removeEnvKey(content []byte, key string) []byte {
	out := []byte{}
	for _, l := range envLines(content) {
		if l.key == key {
			continue
		}
		out = append(out, l.raw...)
	}
	return out
}

func equalStringMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func unionStringKeys(a, b map[string]string) map[string]struct{} {
	out := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		out[k] = struct{}{}
	}
	for k := range b {
		out[k] = struct{}{}
	}
	return out
}

func unionAnyKeys(a, b map[string]interface{}) map[string]struct{} {
	out := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		out[k] = struct{}{}
	}
	for k := range b {
		out[k] = struct{}{}
	}
	return out
}

// pathSet is the set of slash paths of files.
func pathSet(files []manifestedit.FileContent) map[string]struct{} {
	if len(files) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(files))
	for _, f := range files {
		out[filepathToSlash(f.Path)] = struct{}{}
	}
	return out
}
