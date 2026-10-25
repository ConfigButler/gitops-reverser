// SPDX-License-Identifier: Apache-2.0

package v1alpha3

// StreamsStatus is a bounded roll-up of the stream-readiness state for the types a GitTarget
// tracks, or a WatchRule or ClusterWatchRule resolves.
type StreamsStatus struct {
	// It restates Ready and Total, which the API conventions would normally rule out. It exists
	// solely to feed the Streams printer column: a column can read one JSONPath, not format two.

	// Summary is ready/total for display, such as "3/4". Read ready and total instead of parsing
	// it.
	// +optional
	Summary string `json:"summary,omitempty"`

	// Total is how many types are watched.
	Total int32 `json:"total"`

	// Ready is how many of them are streaming events.
	Ready int32 `json:"ready"`

	// Replaying is how many of them are still receiving their initial list.
	Replaying int32 `json:"replaying"`

	// Blocked is how many of them cannot be watched right now.
	Blocked int32 `json:"blocked"`

	// PendingSample names up to five types that are not ready yet.
	// +optional
	// +kubebuilder:validation:MaxItems=5
	PendingSample []string `json:"pendingSample,omitempty"`
}
