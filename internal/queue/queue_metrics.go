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

package queue

import (
	"context"
	"crypto/tls"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// defaultQueueMetricsInterval is how often the metrics reporter polls Redis.
const defaultQueueMetricsInterval = 15 * time.Second

// millisPerSecond converts millisecond stream IDs to whole-second ages.
const millisPerSecond = 1000

// MetricsConfig configures the MetricsReporter.
type MetricsConfig struct {
	Addr       string
	Username   string
	AuthValue  string
	DB         int
	TLSEnabled bool

	// Stream is the canonical audit stream name. Required.
	Stream string
	// Group is the consumer group used to compute lag/pending. Defaults to defaultConsumerGroup.
	Group string
	// DebugStream is the optional debug stream name. Empty disables debug-stream metrics.
	DebugStream string

	// Interval controls poll cadence. Defaults to defaultQueueMetricsInterval when zero.
	Interval time.Duration
}

// MetricsReporter periodically observes Redis stream length, consumer-group lag,
// pending entries, and oldest entry/pending ages, and emits them via OpenTelemetry gauges.
type MetricsReporter struct {
	client      *redis.Client
	stream      string
	group       string
	debugStream string
	interval    time.Duration
	log         logr.Logger
}

// NewMetricsReporter creates a reporter for audit queue health metrics.
func NewMetricsReporter(cfg MetricsConfig, log logr.Logger) (*MetricsReporter, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		return nil, errors.New("redis address is required")
	}
	if strings.TrimSpace(cfg.Stream) == "" {
		return nil, errors.New("stream is required")
	}

	group := strings.TrimSpace(cfg.Group)
	if group == "" {
		group = defaultConsumerGroup
	}

	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultQueueMetricsInterval
	}

	options := &redis.Options{
		Addr:     cfg.Addr,
		Username: cfg.Username,
		Password: cfg.AuthValue,
		DB:       cfg.DB,
	}
	if cfg.TLSEnabled {
		options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	return &MetricsReporter{
		client:      redis.NewClient(options),
		stream:      cfg.Stream,
		group:       group,
		debugStream: strings.TrimSpace(cfg.DebugStream),
		interval:    interval,
		log:         log.WithName("queue-metrics"),
	}, nil
}

// NeedLeaderElection returns false — every replica reports queue metrics for visibility.
func (r *MetricsReporter) NeedLeaderElection() bool {
	return false
}

// Start implements manager.Runnable. It samples queue metrics on r.interval until ctx is cancelled.
func (r *MetricsReporter) Start(ctx context.Context) error {
	r.log.Info("Starting audit queue metrics reporter",
		"stream", r.stream, "group", r.group, "debugStream", r.debugStream, "interval", r.interval)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	r.collect(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.collect(ctx)
		}
	}
}

// collect samples one round of queue metrics. Each call is best-effort: a missing
// stream/group is treated as "no data" rather than an error.
func (r *MetricsReporter) collect(ctx context.Context) {
	r.collectStreamLength(ctx, r.stream, telemetry.AuditQueueStreamLength)
	r.collectOldestEntryAge(ctx, r.stream, telemetry.AuditQueueOldestEntryAgeSeconds)
	r.collectConsumerGroupMetrics(ctx)
	r.collectOldestPendingAge(ctx)

	if r.debugStream != "" {
		r.collectStreamLength(ctx, r.debugStream, telemetry.AuditDebugStreamLength)
	}
}

// collectStreamLength reads XLEN and records it to the given gauge with a stream attribute.
func (r *MetricsReporter) collectStreamLength(ctx context.Context, stream string, gauge metric.Int64Gauge) {
	if gauge == nil {
		return
	}
	length, err := r.client.XLen(ctx, stream).Result()
	if err != nil {
		// A missing stream is normal before any audit event has been enqueued.
		if isMissingStreamErr(err) {
			gauge.Record(ctx, 0, metric.WithAttributes(attribute.String("stream", stream)))
			return
		}
		r.log.V(1).Info("XLEN failed", "stream", stream, "error", err.Error())
		return
	}
	gauge.Record(ctx, length, metric.WithAttributes(attribute.String("stream", stream)))
}

