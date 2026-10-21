// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// ResolveGitBranch reads the GitBranch target references, and that GitBranch's GitProvider. An
// object that does not exist comes back nil with no error: a missing reference is a status, not a
// failure to retry. A target with no spec.gitBranchRef resolves to nothing.
func ResolveGitBranch(
	ctx context.Context,
	reader client.Reader,
	target *v1alpha3.GitTarget,
) (*v1alpha3.GitBranch, *v1alpha3.GitProvider, error) {
	key := target.GitBranchKey()
	if key.Name == "" {
		return nil, nil, nil
	}
	branch := &v1alpha3.GitBranch{}
	if err := reader.Get(ctx, key, branch); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("read GitBranch %s: %w", key, err)
	}
	provider := &v1alpha3.GitProvider{}
	if err := reader.Get(ctx, branch.GitProviderKey(), provider); err != nil {
		if apierrors.IsNotFound(err) {
			return branch, nil, nil
		}
		return nil, nil, fmt.Errorf("read GitProvider %s: %w", branch.GitProviderKey(), err)
	}
	return branch, provider, nil
}
