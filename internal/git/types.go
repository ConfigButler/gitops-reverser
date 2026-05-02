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

	gogit "github.com/go-git/go-git/v5"
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
	// DefaultGroupCommitMessageTemplate is the default message shape for
	// finalized commit-window commits that contain multiple events.
	DefaultGroupCommitMessageTemplate = "{{.Author}} on {{.GitTarget}}: {{.Count}} resource(s)"

	resourceRefStringPartCap = 5
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
	// CommitModePerEvent streams request events through the live commit window.
	// With commitWindow=0 each event finalizes immediately; otherwise events
	// coalesce by author, target, and quiet-window boundaries.
	CommitModePerEvent CommitMode = "per_event"
	// CommitModeAtomic creates one commit for all events in the request.
	CommitModeAtomic CommitMode = "atomic"
)

// WriteRequest is the unit of work queued and written by the BranchWorker.
type WriteRequest struct {
	Events             []Event
	CommitMessage      string
	CommitConfig       *CommitConfig
	Signer             gogit.Signer
	GitTargetName      string
	GitTargetNamespace string
	BootstrapOptions   pathBootstrapOptions
	CommitMode         CommitMode
}

// PendingWriteKind distinguishes the durable write shapes retained until push.
type PendingWriteKind string

const (
	// PendingWriteCommit is one finalized commit-shaped live-event window.
	PendingWriteCommit PendingWriteKind = "grouped_window"
	// PendingWriteAtomic is a caller-defined atomic request, typically from
	// reconciliation.
	PendingWriteAtomic PendingWriteKind = "atomic"
)

type pendingTargetKey struct {
	Name      string
	Namespace string
}

// ResolvedTargetMetadata is the target-scoped planning data retained with a
// pending write so replay does not re-fetch mutable GitTarget state.
type ResolvedTargetMetadata struct {
	Name             string
	Namespace        string
	Path             string
	BootstrapOptions pathBootstrapOptions
	EncryptionConfig *ResolvedEncryptionConfig
}

// PendingWrite is the unit retained until a push succeeds.
type PendingWrite struct {
	Kind               PendingWriteKind
	Events             []Event
	CommitMessage      string
	CommitConfig       CommitConfig
	Signer             gogit.Signer
	GitTargetName      string
	GitTargetNamespace string
	Targets            map[pendingTargetKey]ResolvedTargetMetadata
	ByteSize           int64
}

// CommitMessageKind determines which message/authorship path the executor uses.
type CommitMessageKind string

const (
	CommitMessagePerEvent CommitMessageKind = "event"
	CommitMessageBatch    CommitMessageKind = "batch"
	CommitMessageGrouped  CommitMessageKind = "group"
)

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

// CommitMessageConfig contains the resolved per-event, batch, and grouped templates.
type CommitMessageConfig struct {
	Template      string
	BatchTemplate string
	GroupTemplate string
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

// ResourceRef is the lightweight resource identifier emitted to grouped commit
// templates via GroupedCommitMessageData.Resources.
type ResourceRef struct {
	Group     string
	Version   string
	Resource  string
	Namespace string
	Name      string
}

// String renders the ref as group/version/resource[/namespace]/name.
// The format mirrors ResourceIdentifier.String for templates that want to
// {{range}} over Resources and just print each entry.
func (r ResourceRef) String() string {
	parts := make([]string, 0, resourceRefStringPartCap)
	if r.Group != "" {
		parts = append(parts, r.Group)
	}
	if r.Version != "" {
		parts = append(parts, r.Version)
	}
	if r.Resource != "" {
		parts = append(parts, r.Resource)
	}
	if r.Namespace != "" {
		parts = append(parts, r.Namespace)
	}
	if r.Name != "" {
		parts = append(parts, r.Name)
	}
	return strings.Join(parts, "/")
}

// GroupedCommitMessageData is the template context for grouped commit
// messages. Each grouped commit covers exactly one (author, gitTarget) tuple
// (see docs/design/commit-window-refactor.md).
type GroupedCommitMessageData struct {
	// Author is the verbatim event.UserInfo.Username for the group.
	Author string
	// GitTarget is the single target this commit is bound to.
	GitTarget string
	// Count is the number of distinct resources committed.
	Count int
	// Operations counts events by operation kind (CREATE/UPDATE/DELETE).
	Operations map[string]int
	// Resources is the per-resource list, deduplicated by file path so the
	// final state is what's being committed.
	Resources []ResourceRef
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
			GroupTemplate: DefaultGroupCommitMessageTemplate,
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
		if groupTemplate := strings.TrimSpace(spec.Message.GroupTemplate); groupTemplate != "" {
			config.Message.GroupTemplate = groupTemplate
		}
	}

	return config
}
