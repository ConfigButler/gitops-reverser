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

// Package eventqueue provides a thread-safe queue for processing webhook events.
package eventqueue

import (
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// Event represents a single resource change to be processed.
type Event struct {
	// Object is the sanitized Kubernetes object.
	Object *unstructured.Unstructured

	// Identifier contains all resource identification information
	Identifier types.ResourceIdentifier

	// Operation is the admission operation (CREATE, UPDATE, DELETE)
	Operation string

	// UserInfo contains relevant user information for commit messages
	UserInfo UserInfo

	// GitRepoConfigRef is the name of the GitRepoConfig to use for this event.
	GitRepoConfigRef string

	// GitRepoConfigNamespace is the namespace of the GitRepoConfig to use for this event.
	GitRepoConfigNamespace string

	// Branch is the Git branch to write to (from GitDestination).
	Branch string

	// BaseFolder is an optional POSIX-like relative path prefix under which files will be written.
	// When empty, files are written at the repository root using the identifier path layout.
	BaseFolder string
}

// UserInfo contains relevant user information for commit messages.
type UserInfo struct {
	Username string
	UID      string
}

// Queue is a simple, thread-safe, in-memory queue for events.
type Queue struct {
	mu     sync.Mutex
	events []Event
}

// NewQueue creates a new, empty queue.
func NewQueue() *Queue {
	return &Queue{
		events: make([]Event, 0),
	}
}

// Enqueue adds an event to the queue.
func (q *Queue) Enqueue(event Event) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.events = append(q.events, event)
}

// DequeueAll removes and returns all events from the queue.
func (q *Queue) DequeueAll() []Event {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.events) == 0 {
		return nil
	}

	events := q.events
	q.events = make([]Event, 0)
	return events
}

// Size returns the current number of events in the queue.
func (q *Queue) Size() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.events)
}
