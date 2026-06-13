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

// tailChange builds one per-event audit-tail change for configmaps, stamped with its stream rv —
// the shape ReadTypeAuditChanges hands applyAuditChangesForType.
func tailChange(namespace, name, rv string) git.Event {
	return git.Event{
		Object:     uns("ConfigMap", namespace, name),
		Identifier: itypes.NewResourceIdentifier("", "v1", "configmaps", namespace, name),
		Operation:  "CREATE",
		AuditRV:    rv,
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

	// The target's reconcile through coverage head Hc=117 has been enqueued; its watermark is now
	// published (the batch-cm-2 @117 case from §4 of the doc).
	m.publishTargetTypeWatermark(myTargetRef(), configmapsGVR, "117")

	m.applyAuditChangesForType(context.Background(), logr.Discard(), configmapsGVR, []git.Event{
		tailChange("ns-a", "batch-cm-2", "117"), // rv == Hc: already reconciled -> suppress
		tailChange("ns-a", "cm-live", "130"),    // rv  > Hc: live for this target -> route
	})

	assert.Equal(t, []string{"cm-live"}, rec.names(),
		"the reconciled-through entry (rv<=Hc) must be suppressed; only the strictly-newer entry is live")
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
		tailChange("ns-a", "cm-early", "200"),
	})

	assert.Empty(t, rec.names(), "a NotReconciled target suppresses every tail entry until its first reconcile")
}

// TestAuditTailFanout_RoutesAllAboveWatermark proves the live side of the gate: once a boundary is
// established, every strictly-newer entry is routed (the watermark only suppresses the historical
// band, it does not stall live freshness).
func TestAuditTailFanout_RoutesAllAboveWatermark(t *testing.T) {
	store := rulestore.NewStore()
	addConfigmapsWatchRule(store)
	m := streamingManager(t, gitTargetFixture(), store)
	rec := registerRecordingStream(t, m)

	m.publishTargetTypeWatermark(myTargetRef(), configmapsGVR, "100")

	m.applyAuditChangesForType(context.Background(), logr.Discard(), configmapsGVR, []git.Event{
		tailChange("ns-a", "cm-a", "101"),
		tailChange("ns-a", "cm-b", "102"),
		tailChange("ns-a", "cm-c", "103"),
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

	m.publishTargetTypeWatermark(myTargetRef(), configmapsGVR, "100")

	m.applyAuditChangesForType(context.Background(), logr.Discard(), configmapsGVR, []git.Event{
		tailChange("ns-a", "cm-in", "200"),      // in scope, above Hc -> route
		tailChange("ns-other", "cm-out", "201"), // out of scope -> drop
	})

	assert.Equal(t, []string{"cm-in"}, rec.names(), "an out-of-scope entry is dropped even above Hc")
}

// TestPublishTargetTypeWatermark_Monotonic proves a published watermark advances but never retreats
// while a boundary is held (§7.3): a re-anchor with a higher Hc moves it forward, a lower Hc is
// ignored so a stale-low value can never start suppressing legitimate live events.
func TestPublishTargetTypeWatermark_Monotonic(t *testing.T) {
	m := &Manager{Log: logr.Discard()}

	m.publishTargetTypeWatermark(myTargetRef(), configmapsGVR, "100")
	hc, ok := m.targetTypeWatermarkFor(myTargetRef(), configmapsGVR)
	require.True(t, ok)
	assert.Equal(t, "100", hc)

	m.publishTargetTypeWatermark(myTargetRef(), configmapsGVR, "150") // a re-anchor advances it
	hc, _ = m.targetTypeWatermarkFor(myTargetRef(), configmapsGVR)
	assert.Equal(t, "150", hc)

	m.publishTargetTypeWatermark(myTargetRef(), configmapsGVR, "120") // a lower Hc must not retreat it
	hc, _ = m.targetTypeWatermarkFor(myTargetRef(), configmapsGVR)
	assert.Equal(t, "150", hc, "the watermark holds the higher boundary")

	m.publishTargetTypeWatermark(myTargetRef(), configmapsGVR, "") // a blank Hc is a no-op
	hc, _ = m.targetTypeWatermarkFor(myTargetRef(), configmapsGVR)
	assert.Equal(t, "150", hc)
}

// TestClearTargetTypeWatermarks_ResetsToNotReconciled proves a delete clears the boundary so a
// recreate restarts at NotReconciled rather than inheriting a stale-high Hc (§7.3).
func TestClearTargetTypeWatermarks_ResetsToNotReconciled(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	m.publishTargetTypeWatermark(myTargetRef(), configmapsGVR, "100")
	m.publishTargetTypeWatermark(myTargetRef(), secretsGVR, "200")

	m.clearTargetTypeWatermarks(myTargetRef())

	_, ok := m.targetTypeWatermarkFor(myTargetRef(), configmapsGVR)
	assert.False(t, ok, "configmaps boundary is cleared")
	_, ok = m.targetTypeWatermarkFor(myTargetRef(), secretsGVR)
	assert.False(t, ok, "every type's boundary for the target is cleared")
}

// TestForgetGitTargetDeclaration_ClearsWatermarks proves the delete hook the controller calls also
// clears the coverage watermarks, so the two recreate-safety resets stay together.
func TestForgetGitTargetDeclaration_ClearsWatermarks(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	m.publishTargetTypeWatermark(myTargetRef(), configmapsGVR, "100")

	m.ForgetGitTargetDeclaration(myTargetRef())

	_, ok := m.targetTypeWatermarkFor(myTargetRef(), configmapsGVR)
	assert.False(t, ok, "ForgetGitTargetDeclaration clears the per-type watermarks too")
}

// TestRVAboveWatermark proves the boundary comparison: rv<=Hc is historical (suppress), rv>Hc is
// live (route), and an unparseable rv prefers routing over suppressing (§7.3).
func TestRVAboveWatermark(t *testing.T) {
	assert.False(t, rvAboveWatermark("100", "100"), "rv == Hc is historical")
	assert.False(t, rvAboveWatermark("99", "100"), "rv < Hc is historical")
	assert.True(t, rvAboveWatermark("101", "100"), "rv > Hc is live")
	assert.True(t, rvAboveWatermark("", "100"), "an unparseable rv prefers routing")
	assert.True(t, rvAboveWatermark("abc", "100"), "a non-numeric rv prefers routing")
}
