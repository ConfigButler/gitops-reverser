// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier/fluxsim"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

const (
	applyRequestsMetric = "gitopsreverser_apply_requests_total"
	targetNS            = "team-a"
)

var testBranchKey = git.BranchKey{RepoNamespace: targetNS, RepoName: "platform", Branch: testBranch}

// Test-only annotations on a GitTarget, which newDispatcherFixture reads to create the trigger the
// GitTarget names: the applier it holds ("namespace/name") and the cluster it is in.
const (
	testKustomizationAnnotation  = "test.gitops-reverser/kustomization"
	testApplicationAnnotation    = "test.gitops-reverser/application"
	testTriggerClusterAnnotation = "test.gitops-reverser/trigger-cluster"
)

// gitTarget builds a GitTarget on testBranchKey, mirroring cluster (the default when empty). A
// kustomizationRef makes it name the FluxTrigger of that Kustomization's name, which holds it in the
// same cluster; nil names no trigger.
func gitTarget(name string, kustomizationRef *fluxmeta.NamespacedObjectReference, cluster string) *v1alpha3.GitTarget {
	target := &v1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Namespace: targetNS, Name: name, UID: uid(name)},
		Spec: v1alpha3.GitTargetSpec{
			GitProviderRef: fluxmeta.LocalObjectReference{Name: testBranchKey.RepoName},
			Branch:         testBranch,
			Path:           name,
		},
	}
	if cluster != "" {
		target.Spec.ClusterProviderRef = &fluxmeta.LocalObjectReference{Name: cluster}
	}
	if kustomizationRef != nil {
		namesKustomization(target, kustomizationRef.Namespace, kustomizationRef.Name)
		target.Annotations[testTriggerClusterAnnotation] = target.SourceCluster()
	}
	return target
}

// namesKustomization makes a GitTarget name the FluxTrigger called name, which holds the
// Kustomization namespace/name (the trigger's own namespace when namespace is empty).
func namesKustomization(target *v1alpha3.GitTarget, namespace, name string) {
	if target.Annotations == nil {
		target.Annotations = map[string]string{}
	}
	target.Spec.ApplyTriggerRef = &v1alpha3.ApplyTriggerReference{Kind: v1alpha3.ApplyTriggerKindFlux, Name: name}
	target.Annotations[testKustomizationAnnotation] = namespace + "/" + name
}

// triggersFor returns the trigger each GitTarget names, as its test annotations describe it.
func triggersFor(objects []client.Object) []client.Object {
	var triggers []client.Object
	seen := map[string]bool{}
	for _, obj := range objects {
		target, ok := obj.(*v1alpha3.GitTarget)
		if !ok || target.Spec.ApplyTriggerRef == nil {
			continue
		}
		key := target.Spec.ApplyTriggerRef.Kind + "/" + target.Spec.ApplyTriggerRef.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		cluster := target.Annotations[testTriggerClusterAnnotation]
		if cluster == "" {
			cluster = v1alpha3.DefaultClusterProviderName
		}
		meta := metav1.ObjectMeta{Namespace: target.Namespace, Name: target.Spec.ApplyTriggerRef.Name}
		provider := &fluxmeta.LocalObjectReference{Name: cluster}
		if app, ok := target.Annotations[testApplicationAnnotation]; ok {
			namespace, name, _ := strings.Cut(app, "/")
			triggers = append(triggers, &v1alpha3.ArgoCDTrigger{ObjectMeta: meta, Spec: v1alpha3.ArgoCDTriggerSpec{
				ClusterProviderRef: provider,
				ApplicationRef:     fluxmeta.NamespacedObjectReference{Namespace: namespace, Name: name},
			}})
			continue
		}
		if ks, ok := target.Annotations[testKustomizationAnnotation]; ok {
			namespace, name, _ := strings.Cut(ks, "/")
			// Its reconciler holds the Kustomization (Ask.Held), unless a test says otherwise.
			triggers = append(triggers, &v1alpha3.FluxTrigger{ObjectMeta: meta, Spec: v1alpha3.FluxTriggerSpec{
				ClusterProviderRef: provider,
				KustomizationRef:   fluxmeta.NamespacedObjectReference{Namespace: namespace, Name: name},
			}, Status: v1alpha3.FluxTriggerStatus{Conditions: []metav1.Condition{{
				Type: TriggerConditionHeld, Status: metav1.ConditionTrue, Reason: "Suspended",
				LastTransitionTime: metav1.Now(),
			}}}})
		}
	}
	return triggers
}

// reapplying makes a GitTarget consent to a Refusal request.
func reapplying(target *v1alpha3.GitTarget) *v1alpha3.GitTarget {
	target.Spec.OnRefusal = v1alpha3.RefusalActionReapply
	return target
}

// clusterProvider admits the GitTargets in the named namespaces.
func clusterProvider(name string, admits ...string) *v1alpha3.ClusterProvider {
	return &v1alpha3.ClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha3.ClusterProviderSpec{
			AccessFrom: &v1alpha3.NamespaceMatcher{Names: admits},
			// The test appliers live in flux-system and argocd, not in the triggers' namespace.
			AllowCrossNamespace: &v1alpha3.ClusterProviderCrossNamespace{Appliers: true},
		},
	}
}

// controlPlane is what every dispatcher test reads besides its GitTargets: the GitProvider of
// testBranchKey, and the ClusterProviders the tests name, admitting targetNS.
func controlPlane() []client.Object {
	return []client.Object{
		&v1alpha3.GitProvider{
			ObjectMeta: metav1.ObjectMeta{Namespace: testBranchKey.RepoNamespace, Name: testBranchKey.RepoName},
			Spec:       v1alpha3.GitProviderSpec{URL: testRepoURL},
		},
		clusterProvider(v1alpha3.DefaultClusterProviderName, targetNS),
		clusterProvider("prod-eu-1", targetNS),
	}
}

// checkoutRef names the Kustomization flux-system/checkout.
func checkoutRef() *fluxmeta.NamespacedObjectReference {
	return &fluxmeta.NamespacedObjectReference{Namespace: fluxSystemNS, Name: "checkout"}
}

func ref(name string) itypes.ResourceReference { return itypes.NewResourceReference(name, targetNS) }

// uid is the uid gitTarget gives the GitTarget of that name.
func uid(name string) types.UID { return types.UID("uid-" + name) }

// kustomizationApplier is a Kustomization applier in a cluster, held by the FluxTrigger in targetNS
// of the Kustomization's name.
func kustomizationApplier(cluster, namespace, name string) git.ApplierRef {
	return git.ApplierRef{
		Trigger: git.TriggerRef{Kind: v1alpha3.ApplyTriggerKindFlux, Namespace: targetNS, Name: name},
		Cluster: cluster, Kind: v1alpha3.ApplierKindKustomization, Namespace: namespace, Name: name,
	}
}

// checkout is the applier most tests ask: flux-system/checkout in the default cluster.
var checkout = kustomizationApplier(v1alpha3.DefaultClusterProviderName, fluxSystemNS, "checkout")

// request is one attempt for an applier to apply testCommit, for the named targets, all asking for
// reason.
func request(applier git.ApplierRef, reason v1alpha3.ApplyReason, names ...string) git.ApplyRequest {
	req := git.ApplyRequest{ID: 1, Branch: testBranchKey, Applier: applier, Reason: reason, Commit: testCommit}
	for _, name := range names {
		req.Targets = append(req.Targets, git.ApplyTarget{Ref: ref(name), UID: uid(name), Reason: reason})
	}
	return req
}

// sourceStub hands out one fake source cluster's client, failing every call while failing is set,
// and remembers which cluster ids it was asked for.
type sourceStub struct {
	client   *dynamicfake.FakeDynamicClient
	mu       sync.Mutex
	failing  bool
	clusters []string
}

