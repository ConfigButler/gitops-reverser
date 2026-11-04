// SPDX-License-Identifier: Apache-2.0

package git

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
)

// The reference model of docs/design/concurrent-author-windows.md ("How to test it"): a small,
// independent implementation of the window rules, fed the same random inputs as a branch worker on a
// real Git server, with the worker checked against it.
//
//   - After every step: the open windows, oldest first (each with its number, author, objects,
//     attached save, deadline, charge and footprint), the timers armed for them and for the waiting
//     saves, the registered saves (attached or waiting, and until when), the retained-byte charge,
//     the close reasons so far, and every save's outcome are the model's.
//   - Every commit the worker publishes is the one the model's closing window makes: its author, its
//     message, every path it changes, and each file whole, its documents parsed. Per document, the
//     writes that land do so in the order they were accepted, so an older write never lands after a
//     newer one. A commit of a window that was open beside another changes no path outside the
//     window's footprint.
//   - Every save settles exactly once, or is given back at shutdown.
//   - Drained, Git holds the model's tree, path for path, and nothing is charged.
//
// The folder holds objects in three shapes: x, w and z each alone in its own file at the built-in
// path; k in a kustomize root (kust/, which also takes every object created anew, since it is the
// folder's one render root); and s1 and s2 in one shared file. The model proves footprints by its
// own rules: an object alone in a file no kustomization governs has that file; anything else, a
// create included, is unknown, and never opens beside another window. There is no object y: kustomize
// reads a bare y as a boolean, so the render of the root refuses a ConfigMap named y once it lands
// there.
//
// The model shares no window code with the worker: only the event fixtures, the per-event charge
// (eventRetainedCharge, attachCharge) and the close-reason names. The worker's timers are real; the
// driver steps the worker's clock to the time each armed timer is due and delivers that timer's wake,
// so a deadline the worker armed no timer for is never delivered.
//
// WINDOW_MODEL_SEEDS and WINDOW_MODEL_STEPS raise the search; a failure names its seed, and
// WINDOW_MODEL_SEED replays one. WINDOW_MODEL_LOGS prints the worker's log with a failure.

const (
	// modelIdle and modelMax are the commit window the GitTarget declares none of, so the loop's default.
	modelIdle = 10 * time.Second
	modelMax  = 60 * time.Second
)

var (
	modelObjects = []string{"x", "w", "z", "k", "s1", "s2"}
	modelAuthors = []string{"alice", "bob", "carol"}
	// modelBootstrap is what the worker adds to the folder, from its template, with the first commit
	// it makes there. The GitTarget encrypts nothing, so there is no .sops.yaml.
	modelBootstrap = []string{gitTargetIgnoreFileName, "README.md"}
	// modelKustDir is the folder's one kustomize root, and modelKustFile its kustomization.
	modelKustDir  = echoTarget + "/kust"
	modelKustFile = modelKustDir + "/kustomization.yaml"
	// modelSharedFile holds s1 and s2.
	modelSharedFile = echoTarget + "/default/configmaps/shared.yaml"
)

// modelTree is a Git tree of the folder: each object's value, the files and the objects each holds in
// document order, and the kustomize root's resources: entries.
type modelTree struct {
	values map[string]string
	files  map[string][]string
	kust   []string
}

func (t *modelTree) clone() modelTree {
	files := make(map[string][]string, len(t.files))
	for p, objs := range t.files {
		files[p] = slices.Clone(objs)
	}
	return modelTree{values: maps.Clone(t.values), files: files, kust: slices.Clone(t.kust)}
}

// loc is the file holding obj's document, or "".
func (t *modelTree) loc(obj string) string {
	for p, objs := range t.files {
		if slices.Contains(objs, obj) {
			return p
		}
	}
	return ""
}

// set is what a write of obj with value (a delete when "") does to the tree, as the worker places it:
// an existing document is edited where it lives; a document removed from the kustomize root leaves its
// resources: entry with it; a new object goes into the root and is registered there. It returns the
// paths it changes.
func (t *modelTree) set(obj, value string) []string {
	at := t.loc(obj)
	switch {
	case value == "" && at == "":
		return nil
	case value == "":
		delete(t.values, obj)
		t.files[at] = slices.DeleteFunc(t.files[at], func(o string) bool { return o == obj })
		if len(t.files[at]) > 0 {
			return []string{at}
		}
		delete(t.files, at)
		if entry := strings.TrimPrefix(at, modelKustDir+"/"); entry != at {
			t.kust = slices.DeleteFunc(t.kust, func(e string) bool { return e == entry })
			return []string{at, modelKustFile}
		}
		return []string{at}
	case at != "":
		if t.values[obj] == value {
			return nil
		}
		t.values[obj] = value
		return []string{at}
	default:
		at = modelKustDir + "/" + obj + ".yaml"
		t.values[obj] = value
		t.files[at] = []string{obj}
		t.kust = append(t.kust, obj+".yaml")
		return []string{at, modelKustFile}
	}
}

// setOutside is a write from another clone: it edits a document where it lives, or adds the object at
// its built-in path, unregistered.
func (t *modelTree) setOutside(obj, value string) string {
	at := t.loc(obj)
	if at == "" {
		at = echoFile(obj)
		t.files[at] = []string{obj}
	}
	t.values[obj] = value
	return at
}

// render is the bytes of the file at path.
func (t *modelTree) render(path string) string {
	if path == modelKustFile {
		var b strings.Builder
		b.WriteString("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n")
		for _, entry := range t.kust {
			b.WriteString("- " + entry + "\n")
		}
		return b.String()
	}
	var docs []string
	for _, obj := range t.files[path] {
		docs = append(docs, echoDocument(obj, t.values[obj]))
	}
	return strings.Join(docs, "---\n")
}

// file is what the tree holds at path.
func (t *modelTree) file(path string) modelFile {
	if path == modelKustFile {
		return modelFile{kustomization: true, entries: slices.Clone(t.kust)}
	}
	var f modelFile
	for _, obj := range t.files[path] {
		f.docs = append(f.docs, [2]string{obj, t.values[obj]})
	}
	return f
}

// modelFootprint is a window's footprint by the model's rules: nil is a window never proven.
type modelFootprint struct {
	paths   []string
	unknown bool
}

func (f *modelFootprint) String() string {
	switch {
	case f == nil:
		return "unproven"
	case f.unknown:
		return "unknown"
	default:
		return "[" + strings.Join(f.paths, " ") + "]"
	}
}

