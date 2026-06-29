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

package recorder

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
	"github.com/ConfigButler/gitops-reverser/internal/mutationlab/store"
)

const maxAuditBody = 32 << 20 // 32 MiB, generous for batched audit EventLists.

// auditEventList recovers each event's exact bytes from the posted EventList, so
// the corpus stores what the apiserver actually sent rather than a re-encoded
// round trip.
type auditEventList struct {
	Items []json.RawMessage `json:"items"`
}

// auditEventLite is a deliberately lenient projection of audit.k8s.io/v1 Event:
// it reads only the fields the lab's summary and key need, using plain types.
//
// The typed auditv1.Event is NOT used here on purpose. Its timestamps are
// metav1.MicroTime, whose strict RFC3339-micro parser rejects an otherwise valid
// event if any one timestamp is off, dropping it whole; and its embedded objects
// need the audit codec. The corpus already keeps the raw bytes verbatim, so the
// summary only needs these few fields — extracted without those fragilities.
type auditEventLite struct {
	AuditID    string `json:"auditID"`
	Verb       string `json:"verb"`
	RequestURI string `json:"requestURI"`
	User       struct {
		Username string `json:"username"`
	} `json:"user"`
	ObjectRef *struct {
		Resource        string `json:"resource"`
		Subresource     string `json:"subresource"`
		Namespace       string `json:"namespace"`
		Name            string `json:"name"`
		APIVersion      string `json:"apiVersion"`
		ResourceVersion string `json:"resourceVersion"`
	} `json:"objectRef"`
	ResponseStatus *struct {
		Code int32 `json:"code"`
	} `json:"responseStatus"`
	RequestObject  json.RawMessage `json:"requestObject"`
	ResponseObject json.RawMessage `json:"responseObject"`
}

// Audit records kube-apiserver audit-webhook EventList posts into the store.
type Audit struct {
	store *store.Store
}

// NewAudit returns an Audit recorder for the kube-apiserver audit webhook.
func NewAudit(s *store.Store) *Audit {
	return &Audit{store: s}
}

// ServeHTTP decodes one EventList and records each event. The apiserver audit
// backend only needs a 2xx; the response body is ignored, so a bad request is
// the only non-200 path.
func (a *Audit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAuditBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var list auditEventList
	if err := json.Unmarshal(body, &list); err != nil {
		http.Error(w, "decode EventList", http.StatusBadRequest)
		return
	}
	for _, raw := range list.Items {
		a.recordEvent(raw)
	}
	w.WriteHeader(http.StatusOK)
}

func (a *Audit) recordEvent(raw json.RawMessage) {
	var ev auditEventLite
	if err := json.Unmarshal(raw, &ev); err != nil {
		return
	}
	a.store.Add(mutationlab.Record{
		Source:     mutationlab.SourceAudit,
		Scenario:   scenarioFromAuditEvent(&ev),
		ObservedAt: time.Now(),
		Key:        auditObjectKey(&ev),
		Summary:    auditSummary(&ev),
		Raw:        raw,
	})
}

// scenarioFromAuditEvent prefers the scenario label carried on the
// request/response object, so an unlabeled object Kubernetes auto-creates in the
// scenario namespace (e.g. kube-root-ca.crt) attributes to the namespace, not the
// scenario, and is excluded by a label-scoped read. It falls back to the
// requestURI (a label selector, then the namespace path) for name-less requests
// like deletecollection, and finally to objectRef.Namespace.
func scenarioFromAuditEvent(ev *auditEventLite) string {
	if labels := objectLabels(ev.ResponseObject, ev.RequestObject); labels[ScenarioLabel] != "" {
		return labels[ScenarioLabel]
	}
	if s := scenarioFromRequestURI(ev.RequestURI); s != "" {
		return s
	}
	if ev.ObjectRef != nil {
		return ev.ObjectRef.Namespace
	}
	return ""
}

func auditObjectKey(ev *auditEventLite) mutationlab.ObjectKey {
	key := mutationlab.ObjectKey{}
	if ref := ev.ObjectRef; ref != nil {
		key.Resource = ref.Resource
		key.Subresource = ref.Subresource
		key.Namespace = ref.Namespace
		key.Name = ref.Name
		key.Version = ref.APIVersion
		key.ResourceVersion = ref.ResourceVersion
	}
	return key
}

func auditSummary(ev *auditEventLite) mutationlab.RecordSummary {
	s := mutationlab.RecordSummary{
		AuditID:           ev.AuditID,
		Operation:         ev.Verb,
		User:              ev.User.Username,
		HasRequestObject:  hasRawBody(ev.RequestObject),
		HasResponseObject: hasRawBody(ev.ResponseObject),
	}
	if ev.ResponseStatus != nil {
		s.ResponseCode = ev.ResponseStatus.Code
	}
	return s
}

// hasRawBody reports whether a raw audit body field carries an object (not absent
// and not JSON null).
func hasRawBody(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
