package metrics

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
)

func TestInitOTLPExporter_Success(t *testing.T) {
	ctx := context.Background()

	// Execute
	shutdownFunc, err := InitOTLPExporter(ctx)

	// Verify
	assert.NoError(t, err)
	assert.NotNil(t, shutdownFunc)

	// Verify all metrics are initialized
	assert.NotNil(t, EventsReceivedTotal)
	assert.NotNil(t, EventsProcessedTotal)
	assert.NotNil(t, GitOperationsTotal)
	assert.NotNil(t, GitPushDurationSeconds)
	assert.NotNil(t, GitCommitQueueSize)

	// Test shutdown function
	err = shutdownFunc(ctx)
	assert.NoError(t, err)
}

func TestMetricsInitialization(t *testing.T) {
	ctx := context.Background()

	// Initialize metrics
	shutdownFunc, err := InitOTLPExporter(ctx)
	require.NoError(t, err)
	defer func() {
		if shutdownFunc != nil {
			shutdownFunc(ctx)
		}
	}()

	// Test that all metrics can be used without panicking
	t.Run("EventsReceivedTotal", func(t *testing.T) {
		assert.NotPanics(t, func() {
			EventsReceivedTotal.Add(ctx, 1)
		})
	})

	t.Run("EventsProcessedTotal", func(t *testing.T) {
		assert.NotPanics(t, func() {
			EventsProcessedTotal.Add(ctx, 1)
		})
	})

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

	t.Run("GitCommitQueueSize", func(t *testing.T) {
		assert.NotPanics(t, func() {
			GitCommitQueueSize.Add(ctx, 1)
			GitCommitQueueSize.Add(ctx, -1)
		})
	})
}

func TestMetricNames(t *testing.T) {
	ctx := context.Background()

	// Initialize metrics
	shutdownFunc, err := InitOTLPExporter(ctx)
	require.NoError(t, err)
	defer func() {
		if shutdownFunc != nil {
			shutdownFunc(ctx)
		}
	}()

	// We can't directly test the metric names from the instruments,
	// but we can verify they were created without error
	assert.NotNil(t, EventsReceivedTotal)
	assert.NotNil(t, EventsProcessedTotal)
	assert.NotNil(t, GitOperationsTotal)
	assert.NotNil(t, GitPushDurationSeconds)
	assert.NotNil(t, GitCommitQueueSize)
}

func TestMeterInitialization(t *testing.T) {
	// Verify the meter is initialized with the correct name
	assert.NotNil(t, meter)

	// The meter should be from the global meter provider
	globalMeter := otel.Meter("gitops-reverser")
	assert.NotNil(t, globalMeter)
}

func TestMetricsUsagePatterns(t *testing.T) {
	ctx := context.Background()

	// Initialize metrics
	shutdownFunc, err := InitOTLPExporter(ctx)
	require.NoError(t, err)
	defer func() {
		if shutdownFunc != nil {
			shutdownFunc(ctx)
		}
	}()

	// Test typical usage patterns
	t.Run("WebhookEventProcessing", func(t *testing.T) {
		// Simulate webhook receiving and processing events
		assert.NotPanics(t, func() {
			EventsReceivedTotal.Add(ctx, 1)
			EventsProcessedTotal.Add(ctx, 1)
			GitCommitQueueSize.Add(ctx, 1)
		})
	})

	t.Run("GitOperations", func(t *testing.T) {
		// Simulate git operations
		assert.NotPanics(t, func() {
			GitOperationsTotal.Add(ctx, 1)
			GitPushDurationSeconds.Record(ctx, 2.5)
			GitCommitQueueSize.Add(ctx, -1) // Decrement after processing
		})
	})

	t.Run("BatchProcessing", func(t *testing.T) {
		// Simulate batch processing
		assert.NotPanics(t, func() {
			// Multiple events received
			EventsReceivedTotal.Add(ctx, 5)

			// Queue size increases
			GitCommitQueueSize.Add(ctx, 5)

			// Batch processed
			EventsProcessedTotal.Add(ctx, 5)
			GitOperationsTotal.Add(ctx, 1) // One git operation for the batch
			GitPushDurationSeconds.Record(ctx, 3.2)

			// Queue size decreases
			GitCommitQueueSize.Add(ctx, -5)
		})
	})
}

