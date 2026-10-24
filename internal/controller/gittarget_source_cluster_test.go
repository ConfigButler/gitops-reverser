// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

func TestNamespaceToGitTargets(t *testing.T) {
	gt := func(name, ns string) *configbutleraiv1alpha3.GitTarget {
		return &configbutleraiv1alpha3.GitTarget{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	}
	cl := fake.NewClientBuilder().WithScheme(scScheme(t)).
		WithObjects(gt("a", "team-a"), gt("b", "team-a"), gt("c", "team-b")).Build()
	r := &GitTargetReconciler{Client: cl}

	reqs := r.namespaceToGitTargets(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}})
	// A label change on team-a re-enqueues exactly the GitTargets IN team-a, so a selector-based
	// authorization revocation converges without waiting for the periodic reconcile.
	names := []string{}
	for _, req := range reqs {
		assert.Equal(t, "team-a", req.Namespace)
		names = append(names, req.Name)
	}
	assert.ElementsMatch(t, []string{"a", "b"}, names)
}

func TestKubeProviderReadyOrSpecChanged(t *testing.T) {
	p := kubeProviderReadyOrSpecChanged()
	cp := func(gen int64, ready metav1.ConditionStatus) *configbutleraiv1alpha3.KubeProvider {
		c := &configbutleraiv1alpha3.KubeProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: "team-a", Generation: gen},
		}
		if ready != "" {
			c.Status.Conditions = []metav1.Condition{{Type: ConditionTypeReady, Status: ready}}
		}
		return c
	}

	assert.True(t, p.Update(event.UpdateEvent{
		ObjectOld: cp(1, metav1.ConditionUnknown), ObjectNew: cp(1, metav1.ConditionTrue),
	}), "a Ready flip (status-only, same generation) must fire")
	assert.False(t, p.Update(event.UpdateEvent{
		ObjectOld: cp(1, metav1.ConditionTrue), ObjectNew: cp(1, metav1.ConditionTrue),
	}), "no spec/Ready change must not fire")
	assert.True(t, p.Update(event.UpdateEvent{
		ObjectOld: cp(1, metav1.ConditionTrue), ObjectNew: cp(2, metav1.ConditionTrue),
	}), "a spec (generation) change must fire")
	assert.True(t, p.Create(event.CreateEvent{}))
	assert.True(t, p.Delete(event.DeleteEvent{}))
}

const scValidKubeConfig = `apiVersion: v1
kind: Config
clusters:
- {name: c, cluster: {server: https://192.0.2.1:6443, certificate-authority-data: dGVzdA==}}
contexts:
- {name: c, context: {cluster: c, user: u}}
current-context: c
users:
- {name: u, user: {token: t}}
`

const scExecKubeConfig = `apiVersion: v1
kind: Config
clusters:
- {name: c, cluster: {server: https://192.0.2.1:6443}}
contexts:
- {name: c, context: {cluster: c, user: u}}
current-context: c
users:
- name: u
  user:
    exec: {apiVersion: client.authentication.k8s.io/v1, command: /bin/echo, interactiveMode: Never}
`

const scInsecureKubeConfig = `apiVersion: v1
kind: Config
clusters:
- {name: c, cluster: {server: https://192.0.2.1:6443, insecure-skip-tls-verify: true}}
contexts:
- {name: c, context: {cluster: c, user: u}}
current-context: c
users:
- {name: u, user: {token: t}}
`

func scScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

