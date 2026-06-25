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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
)

// M3 captures Row 14: a multi-version CRD whose two served versions differ by a
// webhook-converted field, so one write produces three shapes — the object the
// user submitted, the object the apiserver stored/serves, and the conversion
// calls that translate between them.
//
// The lab Widget CRD has v1 (spec.sizeBytes: integer) and v2 (spec.size: string,
// the storage version). The lab's /convert handler renames the field. A CRD has
// no controller, so — unlike M2's Deployment/Pod — the capture is naturally
// deterministic.

const (
	widgetGroup = "mutationlab.configbutler.ai"
	widgetCRD   = "widgets." + widgetGroup
)

var (
	crdGVR   = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	vwcGVR   = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingwebhookconfigurations"}
	widgetV1 = schema.GroupVersionResource{Group: widgetGroup, Version: "v1", Resource: "widgets"}
	widgetV2 = schema.GroupVersionResource{Group: widgetGroup, Version: "v2", Resource: "widgets"}
	vwcName  = "gitops-reverser-validating-webhook"
)

// TestCRDConversion captures Row 14. It installs the two-version Widget CRD whose
// conversion webhook reuses the lab's admission cert/port at /convert, creates a
// Widget in v1, and captures the divergence: admission + audit see the submitted
// v1 (spec.sizeBytes), the watch (opened on the v2 storage version) sees the
// stored v2 (spec.size), and the conversion webhook is called in both directions.
func TestCRDConversion(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.installWidgetCRD(ctx, t)
	s := h.newScenario(ctx, t, "crd-conversion")
	h.ensureWidgetWatchLive(ctx, t, s)

	w := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": widgetGroup + "/v1",
		"kind":       "Widget",
		"metadata": map[string]any{
			"name":      "w1",
			"namespace": s.ns,
			"labels":    map[string]any{scenarioLabel: s.id},
		},
		"spec": map[string]any{"sizeBytes": int64(1024)},
	}}
	if _, err := h.dyn.Resource(widgetV1).Namespace(s.ns).Create(ctx, w, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create widget v1: %v", err)
	}

	// admission (v1) + audit create + watch ADDED (v2) + conversion both ways.
	records := h.drain(t, s.id, drainSpec{
		minCount: 4, settle: 4 * time.Second, timeout: 90 * time.Second,
		until: func(rs []mutationlab.Record) bool {
			return conversionToward(rs, "to-v2") != nil && conversionToward(rs, "to-v1") != nil &&
				firstWatch(rs, "ADDED") != nil && firstSource(rs, mutationlab.SourceAdmission) != nil
		},
	})

	admission := firstSource(records, mutationlab.SourceAdmission)
	audit := firstSource(records, mutationlab.SourceAudit)
	added := firstWatch(records, "ADDED")
	toV2 := conversionToward(records, "to-v2")
	toV1 := conversionToward(records, "to-v1")
	for name, r := range map[string]*mutationlab.Record{
		"admission(v1)": admission, "audit": audit, "watch ADDED(v2)": added,
		"conversion to-v2": toV2, "conversion to-v1": toV1,
	} {
		if r == nil {
			t.Fatalf("missing %s record (Row 14 needs all three shapes + both conversions)", name)
		}
	}
	if added.Key.Version != "v2" {
		t.Errorf("watch ADDED version = %q, want v2 (the storage/served version watched)", added.Key.Version)
	}

	// The conversion calls are deterministic in content but not in count (the
	// apiserver may round-trip more than once), so commit one representative call
	// per direction alongside the submitted/stored shapes.
	h.syncCorpus(t, "widget/crd-conversion",
		[]mutationlab.Record{*admission, *audit, *added, *toV2, *toV1})
}

// installWidgetCRD creates the two-version Widget CRD, pointing its conversion
// webhook at the lab by reusing the service and CA bundle the product's
// validating webhook already uses (so M3 needs no new certificate). It waits for
// the CRD to be Established and registers teardown.
func (h *harness) installWidgetCRD(ctx context.Context, t *testing.T) {
	t.Helper()
	svc, ca := h.admissionServiceAndCA(ctx, t)
	crd := widgetCRDObject(svc, ca)
	if _, err := h.dyn.Resource(crdGVR).Create(ctx, crd, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create Widget CRD: %v", err)
	}
	t.Cleanup(func() {
		_ = h.dyn.Resource(crdGVR).Delete(context.Background(), widgetCRD, metav1.DeleteOptions{})
	})
	h.waitForCRDEstablished(ctx, t)
}

