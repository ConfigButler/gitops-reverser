// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	"github.com/ConfigButler/gitops-reverser/internal/auditutil"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// DefaultAttributionFactTTL is how long an attribution fact stays joinable while it waits for the
// matching watch event. It bounds the stream's retention horizon and the in-memory index together,
// and it doubles as the follower's replay horizon, so a restart warms the index with exactly the
// window that is still usable. After it elapses a miss is simply "absent": there is no tombstone,
// so an aged-out fact is indistinguishable from one that never arrived.
// Configurable via --author-attribution-ttl.
const DefaultAttributionFactTTL = 10 * time.Minute

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

// serviceAccountUserPrefix is how the API server spells a service account in an audit event's user.
const serviceAccountUserPrefix = "system:serviceaccount:"

// AttributionResult is the bounded resolver outcome recorded for each watch event. The set is the
// join's tier table, so a reading of attribution_resolutions_total{tier} says which evidence named
// the author, not merely that one was named. It is the source of truth for that label: a lookup
// that could not say which tier answered is the thing the tier label exists to fix, so the split
// lives in the enum rather than at the metric boundary.
//
// WHO was named is a separate question and a separate label — see ActorKind. The two used to be
// crammed into one value (exact_user against exact_serviceaccount), which made counting exact
// resolutions a sum of two series and made the actor kind unaskable of every other tier.
type AttributionResult string

const (
	// AttributionRemoval is the sticky removal pointer: a fact whose own verb is a delete, filed by
	// uid into a slot no later WRITE fact may overwrite. It is the strongest evidence a removal can
	// have about itself, so it is consulted before the exact tier — and only by a removal, because an
	// exact-capable event asks who produced a version rather than who deleted an object.
	//
	// It is also the only tier the TTL does not bound. A uid is unique across space and time, so the
	// statement can never be superseded; its horizon is the index's caps instead. See
	// docs/design/attribution-deletion-intent-actor.md.
	AttributionRemoval AttributionResult = "removal"
	// AttributionExact is an exact UID+resourceVersion match: this actor produced this exact version.
	AttributionExact AttributionResult = "exact"
	// AttributionLatest is the uid-latest tier — the object's own last write or its own delete fact,
	// keyed by uid alone. It is the tier the removal path turns on: a match here that describes a
	// WRITE is held as a fallback while the wait continues for evidence about the deletion.
	AttributionLatest AttributionResult = "latest"
	// AttributionResourceVersion is the rv-only escape hatch: a fact that carried a resourceVersion
	// and no uid, matched on that version alone. It and AttributionLatest were one value ("weak")
	// and are different evidence, which is why they are now two.
	AttributionResourceVersion AttributionResult = "resource_version"
	// AttributionCollectionUID is a removal matched to a deletecollection fact whose uid set
	// contains this object. There is no over-attribution risk in it: either the API server said it
	// deleted this object, or it did not.
	AttributionCollectionUID AttributionResult = "collection_uid"
	// AttributionName is a match on (namespace, name) for a fact that carries neither a uid nor a
	// resourceVersion. It is the tier of last resort for a type whose audit event cannot express
	// object identity: the kube-apiserver proxies an aggregated-API request and never decodes the
	// response, so the objectRef carries the name from the URL path and nothing else, and there is no
	// body to backfill from. Measured in corpus flunder/aggregated-api-delete.
	//
	// It ranks below every other per-object tier because a name is REUSED after a delete and
	// recreate where a uid is not, so it can name the author of a previous object that held this
	// name. The TTL is what bounds that: the wrong answer requires the recreate to happen inside it.
	AttributionName AttributionResult = "name"
	// AttributionCollectionScope is a removal matched to a deletecollection fact by scope alone —
	// same type and namespace, selector accepting the object's labels, within the collection window.
	// It is the weakest evidence the join has, which is why it is reached only when every more
	// specific tier missed. It is also the tier that resolves what the deleted expander gave up on:
	// a collection delete the API server sent no response body for.
	AttributionCollectionScope AttributionResult = "collection_scope"
	// AttributionAbsent means no usable author fact matched before the grace elapsed.
	AttributionAbsent AttributionResult = "absent"
)

