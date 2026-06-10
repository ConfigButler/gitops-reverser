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
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"k8s.io/apimachinery/pkg/runtime"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
)

const (
	// DefaultRedisByTypeStreamPrefix is the default root key prefix for the
	// per-resource-type experiment. Every key is "<prefix>:<group-or-core>:<resource>:…";
	// the audit mirror writes "…:audit:stream" and the objects snapshot writes
	// "…:objects:items" (etc.), with a ":__index__" set listing the per-type base keys.
	// See docs/design/stream/per-resource-type-rv-keyed-streams-experiment.md.
	DefaultRedisByTypeStreamPrefix = "gitops-reverser"

	// byTypeAuditStreamSuffix is appended to a type's base key for the audit event stream.
	byTypeAuditStreamSuffix = ":audit:stream"
	byTypeIndexSuffix       = ":__index__"
	byTypeUnknownBucket     = "__unknown__"
	byTypeCoreGroup         = "core"

	// streamIDTooSmallMarker is a substring of the XADD error returned by Redis
	// (and miniredis) when an explicit stream ID is not strictly greater than the
	// stream's current top entry. We fall back to a server-assigned ID in that case.
	streamIDTooSmallMarker = "equal or smaller"
)

// byTypeKeyDisallowed matches characters not allowed in a sanitized Redis key
// segment. Group/resource/subresource names are DNS-ish and lowercase already;
// this is a defensive scrub so an odd objectRef can never produce a weird key.
var byTypeKeyDisallowed = regexp.MustCompile(`[^a-z0-9._-]`)

// RedisByTypeStreamConfig configures the per-resource-type experiment streams.
type RedisByTypeStreamConfig struct {
	Addr       string
	Username   string
	AuthValue  string
	DB         int
	Prefix     string
	MaxLen     int64
	TLSEnabled bool
}

// RedisByTypeStreamQueue mirrors canonical audit events into one Redis stream per
// resource type at "<prefix>:<group-or-core>:<resource>:audit:stream". For each event
// it writes a single entry — the compact overview fields plus the full event JSON in a
// payload_json field — ID-prefixed by the event's stage timestamp in milliseconds, and
// records the type's base key in a ":__index__" set so the type keyspace can be
// enumerated later without SCAN. It is write-only: nothing reads these structures yet.
// See docs/design/stream/per-resource-type-rv-keyed-streams-experiment.md.
type RedisByTypeStreamQueue struct {
	client *redis.Client
	prefix string
	maxLen int64

	indexedMu sync.Mutex
	indexed   map[string]struct{}
}

// NewRedisByTypeStreamQueue creates a per-resource-type Redis stream mirror.
func NewRedisByTypeStreamQueue(cfg RedisByTypeStreamConfig) (*RedisByTypeStreamQueue, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		return nil, errors.New("redis address is required")
	}

	prefix := strings.TrimSpace(cfg.Prefix)
	if prefix == "" {
		prefix = DefaultRedisByTypeStreamPrefix
	}

	options := &redis.Options{
		Addr:     cfg.Addr,
		Username: cfg.Username,
		Password: cfg.AuthValue,
		DB:       cfg.DB,
	}
	if cfg.TLSEnabled {
		options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	return &RedisByTypeStreamQueue{
		client:  redis.NewClient(options),
		prefix:  prefix,
		maxLen:  cfg.MaxLen,
		indexed: make(map[string]struct{}),
	}, nil
}

// Enqueue mirrors one canonical audit event into its per-resource-type stream as a
// single entry. The error is returned so the caller can log/count it, but callers
// treat the mirror as best-effort: a failure here must not fail the audit request.
func (q *RedisByTypeStreamQueue) Enqueue(ctx context.Context, event auditv1.Event) error {
	base := q.baseKey(event)
	streamKey := base + byTypeAuditStreamSuffix
	millis := stageMillis(event)
	rv := resourceVersionFromEvent(event)

	if err := q.ensureIndexed(ctx, base); err != nil {
		return err
	}

	values, err := q.entryValues(event, millis, rv)
	if err != nil {
		return err
	}
	if err := q.xadd(ctx, streamKey, millis, rv, values); err != nil {
		return fmt.Errorf("failed to append entry to %q: %w", streamKey, err)
	}
	return nil
}

