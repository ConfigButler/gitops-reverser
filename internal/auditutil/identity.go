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
	"encoding/json"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

// AuditObjectIdentity is the (namespace, name, uid) of the object referenced by
// an audit event, after backfilling missing fields from the request or response
// body. Any field may be empty when the audit event does not carry it.
type AuditObjectIdentity struct {
	Namespace string
	Name      string
	UID       types.UID
}

// IdentityFromAuditEvent resolves the object identity of an audit event.
//
// It starts from event.ObjectRef — the URL-level reference Kubernetes attached
// to the event — and backfills missing namespace/name/uid fields from the
// audit body that carries the authoritative object for op:
//
//   - for non-delete operations the preferred body is ResponseObject, with
//     RequestObject as fallback;
//   - for delete operations the preferred body is RequestObject, with
//     ResponseObject as fallback.
//
// This handles `metadata.generateName` creates, where the server allocates the
// final name and writes it into responseObject.metadata.name while the
// objectRef points at the collection URL with an empty name.
//
// A malformed body is ignored: the helper never panics and never overwrites a
// value that was already present on objectRef.
func IdentityFromAuditEvent(event auditv1.Event, op configv1alpha1.OperationType) AuditObjectIdentity {
	var id AuditObjectIdentity
	if event.ObjectRef != nil {
		id.Namespace = event.ObjectRef.Namespace
		id.Name = event.ObjectRef.Name
		id.UID = event.ObjectRef.UID
	}

	preferred, fallback := bodyPriority(event, op)
	backfillIdentityFromBody(&id, preferred)
	backfillIdentityFromBody(&id, fallback)
	return id
}

// bodyPriority returns the (preferred, fallback) audit bodies for op. For
// non-delete ops the response body carries the authoritative post-write state;
// for deletes the request body carries the authoritative pre-delete state.
func bodyPriority(event auditv1.Event, op configv1alpha1.OperationType) (*runtime.Unknown, *runtime.Unknown) {
	if op == configv1alpha1.OperationDelete {
		return event.RequestObject, event.ResponseObject
	}
	return event.ResponseObject, event.RequestObject
}

// backfillIdentityFromBody fills missing namespace/name/uid fields on id from
// the metadata block of the given audit body. Already-populated fields are
// never overwritten. A nil or malformed body is a no-op.
func backfillIdentityFromBody(id *AuditObjectIdentity, body *runtime.Unknown) {
	if id.Namespace != "" && id.Name != "" && id.UID != "" {
		return
	}
	if body == nil || len(body.Raw) == 0 {
		return
	}

	var envelope struct {
		Metadata struct {
			Namespace string    `json:"namespace,omitempty"`
			Name      string    `json:"name,omitempty"`
			UID       types.UID `json:"uid,omitempty"`
		} `json:"metadata,omitempty"`
	}
	if err := json.Unmarshal(body.Raw, &envelope); err != nil {
		return
	}

	if id.Namespace == "" {
		id.Namespace = envelope.Metadata.Namespace
	}
	if id.Name == "" {
		id.Name = envelope.Metadata.Name
	}
	if id.UID == "" {
		id.UID = envelope.Metadata.UID
	}
}
