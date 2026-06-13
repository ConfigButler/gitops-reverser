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

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/auditutil"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

const (
	// DefaultRedisByTypeStreamPrefix is the default root key prefix for the
	// per-resource-type experiment. Every key is "<prefix>:<group-or-core>:<resource>:…";
	// the audit mirror writes "…:audit:stream" (plus "…:audit:late"/"…:audit:idstate") and
	// the objects snapshot writes "…:objects:items" (etc.), with a ":__index__" set listing
	// the per-type base keys.
	// See docs/design/stream/per-resource-type-rv-keyed-streams-experiment.md and
	// docs/design/stream/audit-log-ingestion-and-ordering.md.
	DefaultRedisByTypeStreamPrefix = "gitops-reverser"

	// byTypeAuditStreamSuffix is the strictly RV-ordered main stream: IDs are
	// "<resourceVersion>-<subseq>" (IR2). byTypeAuditLateSuffix is the diagnostic late lane
	// for events that would break that order (IR4). byTypeAuditIDStateSuffix is the per-type
	// observability hash — high-water mark plus counters (IR7).
	byTypeAuditStreamSuffix  = ":audit:stream"
	byTypeAuditLateSuffix    = ":audit:late"
	byTypeAuditIDStateSuffix = ":audit:idstate"
	byTypeIndexSuffix        = ":__index__"
	byTypeUnknownBucket      = "__unknown__"
	byTypeCoreGroup          = "core"

	// streamIDTooSmallMarker is a substring of the XADD error returned by Redis (and
	// miniredis) when an explicit stream ID is not strictly greater than the stream's current
	// top entry. For the RV-first main stream this rejection is the signal that an event is
	// strictly older than the high-water mark, so we divert it to the late lane (§6).
	streamIDTooSmallMarker = "equal or smaller"
)

// Stream-entry and idstate field names. The entry fields extend the compact overview
// (entryValues) with the routing decision; the idstate fields match the hash documented in
// docs/design/stream/audit-log-ingestion-and-ordering.md §4.
const (
	entryFieldRVPresent = "rv_present"
	entryFieldPlacement = "placement"
	entryFieldReason    = "reason"
	entryFieldLastRV    = "last_rv"

	placementResourceVersion  = "resource-version"
	placementAttachedToLastRV = "attached-to-last-rv"
	placementLateLane         = "late-lane"

	lateReasonOlderThanHighWater       = "older-than-high-water"
	lateReasonRVMissingBeforeHighWater = "rv-missing-before-high-water"
	lateReasonNonNumericRV             = "non-numeric-rv"

	idStateLastRV         = "lastRV"
	idStateLastStreamID   = "lastStreamID"
	idStateLastEventAt    = "lastEventAt"
	idStateMainCount      = "mainCount"
	idStateLateCount      = "lateCount"
	idStateRVMissingCount = "rvMissingCount"
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
	// PoolSize overrides the go-redis connection-pool size (0 = library default, 10×GOMAXPROCS).
	// The audit TAIL reader needs a large pool: every followed type runs one tail parked in a
	// blocking XREAD, so it holds one connection continuously, and a wildcard GitTarget follows
	// dozens of types at once. Sizing the reader's pool above that count keeps those blocking
	// reads from starving each other — and, by giving the reader its OWN client, keeps them from
	// ever starving the mirror's writes (which run on a separate, default-pooled client).
	PoolSize int
}

