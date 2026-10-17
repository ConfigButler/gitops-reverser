// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
	"errors"
	"testing"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier/fluxsim"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

const (
	testCommit   = "4f2c1ab9d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f"
	olderCommit  = "0000000000000000000000000000000000000001"
	testRepoURL  = "https://github.com/acme/platform"
	testBranch   = "main"
	fluxSystemNS = "flux-system"
)

var stampTime = time.Date(2026, 10, 20, 10, 14, 3, 123456789, time.UTC)

// testBinding is the repository and branch the test appliers are expected to apply.
var testBinding = Binding{RepositoryURL: testRepoURL, Branch: testBranch}

func newSourceClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			kustomizationGVR(): "KustomizationList",
			gitRepositoryGVR(): "GitRepositoryList",
			helmReleaseGVR():   "HelmReleaseList",
			applicationGVR():   "ApplicationList",
		}, objs...)
}

// kustomizationObj builds a Kustomization; a nil sourceRef omits spec.sourceRef.
func kustomizationObj(namespace, name string, sourceRef map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       kindKustomization,
		"metadata":   map[string]any{"namespace": namespace, "name": name},
		"spec":       map[string]any{"path": "./apps"},
	}}
	if sourceRef != nil {
		_ = unstructured.SetNestedMap(obj.Object, sourceRef, "spec", "sourceRef")
	}
	return obj
}

func gitRepoSourceRef(name string) map[string]any {
	return map[string]any{"kind": kindGitRepository, "name": name}
}

// gitRepositoryObj builds a GitRepository; a nil ref omits spec.ref, an empty revision omits the
// artifact.
func gitRepositoryObj(namespace, name, url string, ref map[string]any, revision string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1",
		"kind":       kindGitRepository,
		"metadata":   map[string]any{"namespace": namespace, "name": name},
		"spec":       map[string]any{"url": url},
	}}
	if ref != nil {
		_ = unstructured.SetNestedMap(obj.Object, ref, "spec", "ref")
	}
	if revision != "" {
		_ = unstructured.SetNestedField(obj.Object, revision, "status", "artifact", "revision")
	}
	return obj
}

func branchRef(branch string) map[string]any { return map[string]any{"branch": branch} }

// requestedAt returns an object's reconcile.fluxcd.io/requestedAt, or "".
func requestedAt(t *testing.T, client *dynamicfake.FakeDynamicClient, gvr schema.GroupVersionResource,
	namespace, name string) string {
	t.Helper()
	obj, err := client.Resource(gvr).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return obj.GetAnnotations()[fluxmeta.ReconcileRequestAnnotation]
}

func pinnedFlux(client *dynamicfake.FakeDynamicClient) *Flux {
	flux := NewFlux(client)
	flux.Now = func() time.Time { return stampTime }
	return flux
}

func TestSplitRevision_Formats(t *testing.T) {
	tests := []struct {
		revision  string
		wantNamed string
		wantHash  string
	}{
		{"main@sha1:" + testCommit, "main", testCommit},
		{"feature/branch@sha1:" + testCommit, "feature/branch", testCommit},
		{"refs/heads/main@sha1:" + testCommit, "refs/heads/main", testCommit},
		{"sha1:" + testCommit, "", testCommit},
		{"main@sha256:abc123", "main", "abc123"},
		{"main/" + testCommit, "main", testCommit},
		{"feature/branch/" + testCommit, "feature/branch", testCommit},
		{"HEAD/" + testCommit, "", testCommit},
		{testCommit, "", testCommit},
		{"", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.revision, func(t *testing.T) {
			named, hash := splitRevision(tt.revision)
			assert.Equal(t, tt.wantNamed, named)
			assert.Equal(t, tt.wantHash, hash)
		})
	}
}

