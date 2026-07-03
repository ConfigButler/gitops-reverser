//go:build mutationlab_e2e

// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
)

const configmapsResource = "v1/configmaps"

// TestWatchExpiredResourceVersion captures Row 16: an unresumable watch
// resourceVersion produces an ERROR/410 transport payload, after which a
// watch-only state pipeline must relist to recover the current object state.
func TestWatchExpiredResourceVersion(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "watch-resync")

	cm := &corev1.ConfigMap{ObjectMeta: s.meta("cm-resync"), Data: map[string]string{"key": "value"}}
	if _, err := h.kube.CoreV1().ConfigMaps(s.ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create configmap: %v", err)
	}
	h.quiesceAndClear(t, s.id, 3)

	records := h.probeWatch(t, watchProbeRequest{
		Scenario:      s.id,
		Mode:          "expired",
		Resource:      configmapsResource,
		Namespace:     s.ns,
		LabelSelector: scenarioLabel + "=" + s.id,
	})
	if len(records) != 1 {
		t.Fatalf("watch-resync records = %d, want exactly the ERROR transport moment", len(records))
	}
	errRecord := records[0]
	if errRecord.Source != mutationlab.SourceWatch || errRecord.Summary.WatchType != "ERROR" {
		t.Fatalf("watch-resync record = %+v, want watch ERROR", errRecord)
	}
	if code, reason := watchStatus(errRecord); code != 410 || reason != "Expired" {
		t.Fatalf("watch-resync status = %d/%s, want 410/Expired", code, reason)
	}

	list, err := h.kube.CoreV1().ConfigMaps(s.ns).List(ctx, metav1.ListOptions{
		LabelSelector: scenarioLabel + "=" + s.id,
	})
	if err != nil {
		t.Fatalf("relist after watch ERROR: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Name != "cm-resync" {
		t.Fatalf("relist after watch ERROR = %#v, want current cm-resync state", list.Items)
	}

	h.syncCorpus(t, "configmap/watch-resync", records)
}

// TestWatchBookmark captures Row 17: with bookmarks enabled, Kubernetes emits a
// BOOKMARK carrying a resourceVersion that can be used as a safe resume anchor.
// The probe uses a streaming-list watch to make the bookmark deterministic rather
// than waiting for an opportunistic progress notification.
func TestWatchBookmark(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "watch-bookmark")

	cm := &corev1.ConfigMap{ObjectMeta: s.meta("cm-bookmark"), Data: map[string]string{"key": "value"}}
	if _, err := h.kube.CoreV1().ConfigMaps(s.ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create configmap: %v", err)
	}
	h.quiesceAndClear(t, s.id, 3)

	records := h.probeWatch(t, watchProbeRequest{
		Scenario:      s.id,
		Mode:          "bookmark",
		Resource:      configmapsResource,
		Namespace:     s.ns,
		LabelSelector: scenarioLabel + "=" + s.id,
	})
	if len(records) != 1 {
		t.Fatalf("watch-bookmark records = %d, want exactly the BOOKMARK transport moment", len(records))
	}
	bookmark := records[0]
	if bookmark.Source != mutationlab.SourceWatch || bookmark.Summary.WatchType != "BOOKMARK" {
		t.Fatalf("watch-bookmark record = %+v, want watch BOOKMARK", bookmark)
	}
	if bookmark.Key.ResourceVersion == "" {
		t.Fatal("watch BOOKMARK carried no resourceVersion")
	}
	if ts := bookmark.ObservedAt; time.Since(ts) > time.Minute || ts.IsZero() {
		t.Fatalf("watch BOOKMARK observedAt = %v, want a fresh timestamp", ts)
	}

	h.syncCorpus(t, "configmap/watch-bookmark", records)
}

