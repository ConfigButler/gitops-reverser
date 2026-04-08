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

	err = queue.Enqueue(context.Background(), "cluster-a", event)
	require.NoError(t, err)

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	entries, err := client.XRange(context.Background(), "audit.events.test", "-", "+").Result()
	require.NoError(t, err)
	require.Len(t, entries, 1)

	entry := entries[0].Values
	assert.Equal(t, "audit-123", entry["audit_id"])
	assert.Equal(t, "cluster-a", entry["cluster_id"])
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

	err = queue.Enqueue(context.Background(), "cluster-a", event)
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
