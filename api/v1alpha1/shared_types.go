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

package v1alpha1

// NamespacedName represents a reference to a namespaced resource.
// Used by both WatchRule and ClusterWatchRule to reference GitRepoConfig.
type NamespacedName struct {
	// Name of the GitRepoConfig.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace containing the GitRepoConfig.
	// For WatchRule: Optional, defaults to WatchRule's namespace if not specified.
	// For ClusterWatchRule: Required, must be explicitly specified.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// PushStrategy defines the rules for when to push commits.
type PushStrategy struct {
	// Interval is the maximum time to wait before pushing queued commits.
	// Defaults to "1m".
	// +optional
	Interval *string `json:"interval,omitempty"`

	// MaxCommits is the maximum number of commits to queue before pushing.
	// Defaults to 20.
	// +optional
	MaxCommits *int `json:"maxCommits,omitempty"`
}
