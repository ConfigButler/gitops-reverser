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
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
)

func TestNewRedisAuditQueue_RequiresAddress(t *testing.T) {
	_, err := NewRedisAuditQueue(RedisAuditQueueConfig{})
	require.Error(t, err)
}

func TestRedisAuditQueue_Enqueue(t *testing.T) {
	mr := miniredis.RunT(t)

	queue, err := NewRedisAuditQueue(RedisAuditQueueConfig{
		Addr:   mr.Addr(),
		Stream: "audit.events.test",
		MaxLen: 100,
	})
	require.NoError(t, err)

	event := auditv1.Event{
		AuditID:    "audit-123",
		Verb:       "create",
		RequestURI: "/api/v1/namespaces/default/configmaps/cm-a",
		Stage:      "ResponseComplete",
		StageTimestamp: metav1.MicroTime{
			Time: time.Date(2026, 3, 5, 10, 0, 0, 0, time.UTC),
		},
		ObjectRef: &auditv1.ObjectReference{
			APIVersion: "v1",
			Resource:   "configmaps",
			Namespace:  "default",
			Name:       "cm-a",
		},
	}
	event.User.Username = "test-user"

	err = queue.Enqueue(context.Background(), event)
	require.NoError(t, err)

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	entries, err := client.XRange(context.Background(), "audit.events.test", "-", "+").Result()
	require.NoError(t, err)
	require.Len(t, entries, 1)

	entry := entries[0].Values
	assert.Equal(t, "audit-123", entry["audit_id"])
	assert.NotContains(t, entry, "cluster_id")
	assert.Equal(t, "create", entry["verb"])
	assert.Empty(t, entry["api_group"])
	assert.Equal(t, "v1", entry["api_version"])
	assert.Equal(t, "configmaps", entry["resource"])
	assert.Equal(t, "default", entry["namespace"])
	assert.Equal(t, "cm-a", entry["name"])
	assert.Equal(t, "test-user", entry["user"])
	assert.NotEmpty(t, entry["event_id"])
	assert.NotEmpty(t, entry["payload_json"])
}

func TestRedisAuditQueue_EnqueueCustomResourceStoresAPIGroup(t *testing.T) {
	mr := miniredis.RunT(t)

	queue, err := NewRedisAuditQueue(RedisAuditQueueConfig{
		Addr:   mr.Addr(),
		Stream: "audit.events.test",
		MaxLen: 100,
	})
	require.NoError(t, err)

	event := auditv1.Event{
		AuditID:    "audit-cr-123",
		Verb:       "create",
		RequestURI: "/apis/shop.example.com/v1/namespaces/default/icecreamorders/order-1",
		Stage:      "ResponseComplete",
		StageTimestamp: metav1.MicroTime{
			Time: time.Date(2026, 3, 5, 10, 0, 0, 0, time.UTC),
		},
		ObjectRef: &auditv1.ObjectReference{
			APIGroup:   "shop.example.com",
			APIVersion: "v1",
			Resource:   "icecreamorders",
			Namespace:  "default",
			Name:       "order-1",
		},
	}
	event.User.Username = "test-user"

	err = queue.Enqueue(context.Background(), event)
	require.NoError(t, err)

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	entries, err := client.XRange(context.Background(), "audit.events.test", "-", "+").Result()
	require.NoError(t, err)
	require.Len(t, entries, 1)

	entry := entries[0].Values
	assert.Equal(t, "shop.example.com", entry["api_group"])
	assert.Equal(t, "v1", entry["api_version"])
	assert.Equal(t, "icecreamorders", entry["resource"])
	assert.Equal(t, "default", entry["namespace"])
	assert.Equal(t, "order-1", entry["name"])
}

func TestRedisAuditQueue_EnqueueBoundedTrimsStream(t *testing.T) {
	mr := miniredis.RunT(t)

	const maxLen = 5
	queue, err := NewRedisAuditQueue(RedisAuditQueueConfig{
		Addr:   mr.Addr(),
		Stream: "audit.events.test",
		MaxLen: maxLen,
	})
	require.NoError(t, err)

	const enqueued = 50
	for i := range enqueued {
		event := auditv1.Event{
			AuditID: auditv1.Event{}.AuditID,
			Verb:    "create",
			Stage:   "ResponseComplete",
			StageTimestamp: metav1.MicroTime{
				Time: time.Date(2026, 3, 5, 10, 0, 0, i, time.UTC),
			},
			ObjectRef: &auditv1.ObjectReference{
				APIVersion: "v1",
				Resource:   "configmaps",
				Namespace:  "default",
				Name:       "cm",
			},
		}
		require.NoError(t, queue.Enqueue(context.Background(), event))
	}

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	length, err := client.XLen(context.Background(), "audit.events.test").Result()
	require.NoError(t, err)
	// MAXLEN ~ is approximate; it must trim but is allowed to leave a small overshoot.
	// What it must not do is leave the stream at its un-trimmed size.
	assert.LessOrEqual(t, length, int64(enqueued/2),
		"approximate MAXLEN should trim well below the un-trimmed length of %d", enqueued)
	assert.GreaterOrEqual(t, length, int64(maxLen),
		"approximate MAXLEN should retain at least maxLen entries when more were enqueued")
}

