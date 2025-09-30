// Package eventqueue provides a thread-safe queue for processing webhook events.
package eventqueue

import (
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Event represents a single resource change to be processed.
type Event struct {
	// Object is the sanitized Kubernetes object.
	Object *unstructured.Unstructured
	// Request is the original admission request.
	Request admission.Request
	// ResourcePlural is the plural form of the resource (e.g., "pods", "deployments").
	ResourcePlural string
	// GitRepoConfigRef is the name of the GitRepoConfig to use for this event.
	GitRepoConfigRef string
	// GitRepoConfigNamespace is the namespace of the GitRepoConfig to use for this event.
	GitRepoConfigNamespace string
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