func TestFluxRequest_Failures(t *testing.T) {
	tests := []struct {
		name          string
		objs          []runtime.Object
		wantPermanent bool
	}{
		{name: "kustomization not found"},
		{
			name: "git repository not found",
			objs: []runtime.Object{kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform"))},
		},
		{
			name: "source is not a git repository",
			objs: []runtime.Object{kustomizationObj(fluxSystemNS, "checkout",
				map[string]any{"kind": "Bucket", "name": "platform"})},
			wantPermanent: true,
		},
		{
			name:          "no source ref",
			objs:          []runtime.Object{kustomizationObj(fluxSystemNS, "checkout", nil)},
			wantPermanent: true,
		},
		{
			// The review's reproduction of finding 2: Validate said SourceMismatch, and the
			// request stamped the release source anyway.
			name: "source follows another branch",
			objs: []runtime.Object{
				kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
				gitRepositoryObj(
					fluxSystemNS,
					"platform",
					testRepoURL,
					branchRef("release"),
					"release@sha1:"+olderCommit,
				),
			},
			wantPermanent: true,
		},
		{
			name: "source fetches another repository",
			objs: []runtime.Object{
				kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
				gitRepositoryObj(fluxSystemNS, "platform", "https://github.com/acme/other", branchRef(testBranch), ""),
			},
			wantPermanent: true,
		},
		{
			// A parent Kustomization applies this one's manifest with spec.suspend in it: it would
			// resume the Kustomization on its next apply.
			name: "spec.suspend is applied by another manager",
			objs: []runtime.Object{
				fluxsim.OwnSuspend(t, kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")), false,
					git.FluxFieldManager, metav1.ManagedFieldsOperationApply),
				gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), ""),
			},
			wantPermanent: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newSourceClient(tt.objs...)
			_, err := pinnedFlux(client).Request(context.Background(),
				types.NamespacedName{Namespace: fluxSystemNS, Name: "checkout"}, Ask{
					Want: testBinding, Commit: testCommit, Reason: v1alpha3.ApplyReasonForeignCommit, Token: "T1",
				})
			require.Error(t, err)
			assert.Equal(t, tt.wantPermanent, isPermanent(err))
			for _, action := range client.Actions() {
				assert.NotEqual(t, "patch", action.GetVerb(), "a failed request writes nothing")
			}
		})
	}
}

// allowAll and denyOn are AccessCheck fakes.
func allowAll(context.Context, authorizationv1.ResourceAttributes) (bool, error) { return true, nil }

func denyOn(resource string) AccessCheck {
	return func(_ context.Context, attrs authorizationv1.ResourceAttributes) (bool, error) {
		return attrs.Resource != resource, nil
	}
}

