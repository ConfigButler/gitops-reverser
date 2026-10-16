// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/repourl"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// ReceiverPathPrefix is the path prefix the push receiver serves. A route is
// ReceiverPathPrefix + "<GitProvider namespace>/<GitProvider name>", as v1alpha3.ReceiverPath builds it.
const ReceiverPathPrefix = "/git-push/"

// DefaultReceiverMaxRequestBodyBytes caps a delivery's body. It is GitHub's own cap on a webhook
// payload (25 MiB), the largest of the hosts the receiver speaks to, so no genuine push event is
// refused for its size. A push event is a few kilobytes in practice; the cap exists so an
// unauthenticated client cannot make the receiver buffer an unbounded body before the signature
// over it can be checked.
const DefaultReceiverMaxRequestBodyBytes = int64(25 * 1024 * 1024)

// Headers each receiver type is signed or classified by. The names are the hosts' own; the
// generic-hmac header is the one Flux's generic-hmac Receiver checks, so a relay written for Flux
// can post here unchanged.
const (
	headerGitHubSignature = "X-Hub-Signature-256"
	headerGitHubEvent     = "X-Github-Event" // canonical form of GitHub's X-GitHub-Event
	headerGitLabToken     = "X-Gitlab-Token" //nolint:gosec // a header name, not a credential
	headerGitLabEvent     = "X-Gitlab-Event"
	headerGiteaSignature  = "X-Gitea-Signature"
	headerGiteaEvent      = "X-Gitea-Event"
	headerGenericHMAC     = "X-Signature"

	sha256SignaturePrefix = "sha256="
	branchRefPrefix       = "refs/heads/"
)

// Delivery outcomes, the outcome label of gitopsreverser_receiver_deliveries_total. See
// telemetry.ReceiverDeliveriesTotal for what each one means.
const (
	deliveryNotified  = "notified"
	deliveryUnmatched = "unmatched"
	deliveryIgnored   = "ignored"
	deliveryRejected  = "rejected"
	deliveryInvalid   = "invalid"
)

// PushNotifier hands a verified delivery's branch to the branch worker that owns it. It reports
// whether such a worker exists. git.WorkerManager implements it.
//
// after is the commit the delivery named. It is passed for logging only: a delivery is a hint,
// and the worker reads where the branch is from the remote itself rather than trusting it.
type PushNotifier interface {
	NotifyPush(providerNamespace, providerName, branch, after string) bool
}

// ReceiverHandlerConfig configures the push receiver.
type ReceiverHandlerConfig struct {
	// Reader looks up the GitProvider a route names and the Secret holding its token. In the
	// manager this is the manager client: GitProviders come from its cache, and Secrets are
	// excluded from that cache (Cache.DisableFor), so each delivery reads the token Secret
	// directly by name. A rotated token therefore takes effect on the next delivery, and the
	// receiver adds no Secret informer. See docs/rbac.md.
	Reader client.Reader
	// Notifier receives each verified push to a branch. Required.
	Notifier PushNotifier
	// MaxRequestBodyBytes caps a delivery's body. Zero means DefaultReceiverMaxRequestBodyBytes.
	MaxRequestBodyBytes int64
}

// ReceiverHandler serves POST /git-push/<namespace>/<name>: the endpoint a Git host's push webhook
// points at, so GitOps Reverser learns quickly that somebody pushed to a branch.
//
// A delivery is a hint, never a fact. The handler authenticates it, extracts the branch, and asks
// the branch worker to look; the worker then reads the remote's advertisement and decides for
// itself whether the branch gained a foreign commit. Nothing the body says beyond the branch name
// is acted on, so a late, duplicated or replayed delivery costs one ref advertisement and can never
// make the worker believe something the remote does not say.
//
// Authentication comes first. The signature (or GitLab's token) is checked over the raw bytes
// before the body is parsed for anything, in constant time, so an unsigned request cannot make a
// target do work or probe which branches are tracked.
type ReceiverHandler struct {
	config ReceiverHandlerConfig
}

