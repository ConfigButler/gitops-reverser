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

package reconcile

import (
	"crypto/sha256"
	"fmt"
	"sync"

	"github.com/go-logr/logr"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
)

// EventStreamState represents the state of the event stream processing.
type EventStreamState string

const (
	// Reconciling buffers live events while a reconcile batch is in flight.
	Reconciling EventStreamState = "RECONCILING"
	// LiveProcessing processes all events normally.
	LiveProcessing EventStreamState = "LIVE_PROCESSING"
)

// GitTargetEventStream synchronizes live event stream with reconciliation process.
// It provides deterministic state machine behavior and event deduplication.
type GitTargetEventStream struct {
	// Identity
	gitTargetName      string
	gitTargetNamespace string

	// State machine
	state EventStreamState

	// Event buffering during reconciliation
	bufferedEvents []git.Event

	// Event hash deduplication (ResourceIdentifier -> lastEventHash)
	processedEventHashes map[string]string

	// Dependencies
	branchWorker EventEnqueuer
	logger       logr.Logger
	mu           sync.RWMutex
}

// EventEnqueuer interface for enqueuing events and batches (allows mocking).
type EventEnqueuer interface {
	Enqueue(event git.Event)
	EnqueueBatch(batch *git.ReconcileBatch)
}

// NewGitTargetEventStream creates a new event stream for a GitTarget.
func NewGitTargetEventStream(
	gitTargetName, gitTargetNamespace string,
	branchWorker EventEnqueuer,
	logger logr.Logger,
) *GitTargetEventStream {
	return &GitTargetEventStream{
		gitTargetName:        gitTargetName,
		gitTargetNamespace:   gitTargetNamespace,
		state:                Reconciling,
		bufferedEvents:       make([]git.Event, 0),
		processedEventHashes: make(map[string]string),
		branchWorker:         branchWorker,
		logger:               logger.WithValues("gitTarget", fmt.Sprintf("%s/%s", gitTargetNamespace, gitTargetName)),
	}
}

// BeginReconciliation transitions the stream to RECONCILING state (buffering live events).
// Safe to call when already in RECONCILING state (no-op).
func (s *GitTargetEventStream) BeginReconciliation() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == Reconciling {
		return
	}
	s.state = Reconciling
	s.bufferedEvents = make([]git.Event, 0)
	s.logger.Info("Transitioned to RECONCILING state")
}

// OnWatchEvent processes incoming watch events from the cluster.
func (s *GitTargetEventStream) OnWatchEvent(event git.Event) {
	s.mu.Lock()
	switch s.state {
	case Reconciling:
		// Buffer all events during reconciliation (no deduplication)
		s.bufferedEvents = append(s.bufferedEvents, event)
		bufferSize := len(s.bufferedEvents)
		s.mu.Unlock()
		s.logger.V(1).
			Info("Buffered event during reconciliation", "resource", event.Identifier.String(), "bufferSize", bufferSize)

	case LiveProcessing:
		// Check for duplicates using event hash
		eventHash := s.computeEventHash(event)
		resourceKey := event.Identifier.Key()

		if lastHash, exists := s.processedEventHashes[resourceKey]; exists && lastHash == eventHash {
			s.mu.Unlock()
			s.logger.V(1).Info("Skipping duplicate event", "resource", resourceKey, "hash", eventHash)
			return
		}
		s.mu.Unlock()

		// Process immediately
		s.processEvent(event, eventHash, resourceKey)

	default:
		s.mu.Unlock()
	}
}

// EmitReconcileBatch forwards a complete reconcile batch to the BranchWorker as a single WorkItem.
// Called while in RECONCILING state by FolderReconciler.
func (s *GitTargetEventStream) EmitReconcileBatch(batch git.ReconcileBatch) error {
	batch.GitTargetName = s.gitTargetName
	batch.GitTargetNamespace = s.gitTargetNamespace
	s.branchWorker.EnqueueBatch(&batch)
	return nil
}

// OnReconciliationComplete signals that reconciliation has finished.
// Transitions to LIVE_PROCESSING and flushes buffered live events.
func (s *GitTargetEventStream) OnReconciliationComplete() {
	s.mu.Lock()
	if s.state != Reconciling {
		currentState := s.state
		s.mu.Unlock()
		s.logger.Info(
			"Reconciliation complete signal received but not in RECONCILING state",
			"currentState",
			currentState,
		)
		return
	}

	bufferedEvents := append([]git.Event(nil), s.bufferedEvents...)
	s.logger.Info("Reconciliation completed, transitioning to LIVE_PROCESSING", "bufferedEvents", len(bufferedEvents))

	// Transition to live processing
	s.state = LiveProcessing
	s.bufferedEvents = nil
	s.mu.Unlock()

	// Process all buffered events
	for _, event := range bufferedEvents {
		eventHash := s.computeEventHash(event)
		resourceKey := event.Identifier.Key()
		s.processEvent(event, eventHash, resourceKey)
	}

	s.logger.Info("Finished processing buffered events")
}

// processEvent forwards the event to BranchWorker and updates deduplication state.
func (s *GitTargetEventStream) processEvent(event git.Event, eventHash, resourceKey string) {
	if event.Object == nil && event.Operation != "DELETE" {
		s.logger.V(1).Info(
			"Skipping event with no object payload",
			"resource", resourceKey,
			"operation", event.Operation,
		)
		return
	}

	event.GitTargetName = s.gitTargetName
	event.GitTargetNamespace = s.gitTargetNamespace

	// Forward to BranchWorker
	s.branchWorker.Enqueue(event)

	// Update deduplication state
	s.mu.Lock()
	s.processedEventHashes[resourceKey] = eventHash
	s.mu.Unlock()

	s.logger.V(1).Info("Processed event", "resource", resourceKey, "hash", eventHash)
}

// computeEventHash calculates a hash of the event content that would be written to Git.
func (s *GitTargetEventStream) computeEventHash(event git.Event) string {
	if event.Object == nil {
		// Control events - hash the operation and identifier
		content := fmt.Sprintf("%s:%s", event.Operation, event.Identifier.String())
		return fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	}

	// For resource events, hash the operation + sanitized YAML content that would be committed
	// This ensures CREATE and UPDATE operations for the same final content are treated as duplicates
	sanitized, err := sanitize.MarshalToOrderedYAML(event.Object)
	if err != nil {
		// Fallback to object hash if sanitization fails
		s.logger.Error(err, "Failed to sanitize object for hash, using fallback", "resource", event.Identifier.String())
		content := fmt.Sprintf("%s:%s:%v", event.Operation, event.Identifier.String(), event.Object.Object)
		return fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	}

	// Include operation in hash to distinguish CREATE from UPDATE/DELETE for same content
	content := fmt.Sprintf("%s:%s", event.Operation, string(sanitized))
	return fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
}

// GetState returns the current state of the event stream.
func (s *GitTargetEventStream) GetState() EventStreamState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// GetBufferedEventCount returns the number of events currently buffered.
func (s *GitTargetEventStream) GetBufferedEventCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.bufferedEvents)
}

// GetProcessedEventCount returns the number of unique events processed.
func (s *GitTargetEventStream) GetProcessedEventCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.processedEventHashes)
}

// String returns a string representation for debugging.
func (s *GitTargetEventStream) String() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fmt.Sprintf("GitTargetEventStream(gitTarget=%s/%s, state=%s, buffered=%d, processed=%d)",
		s.gitTargetNamespace, s.gitTargetName, s.state, len(s.bufferedEvents), len(s.processedEventHashes))
}
