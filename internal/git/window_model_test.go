// SPDX-License-Identifier: Apache-2.0

package git

import (
	"fmt"
	"maps"
	"math/rand"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
)

// The reference model of docs/design/concurrent-author-windows.md ("How to test it"): a small,
// independent implementation of today's single-window rules, fed the same random inputs as a branch
// worker on a real Git server, with the worker checked against it.
//
//   - After every step: the open window (author, objects, attached save, deadline, charge), the timers
//     armed for it and for the waiting saves, the registered saves (attached or waiting, and until
//     when), the retained-byte charge, the close reasons so far, and every save's outcome are the
//     model's.
//   - Every commit the worker publishes is the one the model's closing window makes: its author, its
//     message, every path it changes, and each document whole. Per document, the writes that land do
//     so in the order they were accepted, so an older write never lands after a newer one.
//   - Every save settles exactly once, or is given back at shutdown.
//   - Drained, Git holds the model's tree, path for path, and nothing is charged.
//
// The model shares no window code with the worker: only the event fixtures, the per-event charge
// (eventRetainedCharge, attachCharge) and the close-reason names. The worker's timers are real; the
// driver steps the worker's clock to the time each armed timer is due and delivers that timer's wake,
// so a deadline the worker armed no timer for is never delivered.
//
// WINDOW_MODEL_SEEDS and WINDOW_MODEL_STEPS raise the search; a failure names its seed, and
// WINDOW_MODEL_SEED replays one.

const (
	// modelIdle and modelMax are the commit window the GitTarget declares none of, so the loop's default.
	modelIdle = 10 * time.Second
	modelMax  = 60 * time.Second
)

var (
	modelObjects = []string{"x", "y", "z"}
	modelAuthors = []string{"alice", "bob", "carol"}
	// modelBootstrap is what the worker adds to the folder, from its template, with the first commit
	// it makes there. The GitTarget encrypts nothing, so there is no .sops.yaml.
	modelBootstrap = []string{gitTargetIgnoreFileName, "README.md"}
)

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
	author      string
	objs        []string
	latest      map[string]modelEdit
	save        *modelSave
	idle        time.Duration
	noIdle      bool
	maxAt       time.Time
	lastWriteAt time.Time
	charge      int64
}

func (w *modelWindow) closeAt() (time.Time, windowFinalizeReason) {
	if !w.noIdle && w.lastWriteAt.Add(w.idle).Before(w.maxAt) {
		return w.lastWriteAt.Add(w.idle), windowFinalizeReasonIdleTimeout
	}
	return w.maxAt, windowFinalizeReasonMaxDuration
}

// modelPending is a decided write the next push publishes.
type modelPending struct {
	save    *modelSave
	author  string
	message string
	resync  bool
	changes []modelEdit
}

// modelCommit is a commit the worker must publish. A resync's message is not the model's to say.
// bootstrap is the folder's first commit, which also adds modelBootstrap.
type modelCommit struct {
	author    string
	message   string
	resync    bool
	bootstrap bool
	files     map[string]string
	seqs      map[string]int
}

