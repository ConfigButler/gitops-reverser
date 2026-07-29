// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// MemoryFactStreamConfig configures the in-memory transport. Every zero field falls back to its
// Default… constant, so the zero value is the supported configuration.
type MemoryFactStreamConfig struct {
	// TTL is the retention horizon: entries older than it are dropped from the ring.
	TTL time.Duration
	// MaxLen caps one ring's entry count, oldest first. Zero means DefaultFactStreamMaxLen; a
	// negative value means no cap, leaving the TTL as the only bound.
	MaxLen int64
	// Block is how long one Next waits for a new entry, which bounds how long a change to the
	// followed set waits to take effect.
	Block time.Duration
	// ReadCount bounds how many entries one Next drains per stream.
	ReadCount int64
}

// MemoryFactStream is the in-process implementation of the attribution fact transport: one ring
// buffer per (audit route, group/resource), trimmed by TTL and entry count, with one cursor per
// follower.
//
// It is the same data structure as the Redis one rather than an approximation of it. A Redis
// stream is a capped, ordered log with per-reader cursors, which is a ring buffer: replay from the
// horizon is reading the ring from its tail, and trim-gap detection is comparing a follower's
// cursor against the ring's oldest surviving entry. Both fall out; neither is simulated.
//
// It only works when the audit receiver and the resolver are the same process. Selecting it
// alongside more than one replica is a configuration error, and belongs at startup validation
// rather than here — a transport cannot see how many replicas it has.
type MemoryFactStream struct {
	ttl       time.Duration
	maxLen    int
	block     time.Duration
	readCount int

	mu      sync.Mutex
	streams map[FactStreamKey]*memFactRing
	// signal is closed and replaced on every append, so a parked follower wakes on the next
	// publish rather than at the end of its block period.
	signal chan struct{}
}

// NewMemoryFactStream builds the in-process fact transport.
func NewMemoryFactStream(cfg MemoryFactStreamConfig) *MemoryFactStream {
	stream := &MemoryFactStream{
		ttl:       cfg.TTL,
		maxLen:    int(cfg.MaxLen),
		block:     cfg.Block,
		readCount: int(cfg.ReadCount),
		streams:   map[FactStreamKey]*memFactRing{},
		signal:    make(chan struct{}),
	}
	if stream.ttl <= 0 {
		stream.ttl = DefaultAttributionFactTTL
	}
	if cfg.MaxLen == 0 {
		stream.maxLen = int(DefaultFactStreamMaxLen)
	}
	if stream.block <= 0 {
		stream.block = DefaultFactStreamBlock
	}
	if stream.readCount <= 0 {
		stream.readCount = int(DefaultFactStreamReadCount)
	}
	return stream
}

// PublishFacts appends the batch as one entry on the key's ring and wakes every parked follower.
func (m *MemoryFactStream) PublishFacts(ctx context.Context, key FactStreamKey, facts []AuthorFact) error {
	if len(facts) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := encodeFactBatch(facts)
	if err != nil {
		return err
	}
	now := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()
	ring, ok := m.streams[key]
	if !ok {
		ring = &memFactRing{}
		m.streams[key] = ring
	}
	ring.append(raw, now)
	ring.trim(now.Add(-m.ttl), m.maxLen)
	m.dropIdleRings(now)
	close(m.signal)
	m.signal = make(chan struct{})
	return nil
}

// dropIdleRings forgets every stream whose entries have all aged out. Trimming is amortized onto
// the publish path, so without this a type that stops being written to keeps its last window of
// entries for the life of the process: nothing appends to it, so nothing ever trims it again. A
// namespace-scoped type garbage-collected once when a namespace is torn down is exactly that shape,
// and the memory is billed against a number that only ever grows — every (route, type) pair the
// process has ever seen.
//
// It runs under the publish lock, over a map holding one entry per followed type, so it is a walk
// of a few dozen entries on a path that already holds the lock. Its Redis counterpart is the EXPIRE
// that rides along with every XADD.
func (m *MemoryFactStream) dropIdleRings(now time.Time) {
	horizon := now.Add(-m.ttl)
	for key, ring := range m.streams {
		ring.trim(horizon, m.maxLen)
		if len(ring.entries) == 0 {
			delete(m.streams, key)
		}
	}
}

// FollowFacts starts following keys from horizon before now.
func (m *MemoryFactStream) FollowFacts(keys []FactStreamKey, horizon time.Duration) FactSubscription {
	return &memFactSubscription{stream: m, follow: newFollowSet(keys, horizon)}
}

// TransportKind names this transport for the metric labels.
func (m *MemoryFactStream) TransportKind() FactTransportKind {
	return FactTransportMemory
}

// wakeup returns the channel closed by the next publish.
func (m *MemoryFactStream) wakeup() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.signal
}

// readRing reads one stream under the transport lock.
func (m *MemoryFactStream) readRing(key FactStreamKey, cursor string, limit int) ([]memFactEntry, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ring, ok := m.streams[key]
	if !ok {
		return nil, ""
	}
	return ring.read(cursor, limit)
}

