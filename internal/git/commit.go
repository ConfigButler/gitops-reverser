// SPDX-License-Identifier: Apache-2.0

package git

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"text/template"
	"time"
	"unicode"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func renderEventCommitMessage(event Event, config CommitConfig) (string, error) {
	return renderCommitTemplate(
		"event",
		config.Message.EventTemplate,
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

// renderReconcileCommitMessageFromEvents renders the reconcile commit message for the
// events-based atomic path from the provider's ReconcileTemplate. It carries no single
// type or revision, so those template fields stay empty (the default guards them). An
// explicit override (a literal CommitRequest message) is used verbatim.
func renderReconcileCommitMessageFromEvents(
	events []Event,
	override string,
	gitTarget string,
	config CommitConfig,
) (string, error) {
	if strings.TrimSpace(override) != "" {
		return override, nil
	}

	return renderCommitTemplate(
		"reconcile",
		config.Message.ReconcileTemplate,
		ReconcileCommitMessageData{
			Count:     len(events),
			GitTarget: gitTarget,
		},
	)
}

// renderReconcileCommitMessage renders the reconcile commit message for a resync from the
// provider's ReconcileTemplate, so a resync honours a custom reconcile template. count is
// the number of resources the reconcile changed; scopeGVR names the synced type for a
// per-type splice (the M12/R2 per-type reconcile) and a nil scopeGVR (whole-target
// reconcile) leaves the type fields empty; revision is the cluster resourceVersion the
// desired set was pinned to (empty for a pure sweep). The default template guards the
// type and revision fields so it still renders cleanly when either is absent.
func renderReconcileCommitMessage(
	count int,
	gitTarget string,
	scope *ResyncScope,
	revision string,
	config CommitConfig,
) (string, error) {
	data := ReconcileCommitMessageData{
		Count:     count,
		GitTarget: gitTarget,
		Revision:  revision,
	}
	if scope != nil {
		data.Group = scope.GVR.Group
		data.Version = scope.GVR.Version
		data.Resource = scope.GVR.Resource
		data.APIVersion = buildAPIVersion(scope.GVR.Group, scope.GVR.Version)
		data.Namespace = scope.Namespace
	}
	return renderCommitTemplate("reconcile", config.Message.ReconcileTemplate, data)
}

func renderGroupCommitMessage(pendingWrite PendingWrite, config CommitConfig) (string, error) {
	return renderCommitTemplate(
		"group",
		config.Message.GroupTemplate,
		buildGroupedCommitMessageData(pendingWrite.Author(), pendingWrite.Target().Name, pendingWrite.Events),
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
		UserInfo:      UserInfo{Username: "template-validator"},
		GitTargetName: "example-target",
	}

	if _, err := renderEventCommitMessage(sampleEvent, config); err != nil {
		return err
	}

	if _, err := renderReconcileCommitMessageFromEvents(
		[]Event{sampleEvent},
		"",
		"example-target",
		config,
	); err != nil {
		return err
	}

	// Validate the per-type splice reconcile path with the type and revision fields populated,
	// so a custom reconcile template that names its synced type ({{.Resource}} / {{.APIVersion}})
	// or pins the {{.Revision}} is exercised at admission exactly as a per-type reconcile renders it.
	// The sample scope names a namespace so a template referencing {{.Namespace}} — populated
	// only by a namespace-scoped reconcile — is validated here too.
	sampleScope := ResyncScope{
		GVR:       schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		Namespace: "example-namespace",
	}
	if _, err := renderReconcileCommitMessage(1, "example-target", &sampleScope, "12345", config); err != nil {
		return err
	}

	if _, err := renderGroupCommitMessage(PendingWrite{
		Kind:   PendingWriteCommit,
		Events: []Event{sampleEvent},
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

// commitOptionsFor builds the CommitOptions for a pending write. The committer is always the operator.
func commitOptionsFor(
	pendingWrite PendingWrite,
	config CommitConfig,
	signer git.Signer,
	when time.Time,
) *git.CommitOptions {
	committer := operatorSignature(config, when)
	author := pendingWrite.AuthorUserInfo()
	// An attribution that RAN and did not resolve is authored by the sentinel, not by the
	// committer. Authoring it as the committer is what made a lost actor byte-identical to a
	// configured-author commit, so the loss was invisible in Git history.
	if pendingWrite.AttributionOutcome() == AttributionUnresolved {
		author = UnresolvedAuthor()
	}
	// Reaching here with an empty username now means only "attribution was never attempted"
	// (configured-author mode, reconcile/resync writes), where the committer genuinely IS the
	// author.
	if author.Username == "" {
		return &git.CommitOptions{
			Author:    committer,
			Committer: committer,
			Signer:    signer,
		}
	}

	return &git.CommitOptions{
		Author: &object.Signature{
			Name:  authorName(author),
			Email: authorEmail(author),
			When:  when,
		},
		Committer: committer,
		Signer:    signer,
	}
}

// validEmailRegex matches a syntactically valid email address. It recognises a
// username that is already an email and validates an OIDC-supplied email claim
// before trusting it in a signature header.
var validEmailRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)

// authorName returns the git author Name for a user: the OIDC display name
// when present and safe to place in a signature header, otherwise the
// Kubernetes username.
func authorName(user UserInfo) string {
	if name := strings.TrimSpace(user.DisplayName); name != "" && isSafeSignatureField(name) {
		return name
	}
	return user.Username
}

// authorEmail returns the git author Email for a user: the OIDC email claim
// when present and a valid address, otherwise a safe address constructed from
// the username.
func authorEmail(user UserInfo) string {
	if email := strings.TrimSpace(user.Email); validEmailRegex.MatchString(email) {
		return email
	}
	return ConstructSafeEmail(user.Username, "cluster.local")
}

// isSafeSignatureField reports whether s can be placed verbatim into a git
// signature header field. Control characters (notably newlines) and the angle
// brackets that delimit the email would corrupt the commit object.
func isSafeSignatureField(s string) bool {
	for _, r := range s {
		if r == '<' || r == '>' || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// ConstructSafeEmail takes a raw username and a domain and creates a valid
// git-compliant email address.
func ConstructSafeEmail(username string, domain string) string {
	// Check if username is already a valid email address.
	if validEmailRegex.MatchString(username) {
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
