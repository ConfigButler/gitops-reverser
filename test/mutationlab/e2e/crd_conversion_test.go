//go:build mutationlab_e2e

// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/base64"
	"os"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
)

// CRD conversion (Row 14): a multi-version CRD whose two served versions differ
// by a webhook-converted field, so one write produces three shapes — the object
// the user submitted, the object the apiserver stored/serves, and the conversion
// calls that translate between them.
//
// The lab Widget CRD has v1 (spec.sizeBytes: integer) and v2 (spec.size: string,
// the storage version). The lab's /convert handler renames the field. A CRD has
// no controller, so — unlike the workload Deployment/Pod — the capture is
// naturally deterministic.

const (
	widgetGroup = "mutationlab.configbutler.ai"
	widgetCRD   = "widgets." + widgetGroup
)

var (
	crdGVR = schema.GroupVersionResource{
		Group:    "apiextensions.k8s.io",
		Version:  "v1",
		Resource: "customresourcedefinitions",
	}
	widgetV1 = schema.GroupVersionResource{Group: widgetGroup, Version: "v1", Resource: "widgets"}
	widgetV2 = schema.GroupVersionResource{Group: widgetGroup, Version: "v2", Resource: "widgets"}
)

// The lab's Service, which serves /convert beside /validate-all under the same certificate
// (test/mutationlab/deploy/service.yaml).
const (
	labService   = "mutation-capture-lab"
	labNamespace = "mutation-capture-lab"
	labPort      = int64(443)
)

// TestCRDConversion captures Row 14. It installs the two-version Widget CRD whose
// conversion webhook calls the lab's /convert, creates a
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

	// All 5 committed records: admission (v1) + audit create + watch ADDED (v2) +
	// conversion both ways. Require each in the gate — audit delivery is async, so
	// a count/settle gate alone could return before audit.create lands.
	records := h.drain(t, s.id, drainSpec{
		minCount: 5, settle: 4 * time.Second, timeout: 90 * time.Second,
		until: func(rs []mutationlab.Record) bool {
			return conversionToward(rs, "to-v2") != nil && conversionToward(rs, "to-v1") != nil &&
				firstWatch(rs, "ADDED") != nil && firstSource(rs, mutationlab.SourceAdmission) != nil &&
				firstSource(rs, mutationlab.SourceAudit) != nil
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
// webhook at the lab's Service and trusting the lab CA from the stamp directory
// (LAB_CA_FILE). It waits for the CRD to be Established and registers a teardown
// that waits for the CRD to be gone, so the next install does not meet it still
// terminating.
func (h *harness) installWidgetCRD(ctx context.Context, t *testing.T) {
	t.Helper()
	caFile := os.Getenv("LAB_CA_FILE")
	if caFile == "" {
		t.Fatal("LAB_CA_FILE not set; run via `task lab-capture`")
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatalf("read the lab CA: %v", err)
	}
	crd := widgetCRDObject(base64.StdEncoding.EncodeToString(ca))
	if _, err := h.dyn.Resource(crdGVR).Create(ctx, crd, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create Widget CRD: %v", err)
	}
	t.Cleanup(func() { h.deleteWidgetCRD(t) })
	h.waitForCRDEstablished(ctx, t)
}

func (h *harness) deleteWidgetCRD(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	_ = h.dyn.Resource(crdGVR).Delete(ctx, widgetCRD, metav1.DeleteOptions{})
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := h.dyn.Resource(crdGVR).Get(ctx, widgetCRD, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(time.Second)
	}
	t.Errorf("Widget CRD still present 60s after its deletion")
}

// widgetCRDObject builds the two-version Widget CRD. v1 carries spec.sizeBytes
// (integer); v2 (storage) carries spec.size (string); the conversion webhook
// renames between them at the lab's /convert, trusting the given CA bundle.
func widgetCRDObject(ca string) *unstructured.Unstructured {
	intSpec := map[string]any{"type": "object", "properties": map[string]any{
		"spec": map[string]any{"type": "object", "properties": map[string]any{
			"sizeBytes": map[string]any{"type": "integer"}}}}}
	strSpec := map[string]any{"type": "object", "properties": map[string]any{
		"spec": map[string]any{"type": "object", "properties": map[string]any{
			"size": map[string]any{"type": "string"}}}}}
	convService := map[string]any{
		"name":      labService,
		"namespace": labNamespace,
		"path":      "/convert",
		"port":      labPort,
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
