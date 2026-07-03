// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// watchCursorTTL bounds a stored watch-resume cursor. A live GitTarget refreshes its
// cursor on every watch event and ~minutely bookmark, so the TTL only fires once a
// watch has been gone longer than this — a deleted GitTarget, or a long outage — after
// which the next session safely rebuilds from a fresh replay. The GitTarget UID is part
// of the key, so a recreated target never inherits a stale predecessor's cursor.
const watchCursorTTL = time.Hour

// watchCursorKeySuffix namespaces the resume-cursor keys this store owns. The cursor is
// not an author record, so it lives in its own watch domain rather than under author; the
// durable thing it holds is a watch shard's last processed RV.
const watchCursorKeySuffix = ":watch:v1:"

// RedisStoreConfig configures the Redis/Valkey connection that backs the required
// watch-resume cursor store.
type RedisStoreConfig struct {
	Addr       string
	Username   string
	AuthValue  string
	DB         int
	TLSEnabled bool
}

// RedisStore is the required Redis/Valkey-backed store. It owns the connection and
// persists each GitTarget watch shard's resume cursor (state continuity / work
// re-pickup), and the readiness gate pings it. It is a hard dependency in every mode
// and knows nothing about attribution: author attribution is an optional layer built
// on the same connection via AttributionIndex.
type RedisStore struct {
	client *redis.Client
}

// NewRedisStore opens the Redis/Valkey connection that backs the resume cursors.
func NewRedisStore(cfg RedisStoreConfig) (*RedisStore, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		return nil, errors.New("redis address is required")
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

	return &RedisStore{client: redis.NewClient(options)}, nil
}

// AttributionIndex builds the optional author-attribution fact index on this store's
// connection. Call it only when author attribution is enabled — the store itself,
// and the resume cursors it holds, never depend on it.
func (s *RedisStore) AttributionIndex(factTTL time.Duration) *AttributionIndex {
	if factTTL <= 0 {
		factTTL = DefaultAttributionFactTTL
	}
	return &AttributionIndex{client: s.client, factTTL: factTTL}
}

// CommandAuthorStore builds the command-authorship store on this connection. Wire it
// when the validate-operator-types webhook is enabled; it does not depend on attribution. The
// record lives in the same top-level author domain as audit facts but in the separate
// command subfamily, with its own fixed cleanup TTL.
func (s *RedisStore) CommandAuthorStore() *CommandAuthorStore {
	return &CommandAuthorStore{client: s.client, ttl: commandAuthorRecordTTL}
}

// Ping checks liveness of the underlying Redis/Valkey connection. The readiness gate
// uses it so the pod does not report ready before its resume-cursor store is reachable.
func (s *RedisStore) Ping(ctx context.Context) error {
	return s.client.Ping(ctx).Err()
}

// LookupWatchCursor returns the last resourceVersion durably processed for one
// GitTarget watch shard. A miss means the watch must rebuild from a fresh replay.
func (s *RedisStore) LookupWatchCursor(
	ctx context.Context,
	gitTargetUID string,
	gvr schema.GroupVersionResource,
	namespace string,
) (string, bool) {
	rv, err := s.client.Get(ctx, s.watchCursorKey(gitTargetUID, gvr, namespace)).Result()
	if err != nil || rv == "" {
		return "", false
	}
	return rv, true
}

// RecordWatchCursor stores the last resourceVersion durably processed for one
// GitTarget watch shard, refreshing watchCursorTTL on each write. The cursor is keyed
// by GitTarget UID and bounded by the TTL, so it never needs explicit deletion: a live
// watch keeps it fresh, and a dead one's cursor simply expires.
func (s *RedisStore) RecordWatchCursor(
	ctx context.Context,
	gitTargetUID string,
	gvr schema.GroupVersionResource,
	namespace, rv string,
) error {
	if rv == "" {
		return nil
	}
	if err := s.client.Set(ctx, s.watchCursorKey(gitTargetUID, gvr, namespace), rv, watchCursorTTL).Err(); err != nil {
		return fmt.Errorf("store watch cursor: %w", err)
	}
	return nil
}

// watchCursorKey builds a readable cursor key naming the store and leaf directly, e.g.
// "gitops-reverser:watch:v1:target:<uid>:apps/deployments:namespace:team-a:last-rv" or
// "…:configmaps:cluster:last-rv". The GitTarget UID is globally unique, so its
// namespace/name would be redundant. The GVR version is dropped: a resourceVersion is a
// per-resource counter shared across served versions, so it is redundant in a resume
// cursor. The namespace scope stays, because the live data plane opens one raw watch per
// (GitTarget, GVR, scope) and a namespaced watch must not resume a cluster-wide one.
func (s *RedisStore) watchCursorKey(
	gitTargetUID string,
	gvr schema.GroupVersionResource,
	namespace string,
) string {
	scope := "cluster"
	if namespace != "" {
		scope = "namespace:" + escapeKeyField(namespace)
	}
	return keyPrefix + watchCursorKeySuffix + "target:" + escapeKeyField(gitTargetUID) + ":" +
		groupResourceKey(gvr.Group, gvr.Resource) + ":" + scope + ":last-rv"
}
