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

package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/apis/audit"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ConfigButler/gitops-reverser/internal/audit/outcome"
	"github.com/ConfigButler/gitops-reverser/internal/auditutil"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// auditHandlerFirsts holds one-shot startup-milestone log gates. Per-event
// Info logs are noisy in steady state; these surface the first time the
// pipeline crosses each transition so operators can see startup progress at
// a glance.
type auditHandlerFirsts struct {
	officialRequest   sync.Once
	additionalRequest sync.Once
	officialEmit      sync.Once
	additionalEmit    sync.Once
	impersonatedEvent sync.Once
	byTypeMirrorError sync.Once
}

// DefaultAuditMaxRequestBodyBytes limits incoming audit payload size.
const DefaultAuditMaxRequestBodyBytes = int64(10 * 1024 * 1024)

// AuditHandlerConfig contains configuration for the audit handler.
type AuditHandlerConfig struct {
	// MaxRequestBodyBytes is the maximum accepted HTTP request body size.
	MaxRequestBodyBytes int64
	// DebugQueue enqueues every decoded event before audit processing begins.
	// If nil, early debug stream queueing is disabled.
	DebugQueue AuditDebugEventQueue
	// Joiner optionally merges parked additional-source bodies into shallow
	// official events before they are mirrored.
	Joiner AuditEventJoiner
	// ByTypeQueue mirrors each accepted (StageResponseComplete, body-merged) event
	// into its per-resource-type stream — the substrate every consumer reads
	// (docs/design/stream/api-source-of-truth-reconcile.md). Best-effort: a failure
	// here never fails the audit request. If nil, per-type mirroring is disabled.
	ByTypeQueue AuditEventQueue
	// MirrorGate, when set, demand-gates ByTypeQueue: only events for a type currently in
	// the shared required-set are mirrored, so we stop creating a stream for every cluster
	// type (docs/finished/demand-gated-audit-ingestion.md). Nil disables gating —
	// mirror everything (the pre-gate behaviour). Allow is an in-memory lookup on the hot path.
	MirrorGate MirrorGate
	// CommitRequestAuthors captures the author fact for CommitRequest create events before
	// demand-gating or ordered-stream insertion. Unlike ByTypeQueue this is functional:
	// a write failure returns an audit request error so the API server can retry delivery.
	CommitRequestAuthors CommitRequestAuthorRecorder
}

// AuditEventQueue persists accepted audit events for downstream processing.
type AuditEventQueue interface {
	Enqueue(ctx context.Context, event auditv1.Event) error
}

// CommitRequestAuthorRecorder persists the author attribution fact for CommitRequest creates.
type CommitRequestAuthorRecorder interface {
	CaptureCommitRequestAuthor(ctx context.Context, event auditv1.Event) (bool, error)
}

// MirrorGate is the read side of the demand gate: it decides whether a type's audit events should
// be mirrored. Allow must be safe for concurrent use and must not block (it runs per event).
type MirrorGate interface {
	Allow(group, resource string) bool
}

// AuditDebugEventQueue persists decoded events for early audit debugging.
type AuditDebugEventQueue interface {
	Enqueue(ctx context.Context, source string, event auditv1.Event) error
}

// auditIngressDecision is the intrinsic accept/reject verdict for an audit event
// before it enters the join pipeline. The verdict is derived purely from the
// event — stage, verb, and body shape — and carries no knowledge of WatchRules;
// rule relevance is a consumer-side concern applied later. Reason labels a
// rejection for drop diagnostics.
type auditIngressDecision struct {
	Process bool
	Reason  string
}

// AuditHandler handles incoming audit events and collects telemetry.
type AuditHandler struct {
	scheme       *runtime.Scheme
	deserializer runtime.Decoder
	config       AuditHandlerConfig
	firsts       auditHandlerFirsts
	canonicalMu  sync.Mutex
}

