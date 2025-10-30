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

package types

import "fmt"

// ResourceReference references a Kubernetes resource by name and namespace.
// Provides a clean, reusable type for referencing GitDestinations and other resources.
type ResourceReference struct {
	Name      string
	Namespace string
}

// NewResourceReference creates a new resource reference.
func NewResourceReference(name, namespace string) ResourceReference {
	return ResourceReference{
		Name:      name,
		Namespace: namespace,
	}
}

// String returns "namespace/name" format.
func (r ResourceReference) String() string {
	return fmt.Sprintf("%s/%s", r.Namespace, r.Name)
}

// Key returns a string key suitable for map lookups.
func (r ResourceReference) Key() string {
	return r.String()
}

// Equal checks if two references are equal.
func (r ResourceReference) Equal(other ResourceReference) bool {
	return r.Name == other.Name && r.Namespace == other.Namespace
}

// IsZero returns true if this is an empty reference.
func (r ResourceReference) IsZero() bool {
	return r.Name == "" && r.Namespace == ""
}
