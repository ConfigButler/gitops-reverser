// SPDX-License-Identifier: Apache-2.0

package recorder

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
	"github.com/ConfigButler/gitops-reverser/internal/mutationlab/store"
)

const maxAdmissionBody = 8 << 20

// RejectLabel marks an object the default RejectByLabel policy must
// record-and-reject; set it to "true". This is how Row 12 deterministically
// produces "admission saw a write that never persisted" without depending on a
// second webhook's ordering.
const RejectLabel = "mutationlab.configbutler.ai/reject"

// scenarioAuditAnnotation is the breadcrumb the recorder returns in
// AdmissionResponse.auditAnnotations. Kubernetes surfaces these in the audit log
// prefixed by the webhook name, giving the lab a controlled join between "this
// admission call" and "this audit request" — without pretending a shared native
// ID exists.
const scenarioAuditAnnotation = "scenario"

// RejectPolicy decides whether the admission recorder record-and-rejects a
// request. It is given the decoded request and the labels of the admitted object.
type RejectPolicy func(req *admissionv1.AdmissionRequest, labels map[string]string) (reject bool, message string)

// RejectByLabel rejects a request when its object carries RejectLabel=true.
func RejectByLabel(_ *admissionv1.AdmissionRequest, labels map[string]string) (bool, string) {
	if labels[RejectLabel] == "true" {
		return true, "rejected by mutation-capture-lab record-and-reject policy"
	}
	return false, ""
}

// admissionReviewEnvelope recovers the request's exact bytes so the corpus shows
// the AdmissionReview.request shape the apiserver actually sent.
type admissionReviewEnvelope struct {
	Request json.RawMessage `json:"request"`
}

// Admission is the lab's validating admission recorder. It allows by default but
// can record-and-reject per its RejectPolicy. The lab includes admission
// precisely because it is tempting — it sees the user and object before the write
// — and the corpus's job is to show why that temptation is a trap.
type Admission struct {
	store  *store.Store
	policy RejectPolicy
}

// NewAdmission returns an Admission recorder using the given reject policy
// (RejectByLabel is the usual choice; pass nil to always allow).
func NewAdmission(s *store.Store, policy RejectPolicy) *Admission {
	if policy == nil {
		policy = func(*admissionv1.AdmissionRequest, map[string]string) (bool, string) { return false, "" }
	}
	return &Admission{store: s, policy: policy}
}

// ServeHTTP decodes one AdmissionReview, records it, and returns the allow/reject
// decision.
func (a *Admission) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAdmissionBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var envelope admissionReviewEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil || len(envelope.Request) == 0 {
		http.Error(w, "decode AdmissionReview", http.StatusBadRequest)
		return
	}
	var req admissionv1.AdmissionRequest
	if err := json.Unmarshal(envelope.Request, &req); err != nil {
		http.Error(w, "decode AdmissionRequest", http.StatusBadRequest)
		return
	}

	labels := objectLabels(req.Object.Raw, req.OldObject.Raw)
	scenario := scenarioFromLabels(labels, req.Namespace)
	reject, message := a.policy(&req, labels)

	a.store.Add(mutationlab.Record{
		Source:     mutationlab.SourceAdmission,
		Scenario:   scenario,
		ObservedAt: time.Now(),
		Key:        admissionObjectKey(&req),
		Summary:    admissionSummary(&req, !reject),
		Raw:        wrapRequest(envelope.Request),
	})

	writeAdmissionResponse(w, &req, scenario, reject, message)
}

func wrapRequest(rawRequest json.RawMessage) json.RawMessage {
	out, err := json.Marshal(map[string]json.RawMessage{"request": rawRequest})
	if err != nil {
		return rawRequest
	}
	return out
}

func writeAdmissionResponse(
	w http.ResponseWriter,
	req *admissionv1.AdmissionRequest,
	scenario string,
	reject bool,
	message string,
) {
	resp := admissionv1.AdmissionResponse{
		UID:              req.UID,
		Allowed:          !reject,
		AuditAnnotations: map[string]string{scenarioAuditAnnotation: scenario},
	}
	if reject {
		resp.Result = &metav1.Status{Message: message, Code: http.StatusForbidden}
	}
	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Response: &resp,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(review)
}

func admissionObjectKey(req *admissionv1.AdmissionRequest) mutationlab.ObjectKey {
	key := mutationlab.ObjectKey{
		Group:       req.Resource.Group,
		Version:     req.Resource.Version,
		Resource:    req.Resource.Resource,
		Subresource: req.SubResource,
		Namespace:   req.Namespace,
		Name:        req.Name,
		UID:         string(req.UID),
	}
	// A per-object admission call within a deletecollection carries no
	// request.name — only the object. Derive the name from the object so the
	// corpus filenames stay stable (admission.delete.cm-a, not ordinal .1/.2).
	if key.Name == "" {
		key.Name = objectName(req.Object.Raw, req.OldObject.Raw)
	}
	return key
}

// objectName returns metadata.name from the first non-empty of the given raw
// objects (object, then oldObject for deletes).
func objectName(raws ...[]byte) string {
	for _, raw := range raws {
		if len(raw) == 0 {
			continue
		}
		var probe struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &probe); err == nil && probe.Metadata.Name != "" {
			return probe.Metadata.Name
		}
	}
	return ""
}

func admissionSummary(req *admissionv1.AdmissionRequest, allowed bool) mutationlab.RecordSummary {
	return mutationlab.RecordSummary{
		AdmissionUID: string(req.UID),
		Operation:    string(req.Operation),
		User:         req.UserInfo.Username,
		Persisted:    nil, // never inferred from admission; only test-side correlation sets it.
		HasObject:    len(req.Object.Raw) > 0,
		HasOldObject: len(req.OldObject.Raw) > 0,
		ResponseCode: admissionResponseCode(allowed),
	}
}

func admissionResponseCode(allowed bool) int32 {
	if allowed {
		return http.StatusOK
	}
	return http.StatusForbidden
}

// objectLabels extracts metadata.labels from the first non-empty of the given raw
// objects (object, then oldObject for deletes).
func objectLabels(raws ...[]byte) map[string]string {
	for _, raw := range raws {
		if len(raw) == 0 {
			continue
		}
		var probe struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &probe); err == nil && probe.Metadata.Labels != nil {
			return probe.Metadata.Labels
		}
	}
	return nil
}
