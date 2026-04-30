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
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// GetCommitMessage returns the default structured commit message for the given event.
func GetCommitMessage(event Event) string {
	message, err := renderEventCommitMessage(event, ResolveCommitConfig(nil))
	if err != nil {
		return fmt.Sprintf("[%s] %s", event.Operation, event.Identifier.String())
	}
	return message
}

func renderEventCommitMessage(event Event, config CommitConfig) (string, error) {
	return renderCommitTemplate(
		"event",
		config.Message.Template,
		CommitMessageData{
			Operation:  event.Operation,
			Group:      event.Identifier.Group,
			Version:    event.Identifier.Version,
			Resource:   event.Identifier.Resource,
			Namespace:  event.Identifier.Namespace,
			Name:       event.Identifier.Name,
			APIVersion: buildAPIVersion(event.Identifier.Group, event.Identifier.Version),
			Username:   event.UserInfo.Username,
			GitTarget:  event.GitTargetName,
		},
	)
}

func renderBatchCommitMessage(request *WriteRequest, config CommitConfig) (string, error) {
	if request != nil && strings.TrimSpace(request.CommitMessage) != "" {
		return request.CommitMessage, nil
	}

	count := 0
	gitTargetName := ""
	if request != nil {
		count = len(request.Events)
		gitTargetName = request.GitTargetName
	}

	return renderCommitTemplate(
		"batch",
		config.Message.BatchTemplate,
		BatchCommitMessageData{
			Count:     count,
			GitTarget: gitTargetName,
		},
	)
}

func renderGroupCommitMessage(group *commitGroup, config CommitConfig) (string, error) {
	return renderCommitTemplate(
		"group",
		config.Message.GroupTemplate,
		buildGroupedCommitMessageData(group),
	)
}

func renderCommitMessageForGroup(group *commitGroup, config CommitConfig) (string, error) {
	groupEvents := group.orderedEvents()
	if len(groupEvents) == 1 {
		return renderEventCommitMessage(groupEvents[0], config)
	}

	return renderGroupCommitMessage(group, config)
}

func renderCommitTemplate(name, text string, data any) (string, error) {
	tmpl, err := template.New(name).Option("missingkey=error").Parse(text)
	if err != nil {
		return "", fmt.Errorf("parse %s commit template: %w", name, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute %s commit template: %w", name, err)
	}

	return buf.String(), nil
}

func buildAPIVersion(group, version string) string {
	if group == "" {
		return version
	}
	return group + "/" + version
}

func resolveWriteRequestCommitConfig(request *WriteRequest) CommitConfig {
	if request == nil || request.CommitConfig == nil {
		return ResolveCommitConfig(nil)
	}
	return *request.CommitConfig
}

// ValidateCommitConfig checks that commit templates are syntactically valid.
func ValidateCommitConfig(config CommitConfig) error {
	sampleEvent := Event{
		Operation: "CREATE",
		Identifier: types.ResourceIdentifier{
			Group:     "apps",
			Version:   "v1",
			Resource:  "deployments",
			Namespace: "default",
			Name:      "example",
		},
		UserInfo:      UserInfo{Username: "gitops-reverser"},
		GitTargetName: "example-target",
	}

	if _, err := renderEventCommitMessage(sampleEvent, config); err != nil {
		return err
	}

	if _, err := renderBatchCommitMessage(&WriteRequest{
		Events:        []Event{sampleEvent},
		GitTargetName: "example-target",
	}, config); err != nil {
		return err
	}

	sampleGroup := newCommitGroup(sampleEvent)
	sampleGroup.add(sampleEvent)
	if _, err := renderGroupCommitMessage(sampleGroup, config); err != nil {
		return err
	}

	return nil
}

func operatorSignature(config CommitConfig, when time.Time) *object.Signature {
	return &object.Signature{
		Name:  config.Committer.Name,
		Email: config.Committer.Email,
		When:  when,
	}
}

func commitOptionsForEvent(event Event, config CommitConfig, signer git.Signer, when time.Time) *git.CommitOptions {
	return &git.CommitOptions{
		Author: &object.Signature{
			Name:  event.UserInfo.Username,
			Email: ConstructSafeEmail(event.UserInfo.Username, "cluster.local"),
			When:  when,
		},
		Committer: operatorSignature(config, when),
		Signer:    signer,
	}
}

func commitOptionsForBatch(config CommitConfig, signer git.Signer, when time.Time) *git.CommitOptions {
	operator := operatorSignature(config, when)
	return &git.CommitOptions{
		Author:    operator,
		Committer: operator,
		Signer:    signer,
	}
}

// commitOptionsForGroup attributes the commit to the group's author and keeps
// the operator as the committer (the operator physically writes the commit;
// signing material, when present, is the operator's). Mirrors
// commitOptionsForEvent's use of ConstructSafeEmail so cross-path tooling
// continues to recognise the same identity.
func commitOptionsForGroup(
	group *commitGroup,
	config CommitConfig,
	signer git.Signer,
	when time.Time,
) *git.CommitOptions {
	return &git.CommitOptions{
		Author: &object.Signature{
			Name:  group.Author,
			Email: ConstructSafeEmail(group.Author, "cluster.local"),
			When:  when,
		},
		Committer: operatorSignature(config, when),
		Signer:    signer,
	}
}

// createCommitForEvent creates a commit for the given event.
func createCommitForEvent(
	worktree *git.Worktree,
	event Event,
	config CommitConfig,
	signer git.Signer,
) (plumbing.Hash, error) {
	commitMessage, err := renderEventCommitMessage(event, config)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	when := time.Now()
	return worktree.Commit(commitMessage, commitOptionsForEvent(event, config, signer, when))
}

// ConstructSafeEmail takes a raw username and a domain and creates a valid
// git-compliant email address.
func ConstructSafeEmail(username string, domain string) string {
	// Check if username is already a valid email address.
	emailRegex := regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)
	if emailRegex.MatchString(username) {
		return username
	}

	// Remove unsupported characters so we can safely use the username in a Git signature header.
	clean := strings.ToLower(username)
	reg := regexp.MustCompile(`[^a-z0-9\.\-]`)
	clean = reg.ReplaceAllString(clean, "")
	if clean == "" {
		clean = "unknown-user"
	}

	return fmt.Sprintf("%s@noreply.%s", clean, domain)
}