// NewReceiverHandler builds the push receiver handler.
func NewReceiverHandler(config ReceiverHandlerConfig) (*ReceiverHandler, error) {
	if config.Reader == nil {
		return nil, errors.New("receiver handler requires a Reader")
	}
	if config.Notifier == nil {
		return nil, errors.New("receiver handler requires a Notifier")
	}
	if config.MaxRequestBodyBytes <= 0 {
		config.MaxRequestBodyBytes = DefaultReceiverMaxRequestBodyBytes
	}
	return &ReceiverHandler{config: config}, nil
}

// receiverResponse is the body of every 202. matched is 1 when a branch worker was told to look
// and 0 otherwise; it is a number rather than a boolean so a relay fanning one event out to many
// receivers can sum the answers.
type receiverResponse struct {
	Matched int `json:"matched"`
}

// pushDelivery is what a verified delivery is acted on for.
type pushDelivery struct {
	branch string
	after  string
}

// errDeliveryIgnored marks a verified delivery that is deliberately not acted on (a tag, another
// repository). It answers 202, not 400: none of those is the sender's mistake.
var errDeliveryIgnored = errors.New("delivery ignored")

// ServeHTTP implements http.Handler for push deliveries.
//
// The status codes are a contract with Git hosts, which disable a hook that keeps failing: every
// verified delivery answers 202, including one that names a branch nothing writes, because that is
// not the host's fault. Only an unauthenticated request (401), an unparseable verified one (400),
// a route that does not serve (404) and a receiver that cannot verify anything yet (503) fail.
func (h *ReceiverHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := logf.Log.WithName("push-receiver")

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	key, ok := receiverRoute(r.URL.Path)
	if !ok {
		notFound(w)
		return
	}
	log = log.WithValues("gitProvider", key.String())

	provider, ok := h.lookupProvider(ctx, log, w, key)
	if !ok {
		return
	}
	receiverType := provider.Spec.Receiver.Type
	log = log.WithValues("type", receiverType)

	token, ok := h.lookupToken(ctx, log, w, provider)
	if !ok {
		return
	}

	body, ok := h.authenticate(ctx, log, w, r, receiverType, token)
	if !ok {
		return
	}
	h.act(ctx, log, w, r.Header, key, provider, body)
}

// authenticate reads the body under the size cap and verifies it, answering the request itself when
// it fails. Nothing in the body is parsed here: a request is authenticated before it is read for
// anything.
func (h *ReceiverHandler) authenticate(
	ctx context.Context,
	log logr.Logger,
	w http.ResponseWriter,
	r *http.Request,
	receiverType v1alpha3.ReceiverType,
	token []byte,
) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.config.MaxRequestBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			log.Info("Refused a push delivery over the size cap", "maxBytes", h.config.MaxRequestBodyBytes)
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			h.record(ctx, receiverType, deliveryRejected)
			return nil, false
		}
		log.V(1).Info("Could not read a push delivery", "error", err.Error())
		http.Error(w, "could not read request body", http.StatusBadRequest)
		return nil, false
	}

	if !verifyDelivery(receiverType, r.Header, body, token) {
		// Info, not V(1): a run of these is a misconfigured hook (a token that does not match) or
		// somebody probing the endpoint, and either is worth seeing at the default level. Neither
		// the token nor the body is logged.
		log.Info("Rejected a push delivery: signature absent or wrong")
		http.Error(w, "signature absent or wrong", http.StatusUnauthorized)
		h.record(ctx, receiverType, deliveryRejected)
		return nil, false
	}
	return body, true
}

