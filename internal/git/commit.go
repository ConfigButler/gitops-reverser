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
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

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

func renderBatchCommitMessage(
	events []Event,
	override string,
	gitTarget string,
	config CommitConfig,
) (string, error) {
	if strings.TrimSpace(override) != "" {
		return override, nil
	}

	return renderCommitTemplate(
		"batch",
		config.Message.BatchTemplate,
		BatchCommitMessageData{
			Count:     len(events),
			GitTarget: gitTarget,
		},
	)
}

func renderGroupCommitMessage(unit CommitUnit, config CommitConfig) (string, error) {
	return renderCommitTemplate(
		"group",
		config.Message.GroupTemplate,
		buildGroupedCommitMessageData(unit.GroupAuthor, unit.Target.Name, unit.Events),
	)
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

	if _, err := renderBatchCommitMessage(
		[]Event{sampleEvent},
		"",
		"example-target",
		config,
	); err != nil {
		return err
	}

	if _, err := renderGroupCommitMessage(CommitUnit{
		Events:      []Event{sampleEvent},
		GroupAuthor: sampleEvent.UserInfo.Username,
		Target: ResolvedTargetMetadata{
			Name: sampleEvent.GitTargetName,
		},
	}, config); err != nil {
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
	unit CommitUnit,
	config CommitConfig,
	signer git.Signer,
	when time.Time,
) *git.CommitOptions {
	return &git.CommitOptions{
		Author: &object.Signature{
			Name:  unit.GroupAuthor,
			Email: ConstructSafeEmail(unit.GroupAuthor, "cluster.local"),
			When:  when,
		},
		Committer: operatorSignature(config, when),
		Signer:    signer,
	}
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
