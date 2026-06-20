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
	"strings"

	"k8s.io/apimachinery/pkg/types"

	configv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
	"github.com/ConfigButler/gitops-reverser/internal/auditutil"
)

const (
	// commitRequestResource is the plural resource name of the CommitRequest CRD.
	commitRequestResource = "commitrequests"

	// commitRequestAuthorScanCount bounds one backwards scan of the
	// commitrequests audit stream. CommitRequests are rare control objects and
	// the stream is trimmed like every per-type log, so the create event being
	// looked for is at (or very near) the top.
	commitRequestAuthorScanCount = 512
)

// LookupCommitRequestAuthor scans the CommitRequest type's main audit stream
// backwards for the `create` event of the object identified by
// namespace/name (and UID, when both sides carry one) and returns the
// audit-resolved author username. ok=false means the event has not been
// observed yet — the webhook may still be ingesting it — or a transient Redis
// failure; the caller retries on its own cadence and applies its own bound.
//
// This is the attribution source for the controller-driven CommitRequest
// finalize (C-B2): every request enters through the audit ingestion path, the
// CommitRequest's own create included, and mirrorByType lands it in
// `…commitrequests:audit:stream`. Only the main ordered stream is scanned: a
// CommitRequest create whose audit event was diverted (RV below the high-water)
// is rejected from that stream and stays unattributable, so the finalize fails
// closed — the documented behaviour (rare, since CommitRequests are low-volume).
func (q *RedisByTypeStreamQueue) LookupCommitRequestAuthor(
	ctx context.Context, namespace, name string, uid types.UID,
) (string, bool) {
	base := typeBaseKey(q.prefix, configv1alpha2.GroupVersion.Group, commitRequestResource, "")
	return q.scanForCommitRequestCreate(ctx, base+byTypeAuditStreamSuffix, namespace, name, uid)
}

// scanForCommitRequestCreate reads one stream backwards and returns the author
// of the newest matching CommitRequest create event.
func (q *RedisByTypeStreamQueue) scanForCommitRequestCreate(
	ctx context.Context, streamKey, namespace, name string, uid types.UID,
) (string, bool) {
	entries, err := q.client.XRevRangeN(ctx, streamKey, "+", "-", commitRequestAuthorScanCount).Result()
	if err != nil {
		return "", false
	}
	for _, entry := range entries {
		event, err := parseAuditEvent(entry.Values)
		if err != nil || !strings.EqualFold(event.Verb, "create") {
			continue
		}
		identity := auditutil.IdentityFromAuditEvent(event, configv1alpha2.OperationCreate)
		if identity.Namespace != namespace || identity.Name != name {
			continue
		}
		// A delayed event for a deleted-and-recreated same-named object must
		// not attribute the new incarnation; an absent UID on either side is
		// treated as a match (Metadata-level audit policy omits it).
		if identity.UID != "" && uid != "" && identity.UID != uid {
			continue
		}
		return resolveUserInfo(event).Username, true
	}
	return "", false
}
