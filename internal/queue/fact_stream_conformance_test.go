// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The attribution fact transport has two implementations and ONE test suite. They are the same
// data structure — a capped, ordered log with per-follower cursors — so anything true of one has
// to be true of the other, and two transports tested apart would drift in whichever one the
// maintainers do not run daily.
//
// Every test here runs against both. Nothing implementation-specific belongs in this file.

var (
	_ FactTransport = (*RedisFactStream)(nil)
	_ FactTransport = (*MemoryFactStream)(nil)
)

const (
	// factStreamTestBlock keeps an idle Next short so a test that waits for nothing is quick.
	factStreamTestBlock = 50 * time.Millisecond
	// factStreamTestTimeout bounds a drain that never delivers, so a broken implementation fails
	// with the entries it did deliver rather than hanging.
	factStreamTestTimeout = 10 * time.Second
	// factStreamTestTTL is short enough to age an entry out inside a test, and long enough that a
	// slow machine does not age one out mid-round.
	factStreamTestTTL = 100 * time.Millisecond
	// factStreamTestAgeWait is how long a test waits for factStreamTestTTL to have elapsed.
	factStreamTestAgeWait = 300 * time.Millisecond
	// factStreamTestHorizon replays everything the transport still holds.
	factStreamTestHorizon = time.Hour
)

// factStreamParams are the knobs a conformance case needs to reach a behaviour deterministically:
// a short TTL to observe retention, a small entry cap to observe eviction, and a small read count
// to put a follower BEHIND, which is what makes a trim gap detectable.
type factStreamParams struct {
	TTL       time.Duration
	MaxLen    int64
	ReadCount int64
}

func defaultFactStreamParams() factStreamParams {
	return factStreamParams{
		TTL:       time.Minute,
		MaxLen:    DefaultFactStreamMaxLen,
		ReadCount: DefaultFactStreamReadCount,
	}
}

// factStreamImplementation names one transport and builds it for one case.
type factStreamImplementation struct {
	name  string
	build func(t *testing.T, params factStreamParams) FactTransport
}

func factStreamImplementations() []factStreamImplementation {
	return []factStreamImplementation{
		{
			name: "redis",
			build: func(t *testing.T, params factStreamParams) FactTransport {
				t.Helper()
				store, _ := newTestRedisStoreWithRedis(t)
				return store.FactStream(RedisFactStreamConfig{
					TTL:    params.TTL,
					MaxLen: params.MaxLen,
					// Trim on every publish: the interval only decides how often retention is
					// enforced, and a test that waited a minute for it would test the clock.
					TrimInterval: time.Nanosecond,
					Block:        factStreamTestBlock,
					ReadCount:    params.ReadCount,
				})
			},
		},
		{
			name: "memory",
			build: func(t *testing.T, params factStreamParams) FactTransport {
				t.Helper()
				return NewMemoryFactStream(MemoryFactStreamConfig{
					TTL:       params.TTL,
					MaxLen:    params.MaxLen,
					Block:     factStreamTestBlock,
					ReadCount: params.ReadCount,
				})
			},
		},
	}
}

// runFactStreamConformance runs one case against every implementation.
func runFactStreamConformance(
	t *testing.T,
	params factStreamParams,
	test func(t *testing.T, transport FactTransport),
) {
	t.Helper()
	for _, impl := range factStreamImplementations() {
		t.Run(impl.name, func(t *testing.T) {
			test(t, impl.build(t, params))
		})
	}
}

func factStreamTestKey(groupResource string) FactStreamKey {
	return FactStreamKey{AuditRoute: "prod-eu-1", GroupResource: groupResource}
}

// authorFacts builds one batch naming the given authors.
func authorFacts(authors ...string) []AuthorFact {
	facts := make([]AuthorFact, 0, len(authors))
	for _, author := range authors {
		facts = append(facts, AuthorFact{Author: author, Verb: "update"})
	}
	return facts
}

// factAuthors flattens delivered entries back to the authors they carry, in delivery order.
func factAuthors(entries []FactEntry) []string {
	var authors []string
	for _, entry := range entries {
		for _, fact := range entry.Facts {
			authors = append(authors, fact.Author)
		}
	}
	return authors
}

// drainFactEntries reads until want entries have arrived, failing with what it did get.
func drainFactEntries(t *testing.T, sub FactSubscription, want int) []FactEntry {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), factStreamTestTimeout)
	defer cancel()
	entries, err := readFactEntries(ctx, sub, want)
	require.NoErrorf(t, err, "wanted %d entries, delivered %d", want, len(entries))
	return entries
}