func (f *modelFootprint) overlaps(other modelFootprint) bool {
	if f.unknown || other.unknown {
		return true
	}
	for _, p := range f.paths {
		if slices.Contains(other.paths, p) {
			return true
		}
	}
	return false
}

func (f *modelFootprint) union(other modelFootprint) modelFootprint {
	if f.unknown || other.unknown {
		return modelFootprint{unknown: true}
	}
	paths := slices.Concat(f.paths, other.paths)
	slices.Sort(paths)
	return modelFootprint{paths: slices.Compact(paths)}
}

// modelEdit is one accepted write: seq is the order the worker accepted it in, and value is the
// document's key, empty for a delete.
type modelEdit struct {
	seq    int
	obj    string
	value  string
	op     string
	charge int64
}

type modelSave struct {
	name, author, message string
	seq                   int
	next                  bool
	deadline              time.Time
	idle                  *time.Duration
	maxDuration           time.Duration
	attached              bool
	sawForeign            bool
}

type modelWindow struct {
	seq         int
	author      string
	objs        []string
	latest      map[string]modelEdit
	save        *modelSave
	idle        time.Duration
	noIdle      bool
	maxAt       time.Time
	lastWriteAt time.Time
	charge      int64
	footprint   *modelFootprint
}

func (w *modelWindow) closeAt() (time.Time, windowFinalizeReason) {
	if !w.noIdle && w.lastWriteAt.Add(w.idle).Before(w.maxAt) {
		return w.lastWriteAt.Add(w.idle), windowFinalizeReasonIdleTimeout
	}
	return w.maxAt, windowFinalizeReasonMaxDuration
}

// modelPending is a decided write the next push publishes. footprint is the proven footprint of the
// window it closed, when that window had company.
type modelPending struct {
	save      *modelSave
	author    string
	message   string
	resync    bool
	changes   []modelEdit
	footprint *modelFootprint
}

// modelFile is what a commit leaves at one path: the kustomization's entries, the documents, or
// nothing.
type modelFile struct {
	kustomization bool
	entries       []string
	docs          [][2]string
}

// modelCommit is a commit the worker must publish. A resync's message is not the model's to say.
// bootstrap is the folder's first commit, which also adds modelBootstrap. footprint, when set, holds
// every path the commit changes.
type modelCommit struct {
	author    string
	message   string
	resync    bool
	bootstrap bool
	paths     []string
	files     map[string]modelFile
	seqs      map[string]int
	footprint *modelFootprint
}

type windowModel struct {
	now     time.Time
	windows []*modelWindow
	seq     int
	saves   map[string]*modelSave
	saveSeq int
	editSeq int

	// remote is Git, known what the worker's checkout holds of it, cluster the live objects.
	remote, known modelTree
	cluster       map[string]string
	// history is the values each object held on the cluster, for undos.
	history map[string][]string

	foreignUnseen, applyDue, hinted bool
	// bootstrapped is whether Git holds modelBootstrap.
	bootstrapped bool

	pending  []modelPending
	commits  []modelCommit
	closes   []string
	outcomes map[string]FinalizeOutcome
	given    map[string]bool
	// landed is, per document, the acceptance seq of the last write a commit landed.
	landed map[string]int
}

func newWindowModel(now time.Time, tree modelTree) *windowModel {
	m := &windowModel{
		now: now, saves: map[string]*modelSave{},
		remote: tree.clone(), known: tree.clone(), cluster: maps.Clone(tree.values),
		history: map[string][]string{}, outcomes: map[string]FinalizeOutcome{}, given: map[string]bool{},
		landed: map[string]int{},
	}
	for obj, value := range tree.values {
		m.history[obj] = []string{value}
	}
	return m
}

func (m *windowModel) windowCharge() int64 {
	var charge int64
	for _, w := range m.windows {
		charge += w.charge
	}
	return charge
}

// describeWindows is the open windows, oldest first: each one's number, author, how many objects it
// holds, the save it carries, when and why it closes, what it is charged, and its footprint.
func (m *windowModel) describeWindows() string {
	var out []string
	for _, w := range m.windows {
		saved := ""
		if w.save != nil {
			saved = w.save.name
		}
		at, reason := w.closeAt()
		out = append(out, fmt.Sprintf("#%d %s objs=%d save=%q closes in %s (%s) charge %d footprint %s", w.seq,
			w.author, len(w.objs), saved, at.Sub(m.now), reason, w.charge, w.footprint))
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, "; ")
}

func (m *windowModel) windowOf(author string) *modelWindow {
	for _, w := range m.windows {
		if w.author == author {
			return w
		}
	}
	return nil
}

func (m *windowModel) isOpen(w *modelWindow) bool { return slices.Contains(m.windows, w) }

func (m *windowModel) others(own *modelWindow) []*modelWindow {
	var out []*modelWindow
	for _, w := range m.windows {
		if w != own {
			out = append(out, w)
		}
	}
	return out
}

func (m *windowModel) waiting(s *modelSave) bool { return !s.attached && s.deadline.After(m.now) }

// prove is the footprint of a write of obj on the tree the worker's checkout holds. Before the folder
// is bootstrapped every write adds its bootstrap files.
func (m *windowModel) prove(obj string) modelFootprint {
	at := m.known.loc(obj)
	switch {
	case !m.bootstrapped, at == "", strings.HasPrefix(at, modelKustDir+"/"), len(m.known.files[at]) > 1:
		return modelFootprint{unknown: true}
	default:
		return modelFootprint{paths: []string{at}}
	}
}

func (m *windowModel) proveWindow(w *modelWindow) modelFootprint {
	f := modelFootprint{}
	for _, obj := range w.objs {
		f = f.union(m.prove(obj))
	}
	return f
}

// admit closes the windows a write of obj by author cannot sit beside, and returns the footprint its
// window takes.
func (m *windowModel) admit(author, obj string) *modelFootprint {
	own := m.windowOf(author)
	if len(m.others(own)) == 0 {
		if own == nil || own.footprint == nil {
			return nil
		}
		f := m.proveWindow(own)
		f = f.union(m.prove(obj))
		return &f
	}
	write := m.prove(obj)
	m.reprove()
	own = m.windowOf(author)
	for _, other := range m.others(own) {
		switch {
		case write.unknown || other.footprint.unknown:
			m.close(other, windowFinalizeReasonIdentityChange)
		case write.overlaps(*other.footprint):
			m.close(other, windowFinalizeReasonPathConflict)
		}
	}
	if own == nil {
		return &write
	}
	f := own.footprint.union(write)
	return &f
}

