// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"sync"

	"go.opentelemetry.io/otel/attribute"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// watch_streams_open counts the object-state watch SESSIONS this operator currently holds open
// against each source cluster.
//
// It is a different question from watch_types, and neither answers the other. A GitTarget watching
// ConfigMaps in three namespaces resolves ONE type and opens THREE watches: the type gauge tells an
// operator what their rules cover, and this one tells a cluster admin what it costs their API
// server. That second question had no answer at all.
//
// Counted at the open and close of a session, read at scrape time. A connection attempt that failed
// is not a session, and a stream sitting in reconnect backoff has none open, so neither is counted:
// the number is what an API server would see in its own watch count, not what this operator intends
// to hold. Internal controller and discovery watches are outside it.

// openWatchKey is one series: a source cluster and the GitTarget whose rules opened the session.
type openWatchKey struct {
	sourceCluster string
	namespace     string
	name          string
}

// trackOpenWatch records that a watch session is open and returns the release for it. Call it after
// the watch opens successfully, and defer the release beside the watch's own Stop.
func (m *Manager) trackOpenWatch(gitDest types.ResourceReference) func() {
	key := openWatchKey{
		sourceCluster: m.clusterIDForGitTarget(gitDest),
		namespace:     gitDest.Namespace,
		name:          gitDest.Name,
	}

	m.openWatchesMu.Lock()
	if m.openWatches == nil {
		m.openWatches = map[openWatchKey]int{}
	}
	m.openWatches[key]++
	m.openWatchesMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			m.openWatchesMu.Lock()
			defer m.openWatchesMu.Unlock()
			if m.openWatches[key] <= 1 {
				// Delete rather than leave a zero: a source cluster this operator no longer watches
				// should stop publishing, not report a standing zero forever.
				delete(m.openWatches, key)
				return
			}
			m.openWatches[key]--
		})
	}
}

// installOpenWatchGaugeSource publishes the open-session count as a scrape-time source.
func (m *Manager) installOpenWatchGaugeSource() {
	telemetry.SetGaugeSource(telemetry.GaugeWatchStreamsOpen, m.openWatchSamples)
}

// clearOpenWatchGaugeSource removes the callback so it cannot outlive the manager.
func (m *Manager) clearOpenWatchGaugeSource() {
	telemetry.SetGaugeSource(telemetry.GaugeWatchStreamsOpen, nil)
}

// openWatchSamples reads the live count. It takes only its own short-lived mutex, which no watch
// session holds across I/O.
func (m *Manager) openWatchSamples() []telemetry.GaugeSample {
	m.openWatchesMu.Lock()
	defer m.openWatchesMu.Unlock()

	samples := make([]telemetry.GaugeSample, 0, len(m.openWatches))
	for key, count := range m.openWatches {
		samples = append(samples, telemetry.GaugeSample{
			Value: int64(count),
			Attrs: []attribute.KeyValue{
				attribute.String("source_cluster", key.sourceCluster),
				attribute.String("gittarget_namespace", key.namespace),
				attribute.String("gittarget_name", key.name),
			},
		})
	}
	return samples
}
