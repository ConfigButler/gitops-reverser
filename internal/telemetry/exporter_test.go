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

package telemetry

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// metricAttrs builds a single-attribute measurement option for tests.
func metricAttrs(key, value string) metric.MeasurementOption {
	return metric.WithAttributes(attribute.String(key, value))
}

func TestInitOTLPExporter_Success(t *testing.T) {
	ctx := context.Background()

	shutdownFunc, err := InitOTLPExporter(ctx)

	require.NoError(t, err)
	assert.NotNil(t, shutdownFunc)

	// Verify representative metrics across counters, histograms, and gauges
	// are initialized.
	assert.NotNil(t, GitOperationsTotal)
	assert.NotNil(t, GitPushDurationSeconds)
	assert.NotNil(t, ObjectsScannedTotal)
	assert.NotNil(t, AuditEventListsTotal)
	assert.NotNil(t, AuditEventListEventsTotal)
	assert.NotNil(t, AuditEventListDurationSeconds)
	assert.NotNil(t, AuditEventsTotal)
	assert.NotNil(t, MaterializationCheckpointFillsTotal)
	assert.NotNil(t, APICatalogResources)
	assert.NotNil(t, APICatalogGroupVersions)
	assert.NotNil(t, APICatalogRefreshTotal)
	assert.NotNil(t, APICatalogRefreshDurationSeconds)
	assert.NotNil(t, APICatalogGeneration)

	err = shutdownFunc(ctx)
	require.NoError(t, err)
}

func TestMetricsInitialization(t *testing.T) {
	ctx := context.Background()

	shutdownFunc, err := InitOTLPExporter(ctx)
	require.NoError(t, err)
	defer func() {
		if shutdownFunc != nil {
			assert.NoError(t, shutdownFunc(ctx))
		}
	}()

	// Test that all metrics can be used without panicking.
	t.Run("GitOperationsTotal", func(t *testing.T) {
		assert.NotPanics(t, func() {
			GitOperationsTotal.Add(ctx, 1)
		})
	})

	t.Run("GitPushDurationSeconds", func(t *testing.T) {
		assert.NotPanics(t, func() {
			GitPushDurationSeconds.Record(ctx, 1.5)
		})
	})

	t.Run("AuditEventListsTotal", func(t *testing.T) {
		assert.NotPanics(t, func() {
			AuditEventListsTotal.Add(ctx, 1)
		})
	})

	t.Run("AuditEventListDurationSeconds", func(t *testing.T) {
		assert.NotPanics(t, func() {
			AuditEventListDurationSeconds.Record(ctx, 0.25)
		})
	})

	t.Run("AuditEventsTotal", func(t *testing.T) {
		assert.NotPanics(t, func() {
			AuditEventsTotal.Add(ctx, 1)
		})
	})

	t.Run("APICatalogResources", func(t *testing.T) {
		assert.NotPanics(t, func() {
			APICatalogResources.Record(ctx, 42)
		})
	})

	t.Run("APICatalogGeneration", func(t *testing.T) {
		assert.NotPanics(t, func() {
			APICatalogGeneration.Record(ctx, 7)
		})
	})

	t.Run("APICatalogRefreshDurationSeconds", func(t *testing.T) {
		assert.NotPanics(t, func() {
			APICatalogRefreshDurationSeconds.Record(ctx, 0.05)
		})
	})
}

func TestMeterInitialization(t *testing.T) {
	assert.NotNil(t, otelMeter)

	globalMeter := otel.Meter("gitops-reverser")
	assert.NotNil(t, globalMeter)
}

func TestAuditPipelineMetricUsage(t *testing.T) {
	ctx := context.Background()

	shutdownFunc, err := InitOTLPExporter(ctx)
	require.NoError(t, err)
	defer func() {
		if shutdownFunc != nil {
			assert.NoError(t, shutdownFunc(ctx))
		}
	}()

	// Simulate the audit ingestion pipeline emitting metrics at each stage.
	assert.NotPanics(t, func() {
		AuditEventListsTotal.Add(ctx, 1)
		AuditEventListEventsTotal.Add(ctx, 5)
		AuditEventListDurationSeconds.Record(ctx, 0.12)
		AuditEventsTotal.Add(ctx, 5, metricAttrs("outcome", "queued"))
		AuditEventsTotal.Add(ctx, 1, metricAttrs("outcome", "dry_run"))
	})
}

