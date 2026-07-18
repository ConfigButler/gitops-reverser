// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// FuzzDecodeEventList is the project's dynamic-analysis (fuzzing) gate for the
// network-facing admission/audit surface — the code that decodes untrusted JSON
// posted by the Kubernetes API server. It varies the request body and asserts
// that the whole audit ingress path (decode -> intrinsic accept gate -> record)
// never panics and always answers with a valid HTTP status, and that the sibling
// admission uid probe never panics on the same untrusted bytes.
//
// The handler carries no FactRecorder, so processing is side-effect-free: this
// exercises decoding and classification without a live store. Seed inputs are
// replayed by a plain `go test` (no -fuzz); a fuzz-found crasher is kept under
// testdata/fuzz/FuzzDecodeEventList/ as a regression case.
func FuzzDecodeEventList(f *testing.F) {
	seeds := []string{
		eventListBody(acceptedCreateEvent),
		eventListBody(),
		`{"kind":"EventList","apiVersion":"audit.k8s.io/v1"}`,
		`{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"stage":"ResponseComplete"}]}`,
		`{"items": 5}`, // wrong type for items
		`{`,            // truncated
		`not json`,
		``,
		`{"metadata":{"uid":"abc-123"}}`, // admission uid probe shape
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	// A served route, not the bare endpoint: a bare request is rejected before the body is read, so
	// fuzzing it would never reach the decoder this target exists to exercise.
	handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{MaxRequestBodyBytes: 1 << 20}))
	if err != nil {
		f.Fatalf("NewAuditHandler: %v", err)
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		// The full ingress path must never panic on an untrusted body and must
		// always answer with a syntactically valid HTTP status code.
		req := httptest.NewRequest(http.MethodPost, defaultRoute, bytes.NewReader(body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code < 100 || w.Code > 599 {
			t.Fatalf("invalid HTTP status %d for body %q", w.Code, body)
		}

		// The admission command-authorship path parses the same class of untrusted
		// JSON with a lightweight probe; it must also never panic.
		_ = commandObjectUID(body)
	})
}