type windowModel struct {
	now     time.Time
	window  *modelWindow
	saves   map[string]*modelSave
	saveSeq int
	editSeq int

	// remote is Git, known what the worker's checkout holds of it, cluster the live objects.
	remote, known, cluster map[string]string
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

func newWindowModel(now time.Time, docs map[string]string) *windowModel {
	m := &windowModel{
		now: now, saves: map[string]*modelSave{},
		remote: map[string]string{}, known: map[string]string{}, cluster: map[string]string{},
		history: map[string][]string{}, outcomes: map[string]FinalizeOutcome{}, given: map[string]bool{},
		landed: map[string]int{},
	}
	for obj, value := range docs {
		m.remote[obj], m.known[obj], m.cluster[obj] = value, value, value
		m.history[obj] = []string{value}
	}
	return m
}

func (m *windowModel) windowCharge() int64 {
	if m.window == nil {
		return 0
	}
	return m.window.charge
}

// describeWindow is the open window: its author, how many objects it holds, the save it carries, when
// and why it closes, and what it is charged.
func (m *windowModel) describeWindow() string {
	w := m.window
	if w == nil {
		return "none"
	}
	saved := ""
	if w.save != nil {
		saved = w.save.name
	}
	at, reason := w.closeAt()
	return fmt.Sprintf("%s objs=%d save=%q closes in %s (%s) charge %d", w.author, len(w.objs), saved,
		at.Sub(m.now), reason, w.charge)
}

func (m *windowModel) waiting(s *modelSave) bool { return !s.attached && s.deadline.After(m.now) }

func (m *windowModel) matches(s *modelSave) bool {
	return m.window != nil && m.window.author == s.author
}

// write is appendLiveEvent's rules for one accepted write.
func (m *windowModel) write(author string, edit modelEdit, pressure bool) {
	m.editSeq++
	edit.seq = m.editSeq
	m.cluster[edit.obj] = edit.value
	if edit.value != "" {
		m.history[edit.obj] = append(m.history[edit.obj], edit.value)
	}
	if m.window != nil && m.window.author != author {
		m.close(windowFinalizeReasonIdentityChange)
	}
	opened := m.window == nil
	if opened {
		m.window = &modelWindow{
			author: author, latest: map[string]modelEdit{},
			idle: modelIdle, maxAt: m.now.Add(modelMax),
		}
	}
	w := m.window
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
		if m.window != w {
			return
		}
	}
	if pressure {
		m.close(windowFinalizeReasonBufferLimit)
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
		collecting := earlier.attached && m.window != nil && m.window.save == earlier
		if m.waiting(earlier) || (collecting && !s.next) {
			m.outcomes[s.name] = FinalizeSaveInProgress
			return
		}
	}
	m.saveSeq++
	s.seq = m.saveSeq
	s.deadline = m.now.Add(attachTimeout)
	m.saves[s.name] = s
	if m.window == nil {
		return
	}
	if !m.matches(s) {
		s.sawForeign = true
		return
	}
	if s.next {
		m.close(windowFinalizeReasonAttachNext)
		return
	}
	if m.window.save == nil {
		m.attach(s)
	}
}

func (m *windowModel) attach(s *modelSave) {
	w := m.window
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
	if m.window == nil {
		return
	}
	if at, reason := m.window.closeAt(); !at.After(m.now) {
		m.close(reason)
	}
}

func (m *windowModel) noteForeign() {
	for _, s := range m.saves {
		if m.waiting(s) && m.window != nil && !m.matches(s) {
			s.sawForeign = true
		}
	}
}

