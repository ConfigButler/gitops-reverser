// SPDX-License-Identifier: Apache-2.0

package git

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"text/template"
	"time"
	"unicode"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// renderReconcileCommitMessageFromEvents renders the reconcile commit message for the
// events-based atomic path from the provider's ReconcileTemplate. It carries no single
// type and no snapshot version, so those template fields stay empty (the default guards them).
// Literal overrides are resolved by the caller.
func renderReconcileCommitMessageFromEvents(
	events []Event,
	gitTarget string,
	config CommitConfig,
) (string, error) {
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
// provider's ReconcileTemplate, so a resync honours a custom reconcile template. count is the
// number of resources the reconcile changed; scopeGVR names the synced type for a per-type
// reconcile, and a nil scopeGVR (whole-target reconcile) leaves the type fields empty;
// resourceVersion is the cluster resourceVersion the desired set was pinned to (empty for a
// pure sweep). The default template guards the type and version fields so it still renders
// cleanly when either is absent.
func renderReconcileCommitMessage(
	count int,
	gitTarget string,
	scope *ResyncScope,
	resourceVersion string,
	config CommitConfig,
) (string, error) {
	data := ReconcileCommitMessageData{
		Count:           count,
		GitTarget:       gitTarget,
		ResourceVersion: resourceVersion,
	}
	if scope != nil {
		data.Group = scope.Cell.Group
		data.Version = scope.Version
		data.Resource = scope.Cell.Resource
		data.APIVersion = buildAPIVersion(scope.Cell.Group, scope.Version)
		data.Namespace = scope.Cell.Namespace
	}
	return renderCommitTemplate("reconcile", config.Message.ReconcileTemplate, data)
}

func renderLiveCommitMessage(pendingWrite PendingWrite, config CommitConfig) (string, error) {
	return renderCommitTemplate("live", config.Message.LiveTemplate, pendingWrite.liveMessageData())
}

// renderRequestCommitMessage frames an attached CommitRequest's message with the target's
// requestTemplate. The literal rides in as .RequestMessage rather than being parsed, so nothing a
// requester wrote is ever executed.
func renderRequestCommitMessage(pendingWrite PendingWrite, config CommitConfig) (string, error) {
	return renderCommitTemplate("request", config.Message.RequestTemplate, pendingWrite.liveMessageData())
}

// liveMessageData is the template context both live renders share. One builder, because a framed
// request commit is a live window that happens to carry a message — not a different kind of commit.
func (p PendingWrite) liveMessageData() LiveCommitMessageData {
	return buildLiveCommitMessageData(p.Author(), p.Target().Name, p.CommitMessage, p.Events)
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

	if _, err := renderReconcileCommitMessageFromEvents(
		[]Event{sampleEvent},
		"example-target",
		config,
	); err != nil {
		return err
	}

	// Validate the per-type reconcile path with the type and revision fields populated,
	// so a custom reconcile template that names its synced type ({{.Resource}} / {{.APIVersion}})
	// or pins the {{.ResourceVersion}} is exercised at admission exactly as a per-type reconcile
	// renders it.
	// The sample scope names a namespace so a template referencing {{.Namespace}} — populated
	// only by a namespace-scoped reconcile — is validated here too.
	sampleScope := ResyncScopeFor(
		schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		"example-namespace",
	)
	if _, err := renderReconcileCommitMessage(1, "example-target", &sampleScope, "12345", config); err != nil {
		return err
	}

	// One sample carries an object with labels and a kind, and the others carry none, so both
	// directions of a metadata-reading template are exercised here: the render with the label
	// present, and the render without it. That second one is the important half — these
	// templates run with missingkey=error, so "{{.Labels.team}}" fails for a resource that does
	// not carry "team", and failing at admission is the difference between a rejected GitTarget
	// and a commit that dies mid-window months later. "{{.Label \"team\"}}" renders empty and
	// passes both.
	for _, events := range liveValidationSamples(sampleEvent) {
		if _, err := renderLiveCommitMessage(PendingWrite{
			Kind: PendingWriteCommit, Events: events,
		}, config); err != nil {
			return err
		}
		if err := validateRequestTemplate(config, events); err != nil {
			return err
		}
	}

	return nil
}

// liveValidationSamples is the set of window shapes both live-message templates are validated
// against: growing windows, mixed operations, an empty author, and — the important half — a
// resource that carries an object with labels beside ones that carry neither, because these
// templates run with missingkey=error and it is the render WITHOUT the label that fails.
//
// It is shared so liveTemplate and requestTemplate cannot drift into being checked against
// different worlds. They face identical windows at runtime; checking one more thoroughly than the
// other just moves which template fails months later instead of at admission.
//
// The UPDATE sample deliberately carries NO resourceVersion and NO generation. A live watch
// stamps a version on every event, but reconcile and bootstrap do not, and a generation is absent
// for every kind without a spec (a ConfigMap, a Secret) however it was produced — so the shape a
// template must survive is the mixed one.
func liveValidationSamples(sampleEvent Event) [][]Event {
	var samples [][]Event
	for _, author := range []string{"template-validator", ""} {
		var events []Event
		for _, operation := range []string{"CREATE", "UPDATE", "DELETE"} {
			event := sampleEvent
			event.UserInfo.Username = author
			event.Operation = operation
			event.Identifier.Name = operation
			event.ResourceVersion = sampleResourceVersion(operation)
			event.Generation = sampleGeneration(operation)
			if operation == "CREATE" {
				event.Object = sampleLabeledObject()
			}
			events = append(events, event)
			samples = append(samples, append([]Event(nil), events...))
		}
	}
	return samples
}

// sampleResourceVersion gives the validation samples both states of ResourceVersion: present on a
// CREATE and on a DELETE — which carries one although it carries no object — and absent on the
// UPDATE, standing in for every producer that observed no version.
func sampleResourceVersion(operation string) string {
	switch operation {
	case "CREATE":
		return "20001"
	case "DELETE":
		return "20003"
	default:
		return ""
	}
}

// sampleGeneration mirrors sampleResourceVersion for the desired-state counter, so a template
// naming {{.Generation}} is validated against a resource that has one and a resource that does
// not — the second being every spec-less kind this operator mirrors.
func sampleGeneration(operation string) int64 {
	const (
		sampleCreateGeneration = 3
		sampleDeleteGeneration = 4
	)
	switch operation {
	case "CREATE":
		return sampleCreateGeneration
	case "DELETE":
		return sampleDeleteGeneration
	default:
		return 0
	}
}

// newRequestTemplateProbe mints the sentinel that requestTemplate validation renders as the
// request's message.
//
// Fresh per call, and unpredictable, because the check asks "did the message reach the commit?" by
// looking for this string in the rendered output. A FIXED sentinel answers a weaker question: a
// template that emits the constant itself would pass while never referencing .RequestMessage at
// all. Nobody would write that on purpose, but a check that can be satisfied without doing the
// thing it verifies is not a check.
func newRequestTemplateProbe() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate requestTemplate probe: %w", err)
	}
	return "gitops-reverser-request-probe-" + hex.EncodeToString(raw[:]), nil
}

