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

// Package labserver assembles the lab's HTTP surface: the read/clear records API
// and the health endpoint. Reads are filtered by scenario rather than assuming
// the store holds only the current scenario's events, because asynchronous,
// batched audit delivery can land a late event from one scenario after the next
// has started.
package labserver

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
	"github.com/ConfigButler/gitops-reverser/internal/mutationlab/recorder"
	"github.com/ConfigButler/gitops-reverser/internal/mutationlab/store"
)

const watchProbeTimeout = 30 * time.Second

// WatchProber captures targeted watch transport events for lab rows whose
// payloads cannot be attributed by object labels alone.
type WatchProber interface {
	Probe(context.Context, recorder.WatchProbeRequest) ([]mutationlab.Record, error)
}

// API serves the lab's records and health endpoints over plain HTTP.
type API struct {
	store  *store.Store
	prober WatchProber
}

// NewAPI returns an API backed by the given store.
func NewAPI(s *store.Store, prober ...WatchProber) *API {
	api := &API{store: s}
	if len(prober) > 0 {
		api.prober = prober[0]
	}
	return api
}

// Routes registers the records and health handlers on a fresh mux.
func (a *API) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/records", a.handleRecords)
	mux.HandleFunc("/watch-probe", a.handleWatchProbe)
	mux.HandleFunc("/healthz", a.handleHealthz)
	// /readyz mirrors /healthz so a lab deployment can satisfy the product
	// deployment's readiness probe unchanged when it swaps in the lab image.
	mux.HandleFunc("/readyz", a.handleHealthz)
	return mux
}

// handleRecords serves GET /records[?scenario=<id>] (filtered, insertion-ordered)
// and DELETE /records (clear all).
func (a *API) handleRecords(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		records := a.store.List(r.URL.Query().Get("scenario"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(records)
	case http.MethodDelete:
		a.store.Clear()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type watchProbeRequest struct {
	Scenario      string                  `json:"scenario"`
	Mode          recorder.WatchProbeMode `json:"mode"`
	Resource      string                  `json:"resource"`
	Namespace     string                  `json:"namespace,omitempty"`
	LabelSelector string                  `json:"labelSelector,omitempty"`
}

// handleWatchProbe serves POST /watch-probe. It runs a short-lived targeted
// watch and records the resulting transport moment(s) into the shared store.
func (a *API) handleWatchProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.prober == nil {
		http.Error(w, "watch probe unavailable", http.StatusServiceUnavailable)
		return
	}
	var req watchProbeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode watch probe request: "+err.Error(), http.StatusBadRequest)
		return
	}
	gvr, err := parseGVR(req.Resource)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), watchProbeTimeout)
	defer cancel()
	records, err := a.prober.Probe(ctx, recorder.WatchProbeRequest{
		Scenario:      req.Scenario,
		Mode:          req.Mode,
		GVR:           gvr,
		Namespace:     req.Namespace,
		LabelSelector: req.LabelSelector,
	})
	if err != nil {
		http.Error(w, "watch probe: "+err.Error(), http.StatusGatewayTimeout)
		return
	}
	added := make([]mutationlab.Record, 0, len(records))
	for _, rec := range records {
		added = append(added, a.store.Add(rec))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(added)
}

func (a *API) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