func (s *sourceStub) get(_ context.Context, clusterID string) (dynamic.Interface, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clusters = append(s.clusters, clusterID)
	if s.failing {
		return nil, fmt.Errorf("source cluster %s unreachable", clusterID)
	}
	return s.client, nil
}

type dispatcherFixture struct {
	dispatcher *Dispatcher
	control    client.Client
	source     *sourceStub
	sim        *fluxsim.Sim
	recorder   *record.FakeRecorder
	reader     *sdkmetric.ManualReader
}

func newDispatcherFixture(t *testing.T, targets []client.Object, sourceObjs ...runtime.Object) *dispatcherFixture {
	t.Helper()
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha3.AddToScheme(scheme))
	controlClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(append(append(controlPlane(), targets...), triggersFor(targets)...)...).Build()

	source := &sourceStub{client: newSourceClient(sourceObjs...)}
	sim := fluxsim.New(t, source.client)
	for _, obj := range sourceObjs {
		if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == kindGitRepository {
			sim.Fetches(types.NamespacedName{Namespace: u.GetNamespace(), Name: u.GetName()}, "main@sha1:"+testCommit)
		}
	}
	recorder := record.NewFakeRecorder(64)
	d := NewDispatcher(controlClient, source.get, recorder, logr.Discard())
	d.Now = fluxsim.TickingClock(stampTime)
	d.PollInterval, d.UnresolvedPollMax = time.Millisecond, 5*time.Millisecond
	return &dispatcherFixture{
		dispatcher: d, control: controlClient, source: source, sim: sim, recorder: recorder, reader: reader,
	}
}

// attempt carries out one request synchronously, as the dispatcher's goroutine does, up to the
// request: it does not follow the operation a delivered request started.
func (f *dispatcherFixture) attempt(req git.ApplyRequest) git.ApplyResult {
	if req.Token == "" {
		req.Token = requestToken(f.dispatcher.Now())
	}
	res, _ := f.dispatcher.attempt(context.Background(), req)
	return res
}

// carryOut runs the request and follows its operation on a goroutine of its own, as RequestApply
// does, and returns its reports; the channel is closed once it reports nothing more.
func (f *dispatcherFixture) carryOut(ctx context.Context, req git.ApplyRequest) <-chan git.ApplyResult {
	reports := make(chan git.ApplyResult, 64)
	req.Report = func(res git.ApplyResult) { reports <- res }
	go func() {
		defer close(reports)
		f.dispatcher.carryOut(ctx, req)
	}()
	return reports
}

// next is the next report, failing the test when none comes.
func next(t *testing.T, reports <-chan git.ApplyResult) git.ApplyResult {
	t.Helper()
	select {
	case res := <-reports:
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("no report")
		return git.ApplyResult{}
	}
}

// quiet asserts that no report comes for a while.
func quiet(t *testing.T, reports <-chan git.ApplyResult, why string) {
	t.Helper()
	select {
	case res, open := <-reports:
		if open {
			t.Fatalf("%s: reported %s", why, res.Outcome)
		}
		t.Fatalf("%s: stopped reporting", why)
	case <-time.After(50 * time.Millisecond):
	}
}

func (f *dispatcherFixture) count(reason v1alpha3.ApplyReason, outcome string) int64 {
	return f.countKind(applierKindKustomization, reason, outcome)
}

func (f *dispatcherFixture) countKind(kind string, reason v1alpha3.ApplyReason, outcome string) int64 {
	value, _ := telemetry.CollectInt64Sum(f.reader, applyRequestsMetric, map[string]string{
		"applier_kind": kind, "reason": string(reason), "outcome": outcome,
	})
	return value
}

// events drains the recorded Events.
func (f *dispatcherFixture) events() []string {
	var out []string
	for {
		select {
		case event := <-f.recorder.Events:
			out = append(out, event)
		default:
			return out
		}
	}
}

// patches lists the writes to the source cluster, as "resource namespace", with "(suspend=...)"
// for a server-side apply of a Kustomization's spec.suspend.
func (f *dispatcherFixture) patches() []string {
	var out []string
	for _, action := range f.source.client.Actions() {
		if action.GetVerb() != "patch" {
			continue
		}
		write := action.GetResource().Resource + " " + action.GetNamespace()
		if patch := action.(k8stesting.PatchActionImpl); patch.PatchType == types.ApplyPatchType {
			var body map[string]any
			_ = json.Unmarshal(patch.Patch, &body)
			suspend, _, _ := unstructured.NestedBool(body, "spec", "suspend")
			write += fmt.Sprintf(" (suspend=%t)", suspend)
		}
		out = append(out, write)
	}
	return out
}

// behindSource is a suspended Kustomization flux-system/checkout whose GitRepository on testBranch
// holds an older commit than testCommit, so a request fetches before it resumes.
func behindSource() []runtime.Object {
	ks := kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform"))
	_ = unstructured.SetNestedField(ks.Object, true, "spec", "suspend")
	return []runtime.Object{
		ks,
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), "main@sha1:"+olderCommit),
	}
}

// fetchedAndResumed is the writes of a request that fetched and resumed flux-system/checkout.
var fetchedAndResumed = []string{
	"gitrepositories " + fluxSystemNS, "kustomizations " + fluxSystemNS + " (suspend=false)",
}

// TestDispatcher_AsksOnceForEveryTarget: one attempt asks its applier once for all its targets,
// records each target's own reason, and looks the applier up in its source cluster.
func TestDispatcher_AsksOnceForEveryTarget(t *testing.T) {
	f := newDispatcherFixture(t,
		[]client.Object{
			reapplying(gitTarget("a", checkoutRef(), "")),
			gitTarget("b", checkoutRef(), ""),
		},
		behindSource()...,
	)
	req := request(checkout, v1alpha3.ApplyReasonRefusal, "a")
	req.Targets = append(
		req.Targets,
		git.ApplyTarget{Ref: ref("b"), UID: uid("b"), Reason: v1alpha3.ApplyReasonCatchUp},
	)

	res := f.attempt(req)

	assert.Equal(t, git.ApplyResult{ID: 1, Outcome: git.ApplyRequested}, res)
	assert.Equal(t, fetchedAndResumed, f.patches(), "one fetch and one resume for the applier")
	assert.Equal(t, []string{v1alpha3.DefaultClusterProviderName}, f.source.clusters)
	assert.ElementsMatch(t, []string{
		"Normal ApplyRequested asked Kustomization flux-system/checkout to apply 4f2c1ab9 (Refusal)",
		"Normal ApplyRequested asked Kustomization flux-system/checkout to apply 4f2c1ab9 (CatchUp)",
	}, f.events())
	assert.Equal(t, int64(1), f.count(v1alpha3.ApplyReasonRefusal, outcomeSent))
}

// TestDispatcher_LooksTheApplierUpInItsCluster: the applier is the request's, in the source cluster
// it names, under its own namespace and name; here a GitTarget whose kustomizationRef omits the
// namespace and so names one in its own.
func TestDispatcher_LooksTheApplierUpInItsCluster(t *testing.T) {
	own := kustomizationApplier("prod-eu-1", targetNS, "own")
	f := newDispatcherFixture(t,
		[]client.Object{gitTarget("c", &fluxmeta.NamespacedObjectReference{Name: "own"}, "prod-eu-1")},
		kustomizationObj(targetNS, "own", gitRepoSourceRef("own-source")),
		gitRepositoryObj(targetNS, "own-source", testRepoURL, branchRef(testBranch), ""),
	)

	assert.Equal(t, git.ApplyRequested, f.attempt(request(own, v1alpha3.ApplyReasonForeignCommit, "c")).Outcome)
	assert.Equal(t, []string{"prod-eu-1"}, f.source.clusters)
	assert.Equal(t, []string{"gitrepositories " + targetNS, "kustomizations " + targetNS + " (suspend=false)"},
		f.patches())
}

