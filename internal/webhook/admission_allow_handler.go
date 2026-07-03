// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	// ValidateAllPath is the broad observe-all validating admission endpoint. It
	// matches every resource and always allows — a test-only capture/observation
	// surface today (the e2e SUT wires it; the Helm chart does not), kept as the
	// stable extension point for a future cluster-wide policy. Per-our-type handling
	// (authorship, config validation) lives on ValidateOperatorTypesPath instead.
	ValidateAllPath = "/validate-all"
)

// AdmissionAllowHandler is a validating admission handler that always allows requests.
type AdmissionAllowHandler struct{}

// Handle returns an allow response for every admission request.
func (AdmissionAllowHandler) Handle(_ context.Context, _ admission.Request) admission.Response {
	return admission.Allowed("allowed")
}
