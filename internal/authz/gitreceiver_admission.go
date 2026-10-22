// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// ReasonGitReceiverNotFound is the denial reason when a GitProvider's spec.gitReceiverRef names a
// GitReceiver that does not exist.
const ReasonGitReceiverNotFound = "GitReceiverNotFound"

// GitReceiverAdmitted reports whether provider may be served by the GitReceiver its
// spec.gitReceiverRef names: the GitReceiver exists, and it admits the provider's namespace (its
// own, or through spec.accessFrom). It returns the GitReceiver once it exists. A provider without
// a reference is denied with no reason: there is nothing to admit.
//
// It is the one policy read the GitProvider's GitReceiverReady condition and the push receiver
// both route through, so a delivery never wakes a provider its status reports as refused.
func GitReceiverAdmitted(
	ctx context.Context,
	reader client.Reader,
	provider *configv1alpha3.GitProvider,
) (Decision, *configv1alpha3.GitReceiver, error) {
	key, ok := provider.GitReceiverKey()
	if !ok {
		return Decision{}, nil, nil
	}
	receiver := &configv1alpha3.GitReceiver{}
	if err := reader.Get(ctx, key, receiver); err != nil {
		if apierrors.IsNotFound(err) {
			return Decision{
				Reason:  ReasonGitReceiverNotFound,
				Message: fmt.Sprintf("spec.gitReceiverRef names GitReceiver %s, which does not exist", key),
			}, nil, nil
		}
		return Decision{}, nil, fmt.Errorf("read GitReceiver %s: %w", key, err)
	}
	decision, err := ReceiverAdmitsNamespace(ctx, reader, receiver, provider.Namespace)
	return decision, receiver, err
}

// ReceiverAdmitsNamespace reports whether receiver serves GitProviders in namespace: its own
// namespace always, another one only as its spec.accessFrom admits it. The push receiver asks it of
// each GitProvider that references the receiver, so it reads no GitProvider itself.
func ReceiverAdmitsNamespace(
	ctx context.Context,
	reader client.Reader,
	receiver *configv1alpha3.GitReceiver,
	namespace string,
) (Decision, error) {
	if namespace == receiver.Namespace {
		return Decision{Allowed: true}, nil
	}
	nsLabels, err := namespaceLabels(ctx, reader, namespace)
	if err != nil {
		return Decision{}, err
	}
	allowed, selErr := receiver.AdmitsNamespace(namespace, nsLabels)
	if selErr != nil {
		return Decision{
			Reason:  ReasonNamespaceNotAuthorized,
			Message: fmt.Sprintf("GitReceiver %s accessFrom selector is invalid: %v", receiver.Key(), selErr),
		}, nil
	}
	if !allowed {
		return Decision{
			Reason: ReasonNamespaceNotAuthorized,
			Message: fmt.Sprintf(
				"namespace %q is not permitted to reference GitReceiver %s: it is not the receiver's own "+
					"namespace, and spec.accessFrom does not admit it", namespace, receiver.Key()),
		}, nil
	}
	return Decision{Allowed: true}, nil
}
