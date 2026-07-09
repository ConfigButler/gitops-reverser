// SPDX-License-Identifier: Apache-2.0

package git

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// refusedError is a write-plan refusal wrapped exactly as commitPendingWrites wraps it, so
// these tests also pin errors.As recovery through the wrap.
func refusedError() error {
	return fmt.Errorf("execute pending writes: %w", &manifestanalyzer.AcceptanceRefusedError{
		Issues: []manifestanalyzer.AcceptanceIssue{{
			Kind:    manifestanalyzer.IssueWriteFanIn,
			Path:    "base/deployment.yaml",
			Message: "write-fan-in must be 1",
		}},
	})
}

// captureRefusals installs a recording reporter on a bare worker.
func captureRefusals(w *BranchWorker) *[]types.ResourceReference {
	seen := &[]types.ResourceReference{}
	w.pathRefusal = func(target types.ResourceReference, _ *manifestanalyzer.AcceptanceRefusedError) {
		*seen = append(*seen, target)
	}
	return seen
}

// TestReportPathRefusal_ReportsAttributedRefusal is the happy path: a wrapped refusal with a
// fully named target reaches the reporter and tells the caller not to log a write fault.
func TestReportPathRefusal_ReportsAttributedRefusal(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard()}
	seen := captureRefusals(w)

	assert.True(t, w.reportPathRefusal(refusedError(), "podinfo-test", "team-a"))
	require.Len(t, *seen, 1)
	assert.Equal(t, types.NewResourceReference("podinfo-test", "team-a"), (*seen)[0])
}

// TestReportPathRefusal_PassesThroughNonRefusal proves a plain write fault is left to its
// existing error handling rather than being swallowed as a refusal.
func TestReportPathRefusal_PassesThroughNonRefusal(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard()}
	seen := captureRefusals(w)

	assert.False(t, w.reportPathRefusal(errors.New("remote hung up"), "podinfo-test", "team-a"))
	assert.Empty(t, *seen, "a transient write fault must not be reported as a Git path refusal")
}

// TestReportPathRefusal_UnattributableRefusalIsNotRecorded pins the guard: the acceptance map
// is keyed by "namespace/name", so a half-empty reference would file the refusal under a key no
// GitTarget reads, and every unattributable refusal would collide there. Refusing to guess is
// safer than mis-attributing — the caller is still told it was a refusal, not a write fault.
func TestReportPathRefusal_UnattributableRefusalIsNotRecorded(t *testing.T) {
	for _, c := range []struct{ name, ns string }{
		{"", ""},
		{"podinfo-test", ""},
		{"", "team-a"},
	} {
		w := &BranchWorker{Log: logr.Discard()}
		seen := captureRefusals(w)

		assert.True(t, w.reportPathRefusal(refusedError(), c.name, c.ns),
			"an unattributable refusal is still a refusal, not a write fault")
		assert.Empty(t, *seen,
			"a refusal with an incomplete target reference must never be recorded (%q/%q)", c.ns, c.name)
	}
}

// TestAtomicRefusalTarget_PrefersRequestThenEvents proves the atomic path names the right
// GitTarget. buildAtomicPendingWrite stamps request-level metadata onto every event, but only
// when the request carries it — a request whose events name their own target would otherwise
// have its refusal reported against an empty reference.
func TestAtomicRefusalTarget_PrefersRequestThenEvents(t *testing.T) {
	eventFor := func(name, namespace string) Event {
		return Event{GitTargetName: name, GitTargetNamespace: namespace}
	}

	cases := []struct {
		name          string
		request       *WriteRequest
		wantName      string
		wantNamespace string
	}{
		{
			name: "request-level metadata wins",
			request: &WriteRequest{
				GitTargetName: "from-request", GitTargetNamespace: "team-a",
				Events: []Event{eventFor("from-event", "team-b")},
			},
			wantName: "from-request", wantNamespace: "team-a",
		},
		{
			name: "falls back to the first event that names a target",
			request: &WriteRequest{
				Events: []Event{eventFor("", ""), eventFor("from-event", "team-b")},
			},
			wantName: "from-event", wantNamespace: "team-b",
		},
		{
			name: "a half-set request falls back rather than reporting a partial reference",
			request: &WriteRequest{
				GitTargetName: "from-request",
				Events:        []Event{eventFor("from-event", "team-b")},
			},
			wantName: "from-event", wantNamespace: "team-b",
		},
		{
			name:    "nothing names a target",
			request: &WriteRequest{Events: []Event{eventFor("", "")}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name, namespace := atomicRefusalTarget(c.request)
			assert.Equal(t, c.wantName, name)
			assert.Equal(t, c.wantNamespace, namespace)
		})
	}
}
