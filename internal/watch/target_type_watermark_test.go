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

package watch

import (
	"context"
	"sort"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/reconcile"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// recordingEnqueuer captures the events a GitTargetEventStream forwards, so a fan-out test can
// assert exactly which audit-tail entries were routed as live per-event writes.
type recordingEnqueuer struct {
	mu     sync.Mutex
	events []git.Event
}

func (r *recordingEnqueuer) Enqueue(e git.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recordingEnqueuer) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Identifier.Name)
	}
	sort.Strings(out)
	return out
}

// addConfigmapsWatchRule registers a namespaced WatchRule in ns-a for my-target watching
// configmaps — the watched type the per-target watermark fan-out tests route against (the secrets
// fixture's twin).
func addConfigmapsWatchRule(store *rulestore.RuleStore) {
	store.AddOrUpdateWatchRule(
		configv1alpha1.WatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "wr-configmaps", Namespace: "ns-a"},
			Spec: configv1alpha1.WatchRuleSpec{
				TargetRef: configv1alpha1.LocalTargetReference{Name: "my-target"},
				Rules: []configv1alpha1.ResourceRule{{
					APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"configmaps"},
				}},
			},
		},
		"my-target", "gitops-reverser", "provider", "gitops-reverser", "main", "live",
	)
}

// registerRecordingStream wires an EventRouter onto the Manager with a recording event stream for
// my-target, so applyAuditChangesForType has somewhere to route to and the test can observe it.
func registerRecordingStream(t *testing.T, m *Manager) *recordingEnqueuer {
	t.Helper()
	er := NewEventRouter(nil, m, m.Client, logr.Discard())
	rec := &recordingEnqueuer{}
	stream := reconcile.NewGitTargetEventStream("my-target", "gitops-reverser", rec, logr.Discard())
	er.RegisterGitTargetEventStream(myTargetRef(), stream)
	m.EventRouter = er
	return rec
}

// tailChange builds one per-event audit-tail change for configmaps, stamped with its full stream
// position "<rv>-<seq>" — the shape ReadTypeAuditChanges hands applyAuditChangesForType.
func tailChange(namespace, name, streamID string) git.Event {
	return git.Event{
		Object:        uns("ConfigMap", namespace, name),
		Identifier:    itypes.NewResourceIdentifier("", "v1", "configmaps", namespace, name),
		Operation:     "CREATE",
		AuditStreamID: streamID,
	}
}

// TestAuditTailFanout_SuppressesHistoricalForReconciledTarget is the red-first proof of the
// per-(GitTarget, GVR) coverage watermark (signing-snapshot-tail-replay-failure-investigation.md
// §8): a target whose reconcile covered the type through Hc=117 must NOT have the audit tail
// re-deliver an entry at rv<=Hc as a live per-event commit, while a genuinely newer entry (rv>Hc)
// is still routed. Against the pre-gate fan-out BOTH entries route and this fails.
func TestAuditTailFanout_SuppressesHistoricalForReconciledTarget(t *testing.T) {
	store := rulestore.NewStore()
	addConfigmapsWatchRule(store)
	m := streamingManager(t, gitTargetFixture(), store)
	rec := registerRecordingStream(t, m)

	// The target's reconcile through coverage head Hc=117-0 has been enqueued; its watermark is now
	// published (the batch-cm-2 @117 case from §4 of the doc).
	m.publishTargetTypeWatermark(myTargetRef(), configmapsGVR, "117-0")

	m.applyAuditChangesForType(context.Background(), logr.Discard(), configmapsGVR, []git.Event{
		tailChange("ns-a", "batch-cm-2", "117-0"), // id == Hc: already reconciled -> suppress
		tailChange("ns-a", "cm-live", "130-0"),    // id  > Hc: live for this target -> route
	})

	assert.Equal(t, []string{"cm-live"}, rec.names(),
		"the reconciled-through entry (id<=Hc) must be suppressed; only the strictly-newer entry is live")
}

// TestAuditTailFanout_SuppressesUntilReconciled proves the NotReconciled state (§7.1): a target that
// watches the type but has no watermark yet suppresses every tail entry — its first reconcile owns
// the type's initial history, so the tail must not race ahead with per-event commits.
func TestAuditTailFanout_SuppressesUntilReconciled(t *testing.T) {
	store := rulestore.NewStore()
	addConfigmapsWatchRule(store)
	m := streamingManager(t, gitTargetFixture(), store)
	rec := registerRecordingStream(t, m)

	// No publishTargetTypeWatermark: the target is NotReconciled.
	m.applyAuditChangesForType(context.Background(), logr.Discard(), configmapsGVR, []git.Event{
		tailChange("ns-a", "cm-early", "200-0"),
	})

	assert.Empty(t, rec.names(), "a NotReconciled target suppresses every tail entry until its first reconcile")
}

