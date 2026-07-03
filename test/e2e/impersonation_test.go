// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// createConfigMapAsImpersonatedUser creates a ConfigMap while impersonating
// asUser with the given groups and user.extra entries. The real kube-apiserver
// then emits an audit event whose impersonatedUser.Extra map carries those
// entries — the same shape a structured authentication configuration produces
// when it maps OIDC name/email claims into user.extra. This lets the e2e suite
// exercise OIDC-style commit authorship without running an identity provider.
//
// Authorization for an impersonated request runs as the impersonated identity,
// so callers pass system:masters as a group to keep the create authorized
// without provisioning per-user RBAC.
func createConfigMapAsImpersonatedUser(
	namespace, name, asUser string,
	groups []string,
	extra map[string][]string,
) error {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	if ctx := kubectlContext(); ctx != "" {
		overrides.CurrentContext = ctx
	}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules, overrides,
	).ClientConfig()
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}

	config.Impersonate = rest.ImpersonationConfig{
		UserName: asUser,
		Groups:   groups,
		Extra:    extra,
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("build impersonated clientset: %w", err)
	}

	_, err = clientset.CoreV1().ConfigMaps(namespace).Create(
		context.Background(),
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Data:       map[string]string{"test-key": "oidc-author"},
		},
		metav1.CreateOptions{},
	)
	if err != nil {
		return fmt.Errorf("create impersonated ConfigMap: %w", err)
	}
	return nil
}