func TestCheckSourceAuthorization(t *testing.T) {
	// The provider lives in "platform", so team-a reaches it only through spec.allowedGitTargets.
	provider := func(grant configbutleraiv1alpha3.SourceGrant) *configbutleraiv1alpha3.KubeProvider {
		p := remoteKubeProvider("kc", "")
		p.Namespace = "platform"
		grant.SourceNamespaces = []configbutleraiv1alpha3.ScopeNamespace{configbutleraiv1alpha3.NamespaceVariable}
		p.Spec.AllowedGitTargets = []configbutleraiv1alpha3.SourceGrant{grant}
		return p
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", Labels: map[string]string{"tier": "trusted"}}}
	target := func(providerName string) *configbutleraiv1alpha3.GitTarget {
		gt := &configbutleraiv1alpha3.GitTarget{ObjectMeta: metav1.ObjectMeta{Name: "gt", Namespace: "team-a"}}
		if providerName != "" {
			gt.Spec.KubeProviderRef = meta.NamespacedObjectReference{Namespace: "platform", Name: providerName}
		}
		return gt
	}

	tests := []struct {
		name           string
		objects        []client.Object
		providerRef    string
		wantAuthorized bool
		wantReason     string
	}{
		{
			name:           "provider not found -> hard gate (NotReady, no mirroring)",
			objects:        []client.Object{ns},
			providerRef:    "absent",
			wantAuthorized: false,
			wantReason:     GitTargetReasonKubeProviderNotFound,
		},
		{
			// There is no "default" provider to fall back to: an unset reference names nothing.
			name:           "no kubeProviderRef -> hard gate, no fallback",
			objects:        []client.Object{ns},
			providerRef:    "",
			wantAuthorized: false,
			wantReason:     GitTargetReasonKubeProviderNotFound,
		},
		{
			name: "provider allows the namespace by name",
			objects: []client.Object{
				provider(
					configbutleraiv1alpha3.SourceGrant{
						From:  configbutleraiv1alpha3.GrantFromNames,
						Names: grantNames("team-a"),
					},
				),
				ns,
			},
			providerRef: "prod-eu-1", wantAuthorized: true,
		},
		{
			name: "provider does not allow the namespace -> refused",
			objects: []client.Object{
				provider(
					configbutleraiv1alpha3.SourceGrant{
						From:  configbutleraiv1alpha3.GrantFromNames,
						Names: grantNames("team-b"),
					},
				),
				ns,
			},
			providerRef: "prod-eu-1", wantAuthorized: false, wantReason: GitTargetReasonNamespaceNotAuthorized,
		},
		{
			name: "provider allows the namespace by label selector",
			objects: []client.Object{
				provider(configbutleraiv1alpha3.SourceGrant{
					From:     configbutleraiv1alpha3.GrantFromSelector,
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "trusted"}},
				}), ns,
			},
			providerRef: "prod-eu-1", wantAuthorized: true,
		},
		{
			// A selector the API accepted but that cannot compile must FAIL CLOSED. Treating an
			// unevaluatable policy as "allow" would hand a namespace access it was never granted.
			name: "invalid selector -> refused, not allowed",
			objects: []client.Object{
				provider(configbutleraiv1alpha3.SourceGrant{
					From: configbutleraiv1alpha3.GrantFromSelector,
					Selector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{Key: "tier", Operator: "BogusOperator", Values: []string{"trusted"}},
						},
					},
				}), ns,
			},
			providerRef: "prod-eu-1", wantAuthorized: false, wantReason: GitTargetReasonNamespaceNotAuthorized,
		},
		{
			// A missing Namespace object is not an error: the policy is still evaluated, just with
			// no labels. A name-based allow still works; a selector-only policy then denies.
			name: "namespace object absent -> evaluated with no labels, name allow still holds",
			objects: []client.Object{
				provider(
					configbutleraiv1alpha3.SourceGrant{
						From:  configbutleraiv1alpha3.GrantFromNames,
						Names: grantNames("team-a"),
					},
				),
			},
			providerRef: "prod-eu-1", wantAuthorized: true,
		},
		{
			name: "namespace object absent -> selector-only policy denies (no labels to match)",
			objects: []client.Object{
				provider(configbutleraiv1alpha3.SourceGrant{
					From:     configbutleraiv1alpha3.GrantFromSelector,
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "trusted"}},
				}),
			},
			providerRef: "prod-eu-1", wantAuthorized: false, wantReason: GitTargetReasonNamespaceNotAuthorized,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cl := fake.NewClientBuilder().WithScheme(scScheme(t)).WithObjects(tc.objects...).Build()
			r := &GitTargetReconciler{Client: cl}
			ok, reason, msg, err := r.checkSourceAuthorization(context.Background(), target(tc.providerRef))
			require.NoError(t, err)
			assert.Equal(t, tc.wantAuthorized, ok)
			if !tc.wantAuthorized {
				assert.Equal(t, tc.wantReason, reason)
				assert.NotEmpty(t, msg)
			}
		})
	}
}

