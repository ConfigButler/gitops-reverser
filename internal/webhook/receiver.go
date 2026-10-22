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
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/repourl"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// ReceiverPathPrefix is the path prefix the push receiver serves. A route is the prefix and a hex
// HMAC-SHA256 digest, as v1alpha3.ReceiverPath builds it.
const ReceiverPathPrefix = v1alpha3.ReceiverPathPrefix

// receiverDigestHexLength is the length of a route's digest: a hex SHA-256.
const receiverDigestHexLength = 2 * sha256.Size

// GitReceiverWebhookPathIndex is the cache index the receiver finds a route's GitReceiver by. It is
// registered on the manager's cache (IndexGitReceiverWebhookPath), so a request for a path nothing
// publishes costs no API read.
const GitReceiverWebhookPathIndex = "status.webhookPath"

// IndexGitReceiverWebhookPath indexes a GitReceiver by its published status.webhookPath.
func IndexGitReceiverWebhookPath(obj client.Object) []string {
	receiver, ok := obj.(*v1alpha3.GitReceiver)
	if !ok || receiver.Status.WebhookPath == "" {
		return nil
	}
	return []string{receiver.Status.WebhookPath}
}

// DefaultReceiverMaxRequestBodyBytes caps a delivery's body before its signature is checked. A
// push event is a few kilobytes in practice: GitHub and GitLab list at most 20 commits. The cap
// keeps an unauthenticated client from making the receiver buffer much; a genuine push over it is
// refused (413), and the branch worker still finds the push at the next refresh.
const DefaultReceiverMaxRequestBodyBytes = int64(2 * 1024 * 1024)

// DefaultReceiverMaxInFlight is how many deliveries the receiver handles at once. One is answered
// in milliseconds, so a burst of genuine pushes stays far below it; above it a request is answered
// 429 before anything is read, which bounds what the listener can cost the pod that also mirrors.
const DefaultReceiverMaxInFlight = 8

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
	deliveryNotified          = "notified"
	deliveryUnknownRepository = "unknown_repository"
	deliveryUnknownBranch     = "unknown_branch"
	deliveryIgnored           = "ignored"
	deliveryRejected          = "rejected"
	deliveryInvalid           = "invalid"
)

// PushNotifier hands a verified delivery's branch to the branch worker that writes it for a
// GitProvider. It reports whether such a worker exists. git.WorkerManager implements it.
//
// after is the commit the delivery named. It is passed for logging only: a delivery is a hint,
// and the worker reads where the branch is from the remote itself rather than trusting it.
type PushNotifier interface {
	NotifyPush(providerNamespace, providerName, branch, after string) bool
}

// ReceiverHandlerConfig configures the push receiver.
type ReceiverHandlerConfig struct {
	// Reader looks up the GitReceiver a route names, the Secret holding its token, and the
	// GitProviders a delivery may be for. In the manager this is the manager client: the kinds
	// come from its cache, and Secrets are excluded from that cache (Cache.DisableFor), so each
	// delivery reads the token Secret directly by name. A rotated token therefore takes effect on
	// the next delivery, and the receiver adds no Secret informer. See docs/rbac.md.
	Reader client.Reader
	// Notifier receives each verified push to a branch. Required.
	Notifier PushNotifier
	// MaxRequestBodyBytes caps a delivery's body. Zero means DefaultReceiverMaxRequestBodyBytes.
	MaxRequestBodyBytes int64
	// MaxInFlight bounds the deliveries handled at once. Zero means DefaultReceiverMaxInFlight.
	MaxInFlight int
}

