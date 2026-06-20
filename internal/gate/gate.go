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

// Package gate implements the demand gate for audit ingestion: the shared, multi-pod set of
// resource types whose audit events should be mirrored into per-type Redis streams. Without it the
// webhook mirrors every audited type unconditionally — cost scales with cluster type-cardinality,
// not demand ("the Redis explosion"). See docs/finished/demand-gated-audit-ingestion.md.
//
// The signal is one Redis SET ("<prefix>:__required__") of per-type base keys, fanned out to every
// ingest pod via a tiny ping STREAM ("<prefix>:__required__:updates"). Each pod keeps a local copy:
// it seeds with SMEMBERS, then a background loop wakes on the ping stream (XREAD) and re-reads the
// whole set, with a slow poll as a backstop for a missed ping. The audit hot path calls Allow,
// which is an in-memory lookup — no Redis round-trip per event.
//
// Correctness posture (design §3/§6): the gate is best-effort and eventually consistent. Mirroring
// a type slightly too early is free (the extra entries trim away); slightly too late can miss an
// event, but the periodic checkpoint re-LIST heals it — so a miss costs freshness, not integrity.
// That is why this can be a cheap shared cache instead of a synchronously-coordinated lock, and why
// multiple writers do not need coordination at the membership level: SADD is idempotent and a
// transient SREM/SADD disagreement resolves to a checkpoint-healed miss or harmless over-capture.
package gate

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/queue"
)

const (
	requiredSetSuffix     = ":__required__"
	requiredUpdatesSuffix = ":__required__:updates"

	// defaultPingMaxLen bounds the update stream. Only the latest entry matters (a wakeup to
	// re-read the whole set), so a handful of entries is plenty; the rate is bounded by claim
	// churn, never by event volume.
	defaultPingMaxLen = 16
	// defaultSlowPoll is the backstop re-read cadence — it converges a pod even if a ping is lost
	// or the blocking read never wakes (e.g. an emulator that does not notify across connections).
	defaultSlowPoll = 30 * time.Second
	// defaultBlock is how long one XREAD parks waiting for a ping. On real Redis a ping wakes it
	// immediately; the timeout just lets the loop re-check ctx and lets the slow poll cover gaps.
	defaultBlock = 5 * time.Second
	// pingReadCount bounds how many pings one XREAD drains; they all mean the same thing
	// ("re-read the set"), so a handful is plenty.
	pingReadCount = 16
	// errBackoff is how long the subscriber waits after a transient XREAD error before retrying;
	// the slow poll keeps the set converging meanwhile.
	errBackoff = time.Second
)

// Config configures the gate's Redis connection (mirrors the relevant fields of the by-type queue
// config) plus the two tunables. The zero PingMaxLen/SlowPoll/Block fall back to the defaults.
type Config struct {
	Addr       string
	Username   string
	AuthValue  string
	DB         int
	Prefix     string
	TLSEnabled bool
	PingMaxLen int64
	SlowPoll   time.Duration
	Block      time.Duration
	// AlwaysAllow lists types whose audit events must be mirrored regardless of GitTarget demand,
	// because an INTERNAL consumer reads their per-type stream. These types are allowed on every pod
	// from startup (config-driven, identical everywhere) and never released. See
	// docs/finished/demand-gated-audit-ingestion.md.
	AlwaysAllow []schema.GroupVersionResource
}

// Gate is the per-pod view of the shared required-types set. It is safe for concurrent use: Allow
// (hot path) takes a read lock on the local set; the subscriber loop and the writer methods take
// the write lock when they swap/update it.
type Gate struct {
	client      *redis.Client
	prefix      string
	requiredKey string
	updatesKey  string
	pingMaxLen  int64
	slowPoll    time.Duration
	block       time.Duration

	// static is the always-allowed base-key set (from Config.AlwaysAllow). It is read-only after
	// New, so Allow reads it without a lock; it is unioned with the dynamic members.
	static map[string]struct{}

	mu      sync.RWMutex
	members map[string]struct{}

	// lastID is the subscriber cursor on the ping stream. It is set by Seed and advanced only by
	// the single Run goroutine, so it needs no lock.
	lastID string
}

