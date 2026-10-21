// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"net/http"
	"os/exec"
	"path/filepath"
	"testing"

	gogithttp "github.com/go-git/go-git/v6/plumbing/transport/http"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pushSessionServer serves a seeded bare repository over canonical git's HTTP backend, the way a
// Git host does: anyone may fetch, and a push session needs the user "bot" with the password
// "secret". receivePack opts the repository into pushes over HTTP at all.
func pushSessionServer(t *testing.T, receivePack bool) string {
	t.Helper()
	projectRoot := t.TempDir()
	repoDir := filepath.Join(projectRoot, "repo.git")
	createBareRepo(t, repoDir)
	if receivePack {
		out, err := exec.Command("git", "-C", repoDir, "config", "http.receivepack", "true").CombinedOutput()
		require.NoError(t, err, "git config http.receivepack: %s", out)
	}
	simulateClientCommitOnDisk(t, repoDir, "main", "README.md", "seed\n")

	sim := startRealGitServer(t, projectRoot, repoDir)
	around := func(w http.ResponseWriter, r *http.Request, backend http.Handler) {
		if r.URL.Query().Get("service") == "git-receive-pack" || isReceivePack(r) {
			if user, password, ok := r.BasicAuth(); !ok || user != "bot" || password != "secret" {
				w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
		}
		backend.ServeHTTP(w, r)
	}
	sim.around.Store(&around)
	return sim.RepoURL
}

// botAuth is the user "bot" with password.
func botAuth(password string) Credential {
	return Credential{Basic: &gogithttp.BasicAuth{Username: "bot", Password: password}}
}

// TestProvePushSession_NeedsTheCredentialThatMayPush: the proof a GitBranch gives before it claims
// a branch is a receive-pack session opened with its GitProvider's credential, over a real Git
// server (file:// is not one). Fetching anonymously proves nothing about pushing.
func TestProvePushSession_NeedsTheCredentialThatMayPush(t *testing.T) {
	url := pushSessionServer(t, true)
	ctx := context.Background()

	_, err := CheckRepo(ctx, url, nil)
	require.NoError(t, err, "an anonymous fetch works, which is why it cannot be the proof")

	require.Error(t, ProvePushSession(ctx, url, nil), "no credential")
	require.Error(t, ProvePushSession(ctx, url, botAuth("wrong").Options()), "a wrong credential")
	assert.NoError(t, ProvePushSession(ctx, url, botAuth("secret").Options()))
}

// TestProvePushSession_FailsWhereTheHostRefusesPushes: a host that does not offer receive-pack for
// the repository refuses the session whatever the credential.
func TestProvePushSession_FailsWhereTheHostRefusesPushes(t *testing.T) {
	url := pushSessionServer(t, false)

	assert.Error(t, ProvePushSession(context.Background(), url, botAuth("secret").Options()))
}

// TestProvePushSession_FailsForARepositoryThatIsNotThere: a URL that names nothing proves nothing.
func TestProvePushSession_FailsForARepositoryThatIsNotThere(t *testing.T) {
	url := pushSessionServer(t, true)

	assert.Error(t, ProvePushSession(context.Background(), url+"-missing", botAuth("secret").Options()))
}
