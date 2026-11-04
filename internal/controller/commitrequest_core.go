// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/queue"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

// A CommitRequest is picked up in one of two ways (docs/spec/commitrequest-design.md): by the
// CommitRequestReconciler on the control cluster, or by the CommitRequestServer's watch of a source
// cluster through its KubeProvider. Both hand it to commitRequestCore, which decides everything else
// once, so the two paths cannot drift: who owns the request, whether its GitTarget admits it, who
// created it, the attach and its outcome, the apply it asked for, and how long it is kept.
//
// Three rules keep several observers of one request safe:
//
//  1. Only the owner, the path the named GitTarget's KubeProvider picks, serves the request. A path
//     that is not the owner writes only GitTargetNotFound, after attachTimeout, into an empty status,
//     because nothing else is true for every observer.
//  2. Every status write is conditional on the resourceVersion its decision was made from. A conflict
//     reads the request again and decides again.
//  3. The owner claims a request by writing its first status, the author and Ready=Unknown, before it
//     attaches. After the claim only the owner writes, so a later conflict comes from its own writes,
//     and an outcome the worker produced is written onto the fresh object as it is.
//
// Every step is decided again from the object as it is stored, so the owner is structural rather than
// "the process that wrote the status": after a restart, the owner finds its own claim and resumes.

// commitRequestGVR is the served resource of a CommitRequest, on whichever cluster it lives.
func commitRequestGVR() schema.GroupVersionResource {
	return configbutleraiv1alpha3.GroupVersion.WithResource("commitrequests")
}

const (
	// commitRequestStatusAttempts bounds a write's re-read on conflict before the step is decided again.
	commitRequestStatusAttempts = 3
	// commitRequestUnownedRecheck is how soon a request left to another path is looked at again: its
	// GitTarget's KubeProvider may start or stop serving it.
	commitRequestUnownedRecheck = 5 * time.Minute
)

// commitRequestStore is how one path reads and writes the CommitRequests it serves.
type commitRequestStore interface {
	// get reads the request as it is stored.
	get(ctx context.Context, key types.NamespacedName) (*configbutleraiv1alpha3.CommitRequest, error)
	// updateStatus writes cr's status, conditional on cr.ResourceVersion. On success cr carries the
	// version it wrote.
	updateStatus(ctx context.Context, cr *configbutleraiv1alpha3.CommitRequest) error
	// annotate sets one annotation, conditional on cr.ResourceVersion.
	annotate(ctx context.Context, cr *configbutleraiv1alpha3.CommitRequest, key, value string) error
	// remove deletes the request, conditional on its UID and resourceVersion.
	remove(ctx context.Context, cr *configbutleraiv1alpha3.CommitRequest) error
}

// commitRequestPath is one way of picking a CommitRequest up.
type commitRequestPath struct {
	// provider is the KubeProvider whose watch of a source cluster this is; empty on the control
	// cluster.
	provider types.NamespacedName
	store    commitRequestStore
	// deletes says whether this path stamps configbutler.ai/delete-after on a request that finishes,
	// and deletes a finished request once that time has passed.
	deletes bool
	log     logr.Logger
}

// controlCluster reports whether this is the control-cluster path.
func (p commitRequestPath) controlCluster() bool { return p.provider.Name == "" }

// gitTargetKey is the GitTarget a request names. An omitted namespace is the request's own on the
// control cluster; a request on a source cluster must name it, so without one it is not picked up.
func (p commitRequestPath) gitTargetKey(cr *configbutleraiv1alpha3.CommitRequest) (types.NamespacedName, bool) {
	if !p.controlCluster() && cr.Spec.GitTargetRef.Namespace == "" {
		return types.NamespacedName{}, false
	}
	return cr.GitTargetKey(), true
}

// commitRequestCore serves a CommitRequest for either path, one step at a time.
type commitRequestCore struct {
	// reader reads GitTargets, KubeProviders and Namespaces on the control cluster.
	reader client.Reader
	// controlRequests reads CommitRequests on the control cluster, uncached, for a source-cluster
	// path to tell its own cluster from the control cluster (sourceRefusal); nil on the control path.
	controlRequests client.Reader
	// wakeFallback is how often a request its worker registered is looked at, when the worker's
	// changes wake its steps (CommitRequestWakes); zero polls it at commitRequestPollInterval.
	wakeFallback time.Duration
	finalizer    CommitRequestFinalizer
	// authors finds a request's creator from audit facts; nil when author attribution is off.
	authors watch.AuthorResolver
	// streams holds the fact stream of commitrequests on each audit route a creator is looked up on.
	streams *commitRequestStreams
	// ttl is how long a finished request is kept, written as its delete-after time; zero writes none.
	ttl time.Duration
}

