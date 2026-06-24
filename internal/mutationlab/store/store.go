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

// Package store is the lab's in-memory record store. Audit webhook delivery is
// asynchronous and batched, so a late event from one scenario can land after the
// next has started; clearing between scenarios is therefore not enough. The store
// keeps every record attributed to its scenario and serves reads filtered by
// scenario, so the test driver isolates scenarios instead of trusting a clean
// slate (see docs/design/mutation-capture-lab-design.md, "Isolated Test Setup").
package store

import (
	"fmt"
	"sync"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
)

// Store is a concurrency-safe, insertion-ordered record store.
type Store struct {
	mu      sync.RWMutex
	records []mutationlab.Record
	seq     int
}

// New returns an empty Store.
func New() *Store { return &Store{} }

// Add stores a record, assigning a stable insertion ID when the record has none,
// and returns the stored copy. Records are kept in observation (insertion) order,
// because for the corpus the ordering and count of events are part of the
// behavior under test.
func (s *Store) Add(r mutationlab.Record) mutationlab.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	if r.ID == "" {
		r.ID = fmt.Sprintf("r-%d", s.seq)
	}
	s.records = append(s.records, r)
	return r
}

// List returns the records for one scenario in insertion order. An empty
// scenario returns every record.
func (s *Store) List(scenario string) []mutationlab.Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]mutationlab.Record, 0, len(s.records))
	for _, r := range s.records {
		if scenario == "" || r.Scenario == scenario {
			out = append(out, r)
		}
	}
	return out
}

// Len reports the number of records for one scenario ("" counts all). It is the
// signal the test driver's bounded drain watches: it waits until the expected
// records have arrived and the count has been quiet for a settle window.
func (s *Store) Len(scenario string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if scenario == "" {
		return len(s.records)
	}
	n := 0
	for _, r := range s.records {
		if r.Scenario == scenario {
			n++
		}
	}
	return n
}

// Clear removes all records.
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = nil
}
