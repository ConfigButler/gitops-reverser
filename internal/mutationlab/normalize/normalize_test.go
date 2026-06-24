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

package normalize

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// marshalCompact marshals without HTML-escaping (so the <ph-N> placeholders stay
// readable) and trims the newline the encoder appends.
func marshalCompact(t *testing.T, v any) string {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return strings.TrimRight(buf.String(), "\n")
}

func normJSON(t *testing.T, payloads ...string) []string {
	t.Helper()
	raw := make([]json.RawMessage, len(payloads))
	for i, p := range payloads {
		raw[i] = json.RawMessage(p)
	}
	out, err := Normalize(raw)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	got := make([]string, len(out))
	for i, v := range out {
		got[i] = marshalCompact(t, v)
	}
	return got
}

func TestNormalize_UIDRelationalAcrossRecords(t *testing.T) {
	// Same UID in two records collapses to <uid-1>; a distinct UID becomes <uid-2>.
	got := normJSON(t,
		`{"metadata":{"uid":"abc","name":"cm-a"}}`,
		`{"request":{"uid":"abc"},"metadata":{"uid":"def"}}`,
	)
	if want := `{"metadata":{"name":"cm-a","uid":"<uid-1>"}}`; got[0] != want {
		t.Errorf("record 0\n got %s\nwant %s", got[0], want)
	}
	if want := `{"metadata":{"uid":"<uid-2>"},"request":{"uid":"<uid-1>"}}`; got[1] != want {
		t.Errorf("record 1\n got %s\nwant %s", got[1], want)
	}
}

func TestNormalize_ResourceVersionNumericOrdering(t *testing.T) {
	// Observed out of order; all integers => numeric order, so the smaller RV is
	// <rv-1> even though it appeared second.
	got := normJSON(t,
		`{"metadata":{"resourceVersion":"200"}}`,
		`{"metadata":{"resourceVersion":"100"}}`,
		`{"metadata":{"resourceVersion":"300"}}`,
	)
	want := []string{
		`{"metadata":{"resourceVersion":"<rv-2>"}}`,
		`{"metadata":{"resourceVersion":"<rv-1>"}}`,
		`{"metadata":{"resourceVersion":"<rv-3>"}}`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %v\nwant %v", got, want)
	}
}

func TestNormalize_ResourceVersionNonNumericFirstAppearance(t *testing.T) {
	// A non-integer RV forces the whole category to first-appearance order.
	got := normJSON(t,
		`{"metadata":{"resourceVersion":"zzz"}}`,
		`{"metadata":{"resourceVersion":"aaa"}}`,
	)
	want := []string{
		`{"metadata":{"resourceVersion":"<rv-1>"}}`,
		`{"metadata":{"resourceVersion":"<rv-2>"}}`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %v\nwant %v", got, want)
	}
}

func TestNormalize_TimestampsChronological(t *testing.T) {
	// Captured out of chronological order; <ts-1> is the earliest instant.
	got := normJSON(t,
		`{"metadata":{"creationTimestamp":"2026-01-02T00:00:00Z"}}`,
		`{"stageTimestamp":"2026-01-01T00:00:00Z"}`,
	)
	want := []string{
		`{"metadata":{"creationTimestamp":"<ts-2>"}}`,
		`{"stageTimestamp":"<ts-1>"}`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %v\nwant %v", got, want)
	}
}

func TestNormalize_GenerateNameSuffix(t *testing.T) {
	// The random suffix becomes <rand-1>; the stable generateName prefix stays.
	got := normJSON(t, `{"metadata":{"generateName":"cm-","name":"cm-x7k2p"}}`)
	want := `{"metadata":{"generateName":"cm-","name":"cm-<rand-1>"}}`
	if got[0] != want {
		t.Errorf("\n got %s\nwant %s", got[0], want)
	}
}

func TestNormalize_SourceIPs(t *testing.T) {
	got := normJSON(t, `{"sourceIPs":["10.0.0.1","10.0.0.2","10.0.0.1"]}`)
	want := `{"sourceIPs":["<ip-1>","<ip-2>","<ip-1>"]}`
	if got[0] != want {
		t.Errorf("\n got %s\nwant %s", got[0], want)
	}
}

func TestNormalize_AuditID(t *testing.T) {
	got := normJSON(t, `{"auditID":"req-xyz","verb":"create"}`)
	want := `{"auditID":"<auditID-1>","verb":"create"}`
	if got[0] != want {
		t.Errorf("\n got %s\nwant %s", got[0], want)
	}
}