// serve takes one step for the request key on path p, and says how soon to take the next. Zero means
// nothing is due until the request changes.
func (c *commitRequestCore) serve(
	ctx context.Context, p commitRequestPath, key types.NamespacedName,
) (time.Duration, error) {
	cr, err := p.store.get(ctx, key)
	if err != nil {
		return 0, client.IgnoreNotFound(err)
	}
	if !cr.DeletionTimestamp.IsZero() {
		return 0, nil
	}
	if commitRequestIsTerminal(cr) {
		return c.serveFinished(ctx, p, cr)
	}
	if c.finalizer == nil {
		p.log.V(1).Info("CommitRequest finalize disabled: no Finalizer configured", "name", key)
		return 0, nil
	}
	return c.serveOpen(ctx, p, cr)
}

// serveOpen is the step of a request whose save has not ended: it waits for its GitTarget, leaves it
// to the path that owns it, or claims and drives it as its owner.
func (c *commitRequestCore) serveOpen(
	ctx context.Context, p commitRequestPath, cr *configbutleraiv1alpha3.CommitRequest,
) (time.Duration, error) {
	if cr.Spec.GitTargetRef.Name == "" {
		return 0, c.refusePrunedGitTargetRef(ctx, p, cr)
	}
	targetKey, named := p.gitTargetKey(cr)
	if !named {
		return 0, nil
	}
	target, found, err := c.gitTarget(ctx, targetKey)
	if err != nil {
		return 0, err
	}
	if !found {
		if commitRequestClaimed(cr) {
			// Its GitTarget went away after the claim. Only a worker that holds the request can still
			// say how it ends, and that answer is true for every observer.
			return c.drive(ctx, p, cr, nil, nil)
		}
		return c.awaitGitTarget(ctx, p, cr, targetKey)
	}
	provider, owner, refusal, err := c.owns(ctx, p, cr, target)
	if err != nil {
		return 0, err
	}
	if !owner {
		if refusal != "" && len(cr.Status.Conditions) == 0 {
			return c.decide(ctx, p, cr, commitRequestOutcome{err: fmt.Errorf("%w: %s", errCommitRequestNotAllowed,
				refusal)}, false)
		}
		return commitRequestUnownedRecheck, nil
	}
	if commitRequestClaimed(cr) {
		return c.drive(ctx, p, cr, target, provider)
	}
	return c.claim(ctx, p, cr, target, provider)
}

// gitTarget reads the GitTarget key names; found is false when there is none, or it is being deleted.
func (c *commitRequestCore) gitTarget(
	ctx context.Context, key types.NamespacedName,
) (*configbutleraiv1alpha3.GitTarget, bool, error) {
	var target configbutleraiv1alpha3.GitTarget
	if err := c.reader.Get(ctx, key, &target); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read GitTarget %s: %w", key, err)
	}
	return &target, target.DeletionTimestamp.IsZero(), nil
}

// owns decides whether path p serves requests for target: the target's KubeProvider alone picks the
// path. On the control cluster that is a provider with inCluster. A provider with a kubeconfig may point
// back at the control cluster, and then its own watch serves the request, so it is left alone, unless
// that provider serves no CommitRequests: then refusal says why, and where to create the request
// instead. A provider that cannot be found picks no path yet. On a source cluster, see sourceRefusal.
func (c *commitRequestCore) owns(
	ctx context.Context, p commitRequestPath, cr *configbutleraiv1alpha3.CommitRequest,
	target *configbutleraiv1alpha3.GitTarget,
) (*configbutleraiv1alpha3.KubeProvider, bool, string, error) {
	key := target.KubeProviderKey()
	var provider configbutleraiv1alpha3.KubeProvider
	if err := c.reader.Get(ctx, key, &provider); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, "", nil
		}
		return nil, false, "", fmt.Errorf("read KubeProvider %s: %w", key, err)
	}
	if !p.controlCluster() {
		if key == p.provider {
			return &provider, true, "", nil
		}
		refusal, err := c.sourceRefusal(ctx, p, cr, target, &provider)
		return &provider, false, refusal, err
	}
	if provider.IsInCluster() {
		return &provider, true, "", nil
	}
	if provider.ServesCommitRequests() {
		return &provider, false, "", nil
	}
	return &provider, false, fmt.Sprintf("GitTarget %s mirrors another cluster, through KubeProvider %s; create "+
		"the CommitRequest there, and set spec.commitRequests: Reconcile on KubeProvider %s",
		client.ObjectKeyFromObject(target), key, key), nil
}

