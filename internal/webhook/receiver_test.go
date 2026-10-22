// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

const (
	receiverTestNamespace = "platform"
	receiverTestToken     = "s3cr3t-token"
	receiverTestCommit    = "4f2c1ab9e0d1c2b3a4f5e6d7c8b9a0f1e2d3c4b5"

	platformRepo = "https://github.com/acme/platform.git"
	tenantRepo   = "https://github.com/acme/tenant.git"
)

// fakePushNotifier records every NotifyPush call and reports a worker for the "<namespace>/<name>
// @<branch>" keys in workers.
type fakePushNotifier struct {
	workers map[string]bool
	calls   []fakePushCall
}

type fakePushCall struct {
	namespace, name, branch, after string
}

func (f *fakePushNotifier) NotifyPush(providerNamespace, providerName, branch, after string) bool {
	f.calls = append(f.calls, fakePushCall{providerNamespace, providerName, branch, after})
	return f.workers[providerNamespace+"/"+providerName+"@"+branch]
}

// testWorkers is the branch workers the handler tests run against: main of each provider's
// repository is written, through the provider listed here.
func testWorkers() map[string]bool {
	return map[string]bool{
		"platform/gh-config@main":     true,
		"platform/gt-config@main":     true,
		"tenant-a/tenant-config@main": true,
	}
}

// receiverTestReceiver is a GitReceiver in the test namespace whose status publishes the path
// receiverTestToken derives, as the GitReceiver controller would.
func receiverTestReceiver(name string, receiverType v1alpha3.ReceiverType, secret string) *v1alpha3.GitReceiver {
	return &v1alpha3.GitReceiver{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: receiverTestNamespace},
		Spec: v1alpha3.GitReceiverSpec{
			Type:      receiverType,
			SecretRef: meta.LocalObjectReference{Name: secret},
		},
		Status: v1alpha3.GitReceiverStatus{WebhookPath: route(name)},
	}
}

// route is the path receiverTestToken derives for the receiver name in the test namespace.
func route(name string) string {
	return v1alpha3.ReceiverPath([]byte(receiverTestToken), receiverTestNamespace, name)
}

// rotatedReceiver still publishes the path a token it no longer holds derived.
func rotatedReceiver() *v1alpha3.GitReceiver {
	receiver := receiverTestReceiver("rotated", v1alpha3.ReceiverTypeGitHub, "receiver-token")
	receiver.Status.WebhookPath = v1alpha3.ReceiverPath([]byte("the-old-token"), receiverTestNamespace, "rotated")
	return receiver
}

// testProvider is a GitProvider for url.
func testProvider(namespace, name, url string) *v1alpha3.GitProvider {
	return &v1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha3.GitProviderSpec{
			URL:             url,
			AllowedBranches: []string{"*"},
		},
	}
}

func receiverTestSecret(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: receiverTestNamespace},
		Data:       data,
	}
}

// receiverTestObjects is one receiver per type sharing one token Secret, the receivers that cannot
// verify yet, and the providers every one of them routes to by URL.
//
// The platform repository has two providers spelling it two ways (only gh-config writes main), the
// tenant repository one provider in another namespace, and the Gitea repository one provider
// spelling it by Gitea's SSH host.
func receiverTestObjects() []client.Object {
	return []client.Object{
		receiverTestSecret("receiver-token", map[string][]byte{v1alpha3.ReceiverTokenKey: []byte(receiverTestToken)}),
		receiverTestSecret("wrong-key", map[string][]byte{"password": []byte(receiverTestToken)}),
		receiverTestReceiver("gh", v1alpha3.ReceiverTypeGitHub, "receiver-token"),
		receiverTestReceiver("gl", v1alpha3.ReceiverTypeGitLab, "receiver-token"),
		receiverTestReceiver("gt", v1alpha3.ReceiverTypeGitea, "receiver-token"),
		receiverTestReceiver("gen", v1alpha3.ReceiverTypeGenericHMAC, "receiver-token"),
		receiverTestReceiver("no-secret", v1alpha3.ReceiverTypeGitHub, "absent"),
		receiverTestReceiver("no-key", v1alpha3.ReceiverTypeGitHub, "wrong-key"),
		rotatedReceiver(),
		testProvider(receiverTestNamespace, "gh-config", platformRepo),
		testProvider(receiverTestNamespace, "ssh-config", "git@github.com:acme/platform"),
		testProvider("tenant-a", "tenant-config", tenantRepo),
		testProvider(receiverTestNamespace, "gt-config", "ssh://git@gitea-ssh.example/acme/platform.git"),
	}
}

