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
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-logr/logr"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

const (
	queueStreamLengthMetric      = "gitopsreverser_audit_queue_stream_length"
	queuePendingEntriesMetric    = "gitopsreverser_audit_queue_pending_entries"
	queueOldestEntryAgeMetric    = "gitopsreverser_audit_queue_oldest_entry_age_seconds"
	queueDebugStreamLengthMetric = "gitopsreverser_audit_debug_stream_length"
	queueMetricsTestStream       = "audit.metrics.test"
	queueMetricsTestDebugStream  = "audit.debug.metrics.test"
	queueMetricsTestGroup        = "test-group"
	queueMetricsTestConsumer     = "test-consumer"
)

func newTestReporter(t *testing.T, mr *miniredis.Miniredis, debug string) *MetricsReporter {
	t.Helper()
	r, err := NewMetricsReporter(MetricsConfig{
		Addr:        mr.Addr(),
		Stream:      queueMetricsTestStream,
		Group:       queueMetricsTestGroup,
		DebugStream: debug,
		Interval:    time.Second,
	}, logr.Discard())
	require.NoError(t, err)
	return r
}

func TestNewMetricsReporter_RequiresAddress(t *testing.T) {
	_, err := NewMetricsReporter(MetricsConfig{Stream: "x"}, logr.Discard())
	require.Error(t, err)
}

func TestNewMetricsReporter_RequiresStream(t *testing.T) {
	_, err := NewMetricsReporter(MetricsConfig{Addr: "x:1"}, logr.Discard())
	require.Error(t, err)
}

func TestQueueMetrics_StreamLengthReportsZeroForMissingStream(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	mr := miniredis.RunT(t)
	r := newTestReporter(t, mr, "")

	r.collect(context.Background())

	length, ok := telemetry.CollectInt64Sum(reader, queueStreamLengthMetric, map[string]string{
		"stream": queueMetricsTestStream,
	})
	require.True(t, ok, "expected stream length sample even for a missing stream")
	assert.Equal(t, int64(0), length)
}

func TestQueueMetrics_StreamLengthReflectsXLEN(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	mr := miniredis.RunT(t)

	queue, err := NewRedisAuditQueue(RedisAuditQueueConfig{
		Addr:   mr.Addr(),
		Stream: queueMetricsTestStream,
		MaxLen: 0,
	})
	require.NoError(t, err)

	for i := range 3 {
		require.NoError(t, queue.Enqueue(context.Background(), auditv1.Event{
			Verb:  "create",
			Stage: "ResponseComplete",
			StageTimestamp: metav1.MicroTime{
				Time: time.Date(2026, 3, 5, 10, 0, 0, i, time.UTC),
			},
			ObjectRef: &auditv1.ObjectReference{
				APIVersion: "v1", Resource: "configmaps", Namespace: "default", Name: "cm",
			},
		}))
	}

	r := newTestReporter(t, mr, "")
	r.collect(context.Background())

	length, ok := telemetry.CollectInt64Sum(reader, queueStreamLengthMetric, map[string]string{
		"stream": queueMetricsTestStream,
	})
	require.True(t, ok)
	assert.Equal(t, int64(3), length)
}

