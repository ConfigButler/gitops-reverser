// SPDX-License-Identifier: Apache-2.0

package v1alpha3

// StreamsStatus is a bounded roll-up of the stream-readiness state for the types a GitTarget
// tracks, or a WatchRule or ClusterWatchRule resolves.
type StreamsStatus struct {
	// Summary is the display-only ready/total ratio, e.g. "3/4".
	//
	// It restates Ready and Total, which the API conventions would normally rule out. It exists
	// solely to feed the Streams printer column: a column can read one JSONPath, not format two.
	// Do not compute anything from it — read ready and total.
	// +optional
	Summary string `json:"summary,omitempty"`

	// Total is how many types are tracked.
	Total int32 `json:"total"`

	// Ready is how many tracked types are Streaming.
	Ready int32 `json:"ready"`

	// Replaying is how many tracked types are still replaying their initial events.
	Replaying int32 `json:"replaying"`

	// Blocked is how many tracked types cannot currently be watched.
	Blocked int32 `json:"blocked"`

	// PendingSample is a bounded sample of types not yet ready.
	// +optional
	// +kubebuilder:validation:MaxItems=5
	PendingSample []string `json:"pendingSample,omitempty"`
}
