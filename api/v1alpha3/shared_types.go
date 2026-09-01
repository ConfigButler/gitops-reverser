// SPDX-License-Identifier: Apache-2.0

package v1alpha3

// PushStrategy is the REMOVED GitProvider.spec.push shape. It is retained only so that a manifest
// still setting it is rejected with a message naming the replacement; see the field's own
// documentation on GitProviderSpec.
//
// Deprecated: commit batching moved to GitTarget.spec.commit.window. Removed at v1alpha4.
type PushStrategy struct {
	// CommitWindow is the rolling silence window used to coalesce events into a single commit per
	// author. It moved to GitTarget.spec.commit.window; setting it here is rejected.
	// +optional
	CommitWindow *string `json:"commitWindow,omitempty"`
}