// act handles a verified delivery: it decides whether the delivery is a push to a branch and, if
// so, hands the branch to its worker. Every path answers 202 except a body that cannot be read.
func (h *ReceiverHandler) act(
	ctx context.Context,
	log logr.Logger,
	w http.ResponseWriter,
	header http.Header,
	key types.NamespacedName,
	provider *v1alpha3.GitProvider,
	body []byte,
) {
	receiverType := provider.Spec.Receiver.Type
	if event, isPush := pushEvent(receiverType, header); !isPush {
		// A host's ping (GitHub sends one when the hook is created) and any other event the hook
		// was subscribed to are answered 202, so the host shows the hook as healthy.
		log.V(1).Info("Ignored a verified delivery that is not a push", "event", event)
		h.accept(w, false)
		h.record(ctx, receiverType, deliveryIgnored)
		return
	}

	delivery, err := parseDelivery(receiverType, jsonBody(header, body), provider.Spec.URL)
	switch {
	case errors.Is(err, errDeliveryIgnored):
		log.V(1).Info("Ignored a verified push delivery", "reason", err.Error())
		h.accept(w, false)
		h.record(ctx, receiverType, deliveryIgnored)
		return
	case err != nil:
		log.Info("Refused a verified push delivery that cannot be read", "error", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		h.record(ctx, receiverType, deliveryInvalid)
		return
	}

	matched := h.config.Notifier.NotifyPush(key.Namespace, key.Name, delivery.branch, delivery.after)
	outcome := deliveryUnmatched
	if matched {
		outcome = deliveryNotified
	}
	log.V(1).Info("Push delivery handled", "branch", delivery.branch, "after", delivery.after,
		"outcome", outcome)
	h.accept(w, matched)
	h.record(ctx, receiverType, outcome)
}

// receiverRoute parses /git-push/<namespace>/<name>. Anything else under the prefix, including a
// segment that cannot be a namespace or an object name, is no route: answering 404 for it without
// an API read keeps a scanner from turning into apiserver traffic.
func receiverRoute(path string) (types.NamespacedName, bool) {
	rest, ok := strings.CutPrefix(path, ReceiverPathPrefix)
	if !ok {
		return types.NamespacedName{}, false
	}
	namespace, name, ok := strings.Cut(rest, "/")
	if !ok || strings.Contains(name, "/") {
		return types.NamespacedName{}, false
	}
	if len(validation.IsDNS1123Label(namespace)) > 0 || len(validation.IsDNS1123Subdomain(name)) > 0 {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{Namespace: namespace, Name: name}, true
}

// lookupProvider resolves the GitProvider a route names. A missing provider and one without
// spec.receiver answer the same 404, so the endpoint does not reveal which GitProviders exist.
func (h *ReceiverHandler) lookupProvider(
	ctx context.Context,
	log logr.Logger,
	w http.ResponseWriter,
	key types.NamespacedName,
) (*v1alpha3.GitProvider, bool) {
	var provider v1alpha3.GitProvider
	if err := h.config.Reader.Get(ctx, key, &provider); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("Push delivery for a GitProvider that does not exist")
			notFound(w)
			return nil, false
		}
		log.Error(err, "Could not read the GitProvider a push delivery names")
		http.Error(w, "receiver not ready", http.StatusServiceUnavailable)
		return nil, false
	}
	if provider.Spec.Receiver == nil {
		log.V(1).Info("Push delivery for a GitProvider without spec.receiver")
		notFound(w)
		return nil, false
	}
	return &provider, true
}

// lookupToken reads the receiver token from the Secret spec.receiver.secretRef names. A missing
// Secret or key is the receiver not being ready, never the sender's fault, so it answers 503 and
// not 401: a 401 would tell whoever set up the hook that their secret is wrong when it is ours
// that is absent, and a host retries a 5xx where it may give up on a 4xx.
func (h *ReceiverHandler) lookupToken(
	ctx context.Context,
	log logr.Logger,
	w http.ResponseWriter,
	provider *v1alpha3.GitProvider,
) ([]byte, bool) {
	secretName := provider.Spec.Receiver.SecretRef.Name
	var secret corev1.Secret
	err := h.config.Reader.Get(ctx, types.NamespacedName{Namespace: provider.Namespace, Name: secretName}, &secret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Push receiver token Secret not found", "secret", secretName)
		} else {
			log.Error(err, "Could not read the push receiver token Secret", "secret", secretName)
		}
		http.Error(w, "receiver not ready", http.StatusServiceUnavailable)
		return nil, false
	}
	token := secret.Data[v1alpha3.ReceiverTokenKey]
	if len(token) == 0 {
		log.Info("Push receiver token Secret has no token", "secret", secretName, "key", v1alpha3.ReceiverTokenKey)
		http.Error(w, "receiver not ready", http.StatusServiceUnavailable)
		return nil, false
	}
	return token, true
}