func TestNormalize_PreservesStableIdentity(t *testing.T) {
	// Names, data, and managedFields content are preserved verbatim; only the
	// volatile uid/resourceVersion/timestamp are rewritten.
	got := normJSON(t, `{"metadata":{"name":"cm-a","namespace":"lab","uid":"u1",`+
		`"resourceVersion":"42","managedFields":[{"manager":"kubectl","time":"2026-01-01T00:00:00Z"}]},`+
		`"data":{"key":"value"}}`)
	want := `{"data":{"key":"value"},"metadata":{"managedFields":[{"manager":"kubectl","time":"<ts-1>"}],` +
		`"name":"cm-a","namespace":"<ns-1>","resourceVersion":"<rv-1>","uid":"<uid-1>"}}`
	if got[0] != want {
		t.Errorf("\n got %s\nwant %s", got[0], want)
	}
}

func TestNormalize_NamespaceInRequestURI(t *testing.T) {
	// A unique per-run namespace appears both under the namespace key and embedded
	// in requestURI; both must collapse to <ns-1> so the corpus stays diff-free.
	got := normJSON(t,
		`{"objectRef":{"namespace":"lab-create-succeeds-k1yh"},`+
			`"requestURI":"/api/v1/namespaces/lab-create-succeeds-k1yh/configmaps?labelSelector=x%3Dy"}`)
	want := `{"objectRef":{"namespace":"<ns-1>"},` +
		`"requestURI":"/api/v1/namespaces/<ns-1>/configmaps?labelSelector=x%3Dy"}`
	if got[0] != want {
		t.Errorf("\n got %s\nwant %s", got[0], want)
	}
}

func TestNormalize_Deterministic(t *testing.T) {
	payloads := []string{
		`{"metadata":{"uid":"a","resourceVersion":"5"},"sourceIPs":["1.1.1.1"]}`,
		`{"metadata":{"uid":"b","resourceVersion":"9"}}`,
	}
	first := normJSON(t, payloads...)
	second := normJSON(t, payloads...)
	if !reflect.DeepEqual(first, second) {
		t.Errorf("non-deterministic output:\n first %v\nsecond %v", first, second)
	}
}

func TestNormalize_DeletecollectionFanoutKeepsDistinctIdentities(t *testing.T) {
	// Row 9: three objects removed by one request must keep distinct uids/rvs so
	// the fan-out is visible in the corpus instead of collapsing to one token.
	got := normJSON(t,
		`{"type":"DELETED","object":{"metadata":{"name":"cm-a","uid":"ua","resourceVersion":"10"}}}`,
		`{"type":"DELETED","object":{"metadata":{"name":"cm-b","uid":"ub","resourceVersion":"11"}}}`,
		`{"type":"DELETED","object":{"metadata":{"name":"cm-c","uid":"uc","resourceVersion":"12"}}}`,
	)
	want := []string{
		`{"object":{"metadata":{"name":"cm-a","resourceVersion":"<rv-1>","uid":"<uid-1>"}},"type":"DELETED"}`,
		`{"object":{"metadata":{"name":"cm-b","resourceVersion":"<rv-2>","uid":"<uid-2>"}},"type":"DELETED"}`,
		`{"object":{"metadata":{"name":"cm-c","resourceVersion":"<rv-3>","uid":"<uid-3>"}},"type":"DELETED"}`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %v\nwant %v", got, want)
	}
}

func TestNormalize_FinalizerTerminalDeleteSameUIDHigherRV(t *testing.T) {
	// Row 8: the terminal DELETED keeps the same <uid> but a higher <rv>, proving
	// it is the same object at a later resourceVersion.
	got := normJSON(t,
		`{"type":"MODIFIED","object":{"metadata":{"uid":"u1","resourceVersion":"100"}}}`,
		`{"type":"DELETED","object":{"metadata":{"uid":"u1","resourceVersion":"105"}}}`,
	)
	want := []string{
		`{"object":{"metadata":{"resourceVersion":"<rv-1>","uid":"<uid-1>"}},"type":"MODIFIED"}`,
		`{"object":{"metadata":{"resourceVersion":"<rv-2>","uid":"<uid-1>"}},"type":"DELETED"}`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %v\nwant %v", got, want)
	}
}

func TestNormalize_NumbersRoundTripAsIntegers(t *testing.T) {
	got := normJSON(t, `{"responseStatus":{"code":200},"metadata":{"generation":3}}`)
	want := `{"metadata":{"generation":3},"responseStatus":{"code":200}}`
	if got[0] != want {
		t.Errorf("\n got %s\nwant %s", got[0], want)
	}
}

func TestSingle(t *testing.T) {
	v, err := Single(json.RawMessage(`{"metadata":{"uid":"x"}}`))
	if err != nil {
		t.Fatalf("Single: %v", err)
	}
	if got, want := marshalCompact(t, v), `{"metadata":{"uid":"<uid-1>"}}`; got != want {
		t.Errorf("got %s want %s", got, want)
	}
}

func TestNormalize_InvalidJSON(t *testing.T) {
	if _, err := Normalize([]json.RawMessage{json.RawMessage(`{not json`)}); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}