// reprove proves every open window again, and closes, oldest first, the windows whose footprints
// overlap.
func (m *windowModel) reprove() {
	for _, w := range m.windows {
		f := m.proveWindow(w)
		w.footprint = &f
	}
	involved := map[*modelWindow]bool{}
	for i, a := range m.windows {
		for _, b := range m.windows[i+1:] {
			if a.footprint.overlaps(*b.footprint) {
				involved[a], involved[b] = true, true
			}
		}
	}
	for _, w := range slices.Clone(m.windows) {
		if involved[w] {
			m.close(w, windowFinalizeReasonPathConflict)
		}
	}
}

// write is appendLiveEvent's rules for one accepted write.
func (m *windowModel) write(author string, edit modelEdit, pressure bool) {
	m.editSeq++
	edit.seq = m.editSeq
	m.cluster[edit.obj] = edit.value
	if edit.value != "" {
		m.history[edit.obj] = append(m.history[edit.obj], edit.value)
	}
	footprint := m.admit(author, edit.obj)
	w := m.windowOf(author)
	opened := w == nil
	if opened {
		m.seq++
		w = &modelWindow{
			seq: m.seq, author: author, latest: map[string]modelEdit{},
			idle: modelIdle, maxAt: m.now.Add(modelMax),
		}
		m.windows = append(m.windows, w)
	}
	w.footprint = footprint
	if old, ok := w.latest[edit.obj]; ok {
		w.charge -= old.charge
	} else {
		w.objs = append(w.objs, edit.obj)
	}
	w.latest[edit.obj] = edit
	w.charge += edit.charge
	w.lastWriteAt = m.now
	if opened {
		m.noteForeign()
		m.attachWaiting()
		if !m.isOpen(w) {
			return
		}
	}
	if pressure {
		m.closeAll(windowFinalizeReasonBufferLimit)
		return
	}
	m.closeOrArm()
}

// register is handleAttachCommitRequest's rules for a save.
func (m *windowModel) register(s *modelSave, attachTimeout time.Duration) {
	for _, earlier := range m.saves {
		if earlier.author != s.author {
			continue
		}
		collecting := false
		for _, w := range m.windows {
			collecting = collecting || (earlier.attached && w.save == earlier)
		}
		if m.waiting(earlier) || (collecting && !s.next) {
			m.outcomes[s.name] = FinalizeSaveInProgress
			return
		}
	}
	m.saveSeq++
	s.seq = m.saveSeq
	s.deadline = m.now.Add(attachTimeout)
	m.saves[s.name] = s
	if len(m.windows) == 0 {
		return
	}
	own := m.windowOf(s.author)
	if own == nil {
		s.sawForeign = true
		return
	}
	if s.next {
		m.close(own, windowFinalizeReasonAttachNext)
		return
	}
	if own.save == nil {
		m.attach(own, s)
	}
}

func (m *windowModel) attach(w *modelWindow, s *modelSave) {
	w.save, s.attached = s, true
	w.charge += attachCharge(s.message)
	w.noIdle = s.idle == nil
	if s.idle != nil {
		w.idle = *s.idle
	}
	w.maxAt = m.now.Add(s.maxDuration)
	w.lastWriteAt = m.now
	m.closeOrArm()
}

func (m *windowModel) closeOrArm() {
	for _, w := range slices.Clone(m.windows) {
		if at, reason := w.closeAt(); !at.After(m.now) {
			m.close(w, reason)
		}
	}
}

func (m *windowModel) closeAll(reason windowFinalizeReason) {
	for _, w := range slices.Clone(m.windows) {
		m.close(w, reason)
	}
}

func (m *windowModel) noteForeign() {
	for _, s := range m.saves {
		if m.waiting(s) && len(m.windows) > 0 && m.windowOf(s.author) == nil {
			s.sawForeign = true
		}
	}
}

func (m *windowModel) attachWaiting() {
	for _, w := range slices.Clone(m.windows) {
		if w.save != nil || !m.isOpen(w) {
			continue
		}
		var first *modelSave
		for _, s := range m.saves {
			if m.waiting(s) && s.author == w.author && (first == nil || s.seq < first.seq) {
				first = s
			}
		}
		if first != nil {
			m.attach(w, first)
		}
	}
}

func (m *windowModel) expire() {
	for _, s := range m.saves {
		if s.attached || s.deadline.After(m.now) {
			continue
		}
		outcome := FinalizeNoOpenWindow
		if s.sawForeign {
			outcome = FinalizeWindowMismatch
		}
		m.resolve(s, outcome)
	}
}

func (m *windowModel) resolve(s *modelSave, outcome FinalizeOutcome) {
	delete(m.saves, s.name)
	m.outcomes[s.name] = outcome
}

// close decides a window: the documents whose latest write differs from the checkout, in the order the
// window first saw them. A window that changes nothing and carries no save leaves no trace. A save is
// never trimmed: it decides every document it captured, so a replay onto a push the worker had not
// seen re-asserts them (API wins for a save). A window once proven carries its footprint, proven again
// on the checkout it commits on.
func (m *windowModel) close(w *modelWindow, reason windowFinalizeReason) {
	m.windows = slices.DeleteFunc(m.windows, func(o *modelWindow) bool { return o == w })
	m.closes = append(m.closes, fmt.Sprintf("%s %s #%d", reason, w.author, w.seq))
	var footprint *modelFootprint
	if w.footprint != nil {
		if f := m.proveWindow(w); !f.unknown {
			footprint = &f
		}
	}
	var changes []modelEdit
	for _, obj := range w.objs {
		if edit := w.latest[obj]; edit.value != m.known.values[obj] || w.save != nil {
			changes = append(changes, edit)
			m.known.set(obj, edit.value)
		}
	}
	if len(changes) == 0 && w.save == nil {
		return
	}
	p := modelPending{save: w.save, author: w.author, changes: changes, footprint: footprint}
	if w.save != nil {
		p.message = w.save.message
	} else {
		p.message = generatedMessage(changes)
	}
	m.pending = append(m.pending, p)
}

func generatedMessage(changes []modelEdit) string {
	var b strings.Builder
	plural := "s"
	if len(changes) == 1 {
		plural = ""
	}
	fmt.Fprintf(&b, "chore: sync %d resource%s\n\n", len(changes), plural)
	for _, edit := range changes {
		fmt.Fprintf(&b, "- [%s] v1/configmaps/default/%s\n", edit.op, edit.obj)
	}
	return b.String()
}