// validateRequestTemplate checks that a configured requestTemplate renders, AND that it actually
// puts the request's message in the commit.
//
// The second half is the point. Without it the feature has a hole exactly as bad as the one it was
// designed to avoid: a target could set `requestTemplate: "chore: sync {{.Count}} resources"` and
// every save message would silently vanish — the requester writes a reason, the commit never
// carries it, and the commit is still counted as request-sourced. Framing the message is the whole
// purpose of the field, so a template that drops it is a mistake, not a configuration choice.
//
// It probes the RENDERED OUTPUT rather than scanning the template source. A scan for the literal
// "{{.RequestMessage}}" would reject `{{.RequestMessage | printf "%s"}}`, a template that assigns
// it to a variable first, and every other legitimate spelling — while the probe accepts all of them
// for the right reason: the message reached the commit.
//
// Sample execution cannot prove every branch: a template that drops the message only under, say,
// {{if eq .Count 1}} still passes. That is the same caveat docs/configuration.md already states for
// the other templates, not a new one.
func validateRequestTemplate(config CommitConfig, events []Event) error {
	if config.Message.RequestTemplate == "" {
		return nil
	}

	probe, err := newRequestTemplateProbe()
	if err != nil {
		return err
	}
	rendered, err := renderRequestCommitMessage(PendingWrite{
		Kind:          PendingWriteCommit,
		CommitMessage: probe,
		Events:        events,
	}, config)
	if err != nil {
		return err
	}
	// EVERY sample must carry the message through, not merely one: the contract is that a
	// requester's reason reaches the commit whatever the window happened to contain.
	if !strings.Contains(rendered, probe) {
		return errors.New("requestTemplate must render {{.RequestMessage}}: as written it would " +
			"drop the CommitRequest's message from the commit. Omit requestTemplate to commit that " +
			"message verbatim")
	}
	return nil
}

// sampleLabeledObject is the object the commit-template validator hands one of its sample
// events: enough metadata for {{.Kind}} and a label accessor to render, and deliberately not
// handed to the DELETE sample, which carries no object in production either.
func sampleLabeledObject() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      "example",
			"namespace": "default",
			"labels": map[string]any{
				"app.kubernetes.io/instance": "example",
				"team":                       "example-team",
			},
		},
	}}
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
