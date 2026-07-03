// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestAdmissionAllowHandler_AllowsEveryRequest(t *testing.T) {
	resp := AdmissionAllowHandler{}.Handle(context.Background(), ctrladmission.Request{})

	assert.True(t, resp.Allowed)
	assert.Equal(t, "allowed", resp.Result.Message)
}