// TestFluxValidate_Reasons covers every Configured answer.
func TestFluxValidate_Reasons(t *testing.T) {
	healthyKustomization := kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform"))
	repo := func(url string, ref map[string]any) runtime.Object {
		return gitRepositoryObj(fluxSystemNS, "platform", url, ref, "")
	}
	tests := []struct {
		name        string
		objs        []runtime.Object
		reactor     k8stesting.ReactionFunc
		canI        AccessCheck
		wantReason  string
		wantMessage string
		wantErr     bool
	}{
		{
			name:        "configured",
			objs:        []runtime.Object{healthyKustomization, repo(testRepoURL, branchRef(testBranch))},
			wantReason:  ReasonSucceeded,
			wantMessage: "Kustomization flux-system/checkout applies GitRepository flux-system/platform",
		},
		{
			name:        "kustomization missing",
			wantReason:  ReasonApplierNotFound,
			wantMessage: "Kustomization flux-system/checkout not found",
		},
		{
			name: "kind not served, as a RESTMapper says it",
			reactor: func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, &apimeta.NoKindMatchError{
					GroupKind: schema.GroupKind{Group: kustomizationGVR().Group, Kind: kindKustomization},
				}
			},
			wantReason:  ReasonApplierNotFound,
			wantMessage: "does not serve Kustomization (kustomize.toolkit.fluxcd.io/v1)",
		},
		{
			name: "kind not served, as the API server's bare 404 says it",
			reactor: func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewNotFound(kustomizationGVR().GroupResource(), "")
			},
			wantReason:  ReasonApplierNotFound,
			wantMessage: "is Flux installed there?",
		},
		{
			name:        "git repository missing",
			objs:        []runtime.Object{healthyKustomization},
			wantReason:  ReasonApplierNotFound,
			wantMessage: "GitRepository flux-system/platform not found in the applier's cluster (the sourceRef of",
		},
		{
			name: "source is a bucket",
			objs: []runtime.Object{kustomizationObj(fluxSystemNS, "checkout",
				map[string]any{"kind": "Bucket", "name": "platform"})},
			wantReason:  ReasonUnsupportedSource,
			wantMessage: "applies from a Bucket, not a GitRepository",
		},
		{
			name: "another repository and branch, which only a GitTarget can judge",
			objs: []runtime.Object{
				healthyKustomization,
				repo("https://github.com/acme/other", branchRef("release")),
			},
			wantReason:  ReasonSucceeded,
			wantMessage: "fetches branch release of https://github.com/acme/other",
		},
		{
			name:        "no ref",
			objs:        []runtime.Object{healthyKustomization, repo(testRepoURL, nil)},
			wantReason:  ReasonUnsupportedSource,
			wantMessage: "names no spec.ref.branch",
		},
		{
			name:        "pinned tag",
			objs:        []runtime.Object{healthyKustomization, repo(testRepoURL, map[string]any{"tag": "v1.0.0"})},
			wantReason:  ReasonUnsupportedSource,
			wantMessage: `pins spec.ref.tag "v1.0.0"`,
		},
		{
			name: "pinned commit beside the branch",
			objs: []runtime.Object{healthyKustomization,
				repo(testRepoURL, map[string]any{"branch": testBranch, "commit": testCommit})},
			wantReason:  ReasonUnsupportedSource,
			wantMessage: "pins spec.ref.commit",
		},
		{
			name:        "may not patch the git repository",
			objs:        []runtime.Object{healthyKustomization, repo(testRepoURL, branchRef(testBranch))},
			canI:        denyOn("gitrepositories"),
			wantReason:  ReasonForbidden,
			wantMessage: "may not patch GitRepository flux-system/platform",
		},
		{
			name:        "may not patch the kustomization",
			objs:        []runtime.Object{healthyKustomization, repo(testRepoURL, branchRef(testBranch))},
			canI:        denyOn("kustomizations"),
			wantReason:  ReasonForbidden,
			wantMessage: "may not patch Kustomization flux-system/checkout",
		},
		{
			name: "may not get the kustomization",
			reactor: func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(kustomizationGVR().GroupResource(), "checkout",
					errors.New("denied"))
			},
			wantReason:  ReasonForbidden,
			wantMessage: "may not get Kustomization flux-system/checkout",
		},
		{
			name: "source cluster did not answer",
			reactor: func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewServiceUnavailable("etcd is sulking")
			},
			wantReason: ReasonCheckFailed,
			wantErr:    true,
		},
		{
			name: "access check did not answer",
			objs: []runtime.Object{healthyKustomization, repo(testRepoURL, branchRef(testBranch))},
			canI: func(context.Context, authorizationv1.ResourceAttributes) (bool, error) {
				return false, errors.New("timeout")
			},
			wantReason: ReasonCheckFailed,
			wantErr:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newSourceClient(tt.objs...)
			if tt.reactor != nil {
				client.PrependReactor("get", "kustomizations", tt.reactor)
			}
			flux := pinnedFlux(client)
			flux.CanI = allowAll
			if tt.canI != nil {
				flux.CanI = tt.canI
			}

			got := flux.Validate(context.Background(),
				types.NamespacedName{Namespace: fluxSystemNS, Name: "checkout"})
			assert.Equal(t, tt.wantReason, got.Reason, got.Message)
			assert.Equal(t, tt.wantReason == ReasonSucceeded, got.OK)
			assert.Contains(t, got.Message, tt.wantMessage)
			assert.Equal(t, tt.wantErr, got.Err != nil)
		})
	}
}

// TestFluxValidate_ReportsTheSource: what the Kustomization applies is reported whatever the
// verdict, once the GitRepository could be read, so a GitTarget can judge it.
func TestFluxValidate_ReportsTheSource(t *testing.T) {
	client := newSourceClient(
		kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), ""),
	)
	flux := pinnedFlux(client)
	flux.CanI = denyOn("kustomizations")
	got := flux.Validate(context.Background(), types.NamespacedName{Namespace: fluxSystemNS, Name: "checkout"})
	assert.Equal(t, ReasonForbidden, got.Reason)
	assert.Equal(t, &Source{
		GitRepository: types.NamespacedName{Namespace: fluxSystemNS, Name: "platform"},
		URL:           testRepoURL, Revision: testBranch,
	}, got.Source)
}

