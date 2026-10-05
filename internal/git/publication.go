// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// PublicationStatus is a branch worker's report of its publication, for every GitTarget on the
// branch and every save it holds. The zero value is a branch with nothing failing.
type PublicationStatus struct {
	// Failing is set while a failed attempt waits for its retry.
	Failing bool
	// Since is when this run of failures began. It holds until a publication lands.
	Since time.Time
	// Cause is what the last attempt met.
	Cause string
	// ParentUnavailable is set when the cause is the parent branch a new write branch is created
	// from: the parent status already says so, more precisely.
	ParentUnavailable bool
	// IntakePaused is set while the branch refuses new writes, saves and resyncs.
	IntakePaused bool
}

// Message says what the report means, in words that change only when the report does.
func (p PublicationStatus) Message() string {
	if !p.Failing {
		return ""
	}
	message := fmt.Sprintf("Cannot publish to the Git remote since %s: %s. The kept writes are retried on schedule",
		p.Since.UTC().Format(time.RFC3339), p.Cause)
	if p.IntakePaused {
		message += "; intake of new changes is paused until a push lands"
	}
	return message
}

// Publication is the worker's current report. It reads memory only, and costs no connection.
func (w *BranchWorker) Publication() PublicationStatus {
	if report := w.publication.Load(); report != nil {
		return *report
	}
	return PublicationStatus{}
}

// publicationBacklog is what the loop keeps until a push lands, published for the gauges. Each
// field is an atomic so a scrape never waits on the loop. The timestamps are Unix seconds, zero
// when there is nothing to date.
type publicationBacklog struct {
	retainedBytes  atomic.Int64
	retainedWrites atomic.Int64
	oldestWrite    atomic.Int64
	nextRetry      atomic.Int64
}

// publishPublication publishes the loop's publication report and backlog. The report is replaced,
// and the GitTargets on the branch told, only when what it says changes: a retry that fails the
// same way again leaves it alone, so no status is written per attempt. When the next retry is due
// is in the backlog for the gauges, and deliberately not in the report.
func (l *branchWorkerEventLoop) publishPublication(held int64) {
	backlog := &l.w.backlog
	backlog.retainedBytes.Store(held)
	backlog.retainedWrites.Store(int64(len(l.pendingWrites)))
	backlog.oldestWrite.Store(0)
	if len(l.pendingWrites) > 0 {
		backlog.oldestWrite.Store(l.pendingWrites[0].decidedAt.Unix())
	}
	backlog.nextRetry.Store(0)
	if l.retry.pending() {
		backlog.nextRetry.Store(l.retry.due.Unix())
	}

	report := PublicationStatus{}
	if l.retry.pending() {
		report = PublicationStatus{
			Failing:      true,
			Since:        l.retry.since,
			IntakePaused: l.w.intake.pausedC() != nil,
		}
		if cause := l.retry.cause; cause != nil {
			report.Cause = cause.Error()
			report.ParentUnavailable = isParentUnavailable(cause)
		}
	}
	if report == l.w.Publication() {
		return
	}
	l.w.publication.Store(&report)
	if l.w.publicationReporter != nil {
		l.w.publicationReporter()
	}
}

// recordMaterializationFailure counts an attempt to commit decided writes that stopped before any
// push: the remote could not be reached, or the parent branch is missing.
func (w *BranchWorker) recordMaterializationFailure(err error) {
	if telemetry.GitMaterializationFailuresTotal == nil {
		return
	}
	reason := "unreachable"
	if isParentUnavailable(err) {
		reason = "parent_unavailable"
	}
	telemetry.GitMaterializationFailuresTotal.Add(context.Background(), 1,
		metric.WithAttributes(w.providerAttrs(attribute.String("reason", reason))...))
}

// setPublicationGaugeSources installs the scrape-time sources of the publication gauges: one
// sample per live worker, read from the backlog its loop last published.
func (m *WorkerManager) setPublicationGaugeSources() {
	source := func(read func(*BranchWorker) (int64, bool)) telemetry.GaugeSource {
		return func() []telemetry.GaugeSample {
			var samples []telemetry.GaugeSample
			for _, worker := range m.liveWorkers() {
				if value, ok := read(worker); ok {
					samples = append(samples, telemetry.GaugeSample{Value: value, Attrs: worker.providerAttrs()})
				}
			}
			return samples
		}
	}
	always := func(value int64) (int64, bool) { return value, true }
	dated := func(value int64) (int64, bool) { return value, value != 0 }
	telemetry.SetGaugeSource(telemetry.GaugeGitRetainedBytes, source(func(w *BranchWorker) (int64, bool) {
		return always(w.backlog.retainedBytes.Load())
	}))
	telemetry.SetGaugeSource(telemetry.GaugeGitRetainedWrites, source(func(w *BranchWorker) (int64, bool) {
		return always(w.backlog.retainedWrites.Load())
	}))
	telemetry.SetGaugeSource(telemetry.GaugeGitIntakePaused, source(func(w *BranchWorker) (int64, bool) {
		if w.IntakePaused() != nil {
			return 1, true
		}
		return 0, true
	}))
	telemetry.SetGaugeSource(telemetry.GaugeGitOldestRetainedWrite, source(func(w *BranchWorker) (int64, bool) {
		return dated(w.backlog.oldestWrite.Load())
	}))
	telemetry.SetGaugeSource(telemetry.GaugeGitNextRetry, source(func(w *BranchWorker) (int64, bool) {
		return dated(w.backlog.nextRetry.Load())
	}))
}

// publicationGauges are the gauges setPublicationGaugeSources installs.
//
//nolint:gochecknoglobals // a fixed list
var publicationGauges = []string{
	telemetry.GaugeGitRetainedBytes,
	telemetry.GaugeGitRetainedWrites,
	telemetry.GaugeGitIntakePaused,
	telemetry.GaugeGitOldestRetainedWrite,
	telemetry.GaugeGitNextRetry,
}

// publicationEventBuffer bounds the GitTarget events a burst of report changes can queue. A full
// buffer drops the arriving event: the GitTarget's own requeue reads the report later.
const publicationEventBuffer = 256

// PublicationEvents carries a GitTarget whose branch worker's publication report changed, for the
// GitTarget controller to re-project its status within one reconcile instead of at its next
// requeue. Nil for a manager not built by NewWorkerManager.
func (m *WorkerManager) PublicationEvents() <-chan event.GenericEvent { return m.publicationEvents }

// notifyPublication tells every GitTarget on the branch that its worker's report changed. It reads
// the GitTargets from the cache, and never blocks: the worker's loop calls it, by way of a
// goroutine, and a status that lags is better than a loop that waits for a controller.
func (m *WorkerManager) notifyPublication(ctx context.Context, key BranchKey) {
	if m.Client == nil || m.publicationEvents == nil {
		return
	}
	var targets configv1alpha3.GitTargetList
	if err := m.Client.List(ctx, &targets, client.InNamespace(key.RepoNamespace)); err != nil {
		m.Log.V(1).Info("Cannot list the GitTargets of a branch to report its publication",
			"branch", key.String(), "error", err.Error())
		return
	}
	for i := range targets.Items {
		target := &targets.Items[i]
		if target.Spec.GitProviderRef.Name != key.RepoName || target.Spec.Branch != key.Branch {
			continue
		}
		select {
		case m.publicationEvents <- event.GenericEvent{Object: target}:
		default:
		}
	}
}
