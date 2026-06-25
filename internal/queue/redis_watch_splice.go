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
	"errors"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// watchDeletedEventType is the wire value the watch-state runner records for a watch.Deleted event
// (string(watch.Deleted)). Matched here so the fold can drop the identity without importing the
// apimachinery watch package.
const watchDeletedEventType = "DELETED"

// SpliceWatchType folds a type's checkpoint with the parallel :watch:stream into the WATCH-derived
// desired object set — the watch-side twin of SpliceType, and the read side of the Phase 1
// comparison (docs/design/watch-first-ingestion-architecture.md). It exists ONLY to be diffed against
// SpliceType's audit-derived set; it drives no Git write.
//
// It reads the checkpoint @R, then folds every :watch:stream entry whose recorded resourceVersion is
// strictly greater than R. The watch stream uses arrival-order auto-IDs (a watch restart can replay
// a resourceVersion, so an rv-keyed ID would be rejected), so the post-checkpoint slice is taken on
// the rv FIELD rather than the stream ID — mirroring SpliceType's exclusive "(R" XRANGE. A DELETED
// drops the identity; an ADDED/MODIFIED upserts the sanitized envelope object; last-writer-wins by
// stream (arrival) order, which for a single watch connection is resourceVersion order. A missing
// checkpoint returns ErrSpliceNoCheckpoint so the comparator skips the type rather than diffing
// against an empty set; a malformed entry is skipped best-effort (the next checkpoint backstops).
func (s *RedisTypeSplicer) SpliceWatchType(
	ctx context.Context, group, resource string,
) ([]*unstructured.Unstructured, string, error) {
	base := typeBaseKey(s.prefix, group, resource, "")

	rv, err := s.client.Get(ctx, base+objectsRVSuffix).Result()
	if errors.Is(err, redis.Nil) || rv == "" {
		return nil, "", ErrSpliceNoCheckpoint
	}
	if err != nil {
		return nil, "", fmt.Errorf("watch-splice: read checkpoint rv for %q: %w", base, err)
	}

	items, err := s.client.HGetAll(ctx, base+objectsItemsSuffix).Result()
	if err != nil {
		return nil, "", fmt.Errorf("watch-splice: read checkpoint items for %q: %w", base, err)
	}
	desired := make(map[string]*unstructured.Unstructured, len(items))
	for identity, envJSON := range items {
		if obj, decErr := decodeCheckpointObject(envJSON); decErr == nil {
			desired[identity] = obj
		}
	}

	msgs, err := s.client.XRange(ctx, base+byTypeWatchStreamSuffix, "-", "+").Result()
	if err != nil {
		return nil, "", fmt.Errorf("watch-splice: read watch log for %q: %w", base, err)
	}
	for i := range msgs {
		foldWatchEntry(desired, msgs[i].Values, rv)
	}

	return sortedDesiredObjects(desired), rv, nil
}

// foldWatchEntry applies one :watch:stream entry to the desired set, skipping any entry whose rv is
// at or below the checkpoint revision (already reflected in the checkpoint @R, mirroring SpliceType's
// "(R" fold). A DELETED drops the identity; an ADDED/MODIFIED decodes the sanitized envelope object
// and upserts it. A blank identity, an rv-covered entry, or an undecodable envelope is skipped.
func foldWatchEntry(desired map[string]*unstructured.Unstructured, values map[string]interface{}, checkpointRV string) {
	identity := watchEntryField(values, "identity")
	if identity == "" {
		return
	}
	if rvAtOrBelowCheckpoint(watchEntryField(values, "rv"), checkpointRV) {
		return
	}
	if watchEntryField(values, "event_type") == watchDeletedEventType {
		delete(desired, identity)
		return
	}
	obj, err := decodeCheckpointObject(watchEntryField(values, "envelope_json"))
	if err != nil {
		return
	}
	desired[identity] = obj
}

// watchEntryField reads one string field from an XRANGE entry's values (Redis returns stream fields
// as strings), returning "" when absent or non-string.
func watchEntryField(values map[string]interface{}, key string) string {
	if v, ok := values[key].(string); ok {
		return v
	}
	return ""
}

// rvAtOrBelowCheckpoint reports whether a watch entry's rv is already covered by the checkpoint @R
// (both numeric and entry <= R). A non-numeric or absent rv is folded best-effort (returns false),
// matching the audit splice's lenient handling of rv-less entries.
func rvAtOrBelowCheckpoint(entryRV, checkpointRV string) bool {
	e, eErr := strconv.ParseUint(entryRV, 10, 64)
	c, cErr := strconv.ParseUint(checkpointRV, 10, 64)
	if eErr != nil || cErr != nil {
		return false
	}
	return e <= c
}