// sourceRefusal says why no path can serve a request on the source cluster of path p, which names a
// GitTarget whose KubeProvider is another: empty while some path may still serve it.
//
//   - The GitTarget mirrors the cluster GitOps Reverser runs in. The control-cluster path serves it
//     only when p's kubeconfig points back at that cluster, and then the request is the same object
//     there, with the same UID. Read uncached, so a request created a moment ago is not mistaken for
//     one that does not exist. Without the reader nothing is refused.
//   - The GitTarget's KubeProvider serves no CommitRequests, so no watch picks the request up.
//
// A KubeProvider that does serve them may connect to this same cluster under another identity, and
// then its watch serves the request: it is left alone.
func (c *commitRequestCore) sourceRefusal(
	ctx context.Context, p commitRequestPath, cr *configbutleraiv1alpha3.CommitRequest,
	target *configbutleraiv1alpha3.GitTarget, provider *configbutleraiv1alpha3.KubeProvider,
) (string, error) {
	here := fmt.Sprintf("the cluster KubeProvider %s connects to", p.provider)
	targetKey := client.ObjectKeyFromObject(target)
	if provider.IsInCluster() {
		if c.controlRequests == nil {
			return "", nil
		}
		var local configbutleraiv1alpha3.CommitRequest
		err := c.controlRequests.Get(ctx, client.ObjectKeyFromObject(cr), &local)
		switch {
		case err == nil && local.UID == cr.UID:
			return "", nil // the same object: this cluster is the control cluster, whose path serves it
		case err != nil && !apierrors.IsNotFound(err):
			return "", fmt.Errorf("read CommitRequest %s on the cluster GitOps Reverser runs in: %w",
				client.ObjectKeyFromObject(cr), err)
		}
		return fmt.Sprintf("this CommitRequest is on %s, and GitTarget %s mirrors the cluster GitOps Reverser "+
			"runs in, through KubeProvider %s; create the CommitRequest on the cluster GitOps Reverser runs in",
			here, targetKey, provider.Key()), nil
	}
	if !provider.ServesCommitRequests() {
		return fmt.Sprintf("this CommitRequest is on %s, and GitTarget %s mirrors the cluster KubeProvider %s "+
			"connects to, which serves no CommitRequests; create the CommitRequest on that cluster, and set "+
			"spec.commitRequests: Reconcile on KubeProvider %s", here, targetKey, provider.Key(), provider.Key()), nil
	}
	return "", nil
}

// awaitGitTarget waits attachTimeout from the request's creation for the GitTarget it names, which may
// be created alongside it, and then reports GitTargetNotFound. That outcome is true for every observer,
// so any path may write it, but only into an empty status: a path that claimed the request in the
// meantime has started, and is never overwritten.
func (c *commitRequestCore) awaitGitTarget(
	ctx context.Context, p commitRequestPath, cr *configbutleraiv1alpha3.CommitRequest, key types.NamespacedName,
) (time.Duration, error) {
	if len(cr.Status.Conditions) > 0 {
		return commitRequestPollInterval, nil
	}
	wait := resolveRequestWindow(cr.Spec).attachTimeout - time.Since(cr.CreationTimestamp.Time)
	if wait > 0 {
		return min(wait, commitRequestPollInterval), nil
	}
	return c.decide(ctx, p, cr, commitRequestOutcome{err: fmt.Errorf("%w: GitTarget %s does not exist",
		errGitTargetNotFound, key), fallbackTarget: true}, false)
}