// memFactEntry is one appended batch, held encoded so a follower can never see it mutate.
type memFactEntry struct {
	id  string
	at  time.Time
	raw []byte
}

// memFactRing is one stream's bounded, ordered log. The slice is used as a FIFO ring: entries are
// appended at the back and trimmed from the front, so its backing array is reused rather than
// growing with the stream's lifetime.
type memFactRing struct {
	entries    []memFactEntry
	lastMillis int64
	lastSeq    int64
}

// append adds one entry, assigning the next strictly increasing "<millis>-<seq>" position. The
// sequence half is what keeps several appends within one millisecond distinct and ordered, which
// is the same rule the Redis transport gets from stream IDs.
func (r *memFactRing) append(raw []byte, now time.Time) {
	millis := now.UnixMilli()
	if millis < 0 {
		millis = 0
	}
	if millis > r.lastMillis {
		r.lastMillis, r.lastSeq = millis, 0
	} else {
		// Same millisecond, or a clock that stepped backwards: keep the position monotonic and
		// take the next sequence. A follower's cursor is only ever compared, never interpreted.
		r.lastSeq++
	}
	id := strconv.FormatInt(r.lastMillis, 10) + streamIDSeparator + strconv.FormatInt(r.lastSeq, 10)
	r.entries = append(r.entries, memFactEntry{id: id, at: now, raw: raw})
}

// trim drops entries appended before the horizon, then any beyond the entry cap, oldest first.
func (r *memFactRing) trim(horizon time.Time, maxLen int) {
	drop := 0
	for drop < len(r.entries) && r.entries[drop].at.Before(horizon) {
		drop++
	}
	if maxLen > 0 {
		if excess := len(r.entries) - drop - maxLen; excess > 0 {
			drop += excess
		}
	}
	if drop > 0 {
		r.entries = r.entries[drop:]
	}
}

// read returns up to limit entries appended after cursor, plus the oldest surviving position so a
// follower can tell it has been trimmed past.
func (r *memFactRing) read(cursor string, limit int) ([]memFactEntry, string) {
	if len(r.entries) == 0 {
		return nil, ""
	}
	oldest := r.entries[0].id
	var out []memFactEntry
	for i := range r.entries {
		if len(out) == limit {
			break
		}
		if compareStreamIDs(r.entries[i].id, cursor) <= 0 {
			continue
		}
		out = append(out, r.entries[i])
	}
	return out, oldest
}

// memFactSubscription is one follower's position across its followed rings.
type memFactSubscription struct {
	stream *MemoryFactStream
	follow *followSet
}

// SetStreams replaces the followed set; it takes effect on the next Next.
func (sub *memFactSubscription) SetStreams(keys []FactStreamKey) {
	sub.follow.set(keys)
}

// Next drains every followed ring past this follower's cursors, waiting up to the block period for
// something to arrive. It wakes early on a publish, so the block period bounds an idle round and a
// subscription change rather than delivery latency.
func (sub *memFactSubscription) Next(ctx context.Context) (FactDelivery, error) {
	deadline := time.Now().Add(sub.stream.block)
	for {
		if err := ctx.Err(); err != nil {
			return FactDelivery{}, err
		}
		// Take the wake-up channel BEFORE reading, so a publish that lands between the read and
		// the wait is not missed: it closes the channel this iteration already holds.
		signal := sub.stream.wakeup()
		delivery := sub.collect(ctx)
		if len(delivery.Entries) > 0 || len(delivery.Gaps) > 0 {
			return delivery, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return FactDelivery{}, nil
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return FactDelivery{}, ctx.Err()
		case <-signal:
			timer.Stop()
		case <-timer.C:
			return FactDelivery{}, nil
		}
	}
}

// collect reads every followed ring once, advancing the cursors it delivered.
func (sub *memFactSubscription) collect(ctx context.Context) FactDelivery {
	targets := sub.follow.targets()
	var delivery FactDelivery
	for _, target := range targets {
		entries, oldest := sub.stream.readRing(target.Key, target.Cursor, sub.stream.readCount)
		if gap, ok := sub.follow.gapIfTrimmedPast(target, oldest); ok {
			delivery.Gaps = append(delivery.Gaps, gap)
		}
		if len(entries) == 0 {
			sub.follow.caughtUp(target.Key)
			continue
		}
		for _, entry := range entries {
			facts, err := decodeFactBatch(entry.raw)
			if err != nil {
				recordFactStreamDecodeError(ctx, sub.stream.TransportKind(), target.Key, entry.id, err)
				continue
			}
			delivery.Entries = append(delivery.Entries, FactEntry{Key: target.Key, ID: entry.id, Facts: facts})
		}
		sub.follow.advance(target.Key, entries[len(entries)-1].id, len(entries) >= sub.stream.readCount)
	}
	return delivery
}