// TestCheckSourceAuthorization_OwnNamespaceAndAuditRoute covers the two admission rules a
// namespaced KubeProvider adds: its OWN namespace is admitted only by a grant, such as from: Same,
// with the reference's namespace omitted; and a provider whose audit route another namespace holds
// is denied to every consumer, its own namespace included.
func TestCheckSourceAuthorization_OwnNamespaceAndAuditRoute(t *testing.T) {
	own := inClusterKubeProvider("local")
	own.Spec.AllowedGitTargets = []configbutleraiv1alpha3.SourceGrant{{From: configbutleraiv1alpha3.GrantFromSame,
		SourceNamespaces: []configbutleraiv1alpha3.ScopeNamespace{configbutleraiv1alpha3.NamespaceVariable}}}
	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "gt", Namespace: kpNamespace},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			KubeProviderRef: meta.NamespacedObjectReference{Name: "local"},
		},
	}

	t.Run("own namespace is admitted by from: Same", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scScheme(t)).WithObjects(own).Build()
		ok, _, _, err := (&GitTargetReconciler{Client: cl}).checkSourceAuthorization(context.Background(), target)
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("own namespace is refused with no grant", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scScheme(t)).WithObjects(inClusterKubeProvider("local")).Build()
		ok, reason, msg, err := (&GitTargetReconciler{Client: cl}).checkSourceAuthorization(
			context.Background(), target)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Equal(t, GitTargetReasonNamespaceNotAuthorized, reason)
		assert.Contains(t, msg, "admits no namespace in spec.allowedGitTargets")
	})

	t.Run("a route held by another namespace is refused", func(t *testing.T) {
		// "team-b.edge" names team-b, which holds it while a provider there carries it, however
		// much older the team-a provider spelling it out is.
		squatter := own.DeepCopy()
		squatter.CreationTimestamp = metav1.NewTime(metav1.Now().Add(-time.Hour))
		squatter.Spec.Attribution = &configbutleraiv1alpha3.KubeProviderAttribution{AuditRoute: "team-b.edge"}
		holder := inClusterKubeProvider("edge")
		holder.Namespace = "team-b"
		cl := fake.NewClientBuilder().WithScheme(scScheme(t)).WithObjects(squatter, holder).Build()

		ok, reason, msg, err := (&GitTargetReconciler{Client: cl}).checkSourceAuthorization(
			context.Background(),
			target,
		)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Equal(t, authz.ReasonAuditRouteConflict, reason)
		assert.Contains(t, msg, "team-b.edge")
	})
}

// TestCheckSourceAuthorization_ReadErrorsRequeue pins the fail-closed contract for TRANSIENT
// failures. A read error is not a verdict: the gate must return an error so the reconcile requeues,
// rather than returning authorized=false (which would look like a policy denial and flap the
// GitTarget's status) or authorized=true (which would open a watch on an unevaluated policy).
func TestCheckSourceAuthorization_ReadErrorsRequeue(t *testing.T) {
	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "gt", Namespace: "team-a"},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			KubeProviderRef: meta.NamespacedObjectReference{Namespace: "platform", Name: "prod-eu-1"},
		},
	}
	provider := remoteKubeProvider("kc", "")
	provider.Namespace = "platform"
	provider.Spec.AllowedGitTargets = mirrorGrants("team-a")

	tests := []struct {
		name    string
		failOn  func(client.Object) bool
		wantErr string
	}{
		{
			name:    "KubeProvider read fails",
			failOn:  func(o client.Object) bool { _, ok := o.(*configbutleraiv1alpha3.KubeProvider); return ok },
			wantErr: "read KubeProvider",
		},
		{
			name:    "Namespace read fails",
			failOn:  func(o client.Object) bool { _, ok := o.(*corev1.Namespace); return ok },
			wantErr: "read namespace",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cl := fake.NewClientBuilder().
				WithScheme(scScheme(t)).
				WithObjects(provider).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(
						ctx context.Context,
						c client.WithWatch,
						key client.ObjectKey,
						obj client.Object,
						opts ...client.GetOption,
					) error {
						if tc.failOn(obj) {
							return errors.New("api server unavailable")
						}
						return c.Get(ctx, key, obj, opts...)
					},
				}).
				Build()
			r := &GitTargetReconciler{Client: cl}

			ok, reason, _, err := r.checkSourceAuthorization(context.Background(), target)
			require.Error(t, err, "a transient read failure must requeue, not decide the policy")
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.False(t, ok)
			assert.Empty(t, reason, "an error carries no verdict reason")
		})
	}
}

