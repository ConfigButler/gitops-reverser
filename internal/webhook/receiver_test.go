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
	receiverTestNamespace = "team-a"
	receiverTestToken     = "s3cr3t-token"
	receiverTestRepoURL   = "https://github.com/acme/platform.git"
	receiverTestCommit    = "4f2c1ab9e0d1c2b3a4f5e6d7c8b9a0f1e2d3c4b5"
)

// fakePushNotifier records every NotifyPush call and reports a worker for the branches in workers.
type fakePushNotifier struct {
	workers map[string]bool
	calls   []fakePushCall
}

type fakePushCall struct {
	namespace, name, branch, after string
}

func (f *fakePushNotifier) NotifyPush(providerNamespace, providerName, branch, after string) bool {
	f.calls = append(f.calls, fakePushCall{providerNamespace, providerName, branch, after})
	return f.workers[branch]
}

func receiverTestProvider(name string, receiver *v1alpha3.GitProviderReceiver) *v1alpha3.GitProvider {
	return &v1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: receiverTestNamespace},
		Spec: v1alpha3.GitProviderSpec{
			URL:             receiverTestRepoURL,
			AllowedBranches: []string{"*"},
			Receiver:        receiver,
		},
	}
}

func receiverOf(receiverType v1alpha3.ReceiverType, secret string) *v1alpha3.GitProviderReceiver {
	return &v1alpha3.GitProviderReceiver{Type: receiverType, SecretRef: meta.LocalObjectReference{Name: secret}}
}

func receiverTestSecret(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: receiverTestNamespace},
		Data:       data,
	}
}