// xadd appends one entry, choosing the stream ID per streamIDCandidates: the event
// millisecond is the leading component and — as an experiment — the event's
// resourceVersion is folded into the sequence component in place of the usual auto "*",
// so the ID itself encodes (event-time, RV). Stream IDs must strictly increase, so on the
// "equal or smaller" rejection we try the next, looser candidate (auto sequence within the
// millisecond, then a fully server-assigned ID) and rely on the stage_millis/resource_version
// fields for the true order.
func (q *RedisByTypeStreamQueue) xadd(
	ctx context.Context,
	stream string,
	millis int64,
	rv string,
	values map[string]any,
) error {
	args := &redis.XAddArgs{Stream: stream, Values: values}
	if q.maxLen > 0 {
		args.MaxLen = q.maxLen
		args.Approx = true
	}

	var err error
	for _, id := range streamIDCandidates(millis, rv) {
		args.ID = id
		if _, err = q.client.XAdd(ctx, args).Result(); err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), streamIDTooSmallMarker) {
			return err
		}
		// The strict-increase constraint rejected this candidate; fall back to the next,
		// looser one. TODO: emit a metric here (e.g. telemetry.ByTypeStreamIDFallbackTotal,
		// labelled by the rejected candidate — <ms>-<rv> fold vs <ms>-* vs *) to measure how
		// often the RV-in-ID fold fails and we degrade. Answers the "ordering reality"
		// question in §9 of
		// docs/design/stream/per-resource-type-rv-keyed-streams-experiment.md.
	}
	return err
}

// streamIDCandidates lists the stream IDs to try, in increasing looseness. The leading
// millisecond is preserved where possible. The first candidate folds the event's
// resourceVersion into the sequence component ("<millis>-<rv>"); the next drops to an auto
// sequence within the same millisecond ("<millis>-*", e.g. when two events share an
// (ms, rv) — close deletecollections do — or rv is absent); the last is a fully
// server-assigned ID for a genuinely out-of-order (older) millisecond.
func streamIDCandidates(millis int64, rv string) []string {
	const maxCandidates = 3 // <ms>-<rv>, <ms>-*, *
	candidates := make([]string, 0, maxCandidates)
	if seq, ok := streamSeqFromRV(rv); ok {
		candidates = append(candidates, fmt.Sprintf("%d-%d", millis, seq))
	}
	return append(candidates, fmt.Sprintf("%d-*", millis), "*")
}

// streamSeqFromRV parses a resourceVersion into the uint64 sequence component of a stream ID.
// RV is the etcd revision (an int64 that fits a stream sequence); it is absent or non-numeric
// on deletes, collection verbs, and shallow bodies, in which case there is no RV to fold.
func streamSeqFromRV(rv string) (uint64, bool) {
	if rv == "" {
		return 0, false
	}
	seq, err := strconv.ParseUint(rv, 10, 64)
	if err != nil {
		return 0, false
	}
	return seq, true
}

// entryValues builds the single per-event entry: the compact, scannable summary
// fields plus the full event JSON in payload_json, so the overview and the body
// live in one row that can be both scanned and replayed. rv is passed in (already
// resolved by the caller) so the body is only probed for the resourceVersion once.
func (q *RedisByTypeStreamQueue) entryValues(event auditv1.Event, millis int64, rv string) (map[string]any, error) {
	raw, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal audit event payload: %w", err)
	}
	group, version, resource, subresource, namespace, name := objectRefParts(event.ObjectRef)
	return map[string]any{
		"audit_id":         string(event.AuditID),
		"stage":            string(event.Stage),
		"verb":             event.Verb,
		"api_group":        group,
		"api_version":      version,
		"resource":         resource,
		"subresource":      subresource,
		"namespace":        namespace,
		"name":             name,
		"resource_version": rv,
		"stage_millis":     millis,
		"user":             event.User.Username,
		"payload_json":     string(raw),
	}, nil
}