// readFactEntries is the assertion-free half of drainFactEntries, so a follower can be drained
// from a goroutine and judged on the test's own.
func readFactEntries(ctx context.Context, sub FactSubscription, want int) ([]FactEntry, error) {
	var entries []FactEntry
	for len(entries) < want {
		delivery, err := sub.Next(ctx)
		if err != nil {
			return entries, err
		}
		entries = append(entries, delivery.Entries...)
	}
	return entries, nil
}

// expectNoFactEntries asserts that nothing more arrives within one block period.
func expectNoFactEntries(t *testing.T, sub FactSubscription) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), factStreamTestTimeout)
	defer cancel()
	delivery, err := sub.Next(ctx)
	require.NoError(t, err)
	require.Empty(t, factAuthors(delivery.Entries))
}

func TestFactStreamConformance_AppendsAreFollowedInOrder(t *testing.T) {
	runFactStreamConformance(t, defaultFactStreamParams(), func(t *testing.T, transport FactTransport) {
		ctx := t.Context()
		key := factStreamTestKey("configmaps")
		sub := transport.FollowFacts([]FactStreamKey{key}, factStreamTestHorizon)

		require.NoError(t, transport.PublishFacts(ctx, key, authorFacts("alice")))
		require.NoError(t, transport.PublishFacts(ctx, key, authorFacts("bob", "carol")))
		require.NoError(t, transport.PublishFacts(ctx, key, authorFacts("dave")))

		entries := drainFactEntries(t, sub, 3)
		require.Len(t, entries, 3)
		// One batch is one entry, and the batch keeps its own order inside it.
		require.Equal(t, []string{"alice", "bob", "carol", "dave"}, factAuthors(entries))
		for _, entry := range entries {
			require.Equal(t, key, entry.Key)
		}
		require.Negative(t, compareStreamIDs(entries[0].ID, entries[1].ID))
		require.Negative(t, compareStreamIDs(entries[1].ID, entries[2].ID))
	})
}

func TestFactStreamConformance_EmptyBatchIsNotAppended(t *testing.T) {
	runFactStreamConformance(t, defaultFactStreamParams(), func(t *testing.T, transport FactTransport) {
		ctx := t.Context()
		key := factStreamTestKey("configmaps")
		sub := transport.FollowFacts([]FactStreamKey{key}, factStreamTestHorizon)

		require.NoError(t, transport.PublishFacts(ctx, key, nil))
		expectNoFactEntries(t, sub)
	})
}

func TestFactStreamConformance_LateFollowerReplaysFromHorizon(t *testing.T) {
	runFactStreamConformance(t, defaultFactStreamParams(), func(t *testing.T, transport FactTransport) {
		ctx := t.Context()
		key := factStreamTestKey("apps/deployments")

		// Everything a restart, a reconnect, or a brand new watch would have missed.
		require.NoError(t, transport.PublishFacts(ctx, key, authorFacts("alice")))
		require.NoError(t, transport.PublishFacts(ctx, key, authorFacts("bob")))

		sub := transport.FollowFacts([]FactStreamKey{key}, factStreamTestHorizon)
		require.Equal(t, []string{"alice", "bob"}, factAuthors(drainFactEntries(t, sub, 2)))

		// And it keeps up from there rather than replaying the window again.
		require.NoError(t, transport.PublishFacts(ctx, key, authorFacts("carol")))
		require.Equal(t, []string{"carol"}, factAuthors(drainFactEntries(t, sub, 1)))
	})
}

func TestFactStreamConformance_RetentionTrimDropsAgedEntries(t *testing.T) {
	params := defaultFactStreamParams()
	params.TTL = factStreamTestTTL
	runFactStreamConformance(t, params, func(t *testing.T, transport FactTransport) {
		ctx := t.Context()
		key := factStreamTestKey("configmaps")

		require.NoError(t, transport.PublishFacts(ctx, key, authorFacts("aged-out")))
		time.Sleep(factStreamTestAgeWait)
		require.NoError(t, transport.PublishFacts(ctx, key, authorFacts("retained")))

		// A follower asking for far more history than the TTL still only gets what survives.
		sub := transport.FollowFacts([]FactStreamKey{key}, factStreamTestHorizon)
		require.Equal(t, []string{"retained"}, factAuthors(drainFactEntries(t, sub, 1)))
		expectNoFactEntries(t, sub)
	})
}

