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
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
)

const testByTypePrefix = "test.bytype.v1"

// sharedValkeyAddr is the address of a single Valkey container shared by every test in this
// package that needs real Redis stream-ID semantics — specifically the strong-key rejection of
// a strictly-older "<rv>-*", which miniredis does not emulate (it silently clamps the ID rather
// than rejecting it). It is empty when Docker is unavailable, in which case those tests skip
// while the miniredis-backed tests still run.
// See docs/design/stream/audit-log-ingestion-and-ordering.md §9.
var (
	sharedValkeyAddr string
	valkeyPrefixSeq  atomic.Int64
)

func TestMain(m *testing.M) {
	addr, terminate, err := startSharedValkey()
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"queue tests: real-Valkey container unavailable; real-semantics tests will skip: %v\n", err)
	} else {
		sharedValkeyAddr = addr
	}
	code := m.Run()
	if terminate != nil {
		terminate()
	}
	os.Exit(code)
}

// startSharedValkey boots one Valkey container for the package. Returns ("", nil, err) when the
// container cannot be started (e.g. no Docker) so the caller can degrade to skipping.
func startSharedValkey() (string, func(), error) {
	ctx := context.Background()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "valkey/valkey:8-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		return "", nil, err
	}
	host, err := container.Host(ctx)
	if err != nil {
		_ = container.Terminate(ctx)
		return "", nil, err
	}
	port, err := container.MappedPort(ctx, "6379/tcp")
	if err != nil {
		_ = container.Terminate(ctx)
		return "", nil, err
	}
	return net.JoinHostPort(host, port.Port()),
		func() { _ = container.Terminate(context.Background()) },
		nil
}

// valkeyByTypeQueue returns a queue and an inspection client bound to the shared Valkey
// container, under a prefix unique to this test so the shared keyspace stays isolated. It skips
// the test when the container is unavailable.
func valkeyByTypeQueue(t *testing.T, maxLen int64) (*RedisByTypeStreamQueue, *redis.Client, string) {
	t.Helper()
	if sharedValkeyAddr == "" {
		t.Skip("real-Valkey container unavailable (Docker required); skipping real-semantics test")
	}
	prefix := fmt.Sprintf("test.bytype.%d", valkeyPrefixSeq.Add(1))
	q, err := NewRedisByTypeStreamQueue(RedisByTypeStreamConfig{Addr: sharedValkeyAddr, Prefix: prefix, MaxLen: maxLen})
	require.NoError(t, err)
	client := redis.NewClient(&redis.Options{Addr: sharedValkeyAddr})
	t.Cleanup(func() { _ = client.Close() })
	return q, client, prefix
}

func newTestByTypeQueue(t *testing.T, mr *miniredis.Miniredis, maxLen int64) *RedisByTypeStreamQueue {
	t.Helper()
	q, err := NewRedisByTypeStreamQueue(RedisByTypeStreamConfig{
		Addr:   mr.Addr(),
		Prefix: testByTypePrefix,
		MaxLen: maxLen,
	})
	require.NoError(t, err)
	return q
}

// ingestionEvent builds an apps/deployments event with the given resourceVersion (empty for an
// RV-less event; a non-numeric string for the non-numeric branch) and stage millisecond. RV is
// carried on the response object body, the same place resourceVersionFromEvent reads it.
func ingestionEvent(rv string, millis int64) auditv1.Event {
	e := auditv1.Event{
		AuditID:        "evt",
		Verb:           "update",
		Stage:          auditv1.StageResponseComplete,
		StageTimestamp: metav1.MicroTime{Time: time.UnixMilli(millis)},
		ObjectRef: &auditv1.ObjectReference{
			APIGroup:  "apps",
			Resource:  "deployments",
			Namespace: "prod",
			Name:      "web",
		},
	}
	if rv != "" {
		e.ResponseObject = &runtime.Unknown{Raw: []byte(`{"metadata":{"resourceVersion":"` + rv + `"}}`)}
	}
	return e
}