// collectConsumerGroupMetrics reads XINFO GROUPS and records the lag and pending count for r.group.
func (r *MetricsReporter) collectConsumerGroupMetrics(ctx context.Context) {
	groups, err := r.client.XInfoGroups(ctx, r.stream).Result()
	if err != nil {
		if isMissingStreamErr(err) {
			return
		}
		r.log.V(1).Info("XINFO GROUPS failed", "stream", r.stream, "error", err.Error())
		return
	}
	for _, g := range groups {
		if g.Name != r.group {
			continue
		}
		attrs := metric.WithAttributes(
			attribute.String("stream", r.stream),
			attribute.String("group", r.group),
		)
		if telemetry.AuditQueueConsumerLag != nil {
			telemetry.AuditQueueConsumerLag.Record(ctx, g.Lag, attrs)
		}
		if telemetry.AuditQueuePendingEntries != nil {
			telemetry.AuditQueuePendingEntries.Record(ctx, g.Pending, attrs)
		}
		return
	}
}

// collectOldestEntryAge reads the first stream entry via XRANGE and records its age in seconds.
// Stream IDs encode milliseconds since the epoch in the prefix before the dash.
func (r *MetricsReporter) collectOldestEntryAge(ctx context.Context, stream string, gauge metric.Int64Gauge) {
	if gauge == nil {
		return
	}
	entries, err := r.client.XRangeN(ctx, stream, "-", "+", 1).Result()
	if err != nil {
		if isMissingStreamErr(err) {
			gauge.Record(ctx, 0, metric.WithAttributes(attribute.String("stream", stream)))
			return
		}
		r.log.V(1).Info("XRANGE failed", "stream", stream, "error", err.Error())
		return
	}
	if len(entries) == 0 {
		gauge.Record(ctx, 0, metric.WithAttributes(attribute.String("stream", stream)))
		return
	}
	age, ok := streamEntryAgeSeconds(entries[0].ID, time.Now())
	if !ok {
		return
	}
	gauge.Record(ctx, age, metric.WithAttributes(attribute.String("stream", stream)))
}

// collectOldestPendingAge reads XPENDING for the consumer group and records the oldest
// pending entry's age in seconds. Pending is "claimed but unacked", which can differ
// significantly from consumer lag.
func (r *MetricsReporter) collectOldestPendingAge(ctx context.Context) {
	if telemetry.AuditQueueOldestPendingAgeSeconds == nil {
		return
	}
	pending, err := r.client.XPending(ctx, r.stream, r.group).Result()
	if err != nil {
		if isMissingStreamErr(err) || isNoGroupErr(err) {
			return
		}
		r.log.V(1).Info("XPENDING failed", "stream", r.stream, "group", r.group, "error", err.Error())
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("stream", r.stream),
		attribute.String("group", r.group),
	)
	if pending == nil || pending.Count == 0 {
		telemetry.AuditQueueOldestPendingAgeSeconds.Record(ctx, 0, attrs)
		return
	}
	age, ok := streamEntryAgeSeconds(pending.Lower, time.Now())
	if !ok {
		return
	}
	telemetry.AuditQueueOldestPendingAgeSeconds.Record(ctx, age, attrs)
}

// streamEntryAgeSeconds parses a Redis stream ID of the form "<ms>-<seq>" and returns
// the age in whole seconds at now. Returns (0, false) when the ID cannot be parsed.
func streamEntryAgeSeconds(id string, now time.Time) (int64, bool) {
	dash := strings.IndexByte(id, '-')
	msPart := id
	if dash >= 0 {
		msPart = id[:dash]
	}
	ms, err := strconv.ParseInt(msPart, 10, 64)
	if err != nil || ms <= 0 {
		return 0, false
	}
	age := now.UnixMilli() - ms
	if age < 0 {
		return 0, true
	}
	return age / millisPerSecond, true
}

// isMissingStreamErr returns true when the Redis error indicates the stream key does
// not exist. Both real Redis and miniredis use the "ERR no such key" form for XLEN/XINFO.
func isMissingStreamErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no such key") || strings.Contains(msg, "ERR no such key")
}
