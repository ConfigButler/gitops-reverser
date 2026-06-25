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

package queue

import (
	"context"
	"fmt"
	"time"
)

// byTypeWatchStreamSuffix is the parallel watch-derived state log: one entry per observed
// Kubernetes WATCH event for a claimed type, written ALONGSIDE the authoritative ":audit:stream"
// at "<prefix>:<group-or-core>:<resource>:watch:stream". It is Phase 1 of
// docs/design/watch-only-ingestion-architecture.md — a parallel capture that lets the
// watch-derived desired object set be diffed against the audit-derived one WITHOUT changing any
// Git write. It is off by default (the Manager's WatchStateWriter is nil unless --watch-state-stream
// is set) and is never a correctness input.
const byTypeWatchStreamSuffix = ":watch:stream"

// AppendWatchEvent appends one observed watch event to a type's parallel ":watch:stream".
//
// eventType is the watch.EventType string ("ADDED"/"MODIFIED"/"DELETED"); identity is
// "<namespace>/<name>" (or "<name>" for cluster-scoped); rv is the object's resourceVersion;
// envelopeJSON is the SAME sanitized object envelope the ":objects:items" checkpoint stores
// (built by the watch package's objectEnvelopeJSON), so a later splice can fold checkpoint and
// watch log byte-identically.
//
// Entries use auto-IDs ("*") rather than the audit stream's RV-keyed "<rv>-<subseq>": a single
// watch connection already delivers events in resourceVersion order, and an auto-ID never trips
// the strong-key "equal or smaller" rejection that the RV-first audit stream relies on (a watch
// restart can legitimately replay a resourceVersion). The rv is kept as a field so the consumer
// folds last-writer-wins by rv regardless of stream-ID. Best-effort: a write error is returned
// for the caller to log, never to disturb the authoritative audit path.
func (q *RedisByTypeStreamQueue) AppendWatchEvent(
	ctx context.Context, group, resource, eventType, identity, rv, envelopeJSON string,
) error {
	stream := typeBaseKey(q.prefix, group, resource, "") + byTypeWatchStreamSuffix
	values := map[string]any{
		"event_type":      eventType,
		"identity":        identity,
		"rv":              rv,
		"envelope_json":   envelopeJSON,
		"observed_millis": time.Now().UnixMilli(),
	}
	if _, err := q.xaddID(ctx, stream, "*", values); err != nil {
		return fmt.Errorf("append watch event to %q: %w", stream, err)
	}
	return nil
}

// DeleteTypeWatchStream drops a released type's parallel watch stream, the watch-side twin of
// DeleteType's audit cleanup. Best-effort from the caller's view.
func (q *RedisByTypeStreamQueue) DeleteTypeWatchStream(ctx context.Context, group, resource string) error {
	stream := typeBaseKey(q.prefix, group, resource, "") + byTypeWatchStreamSuffix
	if err := q.client.Del(ctx, stream).Err(); err != nil {
		return fmt.Errorf("delete watch stream %q: %w", stream, err)
	}
	return nil
}
