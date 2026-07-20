// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var configMapGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

// streamedCM is an unstructured ConfigMap a materialization fold would carry, used across the
// splice / checkpoint tests.
func streamedCM(namespace, name, rv string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
		"data":       map[string]interface{}{"k": "v"},
	}}
	u.SetResourceVersion(rv)
	return u
}

func TestDesiredFromObject(t *testing.T) {
	dr, ok := desiredFromObject(configMapGVR, streamedCM("default", "app", "3"))
	require.True(t, ok)
	assert.Equal(t, "configmaps", dr.Resource.Resource)
	assert.Equal(t, "app", dr.Resource.Name)
	assert.Equal(t, "default", dr.Resource.Namespace)

	_, ok = desiredFromObject(configMapGVR, (*unstructured.Unstructured)(nil))
	assert.False(t, ok, "a nil object is not a desired entry")
}
