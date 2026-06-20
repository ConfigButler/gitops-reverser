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

package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	configv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
)

// This file is the audit-event → Kubernetes-object interpretation layer shared
// by every per-type stream consumer: the splice (redis_type_splice.go), the
// freshness tail (redis_bytype_queue.go), and the CommitRequest attribution
// lookup (commitrequest_author.go). It survived the canonical-stream consumer
// it was extracted from (C-C, docs/design/stream/canonical-stream-retirement.md).

var errAuditEventObjectMissing = errors.New("audit event has no requestObject or responseObject")

// errAuditEventObjectIsStatus marks an audit event whose extracted body is a
// metav1.Status error response (apiVersion: v1, kind: Status) rather than a
// real resource. The API server emits such a body when a request fails — most
// commonly a 409 Conflict from an optimistic-concurrency clash.
var errAuditEventObjectIsStatus = errors.New("audit event object is a metav1.Status error body")

// errAuditEventObjectPartial marks an audit event whose body is valid JSON but
// lacks the apiVersion/kind identity of a full Kubernetes object — typically a
// merge-patch fragment such as {"metadata":{"finalizers":null}} recorded as the
// requestObject of a finalizer-removal PATCH.
var errAuditEventObjectPartial = errors.New("audit event object body is a partial object (no kind)")

// Scale-translation drop outcomes. translateScaleToAssignments labels why a scale
// event could not become a replicas assignment; since DEC-A the per-type consumers
// (freshness tail, splice fold) drop silently with the next checkpoint as backstop,
// so these are diagnostic strings only.
// See docs/design/manifest/version2/subresource-scope-reduction.md.
const (
	// pipelineOutcomeDroppedNonScale marks a subresource event that is not /scale.
	pipelineOutcomeDroppedNonScale = "dropped_non_scale_subresource"
	// pipelineOutcomeDroppedScaleMissingReplicas marks a scale event whose
	// responseObject carries no spec.replicas to commit.
	pipelineOutcomeDroppedScaleMissingReplicas = "dropped_scale_missing_response_replicas"
	// pipelineOutcomeDroppedScalePathUnresolved marks a scale event whose parent has no
	// known replica path — a CRD or aggregated API in this pass.
	pipelineOutcomeDroppedScalePathUnresolved = "dropped_scale_path_unresolved"
)

const (
	// displayNameExtraKey is the audit-event user.extra key carrying the OIDC
	// "name" claim, when the API server is configured to map it.
	displayNameExtraKey = "configbutler.ai/claims/display-name"
	// emailExtraKey is the audit-event user.extra key carrying the OIDC
	// "email" claim, when the API server is configured to map it.
	emailExtraKey = "configbutler.ai/claims/email"
)

// resolveUserInfo extracts the effective user identity from an audit event,
// preferring the impersonated user when present. When the effective user
// carries the OIDC display-name / email extras, those populate the optional
// UserInfo fields; absent values are left empty so commit authoring falls back
// to the username.
func resolveUserInfo(event auditv1.Event) git.UserInfo {
	user := event.User
	if event.ImpersonatedUser != nil && event.ImpersonatedUser.Username != "" {
		user = *event.ImpersonatedUser
	}

	return git.UserInfo{
		Username:    user.Username,
		DisplayName: firstExtraValue(user.Extra, displayNameExtraKey),
		Email:       firstExtraValue(user.Extra, emailExtraKey),
	}
}

// firstExtraValue returns the first value for key in an audit event's
// user.extra map, or "" when the key is absent or carries no values.
func firstExtraValue(extra map[string]authnv1.ExtraValue, key string) string {
	values := extra[key]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// parseAuditEvent unmarshals the audit event from the stream entry's payload_json field.
func parseAuditEvent(values map[string]interface{}) (auditv1.Event, error) {
	raw, ok := values["payload_json"]
	if !ok {
		return auditv1.Event{}, errors.New("stream entry missing payload_json field")
	}

	var payload string
	switch v := raw.(type) {
	case string:
		payload = v
	case []byte:
		payload = string(v)
	default:
		return auditv1.Event{}, fmt.Errorf("unexpected payload_json type %T", raw)
	}

	var event auditv1.Event
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return auditv1.Event{}, fmt.Errorf("failed to unmarshal audit event: %w", err)
	}
	return event, nil
}

