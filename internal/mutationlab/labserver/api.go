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
	"encoding/json"
	"net/http"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab/store"
)

// API serves the lab's records and health endpoints over plain HTTP.
type API struct {
	store *store.Store
}

// NewAPI returns an API backed by the given store.
func NewAPI(s *store.Store) *API { return &API{store: s} }

// Routes registers the records and health handlers on a fresh mux.
func (a *API) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/records", a.handleRecords)
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

func (a *API) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