// streamIDsOf returns the IDs of every entry in a stream, in order, or nil when it is empty.
func streamIDsOf(t *testing.T, client *redis.Client, key string) []string {
	t.Helper()
	entries, err := client.XRange(context.Background(), key, "-", "+").Result()
	require.NoError(t, err)
	if len(entries) == 0 {
		return nil
	}
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	return ids
}

// counterValue reads an idstate hash counter, treating a missing field as zero.
func counterValue(t *testing.T, client *redis.Client, key, field string) int64 {
	t.Helper()
	got, err := client.HGet(context.Background(), key, field).Result()
	if errors.Is(err, redis.Nil) {
		return 0
	}
	require.NoError(t, err)
	n, err := strconv.ParseInt(got, 10, 64)
	require.NoError(t, err)
	return n
}

func TestNewRedisByTypeStreamQueue_RequiresAddress(t *testing.T) {
	_, err := NewRedisByTypeStreamQueue(RedisByTypeStreamConfig{})
	require.Error(t, err)
}

func TestNewRedisByTypeStreamQueue_DefaultsPrefix(t *testing.T) {
	q, err := NewRedisByTypeStreamQueue(RedisByTypeStreamConfig{Addr: "127.0.0.1:6379"})
	require.NoError(t, err)
	assert.Equal(t, DefaultRedisByTypeStreamPrefix, q.prefix)
}

func TestRedisByTypeStreamQueue_EnqueueWritesEntryAndIndex(t *testing.T) {
	q, client, prefix := valkeyByTypeQueue(t, 100)
	ctx := context.Background()

	stage := time.Date(2026, 6, 9, 10, 0, 0, 123_000_000, time.UTC)
	event := auditv1.Event{
		AuditID:        "audit-123",
		Verb:           "update",
		Stage:          auditv1.StageResponseComplete,
		StageTimestamp: metav1.MicroTime{Time: stage},
		ObjectRef: &auditv1.ObjectReference{
			APIGroup:   "apps",
			APIVersion: "v1",
			Resource:   "deployments",
			Namespace:  "prod",
			Name:       "web",
		},
		ResponseObject: &runtime.Unknown{Raw: []byte(`{"metadata":{"resourceVersion":"184467"}}`)},
	}
	event.User.Username = "alice"

	require.NoError(t, q.Enqueue(ctx, event))

	baseKey := prefix + ":apps:deployments"
	streamKey := baseKey + byTypeAuditStreamSuffix

	entries, err := client.XRange(ctx, streamKey, "-", "+").Result()
	require.NoError(t, err)
	require.Len(t, entries, 1)

	entry := entries[0]
	assert.Equal(t, "184467-0", entry.ID,
		"the stream ID leads with the resourceVersion, with a Valkey-assigned subseq")

	v := entry.Values
	assert.Equal(t, "audit-123", v["audit_id"])
	assert.Equal(t, "ResponseComplete", v["stage"])
	assert.Equal(t, "update", v["verb"])
	assert.Equal(t, "apps", v["api_group"])
	assert.Equal(t, "v1", v["api_version"])
	assert.Equal(t, "deployments", v["resource"])
	assert.Empty(t, v["subresource"])
	assert.Equal(t, "prod", v["namespace"])
	assert.Equal(t, "web", v["name"])
	assert.Equal(t, "184467", v["resource_version"])
	assert.Equal(t, strconv.FormatInt(stage.UnixMilli(), 10), v["stage_millis"],
		"the stage millisecond is kept as a field even though it no longer leads the ID")
	assert.Equal(t, "true", v[entryFieldRVPresent])
	assert.Equal(t, placementResourceVersion, v[entryFieldPlacement])
	assert.Equal(t, "alice", v["user"])
	assert.Contains(t, v["payload_json"], "deployments",
		"the full event JSON rides along on the same entry")

	members, err := client.SMembers(ctx, prefix+byTypeIndexSuffix).Result()
	require.NoError(t, err)
	assert.Equal(t, []string{baseKey}, members,
		"the type's base key is registered in the index set")
}

