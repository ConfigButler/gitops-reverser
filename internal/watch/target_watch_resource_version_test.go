// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func observedDeployment(resourceVersion string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name": "api", "namespace": "prod", "resourceVersion": resourceVersion,
			"uid": "2f1c8f1e-0000-4000-8000-000000000001",
		},
		"spec": map[string]any{"replicas": int64(2)},
	}}
}

func deploymentsGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
}

// The watch seam is the only place the unsanitized object exists — by the time the event reaches a
// branch worker the object is out of the process and the cluster has moved on. So the version is
// stamped here or it is lost, exactly like the author.
func TestTargetWatchGitEvent_StampsObservedResourceVersion(t *testing.T) {
	event := targetWatchGitEvent(deploymentsGVR(), observedDeployment("20001"), "UPDATE")

	if event.ResourceVersion != "20001" {
		t.Errorf("event.ResourceVersion = %q, want the observed 20001", event.ResourceVersion)
	}
	if event.Object == nil {
		t.Fatal("an UPDATE must carry the sanitized object")
	}
	if got := event.Object.GetResourceVersion(); got != "" {
		t.Errorf("the object the writer commits still carries resourceVersion %q", got)
	}
}

// A DELETE carries no object — the resource is gone — but the Deleted frame still delivers the
// final one, so the last version that existed IS knowable. This is where the version parts company
// with Kind and Labels, which stay empty for a delete.
func TestTargetWatchGitEvent_DeleteCarriesTheFinalResourceVersion(t *testing.T) {
	event := targetWatchGitEvent(deploymentsGVR(), observedDeployment("20003"), "DELETE")

	if event.Object != nil {
		t.Error("a DELETE must carry no object")
	}
	if event.ResourceVersion != "20003" {
		t.Errorf("event.ResourceVersion = %q, want the final 20003", event.ResourceVersion)
	}
}

// An object the API server gave no version (a synthesized or hand-built one) leaves the field
// empty rather than inventing a value; templates guard it with {{with}}.
func TestTargetWatchGitEvent_NoVersionObservedStaysEmpty(t *testing.T) {
	bare := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "api", "namespace": "prod"},
	}}

	if got := targetWatchGitEvent(deploymentsGVR(), bare, "CREATE").ResourceVersion; got != "" {
		t.Errorf("ResourceVersion = %q, want empty when nothing was observed", got)
	}
}