// AuthorFact is the minimal attribution fact published per accepted, mutating audit event and read
// back by the watch-event resolver. It names an author candidate and carries the evidence the join
// needs to decide confidence; it is never object state.
//
// Every field here is either read by the join or printed when a fact is investigated. That is a
// deliberate bar, because a fact is not stored once: it is broadcast to every process following its
// type, held for the whole TTL, and replayed into memory on every restart, so a field nothing reads
// is paid for on all three. Three fields were removed for failing it:
//
//   - the group/resource, which is the STREAM'S OWN NAME — the index takes the scope from the
//     entry's key, never from the fact, so carrying it duplicated the routing on every entry;
//   - the subresource, which no tier joins on and nothing logs.
//
// A stored is-service-account bool went the same way, for a different reason: it is not evidence,
// it is a prefix check on Author that the reader can do for itself (see ActorKind).
//
// Name was removed with the subresource, on the same observation — no tier read it — and is back,
// because that observation was true of the code and false of the domain. An aggregated-API write is
// audited with no uid and no resourceVersion, and the name from the URL path is the ONLY identity it
// carries, so a fact without it could not be joined at all for that whole population. "No code reads
// it" and "nothing could ever read it" are different claims, and only the second justifies dropping a
// field.
type AuthorFact struct { //nolint:recvcheck // UnmarshalJSON must take a pointer; every other method only reads.
	Namespace string `json:"namespace,omitempty"`
	UID       string `json:"uid,omitempty"`
	// Name is the object's name, and it feeds one tier only: the (namespace, name) join a fact with
	// no uid and no resourceVersion is otherwise unreachable through. A collection fact clears it,
	// because a collection request names no object.
	Name string `json:"name,omitempty"`
	// Author is the actor's username, and it is the ONE required field on the wire: a fact exists to
	// name somebody, so a fact that names nobody is not a weak fact, it is not a fact. It is never
	// empty and never null — see UnmarshalJSON, which refuses an entry carrying one.
	Author string `json:"author"`
	// DisplayName and Email are the actor's, when the API server supplied them. They are the only
	// fields here that are not identity or evidence: they exist because a commit author is a name
	// and an email, and re-deriving them at commit time would need a second lookup.
	DisplayName string `json:"displayName,omitempty"`
	Email       string `json:"email,omitempty"`
	Verb        string `json:"verb,omitempty"`
	// AuditID is the one field the join never reads and is kept anyway. It is what ties a commit
	// authored by the wrong person back to the audit event that named them, which is the single
	// question a mis-attribution investigation asks and the one thing nothing else in the system
	// can answer.
	AuditID         string `json:"auditID,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
	StageTimestamp  string `json:"stageTimestamp,omitempty"`

	// LabelSelector is the selector the request URI expressed, carried on a COLLECTION fact only.
	// It is the intent the actor stated, and evaluating it against the object a watch event carries
	// is a better test of membership than reading back a list the API server may not have sent.
	// Empty means the collection covered everything of its type in its namespace, which is what
	// --all means.
	LabelSelector string `json:"labelSelector,omitempty"`
	// UIDs is the set of objects a collection delete covered, reduced from the response body at the
	// receiver, on a COLLECTION fact only. It is absent when the API server sent no body — a
	// truncated, aggregated, or metadata-only response — and when the set was larger than the cap,
	// in which case the join falls back to scope matching, which is already correct.
	UIDs []string `json:"uids,omitempty"`
}

// errFactWithoutAuthor is what an entry violating the fact contract decodes to.
var errFactWithoutAuthor = errors.New("attribution fact carries no author")

// UnmarshalJSON decodes a fact and refuses one that names nobody, which is the whole wire contract:
// `author` must be present, a string, and non-empty. Missing, `null`, and `""` are the same
// violation and are all refused.
//
// Go cannot express "a string of at least one character" as a type — every type has a zero value
// that is constructible without going through any constructor, and `encoding/json` writes exported
// fields straight past one anyway — so the constraint lives at the only boundary that can hold it:
// the point where a fact written by somebody else enters this process.
//
// The refusal is deliberately at ENTRY granularity, not per fact. This operator's publish gate
// cannot produce an authorless fact (AuthorFactFromEvent refuses an event whose user is
// unresolvable, and counts it as no_attribution_fact), so an entry carrying one was written by
// something else: a different version, a different producer, or a hand-written entry. That is a
// protocol violation rather than a low-quality fact, and it is better counted and logged loudly —
// it lands on attribution_fact_stream_decode_errors_total with the stream and entry id — than
// half-absorbed by silently dropping one fact out of a batch.
func (f *AuthorFact) UnmarshalJSON(raw []byte) error {
	// wire has AuthorFact's fields and tags but none of its methods, so decoding it does not recurse.
	type wire AuthorFact
	var decoded wire
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	if decoded.Author == "" {
		return fmt.Errorf("%w (auditID %q, verb %q)", errFactWithoutAuthor, decoded.AuditID, decoded.Verb)
	}
	*f = AuthorFact(decoded)
	return nil
}

// AuthorResolution is the structured result of an attribution lookup.
type AuthorResolution struct {
	Fact   AuthorFact
	Result AttributionResult
}

// ActorKind is the bounded kind of actor a resolution named. It is the same vocabulary
// commits_total{author_kind} uses, so the two metrics stop disagreeing about the shape of one
// distinction, and it is orthogonal to the tier: every tier can name either kind of actor, or none.
type ActorKind string

const (
	// ActorKindUser is a human (or any non-service-account subject) named by the matched fact.
	ActorKindUser ActorKind = "user"
	// ActorKindServiceAccount is a named service account.
	ActorKindServiceAccount ActorKind = "serviceaccount"
	// ActorKindNone is no actor at all: nothing matched, or the fact that matched carried no author.
	ActorKindNone ActorKind = "none"
)

// ActorKind classifies the actor a fact names. It is derived rather than carried: the API server
// spells a service account one way and only one way, so a stored kind would be the same check,
// denormalized onto every fact and able to disagree with the name beside it.
func (f AuthorFact) ActorKind() ActorKind {
	switch {
	case f.Author == "":
		return ActorKindNone
	case strings.HasPrefix(f.Author, serviceAccountUserPrefix):
		return ActorKindServiceAccount
	default:
		return ActorKindUser
	}
}

// ActorKind classifies the actor this resolution named, which is none when no fact matched.
func (r AuthorResolution) ActorKind() ActorKind {
	if r.Result == AttributionAbsent {
		return ActorKindNone
	}
	return r.Fact.ActorKind()
}

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
func AuthorFactFromEvent(
	ctx context.Context,
	event auditv1.Event,
	uidCap int,
) (AuthorFact, schema.GroupResource, bool) {
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
		Namespace:       identity.Namespace,
		UID:             string(identity.UID),
		Name:            identity.Name,
		Author:          user.Username,
		DisplayName:     user.DisplayName,
		Email:           user.Email,
		Verb:            event.Verb,
		AuditID:         string(event.AuditID),
		ResourceVersion: resourceVersionFromEvent(event),
	}
	if !event.StageTimestamp.IsZero() {
		fact.StageTimestamp = event.StageTimestamp.UTC().Format(time.RFC3339Nano)
	}
	if collection {
		describeCollection(ctx, &fact, event, uidCap)
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
func describeCollection(ctx context.Context, fact *AuthorFact, event auditv1.Event, uidCap int) {
	fact.UID = ""
	fact.Name = ""
	fact.ResourceVersion = ""
	fact.LabelSelector = labelSelectorFromRequestURI(event.RequestURI)
	fact.UIDs = collectionUIDs(ctx, event, uidCap)
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
func collectionUIDs(ctx context.Context, event auditv1.Event, uidCap int) []string {
	if uidCap <= 0 {
		uidCap = DefaultCollectionUIDCap
	}
	items := deleteCollectionItems(event.ResponseObject)
	if len(items) == 0 {
		return nil
	}
	if len(items) > uidCap {
		recordCollectionWithoutUIDSet(ctx, "uid_cap")
		return nil
	}
	uids := make([]string, 0, len(items))
	for _, item := range items {
		if item.UID != "" {
			uids = append(uids, string(item.UID))
		}
	}
	if len(uids) == 0 {
		recordCollectionWithoutUIDSet(ctx, "no_uids")
		return nil
	}
	return uids
}

// deleteCollectionItem is the per-object identity read from a deletecollection response list. It is
// all that survives of the deleted expander, and it survives for one job: reducing the body to the
// uid SET the fact carries. Nothing rebuilds N per-object facts from one request any more.
type deleteCollectionItem struct {
	UID types.UID
}

// deleteCollectionItems parses the per-object identities from a deletecollection response body. It
// accepts any list-shaped body (a typed "…List", a v1.List, or anything carrying an "items" array)
// and returns nil for a Status, hollow, or unparseable body — the join then falls back to scope
// matching, which is the floor that must work on its own.
func deleteCollectionItems(obj *runtime.Unknown) []deleteCollectionItem {
	if obj == nil || len(obj.Raw) == 0 {
		return nil
	}
	var envelope struct {
		Items []struct {
			Metadata struct {
				UID types.UID `json:"uid"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(obj.Raw, &envelope); err != nil {
		return nil
	}
	items := make([]deleteCollectionItem, 0, len(envelope.Items))
	for _, it := range envelope.Items {
		items = append(items, deleteCollectionItem{UID: it.Metadata.UID})
	}
	return items
}

