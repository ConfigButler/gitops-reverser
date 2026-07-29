// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisFactStreamConfig configures the Redis Streams transport. Every zero field falls back to
// its Default… constant, so the zero value is the supported configuration.
type RedisFactStreamConfig struct {
	// TTL is the retention horizon: entries older than it are trimmed away. It is the same number
	// as --author-attribution-ttl, which now bounds stream retention and the in-memory index
	// together.
	TTL time.Duration
	// MaxLen caps one stream's entry count via XADD MAXLEN ~, so a hot type cannot grow without
	// bound between retention trims. Zero means DefaultFactStreamMaxLen; a negative value means no
	// cap, leaving the TTL as the only bound.
	MaxLen int64
	// TrimInterval is how often one stream is XTRIMmed to the retention horizon, zero meaning
	// DefaultFactStreamTrimInterval. The trim is amortized onto the publish path rather than run
	// from a goroutine: a stream nobody writes to needs no trimming, since nothing is arriving to
	// age. It bounds a command round trip per publish, which is why the in-memory transport, whose
	// trim is a slice re-slice, has no equivalent knob and trims on every append.
	TrimInterval time.Duration
	// Block is the XREAD BLOCK period, which bounds how long a change to the followed set waits to
	// take effect.
	Block time.Duration
	// ReadCount is the XREAD COUNT per stream per read.
	ReadCount int64
}

// RedisFactStream is the Redis Streams implementation of the attribution fact transport: one
// stream per (audit route, group/resource), appended to with XADD and followed with a single
// blocking XREAD across the followed set.
//
// It uses no consumer groups, deliberately. A consumer group distributes entries BETWEEN
// consumers, and this is a fan-out: every process watching a type needs every fact for that type.
// Each follower reads independently from its own in-memory cursor.
type RedisFactStream struct {
	client    *redis.Client
	keyPrefix string
	ttl       time.Duration
	maxLen    int64
	trimEvery time.Duration
	block     time.Duration
	readCount int64

	trimMu   sync.Mutex
	lastTrim map[string]time.Time
}

// FactStream builds the Redis Streams attribution fact transport on this store's connection, in
// the same keyspace as its other keys. The blocking follower parks on one pooled connection for
// the duration of each block period, so a process runs one follower rather than one per type.
func (s *RedisStore) FactStream(cfg RedisFactStreamConfig) *RedisFactStream {
	stream := &RedisFactStream{
		client:    s.client,
		keyPrefix: s.keyPrefix,
		ttl:       cfg.TTL,
		maxLen:    cfg.MaxLen,
		trimEvery: cfg.TrimInterval,
		block:     cfg.Block,
		readCount: cfg.ReadCount,
		lastTrim:  map[string]time.Time{},
	}
	if stream.ttl <= 0 {
		stream.ttl = DefaultAttributionFactTTL
	}
	if stream.maxLen == 0 {
		stream.maxLen = DefaultFactStreamMaxLen
	}
	if stream.trimEvery <= 0 {
		stream.trimEvery = DefaultFactStreamTrimInterval
	}
	if stream.block <= 0 {
		stream.block = DefaultFactStreamBlock
	}
	if stream.readCount <= 0 {
		stream.readCount = DefaultFactStreamReadCount
	}
	return stream
}

// PublishFacts appends the batch as one XADD on the key's stream, capped with MAXLEN ~, and trims
// the stream to the retention horizon when one is due.
func (s *RedisFactStream) PublishFacts(ctx context.Context, key FactStreamKey, facts []AuthorFact) error {
	if len(facts) == 0 {
		return nil
	}
	raw, err := encodeFactBatch(facts)
	if err != nil {
		return err
	}
	streamKey := s.streamKey(key)
	args := &redis.XAddArgs{
		Stream: streamKey,
		Values: map[string]any{factStreamEntryField: raw},
	}
	if s.maxLen > 0 {
		args.MaxLen = s.maxLen
		args.Approx = true
	}
	// EXPIRE rides along with every append, so a stream whose type stops being written to deletes
	// ITSELF one TTL later. Without it the keyspace only ever grows: MINID trimming is amortized
	// onto the publish path, so a stream that goes quiet is never trimmed again and keeps its last
	// entries for the life of the Redis instance. A namespace-scoped type that is garbage-collected
	// once and never written again — the shape a torn-down namespace produces — would otherwise
	// leave an immortal key behind for every (route, type) pair that ever saw one write.
	//
	// The deadline is refreshed by every append, so a busy stream never expires, and it matches the
	// retention horizon, so the key dies exactly when its newest entry would have aged out anyway.
	pipe := s.client.Pipeline()
	pipe.XAdd(ctx, args)
	pipe.Expire(ctx, streamKey, s.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("append facts to %q: %w", streamKey, err)
	}
	s.trimIfDue(ctx, streamKey)
	return nil
}