// admit decides whether target admits the request in spec.allowedCommitRequests: admitted, or the
// refusal, which wraps errCommitRequestNotAllowed. err is a read to retry.
//
// On the control cluster, a grant's consumerNamespaces admit the request's namespace, by its labels
// there. On a source cluster, a grant's sourceNamespaces name the request's namespace, and the
// KubeProvider must lend that namespace to the GitTarget in spec.allowedGitTargets, the bound its
// WatchRules have too.
func (c *commitRequestCore) admit(
	ctx context.Context, p commitRequestPath, cr *configbutleraiv1alpha3.CommitRequest,
	target *configbutleraiv1alpha3.GitTarget, provider *configbutleraiv1alpha3.KubeProvider,
) (bool, error, error) {
	key := client.ObjectKeyFromObject(target)
	if p.controlCluster() {
		decision, err := authz.CommitRequestAdmitted(ctx, c.reader, cr, target)
		if err != nil {
			return false, nil, err
		}
		if !decision.Allowed {
			return false, fmt.Errorf("%w: %s; on the cluster GitOps Reverser runs in, a grant admits by "+
				"consumerNamespaces", errCommitRequestNotAllowed, decision.Message), nil
		}
		return true, nil, nil
	}
	if !target.AdmitsSourceCommitRequest(cr.Namespace) {
		return false, fmt.Errorf("%w: GitTarget %s does not admit CommitRequests from namespace %q of the "+
			"cluster it mirrors; on that cluster, a grant in spec.allowedCommitRequests admits by sourceNamespaces",
			errCommitRequestNotAllowed, key, cr.Namespace), nil
	}
	labels, err := namespaceLabelsOf(ctx, c.reader, target.Namespace)
	if err != nil {
		return false, nil, err
	}
	scope, err := provider.AdmitsGitTarget(key, labels)
	if err != nil {
		return false, nil, err
	}
	if !scope.AllowsSourceNamespace(cr.Namespace) {
		return false, fmt.Errorf("%w: KubeProvider %s does not lend namespace %q to GitTarget %s in "+
			"spec.allowedGitTargets", errCommitRequestNotAllowed, provider.Key(), cr.Namespace, key), nil
	}
	return true, nil, nil
}

// claim takes an unclaimed request that p owns: it checks the message and the GitTarget's grants,
// finds the request's creator, and writes the first status, which records the author. Only once that
// write landed is the request attached.
func (c *commitRequestCore) claim(
	ctx context.Context, p commitRequestPath, cr *configbutleraiv1alpha3.CommitRequest,
	target *configbutleraiv1alpha3.GitTarget, provider *configbutleraiv1alpha3.KubeProvider,
) (time.Duration, error) {
	// The schema rejects an invalid literal message at admission, so a rejection here names an object
	// the schema could not have seen: one stored before the rule landed. It is terminal rather than
	// retried, because the spec is immutable and no apply can repair it.
	if err := git.ValidateLiteralCommitMessage(cr.Spec.Message); err != nil {
		return c.decide(ctx, p, cr, commitRequestOutcome{err: err, fallbackTarget: true}, false)
	}
	admitted, refusal, err := c.admit(ctx, p, cr, target, provider)
	if err != nil {
		return 0, err
	}
	if !admitted {
		return c.decide(ctx, p, cr, commitRequestOutcome{err: refusal}, false)
	}
	author, refused, err := c.creator(ctx, cr, provider)
	if err != nil {
		return 0, err
	}
	if refused {
		return c.decide(ctx, p, cr, commitRequestOutcome{authorUnresolved: true}, false)
	}

	claimed, err := c.writeClaim(ctx, p, cr, author, string(target.UID))
	if err != nil || !claimed {
		return commitRequestRequeueNow, err
	}
	p.log.Info("CommitRequest claimed", "name", client.ObjectKeyFromObject(cr),
		"gitTarget", client.ObjectKeyFromObject(target), "author", author)
	return c.drive(ctx, p, cr, target, provider)
}