func TestRedisByTypeStreamQueue_BaseKey(t *testing.T) {
	q := &RedisByTypeStreamQueue{prefix: testByTypePrefix}

	tests := []struct {
		name string
		ref  *auditv1.ObjectReference
		want string
	}{
		{
			name: "core group renders as core",
			ref:  &auditv1.ObjectReference{APIVersion: "v1", Resource: "configmaps"},
			want: testByTypePrefix + ":core:configmaps",
		},
		{
			name: "named group is preserved",
			ref:  &auditv1.ObjectReference{APIGroup: "apps", APIVersion: "v1", Resource: "deployments"},
			want: testByTypePrefix + ":apps:deployments",
		},
		{
			name: "subresource is folded onto the resource segment",
			ref:  &auditv1.ObjectReference{APIGroup: "apps", Resource: "deployments", Subresource: "scale"},
			want: testByTypePrefix + ":apps:deployments.scale",
		},
		{
			name: "nil objectRef collapses to the unknown bucket",
			ref:  nil,
			want: testByTypePrefix + ":" + byTypeUnknownBucket,
		},
		{
			name: "empty resource collapses to the unknown bucket",
			ref:  &auditv1.ObjectReference{APIGroup: "apps"},
			want: testByTypePrefix + ":" + byTypeUnknownBucket,
		},
		{
			name: "odd characters (including colons) are sanitized",
			ref:  &auditv1.ObjectReference{APIGroup: "Weird:Group", Resource: "Things!"},
			want: testByTypePrefix + ":weird_group:things_",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, q.baseKey(auditv1.Event{ObjectRef: tt.ref}))
		})
	}
}

func TestResourceVersionFromEvent(t *testing.T) {
	tests := []struct {
		name  string
		event auditv1.Event
		want  string
	}{
		{
			name: "prefers the response object body",
			event: auditv1.Event{
				ResponseObject: &runtime.Unknown{Raw: []byte(`{"metadata":{"resourceVersion":"999"}}`)},
				RequestObject:  &runtime.Unknown{Raw: []byte(`{"metadata":{"resourceVersion":"888"}}`)},
				ObjectRef:      &auditv1.ObjectReference{ResourceVersion: "777"},
			},
			want: "999",
		},
		{
			name: "falls back to the request object body",
			event: auditv1.Event{
				RequestObject: &runtime.Unknown{Raw: []byte(`{"metadata":{"resourceVersion":"888"}}`)},
				ObjectRef:     &auditv1.ObjectReference{ResourceVersion: "777"},
			},
			want: "888",
		},
		{
			name:  "falls back to the objectRef precondition RV",
			event: auditv1.Event{ObjectRef: &auditv1.ObjectReference{ResourceVersion: "777"}},
			want:  "777",
		},
		{
			name:  "empty when no RV is available",
			event: auditv1.Event{ObjectRef: &auditv1.ObjectReference{Resource: "configmaps"}},
			want:  "",
		},
		{
			name: "malformed body is ignored",
			event: auditv1.Event{
				ResponseObject: &runtime.Unknown{Raw: []byte(`not json`)},
				ObjectRef:      &auditv1.ObjectReference{ResourceVersion: "777"},
			},
			want: "777",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, resourceVersionFromEvent(tt.event))
		})
	}
}

func TestStageMillis_Fallbacks(t *testing.T) {
	stage := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	withStage := auditv1.Event{StageTimestamp: metav1.MicroTime{Time: stage}}
	assert.Equal(t, stage.UnixMilli(), stageMillis(withStage))

	received := time.Date(2026, 6, 9, 9, 0, 0, 0, time.UTC)
	withReceived := auditv1.Event{RequestReceivedTimestamp: metav1.MicroTime{Time: received}}
	assert.Equal(t, received.UnixMilli(), stageMillis(withReceived))

	before := time.Now().UnixMilli()
	got := stageMillis(auditv1.Event{})
	assert.GreaterOrEqual(t, got, before, "a timestamp-less event falls back to wall-clock")
}

