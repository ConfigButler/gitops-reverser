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