// appliedFrom puts the applier of a GitTarget's trigger in the cluster another ClusterProvider names.
func appliedFrom(target *v1alpha3.GitTarget, cluster string) *v1alpha3.GitTarget {
	target.Annotations[testTriggerClusterAnnotation] = cluster
	return target
}

// TestDispatcher_AsksAnApplierInItsOwnCluster: a GitTarget mirroring prod-eu-1 whose Kustomization
// lives in a management cluster is asked through that cluster's client, never the source's.
func TestDispatcher_AsksAnApplierInItsOwnCluster(t *testing.T) {
	hub := kustomizationApplier("hub", fluxSystemNS, "checkout")
	f := newDispatcherFixture(
		t,
		[]client.Object{
			appliedFrom(gitTarget("a", checkoutRef(), "prod-eu-1"), "hub"),
			clusterProvider("hub", targetNS),
		},
		behindSource()...,
	)

	assert.Equal(t, git.ApplyRequested, f.attempt(request(hub, v1alpha3.ApplyReasonForeignCommit, "a")).Outcome)
	assert.Equal(t, []string{"hub"}, f.source.clusters)

	// The same Kustomization asked as if it lived in the source cluster is not this target's applier.
	f.source.clusters = nil
	sourceSide := kustomizationApplier("prod-eu-1", fluxSystemNS, "checkout")
	assert.Equal(t, git.ApplyDropped, f.attempt(request(sourceSide, v1alpha3.ApplyReasonForeignCommit, "a")).Outcome)
	assert.Empty(t, f.source.clusters)
}

// TestDispatcher_AnApplierClusterThatDoesNotAdmitAsksNothing: the applier's ClusterProvider gates who
// may wield its credential exactly as a source's does, so a GitTarget its accessFrom does not admit
// asks nothing, though its own source admits it.
func TestDispatcher_AnApplierClusterThatDoesNotAdmitAsksNothing(t *testing.T) {
	hub := kustomizationApplier("hub", fluxSystemNS, "checkout")
	f := newDispatcherFixture(t,
		[]client.Object{
			appliedFrom(gitTarget("a", checkoutRef(), "prod-eu-1"), "hub"),
			clusterProvider("hub", "some-other-namespace"),
		},
		behindSource()...,
	)

	assert.Equal(t, git.ApplyDropped, f.attempt(request(hub, v1alpha3.ApplyReasonForeignCommit, "a")).Outcome)
	assert.Empty(t, f.source.clusters, "the hub's client is never even asked for")
}

// TestDispatcher_AFailureIsReportedAndNeverRetried: retrying is the branch worker's decision, on a
// quiet branch. The dispatcher reports the failure, and records ApplyRequestFailed only for the
// first failure of a run.
func TestDispatcher_AFailureIsReportedAndNeverRetried(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{gitTarget("a", checkoutRef(), "")})
	f.source.failing = true

	res := f.attempt(request(checkout, v1alpha3.ApplyReasonForeignCommit, "a"))
	assert.Equal(t, git.ApplyRequestFailed, res.Outcome)
	require.Error(t, res.Err)
	assert.Len(t, f.source.clusters, 1, "one attempt, no retry")
	events := f.events()
	require.Len(t, events, 1)
	assert.True(t, strings.HasPrefix(
		events[0],
		"Warning ApplyRequestFailed could not ask Kustomization flux-system/checkout to apply 4f2c1ab9 (ForeignCommit)",
	), events[0])

	retry := request(checkout, v1alpha3.ApplyReasonForeignCommit, "a")
	retry.PriorFailures = 1
	assert.Equal(t, git.ApplyRequestFailed, f.attempt(retry).Outcome)
	assert.Empty(t, f.events(), "a failure already reported is not reported again")
	assert.Equal(t, int64(2), f.count(v1alpha3.ApplyReasonForeignCommit, outcomeFailed))
}

// TestDispatcher_ASourceMismatchIsRejectedAndNeverStamped is the first review's finding 2:
// the applier was reported SourceMismatch, and a foreign commit stamped the other branch's source.
func TestDispatcher_ASourceMismatchIsRejectedAndNeverStamped(t *testing.T) {
	f := newDispatcherFixture(t,
		[]client.Object{gitTarget("a", checkoutRef(), "")},
		kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef("release"), "release@sha1:"+olderCommit),
	)

	res := f.attempt(request(checkout, v1alpha3.ApplyReasonForeignCommit, "a"))

	assert.Equal(t, git.ApplyRejected, res.Outcome, "only a change to the applier can make it succeed")
	assert.Empty(t, f.patches())
}

// TestDispatcher_ATargetItsClusterProviderDeniesAsksNothing is the first review's finding 1: the
// source client is shared by every GitTarget that declared the cluster, so a GitTarget its
// ClusterProvider does not admit drove the applier on another GitTarget's admission.
func TestDispatcher_ATargetItsClusterProviderDeniesAsksNothing(t *testing.T) {
	restricted := kustomizationApplier("restricted", fluxSystemNS, "checkout")
	f := newDispatcherFixture(t,
		[]client.Object{
			gitTarget("denied", checkoutRef(), "restricted"),
			clusterProvider("restricted", "some-other-namespace"),
		},
		behindSource()...,
	)

	res := f.attempt(request(restricted, v1alpha3.ApplyReasonForeignCommit, "denied"))

	assert.Equal(t, git.ApplyDropped, res.Outcome)
	assert.Empty(t, f.source.clusters, "the denied GitTarget's cluster client is never even asked for")
	assert.Equal(t, int64(1), f.count(v1alpha3.ApplyReasonForeignCommit, outcomeDropped))
}

// TestDispatcher_WithdrawnConsentAsksNothing is the executor's half of the second review's first
// finding. A Refusal whose target withdrew consent used to become a CatchUp sent at once, which
// fetched a behind artifact without the quiet period. Now it asks nothing, even with the artifact
// behind, and the worker decides what is still pending.
func TestDispatcher_WithdrawnConsentAsksNothing(t *testing.T) {
	suspended := reapplying(gitTarget("a", checkoutRef(), ""))
	suspended.Spec.Suspend = true
	f := newDispatcherFixture(t,
		[]client.Object{suspended, gitTarget("b", checkoutRef(), "")},
		behindSource()...,
	)
	req := request(checkout, v1alpha3.ApplyReasonRefusal, "a")
	req.Targets = append(
		req.Targets,
		git.ApplyTarget{Ref: ref("b"), UID: uid("b"), Reason: v1alpha3.ApplyReasonCatchUp},
	)

	res := f.attempt(req)

	assert.Equal(t, git.ApplyDropped, res.Outcome, "never a weaker request in its place")
	assert.Empty(t, f.patches())
}