func TestRedisAuditQueue_EnqueueUnboundedKeepsAllEntries(t *testing.T) {
	mr := miniredis.RunT(t)

	queue, err := NewRedisAuditQueue(RedisAuditQueueConfig{
		Addr:   mr.Addr(),
		Stream: "audit.events.test",
		MaxLen: 0, // explicit unbounded
	})
	require.NoError(t, err)

	const enqueued = 25
	for i := range enqueued {
		event := auditv1.Event{
			Verb:  "create",
			Stage: "ResponseComplete",
			StageTimestamp: metav1.MicroTime{
				Time: time.Date(2026, 3, 5, 10, 0, 0, i, time.UTC),
			},
			ObjectRef: &auditv1.ObjectReference{
				APIVersion: "v1",
				Resource:   "configmaps",
				Namespace:  "default",
				Name:       "cm",
			},
		}
		require.NoError(t, queue.Enqueue(context.Background(), event))
	}

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	length, err := client.XLen(context.Background(), "audit.events.test").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(enqueued), length, "MaxLen=0 must not trim the stream")
}

func TestRedisAuditDebugQueue_EnqueueBoundedTrimsStream(t *testing.T) {
	mr := miniredis.RunT(t)

	const maxLen = 5
	debugQueue, err := NewRedisAuditDebugQueue(RedisAuditQueueConfig{
		Addr:   mr.Addr(),
		Stream: "audit.debug.test",
		MaxLen: maxLen,
	})
	require.NoError(t, err)

	const enqueued = 50
	for range enqueued {
		event := auditv1.Event{Verb: "create", AuditID: "audit-debug"}
		require.NoError(t, debugQueue.Enqueue(context.Background(), "official", event))
	}

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	length, err := client.XLen(context.Background(), "audit.debug.test").Result()
	require.NoError(t, err)
	assert.LessOrEqual(t, length, int64(enqueued/2),
		"approximate MAXLEN should trim the debug stream well below %d", enqueued)
	assert.GreaterOrEqual(t, length, int64(maxLen),
		"approximate MAXLEN should retain at least maxLen entries when more were enqueued")
}

func TestRedisAuditDebugQueue_EnqueueUnboundedKeepsAllEntries(t *testing.T) {
	mr := miniredis.RunT(t)

	debugQueue, err := NewRedisAuditDebugQueue(RedisAuditQueueConfig{
		Addr:   mr.Addr(),
		Stream: "audit.debug.test",
		MaxLen: 0,
	})
	require.NoError(t, err)

	const enqueued = 25
	for range enqueued {
		event := auditv1.Event{Verb: "create", AuditID: "audit-debug"}
		require.NoError(t, debugQueue.Enqueue(context.Background(), "official", event))
	}

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	length, err := client.XLen(context.Background(), "audit.debug.test").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(enqueued), length, "MaxLen=0 must not trim the debug stream")
}

func TestRedisAuditDebugQueue_EnqueueStoresSource(t *testing.T) {
	mr := miniredis.RunT(t)

	debugQueue, err := NewRedisAuditDebugQueue(RedisAuditQueueConfig{
		Addr:   mr.Addr(),
		Stream: "audit.debug.test",
		MaxLen: 100,
	})
	require.NoError(t, err)

	event := auditv1.Event{AuditID: "audit-debug-123", Verb: "create"}
	err = debugQueue.Enqueue(context.Background(), "official", event)
	require.NoError(t, err)

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	entries, err := client.XRange(context.Background(), "audit.debug.test", "-", "+").Result()
	require.NoError(t, err)
	require.Len(t, entries, 1)

	entry := entries[0].Values
	assert.Equal(t, "official", entry["source"])
	assert.Equal(t, "audit-debug-123", entry["audit_id"])
	assert.NotEmpty(t, entry["payload_json"])
}