// NewAuditHandler creates a new audit handler with the given configuration.
func NewAuditHandler(config AuditHandlerConfig) (*AuditHandler, error) {
	if config.MaxRequestBodyBytes <= 0 {
		config.MaxRequestBodyBytes = DefaultAuditMaxRequestBodyBytes
	}

	scheme := runtime.NewScheme()
	if err := audit.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to initialize scheme: %w", err)
	}
	if err := auditv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add v1 types to scheme: %w", err)
	}

	codecs := serializer.NewCodecFactory(scheme)
	deserializer := codecs.UniversalDeserializer()

	return &AuditHandler{
		scheme:       scheme,
		deserializer: deserializer,
		config:       config,
	}, nil
}

// EventList request-boundary outcome labels. They stay bounded — no path,
// remote address, or status-code dimension — so the ingress metric set is small.
const (
	outcomeProcessed    = "processed"
	outcomeEmpty        = "empty"
	outcomeDecodeError  = "decode_error"
	outcomeProcessError = "process_error"
)

// ServeHTTP implements http.Handler for audit event processing.
func (h *AuditHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := logf.Log.WithName("audit-handler")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	source, err := auditSourceFromPath(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	start := time.Now()
	outcome, eventCount := h.serveEventListRequest(ctx, w, r, source, log)
	h.recordEventListRequest(ctx, source, outcome, eventCount, time.Since(start))
}

// serveEventListRequest decodes and processes one EventList request, returning the
// bounded outcome and the number of decoded event items for the ingress metrics.
func (h *AuditHandler) serveEventListRequest(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	source AuditSource,
	log logr.Logger,
) (string, int) {
	reqLog := log.WithValues(
		"source", source,
		"remoteAddr", r.RemoteAddr,
		"path", r.URL.Path,
	)

	eventListV1, err := h.decodeEventList(r)
	if err != nil {
		reqLog.Error(err, "Failed to decode audit event list")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return outcomeDecodeError, 0
	}

	eventCount := len(eventListV1.Items)
	if err := h.enqueueDebugEvents(ctx, source, eventListV1.Items); err != nil {
		reqLog.Error(err, "Failed to enqueue early audit debug events")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return outcomeProcessError, eventCount
	}
	if eventCount == 0 {
		reqLog.Info("Received empty audit event list", "eventCount", 0, "processingOutcome", "empty")
		h.writeResponse(w, reqLog, "Empty event list processed")
		return outcomeEmpty, 0
	}

	h.logFirstAuditRequest(reqLog, source, eventCount)

	if err := h.processEvents(ctx, source, eventListV1.Items); err != nil {
		reqLog.Error(err, "Failed to process audit events")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return outcomeProcessError, eventCount
	}
	reqLog.V(1).Info("Processed audit request", "eventCount", eventCount, "processingOutcome", "success")

	h.writeResponse(w, reqLog, "Audit event processed")
	return outcomeProcessed, eventCount
}

// writeResponse writes a 200 OK body, logging any write failure.
func (h *AuditHandler) writeResponse(w http.ResponseWriter, log logr.Logger, body string) {
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(body)); err != nil {
		log.Error(err, "Failed to write response")
	}
}

// recordEventListRequest emits the three EventList ingress-boundary metrics.
// The event-item counter has no sample for decode_error, since item count is
// only known after a successful decode.
func (h *AuditHandler) recordEventListRequest(
	ctx context.Context,
	source AuditSource,
	outcome string,
	eventCount int,
	elapsed time.Duration,
) {
	attrs := metric.WithAttributes(
		attribute.String("source", string(source)),
		attribute.String("outcome", outcome),
	)
	if telemetry.AuditEventListsTotal != nil {
		telemetry.AuditEventListsTotal.Add(ctx, 1, attrs)
	}
	if telemetry.AuditEventListDurationSeconds != nil {
		telemetry.AuditEventListDurationSeconds.Record(ctx, elapsed.Seconds(), attrs)
	}
	if outcome != outcomeDecodeError && telemetry.AuditEventListEventsTotal != nil {
		telemetry.AuditEventListEventsTotal.Add(ctx, int64(eventCount), attrs)
	}
}