// TestWatchReplayCollapsesCreateThenModify captures the replay-watermark behavior
// behind the signing-overlap and commit-author-attribution flakes. The product's
// watch-first ingestion (internal/watch/target_watch.go) opens a SendInitialEvents
// watch and files everything observed *before* the initial-events-end BOOKMARK as
// an unattributed baseline resync; only events *after* the bookmark become
// attributable per-event commits.
//
// This probe uses that exact transport. A ConfigMap is created and then modified
// BEFORE the watch opens — and the replay delivers it as a SINGLE collapsed ADDED
// at the post-modify resourceVersion: no distinct CREATE, no MODIFIED, the create's
// resourceVersion invisible. So an object whose creation loses the race against the
// watermark cannot be observed as a per-event CREATE and carries no per-event
// attribution — it is indistinguishable from a long-existing object. Contrast Row 2
// (TestUpdate), where the same modify on an already-live watch is a distinct
// MODIFIED.
func TestWatchReplayCollapsesCreateThenModify(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "watch-replay-collapse")

	// Create then modify the SAME object, both before the replay watch opens.
	cm := &corev1.ConfigMap{ObjectMeta: s.meta("cm-collapse"), Data: map[string]string{"key": "value"}}
	created, err := h.kube.CoreV1().ConfigMaps(s.ns).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	created.Data["key"] = "modified"
	updated, err := h.kube.CoreV1().ConfigMaps(s.ns).Update(ctx, created, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.ResourceVersion == created.ResourceVersion {
		t.Fatalf("update did not advance resourceVersion (%s); the collapse proof needs two distinct writes",
			created.ResourceVersion)
	}
	// Drain the background recorder's own watch/audit/admission for the two writes so
	// they cannot bleed into a later scenario; the probe below returns its own records.
	h.quiesceAndClear(t, s.id, 5)

	records := h.probeWatch(t, watchProbeRequest{
		Scenario:      s.id,
		Mode:          "replay",
		Resource:      configmapsResource,
		Namespace:     s.ns,
		LabelSelector: scenarioLabel + "=" + s.id,
	})

	// The replay window is exactly the collapsed ADDED plus the initial-events-end
	// BOOKMARK that closes it — the create-then-modify is one event, not two.
	if got := countWatchType(records, "ADDED"); got != 1 {
		t.Errorf("replay produced %d ADDED events; want exactly 1 (create+modify collapse)", got)
	}
	if got := countWatchType(records, "MODIFIED"); got != 0 {
		t.Errorf("replay produced %d MODIFIED events; want 0 (the modify is folded into the ADDED)", got)
	}
	if got := countWatchType(records, "BOOKMARK"); got != 1 {
		t.Errorf("replay produced %d BOOKMARK events; want exactly 1 (initial-events-end)", got)
	}

	added := mustRecord(t, firstWatch(records, "ADDED"), "the collapsed replay ADDED for cm-collapse")
	// The collapsed ADDED carries the POST-modify resourceVersion: the create moment
	// (and its resourceVersion) is gone — which is why a late-joining watch cannot
	// attribute or per-event-commit the create.
	if added.Key.ResourceVersion != updated.ResourceVersion {
		t.Errorf("replay ADDED rv = %s; want the post-modify rv %s (the latest state, not the create)",
			added.Key.ResourceVersion, updated.ResourceVersion)
	}
	if added.Key.ResourceVersion == created.ResourceVersion {
		t.Errorf("replay ADDED rv = %s equals the create rv; the modify should have been folded in",
			added.Key.ResourceVersion)
	}

	bookmark := mustRecord(t, firstWatch(records, "BOOKMARK"), "the initial-events-end BOOKMARK")
	if bookmark.Key.ResourceVersion == "" {
		t.Error("initial-events-end BOOKMARK carried no resourceVersion; it is the replay watermark")
	}

	h.syncCorpus(t, "configmap/watch-replay-collapse", records)
}

func watchStatus(r mutationlab.Record) (int32, string) {
	var env struct {
		Object struct {
			Code   int32  `json:"code"`
			Reason string `json:"reason"`
		} `json:"object"`
	}
	_ = json.Unmarshal(r.Raw, &env)
	return env.Object.Code, env.Object.Reason
}
