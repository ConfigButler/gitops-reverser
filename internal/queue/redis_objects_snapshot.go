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
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// objectsItemsSuffix holds the current set of objects for a type as a Redis HASH:
	// field "<namespace>/<name>" (cluster-scoped: "<name>") -> the object JSON. A HASH
	// (not a stream) because this is current state keyed by identity, not history.
	objectsItemsSuffix = ":objects:items"
	// objectsRVSuffix holds the list resourceVersion the items snapshot is pinned to.
	objectsRVSuffix = ":objects:rv"
	// objectsStateSuffix holds a small JSON status doc (phase/count/rv/updated_at).
	objectsStateSuffix = ":objects:state"

	objectsPhaseSynced  = "synced"
	objectsPhaseRemoved = "removed"
)

// RedisObjectsSnapshotConfig configures the per-resource-type current-objects snapshot.
// It reuses the audit Redis connection; there is no MaxLen because the items HASH is
// bounded by the number of live objects of the type, not by event history.
type RedisObjectsSnapshotConfig struct {
	Addr       string
	Username   string
	AuthValue  string
	DB         int
	Prefix     string
	TLSEnabled bool
}

// RedisObjectsSnapshot mirrors the current set of objects for a resource type into the
// per-type experiment keyspace, beside the audit stream that shares the same base key
// "<prefix>:<group-or-core>:<resource>". For each type it maintains an items HASH
// (identity -> object JSON), an rv string, and a small state doc, and records the type's
// base key in the shared ":__index__" set. It is write-only: nothing reads it yet. See
// docs/design/stream/per-resource-type-rv-keyed-streams-experiment.md.
type RedisObjectsSnapshot struct {
	client *redis.Client
	prefix string
}

// NewRedisObjectsSnapshot creates a per-resource-type current-objects snapshot writer.
func NewRedisObjectsSnapshot(cfg RedisObjectsSnapshotConfig) (*RedisObjectsSnapshot, error) {
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

	return &RedisObjectsSnapshot{
		client: redis.NewClient(options),
		prefix: prefix,
	}, nil
}

// ReplaceTypeObjects replaces the stored current-objects set for one resource type with
// items (identity -> object JSON) gathered from a single consistent LIST at
// resourceVersion. The whole replace runs in one transaction: register the base key,
// drop the old items, write the new ones, then pin rv and state. Replace (not merge)
// because the caller hands a complete snapshot — a deleted object must not linger.
//
// The (group, version, resource) is recorded in the state doc so the durable checkpoint is
// self-describing — the base key drops the version, but the boot rebuild (LoadSyncedCheckpoints
// → Materializer.RestoreSynced, DEC-L6) needs the full GVR.
func (q *RedisObjectsSnapshot) ReplaceTypeObjects(
	ctx context.Context,
	group, version, resource string,
	items map[string]string,
	resourceVersion string,
) error {
	base := typeBaseKey(q.prefix, group, resource, "")
	itemsKey := base + objectsItemsSuffix

	state, err := json.Marshal(objectsState{
		Phase:           objectsPhaseSynced,
		Group:           group,
		APIVersion:      version,
		Resource:        resource,
		Count:           len(items),
		ResourceVersion: resourceVersion,
		UpdatedAt:       time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("failed to marshal objects state for %q: %w", base, err)
	}

	pipe := q.client.TxPipeline()
	pipe.SAdd(ctx, q.prefix+byTypeIndexSuffix, base)
	pipe.Del(ctx, itemsKey)
	if len(items) > 0 {
		pipe.HSet(ctx, itemsKey, flattenHash(items)...)
	}
	pipe.Set(ctx, base+objectsRVSuffix, resourceVersion, 0)
	pipe.Set(ctx, base+objectsStateSuffix, string(state), 0)

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("failed to replace objects snapshot for %q: %w", base, err)
	}
	return nil
}

// DeleteTypeObjects drops the current-objects items and rv for a type and leaves a
// "removed" state tombstone (so a consumer can tell "swept" from "never seen"). The base
// key stays in the index; the audit stream for the same base may still exist.
func (q *RedisObjectsSnapshot) DeleteTypeObjects(ctx context.Context, group, resource string) error {
	base := typeBaseKey(q.prefix, group, resource, "")

	state, err := json.Marshal(objectsState{
		Phase:     objectsPhaseRemoved,
		Group:     group,
		Resource:  resource,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("failed to marshal objects state for %q: %w", base, err)
	}

	pipe := q.client.TxPipeline()
	pipe.Del(ctx, base+objectsItemsSuffix)
	pipe.Del(ctx, base+objectsRVSuffix)
	pipe.Set(ctx, base+objectsStateSuffix, string(state), 0)

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("failed to delete objects snapshot for %q: %w", base, err)
	}
	return nil
}

// objectsState is the small JSON status doc stored at ":objects:state". It carries the full
// (group, version, resource) so the durable checkpoint is self-describing for the boot rebuild
// — the base key drops the version (DEC-L6).
type objectsState struct {
	Phase           string `json:"phase"`
	Group           string `json:"group,omitempty"`
	APIVersion      string `json:"api_version,omitempty"`
	Resource        string `json:"resource,omitempty"`
	Count           int    `json:"count,omitempty"`
	ResourceVersion string `json:"resource_version,omitempty"`
	UpdatedAt       string `json:"updated_at"`
}

// SyncedCheckpoint is one type's durable checkpoint identity + pinned revision, read back from
// :objects:state on boot so the control plane can resume a Synced type without re-listing it.
type SyncedCheckpoint struct {
	Group           string
	Version         string
	Resource        string
	ResourceVersion string
}

// LoadSyncedCheckpoints reads every type's :objects:state and returns those currently in the
// "synced" phase with their full GVR and pinned rv — the read side of the durable checkpoint
// (DEC-L6). The boot rebuild replays these into the Materializer (RestoreSynced) so a restart
// resumes serving standing checkpoints. A missing, malformed, non-synced, or rv-less entry is
// skipped; a Redis error is returned for the caller to log and start cold (types re-fill on
// demand).
func (q *RedisObjectsSnapshot) LoadSyncedCheckpoints(ctx context.Context) ([]SyncedCheckpoint, error) {
	bases, err := q.client.SMembers(ctx, q.prefix+byTypeIndexSuffix).Result()
	if err != nil {
		return nil, fmt.Errorf("list type index: %w", err)
	}
	out := make([]SyncedCheckpoint, 0, len(bases))
	for _, base := range bases {
		raw, err := q.client.Get(ctx, base+objectsStateSuffix).Result()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read state %q: %w", base, err)
		}
		var st objectsState
		if json.Unmarshal([]byte(raw), &st) != nil {
			continue
		}
		if st.Phase != objectsPhaseSynced || st.ResourceVersion == "" || st.Resource == "" {
			continue
		}
		out = append(out, SyncedCheckpoint{
			Group:           st.Group,
			Version:         st.APIVersion,
			Resource:        st.Resource,
			ResourceVersion: st.ResourceVersion,
		})
	}
	return out, nil
}

// flattenHash turns an identity->json map into the field,value,... slice HSET expects.
func flattenHash(items map[string]string) []any {
	const argsPerField = 2 // field + value
	args := make([]any, 0, len(items)*argsPerField)
	for field, value := range items {
		args = append(args, field, value)
	}
	return args
}
