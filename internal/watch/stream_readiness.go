// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// StreamState names the per-type watch readiness state.
type StreamState string

const (
	// StreamStateReplaying means the initial-events replay is still being folded.
	StreamStateReplaying StreamState = "Replaying"
	// StreamStateStreaming means the watch is routing live, attributable events.
	StreamStateStreaming StreamState = "Streaming"
	// StreamStateBlocked means the watch cannot currently run.
	StreamStateBlocked StreamState = "Blocked"
)

const (
	StreamReasonInitialReplay          = "InitialReplay"
	StreamReasonResumeReplay           = "ResumeReplay"
	StreamReasonExpiredResourceVersion = "ExpiredResourceVersion"
	StreamReasonWatchError             = "WatchError"
	StreamReasonWatchNotPermitted      = "WatchNotPermitted"
	StreamReasonAllStreamsReady        = "AllStreamsReady"
	StreamReasonReplaying              = "Replaying"
	StreamReasonNoResolvedTypes        = "NoResolvedTypes"
)

const pendingStreamSampleLimit = 5

const (
	streamStateRankStreaming = iota + 1
	streamStateRankReplaying
	streamStateRankBlocked
)

type targetStreamStatus struct {
	state   StreamState
	reason  string
	message string
	at      time.Time
}

// StreamSummary is a bounded status roll-up for a target or rule.
type StreamSummary struct {
	Total         int
	Ready         int
	Replaying     int
	Blocked       int
	Reason        string
	Message       string
	PendingSample []string
	ObservedTime  metav1.Time
}

// Summary returns the display ratio stored in status.streams.summary.
func (s StreamSummary) Summary() string {
	return fmt.Sprintf("%d/%d", s.Ready, s.Total)
}

// StreamsRunning reports whether all resolved streams are Streaming.
func (s StreamSummary) StreamsRunning() bool {
	return s.Total > 0 && s.Ready == s.Total
}

func (m *Manager) markTargetStreamState(
	gitDest types.ResourceReference,
	key targetWatchKey,
	state StreamState,
	reason string,
	message string,
) {
	m.targetWatchesMu.Lock()
	defer m.targetWatchesMu.Unlock()
	m.markTargetStreamStateLocked(gitDest, key, state, reason, message)
}

func (m *Manager) markTargetStreamStateLocked(
	gitDest types.ResourceReference,
	key targetWatchKey,
	state StreamState,
	reason string,
	message string,
) {
	if m.targetStreamStates == nil {
		m.targetStreamStates = map[string]map[targetWatchKey]targetStreamStatus{}
	}
	targetKey := gitDest.Key()
	states := m.targetStreamStates[targetKey]
	if states == nil {
		states = map[targetWatchKey]targetStreamStatus{}
		m.targetStreamStates[targetKey] = states
	}
	states[key] = targetStreamStatus{
		state:   state,
		reason:  reason,
		message: message,
		at:      time.Now(),
	}
}

func (m *Manager) dropTargetStreamStateLocked(gitDest types.ResourceReference) {
	if m.targetStreamStates != nil {
		delete(m.targetStreamStates, gitDest.Key())
	}
}

// StreamSummaryForGitTarget reports the GitTarget stream-readiness roll-up.
func (m *Manager) StreamSummaryForGitTarget(gitDest types.ResourceReference) StreamSummary {
	table, ok := m.watchedTypeTableForGitDest(gitDest)
	if !ok {
		return streamSummaryForTypes(nil, nil, nil)
	}
	specs := targetWatchSpecs(table)
	names := streamDisplayNamesForTable(table)
	return m.streamSummaryForExpectedKeys(gitDest, sortedTargetWatchSpecKeys(specs), names)
}