func newReceiverTestHandler(
	t *testing.T,
	notifier *fakePushNotifier,
	maxBody int64,
	funcs *interceptor.Funcs,
) *ReceiverHandler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha3.AddToScheme(scheme))
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(receiverTestObjects()...).
		WithIndex(&v1alpha3.GitReceiver{}, GitReceiverWebhookPathIndex, IndexGitReceiverWebhookPath)
	if funcs != nil {
		builder = builder.WithInterceptorFuncs(*funcs)
	}
	h, err := NewReceiverHandler(ReceiverHandlerConfig{
		Reader:              builder.Build(),
		Notifier:            notifier,
		MaxRequestBodyBytes: maxBody,
	})
	require.NoError(t, err)
	return h
}

func hmacHex(body, token string) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

// hostPushBody is a GitHub or Gitea push event for ref of the repository cloned from cloneURL.
func hostPushBody(ref, cloneURL string) string {
	body, _ := json.Marshal(map[string]any{
		"ref": ref, "before": "0000", "after": receiverTestCommit,
		"repository": map[string]string{"clone_url": cloneURL, "html_url": strings.TrimSuffix(cloneURL, ".git")},
	})
	return string(body)
}

// gitlabPushBody is a GitLab push event for ref, which names its repository in the project object.
func gitlabPushBody(ref, httpURL string) string {
	body, _ := json.Marshal(map[string]any{
		"ref": ref, "after": receiverTestCommit,
		"project": map[string]string{"git_http_url": httpURL, "web_url": strings.TrimSuffix(httpURL, ".git")},
	})
	return string(body)
}

// giteaPushBody is a Gitea push event whose HTTP and SSH URLs name different hosts, as a Gitea with
// a separate SSH_DOMAIN sends them.
func giteaPushBody(ref string) string {
	body, _ := json.Marshal(map[string]any{
		"ref": ref, "after": receiverTestCommit,
		"repository": map[string]string{
			"clone_url": "http://gitea-http.example:3000/acme/platform.git",
			"ssh_url":   "git@gitea-ssh.example:acme/platform.git",
		},
	})
	return string(body)
}

func genericBody(repository, branch string) string {
	body, _ := json.Marshal(map[string]string{"repository": repository, "branch": branch, "after": receiverTestCommit})
	return string(body)
}

// formEncoded is a JSON body the way GitHub sends it with its default content type.
func formEncoded(body string) string { return url.Values{"payload": {body}}.Encode() }

func githubHeaders(event, body, token string) map[string]string {
	return map[string]string{headerGitHubEvent: event, headerGitHubSignature: "sha256=" + hmacHex(body, token)}
}

func giteaHeaders(event, body, token string) map[string]string {
	return map[string]string{headerGiteaEvent: event, headerGiteaSignature: hmacHex(body, token)}
}

func gitlabHeaders(event, token string) map[string]string {
	return map[string]string{headerGitLabEvent: event, headerGitLabToken: token}
}

func genericHeaders(body, token string) map[string]string {
	return map[string]string{headerGenericHMAC: "sha256=" + hmacHex(body, token)}
}

type receiverCase struct {
	method  string // POST when empty
	path    string
	headers map[string]string
	body    string

	wantStatus int
	// wantCalls is every NotifyPush the delivery must make, in any order; nil asserts none was made.
	wantCalls []fakePushCall
	// wantReceiver/wantOutcome is the one delivery the metric must count; empty asserts none.
	wantReceiver string
	wantType     string
	wantOutcome  string
}