// verifyDelivery authenticates a delivery over its raw bytes, the way its host signs it. Every
// comparison is constant time. An unknown type verifies nothing: the CRD's enum keeps one from
// existing, and failing closed keeps a future type from being served unauthenticated.
func verifyDelivery(receiverType v1alpha3.ReceiverType, header http.Header, body, token []byte) bool {
	switch receiverType {
	case v1alpha3.ReceiverTypeGitHub:
		return verifyPrefixedHMAC(header.Get(headerGitHubSignature), body, token)
	case v1alpha3.ReceiverTypeGitea:
		return verifyHexHMAC(header.Get(headerGiteaSignature), body, token)
	case v1alpha3.ReceiverTypeGenericHMAC:
		return verifyPrefixedHMAC(header.Get(headerGenericHMAC), body, token)
	case v1alpha3.ReceiverTypeGitLab:
		return verifyToken(header.Get(headerGitLabToken), token)
	default:
		return false
	}
}

// verifyPrefixedHMAC checks a "sha256=<hex>" signature header.
func verifyPrefixedHMAC(signature string, body, token []byte) bool {
	hexDigest, ok := strings.CutPrefix(signature, sha256SignaturePrefix)
	if !ok {
		return false
	}
	return verifyHexHMAC(hexDigest, body, token)
}

// verifyHexHMAC checks a bare hex HMAC-SHA256 of body keyed by token.
func verifyHexHMAC(hexDigest string, body, token []byte) bool {
	if hexDigest == "" {
		return false
	}
	got, err := hex.DecodeString(hexDigest)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, token)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// verifyToken compares a plain token header with the configured token. Both sides are hashed
// first so the comparison takes the same time whatever the presented token's length:
// subtle.ConstantTimeCompare alone returns early on a length mismatch, which leaks the length.
func verifyToken(presented string, token []byte) bool {
	if presented == "" {
		return false
	}
	got := sha256.Sum256([]byte(presented))
	want := sha256.Sum256(token)
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// pushEvent reports the delivery's event name and whether it is a push. generic-hmac carries no
// event header: a relay posts only pushes, so every verified generic-hmac delivery is one.
func pushEvent(receiverType v1alpha3.ReceiverType, header http.Header) (string, bool) {
	switch receiverType {
	case v1alpha3.ReceiverTypeGitHub:
		event := header.Get(headerGitHubEvent)
		return event, event == "push"
	case v1alpha3.ReceiverTypeGitLab:
		event := header.Get(headerGitLabEvent)
		return event, event == "Push Hook"
	case v1alpha3.ReceiverTypeGitea:
		event := header.Get(headerGiteaEvent)
		return event, event == "push"
	case v1alpha3.ReceiverTypeGenericHMAC:
		return "push", true
	default:
		return "", false
	}
}

// hostPushPayload is the part of a GitHub, GitLab or Gitea push event the receiver reads. The
// three hosts agree on these two fields, which is why one shape serves all of them.
type hostPushPayload struct {
	Ref   string `json:"ref"`
	After string `json:"after"`
}

// genericPushPayload is a generic-hmac relay's body.
type genericPushPayload struct {
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	After      string `json:"after"`
}

// parseDelivery extracts the branch a verified push names. It returns errDeliveryIgnored (wrapped,
// with the reason) for a push that is deliberately not acted on, and any other error for a body
// that cannot be read.
//
// after is optional in every shape. It never decides whether the branch moved (it only spares an open
// window a notice for the tip already observed), so refusing a delivery for lacking it would turn a
// field nothing depends on into a reason for a host to disable the hook.
//
// The repository check is generic-hmac's alone. A host's own hook is configured on one repository
// (or an organization), and a delivery from a sibling repository on an organization hook costs one
// ref advertisement for a branch of the same name; comparing the host's repository URLs with
// spec.url instead would silently drop every delivery for a provider that reaches the host by
// another name (an SSH host alias, ssh.github.com:443), which is the worse failure.
// jsonBody is the JSON document a delivery carries. GitHub's default content type for a webhook is
// application/x-www-form-urlencoded, which wraps the same JSON in a payload field; the signature is
// over the raw body, so this unwrapping happens only after it was verified. Flux accepts that form
// too, so a hook moved over from a Flux Receiver keeps working.
func jsonBody(header http.Header, body []byte) []byte {
	mediaType, _, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		return body
	}
	form, err := url.ParseQuery(string(body))
	if err != nil || form.Get("payload") == "" {
		return body
	}
	return []byte(form.Get("payload"))
}