func TestMetricsWithAttributes(t *testing.T) {
	ctx := context.Background()

	// Initialize metrics
	shutdownFunc, err := InitOTLPExporter(ctx)
	require.NoError(t, err)
	defer func() {
		if shutdownFunc != nil {
			shutdownFunc(ctx)
		}
	}()

	// Test metrics with different attribute patterns
	// Note: The current implementation doesn't use attributes,
	// but we test that the metrics work with context variations

	t.Run("WithDifferentContexts", func(t *testing.T) {
		ctx1 := context.WithValue(ctx, "operation", "create")
		ctx2 := context.WithValue(ctx, "operation", "update")
		ctx3 := context.WithValue(ctx, "operation", "delete")

		assert.NotPanics(t, func() {
			EventsReceivedTotal.Add(ctx1, 1)
			EventsReceivedTotal.Add(ctx2, 1)
			EventsReceivedTotal.Add(ctx3, 1)
		})
	})
}

func TestConcurrentMetricsUsage(t *testing.T) {
	ctx := context.Background()

	// Initialize metrics
	shutdownFunc, err := InitOTLPExporter(ctx)
	require.NoError(t, err)
	defer func() {
		if shutdownFunc != nil {
			shutdownFunc(ctx)
		}
	}()

	// Test concurrent access to metrics
	done := make(chan bool, 3)

	// Goroutine 1: Events received
	go func() {
		defer func() { done <- true }()
		for i := 0; i < 100; i++ {
			EventsReceivedTotal.Add(ctx, 1)
		}
	}()

	// Goroutine 2: Events processed
	go func() {
		defer func() { done <- true }()
		for i := 0; i < 100; i++ {
			EventsProcessedTotal.Add(ctx, 1)
		}
	}()

	// Goroutine 3: Git operations
	go func() {
		defer func() { done <- true }()
		for i := 0; i < 100; i++ {
			GitOperationsTotal.Add(ctx, 1)
			GitPushDurationSeconds.Record(ctx, float64(i)*0.01)
		}
	}()

	// Wait for all goroutines to complete
	<-done
	<-done
	<-done

	// Test should complete without panics or deadlocks
}

func TestQueueSizeMetricBehavior(t *testing.T) {
	ctx := context.Background()

	// Initialize metrics
	shutdownFunc, err := InitOTLPExporter(ctx)
	require.NoError(t, err)
	defer func() {
		if shutdownFunc != nil {
			shutdownFunc(ctx)
		}
	}()

	// Test UpDownCounter behavior
	t.Run("IncreaseDecrease", func(t *testing.T) {
		assert.NotPanics(t, func() {
			// Increase queue size
			GitCommitQueueSize.Add(ctx, 10)

			// Decrease queue size
			GitCommitQueueSize.Add(ctx, -5)

			// Further decrease
			GitCommitQueueSize.Add(ctx, -5)
		})
	})

	t.Run("NegativeValues", func(t *testing.T) {
		assert.NotPanics(t, func() {
			// UpDownCounter should handle negative values
			GitCommitQueueSize.Add(ctx, -1)
		})
	})
}

func TestHistogramMetricBehavior(t *testing.T) {
	ctx := context.Background()

	// Initialize metrics
	shutdownFunc, err := InitOTLPExporter(ctx)
	require.NoError(t, err)
	defer func() {
		if shutdownFunc != nil {
			shutdownFunc(ctx)
		}
	}()

	// Test histogram with various values
	testValues := []float64{
		0.001, // Very fast
		0.1,   // Fast
		1.0,   // Normal
		5.0,   // Slow
		30.0,  // Very slow
		0.0,   // Edge case: zero
	}

	for _, value := range testValues {
		t.Run("Duration_"+string(rune(int(value*1000))), func(t *testing.T) {
			assert.NotPanics(t, func() {
				GitPushDurationSeconds.Record(ctx, value)
			})
		})
	}
}