func call(namespace, name, branch string) fakePushCall {
	return fakePushCall{namespace, name, branch, receiverTestCommit}
}

func receiverCases() map[string]receiverCase {
	mainPush := hostPushBody("refs/heads/main", platformRepo)
	otherPush := hostPushBody("refs/heads/feature/x", platformRepo)
	tagPush := hostPushBody("refs/tags/v1.0.0", platformRepo)
	tenantPush := hostPushBody("refs/heads/main", tenantRepo)
	unknownPush := hostPushBody("refs/heads/main", "https://github.com/acme/other.git")
	ghPlatform := []fakePushCall{call(receiverTestNamespace, "gh-config", "main"),
		call(receiverTestNamespace, "ssh-config", "main")}
	genMain := genericBody("git@github.com:acme/platform", "main")
	gitlabMain := gitlabPushBody("refs/heads/main", platformRepo)

	return map[string]receiverCase{
		// github, and the routing every type shares
		"a push wakes the worker of the provider that writes the branch, once": {
			path: route("gh"), headers: githubHeaders("push", mainPush, receiverTestToken), body: mainPush,
			wantStatus: http.StatusAccepted, wantCalls: ghPlatform,
			wantReceiver: "gh", wantType: "github", wantOutcome: deliveryNotified,
		},
		"a push wakes a provider in any namespace": {
			path: route("gh"), headers: githubHeaders("push", tenantPush, receiverTestToken), body: tenantPush,
			wantStatus:   http.StatusAccepted,
			wantCalls:    []fakePushCall{call("tenant-a", "tenant-config", "main")},
			wantReceiver: "gh", wantType: "github", wantOutcome: deliveryNotified,
		},
		"a push for a repository no provider has is still a success": {
			path: route("gh"), headers: githubHeaders("push", unknownPush, receiverTestToken), body: unknownPush,
			wantStatus:   http.StatusAccepted,
			wantReceiver: "gh", wantType: "github", wantOutcome: deliveryUnknownRepository,
		},
		"a push to a branch nothing writes is still a success": {
			path: route("gh"), headers: githubHeaders("push", otherPush, receiverTestToken), body: otherPush,
			wantStatus: http.StatusAccepted,
			wantCalls: []fakePushCall{call(receiverTestNamespace, "gh-config", "feature/x"),
				call(receiverTestNamespace, "ssh-config", "feature/x")},
			wantReceiver: "gh", wantType: "github", wantOutcome: deliveryUnknownBranch,
		},
		// GitHub's default content type wraps the JSON in a payload field, and signs the raw body.
		"github form-encoded push reaches the worker": {
			path: route("gh"),
			headers: func() map[string]string {
				h := githubHeaders("push", formEncoded(mainPush), receiverTestToken)
				h["Content-Type"] = "application/x-www-form-urlencoded"
				return h
			}(),
			body:       formEncoded(mainPush),
			wantStatus: http.StatusAccepted, wantCalls: ghPlatform,
			wantReceiver: "gh", wantType: "github", wantOutcome: deliveryNotified,
		},
		"github wrong signature is rejected": {
			path: route("gh"), headers: githubHeaders("push", mainPush, "not-the-token"), body: mainPush,
			wantStatus: http.StatusNotFound, wantReceiver: "gh", wantType: "github", wantOutcome: deliveryRejected,
		},
		"github missing signature is rejected": {
			path: route("gh"), headers: map[string]string{headerGitHubEvent: "push"}, body: mainPush,
			wantStatus: http.StatusNotFound, wantReceiver: "gh", wantType: "github", wantOutcome: deliveryRejected,
		},
		"github signature without the sha256 prefix is rejected": {
			path: route("gh"),
			headers: map[string]string{
				headerGitHubEvent: "push", headerGitHubSignature: hmacHex(mainPush, receiverTestToken),
			},
			body:       mainPush,
			wantStatus: http.StatusNotFound, wantReceiver: "gh", wantType: "github", wantOutcome: deliveryRejected,
		},
		"github signature that is not hex is rejected": {
			path: route("gh"),
			headers: map[string]string{
				headerGitHubEvent: "push", headerGitHubSignature: "sha256=zz",
			},
			body:       mainPush,
			wantStatus: http.StatusNotFound, wantReceiver: "gh", wantType: "github", wantOutcome: deliveryRejected,
		},
		"github ping is accepted and does nothing": {
			path: route("gh"), headers: githubHeaders("ping", `{"zen":"hi"}`, receiverTestToken),
			body:       `{"zen":"hi"}`,
			wantStatus: http.StatusAccepted, wantReceiver: "gh", wantType: "github", wantOutcome: deliveryIgnored,
		},
		"github non-push event is ignored": {
			path: route("gh"), headers: githubHeaders("issues", `{}`, receiverTestToken), body: `{}`,
			wantStatus: http.StatusAccepted, wantReceiver: "gh", wantType: "github", wantOutcome: deliveryIgnored,
		},
		"github tag push is ignored": {
			path: route("gh"), headers: githubHeaders("push", tagPush, receiverTestToken), body: tagPush,
			wantStatus: http.StatusAccepted, wantReceiver: "gh", wantType: "github", wantOutcome: deliveryIgnored,
		},
		"github verified body that is not JSON is invalid": {
			path: route("gh"), headers: githubHeaders("push", "not json", receiverTestToken),
			body:       "not json",
			wantStatus: http.StatusBadRequest, wantReceiver: "gh", wantType: "github", wantOutcome: deliveryInvalid,
		},
		"github verified push without a ref is invalid": {
			path: route("gh"), headers: githubHeaders("push", `{"after":"abc"}`, receiverTestToken),
			body:       `{"after":"abc"}`,
			wantStatus: http.StatusBadRequest, wantReceiver: "gh", wantType: "github", wantOutcome: deliveryInvalid,
		},
		"github ref naming no branch is invalid": {
			path: route("gh"), headers: githubHeaders("push", `{"ref":"refs/heads/"}`, receiverTestToken),
			body:       `{"ref":"refs/heads/"}`,
			wantStatus: http.StatusBadRequest, wantReceiver: "gh", wantType: "github", wantOutcome: deliveryInvalid,
		},
		"github push naming no repository is invalid": {
			path:       route("gh"),
			headers:    githubHeaders("push", `{"ref":"refs/heads/main"}`, receiverTestToken),
			body:       `{"ref":"refs/heads/main"}`,
			wantStatus: http.StatusBadRequest, wantReceiver: "gh", wantType: "github", wantOutcome: deliveryInvalid,
		},

		// gitlab
		"gitlab push names its repository in the project and is notified": {
			path: route("gl"), headers: gitlabHeaders("Push Hook", receiverTestToken), body: gitlabMain,
			wantStatus:   http.StatusAccepted,
			wantCalls:    ghPlatform,
			wantReceiver: "gl", wantType: "gitlab", wantOutcome: deliveryNotified,
		},
		"gitlab wrong token is rejected": {
			path: route("gl"), headers: gitlabHeaders("Push Hook", "nope"), body: gitlabMain,
			wantStatus: http.StatusNotFound, wantReceiver: "gl", wantType: "gitlab", wantOutcome: deliveryRejected,
		},
		"gitlab missing token is rejected": {
			path: route("gl"), headers: map[string]string{headerGitLabEvent: "Push Hook"}, body: gitlabMain,
			wantStatus: http.StatusNotFound, wantReceiver: "gl", wantType: "gitlab", wantOutcome: deliveryRejected,
		},
		"gitlab tag push hook is ignored": {
			path: route("gl"), headers: gitlabHeaders("Tag Push Hook", receiverTestToken), body: tagPush,
			wantStatus: http.StatusAccepted, wantReceiver: "gl", wantType: "gitlab", wantOutcome: deliveryIgnored,
		},

		// gitea
		"gitea push reaches a provider spelling the repository by its SSH host": {
			path:         route("gt"),
			headers:      giteaHeaders("push", giteaPushBody("refs/heads/main"), receiverTestToken),
			body:         giteaPushBody("refs/heads/main"),
			wantStatus:   http.StatusAccepted,
			wantCalls:    []fakePushCall{call(receiverTestNamespace, "gt-config", "main")},
			wantReceiver: "gt", wantType: "gitea", wantOutcome: deliveryNotified,
		},
		"gitea wrong signature is rejected": {
			path: route("gt"), headers: giteaHeaders("push", mainPush, "nope"), body: mainPush,
			wantStatus: http.StatusNotFound, wantReceiver: "gt", wantType: "gitea", wantOutcome: deliveryRejected,
		},
		"gitea github-style prefixed signature is rejected": {
			path: route("gt"),
			headers: map[string]string{
				headerGiteaEvent: "push", headerGiteaSignature: "sha256=" + hmacHex(mainPush, receiverTestToken),
			},
			body:       mainPush,
			wantStatus: http.StatusNotFound, wantReceiver: "gt", wantType: "gitea", wantOutcome: deliveryRejected,
		},
		"gitea non-push event is ignored": {
			path: route("gt"), headers: giteaHeaders("create", `{}`, receiverTestToken), body: `{}`,
			wantStatus: http.StatusAccepted, wantReceiver: "gt", wantType: "gitea", wantOutcome: deliveryIgnored,
		},

		// generic-hmac
		"generic-hmac push for the same repository spelled differently is notified": {
			path: route("gen"), headers: genericHeaders(genMain, receiverTestToken), body: genMain,
			wantStatus:   http.StatusAccepted,
			wantCalls:    ghPlatform,
			wantReceiver: "gen", wantType: "generic-hmac", wantOutcome: deliveryNotified,
		},
		"generic-hmac full ref is read as its branch": {
			path:         route("gen"),
			headers:      genericHeaders(genericBody(platformRepo, "refs/heads/main"), receiverTestToken),
			body:         genericBody(platformRepo, "refs/heads/main"),
			wantStatus:   http.StatusAccepted,
			wantCalls:    ghPlatform,
			wantReceiver: "gen", wantType: "generic-hmac", wantOutcome: deliveryNotified,
		},
		"generic-hmac another repository is counted and ignored": {
			path:       route("gen"),
			headers:    genericHeaders(genericBody("https://github.com/acme/other", "main"), receiverTestToken),
			body:       genericBody("https://github.com/acme/other", "main"),
			wantStatus: http.StatusAccepted, wantReceiver: "gen", wantType: "generic-hmac",
			wantOutcome: deliveryUnknownRepository,
		},
		"generic-hmac tag ref is ignored": {
			path:         route("gen"),
			headers:      genericHeaders(genericBody(platformRepo, "refs/tags/v1"), receiverTestToken),
			body:         genericBody(platformRepo, "refs/tags/v1"),
			wantStatus:   http.StatusAccepted,
			wantReceiver: "gen",
			wantType:     "generic-hmac",
			wantOutcome:  deliveryIgnored,
		},
		"generic-hmac wrong signature is rejected": {
			path: route("gen"), headers: genericHeaders(genMain, "nope"), body: genMain,
			wantStatus: http.StatusNotFound, wantReceiver: "gen", wantType: "generic-hmac",
			wantOutcome: deliveryRejected,
		},
		"generic-hmac without a branch is invalid": {
			path:       route("gen"),
			headers:    genericHeaders(`{"repository":"x"}`, receiverTestToken),
			body:       `{"repository":"x"}`,
			wantStatus: http.StatusBadRequest, wantReceiver: "gen", wantType: "generic-hmac",
			wantOutcome: deliveryInvalid,
		},
		"generic-hmac without a repository is invalid": {
			path:       route("gen"),
			headers:    genericHeaders(genericBody("", "main"), receiverTestToken),
			body:       genericBody("", "main"),
			wantStatus: http.StatusBadRequest, wantReceiver: "gen", wantType: "generic-hmac",
			wantOutcome: deliveryInvalid,
		},

		// every way of reaching no verified receiver is the same 404
		"a path nothing publishes is 404": {
			path:       v1alpha3.ReceiverPath([]byte(receiverTestToken), receiverTestNamespace, "missing"),
			headers:    githubHeaders("push", mainPush, receiverTestToken),
			body:       mainPush,
			wantStatus: http.StatusNotFound,
		},
		"a path from before the token rotated is 404": {
			path:       rotatedReceiver().Status.WebhookPath,
			headers:    githubHeaders("push", mainPush, receiverTestToken),
			body:       mainPush,
			wantStatus: http.StatusNotFound,
		},
		"a missing token Secret is 404": {
			path: route("no-secret"), headers: githubHeaders("push", mainPush, receiverTestToken),
			body: mainPush, wantStatus: http.StatusNotFound,
		},
		"a token Secret without the token key is 404": {
			path: route("no-key"), headers: githubHeaders("push", mainPush, receiverTestToken),
			body: mainPush, wantStatus: http.StatusNotFound,
		},
		"the old namespace/name route is 404": {
			path:       "/git-push/platform/" + "gh",
			headers:    githubHeaders("push", mainPush, receiverTestToken),
			body:       mainPush,
			wantStatus: http.StatusNotFound,
		},
		"an uppercase digest is 404": {
			path: strings.ToUpper(route("gh")), body: mainPush, wantStatus: http.StatusNotFound,
		},
		"a short digest is 404": {
			path: route("gh")[:len(route("gh"))-1], body: mainPush, wantStatus: http.StatusNotFound,
		},
		"a digest with an extra segment is 404": {
			path: route("gh") + "/extra", body: mainPush, wantStatus: http.StatusNotFound,
		},
		"path outside the prefix is 404": {
			path: "/other/" + strings.TrimPrefix(route("gh"), ReceiverPathPrefix), body: mainPush,
			wantStatus: http.StatusNotFound,
		},
		"GET is not allowed": {
			method: http.MethodGet, path: route("gh"), wantStatus: http.StatusMethodNotAllowed,
		},
	}
}