func parseDelivery(receiverType v1alpha3.ReceiverType, body []byte, providerURL string) (pushDelivery, error) {
	if receiverType == v1alpha3.ReceiverTypeGenericHMAC {
		return parseGenericDelivery(body, providerURL)
	}
	var payload hostPushPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return pushDelivery{}, fmt.Errorf("push event is not valid JSON: %w", err)
	}
	if payload.Ref == "" {
		return pushDelivery{}, errors.New("push event has no ref")
	}
	branch, err := branchFromRef(payload.Ref)
	if err != nil {
		return pushDelivery{}, err
	}
	return pushDelivery{branch: branch, after: payload.After}, nil
}

// parseGenericDelivery reads a generic-hmac body. Its branch may be a bare name or a full ref; a
// repository other than spec.url is ignored, not refused, because a relay fanning one host's
// events out to every receiver is doing exactly what it is for.
func parseGenericDelivery(body []byte, providerURL string) (pushDelivery, error) {
	var payload genericPushPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return pushDelivery{}, fmt.Errorf("body is not valid JSON: %w", err)
	}
	if payload.Branch == "" {
		return pushDelivery{}, errors.New("body has no branch")
	}
	branch := payload.Branch
	if strings.HasPrefix(branch, "refs/") {
		var err error
		if branch, err = branchFromRef(branch); err != nil {
			return pushDelivery{}, err
		}
	}
	if payload.Repository != "" && !repourl.Same(payload.Repository, providerURL) {
		return pushDelivery{}, fmt.Errorf("%w: names another repository", errDeliveryIgnored)
	}
	return pushDelivery{branch: branch, after: payload.After}, nil
}

// branchFromRef returns the branch a full ref names. A ref outside refs/heads/ (a tag, a pull
// request ref) is ignored: no branch worker writes it.
func branchFromRef(ref string) (string, error) {
	branch, ok := strings.CutPrefix(ref, branchRefPrefix)
	if !ok {
		return "", fmt.Errorf("%w: %s is not a branch", errDeliveryIgnored, ref)
	}
	if branch == "" {
		return "", fmt.Errorf("ref %q names no branch", ref)
	}
	return branch, nil
}

// notFound answers a route that does not serve. One body for every reason, so the answer does not
// say whether the GitProvider is missing or merely has no receiver.
func notFound(w http.ResponseWriter) {
	http.Error(w, "no push receiver at this path", http.StatusNotFound)
}

// accept answers a verified delivery.
func (h *ReceiverHandler) accept(w http.ResponseWriter, matched bool) {
	resp := receiverResponse{}
	if matched {
		resp.Matched = 1
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(resp)
}

// record counts one delivery for a provider with a receiver.
func (h *ReceiverHandler) record(ctx context.Context, receiverType v1alpha3.ReceiverType, outcome string) {
	if telemetry.ReceiverDeliveriesTotal == nil {
		return
	}
	telemetry.ReceiverDeliveriesTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("type", string(receiverType)),
		attribute.String("outcome", outcome),
	))
}