// push publishes the pending writes. A remote that moved behind the worker's back is found by the
// push, which replays the decided changes onto it, and every applier is due for the foreign commit.
// The footprint of a replayed write was proven on another tree, so it holds nothing about the commit.
func (m *windowModel) push() {
	if len(m.pending) == 0 {
		return
	}
	for _, p := range m.pending {
		commit := modelCommit{author: p.author, message: p.message, resync: p.resync,
			files: map[string]modelFile{}, seqs: map[string]int{}}
		if !m.foreignUnseen {
			commit.footprint = p.footprint
		}
		var paths []string
		for _, edit := range p.changes {
			changed := m.remote.set(edit.obj, edit.value)
			if len(changed) == 0 {
				continue
			}
			paths = append(paths, changed...)
			commit.seqs[edit.obj] = edit.seq
		}
		if len(paths) == 0 {
			if p.save != nil {
				m.resolve(p.save, FinalizeAlreadyPresent)
			}
			continue
		}
		slices.Sort(paths)
		commit.paths = slices.Compact(paths)
		for _, path := range commit.paths {
			commit.files[path] = m.remote.file(path)
		}
		commit.bootstrap = !m.bootstrapped
		m.bootstrapped = true
		m.commits = append(m.commits, commit)
		if p.save != nil {
			m.resolve(p.save, FinalizeCommitted)
		}
	}
	m.pending = nil
	if m.foreignUnseen {
		m.foreignUnseen, m.applyDue = false, true
	}
	m.known = m.remote.clone()
}

// endWake is the loop's end of every wake. It reports whether an apply was sent: the driver's applier
// then puts the tip on the cluster.
func (m *windowModel) endWake() bool {
	if len(m.saves) > 0 {
		m.noteForeign()
		m.expire()
		m.attachWaiting()
	}
	if m.hinted {
		m.hinted = false
		for _, w := range slices.Clone(m.windows) {
			if w.save == nil {
				m.close(w, windowFinalizeReasonForeignCommit)
			}
		}
	}
	m.push()
	if !m.applyDue || len(m.saves) > 0 {
		return false
	}
	m.closeAll(windowFinalizeReasonApplyDue)
	m.push()
	m.applyDue = false
	// The apply is of the tip the worker knows: a push it has not found yet is not in it.
	m.cluster = maps.Clone(m.known.values)
	return true
}

// resync closes the windows, then commits, as the operator, every live object Git does not hold as it
// is. prune.mode is onEvent, so nothing is swept. An object it adds is adopted: an apply is due.
func (m *windowModel) resync() {
	m.closeAll(windowFinalizeReasonResyncBeforeApply)
	var changes []modelEdit
	for _, obj := range modelObjects {
		value := m.cluster[obj]
		if value == "" || value == m.known.values[obj] {
			continue
		}
		if m.known.values[obj] == "" {
			m.applyDue = true
		}
		m.known.set(obj, value)
		changes = append(changes, modelEdit{obj: obj, value: value})
	}
	if len(changes) > 0 {
		m.pending = append(m.pending, modelPending{author: DefaultCommitterName, resync: true, changes: changes})
	}
}

// shutdown closes the windows and publishes; a save still waiting for a window is given back.
func (m *windowModel) shutdown() {
	m.closeAll(windowFinalizeReasonShutdown)
	m.push()
	for name := range m.saves {
		m.given[name] = true
		delete(m.saves, name)
	}
}

// windowModelRun drives one worker and one model with the same random inputs.
type windowModelRun struct {
	t         *testing.T
	seed      int64
	f         *echoFixture
	requester *recordingRequester
	clock     *testClock
	logs      *infoLines
	rng       *rand.Rand
	m         *windowModel
	base      string
	knownTip  string
	start     time.Time
	trace     []string
	checked   int
	exited    bool
	values    int
	saves     int
	// concurrent is how many steps ended with more than one window open.
	concurrent int
}

func newWindowModelRun(t *testing.T, seed int64) *windowModelRun {
	t.Helper()
	tree := modelTree{
		values: map[string]string{"x": "x0", "w": "w0", "z": "z0", "k": "k0", "s1": "s10", "s2": "s20"},
		files: map[string][]string{
			echoFile("x"): {"x"}, echoFile("w"): {"w"}, echoFile("z"): {"z"},
			modelKustDir + "/k.yaml": {"k"}, modelSharedFile: {"s1", "s2"},
		},
		kust: []string{"k.yaml"},
	}
	layout := map[string]string{}
	for _, path := range []string{modelKustFile, modelKustDir + "/k.yaml", modelSharedFile} {
		layout[path] = tree.render(path)
	}
	f := newEchoFixtureWith(t, "window-model-"+strconv.FormatInt(seed, 10), configv1alpha3.PruneOnEvent,
		map[string]string{"x": "x0", "w": "w0", "z": "z0"}, layout)
	requester := f.withClaim()
	r := &windowModelRun{t: t, seed: seed, f: f, requester: requester, rng: rand.New(rand.NewSource(seed))}
	r.clock = useTestClock(f.worker)
	r.logs = captureInfo(f.worker)
	f.loop.defaultWindow = commitWindowDefaults{idleTimeout: modelIdle, maxDuration: modelMax}
	r.base = gitIn(t, f.repoDir, "rev-parse", "main")
	r.knownTip = r.base
	r.start = r.clock.Now()
	r.m = newWindowModel(r.start, tree)
	r.m.bootstrapped = gitIn(t, f.repoDir, "ls-tree", "--name-only", r.base, echoTarget+"/README.md") != ""
	return r
}

