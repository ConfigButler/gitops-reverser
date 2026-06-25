/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package watch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
)

// The parallel watch-state stream is Phase 1 of docs/design/watch-first-ingestion-architecture.md:
// the "build watch state in parallel" step. For every Synced type it holds a long-lived Kubernetes
// WATCH and records each ADDED/MODIFIED/DELETED into a per-type ":watch:stream", written ALONGSIDE
// (never instead of) the authoritative audit stream. Nothing downstream consumes it yet — the point
// is to compare the watch-derived desired set against the audit-derived one. It is therefore:
//
//   - OFF by default — m.WatchStateWriter is nil unless --watch-state-stream wires it;
//   - NEVER a correctness source — the checkpoint LIST still owns correctness, so a watch gap costs
//     only freshness on this experimental stream, healed by the next checkpoint re-anchor;
//   - lifecycle-twinned with the audit tail — started beside startTypeAuditTail (a type became
//     Synced) and stopped beside stopTypeAuditTail (the type was Released).
//
// This deliberately reintroduces a long-lived object watch that the api-source-of-truth reconcile
// (R3) removed — but only behind the flag, and only to feed the parallel capture.

const (
	// watchStateBackoff is the pause before re-opening a watch-state watch after it closes or errors,
	// so a flapping apiserver watch does not hot-loop. Mirrors auditTailBackoff.
	watchStateBackoff = 2 * time.Second
	// watchStateRelistThreshold is the number of consecutive abnormal sessions after which the runner
	// resets its resume cursor to "" (re-watch from the current state). It bounds spinning on an
	// un-resumable resourceVersion — e.g. a 410 Gone after etcd compaction — at the cost of a
	// freshness gap the next checkpoint re-anchor heals. Correctness is never this stream's job.
	watchStateRelistThreshold = 3
)

// errWatchStateClosed marks a watch-state session that ended because the server closed the result
// channel (a normal watch timeout/compaction restart), as opposed to a context cancellation. It
// counts toward the relist-reset threshold.
var errWatchStateClosed = errors.New("watch-state result channel closed")

// StateWriter records observed Kubernetes WATCH events into a type's parallel ":watch:stream"
// and drops that stream when the type is released. It is satisfied by queue.RedisByTypeStreamQueue
// and is optional on the Manager: nil (the default, unless --watch-state-stream is set) disables the
// parallel watch-state capture entirely. See docs/design/watch-first-ingestion-architecture.md.
type StateWriter interface {
	// AppendWatchEvent stores one watch event. envelopeJSON is the sanitized object envelope (the
	// same shape ":objects:items" stores) so the two are foldable; eventType is the watch.EventType
	// string. Best-effort from the caller's view.
	AppendWatchEvent(ctx context.Context, group, resource, eventType, identity, rv, envelopeJSON string) error
	// DeleteTypeWatchStream drops a released type's watch stream.
	DeleteTypeWatchStream(ctx context.Context, group, resource string) error
}

// startTypeWatchStream launches the per-type watch-state stream once for gvr, anchored at the
// type's checkpoint revision so it records every live event strictly after the checkpoint — the
// same anchor discipline as startTypeAuditTail. It is idempotent (a repeat call for a type already
// running is a no-op) and runs under a child of the driver's context so a Released type (or
// shutdown) cancels it. A nil writer (the flag off) is a no-op.
func (m *Manager) startTypeWatchStream(
	ctx context.Context, log logr.Logger, gvr schema.GroupVersionResource, sinceRV string,
) {
	if m.WatchStateWriter == nil {
		return
	}
	m.watchStatesMu.Lock()
	defer m.watchStatesMu.Unlock()
	if m.watchStates == nil {
		m.watchStates = map[schema.GroupVersionResource]context.CancelFunc{}
	}
	if _, running := m.watchStates[gvr]; running {
		return
	}
	wCtx, cancel := context.WithCancel(ctx) //nolint:gosec // cancel is stored below and invoked by stopTypeWatchStream
	m.watchStates[gvr] = cancel
	go m.runTypeWatchStream(wCtx, log, gvr, sinceRV)
	log.V(1).Info("watch-state stream started", "gvr", gvr.String(), "sinceRV", sinceRV)
}

// stopTypeWatchStream cancels and forgets a type's watch-state stream (the type was Released). It is
// a no-op when none is running.
func (m *Manager) stopTypeWatchStream(gvr schema.GroupVersionResource) {
	m.watchStatesMu.Lock()
	defer m.watchStatesMu.Unlock()
	if cancel, ok := m.watchStates[gvr]; ok {
		cancel()
		delete(m.watchStates, gvr)
	}
}

// deleteTypeWatchStream drops a released type's parallel watch stream keyspace, the watch-side twin
// of deleteTypeAuditKeys. Best-effort.
func (m *Manager) deleteTypeWatchStream(ctx context.Context, log logr.Logger, gvr schema.GroupVersionResource) {
	if m.WatchStateWriter == nil {
		return
	}
	if err := m.WatchStateWriter.DeleteTypeWatchStream(ctx, gvr.Group, gvr.Resource); err != nil {
		log.Error(err, "watch-state: delete stream failed", "gvr", gvr.String())
	}
}