// decodeEventList reads and decodes the audit event list from the request.
func (h *AuditHandler) decodeEventList(r *http.Request) (*auditv1.EventList, error) {
	limited := io.LimitReader(r.Body, h.config.MaxRequestBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("failed to read body: %w", err)
	}
	defer r.Body.Close()
	if int64(len(body)) > h.config.MaxRequestBodyBytes {
		return nil, fmt.Errorf("request body too large: max %d bytes", h.config.MaxRequestBodyBytes)
	}

	var eventListV1 auditv1.EventList
	_, _, err = h.deserializer.Decode(body, nil, &eventListV1)
	if err != nil {
		return nil, fmt.Errorf("invalid audit event list: %w", err)
	}

	return &eventListV1, nil
}

// enqueueDebugEvents preserves each decoded event before normal audit processing can filter it.
func (h *AuditHandler) enqueueDebugEvents(ctx context.Context, source AuditSource, events []auditv1.Event) error {
	if h.config.DebugQueue == nil {
		return nil
	}
	for _, event := range events {
		if err := h.config.DebugQueue.Enqueue(ctx, string(source), event); err != nil {
			return fmt.Errorf("failed to enqueue early audit debug event %q: %w", event.AuditID, err)
		}
	}
	return nil
}

// processEvents processes a list of audit events.
func (h *AuditHandler) processEvents(ctx context.Context, source AuditSource, events []auditv1.Event) error {
	for _, auditEventV1 := range events {
		if err := h.processEvent(ctx, source, auditEventV1); err != nil {
			return err
		}
	}

	return nil
}

func (h *AuditHandler) processEvent(ctx context.Context, source AuditSource, auditEventV1 auditv1.Event) error {
	log := logf.Log.WithName("audit-handler")

	auditEvent, process, err := h.prepareAuditEvent(ctx, source, auditEventV1)
	if err != nil {
		return err
	}
	if !process {
		// A non-/scale subresource (or an unmapped-verb subresource): dropped before Redis.
		outcome.Record(ctx, &auditEventV1, outcome.NonScaleSubresource)
		return nil
	}

	// Only ResponseComplete carries the post-commit object state; earlier
	// stages of the same request never reach the per-type mirror.
	if auditEventV1.Stage != auditv1.StageResponseComplete {
		outcome.Record(ctx, &auditEventV1, outcome.Stage)
		return nil
	}

	// Serialize official-source mirroring within the pod: the per-type streams
	// are RV-keyed, and concurrent handler goroutines appending out of arrival
	// order would divert in-order events (reject them from the main stream).
	if source == AuditSourceOfficial {
		gateStart := time.Now()
		h.canonicalMu.Lock()
		defer h.canonicalMu.Unlock()
		observeOfficialGateWait(ctx, time.Since(gateStart).Seconds())
	}

	quality := classifyAuditEventQuality(source, &auditEventV1)
	decision := classifyAuditIngress(source, &auditEventV1, quality)
	if !decision.Process {
		// decision.Reason is one of the dropped-outcome label values (read_only_or_unknown_verb,
		// failed_request, dry_run, unchanged_resource_version, malformed_additional, nil_event).
		outcome.Record(ctx, &auditEventV1, outcome.Outcome(decision.Reason))
		logAuditJoinSkip(
			"Dropped audit event before join pipeline",
			source,
			h.extractGVR(&auditEvent),
			auditEvent.AuditID,
		)
		return nil
	}

	eventToWrite, joinDecision, shouldEmit, err := h.eventToMirror(
		ctx, source, &auditEventV1, auditEvent, quality,
	)
	if err != nil {
		return err
	}
	if !shouldEmit {
		switch joinDecision.Action {
		case AuditJoinActionParked:
			outcome.Record(ctx, &auditEventV1, outcome.Parked)
		case AuditJoinActionDrop:
			outcome.Record(ctx, &auditEventV1, outcome.ShallowDropped)
		case AuditJoinActionEmit:
			// unreachable: shouldEmit is false here, so the action is never Emit.
		}
		return nil
	}
	// The accepted event's census outcome (queued / a divert / write_error) is recorded by the
	// queue inside Enqueue; a demand-gate drop (not_needed) is recorded by mirrorByType.
	if err := h.captureCommitRequestAuthor(ctx, eventToWrite); err != nil {
		return err
	}
	h.mirrorByType(ctx, eventToWrite)

	h.logFirstAuditEmit(log, source, auditEvent)

	log.V(1).Info("Processed audit event",
		"source", source,
		"gvr", h.extractGVR(&auditEvent),
		"action", auditEvent.Verb,
		"auditID", auditEvent.AuditID,
		"user", effectiveAuditUsername(auditEvent),
		"ips", auditEvent.SourceIPs,
		"userAgent", auditEvent.UserAgent)

	return nil
}

