// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// collectMetric collects a manual reader and returns the aggregation of the
// named metric. It is intended for tests wired through InitTestExporter.
func collectMetric(reader *sdkmetric.ManualReader, metricName string) (metricdata.Aggregation, bool) {
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		return nil, false
	}
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == metricName {
				return m.Data, true
			}
		}
	}
	return nil, false
}

// CollectInt64Sum returns the summed value of the named Int64 counter or gauge
// data points whose attributes are a superset of match. ok is false when no
// matching data point exists.
func CollectInt64Sum(
	reader *sdkmetric.ManualReader,
	metricName string,
	match map[string]string,
) (int64, bool) {
	data, found := collectMetric(reader, metricName)
	if !found {
		return 0, false
	}
	var value int64
	var ok bool
	switch agg := data.(type) {
	case metricdata.Sum[int64]:
		for _, dp := range agg.DataPoints {
			if attrsMatch(dp.Attributes, match) {
				value += dp.Value
				ok = true
			}
		}
	case metricdata.Gauge[int64]:
		for _, dp := range agg.DataPoints {
			if attrsMatch(dp.Attributes, match) {
				value = dp.Value
				ok = true
			}
		}
	}
	return value, ok
}

// CollectHistogramCount returns the total sample count of the named float
// histogram data points whose attributes are a superset of match. ok is false
// when no matching data point exists.
func CollectHistogramCount(
	reader *sdkmetric.ManualReader,
	metricName string,
	match map[string]string,
) (uint64, bool) {
	data, found := collectMetric(reader, metricName)
	if !found {
		return 0, false
	}
	agg, isHist := data.(metricdata.Histogram[float64])
	if !isHist {
		return 0, false
	}
	var count uint64
	var ok bool
	for _, dp := range agg.DataPoints {
		if attrsMatch(dp.Attributes, match) {
			count += dp.Count
			ok = true
		}
	}
	return count, ok
}

// attrsMatch reports whether every key/value in match is present in set.
func attrsMatch(set attribute.Set, match map[string]string) bool {
	for key, want := range match {
		got, present := set.Value(attribute.Key(key))
		if !present || got.AsString() != want {
			return false
		}
	}
	return true
}
