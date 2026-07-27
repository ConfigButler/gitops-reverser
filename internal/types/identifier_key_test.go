// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestResourceIdentifier_Key_GoldenFormat pins the exact strings ResourceIdentifier.Key()
// produces.
//
// THIS FORMAT IS DEPENDED ON ACROSS PRODUCT BOUNDARIES. Tools built around GitOps Reverser
// key their own rows on this identity and join them against ours; because this package
// lives under internal/ they cannot import it, so they reimplement the format from the
// method's doc comment. Changing any byte of it is a BREAKING CHANGE rather than a
// refactor. If this test goes red, the question is not "how do I update the fixture" but
// "who is holding rows keyed on the old string, and how do they learn".
//
// The four cases below are the four shapes of the format, not four samples of it: the two
// optional segments (group, namespace) are absent and present independently, and they
// behave in opposite ways — an empty group is emitted (the key leads with "/") while an
// empty namespace is dropped (the key has four segments, not five with a hole).
func TestResourceIdentifier_Key_GoldenFormat(t *testing.T) {
	tests := []struct {
		name       string
		identifier ResourceIdentifier
		want       string
	}{
		{
			name: "namespaced, grouped - all five segments present",
			identifier: ResourceIdentifier{
				Group:     "apps",
				Version:   "v1",
				Resource:  "deployments",
				Namespace: "prod",
				Name:      "api",
			},
			want: "apps/v1/deployments/prod/api",
		},
		{
			// The namespace segment is DROPPED, not emitted empty: a reimplementation
			// that always joins five parts produces "…/clusterroles//admin" and never
			// joins against this.
			name: "cluster-scoped, grouped - four segments, no empty namespace",
			identifier: ResourceIdentifier{
				Group:     "rbac.authorization.k8s.io",
				Version:   "v1",
				Resource:  "clusterroles",
				Namespace: "",
				Name:      "admin",
			},
			want: "rbac.authorization.k8s.io/v1/clusterroles/admin",
		},
		{
			// The core group is empty and IS emitted, so the key has a leading "/".
			// Opposite rule to the namespace above.
			name: "namespaced, core group - leading slash",
			identifier: ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  "secrets",
				Namespace: "prod",
				Name:      "db",
			},
			want: "/v1/secrets/prod/db",
		},
		{
			// The sharpest case: both optional segments are empty and each takes the
			// other's rule. One degenerates to a leading "/", the other vanishes.
			name: "cluster-scoped, core group - both rules at once",
			identifier: ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  "nodes",
				Namespace: "",
				Name:      "node-1",
			},
			want: "/v1/nodes/node-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.identifier.Key(),
				"Key() format is a cross-product contract; see this test's doc comment before updating it")
		})
	}
}

// TestResourceIdentifier_Key_DistinguishesResources guards the property the format exists
// for: two identifiers that differ anywhere must not collide. The cluster-scoped case is
// the one worth stating, since dropping a segment is exactly how a format loses injectivity.
func TestResourceIdentifier_Key_DistinguishesResources(t *testing.T) {
	// A cluster-scoped resource named "admin" and a namespaced one in a namespace
	// called "admin": the dropped segment must not let these read as the same key.
	clusterScoped := ResourceIdentifier{Group: "g", Version: "v1", Resource: "widgets", Name: "admin"}
	namespaced := ResourceIdentifier{Group: "g", Version: "v1", Resource: "widgets", Namespace: "admin", Name: "x"}
	assert.NotEqual(t, clusterScoped.Key(), namespaced.Key())

	// Every field participates.
	base := ResourceIdentifier{Group: "apps", Version: "v1", Resource: "deployments", Namespace: "prod", Name: "api"}
	variants := map[string]ResourceIdentifier{
		"group":     {Group: "batch", Version: "v1", Resource: "deployments", Namespace: "prod", Name: "api"},
		"version":   {Group: "apps", Version: "v2", Resource: "deployments", Namespace: "prod", Name: "api"},
		"resource":  {Group: "apps", Version: "v1", Resource: "statefulsets", Namespace: "prod", Name: "api"},
		"namespace": {Group: "apps", Version: "v1", Resource: "deployments", Namespace: "stage", Name: "api"},
		"name":      {Group: "apps", Version: "v1", Resource: "deployments", Namespace: "prod", Name: "web"},
	}
	for field, v := range variants {
		assert.NotEqual(t, base.Key(), v.Key(), "identifiers differing in %s must not share a key", field)
	}
}

// TestResourceIdentifier_Key_VersionSplitsWhereGitPathDoesNot pins the documented
// disagreement between the two identity functions, so the decision recorded at both
// methods cannot quietly stop being true: a preferred-version bump is a new Key and the
// same Git path.
func TestResourceIdentifier_Key_VersionSplitsWhereGitPathDoesNot(t *testing.T) {
	v1 := ResourceIdentifier{Group: "example.com", Version: "v1", Resource: "widgets", Namespace: "prod", Name: "a"}
	v2 := v1
	v2.Version = "v2"

	assert.NotEqual(t, v1.Key(), v2.Key(), "Key is the API-side identity and includes the version")
	assert.Equal(t, v1.ToGitPath(), v2.ToGitPath(), "the Git path is the durable identity and omits the version")
}

// ExampleResourceIdentifier_Key shows the four shapes of the key format, including the two
// empty-segment rules that pull in opposite directions.
func ExampleResourceIdentifier_Key() {
	namespaced := ResourceIdentifier{
		Group: "apps", Version: "v1", Resource: "deployments", Namespace: "prod", Name: "api",
	}
	clusterScoped := ResourceIdentifier{
		Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles", Name: "admin",
	}
	coreNamespaced := ResourceIdentifier{
		Version: "v1", Resource: "secrets", Namespace: "prod", Name: "db",
	}
	coreClusterScoped := ResourceIdentifier{
		Version: "v1", Resource: "nodes", Name: "node-1",
	}

	fmt.Println(namespaced.Key())
	fmt.Println(clusterScoped.Key())     // no empty namespace segment
	fmt.Println(coreNamespaced.Key())    // empty group: a leading "/"
	fmt.Println(coreClusterScoped.Key()) // both rules at once

	// Output:
	// apps/v1/deployments/prod/api
	// rbac.authorization.k8s.io/v1/clusterroles/admin
	// /v1/secrets/prod/db
	// /v1/nodes/node-1
}

// ExampleResourceIdentifier_ToGitPath contrasts the two identity functions on one object:
// the key carries the API version, the path deliberately does not.
func ExampleResourceIdentifier_ToGitPath() {
	deployment := ResourceIdentifier{
		Group: "apps", Version: "v1", Resource: "deployments", Namespace: "prod", Name: "api",
	}
	node := ResourceIdentifier{Version: "v1", Resource: "nodes", Name: "node-1"}

	fmt.Println(deployment.Key())
	fmt.Println(deployment.ToGitPath())
	fmt.Println(node.Key())
	fmt.Println(node.ToGitPath()) // cluster-scoped resources live under "_cluster"

	// Output:
	// apps/v1/deployments/prod/api
	// prod/apps/deployments/api.yaml
	// /v1/nodes/node-1
	// _cluster/nodes/node-1.yaml
}