// logFirstAuditRequest emits an Info banner the first time we accept an
// audit POST from each source so operators can see ingress is wired up.
func (h *AuditHandler) logFirstAuditRequest(log logr.Logger, source AuditSource, eventCount int) {
	switch source {
	case AuditSourceOfficial:
		h.firsts.officialRequest.Do(func() {
			log.Info("Received first audit request (official)", "eventCount", eventCount)
		})
	case AuditSourceAdditional:
		h.firsts.additionalRequest.Do(func() {
			log.Info("Received first audit request (additional)", "eventCount", eventCount)
		})
	}
}

// logFirstAuditEmit emits an Info banner the first time an event from each
// source is enqueued onto the canonical stream.
func (h *AuditHandler) logFirstAuditEmit(log logr.Logger, source AuditSource, event audit.Event) {
	switch source {
	case AuditSourceOfficial:
		h.firsts.officialEmit.Do(func() {
			log.Info("First audit event enqueued to canonical stream (official)",
				"auditID", event.AuditID,
				"verb", event.Verb)
		})
	case AuditSourceAdditional:
		h.firsts.additionalEmit.Do(func() {
			log.Info("First audit event enqueued to canonical stream (additional)",
				"auditID", event.AuditID,
				"verb", event.Verb)
		})
	}
}

func (h *AuditHandler) prepareAuditEvent(
	_ context.Context,
	source AuditSource,
	auditEventV1 auditv1.Event,
) (audit.Event, bool, error) {
	var auditEvent audit.Event
	if err := h.scheme.Convert(&auditEventV1, &auditEvent, nil); err != nil {
		return audit.Event{}, false, fmt.Errorf("failed to convert audit event: %w", err)
	}

	process, err := h.checkEvent(&auditEvent)
	if err != nil {
		return audit.Event{}, false, fmt.Errorf("failed to check audit event: %w", err)
	}
	h.logAuditEventReceived(source, auditEvent)
	return auditEvent, process, nil
}

// logAuditEventReceived emits the structured "audit event received" logs (and the first-
// impersonation banner). The per-event count is recorded once downstream as the event's
// outcome on gitopsreverser_audit_events_total (internal/audit/outcome), so there is no
// separate received counter here.
func (h *AuditHandler) logAuditEventReceived(
	source AuditSource,
	auditEvent audit.Event,
) {
	handlerLog := logf.Log.WithName("audit-handler")
	if auditEvent.ImpersonatedUser != nil {
		h.firsts.impersonatedEvent.Do(func() {
			handlerLog.Info("First impersonated audit event observed",
				"source", source,
				"authUser", auditEvent.User.Username,
				"impersonatedUser", auditEvent.ImpersonatedUser.Username)
		})
		handlerLog.V(1).Info("Audit event impersonated",
			"source", source,
			"authUser", auditEvent.User.Username,
			"impersonatedUser", auditEvent.ImpersonatedUser,
		)
	}

	// The username is intentionally not logged as a metric label (cardinality bomb); it
	// stays in the structured logs only.
	group, version, resource := gvrParts(&auditEvent)
	subresource := subresourcePart(&auditEvent)
	handlerLog.V(1).Info("Audit event received",
		"source", source,
		"group", group,
		"version", version,
		"resource", resource,
		"subresource", subresource,
		"verb", auditEvent.Verb,
		"user", effectiveAuditUsername(auditEvent))
}