// ReceiverHandler serves POST /git-push/<digest>: the endpoint a Git host's push webhook
// points at, so GitOps Reverser learns quickly that somebody pushed to a branch.
//
// One route is one GitReceiver, and one GitReceiver serves every GitProvider in the cluster, so a
// GitHub App or an organization webhook, which has one endpoint for many repositories, needs one
// route. A delivery is routed by what it names: the branch worker of each provider whose repository
// the payload names, for the branch it names, is woken once.
//
// A delivery is a hint, never a fact. The handler authenticates it, extracts the repository and
// the branch, and asks the branch worker to look; the worker then reads the remote's advertisement
// with its own provider's credential and decides for itself whether the branch gained a foreign
// commit. The repository the body names is only compared with the providers' spec.url, never
// fetched, so a delivery cannot choose a URL, a credential or a commit, and a late, duplicated or
// replayed one costs at most one ref advertisement per woken worker (a worker holds one pending
// look, however many deliveries arrive) and can never make a worker believe something the remote
// does not say.
//
// A verified delivery is answered 202 with no body, whatever it woke. Any namespace can hold a
// GitReceiver, and a reply saying what was woken would tell its token holder whether some other
// namespace tracks a repository and branch they name. What a delivery did is on the
// gitopsreverser_receiver_deliveries_total outcome and on each woken GitBranch's
// status.remote.lastPushNoticeAt.
//
// Authentication comes first. The signature (or GitLab's token) is checked over the raw bytes
// before the body is parsed for anything, in constant time, so an unsigned request cannot make a
// target do work or probe which repositories or branches are tracked.
//
// The route does not name the GitReceiver. Its digest is keyed by the receiver token, so only
// whoever holds the token can find it, and every way a request can fail to reach a verified
// receiver (no such path, a path from before the token rotated, no token, a wrong signature)
// gets the same 404: the endpoint says nothing about which namespaces or receivers exist.
type ReceiverHandler struct {
	config   ReceiverHandlerConfig
	inFlight chan struct{}
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
	if config.MaxInFlight <= 0 {
		config.MaxInFlight = DefaultReceiverMaxInFlight
	}
	return &ReceiverHandler{config: config, inFlight: make(chan struct{}, config.MaxInFlight)}, nil
}

// pushDelivery is what a verified delivery is acted on for.
type pushDelivery struct {
	// repositories is every repository the delivery names, as repourl normalizes them. A host names
	// one repository several ways (an HTTPS and an SSH URL, which can differ in host), and a provider
	// spelling it any of them is the repository the delivery is for.
	repositories map[string]struct{}
	branch       string
	after        string
}

// names reports whether the delivery names the repository at url.
func (d pushDelivery) names(url string) bool {
	normalized := repourl.Normalize(url)
	if normalized == "" {
		return false
	}
	_, ok := d.repositories[normalized]
	return ok
}

// errDeliveryIgnored marks a verified delivery that is deliberately not acted on (a tag). It
// answers 202, not 400: that is not the sender's mistake.
var errDeliveryIgnored = errors.New("delivery ignored")

// ServeHTTP implements http.Handler for push deliveries.
//
// The status codes are a contract with Git hosts, which disable a hook that keeps failing: every
// verified delivery answers 202, including one that names a branch nothing writes, because that is
// not the host's fault. Only an unparseable verified delivery (400), a request that reaches no
// verified receiver (404, whatever the reason), a body over the cap (413), too many requests at
// once (429) and a receiver that cannot read its own configuration (503) fail.
func (h *ReceiverHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := logf.Log.WithName("push-receiver")

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	select {
	case h.inFlight <- struct{}{}:
		defer func() { <-h.inFlight }()
	default:
		log.V(1).Info("Refused a push delivery: too many in flight", "maxInFlight", h.config.MaxInFlight)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many requests in flight", http.StatusTooManyRequests)
		return
	}

	path, ok := receiverRoute(r.URL.Path)
	if !ok {
		notFound(w)
		return
	}

	receiver, ok := h.lookupReceiver(ctx, log, w, path)
	if !ok {
		return
	}
	key := receiver.Key()
	log = log.WithValues("gitReceiver", key.String(), "type", receiver.Spec.Type)

	token, ok := h.lookupToken(ctx, log, w, receiver)
	if !ok {
		return
	}
	// The index holds what status last published. A token rotated since then moves the path, and
	// the old one must stop serving now, not at the next reconcile.
	if subtle.ConstantTimeCompare([]byte(v1alpha3.ReceiverPath(token, key.Namespace, key.Name)), []byte(path)) != 1 {
		log.Info("Push delivery on a path the current receiver token no longer derives; the token was " +
			"rotated, so register the GitReceiver's new status.webhookPath")
		notFound(w)
		return
	}

	body, ok := h.authenticate(ctx, log, w, r, receiver, token)
	if !ok {
		return
	}
	h.act(ctx, log, w, r.Header, receiver, body)
}

