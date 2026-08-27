// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"

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
}

// Summary returns the display ratio stored in status.streams.summary.
func (s StreamSummary) Summary() string {
	return fmt.Sprintf("%d/%d", s.Ready, s.Total)
}

// StreamsRunning reports whether all resolved streams are Streaming.
func (s StreamSummary) StreamsRunning() bool {
	return s.Total > 0 && s.Ready == s.Total
}

// markTargetStreamState is a stream goroutine's report of its own readiness, recorded under its
// CELL rather than under the (versioned) key the stream happens to run at. The rule-level roll-up
// resolves what it expects from the type registry, which serves one record per version, while the
// declared stream set runs one stream per cell. Keyed by version, a rule matching two served
// versions of one resource would expect a stream that by construction never exists, and would
// report permanently not-ready while its stream ran perfectly.
//
// It is a report, not a write to shared state a lock is being borrowed for: the stream posts what
// it observed and the published snapshot moves, so a goroutine unwinding after its context was
// cancelled never contends for the lock the cancellation was issued under.
func (m *Manager) markTargetStreamState(
	gitDest types.ResourceReference,
	cell types.CellKey,
	state StreamState,
	reason string,
	message string,
) {
	changed := m.mutateWatchPlane(func(s *watchPlaneState) bool {
		return setStreamState(s, gitDest.Key(), cell, targetStreamStatus{
			state:   state,
			reason:  reason,
			message: message,
		})
	})
	// A cell reaching Streaming is the last thing that has to happen before this target and every
	// rule pointing at it can honestly say StreamsRunning=True, so the transition is worth an
	// event. Without one the data plane converges in about two seconds and the status follows up
	// to ten seconds later, on RequeueStreamSettleInterval, having learned nothing in between.
	//
	// On a CHANGE only. The data plane reports readiness continuously, and an event per report
	// would enqueue every rule of a target on every watch event it handles.
	//
	// "Change" is the whole status, message included, not just the state: the message is published
	// on the rule's condition, so a stream that stays Blocked for a new reason has moved something
	// a reader sees. A stream flapping between distinct error messages therefore does emit per
	// message — bounded by the non-blocking sends below, and the alternative is a condition that
	// keeps describing the first failure.
	if changed {
		m.enqueueStreamStateChange(gitDest)
	}
}

// setStreamState records one cell's status and reports whether it moved.
func setStreamState(
	s *watchPlaneState,
	targetKey string,
	cell types.CellKey,
	status targetStreamStatus,
) bool {
	states := s.streams[targetKey]
	if states == nil {
		states = map[types.CellKey]targetStreamStatus{}
		s.streams[targetKey] = states
	}
	if prior, had := states[cell]; had && prior == status {
		return false
	}
	states[cell] = status
	return true
}

// StreamSummaryForGitTarget reports the GitTarget stream-readiness roll-up.
func (m *Manager) StreamSummaryForGitTarget(gitDest types.ResourceReference) StreamSummary {
	table, ok := m.watchedTypeTableForGitDest(gitDest)
	if !ok {
		return streamSummaryForTypes(nil, nil, nil)
	}
	specs := targetWatchSpecs(table)
	names := streamDisplayNamesForTable(table)
	return m.streamSummaryForExpectedKeys(gitDest, cellsForWatchKeys(sortedTargetWatchSpecKeys(specs)), names)
}

// StreamSummaryForWatchRule reports stream readiness for one namespaced WatchRule, resolved
// against the source cluster its GitTarget mirrors from.
//
// It reads the COMPILED rule, not the spec. A rule's watched namespaces can no longer be derived
// from its spec at all: a `sourceNamespace: "*"` item's set exists only after resolution against
// the GitTarget's policy and the source-cluster snapshot. Rebuilding the keys from the spec would
// look for streams under keys that were never opened, so a perfectly healthy wildcard rule would
// report permanently not-ready while its streams run — the same class of bug the singular field
// already hit once, one level up.
//
// A rule that is not compiled expects no streams, which is correct: the gate refused it, or the
// store has not been seeded yet.
func (m *Manager) StreamSummaryForWatchRule(rule configv1alpha3.WatchRule) StreamSummary {
	// The GitTarget is in the rule's OWN namespace (targetRef is a LocalTargetReference), but the
	// streams are keyed on the namespaces being WATCHED.
	gitDest := types.NewResourceReference(rule.Spec.TargetRef.Name, rule.Namespace)
	if m.RuleStore == nil {
		return streamSummaryForTypes(nil, nil, nil)
	}
	compiled, ok := m.RuleStore.GetWatchRule(
		k8stypes.NamespacedName{Name: rule.Name, Namespace: rule.Namespace})
	if !ok {
		return streamSummaryForTypes(nil, nil, nil)
	}

	reg := m.registryForGitTarget(gitDest)
	m.refreshClusterTypeRegistry(m.cluster(m.clusterIDForGitTarget(gitDest)))
	records := reg.Followable()
	var cells []types.CellKey
	names := map[schema.GroupResource]string{}
	for _, rr := range compiled.ResourceRules {
		matched := matchFollowableRecords(
			records, rr.APIGroups, rr.APIVersions, rr.Resources, configv1alpha3.ResourceScopeNamespaced)
		for _, rec := range matched {
			for _, namespace := range rr.SourceNamespaces {
				cells = append(cells, types.CellKeyFor(rec.Identity.GVR, namespace))
			}
			names[rec.Identity.GVR.GroupResource()] = streamDisplayName(rec.Identity.GVR)
		}
	}
	return m.streamSummaryForExpectedKeys(gitDest, deduplicateCells(cells), names)
}