func TestFactStreamConformance_EntryCapEvictsOldestFirst(t *testing.T) {
	params := defaultFactStreamParams()
	params.MaxLen = 1
	runFactStreamConformance(t, params, func(t *testing.T, transport FactTransport) {
		ctx := t.Context()
		key := factStreamTestKey("configmaps")

		require.NoError(t, transport.PublishFacts(ctx, key, authorFacts("evicted")))
		require.NoError(t, transport.PublishFacts(ctx, key, authorFacts("kept")))

		sub := transport.FollowFacts([]FactStreamKey{key}, factStreamTestHorizon)
		require.Equal(t, []string{"kept"}, factAuthors(drainFactEntries(t, sub, 1)))
	})
}

func TestFactStreamConformance_ReportsTrimGapWhenFollowerIsTrimmedPast(t *testing.T) {
	params := defaultFactStreamParams()
	params.TTL = factStreamTestTTL
	// One entry per read leaves the follower behind after the first, which is the precondition
	// for a gap: a follower that read everything there was cannot have been trimmed past.
	params.ReadCount = 1
	runFactStreamConformance(t, params, func(t *testing.T, transport FactTransport) {
		ctx := t.Context()
		key := factStreamTestKey("configmaps")

		require.NoError(t, transport.PublishFacts(ctx, key, authorFacts("first")))
		require.NoError(t, transport.PublishFacts(ctx, key, authorFacts("missed")))

		sub := transport.FollowFacts([]FactStreamKey{key}, factStreamTestHorizon)
		read := drainFactEntries(t, sub, 1)
		require.Equal(t, []string{"first"}, factAuthors(read))

		// The follower stalls long enough for retention to drop what it had not read.
		time.Sleep(factStreamTestAgeWait)
		require.NoError(t, transport.PublishFacts(ctx, key, authorFacts("after-the-gap")))

		gap, entries := drainUntilFactStreamGap(t, sub)
		require.Equal(t, key, gap.Key)
		require.Equal(t, read[0].ID, gap.Cursor)
		require.Negative(t, compareStreamIDs(gap.Cursor, gap.FirstSurviving),
			"the surviving entry must be newer than the cursor it skipped past")
		require.Equal(t, []string{"after-the-gap"}, factAuthors(entries))
	})
}

func TestFactStreamConformance_CaughtUpFollowerReportsNoTrimGap(t *testing.T) {
	params := defaultFactStreamParams()
	params.TTL = factStreamTestTTL
	runFactStreamConformance(t, params, func(t *testing.T, transport FactTransport) {
		ctx := t.Context()
		key := factStreamTestKey("configmaps")
		sub := transport.FollowFacts([]FactStreamKey{key}, factStreamTestHorizon)

		require.NoError(t, transport.PublishFacts(ctx, key, authorFacts("read")))
		require.Equal(t, []string{"read"}, factAuthors(drainFactEntries(t, sub, 1)))

		// Ordinary retention: the entry this follower already read ages out while it idles, and
		// the next entry arrives after it. Nothing was lost, so nothing may be reported.
		time.Sleep(factStreamTestAgeWait)
		require.NoError(t, transport.PublishFacts(ctx, key, authorFacts("next")))

		readCtx, cancel := context.WithTimeout(t.Context(), factStreamTestTimeout)
		defer cancel()
		for range 3 {
			delivery, err := sub.Next(readCtx)
			require.NoError(t, err)
			require.Empty(t, delivery.Gaps)
		}
	})
}

func TestFactStreamConformance_FollowedSetChangesAtRuntime(t *testing.T) {
	runFactStreamConformance(t, defaultFactStreamParams(), func(t *testing.T, transport FactTransport) {
		ctx := t.Context()
		watched := factStreamTestKey("configmaps")
		unwatched := factStreamTestKey("secrets")

		require.NoError(t, transport.PublishFacts(ctx, watched, authorFacts("alice")))
		require.NoError(t, transport.PublishFacts(ctx, unwatched, authorFacts("bob")))

		// A type nobody watches is written and never read.
		sub := transport.FollowFacts([]FactStreamKey{watched}, factStreamTestHorizon)
		require.Equal(t, []string{"alice"}, factAuthors(drainFactEntries(t, sub, 1)))
		expectNoFactEntries(t, sub)

		// A new watch on that type replays its window rather than starting empty.
		sub.SetStreams([]FactStreamKey{watched, unwatched})
		require.Equal(t, []string{"bob"}, factAuthors(drainFactEntries(t, sub, 1)))

		// And the last watch on a type going away stops its facts arriving.
		sub.SetStreams([]FactStreamKey{unwatched})
		require.NoError(t, transport.PublishFacts(ctx, watched, authorFacts("carol")))
		require.NoError(t, transport.PublishFacts(ctx, unwatched, authorFacts("dave")))
		require.Equal(t, []string{"dave"}, factAuthors(drainFactEntries(t, sub, 1)))
		expectNoFactEntries(t, sub)
	})
}

