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
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	configv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
	"github.com/ConfigButler/gitops-reverser/internal/auditutil"
)

const (
	// commitRequestResource is the plural resource name of the CommitRequest CRD.
	commitRequestResource = "commitrequests"

	// commitRequestAuthorKeySuffix stores one keyed author-attribution fact per
	// CommitRequest create event. It is deliberately separate from the per-type
	// ordered audit stream so attribution survives demand-gating and RV diverts.
	commitRequestAuthorKeySuffix = ":commitrequests:authors"

	// commitRequestAuthorTTL bounds the attribution facts. The controller fails
	// closed after 60s if it still cannot attribute; keeping records for longer
	// absorbs leader failover, post-failover reconcile backlog, and slow
	// reconciles without growing Redis unboundedly.
	commitRequestAuthorTTL = 10 * time.Minute
)

type commitRequestAuthorRecord struct {
	Namespace       string    `json:"namespace"`
	Name            string    `json:"name"`
	UID             types.UID `json:"uid,omitempty"`
	Author          string    `json:"author"`
	AuditID         string    `json:"auditID,omitempty"`
	ResourceVersion string    `json:"resourceVersion,omitempty"`
	StageTimestamp  string    `json:"stageTimestamp,omitempty"`
}

// CaptureCommitRequestAuthor records the author of an accepted CommitRequest
// create audit event before the event is demand-gated or written into the
// ordered per-type stream. ok=false means the event is not a CommitRequest
// create or does not carry a resolvable namespace/name.
func (q *RedisByTypeStreamQueue) CaptureCommitRequestAuthor(ctx context.Context, event auditv1.Event) (bool, error) {
	if !isCommitRequestCreate(event) {
		return false, nil
	}

	identity := auditutil.IdentityFromAuditEvent(event, configv1alpha2.OperationCreate)
	if identity.Namespace == "" || identity.Name == "" {
		return false, nil
	}

	record := commitRequestAuthorRecord{
		Namespace:       identity.Namespace,
		Name:            identity.Name,
		UID:             identity.UID,
		Author:          resolveUserInfo(event).Username,
		AuditID:         string(event.AuditID),
		ResourceVersion: resourceVersionFromEvent(event),
	}
	if !event.StageTimestamp.IsZero() {
		record.StageTimestamp = event.StageTimestamp.UTC().Format(time.RFC3339Nano)
	}

	raw, err := json.Marshal(record)
	if err != nil {
		return true, fmt.Errorf("failed to marshal CommitRequest author record: %w", err)
	}
	if err := q.storeCommitRequestAuthorRecord(ctx, identity.Namespace, identity.Name, identity.UID, raw); err != nil {
		return true, err
	}
	if identity.UID != "" {
		if err := q.storeCommitRequestAuthorRecord(ctx, identity.Namespace, identity.Name, "", raw); err != nil {
			return true, err
		}
	}
	return true, nil
}

// LookupCommitRequestAuthor reads the CommitRequest create-author fact captured
// at audit ingestion. ok=false means the create event has not been observed yet
// — the webhook may still be ingesting it — or a transient Redis failure; the
// caller retries on its own cadence and applies its own fail-closed bound.
func (q *RedisByTypeStreamQueue) LookupCommitRequestAuthor(
	ctx context.Context, namespace, name string, uid types.UID,
) (string, bool) {
	for _, candidateUID := range []types.UID{uid, ""} {
		raw, err := q.client.Get(ctx, q.commitRequestAuthorKey(namespace, name, candidateUID)).Bytes()
		if err != nil {
			continue
		}
		var record commitRequestAuthorRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			continue
		}
		if record.Namespace != namespace || record.Name != name {
			continue
		}
		if record.UID != "" && uid != "" && record.UID != uid {
			continue
		}
		return record.Author, record.Author != ""
	}
	return "", false
}

func (q *RedisByTypeStreamQueue) storeCommitRequestAuthorRecord(
	ctx context.Context,
	namespace, name string,
	uid types.UID,
	raw []byte,
) error {
	key := q.commitRequestAuthorKey(namespace, name, uid)
	if err := q.client.Set(ctx, key, raw, commitRequestAuthorTTL).Err(); err != nil {
		return fmt.Errorf("failed to store CommitRequest author record %q: %w", key, err)
	}
	return nil
}

func (q *RedisByTypeStreamQueue) commitRequestAuthorKey(namespace, name string, uid types.UID) string {
	return fmt.Sprintf("%s%s:%s", q.prefix, commitRequestAuthorKeySuffix, commitRequestAuthorID(namespace, name, uid))
}

func commitRequestAuthorID(namespace, name string, uid types.UID) string {
	return hex.EncodeToString([]byte(namespace + "\x00" + name + "\x00" + string(uid)))
}

func isCommitRequestCreate(event auditv1.Event) bool {
	if event.ObjectRef == nil {
		return false
	}
	ref := event.ObjectRef
	return event.Verb == "create" &&
		ref.APIGroup == configv1alpha2.GroupVersion.Group &&
		ref.Resource == commitRequestResource &&
		ref.Subresource == ""
}