// TestDispatcher_ATargetThatChangedAsksNothing: the attempt asks only for targets that are still the
// incarnation the worker decided for and still name the applier on its branch. The second review's
// third finding is the first case: a recreated GitTarget executed its predecessor's request.
func TestDispatcher_ATargetThatChangedAsksNothing(t *testing.T) {
	for name, change := range map[string]func(*v1alpha3.GitTarget){
		"it was deleted and recreated": func(g *v1alpha3.GitTarget) { g.UID = "uid-recreated" },
		"it moved to another branch":   func(g *v1alpha3.GitTarget) { g.Spec.Branch = "release" },
		"it names another trigger":     func(g *v1alpha3.GitTarget) { namesKustomization(g, fluxSystemNS, "other") },
		"it names no trigger any more": func(g *v1alpha3.GitTarget) { g.Spec.ApplyTriggerRef = nil },
		"its trigger is gone": func(g *v1alpha3.GitTarget) {
			g.Spec.ApplyTriggerRef.Kind = v1alpha3.ApplyTriggerKindArgoCD // no test annotation creates one
		},
		"its trigger holds the applier in another cluster": func(g *v1alpha3.GitTarget) {
			appliedFrom(g, "prod-eu-1")
		},
	} {
		t.Run(name, func(t *testing.T) {
			target := gitTarget("a", checkoutRef(), "")
			change(target)
			f := newDispatcherFixture(t, []client.Object{target}, behindSource()...)

			res := f.attempt(request(checkout, v1alpha3.ApplyReasonForeignCommit, "a"))

			assert.Equal(t, git.ApplyDropped, res.Outcome)
			assert.Empty(t, f.patches())
		})
	}
}

// TestDispatcher_AStaleDecisionAsksNothing: the worker's decision is checked again just before the
// patch, and an attempt whose branch moved on or became busy reports Stale and resumes nothing.
func TestDispatcher_AStaleDecisionAsksNothing(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{reapplying(gitTarget("a", checkoutRef(), ""))},
		kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), "main@sha1:"+testCommit),
	)
	req := request(checkout, v1alpha3.ApplyReasonRefusal, "a")
	req.Current = func() bool { return false }

	assert.Equal(t, git.ApplyStale, f.attempt(req).Outcome)
	assert.Empty(t, f.patches())
	assert.Empty(t, f.events(), "a stale decision is no failure")
	assert.Equal(t, int64(1), f.count(v1alpha3.ApplyReasonRefusal, outcomeStale))
}

// TestDispatcher_CatchUpAlreadyCurrent: a CatchUp the applier already satisfies (suspended, with
// the commit in its artifact and applied) counts as sent and records nothing, since nothing was
// asked of it.
func TestDispatcher_CatchUpAlreadyCurrent(t *testing.T) {
	ks := kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform"))
	_ = unstructured.SetNestedField(ks.Object, true, "spec", "suspend")
	_ = unstructured.SetNestedField(ks.Object, "main@sha1:"+testCommit, "status", "lastAppliedRevision")
	f := newDispatcherFixture(t,
		[]client.Object{gitTarget("a", checkoutRef(), "")},
		ks, gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), "main@sha1:"+testCommit),
	)

	res := f.attempt(request(checkout, v1alpha3.ApplyReasonCatchUp, "a"))

	assert.Equal(t, git.ApplyCurrent, res.Outcome)
	assert.Empty(t, f.patches())
	assert.Empty(t, f.events())
}

// TestDispatcher_RequestApplyReportsFromItsOwnGoroutine: RequestApply returns at once and the
// attempt reports when it ends; before Start, or after it, an attempt fails at once.
func TestDispatcher_RequestApplyReportsFromItsOwnGoroutine(t *testing.T) {
	f := newDispatcherFixture(
		t,
		[]client.Object{gitTarget("a", checkoutRef(), "")},
		behindSource()...)
	reports := make(chan git.ApplyResult, 1)
	req := request(checkout, v1alpha3.ApplyReasonForeignCommit, "a")
	req.Report = func(res git.ApplyResult) { reports <- res }

	f.dispatcher.RequestApply(req)
	res := <-reports
	assert.Equal(t, git.ApplyRequestFailed, res.Outcome, "not running yet")
	require.ErrorIs(t, res.Err, errNotRunning)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = f.dispatcher.Start(ctx)
	}()
	require.Eventually(t, func() bool {
		f.dispatcher.runMu.RLock()
		defer f.dispatcher.runMu.RUnlock()
		return f.dispatcher.run != nil
	}, 5*time.Second, time.Millisecond)

	f.dispatcher.RequestApply(req)
	assert.Equal(t, git.ApplyRequested, (<-reports).Outcome)
	cancel()
	<-done
}

func TestStrongerApplyReason(t *testing.T) {
	tests := []struct{ older, newer, want v1alpha3.ApplyReason }{
		{v1alpha3.ApplyReasonRefusal, v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonRefusal},
		{v1alpha3.ApplyReasonForeignCommit, v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonForeignCommit},
		{v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonCatchUp},
		{v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonRefusal, v1alpha3.ApplyReasonRefusal},
		{v1alpha3.ApplyReasonRefusal, v1alpha3.ApplyReasonForeignCommit, v1alpha3.ApplyReasonForeignCommit},
		{v1alpha3.ApplyReasonForeignCommit, v1alpha3.ApplyReasonRefusal, v1alpha3.ApplyReasonForeignCommit},
		{"", v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonCatchUp},
		{v1alpha3.ApplyReasonRefusal, v1alpha3.ApplyReasonRepair, v1alpha3.ApplyReasonRepair},
		{v1alpha3.ApplyReasonRepair, v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonRepair},
		{v1alpha3.ApplyReasonRepair, v1alpha3.ApplyReasonForeignCommit, v1alpha3.ApplyReasonForeignCommit},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, git.StrongerApplyReason(tt.older, tt.newer), "%s then %s", tt.older, tt.newer)
	}
}

func TestDescribeCommit(t *testing.T) {
	assert.Equal(t, "4f2c1ab9", DescribeCommit(testCommit))
	assert.Equal(t, "abc", DescribeCommit("abc"))
	assert.Equal(t, "the branch, which the remote does not carry", DescribeCommit(""))
}

// TestDispatcher_ARequestedApplyIsFollowedUntilFluxFinished: the worker's attempt is the apply
// window, so a requested apply reports each step of the suspended lifecycle: Requested once the
// Kustomization is resumed, Running until the reconcile that read the resume's token ended, Settling
// once it is suspended again, and Succeeded only once that suspend's token is handled.
func TestDispatcher_ARequestedApplyIsFollowedUntilFluxFinished(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{gitTarget("a", checkoutRef(), "")}, behindSource()...)
	held := f.sim.HoldNextApply()

	reports := f.carryOut(context.Background(), request(checkout, v1alpha3.ApplyReasonForeignCommit, "a"))
	assert.Equal(t, git.ApplyResult{ID: 1, Outcome: git.ApplyRequested}, next(t, reports))
	<-held
	assert.Equal(t, git.ApplyResult{ID: 1, Outcome: git.ApplyRunning}, next(t, reports))
	quiet(t, reports, "while Flux is still applying")

	f.sim.Release()
	settling := next(t, reports)
	assert.Equal(t, git.ApplySettling, settling.Outcome)
	require.NotEmpty(t, settling.SuspendToken)
	done := next(t, reports)
	assert.Equal(t, git.ApplyResult{
		ID:              1,
		Outcome:         git.ApplySucceeded,
		SuspendToken:    settling.SuspendToken,
		AppliedRevision: "main@sha1:" + testCommit,
	}, done)
	_, open := <-reports
	assert.False(t, open, "and that ends the attempt")
	assert.Equal(t, []string{"flux-system/checkout main@sha1:" + testCommit}, f.sim.Applied())
	assert.True(t, suspended(f.sim.Kustomization(checkoutKey)), "the Kustomization is left suspended")
}