// TestConditionStatusFromString maps the watch layer's stringly-typed state onto the API type.
// Anything unrecognised must land on Unknown rather than being read as a False (which would
// downgrade a GitTarget on a state the data plane never actually reported).
func TestConditionStatusFromString(t *testing.T) {
	tests := []struct {
		state string
		want  metav1.ConditionStatus
	}{
		{"True", metav1.ConditionTrue},
		{"False", metav1.ConditionFalse},
		{"Unknown", metav1.ConditionUnknown},
		{"", metav1.ConditionUnknown},
		{"true", metav1.ConditionUnknown},
		{"garbage", metav1.ConditionUnknown},
	}
	for _, tc := range tests {
		t.Run("state="+tc.state, func(t *testing.T) {
			assert.Equal(t, tc.want, conditionStatusFromString(tc.state))
		})
	}
}

// TestDescribeKubeConfigKey checks the "key not found" hint names the value→value.yaml fallback
// when the spec omitted a key, so the message tells the user what was actually tried.
func TestDescribeKubeConfigKey(t *testing.T) {
	assert.Equal(t, "value or value.yaml", describeKubeConfigKey(""))
	assert.Equal(t, "kubeconfig", describeKubeConfigKey("kubeconfig"))
}

// TestReconcile_UnauthorizedNamespaceStartsNoWatch pins the whole security property in one place,
// through the REAL Reconcile rather than checkSourceAuthorization in isolation: namespace
// authorization is enforced at reconcile only (there is no admission webhook for it), so this path
// is the entire boundary. A GitTarget whose namespace its KubeProvider does not admit must end
// up Validated=False/NamespaceNotAuthorized AND must never reach DeclareForGitTarget — no watch is
// started, so nothing is ever routed to a branch worker and nothing is written to Git.
func TestReconcile_UnauthorizedNamespaceStartsNoWatch(t *testing.T) {
	const ns, providerName = "team-a", "prod-eu-1"
	providerKey := k8stypes.NamespacedName{Namespace: "platform", Name: providerName}

	// The provider lives in platform and admits team-b only; the GitTarget lives in team-a.
	provider := remoteKubeProvider("kc", "")
	provider.Namespace = providerKey.Namespace
	provider.Spec.AllowedGitTargets = mirrorGrants("team-b")
	gitProvider := &configbutleraiv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "gp", Namespace: ns},
		Spec: configbutleraiv1alpha3.GitProviderSpec{
			AllowedGitBranches: branchesFromSame("main"),
		},
	}
	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "gt", Namespace: ns, UID: "gt-uid"},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			GitBranchRef:    meta.NamespacedObjectReference{Name: gitBranchNameForTest("gp", "main")},
			Path:            "apps",
			KubeProviderRef: meta.NamespacedObjectReference{Namespace: providerKey.Namespace, Name: providerName},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scScheme(t)).
		WithObjects(provider, gitProvider, gitBranchOn("gp", ns, "main"), target,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}).
		WithStatusSubresource(&configbutleraiv1alpha3.GitTarget{}).
		Build()

	watchManager := &watch.Manager{Client: cl, Log: logr.Discard()}
	r := &GitTargetReconciler{
		Client:      cl,
		EventRouter: &watch.EventRouter{WatchManager: watchManager},
	}

	_, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: k8stypes.NamespacedName{Name: "gt", Namespace: ns}})
	require.NoError(t, err)

	var got configbutleraiv1alpha3.GitTarget
	require.NoError(t, cl.Get(context.Background(),
		k8stypes.NamespacedName{Name: "gt", Namespace: ns}, &got))

	validated := apimeta.FindStatusCondition(got.Status.Conditions, GitTargetConditionValidated)
	require.NotNil(t, validated, "Validated condition must be set")
	assert.Equal(t, metav1.ConditionFalse, validated.Status)
	assert.Equal(t, GitTargetReasonNamespaceNotAuthorized, validated.Reason)
	assert.Contains(t, validated.Message, "admits team-b in spec.allowedGitTargets")

	// The data plane is reported blocked, not merely un-ready...
	streams := apimeta.FindStatusCondition(got.Status.Conditions, GitTargetConditionStreamsRunning)
	require.NotNil(t, streams)
	assert.Equal(t, metav1.ConditionUnknown, streams.Status)

	// ...and, the point of the test, no declaration ever reached the watch manager. The Validated
	// gate returns before DeclareForGitTarget, so the capture-on-Declare map stays empty for it.
	gitDest := types.NewResourceReference("gt", ns).WithUID("gt-uid")
	assert.False(t, watchManager.DeclareStatusForGitTarget(gitDest).Declared,
		"a refused GitTarget must never declare watches against its source cluster")

	// Positive control, through the real Declare path: the same manager DOES record a declaration,
	// so the assertion above cannot pass vacuously. DeclareForGitTarget records the submitted
	// source cluster whether or not the owner's pass has run yet — which is exactly the record a
	// refused GitTarget must not produce.
	other := types.NewResourceReference("authorized", ns).WithUID("other-uid")
	watchManager.DeclareForGitTarget(other, providerKey.String(), provider.AuditRoute(),
		configbutleraiv1alpha3.PruneOnEvent)
	declaredOther := watchManager.DeclareStatusForGitTarget(other)
	require.True(t, declaredOther.Declared,
		"the positive control must declare, or the assertion above proves nothing")
	assert.Equal(t, providerKey.String(), declaredOther.ClusterID)
}