// ensureIndexed records the stream key in the index set the first time it is seen
// in this process, so the set of type streams can be listed later without a SCAN.
// The in-memory guard keeps it to one SADD per key rather than one per event.
func (q *RedisByTypeStreamQueue) ensureIndexed(ctx context.Context, streamKey string) error {
	q.indexedMu.Lock()
	_, seen := q.indexed[streamKey]
	q.indexedMu.Unlock()
	if seen {
		return nil
	}

	if err := q.client.SAdd(ctx, q.prefix+byTypeIndexSuffix, streamKey).Err(); err != nil {
		return fmt.Errorf("failed to register stream %q in index set: %w", streamKey, err)
	}

	q.indexedMu.Lock()
	q.indexed[streamKey] = struct{}{}
	q.indexedMu.Unlock()
	return nil
}

// baseKey is the per-type base key for an event: "<prefix>:<group-or-core>:<resource>".
// The audit stream is this key plus ":audit:stream". An event without an objectRef or
// resource collapses to a single "__unknown__" bucket.
func (q *RedisByTypeStreamQueue) baseKey(event auditv1.Event) string {
	if event.ObjectRef == nil {
		return typeBaseKey(q.prefix, "", "", "")
	}
	ref := event.ObjectRef
	return typeBaseKey(q.prefix, ref.APIGroup, ref.Resource, ref.Subresource)
}

// typeBaseKey is the shared per-type key both experiment sinks build on:
// "<prefix>:<group-or-core>:<resource>", with the audit mirror appending ":audit:stream"
// and the objects snapshot appending ":objects:items" (etc.). Colons separate the fixed
// segments, so any colon inside a name is scrubbed by sanitizeKeySegment; a subresource is
// folded onto the resource segment with a dot ("deployments.scale") to keep the segment
// count fixed. The core API group renders as "core"; a missing resource collapses to a
// single "__unknown__" bucket.
func typeBaseKey(prefix, group, resource, subresource string) string {
	if resource == "" {
		return prefix + ":" + byTypeUnknownBucket
	}
	if group == "" {
		group = byTypeCoreGroup
	}
	res := sanitizeKeySegment(resource)
	if subresource != "" {
		res += "." + sanitizeKeySegment(subresource)
	}
	return prefix + ":" + sanitizeKeySegment(group) + ":" + res
}

func sanitizeKeySegment(s string) string {
	return byTypeKeyDisallowed.ReplaceAllString(strings.ToLower(s), "_")
}

func objectRefParts(ref *auditv1.ObjectReference) (string, string, string, string, string, string) {
	if ref == nil {
		return "", "", "", "", "", ""
	}
	return ref.APIGroup, ref.APIVersion, ref.Resource, ref.Subresource, ref.Namespace, ref.Name
}

// stageMillis is the event's stage timestamp in Unix milliseconds — the primary
// ordering ("millisecond value first"). It falls back to the request-received
// timestamp, then to wall-clock, so the stream ID always has a usable leading value.
func stageMillis(event auditv1.Event) int64 {
	if !event.StageTimestamp.Time.IsZero() {
		return event.StageTimestamp.Time.UnixMilli()
	}
	if !event.RequestReceivedTimestamp.Time.IsZero() {
		return event.RequestReceivedTimestamp.Time.UnixMilli()
	}
	return time.Now().UnixMilli()
}

// resourceVersionFromEvent returns the event's ResourceVersion when one is available,
// or "" when it is not (deletes, collection verbs, shallow bodies). The post-write RV
// lives in the object body's metadata.resourceVersion; objectRef.resourceVersion is
// usually the empty precondition RV on writes, so it is only the last resort.
func resourceVersionFromEvent(event auditv1.Event) string {
	if rv := rvFromRawObject(event.ResponseObject); rv != "" {
		return rv
	}
	if rv := rvFromRawObject(event.RequestObject); rv != "" {
		return rv
	}
	if event.ObjectRef != nil {
		return event.ObjectRef.ResourceVersion
	}
	return ""
}

func rvFromRawObject(obj *runtime.Unknown) string {
	if obj == nil || len(obj.Raw) == 0 {
		return ""
	}
	var probe struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(obj.Raw, &probe); err != nil {
		return ""
	}
	return probe.Metadata.ResourceVersion
}