func (h *AuditHandler) eventToMirror(
	ctx context.Context,
	source AuditSource,
	auditEventV1 *auditv1.Event,
	auditEvent audit.Event,
	quality AuditEventQuality,
) (*auditv1.Event, AuditJoinDecision, bool, error) {
	if h.config.Joiner == nil {
		emit := quality == AuditEventQualityComplete ||
			quality == AuditEventQualityBodyShallowDeletable ||
			quality == AuditEventQualityCollection
		if !emit {
			// An identity-shallow event with no joiner to await a body is a shallow drop. Return an
			// explicit Drop action: the zero-value AuditJoinAction is Parked, which the caller would
			// otherwise mis-record as outcome.Parked instead of outcome.ShallowDropped.
			return nil, AuditJoinDecision{Action: AuditJoinActionDrop}, false, nil
		}
		return auditEventV1, AuditJoinDecision{Action: AuditJoinActionEmit}, true, nil
	}

	decision, err := h.config.Joiner.Decide(ctx, source, auditEventV1, quality)
	if err != nil {
		return nil, AuditJoinDecision{}, false, fmt.Errorf(
			"failed to decide audit event %q: %w",
			auditEvent.AuditID,
			err,
		)
	}
	switch decision.Action {
	case AuditJoinActionParked:
		logAuditJoinSkip("Parked additional audit body", source, h.extractGVR(&auditEvent), auditEvent.AuditID)
		return nil, decision, false, nil
	case AuditJoinActionDrop:
		logAuditJoinSkip(
			"Dropped audit event before canonical stream enqueue",
			source,
			h.extractGVR(&auditEvent),
			auditEvent.AuditID,
		)
		return nil, decision, false, nil
	case AuditJoinActionEmit:
		if decision.Event == nil {
			return nil, decision, false, fmt.Errorf("joiner emitted nil audit event for %q", auditEvent.AuditID)
		}
		return decision.Event, decision, true, nil
	default:
		return nil, decision, false, fmt.Errorf(
			"unknown audit join action %d for %q",
			decision.Action,
			auditEvent.AuditID,
		)
	}
}

func (h *AuditHandler) captureCommitRequestAuthor(ctx context.Context, event *auditv1.Event) error {
	if h.config.CommitRequestAuthors == nil {
		return nil
	}
	if _, err := h.config.CommitRequestAuthors.CaptureCommitRequestAuthor(ctx, *event); err != nil {
		return err
	}
	return nil
}

// mirrorByType best-effort mirrors the accepted, body-merged event into its
// per-resource-type stream. It only ever sees StageResponseComplete events
// (earlier stages are dropped upstream). A failure here must never fail the
// audit request; the first failure is logged prominently, the rest at V(1).
func (h *AuditHandler) mirrorByType(ctx context.Context, event *auditv1.Event) {
	if h.config.ByTypeQueue == nil {
		return
	}
	if !h.mirrorGateAllows(event) {
		// The type is not currently wanted (no live claim ∩ followable). Skipping the mirror
		// here is the whole point of demand-gating; a brief miss after a fresh Require is healed
		// by the next checkpoint (docs/finished/demand-gated-audit-ingestion.md §6).
		outcome.Record(ctx, event, outcome.NotNeeded)
		return
	}
	// queued / a divert / write_error are recorded once by Enqueue (the queue is the single owner
	// of queue-side outcomes); here we only surface a write failure to the operator log.
	if err := h.config.ByTypeQueue.Enqueue(ctx, *event); err != nil {
		log := logf.Log.WithName("audit-handler")
		h.firsts.byTypeMirrorError.Do(func() {
			log.Error(err,
				"Failed to mirror audit event to per-resource-type streams "+
					"(logged once; later failures at V(1))",
				"auditID", event.AuditID)
		})
		log.V(1).Info("Failed to mirror audit event to per-resource-type streams",
			"auditID", event.AuditID, "error", err.Error())
	}
}

// mirrorGateAllows reports whether the event's type is currently wanted. A nil gate means gating is
// disabled (mirror everything — the pre-gate behaviour). The gate keys on the PARENT (group,
// resource): a scale subresource folds to its parent in the mirror's baseKey, and Allow is keyed
// the same way, so passing the objectRef's group/resource is correct for both scale and non-scale
// events. A missing objectRef (the __unknown__ bucket) is never wanted.
func (h *AuditHandler) mirrorGateAllows(event *auditv1.Event) bool {
	if h.config.MirrorGate == nil {
		return true
	}
	ref := event.ObjectRef
	if ref == nil {
		return false
	}
	return h.config.MirrorGate.Allow(ref.APIGroup, ref.Resource)
}