func TestReceiverHandler_ServeHTTP(t *testing.T) {
	for name, tc := range receiverCases() {
		t.Run(name, func(t *testing.T) {
			reader, err := telemetry.InitTestExporter()
			require.NoError(t, err)

			notifier := &fakePushNotifier{workers: testWorkers()}
			h := newReceiverTestHandler(t, notifier, 0, nil)

			method := tc.method
			if method == "" {
				method = http.MethodPost
			}
			req := httptest.NewRequest(method, tc.path, strings.NewReader(tc.body))
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			require.Equal(t, tc.wantStatus, rec.Code, "body: %s", rec.Body.String())
			if tc.wantStatus == http.StatusAccepted {
				assert.Empty(t, rec.Body.String(), "a 202 says nothing about what the delivery woke")
			}

			if tc.wantCalls == nil {
				assert.Empty(t, notifier.calls, "a delivery that is not acted on must not reach a worker")
			} else {
				assert.ElementsMatch(t, tc.wantCalls, notifier.calls)
			}

			total, _ := telemetry.CollectInt64Sum(reader, "gitopsreverser_receiver_deliveries_total", nil)
			if tc.wantOutcome == "" {
				assert.Zero(t, total, "a request that reaches no receiver is not a delivery")
				return
			}
			assert.Equal(t, int64(1), total, "exactly one delivery is counted")
			got, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_receiver_deliveries_total",
				map[string]string{
					"receiver_namespace": receiverTestNamespace, "receiver_name": tc.wantReceiver,
					"type": tc.wantType, "outcome": tc.wantOutcome,
				})
			require.True(t, ok, "no %s %s/%s delivery recorded", tc.wantReceiver, tc.wantType, tc.wantOutcome)
			assert.Equal(t, int64(1), got)
		})
	}
}