// authenticate reads the body under the size cap and verifies it, answering the request itself when
// it fails. Nothing in the body is parsed here: a request is authenticated before it is read for
// anything.
func (h *ReceiverHandler) authenticate(
	ctx context.Context,
	log logr.Logger,
	w http.ResponseWriter,
	r *http.Request,
	receiver *v1alpha3.GitReceiver,
	token []byte,
) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.config.MaxRequestBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			log.Info("Refused a push delivery over the size cap", "maxBytes", h.config.MaxRequestBodyBytes)
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			h.record(ctx, receiver, deliveryRejected)
			return nil, false
		}
		log.V(1).Info("Could not read a push delivery", "error", err.Error())
		http.Error(w, "could not read request body", http.StatusBadRequest)
		return nil, false
	}

	if !verifyDelivery(receiver.Spec.Type, r.Header, body, token) {
		// Info, not V(1): a run of these is a misconfigured hook (a token that does not match) or
		// somebody probing the endpoint, and either is worth seeing at the default level. Neither
		// the token nor the body is logged. The answer is the unknown route's, so a caller cannot
		// tell a wrong signature from a path that serves nothing.
		log.Info("Rejected a push delivery: signature absent or wrong")
		notFound(w)
		h.record(ctx, receiver, deliveryRejected)
		return nil, false
	}
	return body, true
}