func effectiveAuditUsername(event audit.Event) string {
	if event.ImpersonatedUser != nil && event.ImpersonatedUser.Username != "" {
		return event.ImpersonatedUser.Username
	}
	return event.User.Username
}

func logAuditJoinSkip(message string, source AuditSource, gvr string, auditID types.UID) {
	logf.Log.WithName("audit-handler").V(1).Info(message,
		"source", source,
		"gvr", gvr,
		"auditID", auditID)
}

func auditSourceFromPath(path string) (AuditSource, error) {
	switch path {
	case "/audit-webhook":
		return AuditSourceOfficial, nil
	case "/audit-webhook-additional":
		return AuditSourceAdditional, nil
	case "/audit-webhook/", "/audit-webhook-additional/":
		return "", errors.New("audit webhook path must not include a trailing slash")
	default:
		if strings.HasPrefix(path, "/audit-webhook/") || strings.HasPrefix(path, "/audit-webhook-additional/") {
			return "", errors.New("audit webhook path must not include a cluster ID or extra path segment")
		}
		return "", errors.New("invalid path; expected /audit-webhook or /audit-webhook-additional")
	}
}

// gvrParts splits an audit event's objectRef into bounded group/version/resource
// metric label values. An empty group denotes the core API group; an absent or
// unparseable ref collapses to "unknown"/"invalid" so the label set stays small.
func gvrParts(event *audit.Event) (string, string, string) {
	if event.ObjectRef == nil || event.ObjectRef.APIVersion == "" {
		return "unknown", "unknown", "unknown"
	}
	gv, err := schema.ParseGroupVersion(event.ObjectRef.APIVersion)
	if err != nil {
		return "invalid", event.ObjectRef.APIVersion, orUnknownResource(event.ObjectRef.Resource)
	}
	return gv.Group, gv.Version, orUnknownResource(event.ObjectRef.Resource)
}

// subresourcePart returns the audit event's objectRef.subresource, or "" when
// the event has no objectRef or targets a top-level resource. Kubernetes
// subresources are a bounded, closed set (status, scale, exec, log, ...), so
// this is safe to use as a metric label.
func subresourcePart(event *audit.Event) string {
	if event.ObjectRef == nil {
		return ""
	}
	return event.ObjectRef.Subresource
}

func orUnknownResource(resource string) string {
	if resource == "" {
		return "unknown"
	}
	return resource
}

// extractGVR constructs the Group/Version/Resource string from the audit event
// for use as a log field.
func (h *AuditHandler) extractGVR(event *audit.Event) string {
	group, version, resource := gvrParts(event)
	if group == "" {
		return fmt.Sprintf("/%s/%s", version, resource)
	}
	return fmt.Sprintf("%s/%s/%s", group, version, resource)
}

// classifyAuditIngress is the intrinsic gate in front of the join pipeline. It
// decides accept/reject from the event alone — stage, verb, response status, and
// body shape — and never consults WatchRules: rule relevance is a consumer-side
// concern applied later, when an event is turned into a Git write.
//
//   - A non-ResponseComplete stage, or a read-only/unknown verb (get/list/watch),
//     is never a Git-relevant mutation, so it is rejected for both sources.
//   - A request the API server rejected (responseStatus.code >= 300, e.g. a 409
//     Conflict from an optimistic-concurrency failure) changed nothing in etcd.
//     Its responseObject is a metav1.Status error body, not the resource, so it
//     must never reach Git; it is rejected for both sources.
//   - A dry-run request (`dryRun=All`) completed admission/defaulting but was not
//     persisted, so it is rejected for both sources before it can reach the
//     per-type log at all.
//   - A mutation-shaped update/patch whose request-side RV equals the response RV
//     did not advance stored object state. Re-applying such unchanged objects
//     produces stale RVs that would be diverted as noise, so it is rejected for both
//     sources.
//   - An additional-source event is only worth parking when it actually carries a
//     request/response body; a shallow (malformed) one has nothing to contribute.
//
// Everything else — including every shallow official event — is accepted, so the
// joiner can wait for and merge a proxy-supplied body for it.
func classifyAuditIngress(
	source AuditSource,
	event *auditv1.Event,
	quality AuditEventQuality,
) auditIngressDecision {
	switch {
	case event == nil:
		return auditIngressDecision{Reason: "nil_event"}
	case event.Stage != auditv1.StageResponseComplete:
		return auditIngressDecision{Reason: "stage"}
	}
	if _, ok := auditutil.VerbToOperation(event.Verb); !ok {
		return auditIngressDecision{Reason: "read_only_or_unknown_verb"}
	}
	if isFailedAuditRequest(event) {
		return auditIngressDecision{Reason: "failed_request"}
	}
	if isDryRunAllRequest(event) {
		return auditIngressDecision{Reason: "dry_run"}
	}
	if hasUnchangedResourceVersion(event) {
		return auditIngressDecision{Reason: "unchanged_resource_version"}
	}
	if source == AuditSourceAdditional && quality == AuditEventQualityMalformed {
		return auditIngressDecision{Reason: "malformed_additional"}
	}
	return auditIngressDecision{Process: true}
}