// StreamSummaryForWatchRule reports stream readiness for one namespaced WatchRule, resolved
// against the source cluster its GitTarget mirrors from.
func (m *Manager) StreamSummaryForWatchRule(rule configv1alpha3.WatchRule) StreamSummary {
	gitDest := types.NewResourceReference(rule.Spec.TargetRef.Name, rule.Namespace)
	reg := m.registryForGitTarget(gitDest)
	m.refreshClusterTypeRegistry(m.cluster(m.clusterIDForGitTarget(gitDest)))
	records := reg.Followable()
	var keys []targetWatchKey
	names := map[schema.GroupVersionResource]string{}
	for _, rr := range rule.Spec.Rules {
		matched := matchFollowableRecords(
			records, rr.APIGroups, rr.APIVersions, rr.Resources, configv1alpha3.ResourceScopeNamespaced)
		for _, rec := range matched {
			key := targetWatchKey{GVR: rec.Identity.GVR, Namespace: rule.Namespace}
			keys = append(keys, key)
			names[rec.Identity.GVR] = streamDisplayName(rec.Identity.GVR)
		}
	}
	return m.streamSummaryForExpectedKeys(gitDest, deduplicateTargetWatchKeys(keys), names)
}

// StreamSummaryForClusterWatchRule reports stream readiness for one ClusterWatchRule, resolved
// against the source cluster its GitTarget mirrors from.
func (m *Manager) StreamSummaryForClusterWatchRule(rule configv1alpha3.ClusterWatchRule) StreamSummary {
	gitDest := types.NewResourceReference(rule.Spec.TargetRef.Name, rule.Spec.TargetRef.Namespace)
	reg := m.registryForGitTarget(gitDest)
	m.refreshClusterTypeRegistry(m.cluster(m.clusterIDForGitTarget(gitDest)))
	records := reg.Followable()
	var keys []targetWatchKey
	names := map[schema.GroupVersionResource]string{}
	for _, rr := range rule.Spec.Rules {
		for _, rec := range matchFollowableRecords(records, rr.APIGroups, rr.APIVersions, rr.Resources, rr.Scope) {
			key := targetWatchKey{GVR: rec.Identity.GVR}
			keys = append(keys, key)
			names[rec.Identity.GVR] = streamDisplayName(rec.Identity.GVR)
		}
	}
	return m.streamSummaryForExpectedKeys(gitDest, deduplicateTargetWatchKeys(keys), names)
}

func (m *Manager) streamSummaryForExpectedKeys(
	gitDest types.ResourceReference,
	expected []targetWatchKey,
	displayNames map[schema.GroupVersionResource]string,
) StreamSummary {
	m.targetWatchesMu.Lock()
	states := copyTargetStreamStates(m.targetStreamStates[gitDest.Key()])
	m.targetWatchesMu.Unlock()
	return streamSummaryForTypes(expected, states, displayNames)
}

func streamSummaryForTypes(
	expected []targetWatchKey,
	states map[targetWatchKey]targetStreamStatus,
	displayNames map[schema.GroupVersionResource]string,
) StreamSummary {
	byGVR := streamStatusesByGVR(expected, states)
	out, blockedNames, replayingNames := streamSummaryCounts(byGVR, displayNames)
	sort.Strings(blockedNames)
	sort.Strings(replayingNames)
	out.PendingSample = pendingStreamSample(blockedNames, replayingNames)
	out.Reason, out.Message = streamSummaryReasonAndMessage(out, byGVR, blockedNames, replayingNames)
	return out
}

func streamStatusesByGVR(
	expected []targetWatchKey,
	states map[targetWatchKey]targetStreamStatus,
) map[schema.GroupVersionResource]targetStreamStatus {
	byGVR := map[schema.GroupVersionResource]targetStreamStatus{}
	for _, key := range deduplicateTargetWatchKeys(expected) {
		status, ok := states[key]
		if !ok {
			status = targetStreamStatus{state: StreamStateReplaying, reason: StreamReasonInitialReplay}
		}
		current, seen := byGVR[key.GVR]
		if !seen || strongerStreamStatus(status, current) {
			byGVR[key.GVR] = status
		}
	}
	return byGVR
}

