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

package git

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/ssh"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// parseIdentifierFromPath parses "{group-or-core?}/{version}/{resource}/{namespace?}/{name}.yaml"
// into a ResourceIdentifier. For core group, the path starts with version (e.g., "v1/...").
// This is a shared helper used by both old and new worker implementations.
func parseIdentifierFromPath(p string) (itypes.ResourceIdentifier, bool) {
	parts := strings.Split(p, "/")
	// Minimum cluster-scoped core: v1/{resource}/{name}.yaml => 3 parts
	// Minimum cluster-scoped grouped: {group}/{version}/{resource}/{name}.yaml => 4 parts
	if len(parts) < minCoreClusterParts {
		return itypes.ResourceIdentifier{}, false
	}
	last := parts[len(parts)-1]
	name := strings.TrimSuffix(last, filepath.Ext(last))

	var group, version, resource, namespace string
	switch len(parts) {
	case minCoreClusterParts: // core cluster-scoped: v1/resource/name.yaml
		group = ""
		version = parts[0]
		resource = parts[1]
		namespace = ""
	case groupedClusterOrCoreNamespacedParts: // grouped cluster-scoped OR core namespaced
		// Heuristic: if parts[0] looks like "v1" (starts with 'v' and digits), assume core namespaced is not possible with 4 parts.
		// For our current mapping, core namespaced has 4 parts: v1/resource/namespace/name.yaml
		// so handle that first.
		if strings.HasPrefix(parts[0], "v") { // v1/...
			group = ""
			version = parts[0]
			resource = parts[1]
			namespace = parts[2]
		} else {
			group = parts[0]
			version = parts[1]
			resource = parts[2]
			namespace = "" // cluster-scoped grouped
		}
	case groupedNamespacedParts: // grouped namespaced: group/version/resource/namespace/name.yaml
		group = parts[0]
		version = parts[1]
		resource = parts[2]
		namespace = parts[3]
	default:
		// Longer paths are not expected in current mapping
		return itypes.ResourceIdentifier{}, false
	}

	return itypes.ResourceIdentifier{
		Group:     group,
		Version:   version,
		Resource:  resource,
		Namespace: namespace,
		Name:      name,
	}, true
}

// getAuthFromSecret fetches authentication credentials from the specified secret.
// Shared helper used by both old and new worker implementations.
func getAuthFromSecret(
	ctx context.Context,
	k8sClient client.Client,
	repoConfig *v1alpha1.GitRepoConfig,
) (transport.AuthMethod, error) {
	// If no secret reference is provided, return nil auth (for public repositories)
	if repoConfig.Spec.SecretRef == nil {
		return nil, nil //nolint:nilnil // Returning nil auth for public repos is semantically correct
	}

	secretName := types.NamespacedName{
		Name:      repoConfig.Spec.SecretRef.Name,
		Namespace: repoConfig.Namespace,
	}

	var secret corev1.Secret
	if err := k8sClient.Get(ctx, secretName, &secret); err != nil {
		return nil, fmt.Errorf("failed to get secret %s: %w", secretName, err)
	}

	// Check for SSH authentication first
	if privateKey, ok := secret.Data["ssh-privatekey"]; ok {
		keyPassword := ""
		if passData, hasPass := secret.Data["ssh-password"]; hasPass {
			keyPassword = string(passData)
		}
		// Get known_hosts if available
		knownHosts := ""
		if knownHostsData, hasKnownHosts := secret.Data["known_hosts"]; hasKnownHosts {
			knownHosts = string(knownHostsData)
		}
		return ssh.GetAuthMethod(string(privateKey), keyPassword, knownHosts)
	}

	// Check for HTTP basic authentication
	if username, hasUsername := secret.Data["username"]; hasUsername {
		if password, hasPassword := secret.Data["password"]; hasPassword {
			return GetHTTPAuthMethod(string(username), string(password))
		}
		return nil, fmt.Errorf("secret %s contains username but no password for HTTP auth", secretName)
	}

	return nil, fmt.Errorf(
		"secret %s does not contain valid authentication data (ssh-privatekey or username/password)",
		secretName,
	)
}