// receiverTestObjects is one provider per receiver type sharing one token Secret, plus the
// providers that must not serve or cannot verify yet.
func receiverTestObjects() []client.Object {
	return []client.Object{
		receiverTestSecret("receiver-token", map[string][]byte{v1alpha3.ReceiverTokenKey: []byte(receiverTestToken)}),
		receiverTestSecret("wrong-key", map[string][]byte{"password": []byte(receiverTestToken)}),
		receiverTestProvider("gh", receiverOf(v1alpha3.ReceiverTypeGitHub, "receiver-token")),
		receiverTestProvider("gl", receiverOf(v1alpha3.ReceiverTypeGitLab, "receiver-token")),
		receiverTestProvider("gt", receiverOf(v1alpha3.ReceiverTypeGitea, "receiver-token")),
		receiverTestProvider("gen", receiverOf(v1alpha3.ReceiverTypeGenericHMAC, "receiver-token")),
		receiverTestProvider("plain", nil),
		receiverTestProvider("no-secret", receiverOf(v1alpha3.ReceiverTypeGitHub, "absent")),
		receiverTestProvider("no-key", receiverOf(v1alpha3.ReceiverTypeGitHub, "wrong-key")),
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
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(receiverTestObjects()...)
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

func hostPushBody(ref string) string {
	return `{"ref":"` + ref + `","before":"0000","after":"` + receiverTestCommit + `",` +
		`"repository":{"clone_url":"https://github.com/acme/platform.git"}}`
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
	// wantMatched is the 202 body's matched value; ignored for any other status.
	wantMatched int
	// wantCall is the NotifyPush the delivery must make; nil asserts none was made.
	wantCall *fakePushCall
	// wantType/wantOutcome is the one delivery the metric must count; empty asserts none.
	wantType    string
	wantOutcome string
}

func receiverCases() map[string]receiverCase {
	mainPush := hostPushBody("refs/heads/main")
	otherPush := hostPushBody("refs/heads/feature/x")
	tagPush := hostPushBody("refs/tags/v1.0.0")
	ghCall := &fakePushCall{receiverTestNamespace, "gh", "main", receiverTestCommit}
	genMain := genericBody("git@github.com:acme/platform", "main")

	return map[string]receiverCase{
		// github
		"github push to a worked branch is notified": {
			path: "/git-push/team-a/gh", headers: githubHeaders("push", mainPush, receiverTestToken), body: mainPush,
			wantStatus: http.StatusAccepted, wantMatched: 1, wantCall: ghCall,
			wantType: "github", wantOutcome: deliveryNotified,
		},
		"github push to a branch nothing writes is still a success": {
			path: "/git-push/team-a/gh", headers: githubHeaders("push", otherPush, receiverTestToken), body: otherPush,
			wantStatus: http.StatusAccepted, wantMatched: 0,
			wantCall: &fakePushCall{receiverTestNamespace, "gh", "feature/x", receiverTestCommit},
			wantType: "github", wantOutcome: deliveryUnmatched,
		},
		// GitHub's default content type wraps the JSON in a payload field, and signs the raw body.
		"github form-encoded push reaches the worker": {
			path: "/git-push/team-a/gh",
			headers: func() map[string]string {
				h := githubHeaders("push", formEncoded(mainPush), receiverTestToken)
				h["Content-Type"] = "application/x-www-form-urlencoded"
				return h
			}(),
			body:       formEncoded(mainPush),
			wantStatus: http.StatusAccepted, wantMatched: 1,
			wantCall: &fakePushCall{receiverTestNamespace, "gh", "main", receiverTestCommit},
			wantType: "github", wantOutcome: deliveryNotified,
		},
		"github wrong signature is rejected": {
			path: "/git-push/team-a/gh", headers: githubHeaders("push", mainPush, "not-the-token"), body: mainPush,
			wantStatus: http.StatusUnauthorized, wantType: "github", wantOutcome: deliveryRejected,
		},
		"github missing signature is rejected": {
			path: "/git-push/team-a/gh", headers: map[string]string{headerGitHubEvent: "push"}, body: mainPush,
			wantStatus: http.StatusUnauthorized, wantType: "github", wantOutcome: deliveryRejected,
		},
		"github signature without the sha256 prefix is rejected": {
			path: "/git-push/team-a/gh",
			headers: map[string]string{
				headerGitHubEvent: "push", headerGitHubSignature: hmacHex(mainPush, receiverTestToken),
			},
			body:       mainPush,
			wantStatus: http.StatusUnauthorized, wantType: "github", wantOutcome: deliveryRejected,
		},
		"github signature that is not hex is rejected": {
			path: "/git-push/team-a/gh",
			headers: map[string]string{
				headerGitHubEvent: "push", headerGitHubSignature: "sha256=zz",
			},
			body:       mainPush,
			wantStatus: http.StatusUnauthorized, wantType: "github", wantOutcome: deliveryRejected,
		},
		"github ping is accepted and does nothing": {
			path: "/git-push/team-a/gh", headers: githubHeaders("ping", `{"zen":"hi"}`, receiverTestToken),
			body:       `{"zen":"hi"}`,
			wantStatus: http.StatusAccepted, wantType: "github", wantOutcome: deliveryIgnored,
		},
		"github non-push event is ignored": {
			path: "/git-push/team-a/gh", headers: githubHeaders("issues", `{}`, receiverTestToken), body: `{}`,
			wantStatus: http.StatusAccepted, wantType: "github", wantOutcome: deliveryIgnored,
		},
		"github tag push is ignored": {
			path: "/git-push/team-a/gh", headers: githubHeaders("push", tagPush, receiverTestToken), body: tagPush,
			wantStatus: http.StatusAccepted, wantType: "github", wantOutcome: deliveryIgnored,
		},
		"github verified body that is not JSON is invalid": {
			path: "/git-push/team-a/gh", headers: githubHeaders("push", "not json", receiverTestToken),
			body:       "not json",
			wantStatus: http.StatusBadRequest, wantType: "github", wantOutcome: deliveryInvalid,
		},
		"github verified push without a ref is invalid": {
			path: "/git-push/team-a/gh", headers: githubHeaders("push", `{"after":"abc"}`, receiverTestToken),
			body:       `{"after":"abc"}`,
			wantStatus: http.StatusBadRequest, wantType: "github", wantOutcome: deliveryInvalid,
		},
		"github ref naming no branch is invalid": {
			path: "/git-push/team-a/gh", headers: githubHeaders("push", `{"ref":"refs/heads/"}`, receiverTestToken),
			body:       `{"ref":"refs/heads/"}`,
			wantStatus: http.StatusBadRequest, wantType: "github", wantOutcome: deliveryInvalid,
		},

		// gitlab
		"gitlab push with the token is notified": {
			path: "/git-push/team-a/gl", headers: gitlabHeaders("Push Hook", receiverTestToken), body: mainPush,
			wantStatus: http.StatusAccepted, wantMatched: 1,
			wantCall: &fakePushCall{receiverTestNamespace, "gl", "main", receiverTestCommit},
			wantType: "gitlab", wantOutcome: deliveryNotified,
		},
		"gitlab wrong token is rejected": {
			path: "/git-push/team-a/gl", headers: gitlabHeaders("Push Hook", "nope"), body: mainPush,
			wantStatus: http.StatusUnauthorized, wantType: "gitlab", wantOutcome: deliveryRejected,
		},
		"gitlab missing token is rejected": {
			path: "/git-push/team-a/gl", headers: map[string]string{headerGitLabEvent: "Push Hook"}, body: mainPush,
			wantStatus: http.StatusUnauthorized, wantType: "gitlab", wantOutcome: deliveryRejected,
		},
		"gitlab tag push hook is ignored": {
			path: "/git-push/team-a/gl", headers: gitlabHeaders("Tag Push Hook", receiverTestToken), body: tagPush,
			wantStatus: http.StatusAccepted, wantType: "gitlab", wantOutcome: deliveryIgnored,
		},

		// gitea
		"gitea push is notified": {
			path: "/git-push/team-a/gt", headers: giteaHeaders("push", mainPush, receiverTestToken), body: mainPush,
			wantStatus: http.StatusAccepted, wantMatched: 1,
			wantCall: &fakePushCall{receiverTestNamespace, "gt", "main", receiverTestCommit},
			wantType: "gitea", wantOutcome: deliveryNotified,
		},
		"gitea wrong signature is rejected": {
			path: "/git-push/team-a/gt", headers: giteaHeaders("push", mainPush, "nope"), body: mainPush,
			wantStatus: http.StatusUnauthorized, wantType: "gitea", wantOutcome: deliveryRejected,
		},
		"gitea github-style prefixed signature is rejected": {
			path: "/git-push/team-a/gt",
			headers: map[string]string{
				headerGiteaEvent: "push", headerGiteaSignature: "sha256=" + hmacHex(mainPush, receiverTestToken),
			},
			body:       mainPush,
			wantStatus: http.StatusUnauthorized, wantType: "gitea", wantOutcome: deliveryRejected,
		},
		"gitea non-push event is ignored": {
			path: "/git-push/team-a/gt", headers: giteaHeaders("create", `{}`, receiverTestToken), body: `{}`,
			wantStatus: http.StatusAccepted, wantType: "gitea", wantOutcome: deliveryIgnored,
		},

		// generic-hmac
		"generic-hmac push for the same repository spelled differently is notified": {
			path: "/git-push/team-a/gen", headers: genericHeaders(genMain, receiverTestToken), body: genMain,
			wantStatus: http.StatusAccepted, wantMatched: 1,
			wantCall: &fakePushCall{receiverTestNamespace, "gen", "main", receiverTestCommit},
			wantType: "generic-hmac", wantOutcome: deliveryNotified,
		},
		"generic-hmac full ref is read as its branch": {
			path:       "/git-push/team-a/gen",
			headers:    genericHeaders(genericBody("", "refs/heads/main"), receiverTestToken),
			body:       genericBody("", "refs/heads/main"),
			wantStatus: http.StatusAccepted, wantMatched: 1,
			wantCall: &fakePushCall{receiverTestNamespace, "gen", "main", receiverTestCommit},
			wantType: "generic-hmac", wantOutcome: deliveryNotified,
		},
		"generic-hmac another repository is ignored, not refused": {
			path:       "/git-push/team-a/gen",
			headers:    genericHeaders(genericBody("https://github.com/acme/other", "main"), receiverTestToken),
			body:       genericBody("https://github.com/acme/other", "main"),
			wantStatus: http.StatusAccepted, wantType: "generic-hmac", wantOutcome: deliveryIgnored,
		},
		"generic-hmac tag ref is ignored": {
			path:       "/git-push/team-a/gen",
			headers:    genericHeaders(genericBody("", "refs/tags/v1"), receiverTestToken),
			body:       genericBody("", "refs/tags/v1"),
			wantStatus: http.StatusAccepted, wantType: "generic-hmac", wantOutcome: deliveryIgnored,
		},
		"generic-hmac wrong signature is rejected": {
			path: "/git-push/team-a/gen", headers: genericHeaders(genMain, "nope"), body: genMain,
			wantStatus: http.StatusUnauthorized, wantType: "generic-hmac", wantOutcome: deliveryRejected,
		},
		"generic-hmac without a branch is invalid": {
			path:       "/git-push/team-a/gen",
			headers:    genericHeaders(`{"repository":"x"}`, receiverTestToken),
			body:       `{"repository":"x"}`,
			wantStatus: http.StatusBadRequest, wantType: "generic-hmac", wantOutcome: deliveryInvalid,
		},

		// routes that do not serve, and a receiver that cannot verify yet
		"unknown provider is 404": {
			path:       "/git-push/team-a/missing",
			headers:    githubHeaders("push", mainPush, receiverTestToken),
			body:       mainPush,
			wantStatus: http.StatusNotFound,
		},
		"provider without spec.receiver is 404": {
			path: "/git-push/team-a/plain", headers: githubHeaders("push", mainPush, receiverTestToken), body: mainPush,
			wantStatus: http.StatusNotFound,
		},
		"route without a name is 404": {
			path: "/git-push/team-a", body: mainPush, wantStatus: http.StatusNotFound,
		},
		"route with an extra segment is 404": {
			path: "/git-push/team-a/gh/extra", body: mainPush, wantStatus: http.StatusNotFound,
		},
		"route with an empty name is 404": {
			path: "/git-push/team-a/", body: mainPush, wantStatus: http.StatusNotFound,
		},
		"route with an invalid namespace is 404": {
			path: "/git-push/Team_A/gh", body: mainPush, wantStatus: http.StatusNotFound,
		},
		"path outside the prefix is 404": {
			path: "/other/team-a/gh", body: mainPush, wantStatus: http.StatusNotFound,
		},
		"missing token Secret is 503, not 401": {
			path: "/git-push/team-a/no-secret", headers: githubHeaders("push", mainPush, receiverTestToken),
			body: mainPush, wantStatus: http.StatusServiceUnavailable,
		},
		"token Secret without the token key is 503": {
			path: "/git-push/team-a/no-key", headers: githubHeaders("push", mainPush, receiverTestToken),
			body: mainPush, wantStatus: http.StatusServiceUnavailable,
		},
		"GET is not allowed": {
			method: http.MethodGet, path: "/git-push/team-a/gh", wantStatus: http.StatusMethodNotAllowed,
		},
	}
}

func TestReceiverHandler_ServeHTTP(t *testing.T) {
	for name, tc := range receiverCases() {
		t.Run(name, func(t *testing.T) {
			reader, err := telemetry.InitTestExporter()
			require.NoError(t, err)

			notifier := &fakePushNotifier{workers: map[string]bool{"main": true}}
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
				var resp receiverResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				assert.Equal(t, tc.wantMatched, resp.Matched)
				assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			}

			if tc.wantCall == nil {
				assert.Empty(t, notifier.calls, "a delivery that is not acted on must not reach a worker")
			} else {
				assert.Equal(t, []fakePushCall{*tc.wantCall}, notifier.calls)
			}

			total, _ := telemetry.CollectInt64Sum(reader, "gitopsreverser_receiver_deliveries_total", nil)
			if tc.wantOutcome == "" {
				assert.Zero(t, total, "a request that reaches no receiver is not a delivery")
				return
			}
			assert.Equal(t, int64(1), total, "exactly one delivery is counted")
			got, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_receiver_deliveries_total",
				map[string]string{"type": tc.wantType, "outcome": tc.wantOutcome})
			require.True(t, ok, "no %s/%s delivery recorded", tc.wantType, tc.wantOutcome)
			assert.Equal(t, int64(1), got)
		})
	}
}