func TestKubeProviderReadiness_AllScenarios(t *testing.T) {
	provider := func(conds []metav1.Condition) *configbutleraiv1alpha3.KubeProvider {
		return &configbutleraiv1alpha3.KubeProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "prod-eu-1", Namespace: kpNamespace},
			Status:     configbutleraiv1alpha3.KubeProviderStatus{Conditions: conds},
		}
	}
	ready := metav1.Condition{Type: ConditionTypeReady, Status: metav1.ConditionTrue, Reason: ReasonSucceeded}
	notReady := metav1.Condition{
		Type:    ConditionTypeReady,
		Status:  metav1.ConditionFalse,
		Reason:  "Bad",
		Message: "nope",
	}

	unknownReady := metav1.Condition{
		Type:    ConditionTypeReady,
		Status:  metav1.ConditionUnknown,
		Reason:  "Checking",
		Message: "validating",
	}

	// The absent-provider case is deliberately NOT here: this projection runs only after the
	// Validated gate admitted the target through an existing KubeProvider, and it is handed the
	// provider that gate's reconcile already read. A provider that cannot be read never reaches it.
	tests := []struct {
		name string
		cp   *configbutleraiv1alpha3.KubeProvider
		want metav1.ConditionStatus
	}{
		{"ready", provider([]metav1.Condition{ready}), metav1.ConditionTrue},
		{"not ready -> False (downgrades)", provider([]metav1.Condition{notReady}), metav1.ConditionFalse},
		{"no condition -> Unknown (does not downgrade)", provider(nil), metav1.ConditionUnknown},
		// Only an EXPLICIT Ready=False downgrades. A provider reporting Ready=Unknown is mid-flight,
		// not broken, so it must not hold its GitTargets down.
		{
			"explicit Ready=Unknown -> Unknown (does not downgrade)",
			provider([]metav1.Condition{unknownReady}),
			metav1.ConditionUnknown,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := kubeProviderReadiness(tc.cp)
			assert.Equal(t, tc.want, got.Status)
			assert.NotEmpty(t, got.Message)
		})
	}
}