func TestClassifyRV(t *testing.T) {
	tests := []struct {
		rv   string
		want rvClass
	}{
		{"", rvAbsent},
		{"0", rvNumeric},
		{"184467", rvNumeric},
		{"18446744073709551615", rvNumeric},    // 2^64-1, the max stream-ID component
		{"18446744073709551616", rvNonNumeric}, // 2^64, overflows uint64
		{"abc", rvNonNumeric},
		{"-1", rvNonNumeric},
		{"12.3", rvNonNumeric},
	}
	for _, tt := range tests {
		t.Run(tt.rv, func(t *testing.T) {
			assert.Equal(t, tt.want, classifyRV(tt.rv))
		})
	}
}

// lateWant is the expected shape of one late-lane entry.
type lateWant struct {
	reason    string
	rv        string // the resource_version field (the event RV)
	lastRV    string
	rvPresent string
}

// TestRedisByTypeStreamQueue_Ingestion drives a sequence of events through Enqueue against a real
// Valkey and asserts the resulting main-stream IDs, late-lane entries, and idstate counters — the
// §11 acceptance criteria and the §7 observability counters in one table. It runs against the
// container (not miniredis) because the strictly-older→late criterion depends on Valkey's strong
// key actually rejecting a below-high-water "<rv>-*".
func TestRedisByTypeStreamQueue_Ingestion(t *testing.T) {
	tests := []struct {
		name          string
		rvs           []string // one Enqueue per element ("" = RV-less)
		wantMainIDs   []string
		wantLate      []lateWant
		wantMainCount int64
		wantLateCount int64
		wantRVMissing int64
		wantLastRV    string
	}{
		{
			name:          "increasing RVs land in main with a fresh subseq",
			rvs:           []string{"100", "200", "300"},
			wantMainIDs:   []string{"100-0", "200-0", "300-0"},
			wantMainCount: 3,
			wantLastRV:    "300",
		},
		{
			name:          "events at the same RV disambiguate via Valkey's subseq",
			rvs:           []string{"200", "200", "200"},
			wantMainIDs:   []string{"200-0", "200-1", "200-2"},
			wantMainCount: 3,
			wantLastRV:    "200",
		},
		{
			name:          "an RV equal to the high-water stays in main, not late",
			rvs:           []string{"200", "200"},
			wantMainIDs:   []string{"200-0", "200-1"},
			wantMainCount: 2,
			wantLastRV:    "200",
		},
		{
			name:        "a strictly-older RV is never in main; it is fully recorded in late",
			rvs:         []string{"200", "100"},
			wantMainIDs: []string{"200-0"},
			wantLate: []lateWant{
				{reason: lateReasonOlderThanHighWater, rv: "100", lastRV: "200", rvPresent: "true"},
			},
			wantMainCount: 1,
			wantLateCount: 1,
			wantLastRV:    "200",
		},
		{
			name: "an RV-less event before any high-water goes to late",
			rvs:  []string{""},
			wantLate: []lateWant{
				{reason: lateReasonRVMissingBeforeHighWater, rv: "", lastRV: "", rvPresent: "false"},
			},
			wantLateCount: 1,
		},
		{
			name:          "an RV-less event after a high-water attaches to it",
			rvs:           []string{"150", ""},
			wantMainIDs:   []string{"150-0", "150-1"},
			wantMainCount: 1, // the numeric event
			wantRVMissing: 1, // the attached RV-less event (also in the main stream)
			wantLastRV:    "150",
		},
		{
			name:          "a present-but-non-numeric RV is diverted to late, never crashes",
			rvs:           []string{"abc"},
			wantLate:      []lateWant{{reason: lateReasonNonNumericRV, rv: "abc", lastRV: "", rvPresent: "true"}},
			wantLateCount: 1,
		},
		{
			name: "large RVs beyond 2^53 order correctly (no lossy tonumber)",
			rvs:  []string{"9007199254740992", "9007199254740993", "9007199254740991"},
			wantMainIDs: []string{
				"9007199254740992-0", // 2^53
				"9007199254740993-0", // 2^53 + 1
			},
			wantLate: []lateWant{{
				reason:    lateReasonOlderThanHighWater,
				rv:        "9007199254740991", // 2^53 - 1, strictly below the high-water
				lastRV:    "9007199254740993",
				rvPresent: "true",
			}},
			wantMainCount: 2,
			wantLateCount: 1,
			wantLastRV:    "9007199254740993",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, client, prefix := valkeyByTypeQueue(t, 0)
			ctx := context.Background()
			base := prefix + ":apps:deployments"

			for i, rv := range tt.rvs {
				require.NoError(t, q.Enqueue(ctx, ingestionEvent(rv, int64(1000+i))))
			}

			assert.Equal(t, tt.wantMainIDs, streamIDsOf(t, client, base+byTypeAuditStreamSuffix),
				"main-stream IDs")

			lateEntries, err := client.XRange(ctx, base+byTypeAuditLateSuffix, "-", "+").Result()
			require.NoError(t, err)
			require.Len(t, lateEntries, len(tt.wantLate), "late-lane entry count")
			for i, want := range tt.wantLate {
				v := lateEntries[i].Values
				assert.Equal(t, want.reason, v[entryFieldReason], "late reason")
				assert.Equal(t, want.rv, v["resource_version"], "late event RV")
				assert.Equal(t, want.lastRV, v[entryFieldLastRV], "late high-water")
				assert.Equal(t, want.rvPresent, v[entryFieldRVPresent], "late rv_present")
				assert.Equal(t, placementLateLane, v[entryFieldPlacement], "late placement")
			}

			idState := base + byTypeAuditIDStateSuffix
			assert.Equal(t, tt.wantMainCount, counterValue(t, client, idState, idStateMainCount), "mainCount")
			assert.Equal(t, tt.wantLateCount, counterValue(t, client, idState, idStateLateCount), "lateCount")
			assert.Equal(t, tt.wantRVMissing, counterValue(t, client, idState, idStateRVMissingCount), "rvMissingCount")
			if tt.wantLastRV != "" {
				lastRV, err := client.HGet(ctx, idState, idStateLastRV).Result()
				require.NoError(t, err)
				assert.Equal(t, tt.wantLastRV, lastRV, "idstate high-water lastRV")
			}
		})
	}
}

