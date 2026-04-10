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
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

const (
	// DefaultCommitterName matches the default operator identity in Git history.
	DefaultCommitterName = "GitOps Reverser"
	// DefaultCommitterEmail matches the default operator email in Git history.
	DefaultCommitterEmail = "noreply@configbutler.ai"
	// DefaultCommitMessageTemplate reproduces the current per-event commit message shape.
	DefaultCommitMessageTemplate = "[{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Name}}"
	// DefaultBatchCommitMessageTemplate is the default atomic batch commit message shape.
	DefaultBatchCommitMessageTemplate = "reconcile: sync {{.Count}} resources"
)

// CommitFile represents a single file to be committed.
type CommitFile struct {
	Path    string
	Content []byte
}

// BranchInfo contains information about a Git branch.
type BranchInfo struct {
	ShortName string // e.g., "main"
	Sha       string // commit hash, normally the tip of the default branch. But will be empty ("") for an unborn branch that is going to be orphaned branch (if the default branch does not exist)
	Unborn    bool   // Is true for branches that don't have commits yet: only HEAD is configured to it
}

// RepoInfo represents high-level repository information.
type RepoInfo struct {
	DefaultBranch     *BranchInfo
	RemoteBranchCount int
}

// PullReport provides detailed pull operation results.
type PullReport struct {
	ExistsOnRemote  bool // Branch exists on remote
	HEAD            BranchInfo
	IncomingChanges bool // SHA changed, requiring resource-level reconcile
}

// WriteEventsResult provides detailed writeEvents operation results.
type WriteEventsResult struct {
	CommitsCreated int           // Number of successfully pushed commits (0 if no changes)
	LastHash       string        // SHA of the last created event commit
	ConflictPulls  []*PullReport // List of PullReports: one for each conflict resolution attempt
	Failures       int           // Number of failures while attempting to push commits (0 in ideal situation)
}

// BranchKey uniquely identifies a (GitProvider, Branch) combination.
// This is the unit of worker ownership to prevent merge conflicts.
// Multiple GitTargets can share the same BranchKey (same provider+branch)
// but write to different paths within that branch.
type BranchKey struct {
	// RepoNamespace is the namespace containing the GitProvider.
	RepoNamespace string
	// RepoName is the name of the GitProvider.
	RepoName string
	// Branch is the Git branch name.
	Branch string
}

// String returns a string representation for logging and debugging.
// Format: "namespace/provider-name/branch".
func (k BranchKey) String() string {
	return fmt.Sprintf("%s/%s/%s", k.RepoNamespace, k.RepoName, k.Branch)
}

// UserInfo contains relevant user information for commit messages.
type UserInfo struct {
	Username string
	UID      string
}

// CommitMode defines how a write request should be committed.
type CommitMode string

const (
	// CommitModePerEvent creates one commit per event in the request.
	CommitModePerEvent CommitMode = "per_event"
	// CommitModeAtomic creates one commit for all events in the request.
	CommitModeAtomic CommitMode = "atomic"
)

// WriteRequest is the unit of work queued and written by the BranchWorker.
type WriteRequest struct {
	Events             []Event
	CommitMessage      string
	CommitConfig       *CommitConfig
	GitTargetName      string
	GitTargetNamespace string
	BootstrapOptions   pathBootstrapOptions
	CommitMode         CommitMode
}

// ReconcileBatch is a backward-compatible alias for a write request emitted by reconciliation.
type ReconcileBatch = WriteRequest

// WorkItem is the unit of work in the BranchWorker queue.
type WorkItem struct {
	Request *WriteRequest
}

// Event represents a resource change event to be processed by a branch worker.
// Branch comes from the worker context (not stored in event).
// Path comes from the GitTarget that created this event.
type Event struct {
	// Object is the sanitized Kubernetes object.
	Object *unstructured.Unstructured

	// Identifier contains resource identification information.
	Identifier types.ResourceIdentifier

	// Operation is the admission operation (CREATE, UPDATE, DELETE).
	Operation string

	// UserInfo contains user information for commit messages.
	UserInfo UserInfo

	// Path is the POSIX-like relative path prefix for this event's files.
	// This comes from the GitTarget that triggered this event.
	// Empty string means write to repository root.
	Path string

	// GitTargetName is the target owning this event.
	GitTargetName string

	// GitTargetNamespace is the namespace of the target owning this event.
	GitTargetNamespace string

	// BootstrapOptions controls path-scoped bootstrap file staging for this event.
	BootstrapOptions pathBootstrapOptions
}

// CommitConfig is the resolved commit behavior used by the git writer.
type CommitConfig struct {
	Committer CommitterConfig
	Message   CommitMessageConfig
}

// CommitterConfig defines the operator identity used as the git committer.
type CommitterConfig struct {
	Name  string
	Email string
}

// CommitMessageConfig contains the resolved per-event and batch templates.
type CommitMessageConfig struct {
	Template      string
	BatchTemplate string
}

// CommitMessageData is the template context for per-event commit messages.
type CommitMessageData struct {
	Operation  string
	Group      string
	Version    string
	Resource   string
	Namespace  string
	Name       string
	APIVersion string
	Username   string
	GitTarget  string
}

// BatchCommitMessageData is the template context for atomic batch commit messages.
type BatchCommitMessageData struct {
	Count     int
	GitTarget string
}

// ResolveCommitConfig resolves API commit settings into runtime defaults.
func ResolveCommitConfig(spec *v1alpha1.CommitSpec) CommitConfig {
	config := CommitConfig{
		Committer: CommitterConfig{
			Name:  DefaultCommitterName,
			Email: DefaultCommitterEmail,
		},
		Message: CommitMessageConfig{
			Template:      DefaultCommitMessageTemplate,
			BatchTemplate: DefaultBatchCommitMessageTemplate,
		},
	}

	if spec == nil {
		return config
	}

	if spec.Committer != nil {
		if name := strings.TrimSpace(spec.Committer.Name); name != "" {
			config.Committer.Name = name
		}
		if email := strings.TrimSpace(spec.Committer.Email); email != "" {
			config.Committer.Email = email
		}
	}

	if spec.Message != nil {
		if template := strings.TrimSpace(spec.Message.Template); template != "" {
			config.Message.Template = template
		}
		if batchTemplate := strings.TrimSpace(spec.Message.BatchTemplate); batchTemplate != "" {
			config.Message.BatchTemplate = batchTemplate
		}
	}

	return config
}
