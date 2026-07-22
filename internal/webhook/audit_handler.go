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

// AuditFactRecorder stores the minimal author-attribution fact for one accepted, mutating audit
// event under the AUDIT ROUTE it arrived on, so a fact from one cluster never joins a watch event
// from another. It is the only thing the audit webhook does now: watch carries the object body, so
// audit is a pure attribution lookup table. A nil recorder means configured-author mode — the
// handler is not wired at all.
type AuditFactRecorder interface {
	RecordFact(ctx context.Context, auditRoute string, event auditv1.Event) error
}

// AuditHandlerConfig contains configuration for the audit handler.
type AuditHandlerConfig struct {
	// MaxRequestBodyBytes is the maximum accepted HTTP request body size.
	MaxRequestBodyBytes int64
	// FactRecorder persists the attribution fact for each accepted, mutating event.
	// A write failure returns an audit-request error so the API server retries
	// delivery; mirrored-resource author attribution depends on these facts.
	FactRecorder AuditFactRecorder
	// AuditRouteAnnotationKey enables the bare /audit-webhook endpoint for a SHARED audit stream
	// that carries several logical clusters: the AUDIT ROUTE is read PER EVENT from this
	// audit-event annotation, so one batch may fan out to several routes. Empty (the default) means
	// the bare endpoint is NOT enabled and every producer must post to a named
	// /audit-webhook/<audit-route>.
	AuditRouteAnnotationKey string
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

	route, ok := h.resolveRoute(w, r)
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
func (h *AuditHandler) resolveRoute(w http.ResponseWriter, r *http.Request) (auditRoute, bool) {
	route, named := auditRouteForPath(r.URL.Path)
	if !named {
		// The bare endpoint never guesses a source cluster. It exists only to demultiplex a SHARED
		// stream by annotation, so without a configured key there is nothing it could mean and the
		// producer is simply misconfigured: reject the whole request rather than silently dropping
		// every event in it.
		if h.config.AuditRouteAnnotationKey == "" {
			http.Error(w, "the bare /audit-webhook endpoint is not enabled; post to "+
				"/audit-webhook/<audit-route>", http.StatusBadRequest)
			return auditRoute{}, false
		}
		return auditRoute{annotationKey: h.config.AuditRouteAnnotationKey}, true
	}

	// A named route is accepted as-is. Ingestion does NOT check that a ClusterProvider carries this
	// route: the route is a partition name, not a claim about an object, and a fact filed under a
	// route nobody reads costs one key that expires on the fact TTL. Refusing here instead dropped
	// audit batches in flight while a provider was being created or recreated, which the API server
	// does not retry after a 404. The connection is already CA-authenticated (the audit server
	// requires a client cert), which is the boundary that keeps an unauthenticated producer out.
	return auditRoute{route: route}, true
}

// auditRoute is how one accepted request maps its events to AUDIT ROUTES, the dimension the
// attribution facts are partitioned by. Exactly one field is set: a named /audit-webhook/<route>
// path fixes `route` for the whole batch, while the bare /audit-webhook carries `annotationKey` and
// reads the route PER EVENT, so a single batch from a shared stream may fan out to several routes.
type auditRoute struct {
	route         string
	annotationKey string
}

// auditRouteForPath maps an accepted request path to the AUDIT ROUTE its facts belong to and
// whether the path named one. /audit-webhook/<route> carries the route (named=true). The bare
// /audit-webhook names none — it is the shared, annotation-routed endpoint, so it returns
// ("", false) and the caller reads each event's route separately. validateAuditWebhookPath has
// already accepted the path syntactically.
func auditRouteForPath(path string) (string, bool) {
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

	eventRoute, routed := h.resolveEventRoute(ctx, route, &event)
	if !routed {
		return nil
	}

	if h.config.FactRecorder != nil {
		if err := h.config.FactRecorder.RecordFact(ctx, eventRoute, event); err != nil {
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

// resolveEventRoute returns the AUDIT ROUTE one accepted event's fact is filed under, and whether it
// routed at all. On a named route that is the route's own value, unconditionally. On the shared,
// annotation-routed bare endpoint it is read from the event's own annotations, and an event carrying
// no annotation is REJECTED: it produces no fact and is never credited to a fallback route, because
// a wrong route would let a user from one logical cluster author a matching object in another. The
// rejection is per EVENT so correctly-annotated events in the same batch still land.
//
// An annotation naming a route no ClusterProvider has declared is NOT rejected. The route is a
// partition name rather than a claim about an object, so the fact is stored and expires unread if
// nothing ever joins it. That keeps ingestion free of Kubernetes reads on the hot path, and keeps a
// provider that is created (or recreated) after its events have started flowing from losing them.
func (h *AuditHandler) resolveEventRoute(
	ctx context.Context,
	route auditRoute,
	event *auditv1.Event,
) (string, bool) {
	if route.annotationKey == "" {
		return route.route, true
	}

	name := event.Annotations[route.annotationKey]
	if name == "" {
		h.rejectUnroutableEvent(ctx, event, outcome.MissingClusterAnnotation, route.annotationKey, name)
		return "", false
	}
	return name, true
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
			"audit route any ClusterProvider carries and is never credited to a fallback. Stamp the "+
			"annotation, or point this producer at /audit-webhook/<audit-route>", fields...)
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
// /audit-webhook/<audit-route>. The segment is an audit route, not necessarily a ClusterProvider
// name: a provider joins a route through spec.attribution.auditRoute, which merely defaults to its
// own name. It is a purely SYNTACTIC check; whether any provider carries the named route, and
// whether the bare endpoint is enabled at all, is enforced in ServeHTTP. It rejects a trailing
// slash and any extra path segment.
func validateAuditWebhookPath(path string) error {
	if path == "/audit-webhook" {
		return nil
	}
	if !strings.HasPrefix(path, "/audit-webhook/") {
		return errors.New("invalid path; expected /audit-webhook or /audit-webhook/<audit-route>")
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