func (m *windowModel) attachWaiting() {
	if m.window == nil || m.window.save != nil {
		return
	}
	var first *modelSave
	for _, s := range m.saves {
		if m.waiting(s) && m.matches(s) && (first == nil || s.seq < first.seq) {
			first = s
		}
	}
	if first != nil {
		m.attach(first)
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

// close decides the open window: the documents whose latest write differs from the checkout, in the
// order the window first saw them. A window that changes nothing and carries no save leaves no trace.
// A save is never trimmed: it decides every document it captured, so a replay onto a push the worker
// had not seen re-asserts them (API wins for a save).
func (m *windowModel) close(reason windowFinalizeReason) {
	w := m.window
	m.window = nil
	m.closes = append(m.closes, string(reason)+" "+w.author)
	var changes []modelEdit
	for _, obj := range w.objs {
		if edit := w.latest[obj]; edit.value != m.known[obj] || w.save != nil {
			changes = append(changes, edit)
			m.known[obj] = edit.value
		}
	}
	if len(changes) == 0 && w.save == nil {
		return
	}
	p := modelPending{save: w.save, author: w.author, changes: changes}
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
func (m *windowModel) push() {
	if len(m.pending) == 0 {
		return
	}
	for _, p := range m.pending {
		commit := modelCommit{author: p.author, message: p.message, resync: p.resync,
			files: map[string]string{}, seqs: map[string]int{}}
		for _, edit := range p.changes {
			if edit.value == m.remote[edit.obj] {
				continue
			}
			m.remote[edit.obj] = edit.value
			commit.files[edit.obj] = edit.value
			commit.seqs[edit.obj] = edit.seq
		}
		if len(commit.files) == 0 {
			if p.save != nil {
				m.resolve(p.save, FinalizeAlreadyPresent)
			}
			continue
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
	m.known = maps.Clone(m.remote)
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
		if m.window != nil && m.window.save == nil {
			m.close(windowFinalizeReasonForeignCommit)
		}
	}
	m.push()
	if !m.applyDue || len(m.saves) > 0 {
		return false
	}
	if m.window != nil {
		m.close(windowFinalizeReasonApplyDue)
	}
	m.push()
	m.applyDue = false
	// The apply is of the tip the worker knows: a push it has not found yet is not in it.
	m.cluster = maps.Clone(m.known)
	return true
}

// resync closes the window, then commits, as the operator, every live object Git does not hold as
// it is. prune.mode is onEvent, so nothing is swept. An object it adds is adopted: an apply is due.
func (m *windowModel) resync() {
	if m.window != nil {
		m.close(windowFinalizeReasonResyncBeforeApply)
	}
	var changes []modelEdit
	for _, obj := range modelObjects {
		value := m.cluster[obj]
		if value == "" || value == m.known[obj] {
			continue
		}
		if m.known[obj] == "" {
			m.applyDue = true
		}
		m.known[obj] = value
		changes = append(changes, modelEdit{obj: obj, value: value})
	}
	if len(changes) > 0 {
		m.pending = append(m.pending, modelPending{author: DefaultCommitterName, resync: true, changes: changes})
	}
}

// shutdown closes the window and publishes; a save still waiting for a window is given back.
func (m *windowModel) shutdown() {
	if m.window != nil {
		m.close(windowFinalizeReasonShutdown)
	}
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
}

func newWindowModelRun(t *testing.T, seed int64) *windowModelRun {
	t.Helper()
	docs := map[string]string{"x": "x0", "y": "y0", "z": "z0"}
	f := newEchoFixture(t, "window-model-"+strconv.FormatInt(seed, 10), configv1alpha3.PruneOnEvent, docs)
	requester := f.withClaim()
	r := &windowModelRun{t: t, seed: seed, f: f, requester: requester, rng: rand.New(rand.NewSource(seed))}
	r.clock = useTestClock(f.worker)
	r.logs = captureInfo(f.worker)
	f.loop.defaultWindow = commitWindowDefaults{idleTimeout: modelIdle, maxDuration: modelMax}
	r.base = gitIn(t, f.repoDir, "rev-parse", "main")
	r.knownTip = r.base
	r.start = r.clock.Now()
	r.m = newWindowModel(r.start, docs)
	r.m.bootstrapped = gitIn(t, f.repoDir, "ls-tree", "--name-only", r.base, echoTarget+"/README.md") != ""
	return r
}

func (r *windowModelRun) failf(format string, args ...any) {
	r.t.Helper()
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
	return obj + strconv.Itoa(r.values)
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
// already holds, or a delete.
func (r *windowModelRun) write(pressure bool) {
	author := modelAuthors[r.rng.Intn(len(modelAuthors))]
	obj := modelObjects[r.rng.Intn(len(modelObjects))]
	current := r.m.cluster[obj]
	value, kind := r.freshValue(obj), "write"
	switch roll := r.rng.Intn(20); {
	case roll < 4 && len(r.m.history[obj]) > 1:
		value, kind = r.m.history[obj][r.rng.Intn(len(r.m.history[obj])-1)], "undo"
	case roll < 7 && current != "":
		value, kind = current, "no-op"
	case roll < 10 && current != "":
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
	r.f.pushFromOutside(obj, value)
	r.m.remote[obj] = value
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
	}
	if r.rng.Intn(2) == 0 {
		r.shutdown()
		r.check("shutdown")
	} else {
		r.advance(3 * modelMax)
	}
	r.drained()
}

var closeLine = regexp.MustCompile(`"msg"="Finalizing open commit window" "reason"="([^"]+)" "windowAuthor"="([^"]*)"`)

// check compares the worker with the model after a step.
func (r *windowModelRun) check(what string) {
	r.t.Helper()
	r.checkWindow(what)
	r.checkTimers(what)
	r.checkSaves(what)
	r.checkCloses(what)
	r.checkCommits(what)
}

// checkWindow compares the open window and what is retained.
func (r *windowModelRun) checkWindow(what string) {
	r.t.Helper()
	loop, m := r.f.loop, r.m
	if got, want := r.workerWindow(), m.describeWindow(); got != want {
		r.failf("%s: the open window differs:\n  worker %s\n  model  %s", what, got, want)
	}
	if got, want := loop.totalRetainedBytes(), m.windowCharge(); got != want || loop.commitWindows.windowBytes != want {
		r.failf("%s: retained %d (window %d), the model's window charge is %d", what, got,
			loop.commitWindows.windowBytes, want)
	}
	if len(loop.pendingWrites) != 0 {
		r.failf("%s: %d writes still pending after a wake that pushed at once", what, len(loop.pendingWrites))
	}
}

// checkTimers compares the timers the worker has armed with the model's deadlines: the commit timer
// for the open window's close, the attach timer for the earliest deadline of a save still waiting.
func (r *windowModelRun) checkTimers(what string) {
	r.t.Helper()
	if r.exited {
		return
	}
	loop, m := r.f.loop, r.m
	var commitAt, attachAt time.Time
	if m.window != nil {
		commitAt, _ = m.window.closeAt()
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

// workerWindow describes the worker's open window as describeWindow does the model's.
func (r *windowModelRun) workerWindow() string {
	w := r.f.loop.openWindow
	if w == nil {
		return "none"
	}
	saved := ""
	if w.pendingCR != nil {
		saved = w.pendingCR.Name
	}
	at, reason := w.closeAt()
	return fmt.Sprintf("%s objs=%d save=%q closes in %s (%s) charge %d", w.Author, len(w.pathOrder), saved,
		at.Sub(r.m.now), reason, w.bytes)
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

// checkCloses compares the windows closed so far, by reason and author, in order.
func (r *windowModelRun) checkCloses(what string) {
	r.t.Helper()
	var closes []string
	for _, line := range r.logs.matching("Finalizing open commit window") {
		match := closeLine.FindStringSubmatch(line)
		if match == nil {
			r.failf("unparsed close line %s", line)
		}
		closes = append(closes, match[1]+" "+match[2])
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
	var wantPaths []string
	for obj := range want.files {
		wantPaths = append(wantPaths, echoFile(obj))
	}
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
	for obj, value := range want.files {
		r.checkDocument(what, hash, obj, value)
	}
	for obj, seq := range want.seqs {
		if seq <= r.m.landed[obj] {
			r.failf("%s: commit %s lands write %d of %s after write %d", what, hash[:8], seq, obj, r.m.landed[obj])
		}
		r.m.landed[obj] = seq
	}
}

// checkDocument compares the document obj at rev, parsed, with the ConfigMap obj holding value; value
// "" is no document. YAML 1.2, so a bare y is the name y.
func (r *windowModelRun) checkDocument(what, rev, obj, value string) {
	r.t.Helper()
	var got, want map[string]any
	listed := gitIn(r.t, r.f.repoDir, "ls-tree", "--name-only", rev, echoFile(obj))
	if listed != "" {
		if err := yaml.Unmarshal([]byte(gitIn(r.t, r.f.repoDir, "show", rev+":"+echoFile(obj))), &got); err != nil {
			r.failf("%s: %s at %s: %v", what, obj, rev, err)
		}
	}
	if value != "" {
		want = map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": obj, "namespace": "default"},
			"data":     map[string]any{"key": value},
		}
	}
	if !reflect.DeepEqual(got, want) {
		r.failf("%s: %s at %s is %v; the model's is %v", what, obj, rev, got, want)
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
	if r.m.window != nil || len(r.m.saves) != 0 || len(r.m.pending) != 0 {
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

// checkTree checks that Git is the base tree with the model's documents and, once the folder is
// bootstrapped, the template's files: nothing else was added, changed or removed.
func (r *windowModelRun) checkTree() {
	r.t.Helper()
	present := map[string]bool{}
	for _, path := range splitLines(gitIn(r.t, r.f.repoDir, "ls-tree", "-r", "--name-only", r.base)) {
		present[path] = true
	}
	for _, obj := range modelObjects {
		present[echoFile(obj)] = r.m.remote[obj] != ""
		r.checkDocument("drained", "main", obj, r.m.remote[obj])
	}
	others := []string{"diff", "--name-only", r.base, "main", "--", ".",
		":(exclude)" + echoTarget + "/default/configmaps"}
	for _, name := range modelBootstrap {
		others = append(others, ":(exclude)"+echoTarget+"/"+name)
		if r.m.bootstrapped {
			present[echoTarget+"/"+name] = true
			r.checkBootstrap("drained", "main", name)
		}
	}
	var want []string
	for path, ok := range present {
		if ok {
			want = append(want, path)
		}
	}
	slices.Sort(want)
	if tree := splitLines(gitIn(r.t, r.f.repoDir, "ls-tree", "-r", "--name-only", "main")); !slices.Equal(tree, want) {
		r.failf("drained, Git holds %v; the model %v", tree, want)
	}
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
// the model, and checks every step.
func TestWindowModel_TheWorkerFollowsTheReferenceModel(t *testing.T) {
	seeds, steps := envInt("WINDOW_MODEL_SEEDS", 6), envInt("WINDOW_MODEL_STEPS", 40)
	if testing.Short() {
		seeds = 2
	}
	first := int64(1)
	if seed := envInt("WINDOW_MODEL_SEED", 0); seed > 0 {
		first, seeds = int64(seed), 1
	}
	for seed := first; seed < first+int64(seeds); seed++ {
		t.Run("seed-"+strconv.FormatInt(seed, 10), func(t *testing.T) {
			newWindowModelRun(t, seed).run(steps)
		})
	}
}
