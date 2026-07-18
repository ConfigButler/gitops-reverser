// SPDX-License-Identifier: Apache-2.0

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
	"k8s.io/apiserver/pkg/apis/audit"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ConfigButler/gitops-reverser/internal/audit/outcome"
	"github.com/ConfigButler/gitops-reverser/internal/auditutil"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// DefaultAuditMaxRequestBodyBytes limits incoming audit payload size.
const DefaultAuditMaxRequestBodyBytes = int64(10 * 1024 * 1024)

// auditHandlerFirsts holds one-shot startup-milestone log gates. Per-event Info
// logs are noisy in steady state; these surface the first time the pipeline crosses
// each transition so operators can see ingress is wired up at a glance.
type auditHandlerFirsts struct {
	request           sync.Once
	factRecorded      sync.Once
	impersonatedEvent sync.Once
	unroutableEvent   sync.Once
}

// AuditFactRecorder stores the minimal author-attribution fact for one accepted,
// mutating audit event under a SOURCE CLUSTER (a ClusterProvider name), so a fact from
// one cluster never joins a watch event from another. It is the only thing the audit
// webhook does now: watch carries the object body, so audit is a pure attribution lookup
// table. A nil recorder means configured-author mode — the handler is not wired at all.
type AuditFactRecorder interface {
	RecordFact(ctx context.Context, providerName string, event auditv1.Event) error
}

// AuditProviderResolver reports whether a named source cluster (a ClusterProvider) exists, so an
// /audit-webhook/<name> route is accepted only for a configured cluster. It is the gate behind the
// connection's mTLS: the audit server already requires a CA-signed client cert
// (RequireAndVerifyClientCert), so an unauthenticated apiserver never reaches here; this then
// refuses a route for a provider that does not exist, rather than accumulating orphan facts. Every
// name is gated the same way, "default" included. A nil resolver means no route can be served.
type AuditProviderResolver interface {
	ProviderExists(ctx context.Context, name string) (bool, error)
}

// AuditHandlerConfig contains configuration for the audit handler.
type AuditHandlerConfig struct {
	// MaxRequestBodyBytes is the maximum accepted HTTP request body size.
	MaxRequestBodyBytes int64
	// FactRecorder persists the attribution fact for each accepted, mutating event.
	// A write failure returns an audit-request error so the API server retries
	// delivery; mirrored-resource author attribution depends on these facts.
	FactRecorder AuditFactRecorder
	// ProviderResolver gates every /audit-webhook/<name> route on the ClusterProvider existing.
	// Nil means named routes are all 404.
	ProviderResolver AuditProviderResolver
	// ClusterAnnotationKey enables the bare /audit-webhook endpoint for a SHARED audit stream that
	// carries several logical clusters: the owning ClusterProvider is read PER EVENT from this
	// audit-event annotation, so one batch may fan out to several source clusters. Empty (the
	// default) means the bare endpoint is NOT enabled and every producer must post to a named
	// /audit-webhook/<name>. Setting it requires a ProviderResolver — an annotation naming an
	// unknown provider must be rejected, never guessed.
	ClusterAnnotationKey string
}

// AuditHandler receives kube-apiserver audit events on /audit-webhook and records
// the author-attribution fact for each Git-relevant mutation. It never writes object
// state — that comes from WATCH — so it decodes, applies the intrinsic accept gate
// (stage, verb, success, dry-run, unchanged-RV, subresource), and records the fact.
type AuditHandler struct {
	deserializer runtime.Decoder
	config       AuditHandlerConfig
	firsts       auditHandlerFirsts
}