// act handles a verified delivery: it decides whether the delivery is a push to a branch and, if
// so, wakes the branch workers it names. Every path answers 202 except a body that cannot be read
// and a configuration that cannot be read.
func (h *ReceiverHandler) act(
	ctx context.Context,
	log logr.Logger,
	w http.ResponseWriter,
	header http.Header,
	receiver *v1alpha3.GitReceiver,
	body []byte,
) {
	receiverType := receiver.Spec.Type
	if event, isPush := pushEvent(receiverType, header); !isPush {
		// A host's ping (GitHub sends one when the hook is created) and any other event the hook
		// was subscribed to are answered 202, so the host shows the hook as healthy.
		log.V(1).Info("Ignored a verified delivery that is not a push", "event", event)
		accept(w)
		h.record(ctx, receiver, deliveryIgnored)
		return
	}

	delivery, err := parseDelivery(receiverType, jsonBody(header, body))
	switch {
	case errors.Is(err, errDeliveryIgnored):
		log.V(1).Info("Ignored a verified push delivery", "reason", err.Error())
		accept(w)
		h.record(ctx, receiver, deliveryIgnored)
		return
	case err != nil:
		log.Info("Refused a verified push delivery that cannot be read", "error", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		h.record(ctx, receiver, deliveryInvalid)
		return
	}

	matched, outcome, err := h.wake(ctx, delivery)
	if err != nil {
		log.Error(err, "Could not look up the GitProviders a push delivery is for")
		http.Error(w, "receiver not ready", http.StatusServiceUnavailable)
		return
	}
	log.V(1).Info("Push delivery handled", "branch", delivery.branch, "after", delivery.after,
		"outcome", outcome, "matched", matched)
	accept(w)
	h.record(ctx, receiver, outcome)
}

// wake tells the branch worker of each GitProvider the delivery is for to look, and returns how many
// were told and the delivery's outcome. A delivery is for every provider, in any namespace, whose
// repository it names. Each provider is asked once, and only the worker writing the branch for it
// answers, so a branch is woken once however many spellings of its repository the delivery carries.
//
// Providers are few, so they are listed from the cache and compared one by one rather than kept in
// an index of normalized URLs.
func (h *ReceiverHandler) wake(ctx context.Context, delivery pushDelivery) (int, string, error) {
	var providers v1alpha3.GitProviderList
	if err := h.config.Reader.List(ctx, &providers); err != nil {
		return 0, "", err
	}
	named, matched := 0, 0
	for i := range providers.Items {
		provider := &providers.Items[i]
		if !delivery.names(provider.Spec.URL) {
			continue
		}
		named++
		if h.config.Notifier.NotifyPush(provider.Namespace, provider.Name, delivery.branch, delivery.after) {
			matched++
		}
	}
	switch {
	case named == 0:
		return 0, deliveryUnknownRepository, nil
	case matched == 0:
		return 0, deliveryUnknownBranch, nil
	default:
		return matched, deliveryNotified, nil
	}
}

// receiverRoute returns the path when it is /git-push/<lowercase hex SHA-256>. Anything else is no
// route, answered 404 without even a cache read.
func receiverRoute(path string) (string, bool) {
	digest, ok := strings.CutPrefix(path, ReceiverPathPrefix)
	if !ok || len(digest) != receiverDigestHexLength {
		return "", false
	}
	for _, c := range digest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return path, true
}

// lookupReceiver finds the GitReceiver that published path, through the cache index. A path nobody
// published answers 404.
func (h *ReceiverHandler) lookupReceiver(
	ctx context.Context,
	log logr.Logger,
	w http.ResponseWriter,
	path string,
) (*v1alpha3.GitReceiver, bool) {
	var receivers v1alpha3.GitReceiverList
	if err := h.config.Reader.List(
		ctx,
		&receivers,
		client.MatchingFields{GitReceiverWebhookPathIndex: path},
	); err != nil {
		log.Error(err, "Could not look up the GitReceiver for a push delivery")
		http.Error(w, "receiver not ready", http.StatusServiceUnavailable)
		return nil, false
	}
	if len(receivers.Items) != 1 {
		if len(receivers.Items) > 1 {
			// Two digests keyed by tokens agree only if a status was written by hand.
			log.Info("Push delivery path is published by more than one GitReceiver; serving none",
				"count", len(receivers.Items))
		}
		notFound(w)
		return nil, false
	}
	return &receivers.Items[0], true
}

// lookupToken reads the receiver token from the Secret spec.secretRef names, in the GitReceiver's
// namespace. A missing Secret or key answers the unknown route's 404: the path was derived from a
// token that is gone, so nothing can be verified on it. Only a read that failed answers 503, so a
// host retries it.
func (h *ReceiverHandler) lookupToken(
	ctx context.Context,
	log logr.Logger,
	w http.ResponseWriter,
	receiver *v1alpha3.GitReceiver,
) ([]byte, bool) {
	secretName := receiver.Spec.SecretRef.Name
	var secret corev1.Secret
	err := h.config.Reader.Get(ctx, types.NamespacedName{Namespace: receiver.Namespace, Name: secretName}, &secret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Push receiver token Secret not found", "secret", secretName)
			notFound(w)
		} else {
			log.Error(err, "Could not read the push receiver token Secret", "secret", secretName)
			http.Error(w, "receiver not ready", http.StatusServiceUnavailable)
		}
		return nil, false
	}
	token := secret.Data[v1alpha3.ReceiverTokenKey]
	if len(token) == 0 {
		log.Info("Push receiver token Secret has no token", "secret", secretName, "key", v1alpha3.ReceiverTokenKey)
		notFound(w)
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
// three hosts agree on ref and after. They name the repository in different fields, so every field
// one of them fills with a clone or web URL is read: GitHub and Gitea fill repository.clone_url,
// ssh_url and html_url (GitHub also git_url); GitLab fills project.git_http_url, git_ssh_url and
// web_url, and the same in its legacy repository object. The API URLs (repository.url on Gitea, a
// homepage on GitHub) are not read: they name no clone URL a provider could spell.
type hostPushPayload struct {
	Ref        string         `json:"ref"`
	After      string         `json:"after"`
	Repository hostRepository `json:"repository"`
	Project    hostRepository `json:"project"`
}

// hostRepository is the repository a push event names, in any of the hosts' spellings.
type hostRepository struct {
	CloneURL   string `json:"clone_url"`
	SSHURL     string `json:"ssh_url"`
	GitURL     string `json:"git_url"`
	HTMLURL    string `json:"html_url"`
	GitHTTPURL string `json:"git_http_url"`
	GitSSHURL  string `json:"git_ssh_url"`
	WebURL     string `json:"web_url"`
}

func (r hostRepository) urls() []string {
	return []string{r.CloneURL, r.SSHURL, r.GitURL, r.HTMLURL, r.GitHTTPURL, r.GitSSHURL, r.WebURL}
}

// genericPushPayload is a generic-hmac relay's body.
type genericPushPayload struct {
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	After      string `json:"after"`
}

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

// parseDelivery extracts the repository and the branch a verified push names. It returns
// errDeliveryIgnored (wrapped, with the reason) for a push that is deliberately not acted on, and
// any other error for a body that cannot be read. A delivery that names no repository cannot be
// routed, so it is unreadable rather than ignored.
//
// after is optional in every shape. It never decides whether the branch moved (it only spares an open
// window a notice for the tip already observed), so refusing a delivery for lacking it would turn a
// field nothing depends on into a reason for a host to disable the hook.
func parseDelivery(receiverType v1alpha3.ReceiverType, body []byte) (pushDelivery, error) {
	if receiverType == v1alpha3.ReceiverTypeGenericHMAC {
		return parseGenericDelivery(body)
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
	repositories := normalizedRepositories(append(payload.Repository.urls(), payload.Project.urls()...))
	if len(repositories) == 0 {
		return pushDelivery{}, errors.New("push event names no repository")
	}
	return pushDelivery{repositories: repositories, branch: branch, after: payload.After}, nil
}

// parseGenericDelivery reads a generic-hmac body. Its branch may be a bare name or a full ref.
func parseGenericDelivery(body []byte) (pushDelivery, error) {
	var payload genericPushPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return pushDelivery{}, fmt.Errorf("body is not valid JSON: %w", err)
	}
	if payload.Branch == "" {
		return pushDelivery{}, errors.New("body has no branch")
	}
	repositories := normalizedRepositories([]string{payload.Repository})
	if len(repositories) == 0 {
		return pushDelivery{}, errors.New("body has no repository")
	}
	branch := payload.Branch
	if strings.HasPrefix(branch, "refs/") {
		var err error
		if branch, err = branchFromRef(branch); err != nil {
			return pushDelivery{}, err
		}
	}
	return pushDelivery{repositories: repositories, branch: branch, after: payload.After}, nil
}

// normalizedRepositories is the set of repositories urls name, as repourl normalizes them. An empty
// URL names none.
func normalizedRepositories(urls []string) map[string]struct{} {
	repositories := map[string]struct{}{}
	for _, raw := range urls {
		if normalized := repourl.Normalize(raw); normalized != "" {
			repositories[normalized] = struct{}{}
		}
	}
	return repositories
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

// notFound answers every request that reaches no verified receiver. One status and body for every
// reason, so the answer does not say whether the path, the token or the signature was wrong.
func notFound(w http.ResponseWriter) {
	http.Error(w, "no push receiver at this path", http.StatusNotFound)
}

// accept answers a verified delivery: 202, with no body (see ReceiverHandler).
func accept(w http.ResponseWriter) {
	w.WriteHeader(http.StatusAccepted)
}

// record counts one delivery to a GitReceiver.
func (h *ReceiverHandler) record(ctx context.Context, receiver *v1alpha3.GitReceiver, outcome string) {
	if telemetry.ReceiverDeliveriesTotal == nil {
		return
	}
	telemetry.ReceiverDeliveriesTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("receiver_namespace", receiver.Namespace),
		attribute.String("receiver_name", receiver.Name),
		attribute.String("type", string(receiver.Spec.Type)),
		attribute.String("outcome", outcome),
	))
}