// creator is the request's author, found from its audit fact on the route of the GitTarget's
// KubeProvider, as a watched write's author is, and within the same grace. The wait runs whenever
// attribution is on. refused reports a request no fact names on a provider that has received audit
// facts: its fact went missing, and it must not take the window that stays unattributed.
//
// The lookup asks for the fact that created the request, by its uid: a later write to it, such as a
// label patch, names who wrote that, not who asked for the save, and must not take that actor's
// window. It is made only before the claim, and the claim records the answer.
//
// Whether the provider has received audit facts is read after the wait, not from provider: the first
// facts may arrive during it, and a request whose own fact is still missing then must be refused
// rather than take the unnamed window. A provider that cannot be read then is an error, retried.
func (c *commitRequestCore) creator(
	ctx context.Context, cr *configbutleraiv1alpha3.CommitRequest, provider *configbutleraiv1alpha3.KubeProvider,
) (string, bool, error) {
	if c.authors == nil {
		return "", false, nil
	}
	c.streams.ensure(provider.AuditRoute())
	user, outcome := c.authors.ResolveAuthor(ctx, watch.AuthorQuery{
		AuditRoute:   provider.AuditRoute(),
		GVR:          commitRequestGVR(),
		UID:          cr.UID,
		Namespace:    cr.Namespace,
		Name:         cr.Name,
		ExactCapable: true,
		Creation:     true,
	})
	if outcome.NamesActor() {
		return user.Username, false, nil
	}
	if outcome == git.AttributionNotAttempted {
		return "", false, nil
	}
	var current configbutleraiv1alpha3.KubeProvider
	if err := c.reader.Get(ctx, provider.Key(), &current); err != nil {
		return "", false, fmt.Errorf("read KubeProvider %s after the author wait: %w", provider.Key(), err)
	}
	return "", apimeta.IsStatusConditionTrue(current.Status.Conditions, KubeProviderConditionAuditFactsReceived), nil
}

// writeClaim writes the first status of a request: its author, the GitTarget that admitted it, and
// Ready=Unknown. A conflict reads the request again, and the claim is written onto it only while it is
// still unclaimed; otherwise claimed=false, and the next step decides from what is there.
func (c *commitRequestCore) writeClaim(
	ctx context.Context, p commitRequestPath, cr *configbutleraiv1alpha3.CommitRequest, author, targetUID string,
) (bool, error) {
	for attempt := 1; ; attempt++ {
		cr.Status.Author = author
		cr.Status.GitTargetUID = targetUID
		markCommitRequestProgressing(cr, "", "")
		err := p.store.updateStatus(ctx, cr)
		if err == nil {
			return true, nil
		}
		if !apierrors.IsConflict(err) || attempt == commitRequestStatusAttempts {
			return false, err
		}
		fresh, same, getErr := c.reread(ctx, p, cr)
		if getErr != nil || !same || len(fresh.Status.Conditions) > 0 {
			return false, getErr
		}
		*cr = *fresh
	}
}

// reread reads cr again; same is false when it is gone or was replaced by another request of its name.
func (c *commitRequestCore) reread(
	ctx context.Context, p commitRequestPath, cr *configbutleraiv1alpha3.CommitRequest,
) (*configbutleraiv1alpha3.CommitRequest, bool, error) {
	fresh, err := p.store.get(ctx, client.ObjectKeyFromObject(cr))
	if err != nil {
		return nil, false, client.IgnoreNotFound(err)
	}
	return fresh, fresh.UID == cr.UID, nil
}

// attachFor is the attach of a claimed request to its GitTarget's window.
func (c *commitRequestCore) attachFor(cr *configbutleraiv1alpha3.CommitRequest) git.AttachCommitRequest {
	window := resolveRequestWindow(cr.Spec)
	attribution := git.AttributionNotAttempted
	switch {
	case cr.Status.Author != "":
		attribution = git.AttributionResolved
	case c.authors != nil:
		attribution = git.AttributionUnresolved
	}
	key := cr.GitTargetKey()
	return git.AttachCommitRequest{
		Namespace:          cr.Namespace,
		Name:               cr.Name,
		UID:                string(cr.UID),
		Author:             cr.Status.Author,
		Attribution:        attribution,
		GitTargetName:      key.Name,
		GitTargetNamespace: key.Namespace,
		GitTargetUID:       cr.Status.GitTargetUID,
		Message:            cr.Spec.Message,
		Attach:             window.attach,
		AttachTimeout:      window.attachTimeout,
		IdleTimeout:        window.idleTimeout,
		MaxDuration:        window.maxDuration,
		CommitEmpty:        cr.Spec.WhenNothingToCommit == configbutleraiv1alpha3.NothingToCommitCommitEmpty,
		CatchUpNow:         cr.Spec.CatchUp == configbutleraiv1alpha3.CatchUpAfterPush,
	}
}

