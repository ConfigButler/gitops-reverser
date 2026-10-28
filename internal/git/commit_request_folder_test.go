// SPDX-License-Identifier: Apache-2.0

package git

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// These tests drive one branch worker through a CommitRequest saved the way a person saves one: on the
// worker's clock, with the wakes its timers would deliver, and with the gaps a real cluster has
// between writes and between a request's creation and its registration (its own audit event lags).
// Every push is made at once, so the commits on the remote are the ones the windows closed into.

// saveTarget is the commit window a GitTarget that declares none gets: the schema's defaults.
const (
	saveTargetIdle = 5 * time.Second
	saveTargetMax  = time.Minute
)

// saveRun is one worker driven on a test clock.
type saveRun struct {
	t      *testing.T
	worker *BranchWorker
	loop   *branchWorkerEventLoop
	clock  *testClock
	remote string
	base   string
	start  time.Time
}

func newSaveRun(t *testing.T) *saveRun {
	t.Helper()
	worker, _, remoteURL := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, crTarget, crTarget)
	clock := useTestClock(worker)
	loop := newBranchWorkerEventLoop(worker, saveTargetIdle)
	loop.defaultWindow.maxDuration = saveTargetMax
	t.Cleanup(loop.stopTimers)
	remote := strings.TrimPrefix(remoteURL, "file://")
	return &saveRun{
		t: t, worker: worker, loop: loop, clock: clock, remote: remote,
		base: gitIn(t, remote, "rev-parse", "main"), start: clock.Now(),
	}
}

// until steps the clock to at, from the run's start, delivering each timer wake as it comes due.
func (r *saveRun) until(at time.Duration) {
	const step = 50 * time.Millisecond
	for r.clock.Now().Sub(r.start)+step <= at {
		r.clock.advance(step)
		r.wake()
	}
	r.clock.advance(at - r.clock.Now().Sub(r.start))
	r.wake()
}

// untilResolved steps the clock until the request named name is resolved, for at most 30s.
func (r *saveRun) untilResolved(name string) {
	for range 600 {
		if _, ok := r.outcome(name); ok {
			return
		}
		r.until(r.clock.Now().Sub(r.start) + 50*time.Millisecond)
	}
}

func (r *saveRun) wake() {
	r.loop.lastPushAt = time.Time{}
	timerWake(r.loop)
}

func (r *saveRun) write(name string) {
	r.loop.lastPushAt = time.Time{}
	r.loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events: []Event{configMapTargetEvent(name, "alice", crTarget)},
	}})
}

// register registers a request with the schema's defaults: CurrentOrNext, attachTimeout 5s,
// maxDuration 2s and no idleTimeout.
func (r *saveRun) register(name, message string) {
	r.loop.lastPushAt = time.Time{}
	req := saveRequest(name, message)
	r.loop.handleQueueItem(WorkItem{Attach: req})
}

func saveRequest(name, message string) *AttachCommitRequest {
	return &AttachCommitRequest{
		Namespace: "default", Name: name, UID: "uid-" + name,
		Author: "alice", GitTargetName: crTarget, GitTargetNamespace: "default",
		Message:       message,
		Attach:        configv1alpha3.AttachCurrentOrNext,
		AttachTimeout: 5 * time.Second,
		MaxDuration:   2 * time.Second,
	}
}

func (r *saveRun) outcome(name string) (FinalizeResult, bool) {
	return r.worker.LookupCommitRequestOutcome("default", name, "uid-"+name)
}

// savedCommit is one commit on the remote: its message and the ConfigMaps it changes, by name.
type savedCommit struct {
	hash, author, message string
	configMaps            []string
}

// commits are the commits on the remote since the run started, oldest first.
func (r *saveRun) commits() []savedCommit {
	var out []savedCommit
	for _, hash := range splitLines(gitIn(r.t, r.remote, "rev-list", "--reverse", r.base+"..main")) {
		c := savedCommit{
			hash:    hash,
			author:  gitIn(r.t, r.remote, "log", "-1", "--format=%an", hash),
			message: gitIn(r.t, r.remote, "log", "-1", "--format=%B", hash),
		}
		for _, path := range splitLines(gitIn(r.t, r.remote, "diff-tree", "-r", "--name-only", "--no-commit-id", hash)) {
			if name, ok := strings.CutPrefix(path, crTarget+"/default/configmaps/"); ok {
				c.configMaps = append(c.configMaps, strings.TrimSuffix(name, ".yaml"))
			}
		}
		slices.Sort(c.configMaps)
		out = append(out, c)
	}
	return out
}

// folderStep is one thing kubectl apply -f folder/ makes reach the worker: a ConfigMap's write, or,
// with request set, the CommitRequest's registration.
type folderStep struct {
	at      time.Duration
	write   string
	request bool
}

// TestSaveFolder_AnAppliedFolderMakesOneCommitUnderTheRequestsMessage pins the folder scenario: a
// CommitRequest beside the objects it saves, applied with kubectl apply -f folder/, makes one commit
// of those objects, authored by the person, carrying the request's message, in both orders kubectl
// can apply them in.
func TestSaveFolder_AnAppliedFolderMakesOneCommitUnderTheRequestsMessage(t *testing.T) {
	const message = "feat: raise the checkout limits"
	cases := []struct {
		name  string
		steps []folderStep
	}{
		{
			// The request's own audit event lags, so it registers once both objects are in the window:
			// only CurrentOrNext attaches to it.
			name: "objects first, the request registers after both",
			steps: []folderStep{
				{at: 0, write: "app-config"},
				{at: 800 * time.Millisecond, write: "app-flags"},
				{at: 2 * time.Second, request: true},
			},
		},
		{
			name: "objects first, the request registers between them",
			steps: []folderStep{
				{at: 0, write: "app-config"},
				{at: 900 * time.Millisecond, request: true},
				{at: 1700 * time.Millisecond, write: "app-flags"},
			},
		},
		{
			name: "request first",
			steps: []folderStep{
				{at: 0, request: true},
				{at: 600 * time.Millisecond, write: "app-config"},
				{at: 1900 * time.Millisecond, write: "app-flags"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newSaveRun(t)
			for _, step := range tc.steps {
				r.until(step.at)
				if step.request {
					r.register(crName, message)
				} else {
					r.write(step.write)
				}
			}
			r.until(30 * time.Second)

			res, ok := r.outcome(crName)
			require.True(t, ok, "the request is resolved")
			require.NoError(t, res.Err)
			assert.Equal(t, FinalizeCommitted, res.Outcome)
			commits := r.commits()
			require.Len(t, commits, 1, "one commit: %+v", commits)
			assert.Equal(t, res.Commit, commits[0].hash)
			assert.Equal(t, "alice", commits[0].author)
			assert.Equal(t, message, commits[0].message)
			assert.Equal(t, []string{"app-config", "app-flags"}, commits[0].configMaps)
		})
	}
}
