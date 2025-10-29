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

package git

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// BranchKey uniquely identifies a (GitRepoConfig, Branch) combination.
// This is the unit of worker ownership to prevent merge conflicts.
// Multiple GitDestinations can share the same BranchKey (same repo+branch)
// but write to different baseFolders within that branch.
type BranchKey struct {
	// RepoNamespace is the namespace containing the GitRepoConfig.
	RepoNamespace string
	// RepoName is the name of the GitRepoConfig.
	RepoName string
	// Branch is the Git branch name.
	Branch string
}

// String returns a string representation for logging and debugging.
// Format: "namespace/repo-name/branch".
func (k BranchKey) String() string {
	return fmt.Sprintf("%s/%s/%s", k.RepoNamespace, k.RepoName, k.Branch)
}

// UserInfo contains relevant user information for commit messages.
type UserInfo struct {
	Username string
	UID      string
}

// Event represents a resource change event to be processed by a branch worker.
// Branch comes from the worker context (not stored in event).
// BaseFolder comes from the GitDestination that created this event.
type Event struct {
	// Object is the sanitized Kubernetes object (nil for control events like SEED_SYNC).
	Object *unstructured.Unstructured

	// Identifier contains resource identification information.
	Identifier types.ResourceIdentifier

	// Operation is the admission operation (CREATE, UPDATE, DELETE, SEED_SYNC).
	Operation string

	// UserInfo contains user information for commit messages.
	UserInfo UserInfo

	// BaseFolder is the POSIX-like relative path prefix for this event's files.
	// This comes from the GitDestination that triggered this event.
	// Empty string means write to repository root.
	BaseFolder string
}

// Note: Event types have been moved to internal/events package to avoid import cycles.
// See internal/events/events.go for ClusterStateEvent, RepoStateEvent, and ControlEvent.

// IsControlEvent returns true for control events that don't represent actual resources.
// Control events include SEED_SYNC for orphan detection.
func (e Event) IsControlEvent() bool {
	return e.Operation == "SEED_SYNC"
}