// TestGitBranchReadiness_AllScenarios: the projection follows the claim first (only a GitBranch that
// holds its branch can be wired) and the GitBranch's own Ready after that.
func TestGitBranchReadiness_AllScenarios(t *testing.T) {
	branch := func(conds ...metav1.Condition) *configbutleraiv1alpha3.GitBranch {
		return &configbutleraiv1alpha3.GitBranch{
			ObjectMeta: metav1.ObjectMeta{Name: "main", Namespace: "platform"},
			Status:     configbutleraiv1alpha3.GitBranchStatus{Conditions: conds},
		}
	}
	provider := &configbutleraiv1alpha3.GitProvider{}
	key := k8stypes.NamespacedName{Namespace: "platform", Name: "main"}
	ready := metav1.Condition{Type: ConditionTypeReady, Status: metav1.ConditionTrue, Reason: ReasonSucceeded}
	conflict := metav1.Condition{Type: ConditionTypeReady, Status: metav1.ConditionFalse,
		Reason: GitBranchReasonBranchConflict, Message: "held by team-b/main"}
	failing := metav1.Condition{Type: ConditionTypeReady, Status: metav1.ConditionFalse,
		Reason: GitBranchReasonPushSessionFailed, Message: "authentication required"}

	tests := []struct {
		name    string
		dest    gitDestination
		held    bool
		want    metav1.ConditionStatus
		message string
	}{
		{"holds its branch", gitDestination{key: key, branch: branch(ready), provider: provider}, true,
			metav1.ConditionTrue, "holds its branch"},
		{"not found", gitDestination{key: key}, false, metav1.ConditionFalse, "not found"},
		{"holds nothing yet", gitDestination{key: key, branch: branch(), provider: provider}, false,
			metav1.ConditionFalse, "does not hold its branch yet"},
		{"another holds it", gitDestination{key: key, branch: branch(conflict), provider: provider}, false,
			metav1.ConditionFalse, "held by team-b/main"},
		{"holds it with a failing credential", gitDestination{key: key, branch: branch(failing), provider: provider},
			true, metav1.ConditionFalse, "authentication required"},
		{"its GitProvider is gone", gitDestination{key: key, branch: branch(ready)}, true,
			metav1.ConditionFalse, "GitProvider that does not exist"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := gitBranchReadiness(tc.dest, tc.held)
			assert.Equal(t, tc.want, got.Status)
			assert.Contains(t, got.Message, tc.message)
			if tc.want == metav1.ConditionFalse {
				assert.Equal(t, GitTargetReasonGitBranchNotReady, got.Reason)
			}
		})
	}
}

