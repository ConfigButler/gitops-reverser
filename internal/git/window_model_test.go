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

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
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
//     attached save, deadline, charge and footprint), the pending writes in order (resync or not,
//     the save each carries, its charge, committed or not), the timers (commit, attach, push cooldown
//     and retry), the registered saves (attached or waiting, and until when), the retained-byte
//     charge, the close reasons so far, and every save's outcome are the model's.
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
// Work is kept across wakes, as the worker keeps it: a decided write is committed at the end of its
// wake unless it needs a connection while a retry waits, and published by a push the cooldown spaces,
// or at once for a save, a flush and the first push. The Git server can be taken down and brought
// back (injectOutage): a failed push keeps every write for the retry, which backs off from 10s to 5m.
//
// The model shares no window code with the worker: only the event fixtures, the per-event charge
// (eventRetainedCharge, attachCharge, a resync's charge, the pending-write overhead) and the
// close-reason names. The worker's timers are real; the driver steps the worker's clock to the time
// each armed timer is due and delivers that timer's wake, so a deadline the worker armed no timer for
// is never delivered.
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
// its built-in path, unregistered, after any document a restructuring left in that file.
func (t *modelTree) setOutside(obj, value string) string {
	at := t.loc(obj)
	if at == "" {
		at = echoFile(obj)
		t.files[at] = append(t.files[at], obj)
	}
	t.values[obj] = value
	return at
}

