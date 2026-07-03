// SPDX-License-Identifier: Apache-2.0

package labserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
	"github.com/ConfigButler/gitops-reverser/internal/mutationlab/recorder"
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

func TestAPI_WatchProbeStoresReturnedRecords(t *testing.T) {
	s := store.New()
	prober := fakeWatchProber{records: []mutationlab.Record{{
		Scenario: "watch-bookmark",
		Source:   mutationlab.SourceWatch,
		Summary:  mutationlab.RecordSummary{WatchType: "BOOKMARK"},
	}}}
	mux := NewAPI(s, prober).Routes()

	body := `{"scenario":"watch-bookmark","mode":"bookmark","resource":"v1/configmaps","namespace":"lab-watch"}`
	req := httptest.NewRequest(http.MethodPost, "/watch-probe", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if s.Len("watch-bookmark") != 1 {
		t.Fatalf("stored records = %d, want 1", s.Len("watch-bookmark"))
	}
	var got []mutationlab.Record
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].ID == "" {
		t.Fatalf("response = %+v, want stored record with ID", got)
	}
}

func TestAPI_WatchProbeUnavailable(t *testing.T) {
	mux := NewAPI(store.New()).Routes()
	body := `{"scenario":"watch-bookmark","mode":"bookmark","resource":"v1/configmaps"}`
	req := httptest.NewRequest(http.MethodPost, "/watch-probe", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
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

type fakeWatchProber struct {
	records []mutationlab.Record
	err     error
}

func (p fakeWatchProber) Probe(
	_ context.Context,
	_ recorder.WatchProbeRequest,
) ([]mutationlab.Record, error) {
	return p.records, p.err
}