// TestReceiverHandler_OversizedBodyIsRefusedBeforeVerification pins the cap: a body over it is
// refused whatever its signature says, because verifying it would mean buffering all of it.
func TestReceiverHandler_OversizedBodyIsRefusedBeforeVerification(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	notifier := &fakePushNotifier{workers: testWorkers()}
	h := newReceiverTestHandler(t, notifier, 64, nil)

	body := hostPushBody("refs/heads/main", platformRepo)
	require.Greater(t, len(body), 64)
	req := httptest.NewRequest(http.MethodPost, route("gh"), strings.NewReader(body))
	for k, v := range githubHeaders("push", body, receiverTestToken) {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Empty(t, notifier.calls)
	got, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_receiver_deliveries_total",
		map[string]string{"type": "github", "outcome": deliveryRejected})
	require.True(t, ok)
	assert.Equal(t, int64(1), got)
}

// TestReceiverHandler_UnreadableConfigurationIs503 separates "no such route" from "cannot tell
// yet": an API error reading the GitReceiver, its token or the providers must not answer 404, which
// a host may treat as a dead hook, nor a 202 that drops the delivery.
func TestReceiverHandler_UnreadableConfigurationIs503(t *testing.T) {
	failing := errors.New("apiserver unavailable")
	failListOf := func(match func(client.ObjectList) bool) *interceptor.Funcs {
		return &interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if match(list) {
					return failing
				}
				return c.List(ctx, list, opts...)
			},
		}
	}
	failGetOf := func(match func(client.Object) bool) *interceptor.Funcs {
		return &interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				if match(obj) {
					return failing
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}
	}
	for name, tc := range map[string]struct {
		funcs *interceptor.Funcs
		body  string
	}{
		"receiver lookup fails": {
			funcs: failListOf(func(l client.ObjectList) bool { _, ok := l.(*v1alpha3.GitReceiverList); return ok }),
			body:  hostPushBody("refs/heads/main", platformRepo),
		},
		"token read fails": {
			funcs: failGetOf(func(o client.Object) bool { _, ok := o.(*corev1.Secret); return ok }),
			body:  hostPushBody("refs/heads/main", platformRepo),
		},
		"provider lookup fails": {
			funcs: failListOf(func(l client.ObjectList) bool { _, ok := l.(*v1alpha3.GitProviderList); return ok }),
			body:  hostPushBody("refs/heads/main", platformRepo),
		},
	} {
		t.Run(name, func(t *testing.T) {
			notifier := &fakePushNotifier{}
			h := newReceiverTestHandler(t, notifier, 0, tc.funcs)

			req := httptest.NewRequest(http.MethodPost, route("gh"), strings.NewReader(tc.body))
			for k, v := range githubHeaders("push", tc.body, receiverTestToken) {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		})
	}
}