// merge is a restructuring from another clone: obj's document moves into the file into's document is
// alone in, after it. It returns the paths it changes.
func (t *modelTree) merge(into, obj string) []string {
	from, to := t.loc(obj), t.loc(into)
	delete(t.files, from)
	t.files[to] = append(t.files[to], obj)
	return []string{to, from}
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

// modelOutcome is how a save settled: with an outcome, or failed.
type modelOutcome struct {
	outcome FinalizeOutcome
	failed  bool
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

// modelPending is a decided write a later push publishes. footprint is the proven footprint of the
// window it closed, when that window had company. decision numbers it in the order it was decided,
// charge is what keeping it costs, and committed says the checkout holds it. A write that changes
// nothing and carries no save stays pending until it is committed, and then leaves no trace.
type modelPending struct {
	save      *modelSave
	author    string
	message   string
	resync    bool
	changes   []modelEdit
	footprint *modelFootprint
	decision  int
	charge    int64
	committed bool
	// creates is a resync that adds an object to Git: committed, its applier is due. changesTree is a
	// write that changes the tree it commits on, so its commit adds the folder's bootstrap files: a save
	// that changes nothing commits nothing.
	creates, changesTree bool
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
	// bootstrapped is whether Git holds modelBootstrap, and bootstrapLocal whether the checkout does.
	bootstrapped, bootstrapLocal bool
	// surprise names a state the model does not cover, which the run reports.
	surprise string
	// reach counts the situations the search must reach (reachFloors).
	reach map[string]int
	// applied is what the last apply put on the cluster, and withheld the values of applier writes kept
	// out of Git, by object.
	applied, withheld map[string]string

	// The publication. lastPushAt is the last push that landed, pushAt the cooldown's timer while it
	// is armed, and retryAt and backoff the retry schedule while a failed attempt waits for its next.
	lastPushAt, pushAt, retryAt time.Time
	backoff                     time.Duration
	// down is an outage: every push, fetch and sync fails. untrusted is a base the worker cannot vouch
	// for since a push failed: a commit with nothing committed before it then needs a fetch.
	down, untrusted bool
	// flushing is an apply that waits for the writes decided up to flushMark to be published.
	flushing             bool
	flushMark, decisions int

	pending  []modelPending
	commits  []modelCommit
	closes   []string
	outcomes map[string]modelOutcome
	given    map[string]bool
	// landed is, per document, the acceptance seq of the last write a commit landed.
	landed map[string]int
}

// The situations the search must reach, across its seeds, for a pass to say anything about them.
const (
	reachShutdown     = "a shutdown with several windows open"
	reachFlush        = "an apply's flush with several windows open"
	reachHint         = "a push hint with several windows open"
	reachAttachNext   = "attach: Next beside another author's window"
	reachReproof      = "a reproof that finds open windows overlap"
	reachPathConflict = "a path_conflict close"
	reachFailedPush   = "a failed push with several windows open"
	reachCooldown     = "the cooldown holding writes with several windows open"
	reachRetryWait    = "a retry waiting with several windows open"
	reachDeferred     = "a decided write waiting for its retry to be committed"
	reachReplay       = "a replay onto a moved remote with a window open"
	reachPressure     = "buffer pressure with several windows open"
	reachApplyWaits   = "an apply's flush waiting for the retry"
	reachRestart      = "a restart mid-run"
	reachOverdue      = "an input handled while a timer is overdue"
	reachStaleApply   = "an applier write over a newer edit, withheld"
)

func (m *windowModel) reached(what string) { m.reach[what]++ }

// several reports whether more than one window is open.
func (m *windowModel) several() bool { return len(m.windows) > 1 }

func newWindowModel(now time.Time, tree modelTree) *windowModel {
	m := &windowModel{
		now: now, saves: map[string]*modelSave{},
		remote: tree.clone(), known: tree.clone(), cluster: maps.Clone(tree.values),
		history: map[string][]string{}, outcomes: map[string]modelOutcome{}, given: map[string]bool{},
		landed: map[string]int{}, reach: map[string]int{}, withheld: map[string]string{},
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

// prove is the footprint of a write of obj admitted now: proven on the tree the checkout holds, once
// it holds every decided write, and unknown while a decided write waits for a connection to commit.
func (m *windowModel) prove(obj string) modelFootprint {
	if !m.holdsDecided() {
		return modelFootprint{unknown: true}
	}
	return m.proveOn(obj)
}

// holdsDecided reports whether the checkout holds every decided write.
func (m *windowModel) holdsDecided() bool {
	for _, p := range m.pending {
		if !p.committed {
			return false
		}
	}
	return true
}

// proveOn is the footprint of a write of obj on the tree that holds every decided write. Before the
// folder is bootstrapped every write adds its bootstrap files.
func (m *windowModel) proveOn(obj string) modelFootprint {
	at := m.known.loc(obj)
	switch {
	case !m.bootstrapLocal, at == "", strings.HasPrefix(at, modelKustDir+"/"), len(m.known.files[at]) > 1:
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

// proveCommit is the footprint a closing window's commit is checked against: proven again on the tree
// the commit is planned on, which holds every write decided before it.
func (m *windowModel) proveCommit(w *modelWindow) modelFootprint {
	f := modelFootprint{}
	for _, obj := range w.objs {
		f = f.union(m.proveOn(obj))
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
			m.reached(reachReproof)
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
		if m.several() {
			m.reached(reachPressure)
		}
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
			m.outcomes[s.name] = modelOutcome{outcome: FinalizeSaveInProgress}
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
	m.outcomes[s.name] = modelOutcome{outcome: outcome}
}

// close decides a window: the documents whose latest write differs from the checkout, in the order the
// window first saw them. A window that changes nothing and carries no save leaves no trace. A save is
// never trimmed: it decides every document it captured, so a replay onto a push the worker had not
// seen re-asserts them (API wins for a save). A window once proven carries its footprint, proven again
// on the checkout it commits on.
func (m *windowModel) close(w *modelWindow, reason windowFinalizeReason) {
	switch {
	case reason == windowFinalizeReasonPathConflict:
		m.reached(reachPathConflict)
	case reason == windowFinalizeReasonAttachNext && m.several():
		m.reached(reachAttachNext)
	}
	m.windows = slices.DeleteFunc(m.windows, func(o *modelWindow) bool { return o == w })
	m.closes = append(m.closes, fmt.Sprintf("%s %s #%d", reason, w.author, w.seq))
	var footprint *modelFootprint
	if w.footprint != nil {
		if f := m.proveCommit(w); !f.unknown {
			footprint = &f
		}
	}
	var changes []modelEdit
	changesTree := false
	for _, obj := range w.objs {
		if edit := w.latest[obj]; edit.value != m.known.values[obj] || w.save != nil {
			changes = append(changes, edit)
			changesTree = changesTree || edit.value != m.known.values[obj]
			m.known.set(obj, edit.value)
		}
	}
	// The write keeps the window's charge, less the save's own item: its message is the write's.
	p := modelPending{save: w.save, author: w.author, changes: changes, footprint: footprint, charge: w.charge,
		changesTree: changesTree}
	if w.save != nil {
		p.message = w.save.message
		p.charge -= pendingWriteOverheadBytes
	} else {
		p.message = generatedMessage(changes)
	}
	m.decide(p)
}

// decide appends a decided write; advance commits it at the end of the wake.
func (m *windowModel) decide(p modelPending) {
	m.decisions++
	p.decision = m.decisions
	m.pending = append(m.pending, p)
}

func (m *windowModel) pendingCharge() int64 {
	var charge int64
	for _, p := range m.pending {
		charge += p.charge
	}
	return charge
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

// The model's publication rules: a decided write is committed at the end of the wake that decided it,
// unless it needs a connection while a failed attempt waits for its retry, and published by a push
// the cooldown spaces after the last one that landed. A push that fails keeps every write for a retry,
// one backoff later, 10s doubling to 5m, whatever arrives meanwhile.
const (
	modelCooldown   = 5 * time.Second
	modelRetryFirst = 10 * time.Second
	modelRetryMax   = 5 * time.Minute
)

func (m *windowModel) awaitingRetry() bool { return !m.retryAt.IsZero() && m.now.Before(m.retryAt) }

func (m *windowModel) scheduleRetry() {
	if m.backoff == 0 {
		m.backoff = modelRetryFirst
	} else {
		m.backoff = min(2*m.backoff, modelRetryMax)
	}
	m.retryAt = m.now.Add(m.backoff)
}

func (m *windowModel) endOutage() { m.retryAt, m.backoff = time.Time{}, 0 }

// pendingUpTo reports whether a write decided up to mark is still pending.
func (m *windowModel) pendingUpTo(mark int) bool {
	return slices.ContainsFunc(m.pending, func(p modelPending) bool { return p.decision <= mark })
}

func (m *windowModel) carriesSave() bool {
	return slices.ContainsFunc(m.pending, func(p modelPending) bool { return p.save != nil })
}

// commitIsLocal reports whether committing the decided writes the checkout does not hold needs no
// connection: none is a resync, and the checkout holds a write before them or sits on a base the
// worker can vouch for.
func (m *windowModel) commitIsLocal() bool {
	committed := false
	for _, p := range m.pending {
		if p.committed {
			committed = true
		} else if p.resync {
			return false
		}
	}
	return committed || !m.untrusted
}

// materialize commits, in order, every decided write the checkout does not hold, and reports whether
// it could: a resync fetches first, and so does a write with nothing committed before it on a base
// the worker cannot vouch for. A write that commits nothing and carries no save leaves the pending
// writes once it is committed; a resync that adds an object makes its applier due.
func (m *windowModel) materialize() bool {
	for i := 0; i < len(m.pending); {
		p := &m.pending[i]
		if p.committed {
			i++
			continue
		}
		if (p.resync || (i == 0 && m.untrusted)) && !m.fetch() {
			return false
		}
		p.committed = true
		m.bootstrapLocal = m.bootstrapLocal || p.changesTree
		m.applyDue = m.applyDue || (p.resync && p.creates)
		if len(p.changes) == 0 && p.save == nil {
			m.pending = slices.Delete(m.pending, i, i+1)
			continue
		}
		i++
	}
	return true
}

// fetch is the fetch a commit needs first, and reports whether it could be made.
func (m *windowModel) fetch() bool {
	if m.down {
		return false
	}
	if m.foreignUnseen {
		m.surprise = "a fetch before a commit would find the push from outside"
	}
	m.untrusted = false
	return true
}

// commitDecided commits what the wake decided. While a failed attempt waits for its retry, a commit
// that needs a connection waits for that retry too.
func (m *windowModel) commitDecided() {
	if m.holdsDecided() || (m.awaitingRetry() && !m.commitIsLocal()) {
		return
	}
	if !m.materialize() && m.retryAt.IsZero() {
		m.scheduleRetry()
	}
}

// advance is the end of every wake: commit what was decided, and push it, at once or when the
// cooldown ends; with nothing pending, the outage is over.
func (m *windowModel) advance() {
	for range 100 {
		m.commitDecided()
		if len(m.pending) == 0 {
			m.pushAt = time.Time{}
			m.endOutage()
			return
		}
		m.maybePush()
		if !m.retryAt.IsZero() || m.holdsDecided() {
			return
		}
	}
	m.surprise = "advance made no progress"
}

// maybePush pushes at once the first time, for an apply waiting on its flush, and for a save; any
// other push waits for the cooldown since the last one that landed. While a retry waits, nothing
// pushes before it.
func (m *windowModel) maybePush() {
	if len(m.pending) == 0 || m.awaitingRetry() {
		return
	}
	if m.lastPushAt.IsZero() || (m.flushing && m.pendingUpTo(m.flushMark)) || m.carriesSave() ||
		!m.now.Before(m.lastPushAt.Add(modelCooldown)) {
		m.push()
		return
	}
	if m.pushAt.IsZero() {
		m.pushAt = m.lastPushAt.Add(modelCooldown)
	}
}

// push commits what is decided and publishes all of it, or keeps it all for the retry.
func (m *windowModel) push() {
	m.pushAt = time.Time{}
	if len(m.pending) == 0 {
		return
	}
	if !m.materialize() {
		m.scheduleRetry()
		return
	}
	if len(m.pending) == 0 {
		return
	}
	if m.down {
		if m.several() {
			m.reached(reachFailedPush)
		}
		m.untrusted = true
		m.scheduleRetry()
		return
	}
	if m.foreignUnseen && len(m.windows) > 0 {
		m.reached(reachReplay)
	}
	m.publish()
	m.lastPushAt, m.untrusted = m.now, false
	m.endOutage()
}

// publish publishes the pending writes. A remote that moved behind the worker's back is found by the
// push, which replays the decided changes onto it, and every applier is due for the foreign commit.
// The footprint of a replayed write was proven on another tree, so it holds nothing about the commit.
func (m *windowModel) publish() {
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
	m.bootstrapLocal = m.bootstrapped
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
		if m.several() {
			m.reached(reachHint)
		}
		for _, w := range slices.Clone(m.windows) {
			if w.save == nil {
				m.close(w, windowFinalizeReasonForeignCommit)
			}
		}
	}
	m.advance()
	return m.serviceAppliers()
}

// serviceAppliers sends a due apply once no save is registered: the open windows close and the writes
// decided so far are published first, at once, cooldown or not. While a retry waits, the apply waits
// for it; windows opened meanwhile stay open. A resync not published yet holds the applier back
// (the barrier), and ends the flush: the next one closes the windows again.
func (m *windowModel) serviceAppliers() bool {
	if !m.applyDue || slices.ContainsFunc(m.pending, func(p modelPending) bool { return p.resync }) {
		m.flushing = false
		return false
	}
	if len(m.saves) > 0 {
		return false
	}
	if !m.flushing {
		if m.several() {
			m.reached(reachFlush)
		}
		m.closeAll(windowFinalizeReasonApplyDue)
		m.flushing, m.flushMark = true, m.decisions
		if m.pendingUpTo(m.flushMark) {
			m.advance()
		}
	}
	if m.pendingUpTo(m.flushMark) {
		if !m.retryAt.IsZero() {
			m.reached(reachApplyWaits)
		}
		return false
	}
	m.flushing, m.applyDue = false, false
	// The apply is of the tip the worker knows: a push it has not found yet is not in it.
	m.cluster = maps.Clone(m.known.values)
	m.applied = maps.Clone(m.known.values)
	return true
}

// resync closes the windows, then decides, as the operator, every live object Git does not hold as it
// is, charged what its snapshot is. prune.mode is onEvent, so nothing is swept. An object it adds is
// adopted once it is committed: an apply is due.
func (m *windowModel) resync(charge int64) {
	m.closeAll(windowFinalizeReasonResyncBeforeApply)
	p := modelPending{author: DefaultCommitterName, resync: true, charge: charge}
	for _, obj := range modelObjects {
		value := m.cluster[obj]
		if value == "" || value == m.known.values[obj] {
			continue
		}
		p.creates = p.creates || m.known.values[obj] == ""
		m.known.set(obj, value)
		// What the snapshot found on the cluster is the newest write of the object.
		m.editSeq++
		p.changes = append(p.changes, modelEdit{seq: m.editSeq, obj: obj, value: value})
		p.changesTree = true
	}
	m.decide(p)
}

// restart is a new worker after a shutdown: a checkout cloned from Git, so it knows what Git holds, and
// no window, save, pending write or schedule. Window numbers start again.
func (m *windowModel) restart() {
	m.pending, m.saves, m.given = nil, map[string]*modelSave{}, map[string]bool{}
	m.known, m.bootstrapLocal = m.remote.clone(), m.bootstrapped
	m.foreignUnseen, m.applyDue, m.hinted, m.flushing, m.untrusted = false, false, false, false, false
	m.lastPushAt, m.pushAt = time.Time{}, time.Time{}
	m.endOutage()
	m.seq = 0
}

// shutdown closes the windows and pushes, cooldown and retry or not. A save riding a write the push
// could not publish fails; a save still waiting for a window is given back.
func (m *windowModel) shutdown() {
	if m.several() {
		m.reached(reachShutdown)
	}
	m.closeAll(windowFinalizeReasonShutdown)
	m.push()
	for _, p := range m.pending {
		if p.save != nil {
			delete(m.saves, p.save.name)
			m.outcomes[p.save.name] = modelOutcome{failed: true}
		}
	}
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
	// outage takes the Git server down and brings it back.
	outage  *remoteOutage
	profile windowModelProfile
	// concurrent is how many steps ended with more than one window open.
	concurrent int
	// firstSave is the first save the current worker was sent: a restart leaves the earlier ones with
	// the worker before it.
	firstSave int
	// sent records that an apply was sent since it was last cleared.
	sent bool
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
	r := &windowModelRun{t: t, seed: seed, f: f, requester: requester, rng: rand.New(rand.NewSource(seed)),
		outage: injectOutage(t), firstSave: 1}
	r.profile = []windowModelProfile{outageProfile, wideProfile, concurrentProfile}[seed%3]
	r.clock = useTestClock(f.worker)
	r.logs = captureInfo(f.worker)
	f.loop.defaultWindow = commitWindowDefaults{idleTimeout: modelIdle, maxDuration: modelMax}
	r.base = gitIn(t, f.repoDir, "rev-parse", "main")
	r.knownTip = r.base
	r.start = r.clock.Now()
	r.m = newWindowModel(r.start, tree)
	r.m.bootstrapped = gitIn(t, f.repoDir, "ls-tree", "--name-only", r.base, echoTarget+"/README.md") != ""
	r.m.bootstrapLocal = r.m.bootstrapped
	return r
}

// pushFromOutside commits the files at paths, as tree holds them, from the outside clone: a path the
// tree holds no file at is removed.
func (r *windowModelRun) pushFromOutside(tree modelTree, paths []string, message string) {
	r.t.Helper()
	clone := r.f.clone
	gitIn(r.t, clone, "pull", "--quiet", "--ff-only", "origin", "main")
	for _, path := range paths {
		if _, held := tree.files[path]; !held && path != modelKustFile {
			gitIn(r.t, clone, "rm", "--quiet", path)
			continue
		}
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

// wake runs one wake of the worker's loop on item, and the model's matching wake; then answers an apply
// sent, as the applier, and settles it in a wake of both.
func (r *windowModelRun) wake(item *WorkItem, model func()) {
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
	r.sent = r.sent || modelSent
	if modelSent != (len(sent) > 0) {
		r.failf("the model sent an apply: %v; the worker sent %d", modelSent, len(sent))
	}
	for _, req := range sent {
		if req.Commit != r.knownTip {
			r.failf("an apply was sent for %s, not the tip the worker knows, %s", req.Commit, r.knownTip)
		}
		req.Report(ApplyResult{ID: req.ID, Outcome: ApplySucceeded, AppliedRevision: req.Commit})
	}
	r.f.loop.endWake(0)
	if r.m.endWake() {
		r.failf("the model sent a second apply in the settling wake")
	}
}

// modelTimer names one of the worker's timers the driver delivers.
type modelTimer string

const (
	timerCommit modelTimer = "commit"
	timerAttach modelTimer = "attach"
	timerPush   modelTimer = "push"
	timerRetry  modelTimer = "retry"
)

// armedTimers is every timer the worker has armed, and when it is due on the worker's clock.
func (r *windowModelRun) armedTimers() map[modelTimer]time.Time {
	loop := r.f.loop
	armed := make(map[modelTimer]time.Time, 4)
	if loop.commitTimer != nil {
		armed[timerCommit] = loop.commitTimerAt
	}
	if loop.attachTimer != nil {
		armed[timerAttach] = loop.attachTimerAt
	}
	if loop.pushTimer != nil {
		armed[timerPush] = loop.pushTimerAt
	}
	if loop.retry.timer != nil {
		armed[timerRetry] = loop.retry.due
	}
	return armed
}

// armed is the timer the worker has armed to fire first, and when. Two due at the same instant fire in
// either order, as run's select picks.
func (r *windowModelRun) armed() (time.Time, modelTimer, bool) {
	var at time.Time
	var first []modelTimer
	for _, timer := range []modelTimer{timerCommit, timerAttach, timerPush, timerRetry} {
		due, ok := r.armedTimers()[timer]
		switch {
		case !ok:
		case len(first) == 0 || due.Before(at):
			at, first = due, []modelTimer{timer}
		case due.Equal(at):
			first = append(first, timer)
		}
	}
	if len(first) == 0 {
		return time.Time{}, "", false
	}
	return at, first[r.rng.Intn(len(first))], true
}

// deliver is the wake timer delivers, as run does.
func (r *windowModelRun) deliver(timer modelTimer) {
	r.notef("%s timer", timer)
	loop := r.f.loop
	switch timer {
	case timerCommit:
		loop.commitTimer = nil
		loop.closeOrArmWindow()
		loop.endWake(0)
		r.m.closeOrArm()
	case timerAttach:
		// endWake's serviceCommitRequests does its work.
		loop.attachTimer = nil
		loop.endWake(0)
	case timerPush:
		loop.pushTimer = nil
		loop.pushPending()
		loop.endWake(0)
		r.m.push()
	case timerRetry:
		loop.retry.timer = nil
		loop.runRetry()
		loop.endWake(0)
		r.m.push()
	}
	r.applied(r.m.endWake())
}

// advance steps the clock by d, firing each timer the worker armed when it comes due. A deadline the
// worker armed no timer for passes unseen, as it would in run.
func (r *windowModelRun) advance(d time.Duration) {
	r.notef("clock +%s", d)
	target := r.clock.Now().Add(d)
	for range 1000 {
		at, timer, ok := r.armed()
		if !ok || at.After(target) {
			r.clock.advance(target.Sub(r.clock.Now()))
			r.m.now = target
			return
		}
		if at.After(r.clock.Now()) {
			r.clock.advance(at.Sub(r.clock.Now()))
		}
		r.m.now = r.clock.Now()
		r.deliver(timer)
		r.check("timer")
	}
	r.failf("a timer stayed due after its wake")
}

// clockStep is how far one step of the clock goes: a second up to the profile's longest.
func (r *windowModelRun) clockStep() time.Duration {
	return time.Second + time.Duration(r.rng.Int63n(int64(r.profile.clock/time.Second)))*time.Second
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

// windowModelProfile weighs the inputs a run draws. own is the percentage of writes an author makes of
// an object of its own (alice x, bob w, carol z, each alone in its own file), so their windows can
// stay open side by side; the others are of any object. merge is the percentage of pushes from outside
// that move a document into another's file, and clock the longest step of the clock.
type windowModelProfile struct {
	name    string
	own     int
	merge   int
	clock   time.Duration
	weights map[modelStep]int
}

type modelStep string

const (
	stepWrite    modelStep = "write"
	stepPressure modelStep = "write under buffer pressure"
	stepAdvance  modelStep = "clock"
	stepOverdue  modelStep = "overdue"
	stepSave     modelStep = "save"
	stepForeign  modelStep = "push from outside"
	stepApply    modelStep = "apply"
	stepResync   modelStep = "resync"
	stepOutage   modelStep = "outage"
	stepRestart  modelStep = "restart"
	stepStale    modelStep = "late applier write"
)

// modelSteps fixes the order the weights are drawn in, so a seed replays.
var modelSteps = []modelStep{stepWrite, stepPressure, stepAdvance, stepOverdue, stepSave, stepForeign, stepApply,
	stepResync, stepOutage, stepRestart, stepStale}

var (
	// wideProfile draws every kind of input.
	wideProfile = windowModelProfile{name: "wide", own: 67, merge: 25, clock: 25 * time.Second,
		weights: map[modelStep]int{
			stepWrite: 36, stepPressure: 4, stepAdvance: 16, stepOverdue: 3, stepSave: 12, stepForeign: 6,
			stepApply: 6, stepResync: 7, stepOutage: 8, stepRestart: 2, stepStale: 3,
		}}
	// concurrentProfile keeps authors on objects of their own, so several windows stay open through
	// cooldowns, outages, replays and flushes.
	concurrentProfile = windowModelProfile{name: "concurrent", own: 95, merge: 25, clock: 25 * time.Second,
		weights: map[modelStep]int{
			stepWrite: 46, stepPressure: 3, stepAdvance: 14, stepOverdue: 4, stepSave: 9, stepForeign: 5,
			stepApply: 5, stepResync: 3, stepOutage: 9, stepRestart: 2, stepStale: 3,
		}}
	// outageProfile is the concurrent one with short clock steps, so windows stay open across the
	// cooldown and the retries, and with pushes from outside that restructure the folder under them.
	outageProfile = windowModelProfile{name: "outage", own: 95, merge: 50, clock: 8 * time.Second,
		weights: map[modelStep]int{
			stepWrite: 50, stepPressure: 2, stepAdvance: 16, stepOverdue: 3, stepSave: 5, stepForeign: 8,
			stepApply: 3, stepResync: 4, stepOutage: 7, stepRestart: 1, stepStale: 2,
		}}
)

// write is one accepted live write: a new value, an undo to an earlier one, the value the object
// already holds, or a delete. Each author mostly edits an object of its own, so their windows can stay
// open side by side, and sometimes any other.
func (r *windowModelRun) write(pressure bool) {
	who := r.rng.Intn(len(modelAuthors))
	author := modelAuthors[who]
	obj := modelObjects[r.rng.Intn(len(modelObjects))]
	if r.rng.Intn(100) < r.profile.own {
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
	if value != "" && value == r.m.withheld[obj] {
		// An edit back to the value a withheld apply wrote is carried as the apply's, a decided limit of
		// the withheld-field record: the model does not make one.
		value, kind = r.freshValue(obj), "write"
	}
	r.writeAs(author, obj, value, kind, pressure)
}

// writeAs is the live write of obj to value by author.
func (r *windowModelRun) writeAs(author, obj, value, kind string, pressure bool) {
	event := r.event(author, obj, value)
	r.notef("%s %s %s=%q pressure=%v", author, kind, obj, value, pressure)
	edit := modelEdit{obj: obj, value: value, op: event.Operation, charge: eventRetainedCharge(&event)}
	item := WorkItem{Request: &WriteRequest{Events: []Event{event}}}
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
	r.register(req)
}

// register sends the save req, which names the run's next save.
func (r *windowModelRun) register(req *AttachCommitRequest) {
	name, author := req.Name, req.Author
	r.notef("%s registers %s attach=%s attachTimeout=%s maxDuration=%s idle=%v", author, name, req.Attach,
		req.AttachTimeout, req.MaxDuration, req.IdleTimeout)
	s := &modelSave{name: name, author: author, message: req.Message, next: req.Attach == configv1alpha3.AttachNext,
		idle: req.IdleTimeout, maxDuration: req.MaxDuration}
	r.wake(&WorkItem{Attach: req}, func() { r.m.register(s, req.AttachTimeout) })
}

// foreign pushes a commit from outside and reports it, as a Git host's push notice does.
func (r *windowModelRun) foreign() {
	if r.rng.Intn(100) < r.profile.merge && r.mergeOutside() {
		return
	}
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

// mergeOutside moves, from outside, the document of one of x, w and z into the file another is alone
// in, and reports it, when two of them are each alone in a file of their own: windows proven apart
// before the move overlap once it is found.
func (r *windowModelRun) mergeOutside() bool {
	var alone []string
	for _, obj := range modelObjects[:3] {
		if at := r.m.remote.loc(obj); at != "" && len(r.m.remote.files[at]) == 1 && at == echoFile(obj) {
			alone = append(alone, obj)
		}
	}
	if len(alone) < 2 {
		return false
	}
	r.rng.Shuffle(len(alone), func(i, j int) { alone[i], alone[j] = alone[j], alone[i] })
	r.merge(alone[0], alone[1])
	return true
}

// merge moves, from outside, obj's document into the file into's is alone in, and reports it.
func (r *windowModelRun) merge(into, obj string) {
	r.notef("outside push moves %s into %s's file, hinted", obj, into)
	r.pushFromOutside(r.m.remote, r.m.remote.merge(into, obj), "outside: move "+obj+" in with "+into)
	r.m.foreignUnseen = true
	r.f.loop.pushHinted = true
	r.wake(nil, func() { r.m.hinted = true })
}

// restructure is a push from outside that moves bob's document into alice's file while each has a
// window open, proven apart on the tree before it, and a save keeps the apply the push brings from
// closing them: once the worker finds the push, the next write proves both windows again, finds them
// overlapping, and closes them. It needs x and w each alone in its file, and a recent push, so the
// cooldown holds the push the hint makes; a run starts it with neither.
func (r *windowModelRun) restructure() {
	r.writeAs("alice", "x", r.freshValue("x"), "write", false)
	r.advance(modelIdle + time.Second) // alice's window closes, and its push is made at once
	r.check("restructure: a push")
	r.saves++
	r.register(&AttachCommitRequest{
		Namespace: "default", Name: "save-" + strconv.Itoa(r.saves), UID: "uid-save-" + strconv.Itoa(r.saves),
		Author: "carol", GitTargetName: echoTarget, GitTargetNamespace: "default",
		Message: "feat: carol's save", Attach: configv1alpha3.AttachCurrentOrNext, AttachTimeout: 20 * time.Second,
		MaxDuration: 40 * time.Second,
	})
	r.check("restructure: a save waits")
	r.writeAs("bob", "z", r.freshValue("z"), "write", false)
	r.merge("x", "w") // the hint closes bob's window, and the cooldown holds its push
	r.check("restructure: a push from outside")
	r.writeAs("alice", "x", r.freshValue("x"), "write", false)
	r.writeAs("bob", "w", r.freshValue("w"), "write", false)
	r.check("restructure: two windows")
	r.advance(modelCooldown) // the push replays onto the move; carol's save holds the apply
	r.check("restructure: the replay")
	r.writeAs("carol", "z", r.freshValue("z"), "write", false)
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
	var decision int
	r.wake(&item, func() {
		r.m.resync(req.charge())
		decision = r.m.decisions
	})
	// Its caller hears at once: what it found, once it is committed, or the failure a retry waits out.
	deferred := slices.ContainsFunc(r.m.pending, func(p modelPending) bool {
		return p.decision == decision && !p.committed
	})
	if result := <-req.Result; (result.Err != nil) != deferred {
		r.failf("resync: %v; the model has it deferred: %v", result.Err, deferred)
	}
}

// staleApply is an applier's write landing over a newer edit: an apply is sent and reported, an author
// edits an object of the apply's commit after it, and then the applier's write of that object, with
// the value the commit holds, arrives inside the apply's window. The worker keeps it out of Git, and
// asks the applier for a Repair, which puts the edit back once it is published.
func (r *windowModelRun) staleApply() {
	r.sent = false
	r.apply()
	who := r.rng.Intn(len(modelAuthors))
	obj := modelObjects[who]
	if !r.sent || r.m.applied[obj] == "" {
		return
	}
	r.check("apply")
	r.writeAs(modelAuthors[who], obj, r.freshValue(obj), "write", false)
	r.check("write")
	stale := r.m.applied[obj]
	r.notef("the applier writes %s=%q late, over the edit", obj, stale)
	event := echoEvent(obj, stale)
	event.UserInfo.Username = "system:serviceaccount:flux-system:kustomize-controller"
	event.LastFieldManagers = []string{FluxFieldManager}
	event.LastFieldManagedAt = time.Now().Add(time.Hour)
	r.wake(&WorkItem{Request: &WriteRequest{Events: []Event{event}}}, func() {
		r.m.reached(reachStaleApply)
		r.m.withheld[obj] = stale
		r.m.applyDue = true
	})
}

// toggleOutage takes the Git server down, or brings it back.
func (r *windowModelRun) toggleOutage() {
	r.m.down = !r.m.down
	r.outage.down = r.m.down
	r.notef("outage down=%v", r.m.down)
}

func (r *windowModelRun) shutdown() {
	r.notef("shutdown")
	r.f.loop.handleShutdown()
	r.f.worker.settleCommitRequestsAfterExit()
	r.exited = true
	r.m.shutdown()
}

// overdue steps the clock past the timers' deadlines without delivering them, handles one input first,
// as run's select may when an item and a timer are both ready, and then delivers the timers.
func (r *windowModelRun) overdue() {
	d := r.clockStep()
	r.notef("clock +%s, timers held", d)
	r.clock.advance(d)
	r.m.now = r.clock.Now()
	if at, _, ok := r.armed(); ok && !at.After(r.m.now) {
		r.m.reached(reachOverdue)
	}
	if r.rng.Intn(2) == 0 {
		r.write(false)
	} else {
		r.save()
	}
	r.check("overdue input")
	r.advance(0)
}

// restart stops the worker, as a rollout does, and starts a new one on the same Git server with an
// empty checkout, once the server is up: what the old one could not push is lost with its checkout,
// and the new one starts with no window, save or pending write.
func (r *windowModelRun) restart() {
	r.shutdown()
	r.check("shutdown")
	r.checkGiven()
	r.m.reached(reachRestart)
	if r.m.down {
		r.toggleOutage()
	}
	r.notef("restart")
	old := r.f.worker
	r.f.loop.stopTimers()
	w := NewBranchWorker(old.Client, old.Log, old.GitProviderRef, old.GitProviderNamespace, old.Branch, old.repo,
		nil, BranchWorkerLimits{})
	w.Log, w.ctx, w.GitBranch, w.mapper, w.clock = old.Log, old.ctx, old.GitBranch, old.mapper, old.clock
	w.applyRequester = old.applyRequester
	require.NoError(r.t, os.RemoveAll(w.repoRootPath()))
	require.NoError(r.t, w.ensureRepositoryInitialized(w.ctx))
	loop := newBranchWorkerEventLoop(w, time.Hour)
	r.t.Cleanup(loop.stopTimers)
	loop.defaultWindow = commitWindowDefaults{idleTimeout: modelIdle, maxDuration: modelMax}
	loop.followRecordedOperations(time.Now())
	// As at the start: the converges a new worker leaves pending are answered already.
	w.inbox.take()
	for _, state := range loop.appliers {
		state.converge = false
	}
	r.f.worker, r.f.loop = w, loop
	r.exited, r.firstSave = false, r.saves+1
	r.knownTip = gitIn(r.t, r.f.repoDir, "rev-parse", "main")
	r.m.restart()
}

func (r *windowModelRun) step() {
	step := r.drawStep()
	switch {
	case step == stepForeign && r.resyncWaiting():
		// A resync that waits for its retry fetches before it commits, and a push from outside found
		// by that fetch discards it: the model does not cover that.
		step = stepWrite
	case step == stepResync && (r.m.foreignUnseen || r.m.applyDue):
		step = stepWrite
	}
	map[modelStep]func(){
		stepWrite:    func() { r.write(false) },
		stepPressure: func() { r.write(true) },
		stepAdvance:  func() { r.advance(r.clockStep()) },
		stepOverdue:  r.overdue,
		stepSave:     r.save,
		stepForeign:  r.foreign,
		stepApply:    r.apply,
		stepResync:   r.resync,
		stepOutage:   r.toggleOutage,
		stepRestart:  r.restart,
		stepStale:    r.staleApply,
	}[step]()
}

// drawStep draws a step by the profile's weights.
func (r *windowModelRun) drawStep() modelStep {
	total := 0
	for _, s := range modelSteps {
		total += r.profile.weights[s]
	}
	roll := r.rng.Intn(total)
	for _, s := range modelSteps {
		if roll < r.profile.weights[s] {
			return s
		}
		roll -= r.profile.weights[s]
	}
	return stepWrite
}

// resyncWaiting reports whether a decided resync waits for its retry to be committed.
func (r *windowModelRun) resyncWaiting() bool {
	return slices.ContainsFunc(r.m.pending, func(p modelPending) bool { return p.resync && !p.committed })
}

func (r *windowModelRun) run(steps int) {
	if r.profile.name == outageProfile.name {
		r.restructure()
		r.check("restructure")
	}
	for range steps {
		r.step()
		r.check("step")
		m := r.m
		if m.several() {
			r.concurrent++
			if !m.pushAt.IsZero() {
				m.reached(reachCooldown)
			}
			if !m.retryAt.IsZero() {
				m.reached(reachRetryWait)
			}
		}
		if !m.holdsDecided() {
			m.reached(reachDeferred)
		}
	}
	if r.rng.Intn(2) == 0 {
		r.shutdown()
		r.check("shutdown")
	} else {
		if r.m.down {
			r.toggleOutage()
		}
		// Every window closes, and the retry, at its longest backoff, comes due.
		r.advance(modelMax + modelRetryMax)
	}
	r.drained()
}

var closeLine = regexp.MustCompile(`"msg"="Finalizing open commit window" "reason"="([^"]+)" ` +
	`"windowAuthor"="([^"]*)" "windowTarget"="[^"]*" "window"=(\d+)`)

// check compares the worker with the model after a step.
// check compares the worker with the model after a step.
func (r *windowModelRun) check(what string) {
	r.t.Helper()
	if r.m.surprise != "" {
		r.failf("%s: the model does not cover this: %s", what, r.m.surprise)
	}
	r.checkWindows(what)
	r.checkPending(what)
	r.checkTimers(what)
	r.checkSaves(what)
	r.checkCloses(what)
	r.checkCommits(what)
	if lines := r.logs.matching(decisionSingleWindowFallback); len(lines) > 0 {
		r.failf("%s: the worker refused a commit outside its footprint: %s", what, lines[0])
	}
}

// checkWindows compares the open windows and what they are charged.
func (r *windowModelRun) checkWindows(what string) {
	r.t.Helper()
	loop, m := r.f.loop, r.m
	if got, want := r.workerWindows(), m.describeWindows(); got != want {
		r.failf("%s: the open windows differ:\n  worker %s\n  model  %s", what, got, want)
	}
	if got, want := loop.windows.bytes(), m.windowCharge(); got != want {
		r.failf("%s: the windows are charged %d; the model's %d", what, got, want)
	}
}

// checkPending compares the pending writes, in order, what each is charged, which are committed, and
// what everything retained is charged.
func (r *windowModelRun) checkPending(what string) {
	r.t.Helper()
	loop, m := r.f.loop, r.m
	var got, want []string
	for i := range loop.pendingWrites {
		pw := &loop.pendingWrites[i]
		save := ""
		if pw.CommitRequest != nil {
			save = pw.CommitRequest.Name
		}
		got = append(got, fmt.Sprintf("resync=%v save=%q charge=%d committed=%v", pw.Kind == PendingWriteResync,
			save, pw.charge, pw.committedOnce))
	}
	for _, p := range m.pending {
		save := ""
		if p.save != nil {
			save = p.save.name
		}
		want = append(want, fmt.Sprintf("resync=%v save=%q charge=%d committed=%v", p.resync, save, p.charge,
			p.committed))
	}
	if !slices.Equal(got, want) {
		r.failf("%s: the pending writes differ:\n  worker %v\n  model  %v", what, got, want)
	}
	if loop.pendingWritesBytes != m.pendingCharge() {
		r.failf("%s: the pending writes are charged %d; the model's %d", what, loop.pendingWritesBytes,
			m.pendingCharge())
	}
	if got, want := loop.totalRetainedBytes(), m.windowCharge()+m.pendingCharge(); got != want {
		r.failf("%s: retained %d; the model's %d", what, got, want)
	}
}

// checkTimers compares the timers the worker has armed with the model's deadlines: the commit timer
// for the earliest close of an open window, the attach timer for the earliest deadline of a save still
// waiting, the push timer for the end of the cooldown, and the retry timer for the next attempt.
func (r *windowModelRun) checkTimers(what string) {
	r.t.Helper()
	if r.exited {
		return
	}
	m := r.m
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
	want := map[modelTimer]time.Time{timerCommit: commitAt, timerAttach: attachAt, timerPush: m.pushAt,
		timerRetry: m.retryAt}
	armed := r.armedTimers()
	var gotTimers, wantTimers []string
	for _, timer := range []modelTimer{timerCommit, timerAttach, timerPush, timerRetry} {
		at, ok := armed[timer]
		gotTimers = append(gotTimers, string(timer)+" "+r.describeTimer(ok, at))
		wantTimers = append(wantTimers, string(timer)+" "+r.describeTimer(!want[timer].IsZero(), want[timer]))
	}
	if !slices.Equal(gotTimers, wantTimers) {
		r.failf("%s: the armed timers differ:\n  worker %v\n  model  %v", what, gotTimers, wantTimers)
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
	for i := r.firstSave; i <= r.saves; i++ {
		r.checkOutcome(what, "save-"+strconv.Itoa(i))
	}
}

// checkOutcome compares a save's outcome, or none, and how often it settled.
func (r *windowModelRun) checkOutcome(what, name string) {
	r.t.Helper()
	res, resolved := r.f.worker.LookupCommitRequestOutcome("default", name, "uid-"+name)
	want, modelResolved := r.m.outcomes[name]
	if resolved != modelResolved || (resolved && (res.Err != nil) != want.failed) ||
		(resolved && !want.failed && res.Outcome != want.outcome) {
		r.failf("%s: %s settled %v as %q (err %v); the model says %v as %+v", what, name, resolved,
			res.Outcome, res.Err, modelResolved, want)
	}
	if n := r.settlements(name); n > 1 || (n == 1) != modelResolved {
		r.failf("%s: %s settled %d times", what, name, n)
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
		r.checkFootprintChecked(what, hash, author, f)
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

// TestWindowModel_AFootprintCheckVouchesOnlyForItsOwnChange pins the model's evidence that a commit
// was checked against its footprint. Alice's commit x2 is checked; bob's x3 and carol's return to x2
// are made with the check skipped. Carol's commit has the tree alice's had, from another parent, so
// matching the log by tree alone took alice's check as carol's.
func TestWindowModel_AFootprintCheckVouchesOnlyForItsOwnChange(t *testing.T) {
	f := newAdmissionFixture(t, "model-footprint-witness", nil)
	f.loop.pushPending()
	logs := captureInfo(f.worker)
	f.writeBy("sentinel", "z", "z2") // company, so every window after it is proven
	commit := func(author, value string, skipCheck bool) string {
		f.writeBy(author, "x", value)
		windows := f.loop.windows.all()
		require.Len(t, windows, 2)
		if skipCheck {
			windows[1].footprint = nil
		}
		f.loop.lastPushAt = time.Time{}
		f.loop.finalizeWindow(windows[1], windowFinalizeReasonIdleTimeout)
		f.loop.endWake(0)
		f.loop.pushPending()
		return gitIn(t, f.repoDir, "rev-parse", "main")
	}
	checked := commit("alice", "x2", false)
	commit("bob", "x3", true)
	unchecked := commit("carol", "x2", true)
	require.Equal(t, gitIn(t, f.repoDir, "rev-parse", checked+"^{tree}"),
		gitIn(t, f.repoDir, "rev-parse", unchecked+"^{tree}"), "precondition: carol returns to alice's tree")
	require.Len(t, logs.matching(decisionFootprintChecked), 1, "precondition: only alice's commit was checked")

	r := &windowModelRun{t: t, f: f, logs: logs}
	require.True(t, r.footprintChecked(checked))
	require.False(t, r.footprintChecked(unchecked), "alice's check is not evidence for carol's commit")
}

// checkFootprintChecked fails unless the worker checked the commit against its window's footprint
// itself: one that skipped it would let a footprint bug through unrefused.
func (r *windowModelRun) checkFootprintChecked(what, hash, author string, f *modelFootprint) {
	r.t.Helper()
	if !r.footprintChecked(hash) {
		r.failf("%s: commit %s by %s had a footprint %s, and the worker did not check it (%s)", what,
			hash[:8], author, f, decisionFootprintChecked)
	}
}

// footprintChecked reports whether the worker logged a footprint check of the commit's change. It is
// matched by the commit's tree and its parent's: a rebuild of the prefix onto the same parent (a
// resync's fetch) makes the checked commit again, under another hash, while a later commit that
// returns to a checked tree from another parent is another change.
func (r *windowModelRun) footprintChecked(hash string) bool {
	r.t.Helper()
	tree := gitIn(r.t, r.f.repoDir, "rev-parse", hash+"^{tree}")
	parentTree := plumbing.ZeroHash.String()
	if gitIn(r.t, r.f.repoDir, "rev-list", "--parents", "-n", "1", hash) != hash {
		parentTree = gitIn(r.t, r.f.repoDir, "rev-parse", hash+"^^{tree}")
	}
	return len(r.logs.matching(`"tree"="`+tree+`" "parentTree"="`+parentTree+`" "footprint"=`)) > 0
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
	// A shutdown during an outage loses the writes its last push could not publish, with the checkout.
	lost := r.exited && r.m.down
	if len(r.m.windows) != 0 || len(r.m.saves) != 0 || (len(r.m.pending) != 0 && !lost) {
		r.failf("the model is not drained")
	}
	// An exited loop is dropped with the saves it gave back still counted in heldBytes, and the writes
	// it lost; what it retains for Git is nothing else.
	held := r.f.loop.heldBytes()
	if r.exited {
		held = 0
	}
	if held != 0 || r.f.loop.totalRetainedBytes() != r.m.pendingCharge() {
		r.failf("drained, the worker still charges %d (retained %d)", held, r.f.loop.totalRetainedBytes())
	}
	r.checkTree()
	r.checkGiven()
}

// checkGiven checks that the saves given back at shutdown have no outcome and no phase.
func (r *windowModelRun) checkGiven() {
	r.t.Helper()
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

// reachFloors are the situations the search must reach across its seeds, at the default size or larger,
// or a pass says nothing about them.
var reachFloors = []string{
	reachShutdown, reachFlush, reachHint, reachAttachNext, reachReproof, reachPathConflict, reachFailedPush,
	reachCooldown, reachRetryWait, reachDeferred, reachReplay, reachPressure, reachApplyWaits, reachRestart,
	reachOverdue, reachStaleApply,
}

// TestWindowModel_TheWorkerFollowsTheReferenceModel runs random input sequences through a worker and
// the model, and checks every step. Half the seeds draw the wide profile, half the concurrent one.
// Across the seeds, some steps must end with several windows open, and every reach floor must be met,
// or the search says nothing about them.
func TestWindowModel_TheWorkerFollowsTheReferenceModel(t *testing.T) {
	const defaultSeeds = 20
	seeds, steps := envInt("WINDOW_MODEL_SEEDS", defaultSeeds), envInt("WINDOW_MODEL_STEPS", 60)
	if testing.Short() {
		seeds = 2
	}
	first := int64(1)
	if seed := envInt("WINDOW_MODEL_SEED", 0); seed > 0 {
		first, seeds = int64(seed), 1
	}
	concurrent := 0
	reach := map[string]int{}
	for seed := first; seed < first+int64(seeds); seed++ {
		t.Run("seed-"+strconv.FormatInt(seed, 10), func(t *testing.T) {
			run := newWindowModelRun(t, seed)
			run.run(steps)
			concurrent += run.concurrent
			for what, n := range run.m.reach {
				reach[what] += n
			}
		})
	}
	t.Logf("%d steps ended with several windows open", concurrent)
	for _, what := range reachFloors {
		t.Logf("reached %d times: %s", reach[what], what)
	}
	if t.Failed() {
		return
	}
	if seeds > 1 {
		require.Positive(t, concurrent, "no step ended with several windows open")
	}
	if seeds >= defaultSeeds && steps >= 60 {
		for _, what := range reachFloors {
			assert.Positive(t, reach[what], "the search never reached %s", what)
		}
	}
}