// pushFromOutside commits the files at paths, as tree holds them, from the outside clone.
func (r *windowModelRun) pushFromOutside(tree modelTree, paths []string, message string) {
	r.t.Helper()
	clone := r.f.clone
	gitIn(r.t, clone, "pull", "--quiet", "--ff-only", "origin", "main")
	for _, path := range paths {
		full := filepath.Join(clone, path)
		require.NoError(r.t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(r.t, os.WriteFile(full, []byte(tree.render(path)), 0o600))
		gitIn(r.t, clone, "add", path)
	}
	gitIn(r.t, clone, "commit", "--quiet", "-m", message)
	gitIn(r.t, clone, "push", "--quiet", "origin", "HEAD:main")
}

func (r *windowModelRun) failf(format string, args ...any) {
	r.t.Helper()
	if os.Getenv("WINDOW_MODEL_LOGS") != "" {
		r.t.Log("the worker's log:\n" + strings.Join(r.logs.matching(""), "\n"))
		for _, p := range splitLines(gitIn(r.t, r.f.repoDir, "ls-tree", "-r", "--name-only", "main")) {
			r.t.Log("DEBUGFILE " + p + ":\n" + gitIn(r.t, r.f.repoDir, "show", "main:"+p))
		}
	}
	tail := r.trace
	if len(tail) > 40 {
		tail = tail[len(tail)-40:]
	}
	r.t.Fatalf("seed %d: %s\nlast steps:\n  %s", r.seed, fmt.Sprintf(format, args...), strings.Join(tail, "\n  "))
}

func (r *windowModelRun) notef(format string, args ...any) {
	r.trace = append(r.trace, fmt.Sprintf("t=%s ", r.clock.Now().Sub(r.start))+fmt.Sprintf(format, args...))
}

// wake runs one wake of the worker's loop on item, pushing at once whatever it decides, and the model's
// matching wake; then answers an apply sent, as the applier, and settles it in a wake of both.
func (r *windowModelRun) wake(item *WorkItem, model func()) {
	r.f.loop.lastPushAt = time.Time{}
	var released int64
	if item != nil {
		released = r.f.loop.dispatch(*item)
	}
	r.f.loop.endWake(released)
	if model != nil {
		model()
	}
	r.applied(r.m.endWake())
}

func (r *windowModelRun) applied(modelSent bool) {
	// Every push the worker has made is on main, unless a push from outside came after it unseen.
	if !r.m.foreignUnseen {
		r.knownTip = gitIn(r.t, r.f.repoDir, "rev-parse", "main")
	}
	sent := r.requester.taken()
	if modelSent != (len(sent) > 0) {
		r.failf("the model sent an apply: %v; the worker sent %d", modelSent, len(sent))
	}
	for _, req := range sent {
		if req.Commit != r.knownTip {
			r.failf("an apply was sent for %s, not the tip the worker knows, %s", req.Commit, r.knownTip)
		}
		req.Report(ApplyResult{ID: req.ID, Outcome: ApplySucceeded, AppliedRevision: req.Commit})
	}
	r.f.loop.lastPushAt = time.Time{}
	r.f.loop.endWake(0)
	if r.m.endWake() {
		r.failf("the model sent a second apply in the settling wake")
	}
}

// armed is the timer the worker has armed to fire first, when, and whether it is the commit timer. Two
// due at the same instant fire in either order, as run's select picks.
func (r *windowModelRun) armed() (time.Time, bool, bool) {
	loop := r.f.loop
	commit, attach := loop.commitTimer != nil, loop.attachTimer != nil
	switch {
	case commit && attach && loop.commitTimerAt.Equal(loop.attachTimerAt):
		return loop.commitTimerAt, r.rng.Intn(2) == 0, true
	case commit && (!attach || loop.commitTimerAt.Before(loop.attachTimerAt)):
		return loop.commitTimerAt, true, true
	case attach:
		return loop.attachTimerAt, false, true
	}
	return time.Time{}, false, false
}

// commitWake is the wake the commit timer delivers, as run does.
func (r *windowModelRun) commitWake() {
	r.notef("commit timer")
	r.f.loop.lastPushAt = time.Time{}
	r.f.loop.commitTimer = nil
	r.f.loop.closeOrArmWindow()
	r.f.loop.endWake(0)
	r.m.closeOrArm()
	r.applied(r.m.endWake())
}

// attachWake is the wake the attach timer delivers: endWake's serviceCommitRequests does its work.
func (r *windowModelRun) attachWake() {
	r.notef("attach timer")
	r.f.loop.lastPushAt = time.Time{}
	r.f.loop.attachTimer = nil
	r.f.loop.endWake(0)
	r.applied(r.m.endWake())
}

// advance steps the clock by d, firing each timer the worker armed when it comes due. A deadline the
// worker armed no timer for passes unseen, as it would in run.
func (r *windowModelRun) advance(d time.Duration) {
	r.notef("clock +%s", d)
	target := r.clock.Now().Add(d)
	for range 1000 {
		at, commit, ok := r.armed()
		if !ok || at.After(target) {
			r.clock.advance(target.Sub(r.clock.Now()))
			r.m.now = target
			return
		}
		if at.After(r.clock.Now()) {
			r.clock.advance(at.Sub(r.clock.Now()))
		}
		r.m.now = r.clock.Now()
		if commit {
			r.commitWake()
		} else {
			r.attachWake()
		}
		r.check("timer")
	}
	r.failf("a timer stayed due after its wake")
}

func (r *windowModelRun) freshValue(obj string) string {
	r.values++
	return obj + "v" + strconv.Itoa(r.values)
}

func (r *windowModelRun) event(author, obj, value string) Event {
	if value == "" {
		event := configMapTargetEvent(obj, author, echoTarget)
		event.Operation = "DELETE"
		event.Object = nil
		return event
	}
	event := echoEvent(obj, value)
	event.UserInfo.Username = author
	return event
}

// write is one accepted live write: a new value, an undo to an earlier one, the value the object
// already holds, or a delete. Each author mostly edits an object of its own, so their windows can stay
// open side by side, and sometimes any other.
func (r *windowModelRun) write(pressure bool) {
	who := r.rng.Intn(len(modelAuthors))
	author := modelAuthors[who]
	obj := modelObjects[r.rng.Intn(len(modelObjects))]
	if r.rng.Intn(3) > 0 {
		obj = modelObjects[who]
	}
	current := r.m.cluster[obj]
	value, kind := r.freshValue(obj), "write"
	switch roll := r.rng.Intn(20); {
	case roll < 4 && len(r.m.history[obj]) > 1:
		value, kind = r.m.history[obj][r.rng.Intn(len(r.m.history[obj])-1)], "undo"
	case roll < 7 && current != "":
		value, kind = current, "no-op"
	case roll < 9 && current != "":
		value, kind = "", "delete"
	}
	event := r.event(author, obj, value)
	r.notef("%s %s %s=%q pressure=%v", author, kind, obj, value, pressure)
	edit := modelEdit{obj: obj, value: value, op: event.Operation, charge: eventRetainedCharge(&event)}
	item := WorkItem{Request: &WriteRequest{Events: []Event{event}}}
	r.f.loop.lastPushAt = time.Time{}
	if pressure {
		limit := r.f.worker.branchBufferMaxBytes
		r.f.worker.branchBufferMaxBytes = 1
		r.f.loop.dispatch(item)
		r.f.worker.branchBufferMaxBytes = limit
	} else {
		r.f.loop.dispatch(item)
	}
	r.f.loop.endWake(0)
	r.m.write(author, edit, pressure)
	r.applied(r.m.endWake())
}

func (r *windowModelRun) save() {
	r.saves++
	author := modelAuthors[r.rng.Intn(len(modelAuthors))]
	name := "save-" + strconv.Itoa(r.saves)
	req := &AttachCommitRequest{
		Namespace: "default", Name: name, UID: "uid-" + name,
		Author: author, GitTargetName: echoTarget, GitTargetNamespace: "default",
		Message: fmt.Sprintf("feat: %s by %s", name, author),
		Attach:  configv1alpha3.AttachCurrentOrNext,
	}
	if r.rng.Intn(4) == 0 {
		req.Attach = configv1alpha3.AttachNext
	}
	req.AttachTimeout = []time.Duration{0, 5 * time.Second, 20 * time.Second}[r.rng.Intn(3)]
	req.MaxDuration = []time.Duration{0, 15 * time.Second, 40 * time.Second}[r.rng.Intn(3)]
	if r.rng.Intn(2) == 0 {
		req.IdleTimeout = durationPtr([]time.Duration{0, 5 * time.Second}[r.rng.Intn(2)])
	}
	r.notef("%s registers %s attach=%s attachTimeout=%s maxDuration=%s idle=%v", author, name, req.Attach,
		req.AttachTimeout, req.MaxDuration, req.IdleTimeout)
	s := &modelSave{name: name, author: author, message: req.Message, next: req.Attach == configv1alpha3.AttachNext,
		idle: req.IdleTimeout, maxDuration: req.MaxDuration}
	r.wake(&WorkItem{Attach: req}, func() { r.m.register(s, req.AttachTimeout) })
}

// foreign pushes a commit from outside and reports it, as a Git host's push notice does.
func (r *windowModelRun) foreign() {
	obj := modelObjects[r.rng.Intn(len(modelObjects))]
	r.values++
	value := "outside" + strconv.Itoa(r.values)
	r.notef("outside push %s=%q, hinted", obj, value)
	path := r.m.remote.setOutside(obj, value)
	r.pushFromOutside(r.m.remote, []string{path}, "outside: "+obj+"="+value)
	r.m.foreignUnseen = true
	r.f.loop.pushHinted = true
	r.wake(nil, func() { r.m.hinted = true })
}

// apply makes an apply due for the folder's applier (a Repair).
func (r *windowModelRun) apply() {
	r.notef("apply due")
	r.f.echoApplier().repair = true
	r.wake(nil, func() { r.m.applyDue = true })
}

func (r *windowModelRun) resync() {
	r.notef("resync")
	var desired []manifestanalyzer.DesiredResource
	for _, obj := range modelObjects {
		if value := r.m.cluster[obj]; value != "" {
			desired = append(desired, echoDesired(obj, value))
		}
	}
	req := &ResyncRequest{Desired: desired, GitTargetName: echoTarget, GitTargetNamespace: "default",
		Result: make(chan ResyncResult, 1)}
	item := resyncItem(req)
	r.wake(&item, r.m.resync)
	if result := <-req.Result; result.Err != nil {
		r.failf("resync: %v", result.Err)
	}
}

func (r *windowModelRun) shutdown() {
	r.notef("shutdown")
	r.f.loop.lastPushAt = time.Time{}
	r.f.loop.handleShutdown()
	r.f.worker.settleCommitRequestsAfterExit()
	r.exited = true
	r.m.shutdown()
}

func (r *windowModelRun) step() {
	switch roll := r.rng.Intn(100); {
	case roll < 40:
		r.write(false)
	case roll < 44:
		r.write(true)
	case roll < 62:
		r.advance(time.Duration(1+r.rng.Intn(25)) * time.Second)
	case roll < 76:
		r.save()
	case roll < 83:
		r.foreign()
	case roll < 90:
		r.apply()
	case roll < 96 && !r.m.foreignUnseen && !r.m.applyDue:
		r.resync()
	default:
		r.write(false)
	}
}

func (r *windowModelRun) run(steps int) {
	for range steps {
		r.step()
		r.check("step")
		if len(r.m.windows) > 1 {
			r.concurrent++
		}
	}
	if r.rng.Intn(2) == 0 {
		r.shutdown()
		r.check("shutdown")
	} else {
		r.advance(3 * modelMax)
	}
	r.drained()
}

var closeLine = regexp.MustCompile(`"msg"="Finalizing open commit window" "reason"="([^"]+)" ` +
	`"windowAuthor"="([^"]*)" "windowTarget"="[^"]*" "window"=(\d+)`)

// check compares the worker with the model after a step.
func (r *windowModelRun) check(what string) {
	r.t.Helper()
	r.checkWindows(what)
	r.checkTimers(what)
	r.checkSaves(what)
	r.checkCloses(what)
	r.checkCommits(what)
	if lines := r.logs.matching(decisionSingleWindowFallback); len(lines) > 0 {
		r.failf("%s: the worker refused a commit outside its footprint: %s", what, lines[0])
	}
}

// checkWindows compares the open windows and what is retained.
func (r *windowModelRun) checkWindows(what string) {
	r.t.Helper()
	loop, m := r.f.loop, r.m
	if got, want := r.workerWindows(), m.describeWindows(); got != want {
		r.failf("%s: the open windows differ:\n  worker %s\n  model  %s", what, got, want)
	}
	if got, want := loop.totalRetainedBytes(), m.windowCharge(); got != want || loop.windows.bytes() != want {
		r.failf("%s: retained %d (windows %d), the model's window charge is %d", what, got,
			loop.windows.bytes(), want)
	}
	if len(loop.pendingWrites) != 0 {
		r.failf("%s: %d writes still pending after a wake that pushed at once", what, len(loop.pendingWrites))
	}
}

// checkTimers compares the timers the worker has armed with the model's deadlines: the commit timer
// for the earliest close of an open window, the attach timer for the earliest deadline of a save still
// waiting.
func (r *windowModelRun) checkTimers(what string) {
	r.t.Helper()
	if r.exited {
		return
	}
	loop, m := r.f.loop, r.m
	var commitAt, attachAt time.Time
	for _, w := range m.windows {
		if at, _ := w.closeAt(); commitAt.IsZero() || at.Before(commitAt) {
			commitAt = at
		}
	}
	for _, s := range m.saves {
		if !s.attached && (attachAt.IsZero() || s.deadline.Before(attachAt)) {
			attachAt = s.deadline
		}
	}
	got := r.describeTimer(loop.commitTimer != nil, loop.commitTimerAt) + ", " +
		r.describeTimer(loop.attachTimer != nil, loop.attachTimerAt)
	want := r.describeTimer(!commitAt.IsZero(), commitAt) + ", " + r.describeTimer(!attachAt.IsZero(), attachAt)
	if got != want {
		r.failf("%s: the armed timers (commit, attach) differ:\n  worker %s\n  model  %s", what, got, want)
	}
}

func (r *windowModelRun) describeTimer(armed bool, at time.Time) string {
	if !armed {
		return "none"
	}
	return "in " + at.Sub(r.m.now).String()
}

// workerWindows describes the worker's open windows as describeWindows does the model's. An unknown
// footprint's reason is the worker's to word.
func (r *windowModelRun) workerWindows() string {
	var out []string
	for _, w := range r.f.loop.windows.all() {
		saved := ""
		if w.pendingCR != nil {
			saved = w.pendingCR.Name
		}
		footprint := footprintString(w.footprint)
		if w.footprint != nil && !w.footprint.known() {
			footprint = "unknown"
		}
		at, reason := w.closeAt()
		out = append(out, fmt.Sprintf("#%d %s objs=%d save=%q closes in %s (%s) charge %d footprint %s", w.seq,
			w.Author, len(w.pathOrder), saved, at.Sub(r.m.now), reason, w.bytes, footprint))
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, "; ")
}

// checkSaves compares the registered saves, and every save's outcome and how often it settled.
func (r *windowModelRun) checkSaves(what string) {
	r.t.Helper()
	loop, m := r.f.loop, r.m
	var registered, modelRegistered []string
	for id, pcr := range loop.pendingCRs {
		registered = append(registered, fmt.Sprintf("%s attached=%v deadline=%s", id.Name, pcr.attached,
			pcr.attachDeadline.Sub(m.now)))
	}
	for name, s := range m.saves {
		modelRegistered = append(modelRegistered, fmt.Sprintf("%s attached=%v deadline=%s", name, s.attached,
			s.deadline.Sub(m.now)))
	}
	slices.Sort(registered)
	slices.Sort(modelRegistered)
	// An exited loop keeps what it held; settleCommitRequestsAfterExit gave the waiting saves back.
	if !r.exited && !slices.Equal(registered, modelRegistered) {
		r.failf("%s: registered saves differ:\n  worker %v\n  model  %v", what, registered, modelRegistered)
	}
	for i := 1; i <= r.saves; i++ {
		name := "save-" + strconv.Itoa(i)
		res, resolved := r.f.worker.LookupCommitRequestOutcome("default", name, "uid-"+name)
		want, modelResolved := m.outcomes[name]
		if resolved != modelResolved || (resolved && (res.Outcome != want || res.Err != nil)) {
			r.failf("%s: %s settled %v as %q (err %v); the model says %v as %q", what, name, resolved,
				res.Outcome, res.Err, modelResolved, want)
		}
		if n := r.settlements(name); n > 1 || (n == 1) != modelResolved {
			r.failf("%s: %s settled %d times", what, name, n)
		}
	}
}

// checkCloses compares the windows closed so far, by reason, author and number, in order.
func (r *windowModelRun) checkCloses(what string) {
	r.t.Helper()
	var closes []string
	for _, line := range r.logs.matching("Finalizing open commit window") {
		match := closeLine.FindStringSubmatch(line)
		if match == nil {
			r.failf("unparsed close line %s", line)
		}
		closes = append(closes, match[1]+" "+match[2]+" #"+match[3])
	}
	if !slices.Equal(closes, r.m.closes) {
		r.failf("%s: close reasons differ:\n  worker %v\n  model  %v", what, closes, r.m.closes)
	}
}

// settlements counts the times a save was settled: resolved, or refused at registration.
func (r *windowModelRun) settlements(name string) int {
	n := 0
	for _, line := range r.logs.matching(`"request"="default/` + name + `"`) {
		if strings.Contains(line, `"msg"="CommitRequest resolved"`) ||
			strings.Contains(line, `"msg"="CommitRequest refused`) {
			n++
		}
	}
	return n
}

// checkCommits compares the commits published since the last check with the model's.
func (r *windowModelRun) checkCommits(what string) {
	r.t.Helper()
	var published []string
	for _, line := range splitLines(gitIn(r.t, r.f.repoDir, "log", "--reverse", "--first-parent",
		"--format=%H %an", r.base+"..main")) {
		hash, author, _ := strings.Cut(line, " ")
		if author != "Outside" {
			published = append(published, hash)
		}
	}
	if len(published) != len(r.m.commits) {
		r.failf("%s: the worker published %d commits; the model %d", what, len(published), len(r.m.commits))
	}
	for i := r.checked; i < len(published); i++ {
		r.checkCommit(what, published[i], r.m.commits[i])
	}
	r.checked = len(published)
}

func (r *windowModelRun) checkCommit(what, hash string, want modelCommit) {
	r.t.Helper()
	author := gitIn(r.t, r.f.repoDir, "log", "-1", "--format=%an", hash)
	message := gitIn(r.t, r.f.repoDir, "log", "-1", "--format=%B", hash)
	if author != want.author || (!want.resync && strings.TrimSpace(message) != strings.TrimSpace(want.message)) {
		r.failf("%s: commit %s by %q %q; the model's by %q %q", what, hash[:8], author, message, want.author,
			want.message)
	}
	paths := splitLines(gitIn(r.t, r.f.repoDir, "diff-tree", "-r", "--name-only", "--no-commit-id", hash))
	wantPaths := slices.Clone(want.paths)
	if want.bootstrap {
		for _, name := range modelBootstrap {
			wantPaths = append(wantPaths, echoTarget+"/"+name)
			r.checkBootstrap(what, hash, name)
		}
	}
	slices.Sort(paths)
	slices.Sort(wantPaths)
	if !slices.Equal(paths, wantPaths) {
		r.failf("%s: commit %s by %s changes %v; the model's changes %v", what, hash[:8], author, paths, wantPaths)
	}
	if f := want.footprint; f != nil {
		for _, path := range paths {
			if !slices.Contains(f.paths, path) {
				r.failf("%s: commit %s by %s changes %s, outside its window's footprint %s", what, hash[:8],
					author, path, f)
			}
		}
	}
	for path, file := range want.files {
		r.checkFile(what, hash, path, file)
	}
	for obj, seq := range want.seqs {
		if seq <= r.m.landed[obj] {
			r.failf("%s: commit %s lands write %d of %s after write %d", what, hash[:8], seq, obj, r.m.landed[obj])
		}
		r.m.landed[obj] = seq
	}
}

// checkFile compares the file at path in rev, parsed, with what the model says it holds: the
// kustomization's resources: entries, or each document whole, in order. YAML 1.2, so a bare y is the
// name y.
func (r *windowModelRun) checkFile(what, rev, path string, want modelFile) {
	r.t.Helper()
	var content []byte
	if gitIn(r.t, r.f.repoDir, "ls-tree", "--name-only", rev, path) != "" {
		content = []byte(gitIn(r.t, r.f.repoDir, "show", rev+":"+path))
	}
	docs, err := parseDocuments(content)
	if err != nil {
		r.failf("%s: %s at %s: %v", what, path, rev, err)
	}
	if want.kustomization {
		var entries []string
		if len(docs) == 1 {
			for _, entry := range asSlice(docs[0]["resources"]) {
				entries = append(entries, fmt.Sprint(entry))
			}
		}
		if len(docs) != 1 || !slices.Equal(entries, want.entries) {
			r.failf("%s: %s at %s lists %v; the model's %v", what, path, rev, entries, want.entries)
		}
		return
	}
	var wantDocs []map[string]any
	for _, doc := range want.docs {
		wantDocs = append(wantDocs, map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": doc[0], "namespace": "default"},
			"data":     map[string]any{"key": doc[1]},
		})
	}
	if !reflect.DeepEqual(docs, wantDocs) {
		r.failf("%s: %s at %s is %v; the model's is %v", what, path, rev, docs, wantDocs)
	}
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// parseDocuments parses every document of a YAML stream with yaml.v3.
func parseDocuments(content []byte) ([]map[string]any, error) {
	var docs []map[string]any
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}
}