// TestAuditTailFanout_RoutesSameRVHigherSeq is the seq-sensitivity guard: an entry that shares the
// coverage head's rv but has a HIGHER sub-sequence arrived after the reconcile's fold, so it is live
// and must be routed. A bare-rv comparison would wrongly suppress it (it would compute rv<=Hc),
// silently dropping e.g. an rv-less DELETE riding the high-water — the more dangerous failure of §7.3.
func TestAuditTailFanout_RoutesSameRVHigherSeq(t *testing.T) {
	store := rulestore.NewStore()
	addConfigmapsWatchRule(store)
	m := streamingManager(t, gitTargetFixture(), store)
	rec := registerRecordingStream(t, m)

	// The reconcile folded the log through 150-3.
	m.publishTargetTypeWatermark(myTargetRef(), configmapsGVR, "150-3")

	m.applyAuditChangesForType(context.Background(), logr.Discard(), configmapsGVR, []git.Event{
		tailChange("ns-a", "cm-folded", "150-3"),  // == Hc: already folded -> suppress
		tailChange("ns-a", "cm-same-rv", "150-7"), // same rv, higher seq: arrived after the fold -> route
	})

	assert.Equal(t, []string{"cm-same-rv"}, rec.names(),
		"a same-rv entry with a higher seq than Hc is live and must be routed, not suppressed")
}

// TestAuditTailFanout_RoutesAllAboveWatermark proves the live side of the gate: once a boundary is
// established, every strictly-newer entry is routed (the watermark only suppresses the historical
// band, it does not stall live freshness).
func TestAuditTailFanout_RoutesAllAboveWatermark(t *testing.T) {
	store := rulestore.NewStore()
	addConfigmapsWatchRule(store)
	m := streamingManager(t, gitTargetFixture(), store)
	rec := registerRecordingStream(t, m)

	m.publishTargetTypeWatermark(myTargetRef(), configmapsGVR, "100-0")

	m.applyAuditChangesForType(context.Background(), logr.Discard(), configmapsGVR, []git.Event{
		tailChange("ns-a", "cm-a", "101-0"),
		tailChange("ns-a", "cm-b", "102-0"),
		tailChange("ns-a", "cm-c", "103-0"),
	})

	assert.Equal(t, []string{"cm-a", "cm-b", "cm-c"}, rec.names(), "every entry above Hc is live and routed")
}

// TestAuditTailFanout_RespectsNamespaceScope proves the watermark gate composes with the existing
// namespace-scope check: an out-of-scope entry is dropped regardless of its rv.
func TestAuditTailFanout_RespectsNamespaceScope(t *testing.T) {
	store := rulestore.NewStore()
	addConfigmapsWatchRule(store)
	m := streamingManager(t, gitTargetFixture(), store)
	rec := registerRecordingStream(t, m)

	m.publishTargetTypeWatermark(myTargetRef(), configmapsGVR, "100-0")

	m.applyAuditChangesForType(context.Background(), logr.Discard(), configmapsGVR, []git.Event{
		tailChange("ns-a", "cm-in", "200-0"),      // in scope, above Hc -> route
		tailChange("ns-other", "cm-out", "201-0"), // out of scope -> drop
	})

	assert.Equal(t, []string{"cm-in"}, rec.names(), "an out-of-scope entry is dropped even above Hc")
}

// TestPublishTargetTypeWatermark_Monotonic proves a published watermark advances but never retreats
// while a boundary is held (§7.3), by full stream position: a re-anchor with a later Hc moves it
// forward (including a same-rv higher seq), an earlier Hc is ignored so a stale value can never start
// suppressing legitimate live events, and an unparseable Hc is a no-op.
func TestPublishTargetTypeWatermark_Monotonic(t *testing.T) {
	m := &Manager{Log: logr.Discard()}

	m.publishTargetTypeWatermark(myTargetRef(), configmapsGVR, "100-0")
	hc, ok := m.targetTypeWatermarkFor(myTargetRef(), configmapsGVR)
	require.True(t, ok)
	assert.Equal(t, "100-0", hc)

	m.publishTargetTypeWatermark(myTargetRef(), configmapsGVR, "150-0") // a re-anchor advances it
	hc, _ = m.targetTypeWatermarkFor(myTargetRef(), configmapsGVR)
	assert.Equal(t, "150-0", hc)

	m.publishTargetTypeWatermark(myTargetRef(), configmapsGVR, "150-3") // same rv, higher seq advances
	hc, _ = m.targetTypeWatermarkFor(myTargetRef(), configmapsGVR)
	assert.Equal(t, "150-3", hc, "a later sub-sequence at the same rv advances the boundary")

	m.publishTargetTypeWatermark(myTargetRef(), configmapsGVR, "150-1") // earlier seq must not retreat it
	hc, _ = m.targetTypeWatermarkFor(myTargetRef(), configmapsGVR)
	assert.Equal(t, "150-3", hc, "an earlier sub-sequence does not retreat the boundary")

	m.publishTargetTypeWatermark(myTargetRef(), configmapsGVR, "120-9") // a lower rv must not retreat it
	hc, _ = m.targetTypeWatermarkFor(myTargetRef(), configmapsGVR)
	assert.Equal(t, "150-3", hc, "the watermark holds the higher boundary")

	m.publishTargetTypeWatermark(myTargetRef(), configmapsGVR, "") // an unparseable Hc is a no-op
	hc, _ = m.targetTypeWatermarkFor(myTargetRef(), configmapsGVR)
	assert.Equal(t, "150-3", hc)
}

