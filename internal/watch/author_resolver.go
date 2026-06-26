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
	"context"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/queue"
)

// DefaultAttributionGraceWindow is the bounded wait a watch event spends for a
// matching audit fact to arrive in the index before it ships as committer. It is
// the "slack" that makes "a late audit arrival must not rewrite a shipped commit"
// enforceable: we wait briefly BEFORE shipping rather than rewrite afterwards.
const DefaultAttributionGraceWindow = 3 * time.Second

// attributionPollInterval is how often the resolver re-checks the index while it
// waits out the grace window for a fact that has not arrived yet.
const attributionPollInterval = 150 * time.Millisecond

// ServiceAccountNamingPolicy decides how a matched service-account actor is named
// as the commit author. A matched controller is a *named* attribution, not unknown.
type ServiceAccountNamingPolicy string

const (
	// SANamePolicyName authors the commit as the service account's own username
	// (e.g. system:serviceaccount:flux-system:kustomize-controller). The default.
	SANamePolicyName ServiceAccountNamingPolicy = "name"
	// SANamePolicyBot collapses every matched service account to the committer
	// identity, so only humans ever appear as named authors.
	SANamePolicyBot ServiceAccountNamingPolicy = "bot"
)

// AttributionLookup is the read side of the optional audit attribution index. The
// Redis-backed queue.AttributionIndex satisfies it; nil means committer-only.
type AttributionLookup interface {
	LookupAuthor(
		ctx context.Context,
		gvr schema.GroupVersionResource,
		namespace, name string,
		uid k8stypes.UID,
		rv string,
	) (queue.AuthorFact, bool)
}

// AuthorResolver names the commit author for a live watch event from audit facts.
type AuthorResolver interface {
	// ResolveAuthor returns the author UserInfo for a watch event, or ok=false to
	// commit as the configured committer. It may wait up to the grace window for a
	// matching fact; it never blocks indefinitely and never returns an error path —
	// an absent fact is a committer commit, not a failure.
	ResolveAuthor(
		ctx context.Context,
		gvr schema.GroupVersionResource,
		namespace, name string,
		uid k8stypes.UID,
		rv string,
	) (git.UserInfo, bool)
}

type attributionResolver struct {
	lookup   AttributionLookup
	grace    time.Duration
	saPolicy ServiceAccountNamingPolicy
	log      logr.Logger
}

// NewAuthorResolver builds the conservative author resolver over the attribution
// index. grace bounds the per-event wait for a late fact; saPolicy controls how a
// matched service account is named. A zero grace disables waiting (single lookup).
func NewAuthorResolver(
	lookup AttributionLookup,
	grace time.Duration,
	saPolicy ServiceAccountNamingPolicy,
	log logr.Logger,
) AuthorResolver {
	if saPolicy != SANamePolicyBot {
		saPolicy = SANamePolicyName
	}
	return &attributionResolver{lookup: lookup, grace: grace, saPolicy: saPolicy, log: log}
}

func (r *attributionResolver) ResolveAuthor(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	namespace, name string,
	uid k8stypes.UID,
	rv string,
) (git.UserInfo, bool) {
	if r.lookup == nil {
		return git.UserInfo{}, false
	}
	deadline := time.Now().Add(r.grace)
	for {
		if fact, ok := r.lookup.LookupAuthor(ctx, gvr, namespace, name, uid, rv); ok {
			return r.userInfoForFact(fact)
		}
		if !time.Now().Before(deadline) {
			return git.UserInfo{}, false
		}
		if !sleepOrDone(ctx, attributionPollInterval) {
			return git.UserInfo{}, false
		}
	}
}

// userInfoForFact turns a matched fact into a commit author, applying the
// service-account naming policy. A service account under the "bot" policy collapses
// to the committer (ok=false); otherwise it is named by its own username.
func (r *attributionResolver) userInfoForFact(fact queue.AuthorFact) (git.UserInfo, bool) {
	if fact.Author == "" {
		return git.UserInfo{}, false
	}
	if fact.IsServiceAccount && r.saPolicy == SANamePolicyBot {
		return git.UserInfo{}, false
	}
	return git.UserInfo{
		Username:    fact.Author,
		DisplayName: fact.DisplayName,
		Email:       fact.Email,
	}, true
}
