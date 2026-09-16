// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func observedDeployment(resourceVersion string, generation int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name": "api", "namespace": "prod", "resourceVersion": resourceVersion,
			"generation": generation,
			"uid":        "2f1c8f1e-0000-4000-8000-000000000001",
		},
		"spec": map[string]any{"replicas": int64(2)},
	}}
}

func deploymentsGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
}

// The watch seam is the only place the unsanitized object exists — by the time the event reaches a
// branch worker the object is out of the process and the cluster has moved on. So the counters are
// stamped here or they are lost, exactly like the author.
func TestTargetWatchGitEvent_StampsObservedResourceVersion(t *testing.T) {
	event := targetWatchGitEvent(deploymentsGVR(), observedDeployment("20001", 3), "UPDATE")

	if event.ResourceVersion != "20001" {
		t.Errorf("event.ResourceVersion = %q, want the observed 20001", event.ResourceVersion)
	}
	if event.Generation != 3 {
		t.Errorf("event.Generation = %d, want the observed 3", event.Generation)
	}
	if event.Object == nil {
		t.Fatal("an UPDATE must carry the sanitized object")
	}
	if got := event.Object.GetResourceVersion(); got != "" {
		t.Errorf("the object the writer commits still carries resourceVersion %q", got)
	}
	if got := event.Object.GetGeneration(); got != 0 {
		t.Errorf("the object the writer commits still carries generation %d", got)
	}
}

// A DELETE carries no object — the resource is gone — but the Deleted frame still delivers the
// final one, so the last version that existed IS knowable. This is where the version parts company
// with Kind and Labels, which stay empty for a delete.
func TestTargetWatchGitEvent_DeleteCarriesTheFinalResourceVersion(t *testing.T) {
	event := targetWatchGitEvent(deploymentsGVR(), observedDeployment("20003", 4), "DELETE")

	if event.Object != nil {
		t.Error("a DELETE must carry no object")
	}
	if event.ResourceVersion != "20003" {
		t.Errorf("event.ResourceVersion = %q, want the final 20003", event.ResourceVersion)
	}
	if event.Generation != 4 {
		t.Errorf("event.Generation = %d, want the final 4", event.Generation)
	}
}

// A ConfigMap is the case that matters for Generation: the API server gives spec-less kinds none,
// and this operator mirrors them constantly. The field stays 0 rather than inventing a value, and
// templates guard it with {{with}}. The resourceVersion is still there, which is the whole reason
// the two counters are carried separately.
func TestTargetWatchGitEvent_SpeclessKindCarriesNoGeneration(t *testing.T) {
	configMap := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "cm", "namespace": "prod", "resourceVersion": "20005"},
		"data":       map[string]any{"key": "value"},
	}}
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

	event := targetWatchGitEvent(gvr, configMap, "CREATE")
	if event.Generation != 0 {
		t.Errorf("Generation = %d, want 0 for a kind that has none", event.Generation)
	}
	if event.ResourceVersion != "20005" {
		t.Errorf("ResourceVersion = %q, want it present even without a generation", event.ResourceVersion)
	}
}

// An object the API server gave neither counter leaves both at their zero value.
func TestTargetWatchGitEvent_NothingObservedStaysEmpty(t *testing.T) {
	bare := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "api", "namespace": "prod"},
	}}

	event := targetWatchGitEvent(deploymentsGVR(), bare, "CREATE")
	if event.ResourceVersion != "" || event.Generation != 0 {
		t.Errorf("counters = %q/%d, want both empty when nothing was observed",
			event.ResourceVersion, event.Generation)
	}
}
