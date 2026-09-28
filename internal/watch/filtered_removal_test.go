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

// Only a DELETED frame from a selected collection can be a label exit, so only it takes the
// exact-evidence policy. An unselected removal and a MODIFIED carrying a deletionTimestamp keep the
// existing deletion attribution.
func TestRouteLiveTargetWatchEvent_OnlySelectedDeletionsUseTheFilteredRemovalPolicy(t *testing.T) {
	terminating := configMapObject("12")
	terminating.SetDeletionTimestamp(&metav1.Time{Time: time.Unix(0, 0).UTC()})

	cases := []struct {
		name            string
		selector        string
		event           watch.Event
		wantFiltered    bool
		wantTerminating bool
	}{
		{"selected DELETED", teamA, watch.Event{Type: watch.Deleted, Object: configMapObject("12")}, true, false},
		{"selected DELETED of a terminating object", teamA,
			watch.Event{Type: watch.Deleted, Object: terminating}, true, true},
		{"unselected DELETED", "", watch.Event{Type: watch.Deleted, Object: configMapObject("12")}, false, false},
		{"selected MODIFIED with deletionTimestamp", teamA,
			watch.Event{Type: watch.Modified, Object: terminating}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
				testStream(targetWatchKey{GVR: configmapsGVR, Namespace: "apps", LabelSelector: tc.selector}),
				tc.event)

			require.NoError(t, err)
			assert.Equal(t, tc.wantFiltered, lookup.lastQuery.FilteredRemoval)
			assert.Equal(t, tc.wantTerminating, lookup.lastQuery.Terminating)
		})
	}
}