// TestDispatcher_AnApplyPastItsBudgetIsUnresolvedAndStillFollowed is the third review's fourth
// finding: when the wait ran out, the attempt was reported delivered, and the window closed while
// the apply could still run. Now the operation is reported Unresolved, once, the Kustomization is
// suspended at once (which stops what would follow, not the apply running), and the operation is
// followed on until that suspend's token is handled.
func TestDispatcher_AnApplyPastItsBudgetIsUnresolvedAndStillFollowed(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{gitTarget("a", checkoutRef(), "")}, behindSource()...)
	f.dispatcher.UnresolvedAfter = 20 * time.Millisecond
	f.dispatcher.Now = time.Now
	held := f.sim.HoldNextApply()

	reports := f.carryOut(context.Background(), request(checkout, v1alpha3.ApplyReasonForeignCommit, "a"))
	assert.Equal(t, git.ApplyRequested, next(t, reports).Outcome)
	<-held
	assert.Equal(t, git.ApplyRunning, next(t, reports).Outcome)
	assert.Equal(t, git.ApplyUnresolved, next(t, reports).Outcome)
	resuspended := next(t, reports)
	assert.Equal(t, git.ApplyUnresolved, resuspended.Outcome, "Unresolved holds")
	require.NotEmpty(t, resuspended.SuspendToken, "suspended at once")
	assert.True(t, suspended(f.sim.Kustomization(checkoutKey)))
	quiet(t, reports, "an unresolved operation is not closed by time")
	events := f.events()
	require.Len(t, events, 2)
	assert.True(t, strings.HasPrefix(events[1], "Warning ApplyUnresolved Kustomization flux-system/checkout "+
		"has not completed the apply of 4f2c1ab9 (ForeignCommit) within 20ms"), events[1])

	f.sim.Release()
	done := next(t, reports)
	assert.Equal(t, git.ApplySucceeded, done.Outcome, "it resolves when the suspend's token is handled")
	assert.Equal(t, resuspended.SuspendToken, done.SuspendToken)
}

// TestDispatcher_AFailedApplyIsSuspendedAgain: a failed apply is suspended like any other, which is
// what stops Flux's own retryInterval retry; the worker decides the retry, on a quiet branch.
func TestDispatcher_AFailedApplyIsSuspendedAgain(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{gitTarget("a", checkoutRef(), "")}, behindSource()...)
	f.sim.FailApplies("ConfigMap/apps/x dry-run failed")

	reports := f.carryOut(context.Background(), request(checkout, v1alpha3.ApplyReasonForeignCommit, "a"))
	res := next(t, reports)
	for !res.Outcome.Ends() {
		res = next(t, reports)
	}
	assert.Equal(t, git.ApplyFailed, res.Outcome)
	require.ErrorContains(t, res.Err, "ConfigMap/apps/x dry-run failed")
	assert.NotEmpty(t, res.SuspendToken)
	assert.True(t, suspended(f.sim.Kustomization(checkoutKey)), "no retryInterval retry can fire")
}

// TestDispatcher_ADeletedTriggerEndsItsOperation: a trigger being deleted hands its Kustomization
// back itself, so its operation completes as Failed, and nothing more is written to the
// Kustomization from here.
func TestDispatcher_ADeletedTriggerEndsItsOperation(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{gitTarget("a", checkoutRef(), "")}, behindSource()...)
	held := f.sim.HoldNextApply()
	reports := f.carryOut(context.Background(), request(checkout, v1alpha3.ApplyReasonForeignCommit, "a"))
	assert.Equal(t, git.ApplyRequested, next(t, reports).Outcome)
	<-held
	assert.Equal(t, git.ApplyRunning, next(t, reports).Outcome)

	trigger := &v1alpha3.FluxTrigger{}
	require.NoError(t, f.control.Get(context.Background(),
		client.ObjectKey{Namespace: targetNS, Name: "checkout"}, trigger))
	require.NoError(t, f.control.Delete(context.Background(), trigger))
	res := next(t, reports)
	assert.Equal(t, git.ApplyFailed, res.Outcome)
	require.ErrorContains(t, res.Err, "the trigger is being deleted")
	assert.Equal(t, fetchedAndResumed, f.patches(), "never suspended from here")
}

// TestDispatcher_AnAbandonedOperationIsSuspendedBeforeItIsLetGo: once the worker stops following an
// attempt (its applier is no longer named), the dispatcher reports nothing more, but it does not let
// a resumed Kustomization go unguarded (review 4's B): it suspends it at once, and stops only once
// that suspend's token is handled.
func TestDispatcher_AnAbandonedOperationIsSuspendedBeforeItIsLetGo(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{gitTarget("a", checkoutRef(), "")}, behindSource()...)
	held := f.sim.HoldNextApply()
	abandoned := make(chan struct{})
	req := request(checkout, v1alpha3.ApplyReasonForeignCommit, "a")
	req.Token = "T1"
	req.Abandoned = abandoned

	reports := f.carryOut(context.Background(), req)
	assert.Equal(t, git.ApplyRequested, next(t, reports).Outcome)
	<-held
	assert.Equal(t, git.ApplyRunning, next(t, reports).Outcome)
	assert.True(t, f.dispatcher.Following(checkout.Trigger, "T1"))
	close(abandoned)
	eventually(t, func() bool { return suspended(f.sim.Kustomization(checkoutKey)) }, "suspended at once")
	quiet(t, reports, "still waiting for the suspend to be handled, behind the running apply")
	assert.True(t, f.dispatcher.Following(checkout.Trigger, "T1"), "and it is still followed")

	f.sim.Release()
	_, open := <-reports
	assert.False(t, open, "nothing more is reported")
	assert.True(t, tokenHandled(f.sim.Kustomization(checkoutKey), f.sim.Kustomization(checkoutKey).
		GetAnnotations()[fluxmeta.ReconcileRequestAnnotation]), "let go once the suspend is handled")
	assert.False(t, f.dispatcher.Following(checkout.Trigger, "T1"))
}

// TestDispatcher_AStopMidApplySuspendsTheKustomization is review 7's F1: an apply is in flight, and
// the operator is stopped (`helm upgrade`, a node drain). Nothing suspended the Kustomization, so it
// stayed resumed until the next process recovered the operation, and Flux could apply on its own in
// between. Now the stop suspends it once, without waiting for that to be handled: the next process
// recovers a suspend of its own it did not report.
func TestDispatcher_AStopMidApplySuspendsTheKustomization(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{gitTarget("a", checkoutRef(), "")}, behindSource()...)
	held := f.sim.HoldNextApply()
	req := request(checkout, v1alpha3.ApplyReasonForeignCommit, "a")
	req.Token = "T1"
	ctx, stop := context.WithCancel(context.Background())

	reports := f.carryOut(ctx, req)
	assert.Equal(t, git.ApplyRequested, next(t, reports).Outcome)
	<-held
	assert.Equal(t, git.ApplyRunning, next(t, reports).Outcome)
	require.False(t, suspended(f.sim.Kustomization(checkoutKey)), "resumed for the apply")
	stop()
	_, open := <-reports
	assert.False(t, open, "nothing more is reported")

	assert.True(t, suspended(f.sim.Kustomization(checkoutKey)), "suspended as the process stops")
	assert.True(t, heldByUs(f.sim.Kustomization(checkoutKey)), "under GitOps Reverser's field manager")
	f.sim.Release()
}

// TestDispatcher_AnUnheldTriggerIsNotResumed is review 4's F: the trigger's reconciler suspends the
// Kustomization on its own reads (an adoption, an external resume), and a resume between its read
// and its suspend would be suspended under the apply. So a request resumes only a Kustomization its
// trigger reports Held=True, read uncached; any other is a failed attempt the worker retries.
func TestDispatcher_AnUnheldTriggerIsNotResumed(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{gitTarget("a", checkoutRef(), "")}, behindSource()...)
	trigger := &v1alpha3.FluxTrigger{}
	require.NoError(t, f.control.Get(context.Background(), client.ObjectKey{Namespace: targetNS, Name: "checkout"},
		trigger))
	trigger.Status.Conditions[0].Status, trigger.Status.Conditions[0].Reason = metav1.ConditionFalse, "Suspending"
	require.NoError(t, f.control.Update(context.Background(), trigger))

	res := f.attempt(request(checkout, v1alpha3.ApplyReasonForeignCommit, "a"))

	assert.Equal(t, git.ApplyRequestFailed, res.Outcome)
	require.ErrorContains(t, res.Err, "does not hold its Kustomization suspended: it is Held=False, Suspending")
	for _, write := range f.patches() {
		assert.NotContains(t, write, "suspend=false", "nothing is resumed")
	}
}

