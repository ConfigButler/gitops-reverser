/*
Package metrics provides the OpenTelemetry-based metrics exporter for GitOps Reverser.
It configures Prometheus-compatible metrics collection for monitoring controller operations.
*/
package metrics

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	otelMeter              metric.Meter
	EventsReceivedTotal    metric.Int64Counter
	EventsProcessedTotal   metric.Int64Counter
	GitOperationsTotal     metric.Int64Counter
	GitPushDurationSeconds metric.Float64Histogram
	GitCommitQueueSize     metric.Int64UpDownCounter
)

// InitOTLPExporter initializes the OTLP-to-Prometheus bridge
func InitOTLPExporter(ctx context.Context) (func(context.Context) error, error) {
	fmt.Println("Initializing OTLP exporter")

	// Create a Prometheus exporter that bridges OTLP metrics to Prometheus
	// Configure it to use the controller-runtime registry
	exporter, err := prometheus.New(
		prometheus.WithRegisterer(metrics.Registry),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create Prometheus exporter: %w", err)
	}

	// Create a meter provider with the Prometheus exporter
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	otel.SetMeterProvider(provider)

	// Get the meter from the new provider
	otelMeter = provider.Meter("gitops-reverser")

	// Create all the metrics
	EventsReceivedTotal, err = otelMeter.Int64Counter("gitopsreverser_events_received_total")
	if err != nil {
		return nil, err
	}
	EventsProcessedTotal, err = otelMeter.Int64Counter("gitopsreverser_events_processed_total")
	if err != nil {
		return nil, err
	}
	GitOperationsTotal, err = otelMeter.Int64Counter("gitopsreverser_git_operations_total")
	if err != nil {
		return nil, err
	}
	GitPushDurationSeconds, err = otelMeter.Float64Histogram("gitopsreverser_git_push_duration_seconds")
	if err != nil {
		return nil, err
	}
	GitCommitQueueSize, err = otelMeter.Int64UpDownCounter("gitopsreverser_git_commit_queue_size")
	if err != nil {
		return nil, err
	}

	return func(ctx context.Context) error {
		fmt.Println("Shutting down OTLP exporter")
		return nil
	}, nil
}
