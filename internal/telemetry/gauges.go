// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Gauges here are OBSERVABLE: their value is read when Prometheus scrapes, not pushed from the
// loop that owns the state.
//
// The distinction is not stylistic. Both saturation gauges used to be pushed, and both went stale
// during exactly the incident they exist to detect:
//
//   - branch_worker_queue_depth was republished at the BOTTOM of each worker-loop iteration. From
//     idle, fifty items could enqueue with the gauge still reading 0 because nothing had published
//     yet; the loop would then wake and block inside one item's handling, and the gauge stayed 0
//     for as long as that took. It converged to 0 correctly on drain, and also started at 0 and
//     did not move while work piled up.
//   - watch_plan_oldest_dirty_age_seconds was republished once per owner-loop turn, so a pass that
//     wedged stopped the turn and froze the age at whatever it held when the loop stopped moving.
//
// A callback cannot go stale that way: there is no loop between the state and the scrape. The
// companion rule is that "how long" is exported as a TIMESTAMP rather than an age, so the value
// stays true without anyone recomputing it and PromQL does the arithmetic with time() - <gauge>.
// See https://prometheus.io/docs/practices/instrumentation/#timestamps-not-time-since.

// GaugeSample is one observation: a value and the labels it is published under.
type GaugeSample struct {
	Value int64
	Attrs []attribute.KeyValue
}

// GaugeSource produces every sample for one observable gauge, at scrape time. It must not block:
// it runs inside the metric SDK's collection path, so a source that takes a lock the measured loop
// holds across its slow work would reintroduce the staleness this file exists to remove. Read
// atomics and short-lived snapshots, never the work loop's own mutex.
type GaugeSource func() []GaugeSample

// Names for the observable gauges a producer can install a source for. They are the instrument
// names with the prefix stripped, so a reader grepping the exported metric finds the source too.
const (
	GaugeGitQueueDepth               = "git_queue_depth"
	GaugeWatchTypes                  = "watch_types"
	GaugeWatchPlanDirtyTargets       = "watch_plan_dirty_targets"
	GaugeWatchPlanOldestDirtySince   = "watch_plan_oldest_dirty_since_timestamp_seconds"
	GaugeAttributionFactIndexEntries = "attribution_fact_index_entries"
	GaugeAPICatalogResources         = "api_catalog_resources"
	GaugeAPICatalogGroupVersions     = "api_catalog_group_versions"
)

var (
	gaugeSourcesMu sync.RWMutex
	gaugeSources   = map[string]GaugeSource{}
)

// SetGaugeSource installs the source for one observable gauge, replacing any previous one. A nil
// source clears it, which is what a shutting-down producer should do so its callback cannot outlive
// the state it reads. Safe to call before or after the exporter is initialized: registration and
// installation are independent, and a gauge with no source simply reports nothing.
func SetGaugeSource(name string, src GaugeSource) {
	gaugeSourcesMu.Lock()
	defer gaugeSourcesMu.Unlock()
	if src == nil {
		delete(gaugeSources, name)
		return
	}
	gaugeSources[name] = src
}

// gaugeSource returns the currently installed source for a gauge, if any.
func gaugeSource(name string) (GaugeSource, bool) {
	gaugeSourcesMu.RLock()
	defer gaugeSourcesMu.RUnlock()
	src, ok := gaugeSources[name]
	return src, ok
}

// observeGauge is the callback body shared by every observable gauge: read the installed source and
// publish whatever it returns. No source means no series, which is the honest answer for a gauge
// whose producer is not running.
func observeGauge(name string) func(context.Context, metric.Int64Observer) error {
	return func(_ context.Context, obs metric.Int64Observer) error {
		src, ok := gaugeSource(name)
		if !ok {
			return nil
		}
		for _, sample := range src() {
			obs.Observe(sample.Value, metric.WithAttributes(sample.Attrs...))
		}
		return nil
	}
}
