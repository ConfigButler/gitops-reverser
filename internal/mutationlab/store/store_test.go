// SPDX-License-Identifier: Apache-2.0

package store

import (
	"sync"
	"testing"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
)

func TestAdd_AssignsStableID(t *testing.T) {
	s := New()
	first := s.Add(mutationlab.Record{Source: mutationlab.SourceWatch})
	second := s.Add(mutationlab.Record{Source: mutationlab.SourceAudit})
	if first.ID != "r-1" || second.ID != "r-2" {
		t.Fatalf("got ids %q, %q; want r-1, r-2", first.ID, second.ID)
	}
}

func TestAdd_KeepsProvidedID(t *testing.T) {
	s := New()
	r := s.Add(mutationlab.Record{ID: "custom"})
	if r.ID != "custom" {
		t.Fatalf("got %q; want custom", r.ID)
	}
}

func TestList_FiltersByScenarioInOrder(t *testing.T) {
	s := New()
	s.Add(mutationlab.Record{Scenario: "a", Source: mutationlab.SourceWatch})
	s.Add(mutationlab.Record{Scenario: "b", Source: mutationlab.SourceAudit})
	s.Add(mutationlab.Record{Scenario: "a", Source: mutationlab.SourceAdmission})

	got := s.List("a")
	if len(got) != 2 {
		t.Fatalf("scenario a: got %d records, want 2", len(got))
	}
	if got[0].Source != mutationlab.SourceWatch || got[1].Source != mutationlab.SourceAdmission {
		t.Fatalf("scenario a out of order: %+v", got)
	}
	if all := s.List(""); len(all) != 3 {
		t.Fatalf("empty scenario should return all: got %d, want 3", len(all))
	}
}

func TestLen(t *testing.T) {
	s := New()
	s.Add(mutationlab.Record{Scenario: "a"})
	s.Add(mutationlab.Record{Scenario: "a"})
	s.Add(mutationlab.Record{Scenario: "b"})
	if got := s.Len("a"); got != 2 {
		t.Errorf("Len(a) = %d, want 2", got)
	}
	if got := s.Len(""); got != 3 {
		t.Errorf("Len(all) = %d, want 3", got)
	}
}

func TestClear(t *testing.T) {
	s := New()
	s.Add(mutationlab.Record{Scenario: "a"})
	s.Clear()
	if got := s.Len(""); got != 0 {
		t.Fatalf("after Clear, Len = %d, want 0", got)
	}
}

func TestStore_ConcurrentAdd(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Add(mutationlab.Record{Scenario: "race"})
		}()
	}
	wg.Wait()
	if got := s.Len("race"); got != 50 {
		t.Fatalf("concurrent adds: got %d, want 50", got)
	}
}