// drive advances a claimed request: it sends the attach, or the withdraw once the request is past its
// bound, and records the worker's phase until the worker has an outcome. target and its provider are
// nil when the GitTarget is gone.
//
// A GitTarget with another UID than the one that admitted the request replaced it, and admitted
// nothing: it is asked again. Its admission is recorded before anything is attached to it; its
// refusal is written only once no worker holds the request, because the GitTarget it replaced may
// have had the request committed.
func (c *commitRequestCore) drive(
	ctx context.Context, p commitRequestPath, cr *configbutleraiv1alpha3.CommitRequest,
	target *configbutleraiv1alpha3.GitTarget, provider *configbutleraiv1alpha3.KubeProvider,
) (time.Duration, error) {
	attach := c.attachFor(cr)
	if target != nil && string(target.UID) != cr.Status.GitTargetUID {
		admitted, refusal, err := c.admit(ctx, p, cr, target, provider)
		if err != nil {
			return 0, err
		}
		if !admitted {
			return c.refuseOnceUnheld(ctx, p, cr, attach, refusal)
		}
		cr.Status.GitTargetUID = string(target.UID)
		if err := p.store.updateStatus(ctx, cr); err != nil {
			return commitRequestRequeueNow, ignoreConflict(err)
		}
		attach.GitTargetUID = cr.Status.GitTargetUID
	}
	if time.Since(cr.CreationTimestamp.Time) >= resolveRequestWindow(cr.Spec).resolveTimeout() {
		// Past the bound only the withdraw is sent: an attach sent now would sit behind it on the
		// worker's FIFO and could register the request after it was cancelled.
		return c.withdraw(ctx, p, cr, attach)
	}
	result, resolved, err := c.finalizer.ServiceCommitRequest(ctx, attach)
	if err != nil || !resolved {
		if err != nil {
			p.log.V(1).Info("CommitRequest attach not yet serviceable; will retry",
				"name", client.ObjectKeyFromObject(cr), "err", err.Error())
		}
		return c.recordPhase(ctx, p, cr, result.Phase, result.Held)
	}
	if result.Err != nil {
		p.log.Error(result.Err, "CommitRequest finalize failed", "name", client.ObjectKeyFromObject(cr),
			"gitTarget", cr.GitTargetKey().String())
	}
	return c.decide(ctx, p, cr, commitRequestOutcome{result: result, err: result.Err}, true)
}

// withdraw handles a claimed request past its safety window. A request the worker holds keeps polling
// with no bound of its own: only the worker can say how it ends. Any other request is withdrawn, and
// fails only on the worker's answer, so a request reported failed can never be committed afterwards.
func (c *commitRequestCore) withdraw(
	ctx context.Context, p commitRequestPath, cr *configbutleraiv1alpha3.CommitRequest,
	attach git.AttachCommitRequest,
) (time.Duration, error) {
	result, resolved, err := c.finalizer.WithdrawCommitRequest(ctx, attach)
	if err != nil || !resolved {
		if err != nil {
			p.log.V(1).Info("CommitRequest withdraw not yet serviceable; will retry",
				"name", client.ObjectKeyFromObject(cr), "err", err.Error())
		}
		return c.recordPhase(ctx, p, cr, result.Phase, result.Held)
	}
	if !errors.Is(result.Err, git.ErrCommitRequestWithdrawn) {
		// The worker resolved it on its own before the withdraw: report that outcome.
		return c.decide(ctx, p, cr, commitRequestOutcome{result: result, err: result.Err}, true)
	}
	message := resolveTimeoutMessage
	switch {
	case errors.Is(result.Err, git.ErrBranchWorkerStopped):
		message = git.ErrBranchWorkerStopped.Error()
	case result.Phase == git.PhaseWaitingForWorker:
		message = noWorkerTimeoutMessage
	}
	p.log.Info("CommitRequest was not acted on within the safety window; withdrawn and failed closed",
		"name", client.ObjectKeyFromObject(cr), "reason", message)
	// A worker's answer carries the branch, and only then is the request's GitTarget known.
	return c.decide(ctx, p, cr, commitRequestOutcome{err: errors.New(message),
		fallbackTarget: result.Branch == ""}, true)
}

