// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// The watch side of an apply window's barrier (internal/git/apply_barrier.go). A window whose
// operation completed asks, for each GitTarget its applier covers, to hear from every running stream
// once that stream has handled every write its collection held at that moment. The mark is the
// newest resourceVersion among the collection's objects, from a list made now: the position of the
// latest write to anything in it. A stream records how far it has got each time it records its
// cursor, after the event was routed, so the answer it sends from there rides the worker's queue
// behind every event it routed first.
//
// The mark is an object's resourceVersion, not the list's. The list's is the source cluster's
// current position, which a quiet stream only passes on a bookmark, and a bookmark can be a minute
// away. An object's is a write the stream will see, or has seen.
//
// resourceVersions are compared as numbers. The Kubernetes API leaves them opaque, and a source
// whose resourceVersions do not parse as one fails the barrier, so its window closes after its grace
// alone.

// barrierListPage is the page size of the list a barrier takes of a stream's collection.
const barrierListPage = 500

// streamProgress is how far one running target stream has got: the newest resourceVersion it has
// handled, an event or a bookmark, and the barriers waiting for it to get further.
type streamProgress struct {
	key targetWatchKey

	mu      sync.Mutex
	seen    uint64
	waiting []progressWaiter
}

// progressWaiter is one barrier waiting for a stream to reach mark.
type progressWaiter struct {
	mark    uint64
	deliver func()
}

// advance records that the stream handled rv, and answers every barrier it has now reached. A
// resourceVersion that is not a number advances nothing.
func (p *streamProgress) advance(rv string) {
	n, err := strconv.ParseUint(rv, 10, 64)
	if err != nil {
		return
	}
	p.mu.Lock()
	p.seen = max(p.seen, n)
	var due []func()
	kept := p.waiting[:0]
	for _, waiter := range p.waiting {
		if waiter.mark <= p.seen {
			due = append(due, waiter.deliver)
		} else {
			kept = append(kept, waiter)
		}
	}
	p.waiting = kept
	p.mu.Unlock()
	for _, deliver := range due {
		deliver()
	}
}

// await answers deliver once the stream has handled mark: at once when it already has.
func (p *streamProgress) await(mark uint64, deliver func()) {
	p.mu.Lock()
	if mark <= p.seen {
		p.mu.Unlock()
		deliver()
		return
	}
	p.waiting = append(p.waiting, progressWaiter{mark: mark, deliver: deliver})
	p.mu.Unlock()
}

// trackStreamProgress registers a running stream's progress, replacing a previous stream's for the
// same collection.
func (m *Manager) trackStreamProgress(gitDest types.ResourceReference, key targetWatchKey) *streamProgress {
	progress := &streamProgress{key: key}
	m.streamProgressMu.Lock()
	defer m.streamProgressMu.Unlock()
	if m.streamProgress == nil {
		m.streamProgress = map[string]map[types.CollectionKey]*streamProgress{}
	}
	streams := m.streamProgress[gitDest.Key()]
	if streams == nil {
		streams = map[types.CollectionKey]*streamProgress{}
		m.streamProgress[gitDest.Key()] = streams
	}
	streams[key.Collection()] = progress
	return progress
}

// untrackStreamProgress removes a stopped stream's progress, unless a newer stream replaced it. Its
// waiting barriers are never answered; their windows close at their limit.
func (m *Manager) untrackStreamProgress(gitDest types.ResourceReference, key targetWatchKey, progress *streamProgress) {
	m.streamProgressMu.Lock()
	defer m.streamProgressMu.Unlock()
	streams := m.streamProgress[gitDest.Key()]
	if streams[key.Collection()] != progress {
		return
	}
	delete(streams, key.Collection())
	if len(streams) == 0 {
		delete(m.streamProgress, gitDest.Key())
	}
}

// advanceStreamProgress records that the stream of gitDest's collection handled rv.
func (m *Manager) advanceStreamProgress(gitDest types.ResourceReference, key targetWatchKey, rv string) {
	if rv == "" {
		return
	}
	m.streamProgressMu.Lock()
	progress := m.streamProgress[gitDest.Key()][key.Collection()]
	m.streamProgressMu.Unlock()
	if progress != nil {
		progress.advance(rv)
	}
}

// ApplyBarrier is the watch side of an apply window's barrier (git.ApplyBarrierFunc): it marks every
// running stream of gitDest at the newest write its collection holds now, has each call deliver once
// it has handled that write, and returns how many streams will. It lists the source cluster, so it
// runs off the branch worker's loop.
func (m *Manager) ApplyBarrier(ctx context.Context, gitDest types.ResourceReference, deliver func()) (int, error) {
	m.streamProgressMu.Lock()
	streams := make([]*streamProgress, 0, len(m.streamProgress[gitDest.Key()]))
	for _, progress := range m.streamProgress[gitDest.Key()] {
		streams = append(streams, progress)
	}
	m.streamProgressMu.Unlock()

	clusterID := m.clusterIDForGitTarget(gitDest)
	for _, progress := range streams {
		mark, err := m.collectionMark(ctx, clusterID, progress.key)
		if err != nil {
			return 0, fmt.Errorf("mark %s for %s: %w", progress.key.Collection().String(), gitDest.String(), err)
		}
		progress.await(mark, deliver)
	}
	return len(streams), nil
}

// collectionMark is the newest resourceVersion among the objects of a stream's collection: the
// position of the latest write to any of them. An empty collection is zero.
func (m *Manager) collectionMark(ctx context.Context, clusterID string, key targetWatchKey) (uint64, error) {
	var mark uint64
	opts := metav1.ListOptions{Limit: barrierListPage}
	for {
		list, err := m.openTargetList(ctx, clusterID, key, opts)
		if err != nil {
			return 0, err
		}
		for i := range list.Items {
			rv := list.Items[i].GetResourceVersion()
			n, err := strconv.ParseUint(rv, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("resourceVersion %q of %s is not a number", rv, list.Items[i].GetName())
			}
			mark = max(mark, n)
		}
		if list.GetContinue() == "" {
			return mark, nil
		}
		opts.Continue = list.GetContinue()
	}
}
