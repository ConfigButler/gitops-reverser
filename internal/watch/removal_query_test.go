// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/ConfigButler/gitops-reverser/internal/queue"
	"github.com/ConfigButler/gitops-reverser/internal/reconcile"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// The author query for a removal is the same whether or not its stream carries a label selector.
// A DELETED frame from a selected stream may be a label exit, and the index tells that apart from a
// deletion by the shape of the facts alone. Only the object decides Terminating.
func TestRouteLiveTargetWatchEvent_RemovalQueryIgnoresTheSelector(t *testing.T) {
	terminating := configMapObject("12")
	terminating.SetDeletionTimestamp(&metav1.Time{Time: time.Unix(0, 0).UTC()})

	cases := []struct {
		name            string
		event           watch.Event
		wantTerminating bool
	}{
		{"DELETED", watch.Event{Type: watch.Deleted, Object: configMapObject("12")}, false},
		{"DELETED of a terminating object", watch.Event{Type: watch.Deleted, Object: terminating}, true},
		{"MODIFIED with deletionTimestamp", watch.Event{Type: watch.Modified, Object: terminating}, true},
	}
	for _, tc := range cases {
		var queries []queue.FactQuery
		for _, selector := range []string{teamA, ""} {
			gitDest := types.NewResourceReference("target", "default")
			enqueuer := &recordingEnqueuer{}
			stream := reconcile.NewGitTargetEventStream(gitDest.Name, gitDest.Namespace, enqueuer, logr.Discard())
			lookup := &fakeLookup{resolution: queue.AuthorResolution{Result: queue.AttributionAbsent}}
			manager := &Manager{
				EventRouter: &EventRouter{
					Log:              logr.Discard(),
					gitTargetStreams: map[string]*reconcile.GitTargetEventStream{gitDest.Key(): stream},
				},
				AuthorResolver: NewAuthorResolver(lookup, time.Millisecond, logr.Discard(), nil),
			}

			_, err := manager.routeLiveTargetWatchEvent(context.Background(), logr.Discard(), gitDest,
				testStream(targetWatchKey{GVR: configmapsGVR, Namespace: "apps", LabelSelector: selector}),
				tc.event)

			require.NoError(t, err, tc.name)
			assert.False(t, lookup.lastQuery.ExactCapable, tc.name)
			assert.Equal(t, tc.wantTerminating, lookup.lastQuery.Terminating, tc.name)
			queries = append(queries, lookup.lastQuery)
		}
		assert.Equal(t, queries[0], queries[1], "%s: a selector must not change the query", tc.name)
	}
}