// runTypeWatchStream re-opens a watch session for gvr until its context cancels, advancing a resume
// cursor across restarts and resetting to the live edge after repeated un-resumable failures.
func (m *Manager) runTypeWatchStream(
	ctx context.Context, log logr.Logger, gvr schema.GroupVersionResource, sinceRV string,
) {
	backoff := m.watchStateBackoffDuration()
	failures := 0
	for ctx.Err() == nil {
		next, err := m.watchStateSession(ctx, log, gvr, sinceRV)
		if ctx.Err() != nil {
			return
		}
		sinceRV = next
		if err == nil {
			failures = 0
		} else {
			failures++
			log.V(1).Info("watch-state session ended; will re-open",
				"gvr", gvr.String(), "resumeRV", sinceRV, "failures", failures, "err", err.Error())
			if failures >= watchStateRelistThreshold {
				// An un-resumable resume point (e.g. 410 Gone after compaction): drop to the live edge.
				// The freshness gap is healed by the next checkpoint re-anchor; this stream is never a
				// correctness source.
				log.V(1).Info("watch-state stream resetting to live edge", "gvr", gvr.String())
				sinceRV = ""
				failures = 0
			}
		}
		if !sleepOrDone(ctx, backoff) {
			return
		}
	}
}

// watchStateSession opens one watch from sinceRV and folds events until the channel closes, an
// error event arrives, or the context cancels. It returns the highest resourceVersion it observed —
// the resume cursor for the next session — and a non-nil error when the session ended abnormally
// (channel close or a watch.Error), so the caller can count it toward the relist reset.
func (m *Manager) watchStateSession(
	ctx context.Context, log logr.Logger, gvr schema.GroupVersionResource, sinceRV string,
) (string, error) {
	opts := metav1.ListOptions{AllowWatchBookmarks: true}
	if sinceRV != "" {
		opts.ResourceVersion = sinceRV
	}
	w, err := m.openWatchStateWatch(ctx, gvr, opts)
	if err != nil {
		if ctx.Err() != nil {
			return sinceRV, nil
		}
		return sinceRV, fmt.Errorf("open watch-state watch %s: %w", gvr.String(), err)
	}
	defer w.Stop()

	resume := sinceRV
	for {
		select {
		case <-ctx.Done():
			return resume, nil
		case ev, ok := <-w.ResultChan():
			if !ok {
				return resume, errWatchStateClosed
			}
			rv, ferr := m.foldWatchStateEvent(ctx, log, gvr, ev)
			if ferr != nil {
				return resume, ferr
			}
			if rv != "" {
				resume = rv
			}
		}
	}
}

// foldWatchStateEvent records one watch event to the writer and returns the resourceVersion to
// advance the resume cursor to. A BOOKMARK advances the cursor without recording (its sole purpose).
// A watch.Error ends the session so the runner re-opens. A non-object or unmarshalable event is
// skipped (logged) but still advances the cursor, matching the checkpoint fold's leniency.
func (m *Manager) foldWatchStateEvent(
	ctx context.Context, log logr.Logger, gvr schema.GroupVersionResource, ev watch.Event,
) (string, error) {
	switch ev.Type {
	case watch.Bookmark:
		if u, ok := ev.Object.(*unstructured.Unstructured); ok {
			return u.GetResourceVersion(), nil
		}
		return "", nil
	case watch.Added, watch.Modified, watch.Deleted:
		u, ok := ev.Object.(*unstructured.Unstructured)
		if !ok {
			log.V(1).Info("watch-state: non-unstructured event skipped",
				"gvr", gvr.String(), "type", string(ev.Type))
			return "", nil
		}
		envelope, mErr := objectEnvelopeJSON(gvr, u)
		if mErr != nil {
			log.Error(mErr, "watch-state: marshal failed", "gvr", gvr.String(), "object", objectIdentity(u))
			return u.GetResourceVersion(), nil
		}
		if err := m.WatchStateWriter.AppendWatchEvent(
			ctx, gvr.Group, gvr.Resource, string(ev.Type), objectIdentity(u), u.GetResourceVersion(), envelope,
		); err != nil {
			log.V(1).Info("watch-state: append failed", "gvr", gvr.String(), "err", err.Error())
		}
		return u.GetResourceVersion(), nil
	case watch.Error:
		return "", fmt.Errorf("watch-state error event for %s: %v", gvr.String(), ev.Object)
	default:
		return "", nil
	}
}

// openWatchStateWatch opens the long-lived watch for the parallel stream, honoring the
// m.watchStateOpen test seam when set (the dynamic fake client's watch support is limited). nil →
// build it from the dynamic client.
func (m *Manager) openWatchStateWatch(
	ctx context.Context, gvr schema.GroupVersionResource, opts metav1.ListOptions,
) (watch.Interface, error) {
	if m.watchStateOpen != nil {
		return m.watchStateOpen(ctx, gvr, opts)
	}
	dc := m.dynamicClientFromConfig(m.Log)
	if dc == nil {
		return nil, errors.New("no dynamic client for watch-state stream")
	}
	return dc.Resource(gvr).Watch(ctx, opts)
}

// watchStateBackoffDuration returns the configured restart backoff, honoring the test override.
func (m *Manager) watchStateBackoffDuration() time.Duration {
	if m.watchStateBackoffOverride > 0 {
		return m.watchStateBackoffOverride
	}
	return watchStateBackoff
}