// resourceVersionFromEvent returns the event's ResourceVersion when one is available, or "" when it
// is not (deletes, collection verbs, shallow bodies). The post-write RV lives in the response
// object's metadata.resourceVersion; requestObject.resourceVersion is the pre-write RV on
// update-style requests, so it is intentionally ignored. objectRef.resourceVersion is usually the
// empty precondition RV on writes, so it is only the last resort.
func resourceVersionFromEvent(event auditv1.Event) string {
	if rv := rvFromRawObject(event.ResponseObject); rv != "" {
		return rv
	}
	if event.ObjectRef != nil {
		return event.ObjectRef.ResourceVersion
	}
	return ""
}

func rvFromRawObject(obj *runtime.Unknown) string {
	if obj == nil || len(obj.Raw) == 0 {
		return ""
	}
	var probe struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(obj.Raw, &probe); err != nil {
		return ""
	}
	return probe.Metadata.ResourceVersion
}

// RecordFactsWritten counts facts appended to the fact log. It is called by the publish side, once
// per append rather than once per entry, so the counter measures facts rather than audit batches
// and stays comparable with the matched op on the other side of the join.
func RecordFactsWritten(ctx context.Context, count int) {
	if telemetry.AttributionFactsTotal == nil || count <= 0 {
		return
	}
	telemetry.AttributionFactsTotal.Add(ctx, int64(count),
		metric.WithAttributes(attribute.String("op", factOpWritten)))
}

// recordCollectionWithoutUIDSet counts one collection fact that lost its uid set, under the bounded
// reason it lost it.
func recordCollectionWithoutUIDSet(ctx context.Context, reason string) {
	if telemetry.AttributionCollectionWithoutUIDSetTotal == nil {
		return
	}
	telemetry.AttributionCollectionWithoutUIDSetTotal.Add(ctx, 1,
		metric.WithAttributes(attribute.String("reason", reason)))
}