// checkBootstrap compares the bootstrap file name at rev with the worker's template.
func (r *windowModelRun) checkBootstrap(what, rev, name string) {
	r.t.Helper()
	want, err := readBootstrapTemplateContent(name, pathBootstrapOptions{Enabled: true})
	if err != nil {
		r.failf("%s: %v", what, err)
	}
	if got := gitIn(r.t, r.f.repoDir, "show", rev+":"+echoTarget+"/"+name); got != strings.TrimSpace(string(want)) {
		r.failf("%s: %s at %s is not the template's:\n%s", what, name, rev, got)
	}
}

// drained checks the worker once nothing is open, waiting or pending.
func (r *windowModelRun) drained() {
	r.t.Helper()
	r.check("drained")
	if len(r.m.windows) != 0 || len(r.m.saves) != 0 || len(r.m.pending) != 0 {
		r.failf("the model is not drained")
	}
	// An exited loop is dropped with the saves it gave back still counted in heldBytes; what it retains
	// for Git must be nothing.
	held := r.f.loop.heldBytes()
	if r.exited {
		held = 0
	}
	if held != 0 || r.f.loop.totalRetainedBytes() != 0 {
		r.failf("drained, the worker still charges %d (retained %d)", held, r.f.loop.totalRetainedBytes())
	}
	r.checkTree()
	for name := range r.m.given {
		if _, resolved := r.f.worker.LookupCommitRequestOutcome("default", name, "uid-"+name); resolved {
			r.failf("%s was given back at shutdown, and has an outcome", name)
		}
		if phase := r.f.worker.LookupCommitRequestPhase("default", name, "uid-"+name); phase != "" {
			r.failf("%s was given back at shutdown, and is still %s", name, phase)
		}
	}
}