// NewAuditHandler creates a new audit handler with the given configuration.
func NewAuditHandler(config AuditHandlerConfig) (*AuditHandler, error) {
	if config.MaxRequestBodyBytes <= 0 {
		config.MaxRequestBodyBytes = DefaultAuditMaxRequestBodyBytes
	}
	// Annotation routing must be able to reject an unknown provider, so it cannot run without a
	// resolver. Fail at startup rather than 400-ing every bare request at runtime.
	if config.ClusterAnnotationKey != "" && config.ProviderResolver == nil {
		return nil, errors.New("cluster annotation routing requires a ProviderResolver")
	}

	scheme := runtime.NewScheme()
	if err := audit.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to initialize scheme: %w", err)
	}
	if err := auditv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add v1 types to scheme: %w", err)
	}

	codecs := serializer.NewCodecFactory(scheme)
	return &AuditHandler{
		deserializer: codecs.UniversalDeserializer(),
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

	if err := validateAuditWebhookPath(r.URL.Path); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	route, ok := h.resolveRoute(ctx, w, r)
	if !ok {
		return
	}

	start := time.Now()
	result, eventCount := h.serveEventListRequest(ctx, route, w, r, log)
	h.recordEventListRequest(ctx, result, eventCount, time.Since(start))
}

// resolveRoute maps an already-syntactically-valid path to the route its events belong to. It
// writes the rejection itself and reports ok=false when the request cannot be served at all — as
// opposed to a per-event rejection, which still returns 200 for the rest of the batch.
func (h *AuditHandler) resolveRoute(ctx context.Context, w http.ResponseWriter, r *http.Request) (auditRoute, bool) {
	providerName, named := providerRouteForPath(r.URL.Path)
	if !named {
		// The bare endpoint never guesses a source cluster. It exists only to demultiplex a SHARED
		// stream by annotation, so without a configured key there is nothing it could mean and the
		// producer is simply misconfigured: reject the whole request rather than silently dropping
		// every event in it.
		if h.config.ClusterAnnotationKey == "" {
			http.Error(w, "the bare /audit-webhook endpoint is not enabled; post to "+
				"/audit-webhook/<cluster-provider-name>", http.StatusBadRequest)
			return auditRoute{}, false
		}
		return auditRoute{annotationKey: h.config.ClusterAnnotationKey}, true
	}

	// The connection is already CA-authenticated (the audit server requires a client cert), so gate
	// on the named ClusterProvider existing rather than accepting facts for an unknown cluster.
	// Every name is gated, "default" included — it is an ordinary provider.
	if h.config.ProviderResolver == nil {
		http.NotFound(w, r)
		return auditRoute{}, false
	}
	exists, err := h.config.ProviderResolver.ProviderExists(ctx, providerName)
	if err != nil {
		http.Error(w, "resolve source cluster", http.StatusServiceUnavailable)
		return auditRoute{}, false
	}
	if !exists {
		http.NotFound(w, r)
		return auditRoute{}, false
	}
	return auditRoute{provider: providerName}, true
}

// auditRoute is how one accepted request maps its events to SOURCE CLUSTERS. Exactly one field is
// set: a named /audit-webhook/<name> route fixes `provider` for the whole batch, while the bare
// /audit-webhook route carries `annotationKey` and resolves a provider PER EVENT, so a single batch
// from a shared stream may fan out to several source clusters.
type auditRoute struct {
	provider      string
	annotationKey string
}

// providerRouteForPath maps an accepted audit route to the SOURCE CLUSTER its facts belong to and
// whether it is a NAMED route. Audit routes are named: /audit-webhook/<name> carries the provider
// (named=true), including /audit-webhook/default. The bare /audit-webhook names no provider —
// it is the shared, annotation-routed endpoint, so it returns ("", false) and the caller resolves
// each event separately. validateAuditWebhookPath has already accepted the path syntactically.
func providerRouteForPath(path string) (string, bool) {
	segment := strings.TrimPrefix(path, "/audit-webhook/")
	if segment == path || segment == "" {
		return "", false
	}
	return segment, true
}

