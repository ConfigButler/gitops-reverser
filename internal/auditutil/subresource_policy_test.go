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

package auditutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsHardDeniedSubresource(t *testing.T) {
	tests := []struct {
		name        string
		resource    string
		subresource string
		denied      bool
	}{
		{"top-level resource is never denied", "deployments", "", false},
		{"scale is allowed for built-ins", "deployments", "scale", false},
		{"crd scale is allowed", "widgets", "scale", false},
		{"status is denied for any parent", "deployments", "status", true},
		{"crd status is denied", "widgets", "status", true},
		{"finalize is denied for any parent", "namespaces", "finalize", true},
		{"approval is denied", "certificatesigningrequests", "approval", true},
		{"serviceaccount token is denied", "serviceaccounts", "token", true},
		{"pods/exec is denied", "pods", "exec", true},
		{"pods/attach is denied", "pods", "attach", true},
		{"pods/portforward is denied", "pods", "portforward", true},
		{"pods/log is denied", "pods", "log", true},
		{"pods/eviction is denied", "pods", "eviction", true},
		{"pods/binding is denied", "pods", "binding", true},
		{"pods/proxy is denied", "pods", "proxy", true},
		{"services/proxy is denied", "services", "proxy", true},
		{"nodes/proxy is denied", "nodes", "proxy", true},
		{"proxy is not denied for an arbitrary parent", "deployments", "proxy", false},
		{"an unknown subresource is forwarded", "deployments", "throttle", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.denied, IsHardDeniedSubresource(tt.resource, tt.subresource))
		})
	}
}