// StreamSummaryForClusterWatchRule reports stream readiness for one ClusterWatchRule, resolved
// against the source cluster its GitTarget mirrors from. It always matches cluster-scoped records,
// because a ClusterWatchRule is cluster-scope-only.
func (m *Manager) StreamSummaryForClusterWatchRule(rule configv1alpha3.ClusterWatchRule) StreamSummary {
	gitDest := types.NewResourceReference(rule.Spec.TargetRef.Name, rule.Spec.TargetRef.Namespace)
	reg := m.registryForGitTarget(gitDest)
	m.refreshClusterTypeRegistry(m.cluster(m.clusterIDForGitTarget(gitDest)))
	records := reg.Followable()
	var cells []types.CellKey
	names := map[schema.GroupResource]string{}
	for _, rr := range rule.Spec.Rules {
		matched := matchFollowableRecords(
			records, rr.APIGroups, rr.APIVersions, rr.Resources, configv1alpha3.ResourceScopeCluster)
		for _, rec := range matched {
			cells = append(cells, types.CellKeyFor(rec.Identity.GVR, ""))
			names[rec.Identity.GVR.GroupResource()] = streamDisplayName(rec.Identity.GVR)
		}
	}
	return m.streamSummaryForExpectedKeys(gitDest, deduplicateCells(cells), names)
}

func (m *Manager) streamSummaryForExpectedKeys(
	gitDest types.ResourceReference,
	expected []types.CellKey,
	displayNames map[schema.GroupResource]string,
) StreamSummary {
	return streamSummaryForTypes(expected, m.watchPlane().streams[gitDest.Key()], displayNames)
}

func streamSummaryForTypes(
	expected []types.CellKey,
	states map[types.CellKey]targetStreamStatus,
	displayNames map[schema.GroupResource]string,
) StreamSummary {
	byGVR := streamStatusesByType(expected, states)
	out, blockedNames, replayingNames := streamSummaryCounts(byGVR, displayNames)
	sort.Strings(blockedNames)
	sort.Strings(replayingNames)
	out.PendingSample = pendingStreamSample(blockedNames, replayingNames)
	out.Reason, out.Message = streamSummaryReasonAndMessage(out, byGVR, blockedNames, replayingNames)
	return out
}

// streamStatusesByType reduces the per-cell states to one row per TYPE: a rule watching one
// resource in three namespaces reports one stream, in its weakest state, which is the ratio
// users have always seen in status.
func streamStatusesByType(
	expected []types.CellKey,
	states map[types.CellKey]targetStreamStatus,
) map[schema.GroupResource]targetStreamStatus {
	byType := map[schema.GroupResource]targetStreamStatus{}
	for _, cell := range deduplicateCells(expected) {
		status, ok := states[cell]
		if !ok {
			status = targetStreamStatus{state: StreamStateReplaying, reason: StreamReasonInitialReplay}
		}
		gr := schema.GroupResource{Group: cell.Group, Resource: cell.Resource}
		current, seen := byType[gr]
		if !seen || strongerStreamStatus(status, current) {
			byType[gr] = status
		}
	}
	return byType
}

func streamSummaryCounts(
	byType map[schema.GroupResource]targetStreamStatus,
	displayNames map[schema.GroupResource]string,
) (StreamSummary, []string, []string) {
	out := StreamSummary{Total: len(byType)}
	var blockedNames, replayingNames []string
	for gr, status := range byType {
		name := displayNames[gr]
		if name == "" {
			name = groupResourceDisplayName(gr)
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
	byType map[schema.GroupResource]targetStreamStatus,
	blockedNames, replayingNames []string,
) (string, string) {
	switch {
	case out.Blocked > 0:
		return blockedReason(byType), streamSummaryMessage(out, "blocked", blockedNames)
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

func blockedReason(statuses map[schema.GroupResource]targetStreamStatus) string {
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

func streamDisplayNamesForTable(table WatchedTypeTable) map[schema.GroupResource]string {
	out := map[schema.GroupResource]string{}
	for _, wt := range table.Types {
		out[wt.GVR.GroupResource()] = streamDisplayName(wt.GVR)
	}
	return out
}

func streamDisplayName(gvr schema.GroupVersionResource) string {
	return groupResourceDisplayName(gvr.GroupResource())
}

func groupResourceDisplayName(gr schema.GroupResource) string {
	if gr.Group == "" {
		return gr.Resource
	}
	return gr.Resource + "." + gr.Group
}

// cellsForWatchKeys projects a declared stream set onto the cells it covers.
func cellsForWatchKeys(keys []targetWatchKey) []types.CellKey {
	out := make([]types.CellKey, 0, len(keys))
	for _, key := range keys {
		out = append(out, key.Cell())
	}
	return out
}

func deduplicateCells(cells []types.CellKey) []types.CellKey {
	seen := map[types.CellKey]struct{}{}
	out := make([]types.CellKey, 0, len(cells))
	for _, cell := range cells {
		if _, ok := seen[cell]; ok {
			continue
		}
		seen[cell] = struct{}{}
		out = append(out, cell)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}