// TestRedisByTypeStreamQueue_IDStateObservability checks the full IR7 high-water field set is
// written on a main-stream ingest: lastRV, lastStreamID, lastEventAt, and mainCount.
func TestRedisByTypeStreamQueue_IDStateObservability(t *testing.T) {
	q, client, prefix := valkeyByTypeQueue(t, 0)
	ctx := context.Background()
	idState := prefix + ":apps:deployments" + byTypeAuditIDStateSuffix

	require.NoError(t, q.Enqueue(ctx, ingestionEvent("500", 4242)))

	state, err := client.HGetAll(ctx, idState).Result()
	require.NoError(t, err)
	assert.Equal(t, "500", state[idStateLastRV])
	assert.Equal(t, "500-0", state[idStateLastStreamID])
	assert.Equal(t, "4242", state[idStateLastEventAt])
	assert.Equal(t, "1", state[idStateMainCount])
}

// An RV-less event with no objectRef has no high-water to attach to (the unknown bucket's stream
// is empty), so it is recorded in the late lane with rv-missing-before-high-water.
func TestRedisByTypeStreamQueue_EnqueueUnknownBucket(t *testing.T) {
	q, client, prefix := valkeyByTypeQueue(t, 0)
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, auditv1.Event{
		AuditID:        "no-ref",
		Stage:          auditv1.StageResponseComplete,
		StageTimestamp: metav1.MicroTime{Time: time.UnixMilli(2000)},
	}))

	base := prefix + ":" + byTypeUnknownBucket

	mainEntries, err := client.XRange(ctx, base+byTypeAuditStreamSuffix, "-", "+").Result()
	require.NoError(t, err)
	assert.Empty(t, mainEntries, "an RV-less event with no high-water never enters the main stream")

	lateEntries, err := client.XRange(ctx, base+byTypeAuditLateSuffix, "-", "+").Result()
	require.NoError(t, err)
	require.Len(t, lateEntries, 1)
	assert.Equal(t, "no-ref", lateEntries[0].Values["audit_id"])
	assert.Equal(t, lateReasonRVMissingBeforeHighWater, lateEntries[0].Values[entryFieldReason])
	assert.Empty(t, lateEntries[0].Values["resource_version"])
}

