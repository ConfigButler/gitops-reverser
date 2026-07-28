// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// mutationEvent builds an apps/deployments event for team-a/web authored by username,
// whose objectRef + responseObject carry uid and resourceVersion rv.
func mutationEvent(verb, uid, rv, username string) auditv1.Event {
	const namespace, name = "team-a", "web"
	body := fmt.Sprintf(`{"apiVersion":"apps/v1","kind":"Deployment",`+
		`"metadata":{"name":%q,"namespace":%q,"uid":%q,"resourceVersion":%q}}`, name, namespace, uid, rv)
	return auditv1.Event{
		AuditID:        "audit-1",
		Verb:           verb,
		Stage:          auditv1.StageResponseComplete,
		StageTimestamp: metav1.MicroTime{Time: time.Now()},
		User:           authnv1.UserInfo{Username: username},
		ObjectRef: &auditv1.ObjectReference{
			APIGroup:   "apps",
			APIVersion: "v1",
			Resource:   "deployments",
			Namespace:  namespace,
			Name:       name,
			UID:        k8stypes.UID(uid),
		},
		ResponseObject: &runtime.Unknown{Raw: []byte(body)},
	}
}

func appsDeploymentGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
}

// collectionDeleteEvent is one name-less collection delete over count objects.
func collectionDeleteEvent(selector string, count int) auditv1.Event {
	items := make([]string, 0, count)
	for i := range count {
		items = append(items, fmt.Sprintf(`{"metadata":{"name":"cm-%d","namespace":"team-a","uid":"uid-%d"}}`, i, i))
	}
	uri := "/api/v1/namespaces/team-a/configmaps"
	if selector != "" {
		uri += "?labelSelector=" + selector
	}
	return auditv1.Event{
		AuditID:    "dc-1",
		Verb:       "deletecollection",
		Stage:      auditv1.StageResponseComplete,
		User:       authnv1.UserInfo{Username: "alice"},
		RequestURI: uri,
		ObjectRef: &auditv1.ObjectReference{
			Resource: "configmaps", Namespace: "team-a", APIVersion: "v1",
		},
		ResponseObject: &runtime.Unknown{
			Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMapList","items":[` + strings.Join(items, ",") + `]}`),
		},
	}
}

func TestAuthorFactFromEvent_CollectionCarriesScopeSelectorAndUIDs(t *testing.T) {
	fact, groupResource, ok := AuthorFactFromEvent(t.Context(), collectionDeleteEvent("app%3Dweb", 2), 0)
	require.True(t, ok, "a name-less deletecollection is the case that DOES produce a fact")
	require.Equal(t, "configmaps", groupResource.Resource)
	require.Equal(t, "team-a", fact.Namespace)
	require.Equal(t, "app=web", fact.LabelSelector)
	require.Equal(t, []string{"uid-0", "uid-1"}, fact.UIDs)
	require.Empty(t, fact.Name)
	require.Empty(t, fact.UID)
}

func TestAuthorFactFromEvent_UIDSetIsDroppedPastTheCapAndCounted(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	fact, _, ok := AuthorFactFromEvent(t.Context(), collectionDeleteEvent("", DefaultCollectionUIDCap+1), 0)
	require.True(t, ok)
	// The fact degrades to scope matching, which is already correct — and says so in the metrics,
	// so "we fell back to scope" is visible rather than inferred.
	require.Nil(t, fact.UIDs)
	require.Empty(t, fact.LabelSelector)

	degraded, found := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_collection_degraded_total",
		map[string]string{"reason": "uid_cap"})
	require.True(t, found)
	require.Equal(t, int64(1), degraded)
}

func TestAuthorFactFromEvent_BodylessCollectionStillProducesAFact(t *testing.T) {
	event := collectionDeleteEvent("", 0)
	event.ResponseObject = nil

	// The shape a production cluster with --audit-webhook-truncate-enabled actually sends, and the
	// one the old expander gave up on entirely.
	fact, _, ok := AuthorFactFromEvent(t.Context(), event, 0)
	require.True(t, ok)
	require.Nil(t, fact.UIDs)
	require.Equal(t, "alice", fact.Author)
}

func TestAuthorFactFromEvent_EventsThatCanNameNobody(t *testing.T) {
	cases := map[string]auditv1.Event{
		"no objectRef": {Verb: "create", User: authnv1.UserInfo{Username: "alice"}},
		"no resource": {
			Verb: "create", User: authnv1.UserInfo{Username: "alice"},
			ObjectRef: &auditv1.ObjectReference{APIGroup: "apps"},
		},
		"no user": {
			Verb:      "create",
			ObjectRef: &auditv1.ObjectReference{Resource: "configmaps", Name: "cm"},
		},
		"no resolvable name on an object verb": {
			Verb: "create", User: authnv1.UserInfo{Username: "alice"},
			ObjectRef: &auditv1.ObjectReference{Resource: "configmaps"},
		},
	}
	for name, event := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, ok := AuthorFactFromEvent(t.Context(), event, 0)
			require.False(t, ok)
		})
	}
}

func TestAuthorFactFromEvent_ObjectWriteCarriesTheIdentityTheJoinNeeds(t *testing.T) {
	fact, groupResource, ok := AuthorFactFromEvent(t.Context(), mutationEvent("update", "uid-1", "101", "alice"), 0)
	require.True(t, ok)
	require.Equal(t, "apps", groupResource.Group)
	require.Equal(t, "deployments", groupResource.Resource)
	require.Equal(t, "apps/deployments", fact.GroupResource)
	require.Equal(t, "uid-1", fact.UID)
	require.Equal(t, "101", fact.ResourceVersion)
	require.Equal(t, "alice", fact.Author)
	require.NotEmpty(t, fact.StageTimestamp)
	// An ordinary write is about one object, so it carries no collection fields.
	require.Empty(t, fact.LabelSelector)
	require.Nil(t, fact.UIDs)
}