func streamSummaryCounts(
	byGVR map[schema.GroupVersionResource]targetStreamStatus,
	displayNames map[schema.GroupVersionResource]string,
) (StreamSummary, []string, []string) {
	out := StreamSummary{Total: len(byGVR), ObservedTime: metav1.Now()}
	var blockedNames, replayingNames []string
	for gvr, status := range byGVR {
		name := displayNames[gvr]
		if name == "" {
			name = streamDisplayName(gvr)
		}
		switch status.state {
		case StreamStateStreaming:
			out.Ready++
		case StreamStateBlocked:
			out.Blocked++
			blockedNames = append(blockedNames, name)
		case StreamStateReplaying:
			out.Replaying++
			replayingNames = append(replayingNames, name)
		default:
			out.Replaying++
			replayingNames = append(replayingNames, name)
		}
	}
	return out, blockedNames, replayingNames
}

func pendingStreamSample(blockedNames, replayingNames []string) []string {
	sample := append([]string{}, blockedNames...)
	sample = append(sample, replayingNames...)
	if len(sample) > pendingStreamSampleLimit {
		return sample[:pendingStreamSampleLimit]
	}
	return sample
}

func streamSummaryReasonAndMessage(
	out StreamSummary,
	byGVR map[schema.GroupVersionResource]targetStreamStatus,
	blockedNames, replayingNames []string,
) (string, string) {
	switch {
	case out.Blocked > 0:
		return blockedReason(byGVR), streamSummaryMessage(out, "blocked", blockedNames)
	case out.Replaying > 0:
		return StreamReasonReplaying, streamSummaryMessage(out, "replaying", replayingNames)
	case out.Total == 0:
		return StreamReasonNoResolvedTypes, "0/0 streams running; no resolved resource types"
	default:
		return StreamReasonAllStreamsReady, fmt.Sprintf("%d/%d streams running", out.Ready, out.Total)
	}
}

func strongerStreamStatus(candidate, current targetStreamStatus) bool {
	return streamStateRank(candidate.state) > streamStateRank(current.state)
}

func streamStateRank(state StreamState) int {
	switch state {
	case StreamStateBlocked:
		return streamStateRankBlocked
	case StreamStateReplaying:
		return streamStateRankReplaying
	case StreamStateStreaming:
		return streamStateRankStreaming
	default:
		return streamStateRankReplaying
	}
}

func blockedReason(statuses map[schema.GroupVersionResource]targetStreamStatus) string {
	reason := StreamReasonWatchError
	for _, status := range statuses {
		if status.state != StreamStateBlocked {
			continue
		}
		if status.reason == StreamReasonWatchNotPermitted {
			return StreamReasonWatchNotPermitted
		}
		if status.reason != "" {
			reason = status.reason
		}
	}
	return reason
}

func streamSummaryMessage(summary StreamSummary, label string, names []string) string {
	msg := fmt.Sprintf("%d/%d streams running; %d %s", summary.Ready, summary.Total,
		summary.Total-summary.Ready, label)
	if len(names) == 0 {
		return msg
	}
	if len(names) > pendingStreamSampleLimit {
		names = names[:pendingStreamSampleLimit]
	}
	return msg + " (" + strings.Join(names, ", ") + ")"
}

func streamDisplayNamesForTable(table WatchedTypeTable) map[schema.GroupVersionResource]string {
	out := map[schema.GroupVersionResource]string{}
	for _, wt := range table.Types {
		out[wt.GVR] = streamDisplayName(wt.GVR)
	}
	return out
}

func streamDisplayName(gvr schema.GroupVersionResource) string {
	if gvr.Group == "" {
		return gvr.Resource
	}
	return gvr.Resource + "." + gvr.Group
}

func copyTargetStreamStates(in map[targetWatchKey]targetStreamStatus) map[targetWatchKey]targetStreamStatus {
	out := make(map[targetWatchKey]targetStreamStatus, len(in))
	for key, status := range in {
		out[key] = status
	}
	return out
}

func deduplicateTargetWatchKeys(keys []targetWatchKey) []targetWatchKey {
	seen := map[targetWatchKey]struct{}{}
	out := make([]targetWatchKey, 0, len(keys))
	for _, key := range keys {
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GVR.String() == out[j].GVR.String() {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].GVR.String() < out[j].GVR.String()
	})
	return out
}