// New builds a gate from config. It does not contact Redis; call Seed before serving and Run for
// the ongoing refresh.
func New(cfg Config) (*Gate, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		return nil, errors.New("redis address is required")
	}
	prefix := strings.TrimSpace(cfg.Prefix)
	if prefix == "" {
		prefix = queue.DefaultRedisByTypeStreamPrefix
	}
	opts := &redis.Options{
		Addr:     cfg.Addr,
		Username: cfg.Username,
		Password: cfg.AuthValue,
		DB:       cfg.DB,
	}
	if cfg.TLSEnabled {
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	pingMaxLen := cfg.PingMaxLen
	if pingMaxLen <= 0 {
		pingMaxLen = defaultPingMaxLen
	}
	slowPoll := cfg.SlowPoll
	if slowPoll <= 0 {
		slowPoll = defaultSlowPoll
	}
	block := cfg.Block
	if block <= 0 {
		block = defaultBlock
	}
	static := make(map[string]struct{}, len(cfg.AlwaysAllow))
	for _, g := range cfg.AlwaysAllow {
		static[queue.TypeBaseKey(prefix, g.Group, g.Resource, "")] = struct{}{}
	}
	return &Gate{
		client:      redis.NewClient(opts),
		prefix:      prefix,
		requiredKey: prefix + requiredSetSuffix,
		updatesKey:  prefix + requiredUpdatesSuffix,
		pingMaxLen:  pingMaxLen,
		slowPoll:    slowPoll,
		block:       block,
		static:      static,
		members:     map[string]struct{}{},
	}, nil
}

// Start seeds the gate, then runs the subscriber loop until ctx is cancelled. Its signature
// satisfies the controller-runtime manager.Runnable interface, so the gate can be added to the
// manager directly. A seed failure is non-fatal — Run re-seeds and the slow poll converges once
// Redis is reachable, and the read side tolerates a not-yet-seeded gate (Allow returns false until
// then, a checkpoint-healed miss per the best-effort posture).
func (g *Gate) Start(ctx context.Context) error {
	_ = g.Seed(ctx)
	g.Run(ctx)
	return nil
}

// Close releases the Redis connection pool.
func (g *Gate) Close() error { return g.client.Close() }

// Ping checks gate-store liveness (used by the readiness gate alongside the queue's Ping).
func (g *Gate) Ping(ctx context.Context) error { return g.client.Ping(ctx).Err() }

// baseKey is the type's membership key, derived identically to the mirror (queue.TypeBaseKey) so a
// gate decision and a mirror write agree on the same type. Callers pass the PARENT (group,
// resource): a scale subresource is folded to its parent before Allow, matching the mirror's
// baseKey, and the materializer only ever Requires parent GVRs.
func (g *Gate) baseKey(group, resource string) string {
	return queue.TypeBaseKey(g.prefix, group, resource, "")
}

// Allow reports whether this type is currently required — an O(1) in-memory lookup on the audit hot
// path, never a Redis call. A missing resource (the "__unknown__" bucket) is never wanted.
func (g *Gate) Allow(group, resource string) bool {
	if strings.TrimSpace(resource) == "" {
		return false
	}
	key := g.baseKey(group, resource)
	if _, ok := g.static[key]; ok {
		return true // always-allowed internal-consumer type (e.g. commitrequests)
	}
	g.mu.RLock()
	_, ok := g.members[key]
	g.mu.RUnlock()
	return ok
}

// Require marks a type as wanted: SADD into the shared set and, only when membership actually
// changed, a ping so other pods re-read. It updates the local set immediately so the writer pod
// does not wait for its own ping. Idempotent — a re-anchor that re-Requires a present type adds
// nothing and does not ping.
func (g *Gate) Require(ctx context.Context, gvr schema.GroupVersionResource) error {
	key := g.baseKey(gvr.Group, gvr.Resource)
	added, err := g.client.SAdd(ctx, g.requiredKey, key).Result()
	if err != nil {
		return fmt.Errorf("gate require %q: %w", key, err)
	}
	g.mu.Lock()
	g.members[key] = struct{}{}
	g.mu.Unlock()
	if added > 0 {
		g.ping(ctx)
	}
	return nil
}