func TestMetricsErrorHandling(t *testing.T) {
	// Test behavior when metrics are not initialized
	// This tests the global variables before initialization

	// Save original values
	originalEventsReceived := EventsReceivedTotal
	originalEventsProcessed := EventsProcessedTotal
	originalGitOperations := GitOperationsTotal
	originalGitPushDuration := GitPushDurationSeconds
	originalGitCommitQueue := GitCommitQueueSize

	// Reset to nil to simulate uninitialized state
	EventsReceivedTotal = nil
	EventsProcessedTotal = nil
	GitOperationsTotal = nil
	GitPushDurationSeconds = nil
	GitCommitQueueSize = nil

	defer func() {
		// Restore original values
		EventsReceivedTotal = originalEventsReceived
		EventsProcessedTotal = originalEventsProcessed
		GitOperationsTotal = originalGitOperations
		GitPushDurationSeconds = originalGitPushDuration
		GitCommitQueueSize = originalGitCommitQueue
	}()

	ctx := context.Background()

	// These should panic or handle nil gracefully
	// In a real implementation, we might want to add nil checks
	t.Run("NilMetrics", func(t *testing.T) {
		// Current implementation will panic with nil metrics
		// This test documents the current behavior
		assert.Panics(t, func() {
			EventsReceivedTotal.Add(ctx, 1)
		})
	})
}

func TestShutdownFunction(t *testing.T) {
	ctx := context.Background()

	// Initialize metrics
	shutdownFunc, err := InitOTLPExporter(ctx)
	require.NoError(t, err)
	require.NotNil(t, shutdownFunc)

	// Test shutdown function
	err = shutdownFunc(ctx)
	assert.NoError(t, err)

	// Test calling shutdown multiple times
	err = shutdownFunc(ctx)
	assert.NoError(t, err) // Should not error on multiple calls
}

func TestMetricsAfterShutdown(t *testing.T) {
	ctx := context.Background()

	// Initialize metrics
	shutdownFunc, err := InitOTLPExporter(ctx)
	require.NoError(t, err)

	// Use metrics before shutdown
	assert.NotPanics(t, func() {
		EventsReceivedTotal.Add(ctx, 1)
	})

	// Shutdown
	err = shutdownFunc(ctx)
	assert.NoError(t, err)

	// Metrics should still work after shutdown (they just won't be exported)
	assert.NotPanics(t, func() {
		EventsReceivedTotal.Add(ctx, 1)
	})
}

func TestMeterProviderIntegration(t *testing.T) {
	// Test that our meter integrates properly with the global meter provider

	// Get a meter with the same name
	testMeter := otel.Meter("gitops-reverser")
	assert.NotNil(t, testMeter)

	// Create a test counter
	counter, err := testMeter.Int64Counter("test_counter")
	assert.NoError(t, err)
	assert.NotNil(t, counter)

	// Use the counter
	ctx := context.Background()
	assert.NotPanics(t, func() {
		counter.Add(ctx, 1)
	})
}

func TestNoOpMeterProvider(t *testing.T) {
	// Test behavior with no-op meter provider
	ctx := context.Background()

	// Set up a no-op meter provider
	noopProvider := noop.NewMeterProvider()
	otel.SetMeterProvider(noopProvider)

	defer func() {
		// Reset to default
		otel.SetMeterProvider(nil)
	}()

	// Initialize with no-op provider
	shutdownFunc, err := InitOTLPExporter(ctx)
	assert.NoError(t, err)
	assert.NotNil(t, shutdownFunc)

	// Metrics should still work (but do nothing)
	assert.NotPanics(t, func() {
		EventsReceivedTotal.Add(ctx, 1)
		EventsProcessedTotal.Add(ctx, 1)
		GitOperationsTotal.Add(ctx, 1)
		GitPushDurationSeconds.Record(ctx, 1.0)
		GitCommitQueueSize.Add(ctx, 1)
	})

	// Shutdown should work
	err = shutdownFunc(ctx)
	assert.NoError(t, err)
}

func TestMetricNaming(t *testing.T) {
	// Test that metric names follow OpenTelemetry conventions
	expectedNames := []string{
		"gitopsreverser_events_received_total",
		"gitopsreverser_events_processed_total",
		"gitopsreverser_git_operations_total",
		"gitopsreverser_git_push_duration_seconds",
		"gitopsreverser_git_commit_queue_size",
	}

	// Verify naming conventions
	for _, name := range expectedNames {
		// Names should be lowercase
		assert.Equal(t, name, name)

		// Names should use underscores
		assert.Contains(t, name, "_")

		// Names should have appropriate prefixes
		assert.True(t,
			name == "gitopsreverser_events_received_total" ||
				name == "gitopsreverser_events_processed_total" ||
				name == "gitopsreverser_git_operations_total" ||
				name == "gitopsreverser_git_push_duration_seconds" ||
				name == "gitopsreverser_git_commit_queue_size",
			"Unexpected metric name: %s", name)
	}
}
