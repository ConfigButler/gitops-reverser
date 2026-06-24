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

package labserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
	"github.com/ConfigButler/gitops-reverser/internal/mutationlab/store"
)

func TestAPI_GetRecordsFilteredByScenario(t *testing.T) {
	s := store.New()
	s.Add(mutationlab.Record{Scenario: "a", Source: mutationlab.SourceWatch})
	s.Add(mutationlab.Record{Scenario: "b", Source: mutationlab.SourceAudit})
	mux := NewAPI(s).Routes()

	req := httptest.NewRequest(http.MethodGet, "/records?scenario=a", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got []mutationlab.Record
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].Scenario != "a" {
		t.Fatalf("got %+v, want one scenario-a record", got)
	}
}

func TestAPI_DeleteClears(t *testing.T) {
	s := store.New()
	s.Add(mutationlab.Record{Scenario: "a"})
	mux := NewAPI(s).Routes()

	req := httptest.NewRequest(http.MethodDelete, "/records", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if s.Len("") != 0 {
		t.Fatalf("store not cleared: %d", s.Len(""))
	}
}

func TestAPI_MethodNotAllowed(t *testing.T) {
	mux := NewAPI(store.New()).Routes()
	req := httptest.NewRequest(http.MethodPost, "/records", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestAPI_Healthz(t *testing.T) {
	mux := NewAPI(store.New()).Routes()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("healthz = %d %q", rec.Code, rec.Body.String())
	}
}