// TestReceiverHandler_TooManyInFlightIs429 pins the in-flight cap: a request over it is answered
// before anything is read, and a request after the others finished is served again.
func TestReceiverHandler_TooManyInFlightIs429(t *testing.T) {
	notifier := &fakePushNotifier{workers: testWorkers()}
	h := newReceiverTestHandler(t, notifier, 0, nil)
	for range DefaultReceiverMaxInFlight {
		h.inFlight <- struct{}{}
	}

	body := hostPushBody("refs/heads/main", platformRepo)
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, route("gh"), strings.NewReader(body))
		for k, v := range githubHeaders("push", body, receiverTestToken) {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := post()
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	assert.Empty(t, notifier.calls)

	for range DefaultReceiverMaxInFlight {
		<-h.inFlight
	}
	assert.Equal(t, http.StatusAccepted, post().Code)
	assert.Len(t, notifier.calls, 2, "both providers on the repository are asked")
}

// TestReceiverPath_DoesNotNameTheReceiverAndNeverCollides pins what the path is for: it reveals
// neither namespace nor name, it moves with the token, and two receivers sharing a token whose
// names concatenate alike (Flux's sha256(token+name+namespace) would collide on these) get two paths.
func TestReceiverPath_DoesNotNameTheReceiverAndNeverCollides(t *testing.T) {
	token := []byte(receiverTestToken)
	path := v1alpha3.ReceiverPath(token, "team-a", "platform")
	assert.True(t, strings.HasPrefix(path, ReceiverPathPrefix))
	assert.NotContains(t, path, "team-a")
	assert.NotContains(t, path, "platform")
	_, ok := receiverRoute(path)
	assert.True(t, ok, "the receiver must serve the path the API builds")

	assert.NotEqual(t, path, v1alpha3.ReceiverPath([]byte("another"), "team-a", "platform"))
	assert.NotEqual(t, v1alpha3.ReceiverPath(token, "ab", "c"), v1alpha3.ReceiverPath(token, "a", "bc"))
}

