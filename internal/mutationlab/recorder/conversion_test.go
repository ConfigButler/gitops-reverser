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
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
	"github.com/ConfigButler/gitops-reverser/internal/mutationlab/store"
)

func TestConvertWidget_RenamesFieldBothDirections(t *testing.T) {
	// v1 spec.sizeBytes (integer) -> v2 spec.size (string).
	toV2 := convertWidget(json.RawMessage(
		`{"apiVersion":"mutationlab.configbutler.ai/v1","kind":"Widget","spec":{"sizeBytes":1024}}`),
		"mutationlab.configbutler.ai/v2")
	var v2 map[string]any
	if err := json.Unmarshal(toV2, &v2); err != nil {
		t.Fatalf("unmarshal v2: %v", err)
	}
	if v2["apiVersion"] != "mutationlab.configbutler.ai/v2" {
		t.Errorf("apiVersion = %v, want v2", v2["apiVersion"])
	}
	spec := v2["spec"].(map[string]any)
	if spec["size"] != "1024" {
		t.Errorf("v2 spec.size = %v, want \"1024\"", spec["size"])
	}
	if _, present := spec["sizeBytes"]; present {
		t.Error("v2 still carries spec.sizeBytes; want it removed")
	}

	// v2 spec.size (string) -> v1 spec.sizeBytes (integer).
	toV1 := convertWidget(json.RawMessage(
		`{"apiVersion":"mutationlab.configbutler.ai/v2","kind":"Widget","spec":{"size":"2048"}}`),
		"mutationlab.configbutler.ai/v1")
	var v1 map[string]any
	if err := json.Unmarshal(toV1, &v1); err != nil {
		t.Fatalf("unmarshal v1: %v", err)
	}
	spec1 := v1["spec"].(map[string]any)
	if n, ok := spec1["sizeBytes"].(float64); !ok || int(n) != 2048 {
		t.Errorf("v1 spec.sizeBytes = %v, want 2048", spec1["sizeBytes"])
	}
	if _, present := spec1["size"]; present {
		t.Error("v1 still carries spec.size; want it removed")
	}
}

func TestConversion_RecordsReviewAndConverts(t *testing.T) {
	s := store.New()
	h := NewConversion(s)
	body := `{"apiVersion":"apiextensions.k8s.io/v1","kind":"ConversionReview","request":{` +
		`"uid":"req-1","desiredAPIVersion":"mutationlab.configbutler.ai/v2","objects":[` +
		`{"apiVersion":"mutationlab.configbutler.ai/v1","kind":"Widget",` +
		`"metadata":{"name":"w1","labels":{"mutationlab.configbutler.ai/scenario":"crd-conversion"}},` +
		`"spec":{"sizeBytes":1024}}]}}`
	rec := postJSON(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	got := s.List("crd-conversion")
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	if got[0].Source != mutationlab.SourceConversion {
		t.Errorf("source = %q, want conversion", got[0].Source)
	}
	if got[0].Summary.Operation != "to-v2" {
		t.Errorf("operation = %q, want to-v2", got[0].Summary.Operation)
	}

	// The response must echo the uid and carry the converted (v2) object.
	var resp struct {
		Response struct {
			UID              string            `json:"uid"`
			ConvertedObjects []json.RawMessage `json:"convertedObjects"`
		} `json:"response"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Response.UID != "req-1" {
		t.Errorf("response uid = %q, want req-1", resp.Response.UID)
	}
	if len(resp.Response.ConvertedObjects) != 1 {
		t.Fatalf("got %d converted objects, want 1", len(resp.Response.ConvertedObjects))
	}
}
