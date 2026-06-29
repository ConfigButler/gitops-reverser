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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
)

// Aggregated API write (Row 15) — the body-quality cliff, and the product
// question the lab exists to settle: when the official kube-apiserver audit body for an
// aggregated-API write is shallow/empty (the apiserver proxies the request and
// has no schema to render request/response objects), does the *watch* still carry
// the full object? It does — and that finding is what retired the aggregated-API
// body-enrichment proxy: a watch-based capture carries the object content natively,
// so there is no separate enriched audit body to join.
//
// The vehicle is the wardle sample aggregated API (flunders), which the e2e
// cluster runs as a directly-served aggregated API. So one flunder create yields
// the two views the corpus puts side by side: the official audit (/audit-webhook,
// empty body) and the live watch (full object). Both are load-bearing for Row 15 —
// the empty-audit-vs-full-watch contrast is the point — so the driver waits for
// and requires each.

var flunderGVR = schema.GroupVersionResource{
	Group: "wardle.example.com", Version: "v1alpha1", Resource: "flunders",
}

// TestAggregatedAPIWrite captures Row 15. It creates a flunder and proves the
// watch carries the full object (spec included), then commits the official audit
// (empty body) and the watch side by side so the body-quality difference is
// visible in the corpus. Both are required — the corpus row's whole point is the
// empty-audit-vs-full-watch contrast — so a missing event fails the scenario
// rather than silently dropping a corpus file.
func TestAggregatedAPIWrite(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.newScenario(ctx, t, "aggregated-api-write")

	flunder := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "wardle.example.com/v1alpha1",
		"kind":       "Flunder",
		"metadata": map[string]any{
			"name":      "fl-1",
			"namespace": s.ns,
			"labels":    map[string]any{scenarioLabel: s.id},
		},
		"spec": map[string]any{"referenceType": "Flunder", "reference": "some-flunder"},
	}}
	if _, err := h.dyn.Resource(flunderGVR).Namespace(s.ns).Create(ctx, flunder, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create flunder: %v", err)
	}

	// Wait for the flunder's watch ADDED and its official audit — both are committed
	// side by side. Union the namespace key because a shallow-bodied aggregated-API
	// audit event carries no object label to attribute by — but then select strictly
	// by flunder identity, because that union also surfaces the namespace's unlabeled
	// auto-objects (kube-root-ca.crt, the default ServiceAccount), which must NOT be
	// mistaken for the flunder's records.
	records := h.drain(t, s.id, drainSpec{
		minCount: 2, settle: 5 * time.Second, timeout: 90 * time.Second, alsoNamespace: s.ns,
		until: func(rs []mutationlab.Record) bool {
			return flunderRecord(rs, mutationlab.SourceWatch, "ADDED") != nil &&
				flunderRecord(rs, mutationlab.SourceAudit, "") != nil
		},
	})

	added := flunderRecord(records, mutationlab.SourceWatch, "ADDED")
	official := flunderRecord(records, mutationlab.SourceAudit, "")
	admission := flunderRecord(records, mutationlab.SourceAdmission, "")
	if added == nil {
		t.Fatal("no watch ADDED for the flunder; the aggregated-API watch did not carry it")
	}
	if official == nil {
		t.Fatal("no official audit event for the flunder create")
	}

	// THE RESULT: the watch event carries the full object (spec included). This is
	// the point of Row 15 — whatever the official audit body quality, the live
	// watch carries the object content.
	if !added.Summary.HasObject || flunderReference(added) != "some-flunder" {
		t.Errorf("watch ADDED did not carry the full flunder object (spec.reference=%q, hasObject=%v)",
			flunderReference(added), added.Summary.HasObject)
	}
	t.Logf("Row 15 (flunder only): official audit hasRequestObject=%v hasResponseObject=%v; "+
		"watch carries full object=%v; flunder admission records=%v",
		official.Summary.HasRequestObject, official.Summary.HasResponseObject,
		added.Summary.HasObject, admission != nil)

	h.syncCorpus(t, "flunder/aggregated-api-write",
		[]mutationlab.Record{*official, *added})
}

// flunderRecord returns the first record from the given source that is about the
// flunder (by objectRef resource or object name), optionally restricted to a
// watch type. This isolates the flunder from the namespace's auto-created objects
// that the namespace-union read also surfaces.
func flunderRecord(records []mutationlab.Record, src mutationlab.Source, watchType string) *mutationlab.Record {
	for i := range records {
		r := &records[i]
		if r.Source != src {
			continue
		}
		if watchType != "" && r.Summary.WatchType != watchType {
			continue
		}
		if r.Key.Resource == "flunders" || r.Key.Name == "fl-1" {
			return r
		}
	}
	return nil
}

// flunderReference extracts spec.reference from a watch record's object, the
// field that proves the watch carried the full object rather than a shell.
func flunderReference(r *mutationlab.Record) string {
	var env struct {
		Object struct {
			Spec struct {
				Reference string `json:"reference"`
			} `json:"spec"`
		} `json:"object"`
	}
	if err := json.Unmarshal(r.Raw, &env); err != nil {
		return ""
	}
	return env.Object.Spec.Reference
}
