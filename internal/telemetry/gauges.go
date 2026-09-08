// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Gauges here are OBSERVABLE: their value is read when Prometheus scrapes, not pushed from the loop
// that owns the state.
//
// A gauge pushed from inside a work loop reports the loop's last healthy moment for as long as the
// loop is stuck, which is backwards for anything measuring saturation. The companion rule is that
// "how long" is exported as a TIMESTAMP rather than an age, so the value stays true with nobody
// recomputing it and PromQL does the arithmetic with time() - <gauge>. See
// https://prometheus.io/docs/practices/instrumentation/#timestamps-not-time-since and, for the two
// incidents behind these rules, docs/design/metrics-observability-plan.md §2.5.

// GaugeSample is one observation: a value and the labels it is published under.
type GaugeSample struct {
	Value int64
	Attrs []attribute.KeyValue
}

// GaugeSource produces every sample for one observable gauge, at scrape time.
//
// It must read published state and compute nothing. It runs inside the metric SDK's collection
// path, so a source that resolves, refreshes, or waits on a lock the measured loop holds competes
// with the work it is measuring. Read atomics and short-lived snapshots.
type GaugeSource func() []GaugeSample

// Names for the observable gauges a producer can install a source for. They are the instrument
// names with the prefix stripped, so a reader grepping the exported metric finds the source too.
const (
	GaugeGitQueueDepth             = "git_queue_depth"
	GaugeWatchTypes                = "watch_types"
	GaugeWatchStreamsOpen          = "watch_streams_open"
	GaugeWatchPlanDirtyTargets     = "watch_plan_dirty_targets"
	GaugeWatchPlanOldestDirtySince = "watch_plan_oldest_dirty_since_timestamp_seconds"
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
