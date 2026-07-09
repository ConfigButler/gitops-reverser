// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"time"

	"github.com/go-logr/logr"
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

// ValidateOperatorTypesHandler is the admission handler for our operator CRDs. For a
// command kind (a CommitRequest) it captures the authenticated submitter into the
// CommandAuthorStore. That capture is pure observation with a single side effect (a Redis
// upsert): a missed capture degrades to a committer-authored commit, so a user's command
// never depends on it succeeding (docs/design/commitrequest-admission-authorship.md §2, §4).
//
// It rejects exactly one thing: a CommitRequest whose spec.author asserts an identity the
// requester is not authorized to assert. That check needs the requester, which only
// admission sees. Its verdict is recorded on the author record rather than trusted from
// the allow itself, because this webhook is failurePolicy: Ignore — a bypassed webhook
// writes no record, and the controller then ignores the assertion rather than honoring an
// unchecked one. See docs/design/multi-tenant/asserted-commit-author.md.
//
// It dispatches on the resource (isCommandKind today), so a future config-validation
// branch for non-command kinds slots in alongside without disturbing this one.
type ValidateOperatorTypesHandler struct {
	Store CommandAuthorRecorder
	// Authorizer decides whether a requester may set CommitRequest.spec.author. A nil
	// Authorizer denies every assertion: an unverifiable privilege is not a granted one.
	Authorizer AuthorAssertionAuthorizer
}

// Handle records {uid → author} for an admitted command CREATE before the object
// persists (the authorship invariant, §2), then allows. Every early return still
// allows: a non-command kind, a dry-run, a missing uid, or an unauthenticated request
// simply records nothing and falls back to the committer downstream. The one denial is an
// unauthorized spec.author.
func (h *ValidateOperatorTypesHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx).WithName("validate-operator-types")

	gr := metav1.GroupResource{Group: req.Resource.Group, Resource: req.Resource.Resource}
	if !isCommandKind(gr) {
		// Belt-and-suspenders; the webhook rules already scope us to command kinds.
		return admission.Allowed("not a command kind")
	}

	// The author assertion is authorized before the dry-run and store short-circuits: a
	// dry-run apply must report the same forbidden error a real one would, and an install
	// without Redis must not silently accept a privileged field it will then ignore.
	asserted, asserts := parseAssertedAuthor(req.Object.Raw)
	assertAuthorAllowed := false
	if asserts {
		allowed, denial := h.authorizeAuthorAssertion(ctx, log, req, asserted)
		if !allowed {
			return denial
		}
		assertAuthorAllowed = true
	}

	// Dry-run never persists, so the controller will never read this record — and we
	// declare sideEffects: NoneOnDryRun, so we must honor it.
	if req.DryRun != nil && *req.DryRun {
		return admission.Allowed("dry-run: not recorded")
	}

	// No store means the webhook is running without a Redis backend (admission is on by
	// default, but command-author capture is Redis-backed). Allow without recording; the
	// controller then finalizes as the committer (AuthorAttributed=False), and — because
	// no record exists — ignores any spec.author it just authorized.
	if h.Store == nil {
		if asserts {
			log.Info("spec.author authorized but not recorded: no author store configured; "+
				"the CommitRequest will commit as the committer. Set --redis-addr.",
				"namespace", req.Namespace, "name", req.Name)
		}
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
		Author:              req.UserInfo.Username, // effective (impersonated) user — the apiserver resolved it
		DisplayName:         firstExtraValue(req.UserInfo.Extra, displayNameExtraKey),
		Email:               firstExtraValue(req.UserInfo.Extra, emailExtraKey),
		RequestedAt:         time.Now().UTC().Format(time.RFC3339Nano),
		AssertAuthorAllowed: assertAuthorAllowed,
	}
	if author.Author == "" {
		return admission.Allowed("no user to attribute") // anonymous never reaches a persisted CREATE
	}

	// Synchronous, best-effort: the write completes before we return, so it is present
	// before the object is visible (the authorship invariant, §2). A failure degrades to
	// the committer — never block the command.
	if err := h.Store.RecordCommandAuthor(ctx, uid, author); err != nil {
		log.Error(err, "record command author failed; will fall back to committer",
			"resource", gr.String(), "namespace", req.Namespace, "name", req.Name, "uid", uid)
		return admission.Allowed("author record failed")
	}
	// Info (not V(1)) on purpose: command CREATEs are rare, and this line proves the
	// webhook was actually invoked and the author captured — the first thing to check
	// when a CommitRequest unexpectedly commits as the committer.
	log.Info("recorded command author at admission",
		"resource", gr.String(), "namespace", req.Namespace, "name", req.Name,
		"uid", uid, "author", author.Author)
	return admission.Allowed("author recorded")
}

// authorizeAuthorAssertion runs the SubjectAccessReview behind CommitRequest.spec.author.
// allowed=false carries the admission denial to return.
//
// Three ways to be denied, all fail-closed:
//   - no Authorizer is wired (the operator cannot create SubjectAccessReviews), because an
//     unverifiable privilege is not a granted one;
//   - the review errors, because an authorizer we could not reach has not said yes;
//   - the review says no.
func (h *ValidateOperatorTypesHandler) authorizeAuthorAssertion(
	ctx context.Context,
	log logr.Logger,
	req admission.Request,
	asserted assertedAuthor,
) (bool, admission.Response) {
	if h.Authorizer == nil {
		log.Info("spec.author denied: no author-assertion authorizer configured",
			"namespace", req.Namespace, "name", req.Name, "user", req.UserInfo.Username)
		return false, denyAuthorAssertion(req.UserInfo.Username, req.Namespace, asserted.GitTargetName,
			"the operator cannot create SubjectAccessReviews, so the assertion cannot be authorized")
	}

	allowed, reason, err := h.Authorizer.CanAssertAuthor(ctx, req.UserInfo, req.Namespace, asserted.GitTargetName)
	if err != nil {
		log.Error(err, "author-assertion review failed; denying",
			"namespace", req.Namespace, "name", req.Name, "user", req.UserInfo.Username)
		return false, denyAuthorAssertion(req.UserInfo.Username, req.Namespace, asserted.GitTargetName,
			"the authorization review could not be completed: "+err.Error())
	}
	if !allowed {
		log.Info("spec.author denied: requester lacks the assert-author verb",
			"namespace", req.Namespace, "name", req.Name,
			"user", req.UserInfo.Username, "gitTarget", asserted.GitTargetName)
		return false, denyAuthorAssertion(req.UserInfo.Username, req.Namespace, asserted.GitTargetName, reason)
	}

	// Info, not V(1): asserting an author is a privileged act on the repository's history.
	// Every use of it belongs in the audit trail of the operator's own logs.
	log.Info("spec.author authorized",
		"namespace", req.Namespace, "name", req.Name,
		"user", req.UserInfo.Username, "gitTarget", asserted.GitTargetName,
		"assertedAuthor", asserted.Name)
	return true, admission.Response{}
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
