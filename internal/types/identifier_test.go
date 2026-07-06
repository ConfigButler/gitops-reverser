// SPDX-License-Identifier: Apache-2.0

package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
			want: "default/pods/nginx.yaml",
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
			want: "default/configmaps/config.yaml",
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
			want: "cluster/nodes/worker-1.yaml",
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
			want: "production/apps/deployments/app.yaml",
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
			want: "database/apps/statefulsets/postgres.yaml",
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
			want: "cluster/rbac.authorization.k8s.io/clusterroles/admin.yaml",
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
			want: "prod/example.com/myapps/instance.yaml",
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
			want: "cluster/custom.io/globalconfigs/main.yaml",
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
			want: "default/apps/deployments/my-app-v2.yaml",
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