// FollowFacts starts following keys from horizon before now.
func (s *RedisFactStream) FollowFacts(keys []FactStreamKey, horizon time.Duration) FactSubscription {
	return &redisFactSubscription{stream: s, follow: newFollowSet(keys, horizon)}
}

// TransportKind names this transport for the metric labels.
func (s *RedisFactStream) TransportKind() FactTransportKind {
	return FactTransportRedis
}

// streamKey renders one stream's Redis key, e.g.
// "gitops-reverser:author:v2:audit:route:prod-eu-1:apps/deployments". The route sits directly
// after the domain so one route's streams share a single glob prefix.
func (s *RedisFactStream) streamKey(key FactStreamKey) string {
	return resolveKeyPrefix(s.keyPrefix) + factStreamKeySuffix + routeKeyInfix +
		escapeKeyField(key.AuditRoute) + ":" + key.groupResource()
}

// trimIfDue drops entries past the retention horizon with XTRIM MINID, at most once per stream per
// trim interval. Stream IDs are millisecond timestamps, so the horizon is a plain MINID bound.
//
// A trim failure is deliberately not returned: the MAXLEN cap on every XADD already bounds the
// stream, so a missed trim costs retention accuracy rather than safety, and failing an audit
// request over it would trade a bounded cost for an unbounded one.
func (s *RedisFactStream) trimIfDue(ctx context.Context, streamKey string) {
	now := time.Now()
	s.trimMu.Lock()
	due := now.Sub(s.lastTrim[streamKey]) >= s.trimEvery
	if due {
		s.lastTrim[streamKey] = now
	}
	s.trimMu.Unlock()
	if !due {
		return
	}
	minID := strconv.FormatInt(now.Add(-s.ttl).UnixMilli(), 10)
	_ = s.client.XTrimMinID(ctx, streamKey, minID).Err()
}

// redisFactSubscription is one follower's position across its followed streams: the cursors live
// in memory only, because a process that lost them also lost its in-memory index and is starting
// from the horizon anyway.
type redisFactSubscription struct {
	stream *RedisFactStream
	follow *followSet
}

// SetStreams replaces the followed set; it takes effect on the next Next.
func (sub *redisFactSubscription) SetStreams(keys []FactStreamKey) {
	sub.follow.set(keys)
}