// TestDispatcher_ATriggerAnotherCameBeforeIsNotResumed is review 5's P2, the dispatcher's half: the
// recheck reads the triggers cached, and can miss one that came first in line since (created in the
// same second, after it by name, but with an apply of its own in flight). The hold check before the resume reads them
// uncached, and fails the attempt.
func TestDispatcher_ATriggerAnotherCameBeforeIsNotResumed(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{gitTarget("a", checkoutRef(), "")}, behindSource()...)
	own := &v1alpha3.FluxTrigger{}
	require.NoError(t, f.control.Get(context.Background(), client.ObjectKey{Namespace: targetNS, Name: "checkout"},
		own))
	other := own.DeepCopy()
	other.Namespace, other.ResourceVersion = "team-z", ""
	other.Spec.KustomizationRef.Namespace = own.Applier().Namespace
	other.Status.Operation = &v1alpha3.ApplyOperation{Phase: v1alpha3.ApplyOperationRunning, Token: "T0"}
	other.Finalizers = []string{v1alpha3.FluxTriggerFinalizer} // the holder whose apply this is
	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha3.AddToScheme(scheme))
	f.dispatcher.APIReader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(own.DeepCopy(), other).Build()

	res := f.attempt(request(checkout, v1alpha3.ApplyReasonForeignCommit, "a"))

	assert.Equal(t, git.ApplyRequestFailed, res.Outcome)
	require.ErrorContains(t, res.Err, "no longer holds its Kustomization")
	require.ErrorContains(t, res.Err, "held by FluxTrigger team-z/checkout, whose apply is in flight")
	for _, write := range f.patches() {
		assert.NotContains(t, write, "suspend=false", "nothing is resumed")
	}
}

// TestDispatcher_ARecoveredAttemptBeforeStartWaitsForIt is review 4's B at startup: a worker
// recovers its trigger's operation once, and one that failed because the dispatcher had not started
// yet left the Kustomization resumed with nobody following it. Now it is followed once Start runs.
func TestDispatcher_ARecoveredAttemptBeforeStartWaitsForIt(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{gitTarget("a", checkoutRef(), "")}, resumedSource(t, "T1")...)
	f.sim.Requeue(checkoutKey)
	reports := make(chan git.ApplyResult, 64)
	req := recoveredRequest(&v1alpha3.ApplyOperation{
		Phase: v1alpha3.ApplyOperationRunning, Reason: v1alpha3.ApplyReasonRefusal, Commit: testCommit, Token: "T1",
	})
	req.Report = func(res git.ApplyResult) { reports <- res }

	f.dispatcher.RequestApply(req)
	quiet(t, reports, "nothing is reported before Start")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = f.dispatcher.Start(ctx)
	}()
	res := next(t, reports)
	for !res.Outcome.Ends() {
		res = next(t, reports)
	}
	assert.Equal(t, git.ApplySucceeded, res.Outcome)
	assert.True(t, suspended(f.sim.Kustomization(checkoutKey)), "suspended again")
	cancel()
	<-done
}

// TestDispatcher_AnAbandonedOperationIsNoLongerFollowed: once the worker stops following an
// attempt (its applier is no longer named), the dispatcher stops reading it and reports nothing.
func TestDispatcher_AnAbandonedOperationIsNoLongerFollowed(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{argoTarget()}, manualApp())
	abandoned := make(chan struct{})
	req := request(argoPlatform, v1alpha3.ApplyReasonForeignCommit, "a")
	req.Abandoned = abandoned

	reports := f.carryOut(context.Background(), req)
	assert.Equal(t, git.ApplyRequested, next(t, reports).Outcome)
	close(abandoned)
	_, open := <-reports
	assert.False(t, open, "nothing more is reported")
}

// TestDispatcher_AnApplierThatIsGoneFailsItsOperation: a deleted applier object runs nothing more,
// so its operation completes as Failed and the worker decides again.
func TestDispatcher_AnApplierThatIsGoneFailsItsOperation(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{gitTarget("a", checkoutRef(), "")}, behindSource()...)
	held := f.sim.HoldNextApply()
	reports := f.carryOut(context.Background(), request(checkout, v1alpha3.ApplyReasonForeignCommit, "a"))
	assert.Equal(t, git.ApplyRequested, next(t, reports).Outcome)
	<-held
	assert.Equal(t, git.ApplyRunning, next(t, reports).Outcome)

	require.NoError(t, f.source.client.Resource(kustomizationGVR()).Namespace(fluxSystemNS).
		Delete(context.Background(), "checkout", metav1.DeleteOptions{}))
	res := next(t, reports)
	assert.Equal(t, git.ApplyFailed, res.Outcome)
	require.ErrorContains(t, res.Err, "the applier is gone")
}

// argoTarget is the GitTarget "a", which names the ArgoCDTrigger "platform", holding the
// Application argocd/platform.
func argoTarget() *v1alpha3.GitTarget {
	target := gitTarget("a", nil, "")
	target.Annotations = map[string]string{testApplicationAnnotation: argoNS + "/platform"}
	target.Spec.ApplyTriggerRef = &v1alpha3.ApplyTriggerReference{
		Kind: v1alpha3.ApplyTriggerKindArgoCD,
		Name: "platform",
	}
	return target
}

// argoPlatform is the applier argoTarget's trigger holds.
var argoPlatform = git.ApplierRef{
	Trigger: git.TriggerRef{Kind: v1alpha3.ApplyTriggerKindArgoCD, Namespace: targetNS, Name: "platform"},
	Cluster: v1alpha3.DefaultClusterProviderName, Kind: v1alpha3.ApplierKindApplication,
	Namespace: argoNS, Name: "platform",
}

func (f *dispatcherFixture) updates() []string {
	var out []string
	for _, action := range f.source.client.Actions() {
		if action.GetVerb() == "update" {
			out = append(out, action.GetResource().Resource+" "+action.GetNamespace())
		}
	}
	return out
}

// TestDispatcher_AsksAnApplicationToSync: an Application applier is asked through the Argo CD
// client, with the commit pinned, counted and recorded under its own kind.
func TestDispatcher_AsksAnApplicationToSync(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{reapplying(argoTarget())}, manualApp())

	req := request(argoPlatform, v1alpha3.ApplyReasonRefusal, "a")
	req.Token = "T1"
	recorded := false
	req.Record = func(context.Context) error {
		assert.Empty(t, f.updates(), "recorded before the sync is set")
		recorded = true
		return nil
	}
	res := f.attempt(req)

	assert.Equal(t, git.ApplyResult{ID: 1, Outcome: git.ApplyRequested}, res)
	assert.True(t, recorded)
	assert.Equal(t, []string{"applications " + argoNS}, f.updates())
	assert.Empty(t, f.patches(), "no Flux object is stamped")
	app, err := f.source.client.Resource(applicationGVR()).Namespace(argoNS).
		Get(context.Background(), "platform", metav1.GetOptions{})
	require.NoError(t, err)
	revision, _, _ := unstructured.NestedString(app.Object, "operation", "sync", "revision")
	assert.Equal(t, testCommit, revision)
	operation, _, _ := unstructured.NestedMap(app.Object, "operation")
	assert.Equal(t, "T1", operationToken(operation), "the operation carries the request's token")
	assert.Equal(t, []string{
		"Normal ApplyRequested asked Application argocd/platform to apply 4f2c1ab9 (Refusal)",
	}, f.events())
	assert.Equal(t, int64(1), f.countKind(applierKindApplication, v1alpha3.ApplyReasonRefusal, outcomeSent))
	assert.Zero(t, f.count(v1alpha3.ApplyReasonRefusal, outcomeSent))
}

