// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// prometheusQuantile estimates quantile q the way PromQL's histogram_quantile does: find the bucket
// holding the q-th observation and interpolate linearly across it, starting the first bucket at 0.
func prometheusQuantile(q float64, dp metricdata.HistogramDataPoint[float64]) float64 {
	rank := q * float64(dp.Count)
	var below uint64
	for i, inBucket := range dp.BucketCounts {
		if float64(below+inBucket) < rank {
			below += inBucket
			continue
		}
		if i == len(dp.Bounds) { // the +Inf bucket reports its lower bound
			return dp.Bounds[len(dp.Bounds)-1]
		}
		lower := 0.0
		if i > 0 {
			lower = dp.Bounds[i-1]
		}
		return lower + (dp.Bounds[i]-lower)*(rank-float64(below))/float64(inBucket)
	}
	return math.NaN()
}

// A window closes just after its deadline. The p95 that interpreting-metrics.md queries must read
// such a window at its timer, not at the next wide bucket boundary above it.
func TestCommitWindowBuckets_AWindowClosedJustPastADefaultTimerReadsAtThatTimer(t *testing.T) {
	timers := map[string]time.Duration{
		"request maxDuration":   2 * time.Second,
		"target idleTimeout":    5 * time.Second,
		"target maxDuration":    time.Minute,
		"request ceiling (max)": 5 * time.Minute,
	}
	for name, timer := range timers {
		for _, late := range []time.Duration{time.Millisecond, 100 * time.Millisecond} {
			t.Run(fmt.Sprintf("%s closed %s late", name, late), func(t *testing.T) {
				reader, err := InitTestExporter()
				require.NoError(t, err)
				observed := (timer + late).Seconds()
				for range 100 {
					GitCommitWindowDurationSeconds.Record(context.Background(), observed,
						metric.WithAttributes(attribute.String("gittarget_name", "team-a")))
				}

				data, found := collectMetric(reader, "gitopsreverser_git_commit_window_duration_seconds")
				require.True(t, found)
				hist, ok := data.(metricdata.Histogram[float64])
				require.True(t, ok)
				require.Len(t, hist.DataPoints, 1)

				p95 := prometheusQuantile(0.95, hist.DataPoints[0])
				assert.GreaterOrEqual(t, p95, timer.Seconds())
				assert.LessOrEqual(t, p95, 1.1*timer.Seconds(),
					"a window %s past its %s timer reads as p95 %.3fs", late, timer, p95)
			})
		}
	}
}