// checkTree checks that Git is the base tree outside the folder, and in it the model's files and,
// once the folder is bootstrapped, the template's: nothing else was added, changed or removed.
func (r *windowModelRun) checkTree() {
	r.t.Helper()
	var want []string
	for _, path := range splitLines(gitIn(r.t, r.f.repoDir, "ls-tree", "-r", "--name-only", r.base)) {
		if !strings.HasPrefix(path, echoTarget+"/") {
			want = append(want, path)
		}
	}
	want = append(want, modelKustFile)
	r.checkFile("drained", "main", modelKustFile, r.m.remote.file(modelKustFile))
	for path := range r.m.remote.files {
		want = append(want, path)
		r.checkFile("drained", "main", path, r.m.remote.file(path))
	}
	if r.m.bootstrapped {
		for _, name := range modelBootstrap {
			want = append(want, echoTarget+"/"+name)
			r.checkBootstrap("drained", "main", name)
		}
	}
	slices.Sort(want)
	if tree := splitLines(gitIn(r.t, r.f.repoDir, "ls-tree", "-r", "--name-only", "main")); !slices.Equal(tree, want) {
		r.failf("drained, Git holds %v; the model %v", tree, want)
	}
	others := []string{"diff", "--name-only", r.base, "main", "--", ".", ":(exclude)" + echoTarget}
	if changed := gitIn(r.t, r.f.repoDir, others...); changed != "" {
		r.failf("drained, Git changed the base's %s", changed)
	}
}

func envInt(name string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return fallback
}

// TestWindowModel_TheWorkerFollowsTheReferenceModel runs random input sequences through a worker and
// the model, and checks every step. Across the seeds, some steps must end with several windows open,
// or the search says nothing about them.
func TestWindowModel_TheWorkerFollowsTheReferenceModel(t *testing.T) {
	seeds, steps := envInt("WINDOW_MODEL_SEEDS", 6), envInt("WINDOW_MODEL_STEPS", 40)
	if testing.Short() {
		seeds = 2
	}
	first := int64(1)
	if seed := envInt("WINDOW_MODEL_SEED", 0); seed > 0 {
		first, seeds = int64(seed), 1
	}
	concurrent := 0
	for seed := first; seed < first+int64(seeds); seed++ {
		t.Run("seed-"+strconv.FormatInt(seed, 10), func(t *testing.T) {
			run := newWindowModelRun(t, seed)
			run.run(steps)
			concurrent += run.concurrent
		})
	}
	t.Logf("%d steps ended with several windows open", concurrent)
	if seeds > 1 && !t.Failed() {
		require.Positive(t, concurrent, "no step ended with several windows open")
	}
}