func TestNewReceiverHandler_RequiresReaderAndNotifier(t *testing.T) {
	_, err := NewReceiverHandler(ReceiverHandlerConfig{Notifier: &fakePushNotifier{}})
	require.Error(t, err)

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha3.AddToScheme(scheme))
	_, err = NewReceiverHandler(ReceiverHandlerConfig{Reader: fake.NewClientBuilder().WithScheme(scheme).Build()})
	require.Error(t, err)

	h, err := NewReceiverHandler(ReceiverHandlerConfig{
		Reader:   fake.NewClientBuilder().WithScheme(scheme).Build(),
		Notifier: &fakePushNotifier{},
	})
	require.NoError(t, err)
	assert.Equal(t, DefaultReceiverMaxRequestBodyBytes, h.config.MaxRequestBodyBytes)
}

func TestVerifyToken_ComparesWholeToken(t *testing.T) {
	assert.True(t, verifyToken("abc", []byte("abc")))
	assert.False(t, verifyToken("ab", []byte("abc")))
	assert.False(t, verifyToken("abcd", []byte("abc")))
	assert.False(t, verifyToken("", []byte("abc")))
}

func TestVerifyDelivery_UnknownTypeFailsClosed(t *testing.T) {
	body := []byte(`{}`)
	header := http.Header{}
	header.Set(headerGenericHMAC, "sha256="+hmacHex(string(body), receiverTestToken))
	assert.False(t, verifyDelivery("bitbucket", header, body, []byte(receiverTestToken)))
}