// refuseOnceUnheld fails a request that a replacement of its GitTarget refuses. The worker that may
// hold it answers first: its outcome is reported, and a request it holds is waited for. The refusal is
// written only once the request is withdrawn, or nothing held it.
func (c *commitRequestCore) refuseOnceUnheld(
	ctx context.Context, p commitRequestPath, cr *configbutleraiv1alpha3.CommitRequest,
	attach git.AttachCommitRequest, refusal error,
) (time.Duration, error) {
	result, resolved, err := c.finalizer.WithdrawCommitRequest(ctx, attach)
	if err != nil || !resolved {
		return c.recordPhase(ctx, p, cr, result.Phase, result.Held)
	}
	outcome := commitRequestOutcome{result: result, err: result.Err}
	if errors.Is(result.Err, git.ErrCommitRequestWithdrawn) {
		outcome = commitRequestOutcome{err: refusal}
	}
	return c.decide(ctx, p, cr, outcome, true)
}

// recordPhase reports the phase the worker says the request is in, and polls again. It writes only on
// a change; a conflict is left to the next poll, which reads the request again.
func (c *commitRequestCore) recordPhase(
	ctx context.Context, p commitRequestPath, cr *configbutleraiv1alpha3.CommitRequest,
	phase git.CommitRequestPhase, held string,
) (time.Duration, error) {
	poll := commitRequestPollInterval
	if registered(phase) {
		poll = c.registeredPoll()
	}
	if progressRecorded(cr, phase, held) {
		return poll, nil
	}
	markCommitRequestProgressing(cr, phase, held)
	if err := p.store.updateStatus(ctx, cr); err != nil {
		return poll, ignoreConflict(err)
	}
	return poll, nil
}

// registered reports whether the worker has registered a request in this phase, so it reports the
// request's every change from now on. Before that, while there is no worker or the attach is still
// on its way, nothing would wake the step.
func registered(phase git.CommitRequestPhase) bool {
	return phase == git.PhaseWaitingForWindow || phase.Held()
}

// registeredPoll is how soon a step looks again at a request its worker has registered: the
// fallback for a dropped change when the worker's changes wake the steps, the poll otherwise.
func (c *commitRequestCore) registeredPoll() time.Duration {
	if c.wakeFallback > 0 {
		return c.wakeFallback
	}
	return commitRequestPollInterval
}

// refusePrunedGitTargetRef fails a request whose spec names no GitTarget, which can only be one
// thing: an object stored before spec.targetRef was renamed to spec.gitTargetRef, whose old value the
// apiserver stopped serving the moment the new CRDs landed. Admission refuses an empty name on every
// path, so nothing else can produce it. It is terminal, because the spec is immutable: the message
// names the only fix there is.
func (c *commitRequestCore) refusePrunedGitTargetRef(
	ctx context.Context, p commitRequestPath, cr *configbutleraiv1alpha3.CommitRequest,
) error {
	if len(cr.Status.Conditions) > 0 {
		return nil
	}
	_, err := c.decide(ctx, p, cr, commitRequestOutcome{pruned: true, fallbackTarget: true}, false)
	return err
}

// commitRequestStreams holds, for each audit route a CommitRequest's creator is looked up on, the fact
// stream of commitrequests there, so the fact follower reads those audit facts at all. Without it the
// lookup would wait out the grace and name nobody. A stream is acquired the first time its route is
// needed and held from then on: a stream followed late is replayed from the TTL horizon, so the first
// request on a route still finds its fact.
type commitRequestStreams struct {
	set  *queue.FactStreamSet
	mu   sync.Mutex
	held map[string]func()
}

// ensure acquires the stream of commitrequests on route, once.
func (s *commitRequestStreams) ensure(route string) {
	if s == nil || s.set == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, held := s.held[route]; held {
		return
	}
	if s.held == nil {
		s.held = map[string]func(){}
	}
	s.held[route] = s.set.Acquire(queue.FactStreamKeyFor(route, commitRequestGVR().GroupResource()))
}

// ignoreConflict drops a conflict: the next step reads the request again and decides from that.
func ignoreConflict(err error) error {
	if apierrors.IsConflict(err) {
		return nil
	}
	return err
}
