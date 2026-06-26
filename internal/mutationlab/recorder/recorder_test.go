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

package recorder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
	"github.com/ConfigButler/gitops-reverser/internal/mutationlab/store"
)

func postJSON(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAudit_RecordsEventWithSummaryAndScenario(t *testing.T) {
	s := store.New()
	h := NewAudit(s, mutationlab.SourceAudit)
	body := `{"apiVersion":"audit.k8s.io/v1","kind":"EventList","items":[
		{"auditID":"abc","verb":"create","stage":"ResponseComplete",
		 "user":{"username":"kubernetes-admin"},
		 "objectRef":{"resource":"configmaps","namespace":"lab-create-1","name":"cm-a","apiVersion":"v1"},
		 "responseStatus":{"code":201},
		 "responseObject":{"kind":"ConfigMap"}}]}`
	rec := postJSON(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := s.List("lab-create-1")
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	r := got[0]
	if r.Source != mutationlab.SourceAudit || r.Summary.AuditID != "abc" || r.Summary.Operation != "create" {
		t.Errorf("summary wrong: %+v", r.Summary)
	}
	if r.Summary.User != "kubernetes-admin" || !r.Summary.HasResponseObject || r.Summary.ResponseCode != 201 {
		t.Errorf("summary detail wrong: %+v", r.Summary)
	}
	if r.Key.Name != "cm-a" || r.Key.Resource != "configmaps" {
		t.Errorf("key wrong: %+v", r.Key)
	}
}

func TestAudit_DecodesEventWithTimestamps(t *testing.T) {
	// Regression: audit events carry metav1.MicroTime timestamps and embedded
	// objects; a plain json.Unmarshal silently rejects them. The codec must not.
	s := store.New()
	h := NewAudit(s, mutationlab.SourceAudit)
	body := `{"items":[{"auditID":"ts-1","verb":"create","stage":"ResponseComplete",
		"requestReceivedTimestamp":"2026-06-24T10:00:00.000000Z",
		"stageTimestamp":"2026-06-24T10:00:00Z",
		"objectRef":{"resource":"configmaps","namespace":"ns-ts","name":"cm-a","apiVersion":"v1"},
		"responseObject":{"kind":"ConfigMap","metadata":{"uid":"u","resourceVersion":"100"}}}]}`
	if rec := postJSON(t, h, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := s.List("ns-ts")
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1 (event with timestamps was dropped)", len(got))
	}
	if !got[0].Summary.HasResponseObject || got[0].Summary.AuditID != "ts-1" {
		t.Errorf("summary wrong: %+v", got[0].Summary)
	}
}

func TestAudit_DeletecollectionScenarioFromObjectRefNamespace(t *testing.T) {
	s := store.New()
	h := NewAudit(s, mutationlab.SourceAudit)
	// Name-less deletecollection: scenario still recovered from objectRef namespace.
	body := `{"items":[{"auditID":"d1","verb":"deletecollection",
		"requestURI":"/api/v1/namespaces/lab-dc-2/configmaps?labelSelector=x%3Dy",
		"objectRef":{"resource":"configmaps","namespace":"lab-dc-2","apiVersion":"v1"}}]}`
	postJSON(t, h, body)
	if got := s.List("lab-dc-2"); len(got) != 1 || got[0].Key.Name != "" {
		t.Fatalf("deletecollection attribution wrong: %+v", got)
	}
}

func TestAudit_AdditionalSourceLabeled(t *testing.T) {
	s := store.New()
	h := NewAudit(s, mutationlab.SourceAuditAdditional)
	postJSON(t, h, `{"items":[{"auditID":"e1","verb":"patch","objectRef":{"namespace":"ns","resource":"configmaps"}}]}`)
	if got := s.List("ns"); len(got) != 1 || got[0].Source != mutationlab.SourceAuditAdditional {
		t.Fatalf("additional source wrong: %+v", got)
	}
}

func TestAudit_BadBody(t *testing.T) {
	s := store.New()
	h := NewAudit(s, mutationlab.SourceAudit)
	if rec := postJSON(t, h, `{not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func admissionReviewBody(uid, op, ns, name string, labels string) string {
	obj := `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"` + name + `","namespace":"` + ns + `"` + labels + `}}`
	return `{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview","request":{` +
		`"uid":"` + uid + `","operation":"` + op + `","namespace":"` + ns + `","name":"` + name + `",` +
		`"resource":{"group":"","version":"v1","resource":"configmaps"},` +
		`"userInfo":{"username":"kubernetes-admin"},"object":` + obj + `}}`
}

func TestAdmission_AllowsByDefaultAndRecords(t *testing.T) {
	s := store.New()
	h := NewAdmission(s, RejectByLabel)
	rec := postJSON(t, h, admissionReviewBody("u1", "CREATE", "lab-x", "cm-a", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp admissionv1.AdmissionReview
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Response == nil || !resp.Response.Allowed || string(resp.Response.UID) != "u1" {
		t.Fatalf("response wrong: %+v", resp.Response)
	}
	if resp.Response.AuditAnnotations[scenarioAuditAnnotation] != "lab-x" {
		t.Errorf("missing scenario audit annotation: %+v", resp.Response.AuditAnnotations)
	}
	got := s.List("lab-x")
	if len(got) != 1 || got[0].Summary.AdmissionUID != "u1" || got[0].Summary.Operation != "CREATE" {
		t.Fatalf("record wrong: %+v", got)
	}
	// The recorded payload is the request shape, not the whole review.
	if !strings.Contains(string(got[0].Raw), `"request"`) {
		t.Errorf("raw should wrap request: %s", got[0].Raw)
	}
}

func TestAdmission_RecordAndReject(t *testing.T) {
	s := store.New()
	h := NewAdmission(s, RejectByLabel)
	labels := `,"labels":{"` + RejectLabel + `":"true"}`
	rec := postJSON(t, h, admissionReviewBody("u2", "CREATE", "lab-r", "cm-r", labels))
	var resp admissionv1.AdmissionReview
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Response.Allowed {
		t.Fatal("expected rejection")
	}
	if resp.Response.Result == nil || resp.Response.Result.Code != http.StatusForbidden {
		t.Fatalf("expected 403 status, got %+v", resp.Response.Result)
	}
	got := s.List("lab-r")
	if len(got) != 1 || got[0].Summary.ResponseCode != http.StatusForbidden {
		t.Fatalf("rejected record wrong: %+v", got)
	}
}

func TestAdmission_DeleteCollectionNameFromOldObject(t *testing.T) {
	// A per-object admission DELETE inside a deletecollection has no request.name;
	// the key name must come from oldObject so corpus filenames stay stable.
	s := store.New()
	h := NewAdmission(s, RejectByLabel)
	body := `{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview","request":{` +
		`"uid":"d1","operation":"DELETE","namespace":"lab-dc",` +
		`"resource":{"group":"","version":"v1","resource":"configmaps"},` +
		`"userInfo":{"username":"system:admin"},` +
		`"oldObject":{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm-b","namespace":"lab-dc"}}}}`
	postJSON(t, h, body)
	got := s.List("lab-dc")
	if len(got) != 1 || got[0].Key.Name != "cm-b" {
		t.Fatalf("expected key name cm-b from oldObject, got %+v", got)
	}
}

func TestAdmission_BadBody(t *testing.T) {
	s := store.New()
	h := NewAdmission(s, nil)
	if rec := postJSON(t, h, `{"kind":"AdmissionReview"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for missing request", rec.Code)
	}
}

func TestBuildWatchRecord_ObjectEvent(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":            "cm-a",
			"namespace":       "lab-w",
			"uid":             "uid-1",
			"resourceVersion": "42",
			"labels":          map[string]any{ScenarioLabel: "scenario-w"},
		},
	}}
	r := buildWatchRecord(watch.Added, u)
	if r.Summary.WatchType != "ADDED" || !r.Summary.HasObject {
		t.Errorf("summary wrong: %+v", r.Summary)
	}
	if r.Scenario != "scenario-w" {
		t.Errorf("scenario = %q, want scenario-w (label wins)", r.Scenario)
	}
	if r.Key.Name != "cm-a" || r.Key.UID != "uid-1" || r.Key.ResourceVersion != "42" {
		t.Errorf("key wrong: %+v", r.Key)
	}
	if !strings.Contains(string(r.Raw), `"type":"ADDED"`) || !strings.Contains(string(r.Raw), `"cm-a"`) {
		t.Errorf("raw wrong: %s", r.Raw)
	}
}

func TestBuildWatchRecord_BookmarkHasNoKey(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"resourceVersion": "99"},
	}}
	r := buildWatchRecord(watch.Bookmark, u)
	if r.Summary.WatchType != "BOOKMARK" || r.Key.ResourceVersion != "99" || r.Key.Name != "" {
		t.Errorf("bookmark record wrong: %+v / %+v", r.Summary, r.Key)
	}
}

func TestWatchProbe_BookmarkRecordsOnlyBookmark(t *testing.T) {
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	client.PrependWatchReactor("configmaps", func(_ ktesting.Action) (bool, watch.Interface, error) {
		w := watch.NewFakeWithChanSize(2, false)
		w.Action(watch.Added, watchObject("cm-a", "10"))
		w.Action(watch.Bookmark, watchObject("", "11"))
		return true, w, nil
	})

	records, err := NewWatchProbe(client).Probe(context.Background(), WatchProbeRequest{
		Scenario:      "watch-bookmark",
		Mode:          WatchProbeBookmark,
		GVR:           gvr,
		Namespace:     "lab-watch",
		LabelSelector: ScenarioLabel + "=watch-bookmark",
	})
	if err != nil {
		t.Fatalf("probe bookmark: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want exactly the bookmark", len(records))
	}
	got := records[0]
	if got.Scenario != "watch-bookmark" || got.Summary.WatchType != "BOOKMARK" {
		t.Fatalf("record = %+v, want scenario-tagged BOOKMARK", got)
	}
	if got.Key.ResourceVersion != "11" || got.Key.Resource != "configmaps" {
		t.Fatalf("key = %+v, want bookmark rv and resource", got.Key)
	}
}

// TestWatchProbe_ReplayKeepsCollapsedAdded pins the replay mode's defining trait:
// unlike the bookmark probe (which returns only the boundary), the replay probe
// keeps every initial ADDED that precedes the initial-events-end BOOKMARK. A
// create-then-modify performed before the watch opens surfaces in the
// SendInitialEvents replay as ONE synthetic ADDED at the post-modify rv — the
// collapsed observation the product files as an unattributed baseline.
func TestWatchProbe_ReplayKeepsCollapsedAdded(t *testing.T) {
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	client.PrependWatchReactor("configmaps", func(_ ktesting.Action) (bool, watch.Interface, error) {
		w := watch.NewFakeWithChanSize(2, false)
		// rv 12 is the post-modify resourceVersion; the create's rv is already gone.
		w.Action(watch.Added, watchObject("cm-collapse", "12"))
		w.Action(watch.Bookmark, watchObject("", "12"))
		return true, w, nil
	})

	records, err := NewWatchProbe(client).Probe(context.Background(), WatchProbeRequest{
		Scenario:      "watch-replay-collapse",
		Mode:          WatchProbeReplay,
		GVR:           gvr,
		Namespace:     "lab-watch",
		LabelSelector: ScenarioLabel + "=watch-replay-collapse",
	})
	if err != nil {
		t.Fatalf("probe replay: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want the collapsed ADDED + the boundary BOOKMARK", len(records))
	}
	if got := records[0]; got.Summary.WatchType != "ADDED" || got.Key.Name != "cm-collapse" ||
		got.Key.ResourceVersion != "12" || got.Scenario != "watch-replay-collapse" {
		t.Fatalf("record[0] = %+v, want collapsed ADDED cm-collapse@12 (no MODIFIED)", got)
	}
	if got := records[1]; got.Summary.WatchType != "BOOKMARK" || got.Key.ResourceVersion != "12" {
		t.Fatalf("record[1] = %+v, want initial-events-end BOOKMARK@12", got)
	}
}

func TestWatchProbe_ExpiredWatchErrorBecomesRecord(t *testing.T) {
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	client.PrependWatchReactor("configmaps", func(_ ktesting.Action) (bool, watch.Interface, error) {
		return true, nil, &apierrors.StatusError{ErrStatus: metav1.Status{
			Status:  metav1.StatusFailure,
			Code:    http.StatusGone,
			Reason:  metav1.StatusReasonExpired,
			Message: "too old resource version",
		}}
	})

	records, err := NewWatchProbe(client).Probe(context.Background(), WatchProbeRequest{
		Scenario:  "watch-resync",
		Mode:      WatchProbeExpired,
		GVR:       gvr,
		Namespace: "lab-watch",
	})
	if err != nil {
		t.Fatalf("probe expired: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want one ERROR", len(records))
	}
	got := records[0]
	if got.Scenario != "watch-resync" || got.Summary.WatchType != "ERROR" {
		t.Fatalf("record = %+v, want scenario-tagged ERROR", got)
	}
	if !strings.Contains(string(got.Raw), `"code":410`) || !strings.Contains(string(got.Raw), `"reason":"Expired"`) {
		t.Fatalf("raw = %s, want 410 Expired status", got.Raw)
	}
}

func watchObject(name, rv string) *unstructured.Unstructured {
	metadata := map[string]any{"resourceVersion": rv}
	if name != "" {
		metadata["name"] = name
		metadata["namespace"] = "lab-watch"
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   metadata,
	}}
}

func TestScenarioFromRequestURI(t *testing.T) {
	cases := map[string]string{
		"/api/v1/namespaces/lab-a/configmaps?labelSelector=" + ScenarioLabel + "%3Dsel-scn": "sel-scn",
		"/api/v1/namespaces/lab-b/configmaps":                                               "lab-b",
		"::nonsense":                                                                        "",
	}
	for uri, want := range cases {
		if got := scenarioFromRequestURI(uri); got != want {
			t.Errorf("scenarioFromRequestURI(%q) = %q, want %q", uri, got, want)
		}
	}
}