// extractObject obtains the Kubernetes object from the audit event and sanitizes it. For DELETE
// operations the RequestObject is used; otherwise the ResponseObject. It is the audit-body→object
// conversion the per-type splice reuses verbatim (redis_type_splice.foldAuditEntry) to turn a
// logged audit entry into the Git-writable object the reconcile commits (DEC-5/§6).
func extractObject(
	event auditv1.Event,
	op configv1alpha2.OperationType,
	apiVersion, resource, namespace, name string,
) (*unstructured.Unstructured, error) {
	raw := selectAuditObjectRaw(event, op)

	if len(raw) == 0 {
		if !allowsBodylessSingleDelete(event, resource, name) {
			return nil, errAuditEventObjectMissing
		}
		u := &unstructured.Unstructured{}
		u.SetAPIVersion(apiVersion)
		u.SetKind(resource)
		u.SetNamespace(namespace)
		u.SetName(name)
		return u, nil
	}

	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(raw); err != nil {
		if isPartialObjectBody(raw) {
			return nil, errAuditEventObjectPartial
		}
		return nil, fmt.Errorf("failed to unmarshal object JSON: %w", err)
	}

	if isStatusObject(obj) {
		return nil, errAuditEventObjectIsStatus
	}

	return backfillSanitizedIdentity(sanitize.Sanitize(obj), apiVersion, resource, namespace, name), nil
}

// isPartialObjectBody reports whether raw is well-formed JSON describing an object that lacks a
// "kind" — the condition that makes (*Unstructured).UnmarshalJSON fail on an otherwise valid body.
func isPartialObjectBody(raw []byte) bool {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	kind, _ := m["kind"].(string)
	return kind == ""
}

// isStatusObject reports whether obj is a core metav1.Status error response (apiVersion: v1,
// kind: Status) rather than a real Kubernetes resource, so it is never written to Git.
func isStatusObject(obj *unstructured.Unstructured) bool {
	return obj != nil && obj.GetAPIVersion() == "v1" && obj.GetKind() == "Status"
}

func allowsBodylessSingleDelete(event auditv1.Event, resource, name string) bool {
	return strings.EqualFold(event.Verb, "delete") &&
		resource != "" &&
		name != ""
}

func selectAuditObjectRaw(event auditv1.Event, op configv1alpha2.OperationType) []byte {
	if op == configv1alpha2.OperationDelete {
		return firstAuditObjectRaw(event.RequestObject, event.ResponseObject)
	}

	return firstAuditObjectRaw(event.ResponseObject, event.RequestObject)
}

func firstAuditObjectRaw(objects ...*runtime.Unknown) []byte {
	for _, object := range objects {
		if object != nil && len(object.Raw) > 0 {
			return object.Raw
		}
	}

	return nil
}

func backfillSanitizedIdentity(
	sanitized *unstructured.Unstructured,
	apiVersion, resource, namespace, name string,
) *unstructured.Unstructured {
	if sanitized.GetAPIVersion() == "" {
		sanitized.SetAPIVersion(apiVersion)
	}
	if sanitized.GetKind() == "" {
		sanitized.SetKind(resource)
	}
	if sanitized.GetNamespace() == "" {
		sanitized.SetNamespace(namespace)
	}
	if sanitized.GetName() == "" {
		sanitized.SetName(name)
	}

	return sanitized
}

func effectiveAuditOperation(event auditv1.Event, op configv1alpha2.OperationType) configv1alpha2.OperationType {
	if op == configv1alpha2.OperationDelete {
		return op
	}
	if auditEventObjectMarkedForDeletion(event) {
		return configv1alpha2.OperationDelete
	}
	return op
}

func auditEventObjectMarkedForDeletion(event auditv1.Event) bool {
	return auditObjectMarkedForDeletion(event.ResponseObject) || auditObjectMarkedForDeletion(event.RequestObject)
}

func auditObjectMarkedForDeletion(rawObj *runtime.Unknown) bool {
	if rawObj == nil || len(rawObj.Raw) == 0 {
		return false
	}

	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(rawObj.Raw); err != nil {
		return false
	}

	return !obj.GetDeletionTimestamp().IsZero()
}

// stringField safely reads a string value from a stream entry's Values map.
func stringField(values map[string]interface{}, key string) string {
	v, ok := values[key]
	if !ok {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	default:
		return fmt.Sprintf("%v", v)
	}
}
