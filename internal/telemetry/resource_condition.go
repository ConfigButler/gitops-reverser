// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"sync"

	"go.opentelemetry.io/otel/attribute"
)

// resource_condition is the CONFIGURATION-STATE surface, and the one question every other
// instrument in this package leaves unanswered.
//
// The rest of the metric set answers "is the pipeline flowing" — events ingested, documents
// written, commits pushed. None of it answers "was my configuration accepted", and a GitTarget that
// has sat at Ready=False for a day is as serious as a stalled push while only one of the two can
// page anybody. The docs route the reader to a condition over and over; this is the signal behind
// those sentences.
//
// Three details carry the design, all three learned from Flux's gotk_reconcile_condition:
//
//   - STATUS IS A LABEL, not the value. Each condition publishes one series per possible status —
//     True, False, Unknown — of which exactly one is 1. The obvious alternative, one series whose
//     value encodes the status, cannot be selected on and cannot be aggregated: `count by (status)`
//     is meaningless over an encoded number. Three times the series, and worth it every time.
//   - UNKNOWN IS SYNTHESIZED by the caller, never left as an absent series. "The object exists and
//     has not been reconciled yet" is a state, and a missing series is how a state becomes
//     invisible.
//   - THE DELETE PATH IS THE CORRECTNESS-CRITICAL HALF. Flux shipped this metric in 2021 and added
//     deletion in 2023; for two years a deleted object reported Ready=False forever and the alert
//     never cleared, which is worse than no metric because it teaches people to ignore the panel.
//     Publishing from a map read at scrape time makes the delete a map delete: ForgetResourceConditions
//     drops the key and the series stops. There is no gauge value to "unset".
//
// It is also the only family here keyed on object identity, which
// docs/design/metrics-observability-plan.md §7.1 permits for exactly this population and nothing
// else: these are CONFIG objects, bounded by how many a human wrote, not the watched objects the
// operator mirrors, which are data and unbounded. The label keys are prefixed — resource_namespace,
// not a bare namespace — because a Prometheus pod scrape with honor_labels=false overwrites a bare
// `namespace` attribute with the scraped pod's own.

// ResourceConditionState is one condition as a reconcile last observed it.
//
// Type/Status/Reason are plain strings rather than the apimachinery types so this package stays
// free of a Kubernetes dependency: the caller reads the condition, decides which types are on the
// surface, and synthesizes Unknown for the ones the object does not carry yet.
type ResourceConditionState struct {
	Type   string
	Status string
	Reason string
}

// resourceConditionStatuses are the three values a Kubernetes condition status may take, fixed by
// API convention. Every condition publishes one series per entry, and a state whose Status matches
// none of them publishes three zeros — visibly wrong rather than silently absent.
var resourceConditionStatuses = []string{"True", "False", "Unknown"}

// resourceConditionKey identifies the object and condition one published group of series is about.
type resourceConditionKey struct {
	kind          string
	namespace     string
	name          string
	conditionType string
}

var (
	resourceConditionsMu sync.RWMutex
	resourceConditions   = map[resourceConditionKey]ResourceConditionState{}
)

// RecordResourceConditions replaces everything published about one configuration object.
//
// It REPLACES rather than merges, which is what keeps `reason` safe to carry as a label: a
// condition that moves from ProviderNotFound to Succeeded leaves no series behind, because the
// object's whole entry is rewritten and the next scrape observes only what is in the map. Call it
// on every reconcile, including one that computes no change: a gauge is level, not an event, and a
// fresh pod whose first pass has nothing to write must still publish or the series is missing until
// something happens to move it.
//
// An empty states slice is a no-op rather than a deletion; deleting an object's series is
// ForgetResourceConditions, and saying so in one place keeps a caller from erasing an object by
// passing it nothing.
func RecordResourceConditions(kind, namespace, name string, states []ResourceConditionState) {
	if len(states) == 0 {
		return
	}
	resourceConditionsMu.Lock()
	defer resourceConditionsMu.Unlock()

	forgetResourceConditionsLocked(kind, namespace, name)
	for _, state := range states {
		resourceConditions[resourceConditionKey{
			kind:          kind,
			namespace:     namespace,
			name:          name,
			conditionType: state.Type,
		}] = state
	}
}

// ForgetResourceConditions stops publishing every series about one object. It is the delete path:
// call it when the object is gone, not when its deletion was merely requested.
func ForgetResourceConditions(kind, namespace, name string) {
	resourceConditionsMu.Lock()
	defer resourceConditionsMu.Unlock()
	forgetResourceConditionsLocked(kind, namespace, name)
}

// forgetResourceConditionsLocked drops one object's entries. The caller holds the write lock.
func forgetResourceConditionsLocked(kind, namespace, name string) {
	for key := range resourceConditions {
		if key.kind == kind && key.namespace == namespace && key.name == name {
			delete(resourceConditions, key)
		}
	}
}

// resourceConditionSamples renders the map as gauge samples, one per (condition, possible status).
// It takes only its own short-lived lock, which no reconcile holds across I/O.
func resourceConditionSamples() []GaugeSample {
	resourceConditionsMu.RLock()
	defer resourceConditionsMu.RUnlock()

	samples := make([]GaugeSample, 0, len(resourceConditions)*len(resourceConditionStatuses))
	for key, state := range resourceConditions {
		for _, status := range resourceConditionStatuses {
			var value int64
			if status == state.Status {
				value = 1
			}
			samples = append(samples, GaugeSample{
				Value: value,
				Attrs: []attribute.KeyValue{
					attribute.String("kind", key.kind),
					attribute.String("resource_namespace", key.namespace),
					attribute.String("resource_name", key.name),
					attribute.String("type", key.conditionType),
					attribute.String("status", status),
					// The reason of the condition as it stands, carried on all three series
					// because they are one condition rendered three ways. It is what turns a count
					// panel into a first guess: Ready=False with reason UnsupportedContent names
					// the gate that failed without a kubectl describe.
					attribute.String("reason", state.Reason),
				},
			})
		}
	}
	return samples
}