// Unrequire marks a type as no longer wanted: SREM from the shared set + a ping when it changed.
// It is called on the demand Released event, alongside DeleteType (the watch layer), so new
// mirroring stops across pods within a ping.
func (g *Gate) Unrequire(ctx context.Context, gvr schema.GroupVersionResource) error {
	key := g.baseKey(gvr.Group, gvr.Resource)
	removed, err := g.client.SRem(ctx, g.requiredKey, key).Result()
	if err != nil {
		return fmt.Errorf("gate unrequire %q: %w", key, err)
	}
	g.mu.Lock()
	delete(g.members, key)
	g.mu.Unlock()
	if removed > 0 {
		g.ping(ctx)
	}
	return nil
}

// ping appends a wakeup to the update stream. The payload is irrelevant — subscribers re-read the
// whole set — so it carries only a coarse timestamp for debuggability. Best-effort: a lost ping is
// covered by the slow poll.
func (g *Gate) ping(ctx context.Context) {
	_ = g.client.XAdd(ctx, &redis.XAddArgs{
		Stream: g.updatesKey,
		MaxLen: g.pingMaxLen,
		Values: map[string]any{"at": time.Now().UnixMilli()},
	}).Err()
}

// Seed loads the current required set into the local cache and pins the subscriber cursor. The
// cursor is captured BEFORE the SMEMBERS snapshot, so a membership change that races the seed is
// re-read by the first XREAD rather than missed. Call it before the audit handler starts serving.
func (g *Gate) Seed(ctx context.Context) error {
	lastID, err := g.streamLastID(ctx)
	if err != nil {
		return err
	}
	g.lastID = lastID
	return g.refresh(ctx)
}

// Run is the subscriber loop: it parks on the ping stream and re-reads the set on each wakeup, with
// a parallel slow poll as a backstop. It blocks until ctx is cancelled; run it in its own goroutine
// after Seed.
func (g *Gate) Run(ctx context.Context) {
	if g.lastID == "" {
		if err := g.Seed(ctx); err != nil && ctx.Err() == nil {
			// Seed failed but we must not spin; the loop's XREAD/poll will converge once Redis is back.
			g.lastID = "0"
		}
	}
	go g.slowPollLoop(ctx)
	for ctx.Err() == nil {
		g.readAndRefresh(ctx)
	}
}

// readAndRefresh parks on one XREAD wakeup and re-reads the set. A block-timeout (redis.Nil)
// returns promptly so Run can re-check ctx; a transient error backs off (the slow poll covers the
// gap). It advances the subscriber cursor past the consumed pings.
func (g *Gate) readAndRefresh(ctx context.Context) {
	res, err := g.client.XRead(ctx, &redis.XReadArgs{
		Streams: []string{g.updatesKey, g.lastID},
		Block:   g.block,
		Count:   pingReadCount,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return // block elapsed with no ping
	}
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(errBackoff):
		}
		return
	}
	for i := range res {
		if msgs := res[i].Messages; len(msgs) > 0 {
			g.lastID = msgs[len(msgs)-1].ID
		}
	}
	_ = g.refresh(ctx)
}

func (g *Gate) slowPollLoop(ctx context.Context) {
	ticker := time.NewTicker(g.slowPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = g.refresh(ctx)
		}
	}
}

// refresh replaces the local set with a fresh SMEMBERS snapshot. Re-reading the whole set (rather
// than applying deltas) makes a missed ping harmless — the next refresh converges regardless.
func (g *Gate) refresh(ctx context.Context) error {
	members, err := g.client.SMembers(ctx, g.requiredKey).Result()
	if err != nil {
		return fmt.Errorf("gate refresh: %w", err)
	}
	set := make(map[string]struct{}, len(members))
	for _, m := range members {
		set[m] = struct{}{}
	}
	g.mu.Lock()
	g.members = set
	g.mu.Unlock()
	return nil
}

// streamLastID returns the ping stream's top ID, or "0" when the stream does not exist yet (so the
// first XREAD starts from the beginning and cannot miss the first ping).
func (g *Gate) streamLastID(ctx context.Context) (string, error) {
	info, err := g.client.XInfoStream(ctx, g.updatesKey).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) || strings.Contains(err.Error(), "no such key") {
			return "0", nil
		}
		return "", fmt.Errorf("gate seed xinfo: %w", err)
	}
	return info.LastGeneratedID, nil
}