// TestBindingMismatch_ComparesRepositoriesAndBranches: a trigger's source and the GitProvider's url
// are compared as repositories, not as strings, and the branch exactly.
func TestBindingMismatch_ComparesRepositoriesAndBranches(t *testing.T) {
	tests := []struct {
		name      string
		sourceURL string
		branch    string
		provider  string
		want      string
	}{
		{"identical", testRepoURL, testBranch, testRepoURL, ""},
		{"trailing .git", testRepoURL + ".git", testBranch, testRepoURL, ""},
		{"scp-style ssh", "git@github.com:acme/platform.git", testBranch, testRepoURL, ""},
		{"ssh url", "ssh://git@github.com/acme/platform", testBranch, "git@github.com:acme/platform.git", ""},
		{"case and trailing slash", "https://GitHub.com/Acme/Platform/", testBranch, testRepoURL, ""},
		{"default port", "https://github.com:443/acme/platform", testBranch, testRepoURL, ""},
		{
			"another org",
			"https://github.com/other/platform",
			testBranch,
			testRepoURL,
			"applies https://github.com/other",
		},
		{"another host", "https://gitlab.com/acme/platform", testBranch, testRepoURL, "applies https://gitlab.com"},
		{"another branch", testRepoURL, "release", testRepoURL, `applies branch "release", not main`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trigger := &v1alpha3.FluxTrigger{ObjectMeta: metav1.ObjectMeta{Name: "apps"}}
			assert.Empty(t, BindingMismatch(trigger, Binding{RepositoryURL: tt.provider, Branch: testBranch}),
				"a trigger that has read nothing yet is not a mismatch")
			trigger.Status.Source = &v1alpha3.FluxTriggerSource{URL: tt.sourceURL, Branch: tt.branch}
			got := BindingMismatch(trigger, Binding{RepositoryURL: tt.provider, Branch: testBranch})
			if tt.want == "" {
				assert.Empty(t, got)
				return
			}
			assert.Contains(t, got, tt.want)
			assert.Contains(t, got, "FluxTrigger apps")
		})
	}
}

// TestSelfSubjectAccessCheck_AsksTheSourceCluster: the default access check creates a
// SelfSubjectAccessReview with the attributes asked about and answers from its status.
func TestSelfSubjectAccessCheck_AsksTheSourceCluster(t *testing.T) {
	client := newSourceClient()
	var asked []string
	client.PrependReactor("create", "selfsubjectaccessreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			review := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
			resource, _, _ := unstructured.NestedString(review.Object, "spec", "resourceAttributes", "resource")
			verb, _, _ := unstructured.NestedString(review.Object, "spec", "resourceAttributes", "verb")
			asked = append(asked, verb+" "+resource)
			_ = unstructured.SetNestedField(review.Object, resource == "gitrepositories", "status", "allowed")
			return true, review, nil
		})
	check := SelfSubjectAccessCheck(client)

	allowed, err := check(context.Background(), authorizationv1.ResourceAttributes{
		Verb: "patch", Group: gitRepositoryGVR().Group, Resource: "gitrepositories", Namespace: fluxSystemNS,
	})
	require.NoError(t, err)
	assert.True(t, allowed)

	allowed, err = check(context.Background(), authorizationv1.ResourceAttributes{
		Verb: "patch", Group: kustomizationGVR().Group, Resource: "kustomizations", Namespace: fluxSystemNS,
	})
	require.NoError(t, err)
	assert.False(t, allowed)
	assert.Equal(t, []string{"patch gitrepositories", "patch kustomizations"}, asked)

	client.PrependReactor("create", "selfsubjectaccessreviews",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewServiceUnavailable("down")
		})
	_, err = check(context.Background(), authorizationv1.ResourceAttributes{Verb: "patch"})
	require.Error(t, err)
}