func TestQueueMetrics_ConsumerLagAndPending(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	// Create the consumer group at the start of the stream so all entries count as lag.
	require.NoError(t, client.XGroupCreateMkStream(
		context.Background(), queueMetricsTestStream, queueMetricsTestGroup, "$",
	).Err())

	// Enqueue 3 events.
	queue, err := NewRedisAuditQueue(RedisAuditQueueConfig{
		Addr: mr.Addr(), Stream: queueMetricsTestStream, MaxLen: 0,
	})
	require.NoError(t, err)
	for i := range 3 {
		require.NoError(t, queue.Enqueue(context.Background(), auditv1.Event{
			Verb:  "create",
			Stage: "ResponseComplete",
			StageTimestamp: metav1.MicroTime{
				Time: time.Date(2026, 3, 5, 10, 0, 0, i, time.UTC),
			},
			ObjectRef: &auditv1.ObjectReference{
				APIVersion: "v1", Resource: "configmaps", Namespace: "default", Name: "cm",
			},
		}))
	}

	// Read but don't ACK — entries become pending.
	_, err = client.XReadGroup(context.Background(), &redis.XReadGroupArgs{
		Group:    queueMetricsTestGroup,
		Consumer: queueMetricsTestConsumer,
		Streams:  []string{queueMetricsTestStream, ">"},
		Count:    10,
		Block:    100 * time.Millisecond,
	}).Result()
	require.NoError(t, err)

	r := newTestReporter(t, mr, "")
	r.collect(context.Background())

	pending, ok := telemetry.CollectInt64Sum(reader, queuePendingEntriesMetric, map[string]string{
		"stream": queueMetricsTestStream, "group": queueMetricsTestGroup,
	})
	require.True(t, ok)
	assert.Equal(t, int64(3), pending, "all 3 read-but-unacked entries should be pending")
}

func TestQueueMetrics_OldestEntryAgeIsRecorded(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	mr := miniredis.RunT(t)

	queue, err := NewRedisAuditQueue(RedisAuditQueueConfig{
		Addr: mr.Addr(), Stream: queueMetricsTestStream, MaxLen: 0,
	})
	require.NoError(t, err)
	require.NoError(t, queue.Enqueue(context.Background(), auditv1.Event{
		Verb:  "create",
		Stage: "ResponseComplete",
		StageTimestamp: metav1.MicroTime{
			Time: time.Now(),
		},
		ObjectRef: &auditv1.ObjectReference{
			APIVersion: "v1", Resource: "configmaps", Namespace: "default", Name: "cm",
		},
	}))

	r := newTestReporter(t, mr, "")
	r.collect(context.Background())

	age, ok := telemetry.CollectInt64Sum(reader, queueOldestEntryAgeMetric, map[string]string{
		"stream": queueMetricsTestStream,
	})
	require.True(t, ok)
	assert.GreaterOrEqual(t, age, int64(0))
}

func TestQueueMetrics_DebugStreamLengthRecordedWhenEnabled(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	mr := miniredis.RunT(t)

	debugQueue, err := NewRedisAuditDebugQueue(RedisAuditQueueConfig{
		Addr: mr.Addr(), Stream: queueMetricsTestDebugStream, MaxLen: 0,
	})
	require.NoError(t, err)
	require.NoError(t, debugQueue.Enqueue(context.Background(), "official", auditv1.Event{
		Verb: "create", AuditID: "debug-1",
	}))

	r := newTestReporter(t, mr, queueMetricsTestDebugStream)
	r.collect(context.Background())

	length, ok := telemetry.CollectInt64Sum(reader, queueDebugStreamLengthMetric, map[string]string{
		"stream": queueMetricsTestDebugStream,
	})
	require.True(t, ok)
	assert.Equal(t, int64(1), length)
}

func TestQueueMetrics_DebugStreamSkippedWhenDisabled(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	mr := miniredis.RunT(t)
	r := newTestReporter(t, mr, "")
	r.collect(context.Background())

	_, ok := telemetry.CollectInt64Sum(reader, queueDebugStreamLengthMetric, map[string]string{
		"stream": queueMetricsTestDebugStream,
	})
	assert.False(t, ok, "debug stream length must not be recorded when DebugStream is empty")
}

func TestStreamEntryAgeSeconds(t *testing.T) {
	now := time.UnixMilli(10_000)
	cases := []struct {
		name string
		id   string
		want int64
		ok   bool
	}{
		{"valid id with seq", "5000-0", 5, true},
		{"valid id without dash", "5000", 5, true},
		{"future id clamped to zero", "20000-0", 0, true},
		{"unparseable id rejected", "not-a-number", 0, false},
		{"zero id rejected", "0-0", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := streamEntryAgeSeconds(tc.id, now)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}
