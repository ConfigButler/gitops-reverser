//go:build mutationlab_e2e

// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
)

// TestSourcesLive proves that every source the corpus records reaches the lab, each by a probe
// write it then finds among the records: the audit webhook backend, the admission webhook, the
// CRD conversion webhook, the aggregated API server, and the watch. `task lab-bootstrap` runs it
// alone, so a capture starts only on a cluster where all five deliver; the capture itself skips
// it.
//
// It exists because a source can go quiet without anything failing: the swapped-in lab of old
// lost its admission registration, and audit kept flowing, so only a check that names each source
// tells a missing one apart from a scenario that has none.
func TestSourcesLive(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	t.Run("audit, admission, and watch", func(t *testing.T) {
		s := h.newScenario(ctx, t, "live-configmap")
		cm := &corev1.ConfigMap{ObjectMeta: s.meta("probe"), Data: map[string]string{"k": "v"}}
		if _, err := h.kube.CoreV1().ConfigMaps(s.ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create probe ConfigMap: %v", err)
		}
		h.awaitSources(t, s.id, "", []sourceCheck{
			{"audit", func(rs []mutationlab.Record) bool { return firstSource(rs, mutationlab.SourceAudit) != nil }},
			{"admission", func(rs []mutationlab.Record) bool {
				return firstSource(rs, mutationlab.SourceAdmission) != nil
			}},
			{"watch", func(rs []mutationlab.Record) bool { return firstWatch(rs, "ADDED") != nil }},
		})
	})

	t.Run("conversion", func(t *testing.T) {
		h.installWidgetCRD(ctx, t)
		s := h.newScenario(ctx, t, "live-conversion")
		h.ensureWidgetWatchLive(ctx, t, s)
		w := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": widgetGroup + "/v1",
			"kind":       "Widget",
			"metadata": map[string]any{
				"name": "probe-2", "namespace": s.ns, "labels": map[string]any{scenarioLabel: s.id},
			},
			"spec": map[string]any{"sizeBytes": int64(1)},
		}}
		if _, err := h.dyn.Resource(widgetV1).Namespace(s.ns).Create(ctx, w, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create probe Widget: %v", err)
		}
		h.awaitSources(t, s.id, "", []sourceCheck{
			{"conversion", func(rs []mutationlab.Record) bool {
				return firstSource(rs, mutationlab.SourceConversion) != nil
			}},
		})
	})

	t.Run("aggregated API", func(t *testing.T) {
		s := h.newScenario(ctx, t, "live-aggregated")
		fl := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "wardle.example.com/v1alpha1",
			"kind":       "Flunder",
			"metadata": map[string]any{
				"name": "probe", "namespace": s.ns, "labels": map[string]any{scenarioLabel: s.id},
			},
			"spec": map[string]any{"referenceType": "Flunder", "reference": "probe"},
		}}
		if _, err := h.dyn.Resource(flunderGVR).Namespace(s.ns).Create(ctx, fl, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create probe flunder: %v", err)
		}
		// The namespace key as well: an aggregated API's audit event has no body to carry the
		// scenario label, and its objectRef has no name for a create (row 15).
		h.awaitSources(t, s.id, s.ns, []sourceCheck{
			{"aggregated API watch", func(rs []mutationlab.Record) bool {
				return flunderRecordNamed(rs, mutationlab.SourceWatch, "ADDED", "probe") != nil
			}},
			{"aggregated API audit", func(rs []mutationlab.Record) bool {
				return flunderRecordNamed(rs, mutationlab.SourceAudit, "", "") != nil
			}},
		})
	})
}

type sourceCheck struct {
	name  string
	found func([]mutationlab.Record) bool
}

// awaitSources polls the records of one scenario, unioned with those of namespace when it is set,
// until every check finds its record, and on timeout names the sources that never arrived.
func (h *harness) awaitSources(t *testing.T, id, namespace string, checks []sourceCheck) {
	t.Helper()
	const timeout = 90 * time.Second
	deadline := time.Now().Add(timeout)
	var missing []string
	for time.Now().Before(deadline) {
		records := h.fetchScenario(t, id, namespace)
		missing = missing[:0]
		for _, c := range checks {
			if !c.found(records) {
				missing = append(missing, c.name)
			}
		}
		if len(missing) == 0 {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("no %s record reached the lab within %s", strings.Join(missing, ", "), timeout)
}