// TestRedisByTypeStreamQueue_BoundedTrimsStreams verifies the MaxLen knob is plumbed onto every
// XADD. It uses miniredis, whose trim is exact, because we are testing that the bound is applied
// at all — not Valkey's approximate (macro-node) trim, which would not trim a stream this small.
func TestRedisByTypeStreamQueue_BoundedTrimsStreams(t *testing.T) {
	mr := miniredis.RunT(t)
	const maxLen = 5
	q := newTestByTypeQueue(t, mr, maxLen)

	for i := range 50 {
		event := auditv1.Event{
			Stage:          auditv1.StageResponseComplete,
			StageTimestamp: metav1.MicroTime{Time: time.Date(2026, 6, 9, 10, 0, 0, i*1_000_000, time.UTC)},
			ObjectRef:      &auditv1.ObjectReference{APIVersion: "v1", Resource: "configmaps"},
			// Increasing RVs so each lands in the main stream and the MaxLen bound trims it.
			ResponseObject: &runtime.Unknown{
				Raw: []byte(`{"metadata":{"resourceVersion":"` + strconv.Itoa(i+1) + `"}}`),
			},
		}
		require.NoError(t, q.Enqueue(context.Background(), event))
	}

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	streamLen, err := client.XLen(context.Background(), testByTypePrefix+":core:configmaps"+byTypeAuditStreamSuffix).
		Result()
	require.NoError(t, err)
	assert.LessOrEqual(t, streamLen, int64(maxLen))
}

func TestRedisByTypeStreamQueue_IndexErrorPropagates(t *testing.T) {
	mr := miniredis.RunT(t)
	q := newTestByTypeQueue(t, mr, 0)
	mr.Close()

	err := q.Enqueue(context.Background(), auditv1.Event{
		AuditID:   "x",
		Stage:     auditv1.StageResponseComplete,
		ObjectRef: &auditv1.ObjectReference{APIGroup: "apps", Resource: "deployments"},
	})
	require.Error(t, err)
}

func TestRedisByTypeStreamQueue_XAddErrorPropagates(t *testing.T) {
	mr := miniredis.RunT(t)
	q := newTestByTypeQueue(t, mr, 0)

	require.NoError(t, q.Enqueue(context.Background(), ingestionEvent("100", 3000)),
		"first enqueue indexes the key in-memory")

	mr.Close()
	require.Error(t, q.Enqueue(context.Background(), ingestionEvent("101", 3001)),
		"once the index is cached, a later main-stream XADD failure must surface")
}

func TestRedisByTypeStreamQueue_LateXAddErrorPropagates(t *testing.T) {
	mr := miniredis.RunT(t)
	q := newTestByTypeQueue(t, mr, 0)

	require.NoError(t, q.Enqueue(context.Background(), ingestionEvent("100", 3000)),
		"first enqueue indexes the key in-memory")

	mr.Close()
	// A present-but-non-numeric RV routes straight to the late lane; with Redis down that
	// late-lane XADD must surface as an error for the caller to log/count.
	require.Error(t, q.Enqueue(context.Background(), ingestionEvent("not-a-number", 3001)))
}
