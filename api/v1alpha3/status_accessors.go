// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The kstatus status bookkeeping, reachable without knowing which kind you hold.
//
// Every status in this package carries the same two fields — the generation the controller
// observed, and the condition set — and the controllers' shared status session needs both: it
// stamps the generation as the session opens, and rewrites the conditions as gates report. Written
// out per kind, the stamp was five identical lines that a sixth kind could simply forget, and
// forgetting it publishes a status kstatus reads as CURRENT for a spec nobody looked at. These
// accessors let the session do it once. See internal/controller's statusObject, which is the
// interface these satisfy.
//
// CommitRequest is deliberately not here: it does not use that helper (see the comment on
// requeueAfter for why), so an accessor on it would be a method nothing calls.

// SetObservedGeneration records the generation this object's status describes.
func (t *GitTarget) SetObservedGeneration(generation int64) { t.Status.ObservedGeneration = generation }

// StatusConditions returns the condition set to write through.
func (t *GitTarget) StatusConditions() *[]metav1.Condition { return &t.Status.Conditions }

// SetObservedGeneration records the generation this object's status describes.
func (p *GitProvider) SetObservedGeneration(generation int64) {
	p.Status.ObservedGeneration = generation
}

// StatusConditions returns the condition set to write through.
func (p *GitProvider) StatusConditions() *[]metav1.Condition { return &p.Status.Conditions }

// SetObservedGeneration records the generation this object's status describes.
func (p *ClusterProvider) SetObservedGeneration(generation int64) {
	p.Status.ObservedGeneration = generation
}

// StatusConditions returns the condition set to write through.
func (p *ClusterProvider) StatusConditions() *[]metav1.Condition { return &p.Status.Conditions }

// SetObservedGeneration records the generation this object's status describes.
func (r *WatchRule) SetObservedGeneration(generation int64) { r.Status.ObservedGeneration = generation }

// StatusConditions returns the condition set to write through.
func (r *WatchRule) StatusConditions() *[]metav1.Condition { return &r.Status.Conditions }

// SetObservedGeneration records the generation this object's status describes.
func (r *ClusterWatchRule) SetObservedGeneration(generation int64) {
	r.Status.ObservedGeneration = generation
}

// StatusConditions returns the condition set to write through.
func (r *ClusterWatchRule) StatusConditions() *[]metav1.Condition { return &r.Status.Conditions }