// admissionServiceAndCA reads the product validating webhook config and returns
// its clientConfig.service (name/namespace/port) and caBundle, so the Widget CRD
// conversion webhook can address the same lab endpoint over the same TLS trust.
func (h *harness) admissionServiceAndCA(ctx context.Context, t *testing.T) (map[string]any, string) {
	t.Helper()
	vwc, err := h.dyn.Resource(vwcGVR).Get(ctx, vwcName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get validating webhook config %s: %v", vwcName, err)
	}
	webhooks, _, _ := unstructured.NestedSlice(vwc.Object, "webhooks")
	if len(webhooks) == 0 {
		t.Fatalf("validating webhook config %s has no webhooks", vwcName)
	}
	cc, _ := webhooks[0].(map[string]any)["clientConfig"].(map[string]any)
	svc, _ := cc["service"].(map[string]any)
	ca, _ := cc["caBundle"].(string)
	if svc == nil || ca == "" {
		t.Fatalf("validating webhook config %s missing service/caBundle", vwcName)
	}
	return svc, ca
}

// widgetCRDObject builds the two-version Widget CRD. v1 carries spec.sizeBytes
// (integer); v2 (storage) carries spec.size (string); the conversion webhook
// renames between them at the given service/path with the given CA bundle.
func widgetCRDObject(svc map[string]any, ca string) *unstructured.Unstructured {
	intSpec := map[string]any{"type": "object", "properties": map[string]any{
		"spec": map[string]any{"type": "object", "properties": map[string]any{
			"sizeBytes": map[string]any{"type": "integer"}}}}}
	strSpec := map[string]any{"type": "object", "properties": map[string]any{
		"spec": map[string]any{"type": "object", "properties": map[string]any{
			"size": map[string]any{"type": "string"}}}}}
	convService := map[string]any{
		"name":      svc["name"],
		"namespace": svc["namespace"],
		"path":      "/convert",
		"port":      svc["port"],
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": widgetCRD},
		"spec": map[string]any{
			"group": widgetGroup,
			"scope": "Namespaced",
			"names": map[string]any{
				"plural": "widgets", "singular": "widget", "kind": "Widget", "listKind": "WidgetList",
			},
			"versions": []any{
				map[string]any{"name": "v1", "served": true, "storage": false,
					"schema": map[string]any{"openAPIV3Schema": intSpec}},
				map[string]any{"name": "v2", "served": true, "storage": true,
					"schema": map[string]any{"openAPIV3Schema": strSpec}},
			},
			"conversion": map[string]any{
				"strategy": "Webhook",
				"webhook": map[string]any{
					"conversionReviewVersions": []any{"v1"},
					"clientConfig":             map[string]any{"service": convService, "caBundle": ca},
				},
			},
		},
	}}
}

func (h *harness) waitForCRDEstablished(ctx context.Context, t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		crd, err := h.dyn.Resource(crdGVR).Get(ctx, widgetCRD, metav1.GetOptions{})
		if err == nil && crdConditionTrue(crd, "Established") {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatal("Widget CRD never became Established")
}

func crdConditionTrue(crd *unstructured.Unstructured, condType string) bool {
	conds, _, _ := unstructured.NestedSlice(crd.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if ok && m["type"] == condType && m["status"] == "True" {
			return true
		}
	}
	return false
}

// ensureWidgetWatchLive creates and deletes a probe Widget until the lab's watch
// on the v2 storage version reports it, then clears records — so the scenario
// that follows captures a watch that is provably live. The lab's watch on a
// not-yet-existing CRD retries until the type appears, so this closes the gap
// between CRD creation and the watch establishing.
func (h *harness) ensureWidgetWatchLive(ctx context.Context, t *testing.T, s scenario) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		probe := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": widgetGroup + "/v1", "kind": "Widget",
			"metadata": map[string]any{
				"name": "probe", "namespace": s.ns, "labels": map[string]any{scenarioLabel: s.id},
			},
			"spec": map[string]any{"sizeBytes": int64(1)},
		}}
		_, _ = h.dyn.Resource(widgetV1).Namespace(s.ns).Create(ctx, probe, metav1.CreateOptions{})
		time.Sleep(time.Second)
		if firstWatch(h.mustFetch(t, s.id), "ADDED") != nil {
			_ = h.dyn.Resource(widgetV1).Namespace(s.ns).Delete(ctx, "probe", metav1.DeleteOptions{})
			time.Sleep(time.Second)
			h.clearRecords(t)
			return
		}
	}
	t.Fatal("Widget watch never went live after CRD creation")
}

// conversionToward returns the first conversion record whose target direction
// (Summary.Operation, e.g. "to-v2") matches.
func conversionToward(records []mutationlab.Record, op string) *mutationlab.Record {
	for i := range records {
		if r := &records[i]; r.Source == mutationlab.SourceConversion && r.Summary.Operation == op {
			return r
		}
	}
	return nil
}