// RedisByTypeStreamQueue mirrors canonical audit events into one strictly RV-ordered Redis
// stream per resource type at "<prefix>:<group-or-core>:<resource>:audit:stream". Each event
// becomes one entry — the compact overview fields plus the full event JSON in a payload_json
// field — keyed "<resourceVersion>-<subseq>" so the stream replays in etcd-commit order (IR2).
// An event whose RV is strictly below the stream's high-water mark is never forced into the
// main stream; it is diverted to the diagnostic late lane ":audit:late" (IR3/IR4). An RV-less
// event attaches to the high-water mark, and per-type counters/high-water live in the
// ":audit:idstate" hash (IR5/IR7). The type's base key is recorded in a ":__index__" set so the
// keyspace can be enumerated without SCAN. Routing is atomic per XADD with no Lua and best-effort
// throughout (IR8/IR9).
// See docs/design/stream/audit-log-ingestion-and-ordering.md.
type RedisByTypeStreamQueue struct {
	client *redis.Client
	prefix string
	maxLen int64

	indexedMu sync.Mutex
	indexed   map[string]struct{}

	// lateNotify, when set, is called after an ordered (numeric-RV) event is diverted to
	// the late lane because its RV was strictly below the stream's high-water. The late
	// lane is diagnostic-only — the ordered log never replays the event — so the notifier
	// lets the materialization layer pull the next checkpoint forward instead of leaving
	// the mirror stale until the periodic sweep. Best-effort: it must be fast and
	// non-blocking (it runs on the audit ingest path).
	lateNotify func(group, resource string)
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
	if cfg.PoolSize > 0 {
		options.PoolSize = cfg.PoolSize
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

// Ping checks liveness of the underlying Redis/Valkey connection with a single PING. It is used
// by the audit-pipeline readiness gate at startup: the pod stays not-ready until the first PING
// succeeds, so it never joins the audit Service's endpoints before it can enqueue events. The
// error is returned verbatim so the caller can surface it in the readiness reason.
func (q *RedisByTypeStreamQueue) Ping(ctx context.Context) error {
	return q.client.Ping(ctx).Err()
}

// SetLateEventNotifier wires the late-lane diversion hook; see the lateNotify field.
func (q *RedisByTypeStreamQueue) SetLateEventNotifier(notify func(group, resource string)) {
	q.lateNotify = notify
}

// Enqueue mirrors one canonical audit event into its per-resource-type audit log, routed by
// the event's resourceVersion (§9): a numeric RV at or above the stream's high-water lands in
// the strictly-ordered main stream as "<rv>-<subseq>"; a strictly-older RV is diverted to the
// diagnostic late lane; an RV-less event attaches to the high-water mark; a present-but-non-
// numeric RV is diverted to the late lane. The error is returned so the caller can log/count
// it, but callers treat the mirror as best-effort: a failure here must not fail the audit
// request (IR8).
func (q *RedisByTypeStreamQueue) Enqueue(ctx context.Context, event auditv1.Event) error {
	base := q.baseKey(event)
	millis := stageMillis(event)
	rv := resourceVersionFromEvent(event)

	if err := q.ensureIndexed(ctx, base); err != nil {
		return err
	}

	values, err := q.entryValues(event, millis, rv)
	if err != nil {
		return err
	}

	keys := byTypeAuditKeys{
		stream:  base + byTypeAuditStreamSuffix,
		late:    base + byTypeAuditLateSuffix,
		idState: base + byTypeAuditIDStateSuffix,
	}
	if ref := event.ObjectRef; ref != nil &&
		(ref.Subresource == "" || auditutil.IsScaleSubresource(ref.Subresource)) {
		// A scale entry lives in the parent's stream (DEC-A), so a late-diverted scale
		// nudges the PARENT type's resync like any other parent write.
		keys.group = ref.APIGroup
		keys.resource = ref.Resource
	}
	switch classifyRV(rv) {
	case rvNumeric:
		return q.ingestOrdered(ctx, keys, rv, millis, values)
	case rvAbsent:
		return q.ingestRVLess(ctx, keys, millis, values)
	case rvNonNumeric:
		return q.divertLate(ctx, keys, lateReasonNonNumericRV, rv, "", values)
	}
	// classifyRV returns only the three cases above; this is unreachable.
	return nil
}

// TrimTypeAuditLog bounds a type's main audit stream to the checkpoint cursor: it evicts every
// entry whose resourceVersion is strictly below minRV (XTRIM ... MINID), so a reconcile never
// scans more than one checkpoint interval of history. It is the trim half of R1, called on each
// successful checkpoint re-anchor with the oldest currently-serving checkpoint rv (the §6
// trim-cursor model of docs/design/stream/api-source-of-truth-reconcile.md) — normally the just-
// pinned :objects:rv, since a single checkpoint serves a type.
//
// minRV is a bare resourceVersion; Valkey completes it to "<minRV>-0", so an entry at exactly
// minRV (any subseq) is KEPT while everything strictly older is dropped. That is deliberately one
// notch conservative: the splice replays the log with an exclusive "(R +" range, so it skips the
// rv==R entries the checkpoint already contains, and they age out at the next re-anchor. Trimming
// is safe, never lossy — a reconcile that finds the log trimmed past its cursor simply re-reads
// the checkpoint and folds forward from there. A blank minRV is a no-op (nothing to anchor to).
func (q *RedisByTypeStreamQueue) TrimTypeAuditLog(ctx context.Context, group, resource, minRV string) error {
	if strings.TrimSpace(minRV) == "" {
		return nil
	}
	streamKey := typeBaseKey(q.prefix, group, resource, "") + byTypeAuditStreamSuffix
	if err := q.client.XTrimMinID(ctx, streamKey, minRV).Err(); err != nil {
		return fmt.Errorf("failed to trim audit stream %q to minID %q: %w", streamKey, minRV, err)
	}
	return nil
}

// byTypeAuditReadCount bounds how many new entries one ReadTypeAuditChanges read drains.
const byTypeAuditReadCount = 1024

// ReadTypeAuditChanges blocks up to `block` for new entries on a type's main audit stream after
// lastID, parses each into a per-event git.Event, and returns them with the newest stream ID to
// resume from. It is the freshness half of the R3 split: the per-type tail loops on it and applies
// each change as a sweep-free UPSERT/DELETE (the old per-event apply, re-sourced from the per-type
// stream), so a partial burst can never delete an unseen object — correctness (orphan/missed-delete
// detection) is the checkpoint sweep's job, not the log's. lastID "" or "$" anchors at the present;
// thereafter the caller passes back the returned concrete ID so no entry is missed between reads.
//
// Each returned event carries Object (for an upsert), just the Identifier (for a DELETE), or a
// FieldPatch (for a parent-stream scale entry, DEC-A), with no Path/GitTarget set — the tail fills
// those per watching GitTarget. Entries that are non-mutating, a non-scale subresource, name-less
// (a deletecollection — backstopped by the next checkpoint, DEC-5), or whose body is a
// Status/partial/missing object are skipped here and healed by the next checkpoint. A block
// timeout returns (nil, lastID, nil); a Redis error is returned for the caller to back off on.
func (q *RedisByTypeStreamQueue) ReadTypeAuditChanges(
	ctx context.Context,
	group, resource, lastID string,
	block time.Duration,
) ([]git.Event, string, error) {
	if strings.TrimSpace(lastID) == "" {
		lastID = "$"
	}
	streamKey := typeBaseKey(q.prefix, group, resource, "") + byTypeAuditStreamSuffix
	res, err := q.client.XRead(ctx, &redis.XReadArgs{
		Streams: []string{streamKey, lastID},
		Count:   byTypeAuditReadCount,
		Block:   block,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, lastID, nil // block elapsed with nothing new
	}
	if err != nil {
		return nil, lastID, fmt.Errorf("read audit changes on %q: %w", streamKey, err)
	}

	newID := lastID
	var changes []git.Event
	for i := range res {
		for j := range res[i].Messages {
			msg := res[i].Messages[j]
			newID = msg.ID
			if ev, ok := auditChangeFromEntry(msg.Values); ok {
				// Stamp the FULL stream position "<rv>-<seq>" so the tail fan-out's per-(GitTarget,
				// GVR) coverage-watermark gate can classify it as historical (id <= Hc) or live
				// (id > Hc). Keeping the sub-sequence is what makes the gate safe for the cases where
				// several entries share an rv (rv-less DELETE/Status, duplicate or same-rv writes);
				// the stream position IS the rv-ordered position (DEC-3), so this is exact.
				ev.AuditStreamID = msg.ID
				changes = append(changes, ev)
			}
		}
	}
	return changes, newID, nil
}

// auditChangeFromEntry parses one per-type audit-stream entry into a per-event git.Event (no Path /
// GitTarget — the tail fills those), reusing the consumer's parse/extract path so a spliced upsert
// is byte-identical to what the live pipeline wrote. It returns ok=false for entries the incremental
// freshness path must not act on (see ReadTypeAuditChanges); their correctness is the checkpoint's.
func auditChangeFromEntry(values map[string]interface{}) (git.Event, bool) {
	event, err := parseAuditEvent(values)
	if err != nil {
		return git.Event{}, false
	}
	if event.Stage != auditv1.StageResponseComplete || event.ObjectRef == nil {
		return git.Event{}, false
	}
	op, ok := auditutil.VerbToOperation(event.Verb)
	if !ok {
		return git.Event{}, false
	}
	op = effectiveAuditOperation(event, op)

	ref := event.ObjectRef
	if ref.Subresource != "" && !auditutil.IsScaleSubresource(ref.Subresource) {
		return git.Event{}, false
	}
	id := auditutil.IdentityFromAuditEvent(event, op)
	if id.Name == "" {
		return git.Event{}, false
	}
	apiGroup, apiVersion := auditutil.ObjectRefGroupVersion(ref)
	identifier := itypes.NewResourceIdentifier(apiGroup, apiVersion, ref.Resource, id.Namespace, id.Name)
	userInfo := resolveUserInfo(event)

	if ref.Subresource != "" {
		// A parent-stream scale entry (DEC-A, canonical-stream-retirement.md) becomes the
		// same parent-manifest replicas field patch the canonical consumer used to build —
		// the writer's applyFieldPatch does the rest; only the source stream changed. A
		// scale whose parent replica path is unknown (CRD/aggregated parent) is dropped
		// here, never guessed, and the next checkpoint backstops it.
		assignments, _, ok := translateScaleToAssignments(event, apiGroup, ref.Resource)
		if !ok {
			return git.Event{}, false
		}
		return git.Event{
			FieldPatch: &git.FieldPatch{
				Assignments: assignments,
				Source:      ref.Resource + "/" + ref.Subresource,
			},
			Identifier: identifier,
			Operation:  string(op),
			UserInfo:   userInfo,
		}, true
	}

	if op == configv1alpha1.OperationDelete {
		return git.Event{Identifier: identifier, Operation: string(op), UserInfo: userInfo}, true
	}

	fullAPIVersion := apiVersion
	if apiGroup != "" {
		fullAPIVersion = apiGroup + "/" + apiVersion
	}
	obj, err := extractObject(event, op, fullAPIVersion, ref.Resource, id.Namespace, id.Name)
	if err != nil {
		return git.Event{}, false
	}
	return git.Event{Object: obj, Identifier: identifier, Operation: string(op), UserInfo: userInfo}, true
}

// byTypeAuditKeys bundles the three per-type audit keys one Enqueue touches, plus the
// raw (group, resource) identity they were derived from, so the late-lane hook can name
// the type without re-parsing a key.
type byTypeAuditKeys struct {
	stream  string // the strictly RV-ordered main stream
	late    string // the diagnostic late lane
	idState string // the observability hash (high-water + counters)

	group    string
	resource string
}

// rvClass partitions a resourceVersion into the three ingestion branches of §9.
type rvClass int

const (
	rvAbsent     rvClass = iota // no usable RV (deletes, collection verbs, shallow bodies)
	rvNumeric                   // a non-negative decimal integer ≤ 2^64-1: a valid stream-ID component
	rvNonNumeric                // present but not a valid stream-ID component (aggregated apiservers)
)

// classifyRV decides which branch an RV takes. The numeric test is exactly the stream-ID
// admission rule — a non-negative decimal integer that fits uint64 — so a value we classify
// numeric is one Valkey will accept as the "<rv>" component of an ID. This is validation, not
// comparison: in the baseline the RV ordering is delegated to Valkey's native 64-bit ID
// ordering (the strong key), so we never compare RVs via a lossy tonumber and need no
// decimal-string compare of our own (IR6).
func classifyRV(rv string) rvClass {
	if rv == "" {
		return rvAbsent
	}
	if _, err := strconv.ParseUint(rv, 10, 64); err != nil {
		return rvNonNumeric
	}
	return rvNumeric
}

// ingestOrdered writes a numeric-RV event to the main stream as "<rv>-*" and lets the strong
// key arbitrate (P2): an RV at or above the high-water is accepted and Valkey allocates the
// subseq, disambiguating events at the same RV (IR2); a strictly-older RV is rejected with
// streamIDTooSmallMarker and diverted to the late lane (IR3/IR4). The routing is atomic per
// XADD with no Lua and no read-then-write — Valkey's native 64-bit ID ordering is the arbiter,
// so we never need a lossy tonumber or a decimal-string compare of our own (IR6). Only the
// idstate counters/cache are best-effort and self-correcting (§9).
func (q *RedisByTypeStreamQueue) ingestOrdered(
	ctx context.Context,
	keys byTypeAuditKeys,
	rv string,
	millis int64,
	values map[string]any,
) error {
	values[entryFieldRVPresent] = "true"
	values[entryFieldPlacement] = placementResourceVersion

	id, err := q.xaddID(ctx, keys.stream, rv+"-*", values)
	switch {
	case err == nil:
		q.recordMain(ctx, keys.idState, rv, id, millis)
		return nil
	case isIDTooSmall(err):
		// The strong key rejected a strictly-older RV — the late-lane signal (P2). The high-water
		// for the diagnostic payload is the stream's authoritative top (§10). The ordered log will
		// never replay this event, so nudge the materialization layer to pull the type's next
		// checkpoint forward (the notifier is best-effort; the periodic sweep stays the backstop).
		divertErr := q.divertLate(ctx, keys, lateReasonOlderThanHighWater, rv, q.streamTopRV(ctx, keys.stream), values)
		if divertErr == nil && q.lateNotify != nil && keys.resource != "" {
			q.lateNotify(keys.group, keys.resource)
		}
		return divertErr
	default:
		return fmt.Errorf("failed to append entry to %q: %w", keys.stream, err)
	}
}

// ingestRVLess places an event that carries no usable RV (IR5). It attaches to the stream's
// current high-water mark — "<highWaterRV>-*", which Valkey accepts as a fresh subseq at the
// top RV — and marks it rv_present=false so a consumer knows it is a declared policy placement,
// not a claimed RV; the next checkpoint backstops it. Before any high-water exists there is
// nothing to attach to, so the event is recorded in the late lane instead.
func (q *RedisByTypeStreamQueue) ingestRVLess(
	ctx context.Context,
	keys byTypeAuditKeys,
	millis int64,
	values map[string]any,
) error {
	highWater := q.streamTopRV(ctx, keys.stream)
	if highWater == "" {
		return q.divertLate(ctx, keys, lateReasonRVMissingBeforeHighWater, "", "", values)
	}

	values[entryFieldRVPresent] = "false"
	values[entryFieldPlacement] = placementAttachedToLastRV

	id, err := q.xaddID(ctx, keys.stream, highWater+"-*", values)
	if err != nil {
		return fmt.Errorf("failed to append entry to %q: %w", keys.stream, err)
	}
	q.recordRVMissing(ctx, keys.idState, id, millis)
	return nil
}

// divertLate records an event in the diagnostic late lane with a server-assigned ID and full
// context — the reason, the event RV, and the current high-water. The late lane is
// observability only: never reordered into main, never a reconcile input (§6).
func (q *RedisByTypeStreamQueue) divertLate(
	ctx context.Context,
	keys byTypeAuditKeys,
	reason, rv, lastRV string,
	values map[string]any,
) error {
	values[entryFieldReason] = reason
	values[entryFieldPlacement] = placementLateLane
	values[entryFieldRVPresent] = strconv.FormatBool(rv != "")
	values[entryFieldLastRV] = lastRV

	if _, err := q.xaddID(ctx, keys.late, "*", values); err != nil {
		return fmt.Errorf("failed to append entry to %q: %w", keys.late, err)
	}
	q.incrIDState(ctx, keys.idState, idStateLateCount)
	return nil
}

// streamTopRV returns the resourceVersion at the stream's high-water mark — the leading
// component of its last-generated ID (XINFO STREAM), which is authoritative and survives
// trimming (§10). It is the value reported in late-lane diagnostics and the mark RV-less events
// attach to. Empty when the stream has no entries yet (or is unreachable).
func (q *RedisByTypeStreamQueue) streamTopRV(ctx context.Context, streamKey string) string {
	info, err := q.client.XInfoStream(ctx, streamKey).Result()
	if err != nil {
		return ""
	}
	if info.LastGeneratedID == "" || info.LastGeneratedID == "0-0" {
		return ""
	}
	rv, _, _ := strings.Cut(info.LastGeneratedID, "-")
	return rv
}

// xaddID appends one entry under an explicit (possibly partial, e.g. "<rv>-*") ID, applying the
// approximate MaxLen bound when configured. It returns the server-assigned ID.
func (q *RedisByTypeStreamQueue) xaddID(
	ctx context.Context,
	stream, id string,
	values map[string]any,
) (string, error) {
	args := &redis.XAddArgs{Stream: stream, ID: id, Values: values}
	if q.maxLen > 0 {
		args.MaxLen = q.maxLen
		args.Approx = true
	}
	return q.client.XAdd(ctx, args).Result()
}

// recordMain advances the idstate observability hash after a main-stream write: it moves the
// high-water (lastRV/lastStreamID/lastEventAt) and bumps mainCount. Best-effort and
// self-correcting — the stream's true top is authoritative, idstate is only a cache (§10) — so
// its errors are swallowed rather than surfaced as a mirror failure.
func (q *RedisByTypeStreamQueue) recordMain(ctx context.Context, idStateKey, rv, id string, millis int64) {
	_ = q.client.HSet(ctx, idStateKey,
		idStateLastRV, rv,
		idStateLastStreamID, id,
		idStateLastEventAt, millis,
	).Err()
	q.incrIDState(ctx, idStateKey, idStateMainCount)
}

// recordRVMissing updates the idstate hash after an RV-less attach: lastStreamID/lastEventAt
// advance to the attached entry but lastRV does not (the high-water RV is unchanged), and
// rvMissingCount is bumped. Best-effort (see recordMain).
func (q *RedisByTypeStreamQueue) recordRVMissing(ctx context.Context, idStateKey, id string, millis int64) {
	_ = q.client.HSet(ctx, idStateKey,
		idStateLastStreamID, id,
		idStateLastEventAt, millis,
	).Err()
	q.incrIDState(ctx, idStateKey, idStateRVMissingCount)
}

// incrIDState bumps a best-effort idstate counter by one, swallowing the error (IR7/IR8).
func (q *RedisByTypeStreamQueue) incrIDState(ctx context.Context, idStateKey, field string) {
	_ = q.client.HIncrBy(ctx, idStateKey, field, 1).Err()
}

// isIDTooSmall reports whether an XADD error is the strong-key rejection of an explicit ID that
// is not strictly greater than the stream top — for the RV-first stream, a strictly-older event.
func isIDTooSmall(err error) bool {
	return err != nil && strings.Contains(err.Error(), streamIDTooSmallMarker)
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
//
// A /scale event keys onto its PARENT type (DEC-A, canonical-stream-retirement.md): a
// scale is a mutation of the parent object, so it lands in the parent's stream at the
// parent's post-scale resourceVersion (the Scale body carries it), ordered among the
// parent's other writes; the entry's subresource=scale field stays the discriminator
// for the consumers. The sibling "<resource>.scale" stream no longer exists. Scale is
// the only subresource the webhook forwards (shouldForwardSubresource), so the
// subresource fold below is defensive only.
func (q *RedisByTypeStreamQueue) baseKey(event auditv1.Event) string {
	if event.ObjectRef == nil {
		return typeBaseKey(q.prefix, "", "", "")
	}
	ref := event.ObjectRef
	if auditutil.IsScaleSubresource(ref.Subresource) {
		return typeBaseKey(q.prefix, ref.APIGroup, ref.Resource, "")
	}
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
