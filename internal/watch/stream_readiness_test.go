// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func TestStreamSummaryForTypes_AggregatesByType(t *testing.T) {
	configmaps := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	secrets := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	expected := []types.CellKey{
		types.CellKeyFor(configmaps, "a"),
		types.CellKeyFor(configmaps, "b"),
		types.CellKeyFor(secrets, "a"),
	}
	states := map[types.CellKey]targetStreamStatus{
		types.CellKeyFor(configmaps, "a"): {state: StreamStateStreaming},
		types.CellKeyFor(configmaps, "b"): {state: StreamStateReplaying, reason: StreamReasonInitialReplay},
		types.CellKeyFor(secrets, "a"):    {state: StreamStateStreaming},
	}

	summary := streamSummaryForTypes(expected, states, nil)

	assert.Equal(t, 2, summary.Total)
	assert.Equal(t, 1, summary.Ready)
	assert.Equal(t, 1, summary.Replaying)
	assert.Equal(t, StreamReasonReplaying, summary.Reason)
	assert.Equal(t, []string{"configmaps"}, summary.PendingSample)
}

func TestStreamSummaryForTypes_BlockedOutranksReplaying(t *testing.T) {
	configmaps := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	secrets := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	expected := []types.CellKey{types.CellKeyFor(configmaps, ""), types.CellKeyFor(secrets, "")}
	states := map[types.CellKey]targetStreamStatus{
		types.CellKeyFor(configmaps, ""): {state: StreamStateReplaying, reason: StreamReasonInitialReplay},
		types.CellKeyFor(secrets, ""):    {state: StreamStateBlocked, reason: StreamReasonWatchError},
	}

	summary := streamSummaryForTypes(expected, states, nil)

	assert.Equal(t, 2, summary.Total)
	assert.Equal(t, 1, summary.Blocked)
	assert.Equal(t, 1, summary.Replaying)
	assert.Equal(t, StreamReasonWatchError, summary.Reason)
	assert.False(t, summary.StreamsRunning())
}

// The regression this keying exists to prevent. A rule can match two SERVED VERSIONS of one
// resource (a `*` apiVersions wildcard over, say, autoscaling/v1 and autoscaling/v2), while the
// declared set runs exactly one stream for that cell. Expecting a per-version stream meant
// expecting one that by construction never exists, and the rule reported permanently
// not-ready while its stream ran perfectly.
func TestStreamSummaryForTypes_TwoServedVersionsAreOneStream(t *testing.T) {
	v1 := schema.GroupVersionResource{Group: "autoscaling", Version: "v1", Resource: "horizontalpodautoscalers"}
	v2 := schema.GroupVersionResource{Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"}
	expected := []types.CellKey{types.CellKeyFor(v1, "team-a"), types.CellKeyFor(v2, "team-a")}
	states := map[types.CellKey]targetStreamStatus{
		types.CellKeyFor(v2, "team-a"): {state: StreamStateStreaming},
	}

	summary := streamSummaryForTypes(expected, states, nil)

	assert.Equal(t, 1, summary.Total, "one cell is one stream, whatever versions serve it")
	assert.Equal(t, 1, summary.Ready)
	assert.True(t, summary.StreamsRunning())
}

// A stream reaching Streaming is the last thing that has to happen before a target and its rules
// can honestly report StreamsRunning=True, and it used to notify nobody: the data plane converged
// in about two seconds and status followed up to ten seconds later, on the settle requeue.
func TestMarkTargetStreamState_NotifiesOnTheTransitionOnly(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	gitDest := types.NewResourceReference("target", "team-a")
	cell := types.CellKeyFor(configmapsGVR, "apps")

	// Two subscribers, because a Go channel has one consumer and three controllers project this.
	ruleEvents := m.StreamStateEvents()
	clusterRuleEvents := m.StreamStateEvents()
	targetEvents := m.GitPathEvents()

	m.markTargetStreamState(gitDest, cell, StreamStateReplaying, StreamReasonInitialReplay, "replaying")
	for name, ch := range map[string]<-chan event.GenericEvent{
		"watch rules": ruleEvents, "cluster watch rules": clusterRuleEvents, "the GitTarget": targetEvents,
	} {
		select {
		case evt := <-ch:
			assert.Equal(t, "target", evt.Object.GetName(), "%s are told which target moved", name)
		default:
			t.Fatalf("%s were not notified of a stream-state transition", name)
		}
	}

	// The data plane reports readiness continuously. An event per REPORT rather than per CHANGE
	// would enqueue every rule of a target on every watch event it handles.
	m.markTargetStreamState(gitDest, cell, StreamStateReplaying, StreamReasonInitialReplay, "replaying")
	select {
	case <-ruleEvents:
		t.Fatal("a re-report of an unchanged state must not enqueue anything")
	default:
	}

	m.markTargetStreamState(gitDest, cell, StreamStateStreaming, StreamReasonAllStreamsReady, "streaming")
	select {
	case <-ruleEvents:
	default:
		t.Fatal("reaching Streaming is a transition and must be announced")
	}
}