// Next issues one blocking XREAD across the whole followed set and returns what it read, plus any
// stream this follower has been trimmed past. Re-issuing per call, rather than holding one long
// read open, is what lets a subscription change take effect within one block period.
func (sub *redisFactSubscription) Next(ctx context.Context) (FactDelivery, error) {
	targets := sub.follow.targets()
	if len(targets) == 0 {
		return FactDelivery{}, waitBlock(ctx, sub.stream.block)
	}
	gaps := sub.detectGaps(ctx, targets)

	byStreamKey := make(map[string]FactStreamKey, len(targets))
	streams := make([]string, 0, len(targets))
	cursors := make([]string, 0, len(targets))
	for _, target := range targets {
		streamKey := sub.stream.streamKey(target.Key)
		byStreamKey[streamKey] = target.Key
		streams = append(streams, streamKey)
		cursors = append(cursors, target.Cursor)
	}
	// XREAD takes every key first and then every cursor, positionally paired.
	streamArgs := make([]string, 0, len(streams)+len(cursors))
	streamArgs = append(streamArgs, streams...)
	streamArgs = append(streamArgs, cursors...)

	res, err := sub.stream.client.XRead(ctx, &redis.XReadArgs{
		Streams: streamArgs,
		Count:   sub.stream.readCount,
		Block:   sub.stream.block,
	}).Result()
	if errors.Is(err, redis.Nil) {
		// The whole block period elapsed with nothing on any followed stream, so every one of them
		// is caught up — including any the last read left marked behind for having exactly filled
		// its entry budget.
		for _, target := range targets {
			sub.follow.caughtUp(target.Key)
		}
		return FactDelivery{Gaps: gaps}, nil
	}
	if err != nil {
		// This round's gap reports go nowhere, so forget them: a gap deduped against a report
		// nobody received is exactly the silent loss the detection exists to prevent.
		sub.follow.forgetGaps(gaps)
		return FactDelivery{}, fmt.Errorf("read fact streams: %w", err)
	}
	return FactDelivery{Entries: sub.collect(ctx, res, byStreamKey), Gaps: gaps}, nil
}

// collect turns one XREAD result into entries and advances the cursors it delivered. An entry
// whose payload does not decode is skipped rather than retried: it can never decode, and stalling
// the follower on it would cost every later fact on that stream. It is counted and logged, because
// skipping it loses its facts and leaves no other trace.
func (sub *redisFactSubscription) collect(
	ctx context.Context,
	res []redis.XStream,
	byStreamKey map[string]FactStreamKey,
) []FactEntry {
	// XREAD returns only the streams that had something, so every followed stream missing from the
	// result was asked and gave nothing: it is caught up, whatever the last read left marked.
	delivered := make(map[FactStreamKey]struct{}, len(res))
	for i := range res {
		if key, ok := byStreamKey[res[i].Stream]; ok && len(res[i].Messages) > 0 {
			delivered[key] = struct{}{}
		}
	}
	for _, key := range byStreamKey {
		if _, ok := delivered[key]; !ok {
			sub.follow.caughtUp(key)
		}
	}

	var entries []FactEntry
	for i := range res {
		key, ok := byStreamKey[res[i].Stream]
		if !ok {
			continue
		}
		messages := res[i].Messages
		if len(messages) == 0 {
			continue
		}
		for j := range messages {
			facts, err := factsFromMessage(messages[j])
			if err != nil {
				recordFactStreamDecodeError(ctx, sub.stream.TransportKind(), key, messages[j].ID, err)
				continue
			}
			entries = append(entries, FactEntry{Key: key, ID: messages[j].ID, Facts: facts})
		}
		sub.follow.advance(key, messages[len(messages)-1].ID, int64(len(messages)) >= sub.stream.readCount)
	}
	return entries
}

// detectGaps asks each stream this follower is BEHIND on for its oldest surviving entry, and
// reports the ones that have been trimmed past the follower's cursor. Only a behind follower is
// asked, so a caught-up one costs nothing: it cannot have been trimmed past.
func (sub *redisFactSubscription) detectGaps(ctx context.Context, targets []followTarget) []FactStreamGap {
	var gaps []FactStreamGap
	for _, target := range targets {
		if !target.Behind {
			continue
		}
		oldest, err := sub.stream.client.XRangeN(ctx, sub.stream.streamKey(target.Key), "-", "+", 1).Result()
		if err != nil || len(oldest) == 0 {
			continue
		}
		if gap, ok := sub.follow.gapIfTrimmedPast(target, oldest[0].ID); ok {
			gaps = append(gaps, gap)
		}
	}
	return gaps
}

// factsFromMessage decodes one stream entry's fact batch.
func factsFromMessage(message redis.XMessage) ([]AuthorFact, error) {
	value, ok := message.Values[factStreamEntryField]
	if !ok {
		return nil, fmt.Errorf("stream entry %q carries no %q field", message.ID, factStreamEntryField)
	}
	raw, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("stream entry %q carries a non-string %q field", message.ID, factStreamEntryField)
	}
	return decodeFactBatch([]byte(raw))
}
