// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/reconcile"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// retire must wait out an enqueue already in flight, so that enqueue lands ahead of anything the
// replacement stream queues, and must admit nothing afterward.
func TestProducerGate_RetireWaitsForAnEnqueueInFlightAndAdmitsNothingAfter(t *testing.T) {
	gate := &producerGate{}
	entered, release := make(chan struct{}), make(chan struct{})
	var order []string
	go gate.enqueue(context.Background(), func() {
		close(entered)
		<-release
		order = append(order, "old")
	})
	<-entered

	retired := make(chan struct{})
	go func() {
		gate.retire()
		close(retired)
	}()
	select {
	case <-retired:
		t.Fatal("retire returned while an enqueue was still in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-retired
	order = append(order, "replacement")

	assert.False(t, gate.enqueue(context.Background(), func() { order = append(order, "late") }))
	assert.Equal(t, []string{"old", "replacement"}, order)
}

// One stream's gate never holds up another's: retiring or enqueuing on a collection is invisible to
// every other collection of the target.
func TestProducerGate_UnrelatedStreamsDoNotContend(t *testing.T) {
	busy, other := &producerGate{}, &producerGate{}
	entered, release := make(chan struct{}), make(chan struct{})
	go busy.enqueue(context.Background(), func() {
		close(entered)
		<-release
	})
	<-entered
	defer close(release)

	ran := make(chan struct{})
	go func() {
		other.enqueue(context.Background(), func() {})
		other.retire()
		close(ran)
	}()
	select {
	case <-ran:
	case <-time.After(time.Second):
		t.Fatal("an unrelated stream waited on another stream's gate")
	}
}

// blockingAuthorResolver parks an event in attribution until released, which is the widest window a
// live event spends between its context check and its enqueue.
type blockingAuthorResolver struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingAuthorResolver) ResolveAuthor(context.Context, AuthorQuery) (git.UserInfo, git.AttributionOutcome) {
	close(b.entered)
	<-b.release
	return git.UserInfo{}, git.AttributionUnresolved
}

// The replacement race the gate exists for: a retired producer paused mid-event must never enqueue
// behind the replacement's snapshot, where it would restore an object the new selection excluded.
// The event's context is deliberately NOT cancelled, so the retirement alone is what stops it.
func TestRouteLiveTargetWatchEvent_ARetiredProducerNeverEnqueuesAfterItsReplacement(t *testing.T) {
	gitDest := types.NewResourceReference("target", "default")
	enqueuer := &recordingEnqueuer{}
	stream := reconcile.NewGitTargetEventStream(gitDest.Name, gitDest.Namespace, enqueuer, logr.Discard())
	resolver := &blockingAuthorResolver{entered: make(chan struct{}), release: make(chan struct{})}
	manager := &Manager{
		EventRouter: &EventRouter{
			Log:              logr.Discard(),
			gitTargetStreams: map[string]*reconcile.GitTargetEventStream{gitDest.Key(): stream},
		},
		AuthorResolver: resolver,
	}

	old := testStream(targetWatchKey{GVR: configmapsGVR, Namespace: "apps", LabelSelector: "team in (a)"})
	set := manager.targetWatchSet(gitDest)
	set.streams[old.key.Collection()] = &runningTargetWatch{key: old.key, cancel: func() {}, gate: old.gate}

	done := make(chan error, 1)
	go func() {
		_, err := manager.routeLiveTargetWatchEvent(context.Background(), logr.Discard(), gitDest, old,
			watch.Event{Type: watch.Added, Object: configMapObject("12")})
		done <- err
	}()
	<-resolver.entered

	set.stop(old.key.Collection())
	replacement := testStream(targetWatchKey{GVR: configmapsGVR, Namespace: "apps", LabelSelector: "team in (b)"})
	require.True(t, replacement.gate.enqueue(context.Background(), func() {
		enqueuer.Enqueue(git.Event{Operation: "RECONCILE", SourceCollection: replacement.sourceCollection()})
	}))

	close(resolver.release)
	require.NoError(t, <-done)
	events := enqueuer.snapshot()
	require.Len(t, events, 1, "the retired producer's event must not land behind the replacement")
	assert.Equal(t, replacement.sourceCollection(), events[0].SourceCollection)
}