func TestAPICatalogMetricUsage(t *testing.T) {
	ctx := context.Background()

	shutdownFunc, err := InitOTLPExporter(ctx)
	require.NoError(t, err)
	defer func() {
		if shutdownFunc != nil {
			assert.NoError(t, shutdownFunc(ctx))
		}
	}()

	// Gauges are idempotent: overwriting them on every refresh is correct.
	assert.NotPanics(t, func() {
		APICatalogResources.Record(ctx, 120)
		APICatalogResources.Record(ctx, 118)
		APICatalogGroupVersions.Record(ctx, 30)
		APICatalogGroupVersions.Record(ctx, 0)
		APICatalogRefreshTotal.Add(ctx, 1)
		APICatalogRefreshDurationSeconds.Record(ctx, 0.03)
		APICatalogGeneration.Record(ctx, 12)
	})
}

func TestConcurrentMetricsUsage(t *testing.T) {
	ctx := context.Background()

	shutdownFunc, err := InitOTLPExporter(ctx)
	require.NoError(t, err)
	defer func() {
		if shutdownFunc != nil {
			assert.NoError(t, shutdownFunc(ctx))
		}
	}()

	done := make(chan bool, 3)

	go func() {
		defer func() { done <- true }()
		for range 100 {
			AuditEventListsTotal.Add(ctx, 1)
		}
	}()

	go func() {
		defer func() { done <- true }()
		for range 100 {
			AuditEventsTotal.Add(ctx, 1, metricAttrs("outcome", "queued"))
		}
	}()

	go func() {
		defer func() { done <- true }()
		for i := range 100 {
			GitOperationsTotal.Add(ctx, 1)
			GitPushDurationSeconds.Record(ctx, float64(i)*0.01)
		}
	}()

	<-done
	<-done
	<-done
}

func TestHistogramMetricBehavior(t *testing.T) {
	ctx := context.Background()

	shutdownFunc, err := InitOTLPExporter(ctx)
	require.NoError(t, err)
	defer func() {
		if shutdownFunc != nil {
			assert.NoError(t, shutdownFunc(ctx))
		}
	}()

	testValues := []float64{0.001, 0.1, 1.0, 5.0, 30.0, 0.0}

	for _, value := range testValues {
		t.Run(fmt.Sprintf("Duration_%g", value), func(t *testing.T) {
			assert.NotPanics(t, func() {
				GitPushDurationSeconds.Record(ctx, value)
				AuditEventListDurationSeconds.Record(ctx, value)
				APICatalogRefreshDurationSeconds.Record(ctx, value)
			})
		})
	}
}

func TestMetricsErrorHandling(t *testing.T) {
	// Document behavior when metrics are not initialized.
	original := GitOperationsTotal
	GitOperationsTotal = nil
	defer func() { GitOperationsTotal = original }()

	ctx := context.Background()

	t.Run("NilMetrics", func(t *testing.T) {
		assert.Panics(t, func() {
			GitOperationsTotal.Add(ctx, 1)
		})
	})
}

func TestShutdownFunction(t *testing.T) {
	ctx := context.Background()

	shutdownFunc, err := InitOTLPExporter(ctx)
	require.NoError(t, err)
	require.NotNil(t, shutdownFunc)

	err = shutdownFunc(ctx)
	require.NoError(t, err)

	// Calling shutdown multiple times should not error.
	err = shutdownFunc(ctx)
	require.NoError(t, err)
}

