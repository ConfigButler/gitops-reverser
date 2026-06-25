//go:build mutationlab_e2e

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
