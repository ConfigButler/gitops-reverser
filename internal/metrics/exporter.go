package metrics

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/global"
)

var (
	meter                  = global.Meter("gitops-reverser")
	EventsReceivedTotal    metric.Int64Counter
	EventsProcessedTotal   metric.Int64Counter
	GitOperationsTotal     metric.Int64Counter
	GitPushDurationSeconds metric.Float64Histogram
	GitCommitQueueSize     metric.Int64UpDownCounter
)

// InitOTLPExporter initializes the OTLP exporter.
func InitOTLPExporter(ctx context.Context) (func(context.Context) error, error) {
	// This is a placeholder for the real OTLP exporter initialization.
	// In a real implementation, we would configure the exporter with an endpoint,
	// authentication, etc.
	fmt.Println("Initializing OTLP exporter")

	var err error
	EventsReceivedTotal, err = meter.Int64Counter("gitopsreverser_events_received_total")
	if err != nil {
		return nil, err
	}
	EventsProcessedTotal, err = meter.Int64Counter("gitopsreverser_events_processed_total")
	if err != nil {
		return nil, err
	}
	GitOperationsTotal, err = meter.Int64Counter("gitopsreverser_git_operations_total")
	if err != nil {
		return nil, err
	}
	GitPushDurationSeconds, err = meter.Float64Histogram("gitopsreverser_git_push_duration_seconds")
	if err != nil {
		return nil, err
	}
	GitCommitQueueSize, err = meter.Int64UpDownCounter("gitopsreverser_git_commit_queue_size")
	if err != nil {
		return nil, err
	}

	return func(ctx context.Context) error {
		fmt.Println("Shutting down OTLP exporter")
		return nil
	}, nil
}
