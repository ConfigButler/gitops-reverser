// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Defaults for the attribution fact transport. They are shared by both implementations so a
// conformance test, and an operator reading one implementation's flags, sees one set of numbers.
const (
	// DefaultFactStreamMaxLen bounds one stream so a hot type cannot grow without bound between
	// retention trims. It is a count of ENTRIES, and one entry carries a whole audit batch's facts
	// for one (route, group/resource), so it is far more history than the TTL horizon usually holds.
	DefaultFactStreamMaxLen int64 = 10000

	// DefaultFactStreamReadCount bounds how many entries one Next drains per stream. It also decides
	// when a follower is considered BEHIND, which is the precondition for trim-gap detection.
	DefaultFactStreamReadCount int64 = 512

	// DefaultFactStreamBlock is how long one Next waits for a new entry before returning empty. It
	// sets how quickly a change to the followed set takes effect: a follower re-reads its set on
	// every Next, so a subscribe or unsubscribe lands within one block period.
	DefaultFactStreamBlock = time.Second

	// DefaultFactStreamTrimInterval is how often a stream is trimmed to the retention horizon.
	// Trimming is amortized onto the publish path, so this bounds the extra command rate rather
	// than the accuracy of the horizon.
	DefaultFactStreamTrimInterval = time.Minute
)

// factStreamKeySuffix namespaces the attribution fact STREAMS, one version on from the v1 fact
// KEYS in attribution_index.go. The two are unrelated keyspaces that happen to carry the same
// facts, so they are deliberately not siblings: an install that rolls back keeps reading v1 keys
// while the v2 streams age out on their own.
const factStreamKeySuffix = ":author:v2:audit:"

// factStreamEntryField is the single stream-entry field carrying one JSON-encoded fact batch.
// One field rather than one per fact keeps an entry's shape independent of how many facts an
// audit batch produced.
const factStreamEntryField = "facts"

// streamIDSeparator splits a stream ID into its millisecond and sequence halves.
const streamIDSeparator = "-"

// FactStreamKey identifies one attribution fact stream: the audit route the facts arrived under,
// and the group/resource they are about. The route is part of the identity for the same reason it
// is part of the v1 fact keys — a fact from cluster A must never name the author of an object
// watched on cluster B. GroupResource is the API-path form groupResourceKey renders: "configmaps"
// for the core group, "apps/deployments" otherwise.
type FactStreamKey struct {
	AuditRoute    string
	GroupResource string
}

// String renders the key for logs and metrics as "<route>/<group-resource>".
func (k FactStreamKey) String() string {
	return k.AuditRoute + "/" + k.GroupResource
}

// FactEntry is one appended batch of facts, as it comes back to a follower. ID is the
// transport-assigned position, "<unix-millis>-<sequence>", which increases strictly within a
// stream and is what a follower resumes from.
type FactEntry struct {
	Key   FactStreamKey
	ID    string
	Facts []AuthorFact
}

// FactStreamGap reports that a follower was trimmed past on one stream: entries it had not read
// were dropped by retention before it got to them, so the facts they carried are lost for good.
// It is the one loss this transport can see, and reporting it is why the transport is a log with
// positions rather than fire-and-forget publish/subscribe.
type FactStreamGap struct {
	Key FactStreamKey
	// Cursor is the position the follower had reached; FirstSurviving is the oldest entry the
	// stream still holds. FirstSurviving is newer than Cursor, and everything between them is gone.
	Cursor         string
	FirstSurviving string
}

// FactDelivery is one Next's result: the entries read this round, in append order per stream, plus
// any trim gaps noticed. Both may be empty — that is an idle block period, not an error.
type FactDelivery struct {
	Entries []FactEntry
	Gaps    []FactStreamGap
}

// FactPublisher appends a batch of facts for one (route, group/resource). The audit receiver is
// its only caller: it decodes one EventList per request, groups the facts it accepted by stream,
// and appends once per group.
type FactPublisher interface {
	// PublishFacts appends facts as ONE entry on the key's stream. An empty batch is a no-op.
	// Entries appear to followers in the order they were published per stream; there is no
	// ordering promise across streams, and none is needed — an object belongs to exactly one
	// group/resource and therefore to exactly one stream.
	PublishFacts(ctx context.Context, key FactStreamKey, facts []AuthorFact) error
}