// isFailedAuditRequest reports whether the API server rejected the request the
// audit event describes. A non-success responseStatus.code (>= 300) means the
// mutation never reached etcd, and the event's responseObject is a metav1.Status
// error body rather than the resource. The status code is left at zero by some
// additional-source proxies; that is treated as success here, because the
// matching official event — which always carries responseStatus — is the one
// that drives the canonical stream.
func isFailedAuditRequest(event *auditv1.Event) bool {
	return event != nil && event.ResponseStatus != nil && event.ResponseStatus.Code >= 300
}

func isDryRunAllRequest(event *auditv1.Event) bool {
	if event == nil || event.RequestURI == "" {
		return false
	}
	parsed, err := url.Parse(event.RequestURI)
	if err != nil {
		return false
	}
	for _, value := range parsed.Query()["dryRun"] {
		if value == metav1.DryRunAll {
			return true
		}
	}
	return false
}

func hasUnchangedResourceVersion(event *auditv1.Event) bool {
	if event == nil {
		return false
	}
	responseRV := metadataResourceVersion(event.ResponseObject)
	if responseRV == "" {
		return false
	}
	requestRV := metadataResourceVersion(event.RequestObject)
	return requestRV != "" && requestRV == responseRV
}

func metadataResourceVersion(object *runtime.Unknown) string {
	if object == nil || len(object.Raw) == 0 {
		return ""
	}
	var probe struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(object.Raw, &probe); err != nil {
		return ""
	}
	return probe.Metadata.ResourceVersion
}

// checkEvent validates an audit event before processing.
func (h *AuditHandler) checkEvent(event *audit.Event) (bool, error) {
	process := shouldForwardSubresource(event)
	if string(event.AuditID) == "" {
		return process, errors.New("invalid audit event: auditID cannot be empty")
	}

	return process, nil
}

// shouldForwardSubresource is the cheap subresource forwarding gate. Top-level
// resource events always pass. A subresource event passes only when it is a mutating
// /scale — the single subresource GitOps Reverser mirrors — so a deployments/scale
// event reaches the consumer to be translated into a parent-manifest replicas patch,
// while status, exec, proxy, log, and every other subresource is dropped before Redis.
// The consumer remains the authority for whether a forwarded scale can actually be
// resolved (it drops a scale whose parent replica path is unknown). See
// docs/design/manifest/version2/subresource-scope-reduction.md.
func shouldForwardSubresource(event *audit.Event) bool {
	if event.ObjectRef == nil || event.ObjectRef.Subresource == "" {
		return true
	}
	if _, ok := auditutil.VerbToOperation(event.Verb); !ok {
		return false
	}
	return auditutil.IsScaleSubresource(event.ObjectRef.Subresource)
}

func hasAuditV1ObjectBody(event *auditv1.Event) bool {
	return event != nil && (hasRuntimeUnknownBody(event.RequestObject) || hasRuntimeUnknownBody(event.ResponseObject))
}

func hasRuntimeUnknownBody(object *runtime.Unknown) bool {
	return object != nil && len(object.Raw) > 0
}
