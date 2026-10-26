// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"sync"

	"go.opentelemetry.io/otel/attribute"
)

// objectSeries holds always-1 gauge series keyed by one configuration object, read at scrape time:
// the shape of git_branch_targets. Each object publishes one series, its own identity followed by
// what is recorded for it.
//
// Observable rather than pushed for the reason resource_condition is: these families need series
// that STOP. A pushed gauge latches its last value, so a deleted object would stay published for
// the life of the process. Reading a map at scrape time makes the delete a map delete.
type objectSeries struct {
	// namespaceKey and nameKey are the label keys the object's identity is published under. Always
	// prefixed, never a bare namespace/name: a Prometheus pod scrape with honor_labels=false renames
	// those to exported_* and puts the scraped pod's own in their place.
	namespaceKey string
	nameKey      string

	mu      sync.RWMutex
	entries map[objectKey][]attribute.KeyValue
}

// objectKey identifies the object one published series is about.
type objectKey struct {
	namespace string
	name      string
}

func newObjectSeries(namespaceKey, nameKey string) *objectSeries {
	return &objectSeries{
		namespaceKey: namespaceKey,
		nameKey:      nameKey,
		entries:      map[objectKey][]attribute.KeyValue{},
	}
}

// record publishes one object's labels, replacing any previous entry for it, so a re-record can
// never leave two series for one object.
func (s *objectSeries) record(namespace, name string, attrs ...attribute.KeyValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[objectKey{namespace: namespace, name: name}] = attrs
}

// forget stops publishing one object's series.
func (s *objectSeries) forget(namespace, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, objectKey{namespace: namespace, name: name})
}

// samples renders one sample per object. It takes only its own short-lived lock, which no
// reconcile holds across I/O.
func (s *objectSeries) samples() []GaugeSample {
	s.mu.RLock()
	defer s.mu.RUnlock()

	samples := make([]GaugeSample, 0, len(s.entries))
	for key, attrs := range s.entries {
		samples = append(samples, GaugeSample{
			Value: 1,
			Attrs: append([]attribute.KeyValue{
				attribute.String(s.namespaceKey, key.namespace),
				attribute.String(s.nameKey, key.name),
			}, attrs...),
		})
	}
	return samples
}