// FactFollower follows a set of streams from a horizon. The fact index is its only caller: it
// follows the union of the types any watch covers and applies what it reads into memory.
type FactFollower interface {
	// FollowFacts starts following keys, reading each from horizon before now. A horizon of the
	// fact TTL is what makes a restart cost nothing: the follower replays the whole retention
	// window before the first watch event needs it. Entries older than the horizon are skipped,
	// to within the millisecond granularity of a stream position.
	FollowFacts(keys []FactStreamKey, horizon time.Duration) FactSubscription
}

// FactSubscription is one follower's live position across its followed streams. It is not safe to
// call Next concurrently with itself; SetStreams may be called from another goroutine at any time.
// A subscription owns no resources beyond its cursors, so it is dropped rather than closed.
type FactSubscription interface {
	// SetStreams replaces the followed set. A newly followed stream starts from the horizon, so
	// it replays its retention window; an unfollowed stream's cursor is forgotten, so following it
	// again replays too. It takes effect on the next Next, hence within one block period.
	SetStreams(keys []FactStreamKey)

	// Next returns the entries appended since the last call, waiting up to the block period for
	// the first of them. An empty delivery with a nil error means the block period elapsed with
	// nothing new, which is the ordinary idle case. It returns an error only when the context ends
	// or the transport fails.
	Next(ctx context.Context) (FactDelivery, error)
}

// FactTransport is the whole seam: publish a batch, follow a set. Everything above it — the
// in-memory index, the TTL sweep, the waiter registry, the resolver — has one implementation and
// never learns which transport it has. Consumers should depend on FactPublisher or FactFollower,
// the half they use; this composition exists for wiring and for the conformance suite.
type FactTransport interface {
	FactPublisher
	FactFollower
}

// encodeFactBatch renders one batch as the entry payload. Both implementations store the encoded
// form, so an entry is an immutable snapshot in both and a follower can never see a fact mutate
// under it.
func encodeFactBatch(facts []AuthorFact) ([]byte, error) {
	raw, err := json.Marshal(facts)
	if err != nil {
		return nil, fmt.Errorf("marshal fact batch: %w", err)
	}
	return raw, nil
}

// decodeFactBatch parses an entry payload back into facts.
func decodeFactBatch(raw []byte) ([]AuthorFact, error) {
	var facts []AuthorFact
	if err := json.Unmarshal(raw, &facts); err != nil {
		return nil, fmt.Errorf("unmarshal fact batch: %w", err)
	}
	return facts, nil
}

// streamIDAt renders the position a stream reads from for entries appended at or after t. Stream
// IDs are millisecond timestamps, so a time horizon is a plain position and needs no side index.
// The position is EXCLUSIVE — a follower reads entries strictly after it — which is why an entry
// appended in the horizon's own millisecond may be skipped. That granularity is deliberate and is
// the one place the two implementations are allowed to differ by a hair.
func streamIDAt(t time.Time) string {
	millis := t.UnixMilli()
	if millis < 0 {
		millis = 0
	}
	return strconv.FormatInt(millis, 10) + streamIDSeparator + "0"
}

// compareStreamIDs orders two "<millis>-<seq>" positions, returning -1, 0 or 1. A malformed half
// reads as zero: IDs are transport-assigned, so this is defensive only, and ordering a
// unparseable ID first is the safe direction (it can only make a follower re-read).
func compareStreamIDs(a, b string) int {
	aMillis, aSeq := parseStreamID(a)
	bMillis, bSeq := parseStreamID(b)
	if aMillis != bMillis {
		return compareUint64(aMillis, bMillis)
	}
	return compareUint64(aSeq, bSeq)
}

func compareUint64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// parseStreamID splits a stream ID into its millisecond and sequence halves.
func parseStreamID(id string) (uint64, uint64) {
	millisPart, seqPart, _ := strings.Cut(id, streamIDSeparator)
	millis, _ := strconv.ParseUint(millisPart, 10, 64)
	seq, _ := strconv.ParseUint(seqPart, 10, 64)
	return millis, seq
}

// followState is one followed stream's position. behind records that the last read filled its
// entry budget, so more was waiting: it is the precondition for trim-gap detection, because a
// follower that read everything there was cannot have been trimmed past. Without it, ordinary
// retention — the cursor's own entry aging out while the follower idles — would report a gap on
// every stream once per TTL period. reportedGap dedupes the report while the gap persists.
type followState struct {
	cursor      string
	behind      bool
	reportedGap string
}