// TestReceiverIndex indexes only a published path.
func TestReceiverIndex(t *testing.T) {
	assert.Nil(t, IndexGitReceiverWebhookPath(&v1alpha3.GitProvider{}), "only GitReceivers carry a route")
	assert.Nil(t, IndexGitReceiverWebhookPath(&v1alpha3.GitReceiver{}), "no published path, no route")
	assert.Equal(t, []string{route("gh")}, IndexGitReceiverWebhookPath(
		receiverTestReceiver("gh", v1alpha3.ReceiverTypeGitHub, "receiver-token")))
}

// TestParseDelivery_ReadsEveryRepositorySpelling collects each URL a host fills, so a provider
// spelling its repository any of those ways is the one the delivery is for, and none that names no
// clone URL.
func TestParseDelivery_ReadsEveryRepositorySpelling(t *testing.T) {
	body := `{"ref":"refs/heads/main","repository":{` +
		`"clone_url":"https://github.com/acme/platform.git","ssh_url":"git@github.com:acme/platform.git",` +
		`"git_url":"git://github.com/acme/platform.git","html_url":"https://github.com/acme/platform",` +
		`"url":"https://api.example/repos/acme/platform","homepage":"https://acme.example"},` +
		`"project":{"git_ssh_url":"git@gitlab.example:acme/platform.git"}}`
	delivery, err := parseDelivery(v1alpha3.ReceiverTypeGitHub, []byte(body))
	require.NoError(t, err)
	assert.Equal(t, "main", delivery.branch)
	assert.Equal(t, map[string]struct{}{
		"github.com/acme/platform":     {},
		"gitlab.example/acme/platform": {},
	}, delivery.repositories)
	assert.True(t, delivery.names("ssh://git@github.com:22/Acme/Platform"))
	assert.False(t, delivery.names(""), "an empty URL names no repository")
	assert.False(t, delivery.names("https://api.example/repos/acme/platform"), "an API URL is not read")
}
