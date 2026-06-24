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

package webhook

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	// ValidateAdmissionWebhookPath is the validating admission webhook endpoint. It
	// currently allows every request and exists as the stable extension point for
	// future policy checks.
	ValidateAdmissionWebhookPath = "/validate-admission-webhook"
)

// AdmissionAllowHandler is a validating admission handler that always allows requests.
type AdmissionAllowHandler struct{}

// Handle returns an allow response for every admission request.
func (AdmissionAllowHandler) Handle(_ context.Context, _ admission.Request) admission.Response {
	return admission.Allowed("allowed")
}