// followTarget is one followed stream as a reader sees it for one pass.
type followTarget struct {
	Key    FactStreamKey
	Cursor string
	Behind bool
}

// followSet is the shared cursor bookkeeping behind both implementations' subscriptions: the
// followed set, each stream's position, and the trim-gap precondition. Sharing it is what keeps
// the two transports from drifting on the parts that are not transport-specific at all.
type followSet struct {
	horizon time.Duration

	mu     sync.Mutex
	states map[FactStreamKey]*followState
}

func newFollowSet(keys []FactStreamKey, horizon time.Duration) *followSet {
	set := &followSet{horizon: horizon, states: map[FactStreamKey]*followState{}}
	set.set(keys)
	return set
}

// set replaces the followed set, starting anything newly followed at the horizon.
func (f *followSet) set(keys []FactStreamKey) {
	wanted := make(map[FactStreamKey]struct{}, len(keys))
	for _, key := range keys {
		wanted[key] = struct{}{}
	}
	from := streamIDAt(time.Now().Add(-f.horizon))

	f.mu.Lock()
	defer f.mu.Unlock()
	for key := range f.states {
		if _, ok := wanted[key]; !ok {
			delete(f.states, key)
		}
	}
	for key := range wanted {
		if _, ok := f.states[key]; !ok {
			f.states[key] = &followState{cursor: from}
		}
	}
}

// targets snapshots the followed set for one read pass, in a stable order so a follower reading
// several streams sees them the same way every time.
func (f *followSet) targets() []followTarget {
	f.mu.Lock()
	defer f.mu.Unlock()
	targets := make([]followTarget, 0, len(f.states))
	for key, state := range f.states {
		targets = append(targets, followTarget{Key: key, Cursor: state.cursor, Behind: state.behind})
	}
	slices.SortFunc(targets, func(a, b followTarget) int {
		if c := strings.Compare(a.Key.AuditRoute, b.Key.AuditRoute); c != 0 {
			return c
		}
		return strings.Compare(a.Key.GroupResource, b.Key.GroupResource)
	})
	return targets
}

// advance moves one stream's cursor to the last entry delivered and records whether more was
// waiting. A stream unfollowed during the read pass is not resurrected.
func (f *followSet) advance(key FactStreamKey, cursor string, behind bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state, ok := f.states[key]
	if !ok {
		return
	}
	state.cursor = cursor
	state.behind = behind
	if state.reportedGap != "" && compareStreamIDs(cursor, state.reportedGap) >= 0 {
		state.reportedGap = ""
	}
}

// noteGap records a trim gap and reports whether it is new. The same gap is reported once, not on
// every pass until the follower catches up past it.
func (f *followSet) noteGap(key FactStreamKey, firstSurviving string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	state, ok := f.states[key]
	if !ok || state.reportedGap == firstSurviving {
		return false
	}
	state.reportedGap = firstSurviving
	return true
}

// forgetGaps drops the dedupe marks for gaps that were detected but never delivered, so the next
// read pass reports them again.
func (f *followSet) forgetGaps(gaps []FactStreamGap) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, gap := range gaps {
		if state, ok := f.states[gap.Key]; ok && state.reportedGap == gap.FirstSurviving {
			state.reportedGap = ""
		}
	}
}

// gapIfTrimmedPast turns a stream's oldest surviving position into a gap report when the target
// was behind and that position has moved past its cursor. It is the whole detection rule, shared
// so both transports decide it identically.
func (f *followSet) gapIfTrimmedPast(target followTarget, firstSurviving string) (FactStreamGap, bool) {
	if !target.Behind || firstSurviving == "" {
		return FactStreamGap{}, false
	}
	if compareStreamIDs(target.Cursor, firstSurviving) >= 0 {
		return FactStreamGap{}, false
	}
	if !f.noteGap(target.Key, firstSurviving) {
		return FactStreamGap{}, false
	}
	return FactStreamGap{Key: target.Key, Cursor: target.Cursor, FirstSurviving: firstSurviving}, true
}

// waitBlock sleeps out a block period, reporting the context ending as an error so a follower's
// Next never reports an idle round it did not actually wait through.
func waitBlock(ctx context.Context, block time.Duration) error {
	if block <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(block)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
