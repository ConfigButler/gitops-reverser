// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/ConfigButler/gitops-reverser/internal/queue"
)

// ValidateOperatorTypesPath is the validating admission endpoint scoped to our own
// operator CRDs. Today its one job is command authorship — capturing the submitter of a
// command kind (a CommitRequest) into Redis and always allowing — but it is the intended
// home for per-our-type admission generally (e.g. config validation of WatchRule /
// GitProvider / GitTarget later), which would be added as additional webhook-config
// entries with their own rules and failurePolicy. Distinct from the broad observe-all
// ValidateAllPath.
const ValidateOperatorTypesPath = "/validate-operator-types"

const (
	// displayNameExtraKey is the user.extra key carrying the OIDC "name" claim, when
	// the API server is configured to map it. It mirrors the audit path's extra key
	// (internal/queue/audit_event_parsing.go) so a command author and a mirrored-
	// resource author resolve the same display name from the same claim.
	displayNameExtraKey = "configbutler.ai/claims/display-name"
	// emailExtraKey is the user.extra key carrying the OIDC "email" claim.
	emailExtraKey = "configbutler.ai/claims/email"
)

// isCommandKind reports whether gr is one of our own command CRDs whose submitter we
// capture at admission. It is the registry of command kinds: adding one is a single
// case here plus one rule in the webhook config — the handler body does not change.
func isCommandKind(gr metav1.GroupResource) bool {
	switch gr {
	case metav1.GroupResource{Group: "configbutler.ai", Resource: "commitrequests"}:
		return true
	// future: case metav1.GroupResource{Group: "configbutler.ai", Resource: "<next-command>"}:
	default:
		return false
	}
}

// CommandAuthorRecorder records the authenticated submitter of one command object.
// *queue.CommandAuthorStore satisfies it; the interface keeps the handler unit-testable
// without a live Redis.
type CommandAuthorRecorder interface {
	RecordCommandAuthor(ctx context.Context, uid types.UID, author queue.CommandAuthor) error
}

// ValidateOperatorTypesHandler is the admission handler for our operator CRDs. Today it
// does one thing: for a command kind (a CommitRequest) it captures the authenticated
// submitter into the CommandAuthorStore and always allows — pure observation with a
// single side effect (a Redis upsert), never a rejection, so a user's command never
// depends on it succeeding (a missed capture leaves the request without a claimed actor;
// see docs/spec/commitrequest-admission-authorship.md). It dispatches on the
// resource (isCommandKind today), so a future config-validation branch for non-command
// kinds slots in alongside without disturbing this one.
type ValidateOperatorTypesHandler struct {
	Store CommandAuthorRecorder
}

// Handle records {uid → author} for an admitted command CREATE before the object
// persists (the authorship invariant, §2), then allows. Every early return still
// allows: a non-command kind, a dry-run, a missing uid, or an unauthenticated request
// simply records nothing and leaves the request without a claimed actor downstream.
func (h *ValidateOperatorTypesHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx).WithName("validate-operator-types")

	gr := metav1.GroupResource{Group: req.Resource.Group, Resource: req.Resource.Resource}
	if !isCommandKind(gr) {
		// Belt-and-suspenders; the webhook rules already scope us to command kinds.
		return admission.Allowed("not a command kind")
	}
	// Dry-run never persists, so the controller will never read this record — and we
	// declare sideEffects: NoneOnDryRun, so we must honor it.
	if req.DryRun != nil && *req.DryRun {
		return admission.Allowed("dry-run: not recorded")
	}

	// No store means the webhook is running without a Redis backend (admission is on by
	// default, but command-author capture is Redis-backed). Allow without recording; the
	// controller then uses the no-actor path (AuthorAttributed=False).
	if h.Store == nil {
		return admission.Allowed("no author store: not recorded")
	}

	// metadata.uid identifies the object (not req.UID, which identifies the review). The
	// API server assigns it before validating admission runs, so it is present even for
	// a generateName CREATE — no response-body name recovery is ever needed.
	uid := commandObjectUID(req.Object.Raw)
	if uid == "" {
		log.Info("command object has no uid; not recording", "namespace", req.Namespace, "name", req.Name)
		return admission.Allowed("no uid: not recorded")
	}

	author := queue.CommandAuthor{
		Author:      req.UserInfo.Username, // effective (impersonated) user — the apiserver resolved it
		DisplayName: firstExtraValue(req.UserInfo.Extra, displayNameExtraKey),
		Email:       firstExtraValue(req.UserInfo.Extra, emailExtraKey),
		RequestedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if author.Author == "" {
		return admission.Allowed("no user to attribute") // anonymous never reaches a persisted CREATE
	}

	// Synchronous, best-effort: the write completes before we return, so it is present
	// before the object is visible. A failure leaves the request without a claimed actor —
	// never block the command.
	if err := h.Store.RecordCommandAuthor(ctx, uid, author); err != nil {
		log.Error(err, "record command author failed; request will claim no actor",
			"resource", gr.String(), "namespace", req.Namespace, "name", req.Name, "uid", uid)
		return admission.Allowed("author record failed")
	}
	// Info (not V(1)) on purpose: command CREATEs are rare, and this line proves the
	// webhook was actually invoked and the author captured — the first thing to check
	// when a CommitRequest unexpectedly reports AuthorAttributed=False.
	log.Info("recorded command author at admission",
		"resource", gr.String(), "namespace", req.Namespace, "name", req.Name,
		"uid", uid, "author", author.Author)
	return admission.Allowed("author recorded")
}

// commandObjectUID extracts metadata.uid from an admission request's raw object. It
// reads only the field it needs (like the audit handler's metadata probes), so it never
// depends on the command kind being registered in a decoder scheme.
func commandObjectUID(raw []byte) types.UID {
	if len(raw) == 0 {
		return ""
	}
	var probe struct {
		Metadata struct {
			UID types.UID `json:"uid"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return probe.Metadata.UID
}

// firstExtraValue returns the first value for key in an admission request's
// user.extra map, or "" when the key is absent or carries no values. It mirrors the
// audit path's helper (internal/queue/audit_event_parsing.go) for the same user.extra
// shape carried on an admission request.
func firstExtraValue(extra map[string]authnv1.ExtraValue, key string) string {
	values := extra[key]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
