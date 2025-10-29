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

// Package events provides event types and interfaces for the GitOps Reverser.
package events

import (
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// ClusterStateEvent reports cluster resources for a specific base folder scope.
type ClusterStateEvent struct {
	// Repository identity (for routing)
	RepoName   string
	Branch     string
	BaseFolder string

	// Resources currently in cluster for this base folder
	Resources []types.ResourceIdentifier
}

// RepoStateEvent reports what Kubernetes resources exist in a Git repository branch.
type RepoStateEvent struct {
	// Repository identity
	RepoName   string
	Branch     string
	BaseFolder string

	// Resources found in Git (parsed from YAML files)
	Resources []types.ResourceIdentifier
}

// ControlEventType represents types of control events.
type ControlEventType string

const (
	// RequestClusterState requests cluster snapshot from WatchManager.
	RequestClusterState ControlEventType = "REQUEST_CLUSTER_STATE"
	// RequestRepoState triggers RepoStateEvent emission for specific base folder.
	RequestRepoState ControlEventType = "REQUEST_REPO_STATE"
	// ReconcileResource is a reminder event for individual resources that exist in both cluster and Git.
	ReconcileResource ControlEventType = "RECONCILE_RESOURCE"
)

// ControlEvent represents control events for coordination between components.
type ControlEvent struct {
	Type ControlEventType

	// Repository identity
	RepoName   string
	Branch     string
	BaseFolder string

	// Additional context based on event type
	Resource *types.ResourceIdentifier
}

// EventEmitter interface for emitting reconciliation events.
type EventEmitter interface {
	EmitCreateEvent(resource types.ResourceIdentifier) error
	EmitDeleteEvent(resource types.ResourceIdentifier) error
	EmitReconcileResourceEvent(resource types.ResourceIdentifier) error
}