// TestClearTargetTypeWatermarks_ResetsToNotReconciled proves a delete clears the boundary so a
// recreate restarts at NotReconciled rather than inheriting a stale-high Hc (§7.3).
func TestClearTargetTypeWatermarks_ResetsToNotReconciled(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	m.publishTargetTypeWatermark(myTargetRef(), configmapsGVR, "100-0")
	m.publishTargetTypeWatermark(myTargetRef(), secretsGVR, "200-0")

	m.clearTargetTypeWatermarks(myTargetRef())

	_, ok := m.targetTypeWatermarkFor(myTargetRef(), configmapsGVR)
	assert.False(t, ok, "configmaps boundary is cleared")
	_, ok = m.targetTypeWatermarkFor(myTargetRef(), secretsGVR)
	assert.False(t, ok, "every type's boundary for the target is cleared")
}

// TestDeclareForGitTarget_PrunesWatermarksForDeclaimedTypes proves a watched-type-set change drops
// the boundary for a type the GitTarget no longer claims (§7.3.7): otherwise a later re-add would
// gate the tail against the stale boundary before the fresh reconcile re-publishes one. Here the
// GitTarget watches only configmaps, so a stale secrets watermark must be pruned while configmaps
// is kept. EventRouter is nil, so Declare just resolves + claims + prunes (no backfill).
func TestDeclareForGitTarget_PrunesWatermarksForDeclaimedTypes(t *testing.T) {
	store := rulestore.NewStore()
	addConfigmapsWatchRule(store) // the GitTarget now claims configmaps only — secrets was dropped
	m := streamingManager(t, gitTargetFixture(), store)

	// Boundaries left over from a prior rule set that also watched secrets.
	m.publishTargetTypeWatermark(myTargetRef(), configmapsGVR, "100-0")
	m.publishTargetTypeWatermark(myTargetRef(), secretsGVR, "200-0")

	require.NoError(t, m.DeclareForGitTarget(context.Background(), myTargetRef()))

	_, ok := m.targetTypeWatermarkFor(myTargetRef(), configmapsGVR)
	assert.True(t, ok, "a still-claimed type keeps its boundary")
	_, ok = m.targetTypeWatermarkFor(myTargetRef(), secretsGVR)
	assert.False(t, ok, "a de-claimed type's stale boundary is pruned so a re-add restarts NotReconciled")
}

// TestForgetGitTargetDeclaration_ClearsWatermarks proves the delete hook the controller calls also
// clears the coverage watermarks, so the two recreate-safety resets stay together.
func TestForgetGitTargetDeclaration_ClearsWatermarks(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	m.publishTargetTypeWatermark(myTargetRef(), configmapsGVR, "100-0")

	m.ForgetGitTargetDeclaration(myTargetRef())

	_, ok := m.targetTypeWatermarkFor(myTargetRef(), configmapsGVR)
	assert.False(t, ok, "ForgetGitTargetDeclaration clears the per-type watermarks too")
}

// TestStreamIDAfterWatermark proves the boundary comparison is by full stream position (rv, seq):
// id<=Hc is historical (suppress), id>Hc is live (route) — crucially a same-rv higher-seq entry is
// live — and an unparseable id prefers routing over suppressing (§7.3).
func TestStreamIDAfterWatermark(t *testing.T) {
	assert.False(t, streamIDAfterWatermark("100-0", "100-0"), "id == Hc is historical")
	assert.False(t, streamIDAfterWatermark("100-2", "100-3"), "same rv, lower seq is historical")
	assert.False(t, streamIDAfterWatermark("99-9", "100-0"), "lower rv is historical")
	assert.True(t, streamIDAfterWatermark("100-4", "100-3"), "same rv, HIGHER seq is live (the seq is load-bearing)")
	assert.True(t, streamIDAfterWatermark("101-0", "100-9"), "higher rv is live")
	assert.True(t, streamIDAfterWatermark("", "100-0"), "an unparseable id prefers routing")
	assert.True(t, streamIDAfterWatermark("abc", "100-0"), "a non-numeric id prefers routing")
}