// TestReceiverHandler_OversizedBodyIsRefusedBeforeVerification pins the cap: a body over it is
// refused whatever its signature says, because verifying it would mean buffering all of it.
func TestReceiverHandler_OversizedBodyIsRefusedBeforeVerification(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	notifier := &fakePushNotifier{workers: map[string]bool{"main": true}}
	h := newReceiverTestHandler(t, notifier, 64, nil)

	body := hostPushBody("refs/heads/main")
	require.Greater(t, len(body), 64)
	req := httptest.NewRequest(http.MethodPost, "/git-push/team-a/gh", strings.NewReader(body))
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

// TestReceiverHandler_UnreadableProviderIs503 separates "no such route" from "cannot tell yet": an
// API error reading the GitProvider must not answer 404, which a host may treat as a dead hook.
func TestReceiverHandler_UnreadableProviderIs503(t *testing.T) {
	notifier := &fakePushNotifier{}
	h := newReceiverTestHandler(t, notifier, 0, &interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return errors.New("apiserver unavailable")
		},
	})

	body := hostPushBody("refs/heads/main")
	req := httptest.NewRequest(http.MethodPost, "/git-push/team-a/gh", strings.NewReader(body))
	for k, v := range githubHeaders("push", body, receiverTestToken) {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Empty(t, notifier.calls)
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