// TestDispatcher_AnApplicationWithAutomatedSyncIsRejectedAndNeverSynced: the standard is full
// control, so an Application that syncs on its own is never asked to, whatever its verdict says.
func TestDispatcher_AnApplicationWithAutomatedSyncIsRejectedAndNeverSynced(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{argoTarget()},
		applicationObj(testRepoURL, testBranch, map[string]any{"automated": map[string]any{"prune": true}}))

	res := f.attempt(request(argoPlatform, v1alpha3.ApplyReasonForeignCommit, "a"))

	assert.Equal(t, git.ApplyRejected, res.Outcome)
	require.ErrorContains(t, res.Err, "has automated sync on")
	assert.Empty(t, f.updates())
	assert.Equal(t, int64(1), f.countKind(applierKindApplication, v1alpha3.ApplyReasonForeignCommit, outcomeFailed))
}

// TestDispatcher_AnApplicationIsNotTheKustomizationOfItsName: the kind is part of which applier a
// GitTarget names, so a request for a Kustomization of the same name asks nothing for it.
func TestDispatcher_AnApplicationIsNotTheKustomizationOfItsName(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{argoTarget()}, manualApp())
	sameName := kustomizationApplier(v1alpha3.DefaultClusterProviderName, argoNS, "platform")

	assert.Equal(t, git.ApplyDropped, f.attempt(request(sameName, v1alpha3.ApplyReasonForeignCommit, "a")).Outcome)
	assert.Empty(t, f.updates())
}

// TestDispatcher_AnApplicationAttemptWaitsWithinItsWindow: an Argo CD attempt may wait out an
// operation already running, so it is bounded by the operation wait on top of the attempt timeout.
func TestDispatcher_AnApplicationAttemptWaitsWithinItsWindow(t *testing.T) {
	running := manualApp()
	_ = unstructured.SetNestedMap(running.Object, theirOperation(), "operation")
	f := newDispatcherFixture(t, []client.Object{argoTarget()}, running)
	f.dispatcher.AttemptTimeout = 30 * time.Millisecond
	f.dispatcher.OperationWait = 100 * time.Millisecond
	f.dispatcher.Now = time.Now

	res := f.attempt(request(argoPlatform, v1alpha3.ApplyReasonForeignCommit, "a"))

	assert.Equal(t, git.ApplyRequestFailed, res.Outcome)
	require.ErrorContains(t, res.Err, "has an operation in progress")
	require.NotErrorIs(t, res.Err, context.DeadlineExceeded,
		"the wait ends by the operation wait, not by the shorter attempt timeout")
	assert.Empty(t, f.updates())
}

// argoOperationState sets the Application's operation state to the operation it carries, at phase,
// and clears the operation, as Argo CD's controller does once it completes one.
func argoOperationState(t *testing.T, f *dispatcherFixture, phase string) {
	t.Helper()
	app := readApp(t, f.source.client)
	operation, _, _ := unstructured.NestedMap(app.Object, "operation")
	if operation == nil {
		operation, _, _ = unstructured.NestedMap(app.Object, "status", "operationState", "operation")
	}
	_ = unstructured.SetNestedMap(app.Object, map[string]any{
		"operation": operation, "phase": phase, "message": "a hook failed",
	}, "status", "operationState")
	if phase != "Running" {
		unstructured.RemoveNestedField(app.Object, "operation")
	}
	writeApp(t, f.source.client, app)
}

// TestDispatcher_AnApplicationsSyncIsFollowedToItsOutcome: an Argo CD sync reports Running once the
// controller started it, and its outcome once it completed; a failed one records ApplyFailed for the
// first failure of a run, so the worker's retry of it is not reported twice.
func TestDispatcher_AnApplicationsSyncIsFollowedToItsOutcome(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{argoTarget()}, manualApp())

	reports := f.carryOut(context.Background(), request(argoPlatform, v1alpha3.ApplyReasonForeignCommit, "a"))
	assert.Equal(t, git.ApplyRequested, next(t, reports).Outcome)
	argoOperationState(t, f, "Running")
	assert.Equal(t, git.ApplyRunning, next(t, reports).Outcome)
	argoOperationState(t, f, "Failed")
	res := next(t, reports)
	assert.Equal(t, git.ApplyFailed, res.Outcome)
	require.ErrorContains(t, res.Err, "the sync ended Failed: a hook failed")
	events := f.events()
	require.Len(t, events, 2)
	assert.True(t, strings.HasPrefix(events[1], "Warning ApplyFailed Application argocd/platform did not "+
		"complete the apply of 4f2c1ab9 (ForeignCommit)"), events[1])

	retry := request(argoPlatform, v1alpha3.ApplyReasonForeignCommit, "a")
	retry.PriorFailures = 1
	reports = f.carryOut(context.Background(), retry)
	assert.Equal(t, git.ApplyRequested, next(t, reports).Outcome)
	argoOperationState(t, f, "Error")
	assert.Equal(t, git.ApplyFailed, next(t, reports).Outcome)
	assert.Len(t, f.events(), 1, "only the ApplyRequested: a failure already reported is not reported again")
}

// TestDispatcher_AnUnansweredApplicationUpdateIsFollowed is review 5's first finding, end to end:
// an update the API server made but whose answer timed out is no RequestFailed (which would have
// the worker withdraw its record and close the apply window while Argo CD syncs). It is reported
// Requested, and followed until the sync completes. One that was never made is Failed once a write
// moved the Application on from the version it was made from, so the worker asks again.
func TestDispatcher_AnUnansweredApplicationUpdateIsFollowed(t *testing.T) {
	for name, landed := range map[string]bool{"made": true, "never made": false} {
		t.Run(name, func(t *testing.T) {
			f := newDispatcherFixture(t, []client.Object{argoTarget()}, manualApp())
			var answered atomic.Bool
			f.source.client.PrependReactor("update", "applications",
				func(action k8stesting.Action) (bool, runtime.Object, error) {
					if answered.Swap(true) {
						return false, nil, nil
					}
					if landed {
						update, _ := action.(k8stesting.UpdateActionImpl)
						tracker := f.source.client.Tracker()
						require.NoError(t, tracker.Update(applicationGVR(), update.GetObject(), argoNS))
					}
					return true, nil, context.DeadlineExceeded
				})

			reports := f.carryOut(context.Background(), request(argoPlatform, v1alpha3.ApplyReasonForeignCommit, "a"))
			assert.Equal(t, git.ApplyResult{ID: 1, Outcome: git.ApplyRequested}, next(t, reports))
			if !landed {
				res := next(t, reports)
				assert.Equal(t, git.ApplyFailed, res.Outcome)
				require.ErrorContains(t, res.Err, "was never made")
				assert.Contains(t, readApp(t, f.source.client).GetAnnotations(), argoRefreshAnnotation,
					"settled by a write that moved the Application on")
				return
			}
			argoOperationState(t, f, "Running")
			assert.Equal(t, git.ApplyRunning, next(t, reports).Outcome)
			argoOperationState(t, f, "Succeeded")
			assert.Equal(t, git.ApplySucceeded, next(t, reports).Outcome)
		})
	}
}

