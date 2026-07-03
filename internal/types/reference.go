// SPDX-License-Identifier: Apache-2.0

package types

import "fmt"

// ResourceReference references a Kubernetes resource by name and namespace.
// Provides a clean, reusable type for referencing GitDestinations and other resources.
//
// UID, when set, identifies the specific object generation. It is deliberately
// excluded from String/Key/Equal so in-memory bookkeeping stays keyed by
// namespace/name; it scopes durable Redis keys (e.g. watch cursors) so a recreated
// GitTarget never inherits a deleted predecessor's state.
type ResourceReference struct {
	Name      string
	Namespace string
	UID       string
}

// NewResourceReference creates a new resource reference.
func NewResourceReference(name, namespace string) ResourceReference {
	return ResourceReference{
		Name:      name,
		Namespace: namespace,
	}
}

// WithUID returns a copy of the reference carrying the given object UID.
func (r ResourceReference) WithUID(uid string) ResourceReference {
	r.UID = uid
	return r
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
