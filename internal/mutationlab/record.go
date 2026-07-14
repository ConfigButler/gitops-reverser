// SPDX-License-Identifier: Apache-2.0

// Package mutationlab holds the mutation-capture lab: a small, separate
// application that records the exact structures Kubernetes exposes through
// native watches, audit webhooks, and validating admission webhooks, and
// commits those structures as a versioned corpus. It is deliberately NOT a
// second implementation of GitOps Reverser; see
// docs/spec/mutation-capture-lab-design.md.
package mutationlab

import (
	"encoding/json"
	"time"
)

// Source identifies which mechanism produced a Record.
type Source string

const (
	// SourceWatch is a native watch event recorder.
	SourceWatch Source = "watch"
	// SourceAudit is the kube-apiserver audit webhook recorder.
	SourceAudit Source = "audit"
	// SourceAdmission is the validating admission webhook recorder.
	SourceAdmission Source = "admission"
	// SourceConversion is the CRD conversion webhook recorder. The apiserver calls
	// it to convert a custom resource between served versions (Row 14); the lab
	// records each ConversionReview so the corpus shows what the apiserver asked
	// the webhook to convert, and to which version.
	SourceConversion Source = "conversion"
)

// Record is the single envelope that drives both lab layers. Summary feeds the
// structured invariant assertions; Raw (after normalization) becomes the golden
// corpus YAML. A single observation is captured once and emitted twice.
type Record struct {
	ID         string          `json:"id"`
	Source     Source          `json:"source"`
	Scenario   string          `json:"scenario,omitempty"`
	ObservedAt time.Time       `json:"observedAt"`
	Key        ObjectKey       `json:"key"`
	Summary    RecordSummary   `json:"summary"`
	Raw        json.RawMessage `json:"raw"`
}

// ObjectKey is the identity extracted from an observation, used to correlate
// records across the three mechanisms.
type ObjectKey struct {
	Group           string `json:"group,omitempty"`
	Version         string `json:"version,omitempty"`
	Resource        string `json:"resource,omitempty"`
	Subresource     string `json:"subresource,omitempty"`
	Namespace       string `json:"namespace,omitempty"`
	Name            string `json:"name,omitempty"`
	UID             string `json:"uid,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

// RecordSummary is the small, structured projection the invariant assertions
// read. It deliberately records only what was directly observed; nothing here is
// inferred from the payload alone (see the Persisted note).
type RecordSummary struct {
	WatchType    string `json:"watchType,omitempty"`
	AuditID      string `json:"auditID,omitempty"`
	AdmissionUID string `json:"admissionUID,omitempty"`
	Operation    string `json:"operation,omitempty"`
	User         string `json:"user,omitempty"`
	// Persisted is set only by test-side correlation that verifies the object
	// exists (or does not) after the request. It is never guessed from the
	// payload alone — admission and audit both observe attempted writes.
	Persisted         *bool `json:"persisted,omitempty"`
	HasObject         bool  `json:"hasObject"`
	HasOldObject      bool  `json:"hasOldObject"`
	HasRequestObject  bool  `json:"hasRequestObject"`
	HasResponseObject bool  `json:"hasResponseObject"`
	ResponseCode      int32 `json:"responseCode,omitempty"`
}