// TestDispatcher_AnApplyOfAnotherRevisionIsCounted is the residual race: somebody fetched the
// source between the recheck and the reconcile, so Flux applied another revision. It is still an
// operation that succeeded, and it is logged and counted.
func TestDispatcher_AnApplyOfAnotherRevisionIsCounted(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{gitTarget("a", checkoutRef(), "")})
	req := request(checkout, v1alpha3.ApplyReasonForeignCommit, "a")
	op := &operation{log: logr.Discard()}

	res := f.dispatcher.completed(context.Background(), req, op, Progress{
		Outcome: git.ApplySucceeded, AppliedRevision: "main@sha1:" + olderCommit,
		Message: "Flux applied main@sha1:" + olderCommit + ", not commit " + testCommit,
	}, time.Second)

	assert.Equal(t, git.ApplySucceeded, res.Outcome)
	assert.Contains(t, res.Message, "not commit")
	mismatches, _ := telemetry.CollectInt64Sum(f.reader, "gitopsreverser_apply_revision_mismatches_total",
		map[string]string{"applier_kind": applierKindKustomization})
	assert.Equal(t, int64(1), mismatches)
}

// TestApplyVerdict_ReadsTheReconcileThatHandledTheToken pins the outcome table.
func TestApplyVerdict_ReadsTheReconcileThatHandledTheToken(t *testing.T) {
	ks := func(status metav1.ConditionStatus, reason, applied string) *unstructured.Unstructured {
		obj := kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform"))
		fluxsim.SetReady(obj, status, reason, "message")
		_ = unstructured.SetNestedField(obj.Object, applied, "status", "lastAppliedRevision")
		return obj
	}
	commit := "main@sha1:" + testCommit
	assert.Equal(t, Progress{Outcome: git.ApplySucceeded, AppliedRevision: commit},
		applyVerdict(ks(metav1.ConditionTrue, "ReconciliationSucceeded", commit), testCommit))
	other := applyVerdict(ks(metav1.ConditionTrue, "ReconciliationSucceeded", "main@sha1:"+olderCommit), testCommit)
	assert.Equal(t, git.ApplySucceeded, other.Outcome)
	assert.Contains(t, other.Message, "not commit "+testCommit)
	dependency := applyVerdict(ks(metav1.ConditionFalse, fluxmeta.DependencyNotReadyReason, commit), testCommit)
	assert.Equal(t, git.ApplyFailed, dependency.Outcome)
	assert.Contains(t, dependency.Message, "a dependency is not ready")
	failed := applyVerdict(ks(metav1.ConditionFalse, "ReconciliationFailed", commit), testCommit)
	assert.Equal(t, git.ApplyFailed, failed.Outcome)
	assert.Contains(t, failed.Message, "ReconciliationFailed")
	none := applyVerdict(kustomizationObj(fluxSystemNS, "checkout", nil), testCommit)
	assert.Equal(t, git.ApplyFailed, none.Outcome)
}

// recoveredRequest follows the recorded operation instead of asking.
func recoveredRequest(recorded *v1alpha3.ApplyOperation) git.ApplyRequest {
	req := request(checkout, recorded.Reason, "a")
	req.Commit, req.Token, req.Recovered = recorded.Commit, recorded.Token, recorded
	return req
}

// followToEnd runs RequestApply and returns the report that ends the attempt.
func (f *dispatcherFixture) followToEnd(t *testing.T, req git.ApplyRequest) git.ApplyResult {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = f.dispatcher.Start(ctx) }()
	require.Eventually(t, func() bool {
		f.dispatcher.runMu.RLock()
		defer f.dispatcher.runMu.RUnlock()
		return f.dispatcher.run != nil
	}, 5*time.Second, time.Millisecond)
	reports := make(chan git.ApplyResult, 64)
	req.Report = func(res git.ApplyResult) { reports <- res }
	f.dispatcher.RequestApply(req)
	res := next(t, reports)
	for !res.Outcome.Ends() {
		res = next(t, reports)
	}
	return res
}

// resumedSource is flux-system/checkout resumed with token, as a process that stopped mid-apply
// leaves it, with its GitRepository at testCommit.
func resumedSource(t *testing.T, token string) []runtime.Object {
	ks := kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform"))
	ks = fluxsim.OwnSuspend(t, ks, false, SuspendFieldManager, metav1.ManagedFieldsOperationApply)
	ks.SetAnnotations(map[string]string{fluxmeta.ReconcileRequestAnnotation: token})
	ks.SetGeneration(2)
	return []runtime.Object{
		ks, gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), "main@sha1:"+testCommit),
	}
}

// TestDispatcher_ARecoveredOperationIsFollowedAndSuspended is step 5: a process stopped while the
// Kustomization was resumed for its operation. The next one follows that operation by its recorded
// token, asks for nothing, and suspends the Kustomization again before the operation completes.
func TestDispatcher_ARecoveredOperationIsFollowedAndSuspended(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{gitTarget("a", checkoutRef(), "")}, resumedSource(t, "T1")...)
	f.sim.Requeue(checkoutKey) // the reconcile the resume enqueued, still to run

	res := f.followToEnd(t, recoveredRequest(&v1alpha3.ApplyOperation{
		Phase: v1alpha3.ApplyOperationRunning, Reason: v1alpha3.ApplyReasonRefusal, Commit: testCommit, Token: "T1",
	}))

	assert.Equal(t, git.ApplySucceeded, res.Outcome)
	assert.NotEmpty(t, res.SuspendToken)
	assert.True(t, suspended(f.sim.Kustomization(checkoutKey)), "suspended again")
	for _, write := range f.patches() {
		assert.NotContains(t, write, "suspend=false", "nothing is resumed for a recovered operation")
		assert.NotContains(t, write, "gitrepositories", "and nothing is fetched")
	}
	assert.Empty(t, f.events(), "and it records no ApplyRequested")
}

// TestDispatcher_ARecoveredOperationThatNeverResumedFails: the record is written before the resume,
// so a process can stop between the two. Its token is nowhere on the Kustomization; no outcome can
// be read, so it ends Failed (after a suspend) and the worker decides again.
func TestDispatcher_ARecoveredOperationThatNeverResumedFails(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{gitTarget("a", checkoutRef(), "")}, behindSource()...)

	res := f.followToEnd(t, recoveredRequest(&v1alpha3.ApplyOperation{
		Phase: v1alpha3.ApplyOperationRequested, Reason: v1alpha3.ApplyReasonCatchUp, Commit: testCommit, Token: "T1",
	}))

	assert.Equal(t, git.ApplyFailed, res.Outcome)
	require.ErrorContains(t, res.Err, "no sign of its resume")
	assert.Empty(t, f.sim.Applied())
	assert.True(t, suspended(f.sim.Kustomization(checkoutKey)))
}

// TestDispatcher_AnApplyStartedElsewhereIsFollowedByItsSuspend: the trigger's reconciler recorded a
// `flux resume` it suspended again, with the suspend token and no request token; following it ends
// once that suspend is handled.
func TestDispatcher_AnApplyStartedElsewhereIsFollowedByItsSuspend(t *testing.T) {
	objs := resumedSource(t, "F1")
	_ = unstructured.SetNestedField(objs[0].(*unstructured.Unstructured).Object, true, "spec", "suspend")
	f := newDispatcherFixture(t, []client.Object{gitTarget("a", checkoutRef(), "")}, objs...)
	f.sim.Requeue(checkoutKey)

	res := f.followToEnd(t, recoveredRequest(&v1alpha3.ApplyOperation{
		Phase: v1alpha3.ApplyOperationUnresolved, SuspendToken: "F1",
	}))

	assert.Equal(t, git.ApplyFailed, res.Outcome,
		"the Kustomization reports no Ready: nothing of that apply can be read")
	assert.Equal(t, "F1", res.SuspendToken)
	for _, write := range f.patches() {
		assert.NotContains(t, write, "kustomizations", "the suspend was the reconciler's; nothing is written")
	}
}