// serveEventListRequest decodes and processes one EventList request, returning the
// bounded outcome and the number of decoded event items for the ingress metrics.
func (h *AuditHandler) serveEventListRequest(
	ctx context.Context,
	route auditRoute,
	w http.ResponseWriter,
	r *http.Request,
	log logr.Logger,
) (string, int) {
	reqLog := log.WithValues("remoteAddr", r.RemoteAddr, "path", r.URL.Path)

	eventListV1, err := h.decodeEventList(r)
	if err != nil {
		reqLog.Error(err, "Failed to decode audit event list")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return outcomeDecodeError, 0
	}

	eventCount := len(eventListV1.Items)
	if eventCount == 0 {
		reqLog.Info("Received empty audit event list", "eventCount", 0, "processingOutcome", "empty")
		h.writeResponse(w, reqLog, "Empty event list processed")
		return outcomeEmpty, 0
	}

	h.firsts.request.Do(func() {
		reqLog.Info("Received first audit request", "eventCount", eventCount)
	})

	if err := h.processEvents(ctx, route, eventListV1.Items); err != nil {
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
	result string,
	eventCount int,
	elapsed time.Duration,
) {
	attrs := metric.WithAttributes(attribute.String("outcome", result))
	if telemetry.AuditEventListsTotal != nil {
		telemetry.AuditEventListsTotal.Add(ctx, 1, attrs)
	}
	if telemetry.AuditEventListDurationSeconds != nil {
		telemetry.AuditEventListDurationSeconds.Record(ctx, elapsed.Seconds(), attrs)
	}
	if result != outcomeDecodeError && telemetry.AuditEventListEventsTotal != nil {
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
	if _, _, err = h.deserializer.Decode(body, nil, &eventListV1); err != nil {
		return nil, fmt.Errorf("invalid audit event list: %w", err)
	}

	return &eventListV1, nil
}

// processEvents processes a list of audit events for one route. On the annotation-routed bare
// endpoint the events in one list may belong to different source clusters, so each is resolved
// independently and an unroutable event only drops itself.
func (h *AuditHandler) processEvents(ctx context.Context, route auditRoute, events []auditv1.Event) error {
	for i := range events {
		if err := h.processEvent(ctx, route, events[i]); err != nil {
			return err
		}
	}
	return nil
}

// processEvent applies the intrinsic accept gate, resolves the event's source cluster, and records
// the attribution fact for an accepted, mutating event. A rejected event is recorded with its
// terminal outcome and dropped; only a fact-store or provider-lookup failure returns an error (the
// API server then retries delivery).
func (h *AuditHandler) processEvent(ctx context.Context, route auditRoute, event auditv1.Event) error {
	log := logf.Log.WithName("audit-handler")
	h.logAuditEventReceived(event)

	if !shouldForwardSubresource(&event) {
		// A non-/scale subresource (or an unmapped-verb subresource): dropped before recording.
		outcome.Record(ctx, &event, outcome.NonScaleSubresource)
		return nil
	}

	if decision := classifyAuditIngress(&event); !decision.Process {
		outcome.Record(ctx, &event, outcome.Outcome(decision.Reason))
		log.V(1).Info("Dropped audit event before recording",
			"reason", decision.Reason, "gvr", extractGVR(&event), "auditID", event.AuditID)
		return nil
	}

	providerName, routed, err := h.resolveEventProvider(ctx, route, &event)
	if err != nil {
		return err
	}
	if !routed {
		return nil
	}

	if h.config.FactRecorder != nil {
		if err := h.config.FactRecorder.RecordFact(ctx, providerName, event); err != nil {
			outcome.Record(ctx, &event, outcome.WriteError)
			return fmt.Errorf("record attribution fact %q: %w", event.AuditID, err)
		}
	}
	outcome.Record(ctx, &event, outcome.Queued)

	h.firsts.factRecorded.Do(func() {
		log.Info("Recorded first audit attribution fact", "auditID", event.AuditID, "verb", event.Verb)
	})
	log.V(1).Info("Recorded audit attribution fact",
		"gvr", extractGVR(&event), "verb", event.Verb, "auditID", event.AuditID,
		"user", effectiveAuditUsername(event))
	return nil
}

// resolveEventProvider returns the ClusterProvider that owns one accepted event, and whether it
// routed at all. On a named route that is the route's provider, unconditionally. On the shared,
// annotation-routed bare endpoint it is read from the event's own annotations and existence-checked:
// an event carrying no annotation, or naming a ClusterProvider that does not exist, is REJECTED —
// it produces no fact and is never credited to a fallback provider, because a wrong source cluster
// would let a user from one logical cluster author a matching object in another. The rejection is
// per EVENT so correctly-annotated events in the same batch still land. A resolver failure is
// transient rather than a verdict, so it is returned as an error and the API server retries the
// whole batch.
func (h *AuditHandler) resolveEventProvider(
	ctx context.Context,
	route auditRoute,
	event *auditv1.Event,
) (string, bool, error) {
	if route.annotationKey == "" {
		return route.provider, true, nil
	}

	name := event.Annotations[route.annotationKey]
	if name == "" {
		h.rejectUnroutableEvent(ctx, event, outcome.MissingClusterAnnotation, route.annotationKey, name)
		return "", false, nil
	}

	exists, err := h.config.ProviderResolver.ProviderExists(ctx, name)
	if err != nil {
		return "", false, fmt.Errorf("resolve source cluster %q: %w", name, err)
	}
	if !exists {
		h.rejectUnroutableEvent(ctx, event, outcome.UnknownClusterProvider, route.annotationKey, name)
		return "", false, nil
	}
	return name, true, nil
}

// rejectUnroutableEvent counts and logs one event the shared endpoint could not route. Counting it
// on the ordinary per-event outcome counter is the point: a producer that is not stamping the
// annotation shows up as a rising drop rate rather than as silence. The first rejection is logged
// at Info so the misconfiguration is visible without reading metrics; the rest stay at V(1) so a
// steady stream of them cannot flood the log.
func (h *AuditHandler) rejectUnroutableEvent(
	ctx context.Context,
	event *auditv1.Event,
	reason outcome.Outcome,
	annotationKey string,
	sourceCluster string,
) {
	outcome.Record(ctx, event, reason)

	log := logf.Log.WithName("audit-handler")
	fields := []any{
		"reason", string(reason),
		"annotationKey", annotationKey,
		"sourceCluster", sourceCluster,
		"auditID", event.AuditID,
		"gvr", extractGVR(event),
	}
	h.firsts.unroutableEvent.Do(func() {
		log.Info("Rejected an unroutable audit event on the shared /audit-webhook endpoint; it names no "+
			"existing ClusterProvider and is never credited to a fallback. Stamp the annotation, or point "+
			"this producer at /audit-webhook/<cluster-provider-name>", fields...)
	})
	log.V(1).Info("Rejected unroutable audit event", fields...)
}

// logAuditEventReceived emits the structured "audit event received" log (and the
// first-impersonation banner). The per-event count is recorded once as the event's
// outcome on gitopsreverser_audit_events_total, so there is no separate counter here.
func (h *AuditHandler) logAuditEventReceived(event auditv1.Event) {
	log := logf.Log.WithName("audit-handler")
	if event.ImpersonatedUser != nil {
		h.firsts.impersonatedEvent.Do(func() {
			log.Info("First impersonated audit event observed",
				"authUser", event.User.Username, "impersonatedUser", event.ImpersonatedUser.Username)
		})
	}
	group, version, resource := gvrParts(&event)
	log.V(1).Info("Audit event received",
		"group", group, "version", version, "resource", resource,
		"subresource", subresourcePart(&event), "verb", event.Verb, "user", effectiveAuditUsername(event))
}

// auditIngressDecision is the intrinsic accept/reject verdict for an audit event,
// derived purely from the event — stage, verb, status, body shape. Reason labels a
// rejection for drop diagnostics; it carries no knowledge of WatchRules.
type auditIngressDecision struct {
	Process bool
	Reason  string
}

// classifyAuditIngress is the intrinsic gate. It decides accept/reject from the
// event alone — stage, verb, response status, and body shape — and never consults
// WatchRules.
//
//   - A non-ResponseComplete stage, or a read-only/unknown verb (get/list/watch),
//     is never a Git-relevant mutation, so it is rejected.
//   - A request the API server rejected (responseStatus.code >= 300, e.g. a 409
//     Conflict) changed nothing in etcd, so it is rejected.
//   - A dry-run request (`dryRun=All`) was not persisted, so it is rejected.
//   - A mutation-shaped update/patch whose request-side RV equals the response RV
//     did not advance stored object state, so it is rejected as noise.
func classifyAuditIngress(event *auditv1.Event) auditIngressDecision {
	switch {
	case event == nil:
		return auditIngressDecision{Reason: string(outcome.NilEvent)}
	case event.Stage != auditv1.StageResponseComplete:
		return auditIngressDecision{Reason: string(outcome.Stage)}
	}
	if _, ok := auditutil.VerbToOperation(event.Verb); !ok {
		return auditIngressDecision{Reason: string(outcome.ReadOnlyOrUnknownVerb)}
	}
	if isFailedAuditRequest(event) {
		return auditIngressDecision{Reason: string(outcome.FailedRequest)}
	}
	if isDryRunAllRequest(event) {
		return auditIngressDecision{Reason: string(outcome.DryRun)}
	}
	if hasUnchangedResourceVersion(event) {
		return auditIngressDecision{Reason: string(outcome.UnchangedResourceVersion)}
	}
	return auditIngressDecision{Process: true}
}

// isFailedAuditRequest reports whether the API server rejected the request the
// audit event describes. A non-success responseStatus.code (>= 300) means the
// mutation never reached etcd, and the event's responseObject is a metav1.Status
// error body rather than the resource.
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

// shouldForwardSubresource is the cheap subresource forwarding gate. Top-level
// resource events always pass. A subresource event passes only when it is a mutating
// /scale — the single subresource whose author is worth attributing to the parent
// (the parent watch event lands at the scale response RV); status, exec, log, and
// every other subresource is dropped before recording. Object state itself comes from
// watch, so /scale is forwarded only to name who scaled the parent, never to capture
// the replica count.
func shouldForwardSubresource(event *auditv1.Event) bool {
	if event.ObjectRef == nil || event.ObjectRef.Subresource == "" {
		return true
	}
	if _, ok := auditutil.VerbToOperation(event.Verb); !ok {
		return false
	}
	return event.ObjectRef.Subresource == "scale"
}

func effectiveAuditUsername(event auditv1.Event) string {
	if event.ImpersonatedUser != nil && event.ImpersonatedUser.Username != "" {
		return event.ImpersonatedUser.Username
	}
	return event.User.Username
}

// validateAuditWebhookPath accepts the bare shared /audit-webhook and a single-segment
// /audit-webhook/<cluster-provider-name>. It is a purely SYNTACTIC check; whether the named provider
// actually exists, and whether the bare endpoint is enabled at all, is enforced in ServeHTTP. It
// rejects a trailing slash and any extra path segment.
func validateAuditWebhookPath(path string) error {
	if path == "/audit-webhook" {
		return nil
	}
	if !strings.HasPrefix(path, "/audit-webhook/") {
		return errors.New("invalid path; expected /audit-webhook or /audit-webhook/<cluster-provider-name>")
	}
	segment := strings.TrimPrefix(path, "/audit-webhook/")
	if segment == "" {
		return errors.New("audit webhook path must not include a trailing slash")
	}
	if strings.Contains(segment, "/") {
		return errors.New("audit webhook path must name exactly one ClusterProvider: /audit-webhook/<name>")
	}
	return nil
}

// gvrParts splits an audit event's objectRef into bounded group/version/resource
// metric label values. An empty group denotes the core API group; an absent or
// unparseable ref collapses to "unknown"/"invalid" so the label set stays small.
func gvrParts(event *auditv1.Event) (string, string, string) {
	if event.ObjectRef == nil || event.ObjectRef.APIVersion == "" {
		return "unknown", "unknown", "unknown"
	}
	gv, err := schema.ParseGroupVersion(event.ObjectRef.APIVersion)
	if err != nil {
		return "invalid", event.ObjectRef.APIVersion, orUnknownResource(event.ObjectRef.Resource)
	}
	return gv.Group, gv.Version, orUnknownResource(event.ObjectRef.Resource)
}

// subresourcePart returns the audit event's objectRef.subresource, or "" when the
// event has no objectRef or targets a top-level resource.
func subresourcePart(event *auditv1.Event) string {
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
func extractGVR(event *auditv1.Event) string {
	group, version, resource := gvrParts(event)
	if group == "" {
		return fmt.Sprintf("/%s/%s", version, resource)
	}
	return fmt.Sprintf("%s/%s/%s", group, version, resource)
}
