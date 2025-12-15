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

	"github.com/go-logr/logr"

	"github.com/ConfigButler/gitops-reverser/internal/events"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// EventStreamState represents the state of the event stream processing.
type EventStreamState string

const (
	// StartupReconcile buffers events while initial reconciliation runs.
	StartupReconcile EventStreamState = "STARTUP_RECONCILE"
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
}

// EventEnqueuer interface for enqueuing events (allows mocking).
type EventEnqueuer interface {
	Enqueue(event git.Event)
}

// EventEmitter interface for emitting reconciliation events.
type EventEmitter interface {
	EmitCreateEvent(resource types.ResourceIdentifier) error
	EmitDeleteEvent(resource types.ResourceIdentifier) error
	EmitReconcileResourceEvent(resource types.ResourceIdentifier) error
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
		state:                LiveProcessing, // Start in LiveProcessing for now to ensure events are processed
		bufferedEvents:       make([]git.Event, 0),
		processedEventHashes: make(map[string]string),
		branchWorker:         branchWorker,
		logger:               logger.WithValues("gitTarget", fmt.Sprintf("%s/%s", gitTargetNamespace, gitTargetName)),
	}
}

// OnWatchEvent processes incoming watch events from the cluster.
func (s *GitTargetEventStream) OnWatchEvent(event git.Event) {
	switch s.state {
	case StartupReconcile:
		// Buffer all events during reconciliation (no deduplication)
		s.bufferedEvents = append(s.bufferedEvents, event)
		s.logger.V(1).
			Info("Buffered event during reconciliation", "resource", event.Identifier.String(), "bufferSize", len(s.bufferedEvents))

	case LiveProcessing:
		// Check for duplicates using event hash
		eventHash := s.computeEventHash(event)
		resourceKey := event.Identifier.String()

		if lastHash, exists := s.processedEventHashes[resourceKey]; exists && lastHash == eventHash {
			s.logger.V(1).Info("Skipping duplicate event", "resource", resourceKey, "hash", eventHash)
			return
		}

		// Process immediately
		s.processEvent(event, eventHash, resourceKey)
	}
}

// OnReconciliationComplete signals that initial reconciliation has finished.
func (s *GitTargetEventStream) OnReconciliationComplete() {
	if s.state != StartupReconcile {
		s.logger.Info(
			"Reconciliation complete signal received but not in STARTUP_RECONCILE state",
			"currentState",
			s.state,
		)
		return
	}

	s.logger.Info("Reconciliation completed, transitioning to LIVE_PROCESSING", "bufferedEvents", len(s.bufferedEvents))

	// Transition to live processing
	s.state = LiveProcessing

	// Process all buffered events
	for _, event := range s.bufferedEvents {
		eventHash := s.computeEventHash(event)
		resourceKey := event.Identifier.String()
		s.processEvent(event, eventHash, resourceKey)
	}

	// Clear buffer
	s.bufferedEvents = nil
	s.logger.Info("Finished processing buffered events")
}

// processEvent forwards the event to BranchWorker and updates deduplication state.
func (s *GitTargetEventStream) processEvent(event git.Event, eventHash, resourceKey string) {
	// Forward to BranchWorker
	s.branchWorker.Enqueue(event)

	// Update deduplication state
	s.processedEventHashes[resourceKey] = eventHash

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
	return s.state
}

// GetBufferedEventCount returns the number of events currently buffered.
func (s *GitTargetEventStream) GetBufferedEventCount() int {
	return len(s.bufferedEvents)
}

// GetProcessedEventCount returns the number of unique events processed.
func (s *GitTargetEventStream) GetProcessedEventCount() int {
	return len(s.processedEventHashes)
}

// String returns a string representation for debugging.
func (s *GitTargetEventStream) String() string {
	return fmt.Sprintf("GitTargetEventStream(gitTarget=%s/%s, state=%s, buffered=%d, processed=%d)",
		s.gitTargetNamespace, s.gitTargetName, s.state, len(s.bufferedEvents), len(s.processedEventHashes))
}

// EmitCreateEvent emits a CREATE event for reconciliation.
func (s *GitTargetEventStream) EmitCreateEvent(resource types.ResourceIdentifier) error {
	event := git.Event{
		Operation:  "CREATE",
		Identifier: resource,
		// BaseFolder will be set by the reconciler
	}
	s.OnWatchEvent(event)
	return nil
}

// EmitDeleteEvent emits a DELETE event for reconciliation.
func (s *GitTargetEventStream) EmitDeleteEvent(resource types.ResourceIdentifier) error {
	event := git.Event{
		Operation:  "DELETE",
		Identifier: resource,
		// BaseFolder will be set by the reconciler
	}
	s.OnWatchEvent(event)
	return nil
}

// EmitReconcileResourceEvent emits a RECONCILE_RESOURCE event for reconciliation.
func (s *GitTargetEventStream) EmitReconcileResourceEvent(resource types.ResourceIdentifier) error {
	event := git.Event{
		Operation:  string(events.ReconcileResource),
		Identifier: resource,
		// BaseFolder will be set by the reconciler
	}
	s.OnWatchEvent(event)
	return nil
}