func TestMetricsAfterShutdown(t *testing.T) {
	ctx := context.Background()

	shutdownFunc, err := InitOTLPExporter(ctx)
	require.NoError(t, err)

	assert.NotPanics(t, func() {
		GitOperationsTotal.Add(ctx, 1)
	})

	err = shutdownFunc(ctx)
	require.NoError(t, err)

	// Metrics still work after shutdown (they just are not exported).
	assert.NotPanics(t, func() {
		GitOperationsTotal.Add(ctx, 1)
	})
}

func TestInitTestExporterAndCollect(t *testing.T) {
	ctx := context.Background()

	reader, err := InitTestExporter()
	require.NoError(t, err)
	require.NotNil(t, reader)

	t.Run("counter sum by attributes", func(t *testing.T) {
		AuditEventsTotal.Add(ctx, 2, metricAttrs("outcome", "queued"))
		AuditEventsTotal.Add(ctx, 3, metricAttrs("outcome", "queued"))
		AuditEventsTotal.Add(ctx, 7, metricAttrs("outcome", "older_than_high_water"))

		queued, ok := CollectInt64Sum(reader, "gitopsreverser_audit_events_total",
			map[string]string{"outcome": "queued"})
		require.True(t, ok)
		assert.Equal(t, int64(5), queued)

		diverted, ok := CollectInt64Sum(reader, "gitopsreverser_audit_events_total",
			map[string]string{"outcome": "older_than_high_water"})
		require.True(t, ok)
		assert.Equal(t, int64(7), diverted)
	})

	t.Run("missing metric and attribute", func(t *testing.T) {
		_, ok := CollectInt64Sum(reader, "gitopsreverser_does_not_exist", nil)
		assert.False(t, ok)

		_, ok = CollectInt64Sum(reader, "gitopsreverser_audit_events_total",
			map[string]string{"outcome": "never_recorded"})
		assert.False(t, ok)
	})

	t.Run("gauge last value", func(t *testing.T) {
		APICatalogResources.Record(ctx, 10, metricAttrs("state", "allowed"))
		APICatalogResources.Record(ctx, 12, metricAttrs("state", "allowed"))

		allowed, ok := CollectInt64Sum(reader, "gitopsreverser_api_catalog_resources",
			map[string]string{"state": "allowed"})
		require.True(t, ok)
		assert.Equal(t, int64(12), allowed)
	})

	t.Run("histogram count", func(t *testing.T) {
		AuditEventListDurationSeconds.Record(ctx, 0.1, metricAttrs("source", "official"))
		AuditEventListDurationSeconds.Record(ctx, 0.2, metricAttrs("source", "official"))

		count, ok := CollectHistogramCount(reader, "gitopsreverser_audit_eventlist_duration_seconds",
			map[string]string{"source": "official"})
		require.True(t, ok)
		assert.Equal(t, uint64(2), count)

		_, ok = CollectHistogramCount(reader, "gitopsreverser_audit_events_total", nil)
		assert.False(t, ok, "a counter is not a histogram")
	})
}

func TestMeterProviderIntegration(t *testing.T) {
	testMeter := otel.Meter("gitops-reverser")
	assert.NotNil(t, testMeter)

	counter, err := testMeter.Int64Counter("test_counter")
	require.NoError(t, err)
	assert.NotNil(t, counter)

	ctx := context.Background()
	assert.NotPanics(t, func() {
		counter.Add(ctx, 1)
	})
}

func TestNoOpMeterProvider(t *testing.T) {
	ctx := context.Background()

	noopProvider := noop.NewMeterProvider()
	otel.SetMeterProvider(noopProvider)
	defer otel.SetMeterProvider(nil)

	shutdownFunc, err := InitOTLPExporter(ctx)
	require.NoError(t, err)
	assert.NotNil(t, shutdownFunc)

	assert.NotPanics(t, func() {
		GitOperationsTotal.Add(ctx, 1)
		GitPushDurationSeconds.Record(ctx, 1.0)
		AuditEventsTotal.Add(ctx, 1, metricAttrs("outcome", "queued"))
		APICatalogResources.Record(ctx, 1)
	})

	err = shutdownFunc(ctx)
	require.NoError(t, err)
}
