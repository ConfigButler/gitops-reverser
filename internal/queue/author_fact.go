// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"context"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/runtime/schema"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	"github.com/ConfigButler/gitops-reverser/internal/auditutil"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// DefaultCollectionUIDCap is how many uids a collection fact may carry before the set is dropped
// and the join falls back to scope matching.
//
// It bounds one entry's size, the broadcast to every subscriber of the type, and the replay on
// restart. A uid is 36 bytes, so this cap is a few hundred kilobytes at worst — against a response
// body for the same request that runs to tens of megabytes. It is a tuning number rather than a
// correctness one: the fallback is already correct, so the cap only decides how often the precise
// path is taken, and a collection delete large enough to exceed it is exactly the one whose body a
// production cluster with audit truncation enabled would not have sent in the first place.
const DefaultCollectionUIDCap = 10000

// labelSelectorQueryParam is where a collection request states which objects it meant.
const labelSelectorQueryParam = "labelSelector"

// AuthorFactFromEvent reduces one accepted, mutating audit event to the fact the stream carries,
// reporting false when the event can never name an author. Only facts that WOULD have been stored
// may be published: an event with no objectRef or no user produces nothing, or waiters are woken by
// facts that can name nobody.
//
// The one rule that changes from the per-key write path is the name check. A deletecollection is
// name-less by nature and is now exactly the case that produces a fact — one fact describing the
// COLLECTION, which every removal in its scope joins — so "no resolvable name" becomes "no name and
// not a collection verb".
//
// The caller has already applied the intrinsic accept gate: reads, failures, dry runs, and
// non-ResponseComplete stages never reach here.
func AuthorFactFromEvent(ctx context.Context, event auditv1.Event) (AuthorFact, schema.GroupResource, bool) {
	if event.ObjectRef == nil || event.ObjectRef.Resource == "" {
		return AuthorFact{}, schema.GroupResource{}, false
	}
	user := resolveUserInfo(event)
	if user.Username == "" {
		return AuthorFact{}, schema.GroupResource{}, false
	}

	collection := strings.EqualFold(event.Verb, deleteCollectionVerb)
	op, _ := auditutil.VerbToOperation(event.Verb)
	identity := auditutil.IdentityFromAuditEvent(event, op)
	if identity.Name == "" && !collection {
		return AuthorFact{}, schema.GroupResource{}, false
	}

	groupResource := schema.GroupResource{Group: event.ObjectRef.APIGroup, Resource: event.ObjectRef.Resource}
	fact := AuthorFact{
		GroupResource:    groupResourceKey(groupResource.Group, groupResource.Resource),
		Namespace:        identity.Namespace,
		Name:             identity.Name,
		UID:              string(identity.UID),
		Author:           user.Username,
		DisplayName:      user.DisplayName,
		Email:            user.Email,
		Verb:             event.Verb,
		Subresource:      event.ObjectRef.Subresource,
		AuditID:          string(event.AuditID),
		ResourceVersion:  resourceVersionFromEvent(event),
		IsServiceAccount: strings.HasPrefix(user.Username, serviceAccountUserPrefix),
	}
	if !event.StageTimestamp.IsZero() {
		fact.StageTimestamp = event.StageTimestamp.UTC().Format(time.RFC3339Nano)
	}
	if collection {
		describeCollection(ctx, &fact, event)
	}
	return fact, groupResource, true
}

// describeCollection turns an object-shaped fact into a fact about a COLLECTION: what the actor
// asked for — the type, the namespace, and the selector from the request URI — plus the uids the
// API server said it covered, when it sent them.
//
// The per-object identity goes away, because a collection request names no object. That is the
// asymmetry the expander used to fight: audit reports the ONE request that was made, the watch
// reports each of the N objects that changed, and the join belongs at the point where both are in
// hand rather than in a receiver rebuilding N from one.
func describeCollection(ctx context.Context, fact *AuthorFact, event auditv1.Event) {
	fact.Name = ""
	fact.UID = ""
	fact.ResourceVersion = ""
	fact.LabelSelector = labelSelectorFromRequestURI(event.RequestURI)
	fact.UIDs = collectionUIDs(ctx, event)
}

// labelSelectorFromRequestURI reads the selector a collection request expressed. It is better
// evidence than the response body: the selector is the INTENT the actor stated, evaluating it
// against the object a watch event carries tests membership directly, and it is there even when the
// body is not. An empty selector means the request covered everything of its type in its namespace,
// which is what --all means.
func labelSelectorFromRequestURI(requestURI string) string {
	if requestURI == "" {
		return ""
	}
	parsed, err := url.Parse(requestURI)
	if err != nil {
		return ""
	}
	return parsed.Query().Get(labelSelectorQueryParam)
}

// collectionUIDs reduces a deletecollection response body to the set of uids it covered, at the
// receiver, on the goroutine that already has the body decoded. It returns nil when the body was
// absent, hollow, or larger than the cap — all of which degrade the join to scope matching, which
// is the floor that must work on its own. Crossing the cap is COUNTED, so "we fell back to scope"
// is visible rather than inferred.
func collectionUIDs(ctx context.Context, event auditv1.Event) []string {
	items := deleteCollectionItems(event.ResponseObject)
	if len(items) == 0 {
		return nil
	}
	if len(items) > DefaultCollectionUIDCap {
		recordCollectionDegraded(ctx, "uid_cap")
		return nil
	}
	uids := make([]string, 0, len(items))
	for _, item := range items {
		if item.UID != "" {
			uids = append(uids, string(item.UID))
		}
	}
	if len(uids) == 0 {
		recordCollectionDegraded(ctx, "no_uids")
		return nil
	}
	return uids
}

// recordCollectionDegraded counts one collection fact that lost its uid set, under the bounded
// reason it lost it.
func recordCollectionDegraded(ctx context.Context, reason string) {
	if telemetry.AttributionCollectionDegradedTotal == nil {
		return
	}
	telemetry.AttributionCollectionDegradedTotal.Add(ctx, 1,
		metric.WithAttributes(attribute.String("reason", reason)))
}
