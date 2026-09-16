// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// The deletion-as-intent rule says an object carrying a deletionTimestamp is logically absent from
// the intent tree: the live path already renders one as a DELETE however the event arrived. A
// desired snapshot is the same tree, gathered a different way, so it has to answer the same.
//
// It did not. A replay or a LIST fallback that ran while an object was still Terminating folded it
// into the desired set, and a resync applies desired entries as upserts — so an object the live
// path had already removed from Git came back, and a later resync removed it again once the
// finalizers cleared. Two spurious commits for a resource the user had deleted, and because
// sanitize strips deletionTimestamp, the committed manifest looked like an ordinary live resource.
func TestDesiredFromObject_SkipsTerminatingObject(t *testing.T) {
	_, ok := desiredFromObject(configmapsGVR, terminatingConfigMapObject("7"))

	assert.False(t, ok,
		"a Terminating object is absent from the intent tree and must not enter the desired set")
}

// The live rule does not care how the object arrived, and neither does this one: an object can be
// Terminating in an ADDED replay frame just as easily as in a MODIFIED one.
func TestFoldTargetReplayEvent_SkipsTerminatingObjects(t *testing.T) {
	for _, eventType := range []watch.EventType{watch.Added, watch.Modified} {
		t.Run(string(eventType), func(t *testing.T) {
			manager := &Manager{}
			key := targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}
			var desired []manifestanalyzer.DesiredResource

			done, rv, err := manager.foldTargetReplayEvent(
				logr.Discard(),
				types.NewResourceReference("target", "default"),
				testStream(key, nil),
				watch.Event{Type: eventType, Object: terminatingConfigMapObject("7")},
				&desired,
			)

			require.NoError(t, err)
			assert.False(t, done)
			assert.Empty(t, rv)
			assert.Empty(t, desired,
				"a Terminating object must not be folded into the replay's desired set")
		})
	}
}

// The LIST fallback is the other way a snapshot is gathered, and it runs on exactly the path this
// bug needs: a watch that could not resume. It must agree with the replay fold, or which one ran
// decides whether a deleted resource reappears in Git.
func TestDesiredFromList_ExcludesTerminatingItems(t *testing.T) {
	live := configMapObject("6")
	live.SetName("still-here")
	terminating := terminatingConfigMapObject("7")
	terminating.SetName("on-the-way-out")

	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*live, *terminating}}

	desired := desiredFromList(configmapsGVR, list)

	require.Len(t, desired, 1, "only the live object belongs in the desired set")
	assert.Equal(t, "still-here", desired[0].Resource.Name)
}

// A resync applies desired as upserts and sweeps what desired omits, so excluding a Terminating
// object is not merely "do not write it": it is what makes the snapshot REMOVE the file, matching
// what the live DELETE would have done. This pins the direction, since a filter that instead
// retained the entry would look equally "fixed" from the desiredFromObject test alone.
func TestDesiredFromObject_TerminatingObjectLeavesTheSetSoTheSweepDropsIt(t *testing.T) {
	deleted := terminatingConfigMapObject("7")
	deleted.SetName("gone")
	kept := configMapObject("6")
	kept.SetName("kept")

	var desired []manifestanalyzer.DesiredResource
	for _, obj := range []*unstructured.Unstructured{kept, deleted} {
		if item, ok := desiredFromObject(configmapsGVR, obj); ok {
			desired = append(desired, item)
		}
	}

	names := make([]string, 0, len(desired))
	for _, item := range desired {
		names = append(names, item.Resource.Name)
	}
	assert.Equal(t, []string{"kept"}, names,
		"the Terminating resource must be absent from desired, so the mark-and-sweep drops its file")
}

// A deletionTimestamp with no finalizers is the ordinary case — the object is moments from gone —
// and it is just as absent from the intent tree as one held by a finalizer.
//
// The timestamp has to be a real one. SetDeletionTimestamp(&metav1.Time{}) writes an EMPTY string
// (a zero metav1.Time marshals to ""), which GetDeletionTimestamp then reads back as nil — so an
// object built that way is not Terminating at all and would pass this test against the unfixed
// code. No real object can reach that state; the API server always writes an RFC3339 value.
func TestDesiredFromObject_SkipsTerminatingWithoutFinalizers(t *testing.T) {
	obj := configMapObject("8")
	obj.SetDeletionTimestamp(&metav1.Time{Time: time.Unix(0, 0).UTC()})
	obj.SetFinalizers(nil)
	require.NotNil(t, obj.GetDeletionTimestamp(), "the fixture must actually be Terminating")

	_, ok := desiredFromObject(configmapsGVR, obj)

	assert.False(t, ok, "a deletionTimestamp is the signal, not the finalizer that holds it")
}
