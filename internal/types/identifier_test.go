/*
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

package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestResourceIdentifier_ToGitPath(t *testing.T) {
	tests := []struct {
		name       string
		identifier ResourceIdentifier
		want       string
	}{
		{
			name: "core namespaced resource - Pod",
			identifier: ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  "pods",
				Namespace: "default",
				Name:      "nginx",
			},
			want: "v1/pods/default/nginx.yaml",
		},
		{
			name: "core namespaced resource - ConfigMap",
			identifier: ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  "configmaps",
				Namespace: "default",
				Name:      "config",
			},
			want: "v1/configmaps/default/config.yaml",
		},
		{
			name: "core cluster-scoped resource - Node",
			identifier: ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  "nodes",
				Namespace: "",
				Name:      "worker-1",
			},
			want: "v1/nodes/worker-1.yaml",
		},
		{
			name: "non-core namespaced resource - Deployment",
			identifier: ResourceIdentifier{
				Group:     "apps",
				Version:   "v1",
				Resource:  "deployments",
				Namespace: "production",
				Name:      "app",
			},
			want: "apps/v1/deployments/production/app.yaml",
		},
		{
			name: "non-core namespaced resource - StatefulSet",
			identifier: ResourceIdentifier{
				Group:     "apps",
				Version:   "v1",
				Resource:  "statefulsets",
				Namespace: "database",
				Name:      "postgres",
			},
			want: "apps/v1/statefulsets/database/postgres.yaml",
		},
		{
			name: "non-core cluster-scoped resource - ClusterRole",
			identifier: ResourceIdentifier{
				Group:     "rbac.authorization.k8s.io",
				Version:   "v1",
				Resource:  "clusterroles",
				Namespace: "",
				Name:      "admin",
			},
			want: "rbac.authorization.k8s.io/v1/clusterroles/admin.yaml",
		},
		{
			name: "custom CRD with group",
			identifier: ResourceIdentifier{
				Group:     "example.com",
				Version:   "v1alpha1",
				Resource:  "myapps",
				Namespace: "prod",
				Name:      "instance",
			},
			want: "example.com/v1alpha1/myapps/prod/instance.yaml",
		},
		{
			name: "custom CRD cluster-scoped",
			identifier: ResourceIdentifier{
				Group:     "custom.io",
				Version:   "v1beta1",
				Resource:  "globalconfigs",
				Namespace: "",
				Name:      "main",
			},
			want: "custom.io/v1beta1/globalconfigs/main.yaml",
		},
		{
			name: "resource with special characters in name",
			identifier: ResourceIdentifier{
				Group:     "apps",
				Version:   "v1",
				Resource:  "deployments",
				Namespace: "default",
				Name:      "my-app-v2",
			},
			want: "apps/v1/deployments/default/my-app-v2.yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.identifier.ToGitPath()
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResourceIdentifier_IsClusterScoped(t *testing.T) {
	tests := []struct {
		name       string
		identifier ResourceIdentifier
		want       bool
	}{
		{
			name: "namespaced resource",
			identifier: ResourceIdentifier{
				Namespace: "default",
			},
			want: false,
		},
		{
			name: "cluster-scoped resource",
			identifier: ResourceIdentifier{
				Namespace: "",
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.identifier.IsClusterScoped()
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResourceIdentifier_String(t *testing.T) {
	tests := []struct {
		name       string
		identifier ResourceIdentifier
		want       string
	}{
		{
			name: "core resource",
			identifier: ResourceIdentifier{
				Group:    "",
				Version:  "v1",
				Resource: "pods",
				Name:     "nginx",
			},
			want: "v1/pods/nginx",
		},
		{
			name: "non-core resource",
			identifier: ResourceIdentifier{
				Group:    "apps",
				Version:  "v1",
				Resource: "deployments",
				Name:     "app",
			},
			want: "apps/v1/deployments/app",
		},
		{
			name: "custom resource",
			identifier: ResourceIdentifier{
				Group:    "example.com",
				Version:  "v1alpha1",
				Resource: "myapps",
				Name:     "instance",
			},
			want: "example.com/v1alpha1/myapps/instance",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.identifier.String()
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFromAdmissionRequest(t *testing.T) {
	tests := []struct {
		name    string
		request admission.Request
		want    ResourceIdentifier
	}{
		{
			name: "core resource request",
			request: admission.Request{
				AdmissionRequest: admissionv1.AdmissionRequest{
					Resource: metav1.GroupVersionResource{
						Group:    "",
						Version:  "v1",
						Resource: "pods",
					},
					Namespace: "default",
					Name:      "nginx",
				},
			},
			want: ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  "pods",
				Namespace: "default",
				Name:      "nginx",
			},
		},
		{
			name: "non-core resource request",
			request: admission.Request{
				AdmissionRequest: admissionv1.AdmissionRequest{
					Resource: metav1.GroupVersionResource{
						Group:    "apps",
						Version:  "v1",
						Resource: "deployments",
					},
					Namespace: "production",
					Name:      "app",
				},
			},
			want: ResourceIdentifier{
				Group:     "apps",
				Version:   "v1",
				Resource:  "deployments",
				Namespace: "production",
				Name:      "app",
			},
		},
		{
			name: "cluster-scoped resource request",
			request: admission.Request{
				AdmissionRequest: admissionv1.AdmissionRequest{
					Resource: metav1.GroupVersionResource{
						Group:    "rbac.authorization.k8s.io",
						Version:  "v1",
						Resource: "clusterroles",
					},
					Namespace: "",
					Name:      "admin",
				},
			},
			want: ResourceIdentifier{
				Group:     "rbac.authorization.k8s.io",
				Version:   "v1",
				Resource:  "clusterroles",
				Namespace: "",
				Name:      "admin",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FromAdmissionRequest(tt.request)
			assert.Equal(t, tt.want, got)
		})
	}
}