// TestResolveSourceKubeProvider covers the single read that feeds both the audit route a
// GitTarget's streams are declared under and its KubeProviderReady projection.
//
// The load-bearing case is the unreadable provider: it must ERROR, so the reconcile requeues
// without declaring. Resolving a route by falling back to the provider's default is right only for a
// provider that declares none — do it for one that declares `shared-route` and this target's
// streams are keyed to a partition the audit handler never writes, which is exactly the silent
// attribution loss this resolution exists to prevent.
func TestResolveSourceKubeProvider(t *testing.T) {
	const providerName = "srcns-delegating"

	// The provider is read from the namespace the reference names, not the GitTarget's own.
	target := func() *configbutleraiv1alpha3.GitTarget {
		return &configbutleraiv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "gt", Namespace: "tenant"},
			Spec: configbutleraiv1alpha3.GitTargetSpec{
				KubeProviderRef: meta.NamespacedObjectReference{Namespace: kpNamespace, Name: providerName},
			},
		}
	}
	provider := func(attribution *configbutleraiv1alpha3.KubeProviderAttribution) client.Object {
		p := inClusterKubeProvider(providerName)
		p.Spec.Attribution = attribution
		return p
	}
	defaultRoute := kpNamespace + "." + providerName

	tests := []struct {
		name      string
		objects   []client.Object
		wantRoute string
	}{
		{
			name: "a declared route is used",
			objects: []client.Object{
				provider(&configbutleraiv1alpha3.KubeProviderAttribution{AuditRoute: "shared-route"}),
			},
			wantRoute: "shared-route",
		},
		{
			name:      "an empty attribution block resolves <namespace>.<name>",
			objects:   []client.Object{provider(&configbutleraiv1alpha3.KubeProviderAttribution{})},
			wantRoute: defaultRoute,
		},
		{
			name:      "no attribution block resolves <namespace>.<name>",
			objects:   []client.Object{provider(nil)},
			wantRoute: defaultRoute,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cl := fake.NewClientBuilder().WithScheme(scScheme(t)).WithObjects(tc.objects...).Build()
			r := &GitTargetReconciler{Client: cl}
			cp, err := r.resolveSourceKubeProvider(context.Background(), target())
			require.NoError(t, err)
			assert.Equal(t, tc.wantRoute, cp.AuditRoute())
		})
	}

	t.Run("an unreadable provider errors instead of guessing a route", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scScheme(t)).Build()
		r := &GitTargetReconciler{Client: cl}
		cp, err := r.resolveSourceKubeProvider(context.Background(), target())
		require.Error(t, err, "a guessed route silently mis-keys this target's attribution facts")
		assert.Nil(t, cp)
		assert.Contains(t, err.Error(), providerName)
	})
}

// TestObserveSourceReachable_Defaults pins what SourceClusterReachable says before a watch manager
// is wired: an inCluster provider names the cluster the operator runs in, reachable by definition;
// a secretRef provider is Unknown until the data plane's first discovery.
func TestObserveSourceReachable_Defaults(t *testing.T) {
	target := &configbutleraiv1alpha3.GitTarget{ObjectMeta: metav1.ObjectMeta{Name: "gt", Namespace: kpNamespace}}
	tests := []struct {
		name       string
		provider   *configbutleraiv1alpha3.KubeProvider
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{"inCluster", inClusterKubeProvider("local"), metav1.ConditionTrue, "LocalCluster"},
		{"secretRef", remoteKubeProvider("kc", ""), metav1.ConditionUnknown, "AwaitingDiscovery"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := (&GitTargetReconciler{}).observeSourceReachable(target, tc.provider)
			assert.Equal(t, tc.wantStatus, got.Status)
			assert.Equal(t, tc.wantReason, got.Reason)
		})
	}
}

// TestKubeProviderToGitTargets: a provider event requeues the GitTargets that mirror through it,
// in any namespace its grants may admit, matched on the provider's namespace AND name.
func TestKubeProviderToGitTargets(t *testing.T) {
	target := func(namespace, name string, ref meta.NamespacedObjectReference) *configbutleraiv1alpha3.GitTarget {
		return &configbutleraiv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       configbutleraiv1alpha3.GitTargetSpec{KubeProviderRef: ref},
		}
	}
	provider := inClusterKubeProvider("local")
	cl := fake.NewClientBuilder().WithScheme(scScheme(t)).WithObjects(
		target(kpNamespace, "own", meta.NamespacedObjectReference{Name: "local"}),
		target("team-b", "admitted", meta.NamespacedObjectReference{Namespace: kpNamespace, Name: "local"}),
		target("team-b", "same-name-elsewhere", meta.NamespacedObjectReference{Name: "local"}),
		target(kpNamespace, "other-provider", meta.NamespacedObjectReference{Name: "remote"}),
	).Build()

	reqs := (&GitTargetReconciler{Client: cl}).kubeProviderToGitTargets(context.Background(), provider)
	keys := make([]k8stypes.NamespacedName, 0, len(reqs))
	for _, req := range reqs {
		keys = append(keys, req.NamespacedName)
	}
	assert.ElementsMatch(t, []k8stypes.NamespacedName{
		{Namespace: kpNamespace, Name: "own"}, {Namespace: "team-b", Name: "admitted"},
	}, keys)
}
