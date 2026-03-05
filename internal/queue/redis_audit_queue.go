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
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
)

const (
	// DefaultRedisAuditStream is the default stream used for audit ingestion events.
	DefaultRedisAuditStream = "gitopsreverser.audit.events.v1"
)

// RedisAuditQueueConfig configures a Redis-backed audit queue.
type RedisAuditQueueConfig struct {
	Addr       string
	Username   string
	AuthValue  string
	DB         int
	Stream     string
	MaxLen     int64
	TLSEnabled bool
}

// RedisAuditQueue enqueues audit events into a Redis stream.
type RedisAuditQueue struct {
	client *redis.Client
	stream string
	maxLen int64
}

// NewRedisAuditQueue creates a Redis stream-backed audit queue.
func NewRedisAuditQueue(cfg RedisAuditQueueConfig) (*RedisAuditQueue, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		return nil, errors.New("redis address is required")
	}

	stream := strings.TrimSpace(cfg.Stream)
	if stream == "" {
		stream = DefaultRedisAuditStream
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

	return &RedisAuditQueue{
		client: redis.NewClient(options),
		stream: stream,
		maxLen: cfg.MaxLen,
	}, nil
}

// Enqueue writes one audit event to Redis stream storage.
func (q *RedisAuditQueue) Enqueue(ctx context.Context, clusterID string, event auditv1.Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal audit event payload: %w", err)
	}

	objectRef := event.ObjectRef
	apiVersion := ""
	resource := ""
	namespace := ""
	name := ""
	if objectRef != nil {
		apiVersion = objectRef.APIVersion
		resource = objectRef.Resource
		namespace = objectRef.Namespace
		name = objectRef.Name
	}

	values := map[string]any{
		"event_id":        buildEventID(clusterID, event),
		"audit_id":        string(event.AuditID),
		"cluster_id":      clusterID,
		"verb":            event.Verb,
		"api_version":     apiVersion,
		"resource":        resource,
		"namespace":       namespace,
		"name":            name,
		"user":            event.User.Username,
		"stage_timestamp": formatStageTimestamp(event.StageTimestamp.Time),
		"payload_json":    string(payload),
	}

	args := &redis.XAddArgs{
		Stream: q.stream,
		ID:     "*",
		Values: values,
	}
	if q.maxLen > 0 {
		args.MaxLen = q.maxLen
		args.Approx = true
	}

	if _, err := q.client.XAdd(ctx, args).Result(); err != nil {
		return fmt.Errorf("failed to append audit event to redis stream %q: %w", q.stream, err)
	}

	return nil
}

func buildEventID(clusterID string, event auditv1.Event) string {
	var builder strings.Builder
	builder.WriteString(strings.TrimSpace(clusterID))
	builder.WriteString("|")
	builder.WriteString(string(event.AuditID))
	builder.WriteString("|")
	builder.WriteString(string(event.Stage))
	builder.WriteString("|")
	builder.WriteString(event.Verb)
	builder.WriteString("|")
	builder.WriteString(event.RequestURI)
	builder.WriteString("|")
	builder.WriteString(event.StageTimestamp.Time.UTC().Format(time.RFC3339Nano))
	if event.ObjectRef != nil {
		builder.WriteString("|")
		builder.WriteString(event.ObjectRef.APIVersion)
		builder.WriteString("|")
		builder.WriteString(event.ObjectRef.Resource)
		builder.WriteString("|")
		builder.WriteString(event.ObjectRef.Namespace)
		builder.WriteString("|")
		builder.WriteString(event.ObjectRef.Name)
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

func formatStageTimestamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