func TestFactStreamConformance_ConcurrentFollowersEachReceiveEveryEntry(t *testing.T) {
	const followers, batches = 3, 5
	runFactStreamConformance(t, defaultFactStreamParams(), func(t *testing.T, transport FactTransport) {
		ctx := t.Context()
		key := factStreamTestKey("configmaps")

		// Fan-out, not work sharing: no consumer groups, so every follower needs every entry.
		subs := make([]FactSubscription, 0, followers)
		for range followers {
			subs = append(subs, transport.FollowFacts([]FactStreamKey{key}, factStreamTestHorizon))
		}

		readCtx, cancel := context.WithTimeout(t.Context(), factStreamTestTimeout)
		defer cancel()
		var wg sync.WaitGroup
		delivered := make([][]string, followers)
		errs := make([]error, followers)
		for i, sub := range subs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				entries, err := readFactEntries(readCtx, sub, batches)
				delivered[i], errs[i] = factAuthors(entries), err
			}()
		}

		want := make([]string, 0, batches)
		for i := range batches {
			author := "author-" + string(rune('a'+i))
			want = append(want, author)
			require.NoError(t, transport.PublishFacts(ctx, key, authorFacts(author)))
		}
		wg.Wait()

		for i := range followers {
			require.NoErrorf(t, errs[i], "follower %d", i)
			require.Equalf(t, want, delivered[i], "follower %d", i)
		}
	})
}

func TestFactStreamConformance_EmptyFollowedSetWaitsAndDeliversNothing(t *testing.T) {
	runFactStreamConformance(t, defaultFactStreamParams(), func(t *testing.T, transport FactTransport) {
		// A process with no watches follows nothing, and must idle rather than spin.
		sub := transport.FollowFacts(nil, factStreamTestHorizon)
		ctx, cancel := context.WithTimeout(t.Context(), factStreamTestTimeout)
		defer cancel()
		start := time.Now()
		delivery, err := sub.Next(ctx)
		require.NoError(t, err)
		require.Empty(t, delivery.Entries)
		require.GreaterOrEqual(t, time.Since(start), factStreamTestBlock)
	})
}

func TestFactStreamConformance_EndedContextIsReported(t *testing.T) {
	runFactStreamConformance(t, defaultFactStreamParams(), func(t *testing.T, transport FactTransport) {
		key := factStreamTestKey("configmaps")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		following := transport.FollowFacts([]FactStreamKey{key}, factStreamTestHorizon)
		_, err := following.Next(ctx)
		require.Error(t, err)

		idle := transport.FollowFacts(nil, factStreamTestHorizon)
		_, err = idle.Next(ctx)
		require.Error(t, err)
	})
}

func TestFactStreamConformance_ZeroConfigIsUsable(t *testing.T) {
	// The zero value of each config is the supported configuration: every knob falls back to its
	// shared Default… constant rather than to zero.
	store, _ := newTestRedisStoreWithRedis(t)
	transports := map[string]FactTransport{
		"redis":  store.FactStream(RedisFactStreamConfig{}),
		"memory": NewMemoryFactStream(MemoryFactStreamConfig{}),
	}
	for name, transport := range transports {
		t.Run(name, func(t *testing.T) {
			key := factStreamTestKey("configmaps")
			require.NoError(t, transport.PublishFacts(t.Context(), key, authorFacts("alice")))
			sub := transport.FollowFacts([]FactStreamKey{key}, factStreamTestHorizon)
			require.Equal(t, []string{"alice"}, factAuthors(drainFactEntries(t, sub, 1)))
		})
	}
}

func TestFactStreamKey_StringNamesRouteAndType(t *testing.T) {
	require.Equal(t, "prod-eu-1/apps/deployments", factStreamTestKey("apps/deployments").String())
}

// drainUntilFactStreamGap reads until a trim gap is reported, returning it with everything
// delivered alongside and after it up to that round.
func drainUntilFactStreamGap(t *testing.T, sub FactSubscription) (FactStreamGap, []FactEntry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), factStreamTestTimeout)
	defer cancel()
	var entries []FactEntry
	for {
		delivery, err := sub.Next(ctx)
		require.NoError(t, err, "no trim gap was reported")
		entries = append(entries, delivery.Entries...)
		if len(delivery.Gaps) > 0 {
			return delivery.Gaps[0], entries
		}
	}
}
