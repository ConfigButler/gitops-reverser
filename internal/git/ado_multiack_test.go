// SPDX-License-Identifier: Apache-2.0

package git

// Azure DevOps rejects any protocol-v0 `upload-pack` request whose capability list omits
// `multi_ack`, answering HTTP 400 with `TF401041: Clients must support multi-ack.` go-git v5 keeps
// MultiACK and MultiACKDetailed in transport.UnsupportedCapabilities and deletes them from the
// server's advertisement as it parses, so the capability is never requested and every fetch against
// ADO fails.
//
// Nobody on the team has an Azure DevOps tenant, so these tests reproduce the failure locally. The
// load-bearing fact is that canonical git's own `upload-pack` advertises `multi_ack` and
// `multi_ack_detailed`, which makes `git-http-backend` a genuine multi_ack server. Wrapping it in a
// proxy that enforces ADO's rule gives a faithful simulator for both halves of the problem: the
// proxy reproduces the rejected request, and the real backend reproduces the multi-ACK response that
// v5 additionally cannot parse.
//
// References:
//   - https://github.com/go-git/go-git/issues/64 — the original ADO report, open since 2019
//   - https://github.com/fluxcd/source-controller/issues/104 — Flux hitting the same wall
//   - https://github.com/go-git/go-git/pull/1204 — the multi_ack implementation, v6 only
//   - https://git-scm.com/docs/protocol-capabilities — multi_ack is upload-pack only
//   - docs/design/azure-devops-multi-ack.md — the capability matrix these tests encode

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adoRejectionBody is Azure DevOps' response to an upload-pack request without multi_ack.
const adoRejectionBody = "TF401041: Clients must support multi-ack."

// adoSimulator is a git HTTP server that enforces Azure DevOps' multi_ack rule.
type adoSimulator struct {
	// RepoURL is the clone URL of the served repository.
	RepoURL string

	// uploadPackPosts counts POSTs to git-upload-pack; rejected counts those answered 400.
	uploadPackPosts atomic.Int64
	rejected        atomic.Int64
}

// gitHTTPBackend locates canonical git's CGI server, skipping the test when it is unavailable.
func gitHTTPBackend(tb testing.TB) string {
	tb.Helper()

	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		tb.Skipf("git --exec-path failed, cannot run the ADO simulator: %v", err)
	}

	path := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(path); err != nil {
		tb.Skipf("git-http-backend not found at %s: %v", path, err)
	}

	return path
}

// startADOSimulator serves a bare repository over HTTP through canonical git's http-backend and
// reproduces Azure DevOps' rejection: an upload-pack POST whose body does not contain the multi_ack
// capability is answered with HTTP 400 instead of being forwarded.
//
// The v2 opt-in header is stripped from every request. ADO's failure is a protocol-v0 behaviour, and
// a client that can negotiate protocol v2 would otherwise sidestep multi_ack entirely and pass these
// tests without implementing the capability under test.
func startADOSimulator(tb testing.TB, projectRoot, repoDir string) *adoSimulator {
	tb.Helper()
	return startGitHTTPServer(tb, projectRoot, repoDir, true)
}

// startRealGitServer serves a bare repository over HTTP through canonical git's http-backend with no
// added restrictions.
//
// This exists because go-git's `file://` transport is no longer a faithful server. In v5 it spawned
// the real git-upload-pack/git-receive-pack binaries; in v6 it runs go-git's own in-process
// transport.ReceivePack, whose updateReferences validates only that a reference exists and then
// calls SetReference(cmd.New) — it never compares cmd.Old against the current value. A push over
// `file://` therefore always wins, which silently makes any test of our compare-and-swap vacuous.
// Real git enforces it, so tests that depend on a rejected concurrent push must use this.
func startRealGitServer(tb testing.TB, projectRoot, repoDir string) *adoSimulator {
	tb.Helper()
	return startGitHTTPServer(tb, projectRoot, repoDir, false)
}

// startGitHTTPServer serves repoDir over HTTP via canonical git's CGI backend. When
// enforceMultiAck is set it additionally rejects upload-pack requests that omit the capability,
// which is what makes it an Azure DevOps simulator.
func startGitHTTPServer(tb testing.TB, projectRoot, repoDir string, enforceMultiAck bool) *adoSimulator {
	tb.Helper()

	backendPath := gitHTTPBackend(tb)
	sim := &adoSimulator{}

	backend := &cgi.Handler{
		Path: backendPath,
		Env: []string{
			"GIT_PROJECT_ROOT=" + projectRoot,
			"GIT_HTTP_EXPORT_ALL=1",
		},
		InheritEnv: []string{"PATH"},
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Force protocol v0: see the doc comment above.
		r.Header.Del("Git-Protocol")

		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
			sim.uploadPackPosts.Add(1)

			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read body: "+err.Error(), http.StatusInternalServerError)
				return
			}

			if enforceMultiAck && !requestOffersMultiAck(body, r.Header.Get("Content-Encoding")) {
				sim.rejected.Add(1)
				http.Error(w, adoRejectionBody, http.StatusBadRequest)
				return
			}

			// Rewind for the backend.
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}

		backend.ServeHTTP(w, r)
	})

	server := httptest.NewServer(handler)
	tb.Cleanup(server.Close)

	sim.RepoURL = server.URL + "/" + filepath.Base(repoDir)
	return sim
}

// requestOffersMultiAck reports whether an upload-pack request body advertises multi_ack. The
// capability list travels on the first `want` pkt-line, so a substring test over the body is
// exactly the check ADO performs.
func requestOffersMultiAck(body []byte, contentEncoding string) bool {
	if strings.Contains(strings.ToLower(contentEncoding), "gzip") {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return false
		}
		defer func() { _ = zr.Close() }()
		if plain, err := io.ReadAll(zr); err == nil {
			body = plain
		}
	}
	return bytes.Contains(body, []byte("multi_ack"))
}

// newADORepo creates a bare repository seeded with one commit on main, configured so http-backend
// will serve both fetch and push.
func newADORepo(tb testing.TB) (string, string) {
	tb.Helper()

	projectRoot := tb.TempDir()
	repoDir := filepath.Join(projectRoot, "repo.git")
	createBareRepo(tb, repoDir)

	// http-backend refuses receive-pack unless the repository opts in.
	cmd := exec.Command("git", "-C", repoDir, "config", "http.receivepack", "true")
	out, err := cmd.CombinedOutput()
	require.NoError(tb, err, "git config http.receivepack: %s", out)

	simulateClientCommitOnDisk(tb, repoDir, "main", "seed.yaml", "kind: Seed\n")
	return projectRoot, repoDir
}

// setRemoteURL repoints origin at a different URL, so a fixture can be built over a cheap file
// path and then exercised across the simulator.
func setRemoteURL(tb testing.TB, repo *git.Repository, url string) {
	tb.Helper()

	cfg, err := repo.Config()
	require.NoError(tb, err)

	remote, ok := cfg.Remotes["origin"]
	require.True(tb, ok, "origin remote must exist")
	remote.URLs = []string{url}

	require.NoError(tb, repo.SetConfig(cfg))
}

// revParse returns the hash a ref points at in an on-disk repository, read with canonical git so
// the assertion does not depend on the library under test.
func revParse(tb testing.TB, repoDir, ref string) string {
	tb.Helper()

	out, err := exec.Command("git", "-C", repoDir, "rev-parse", ref).Output()
	require.NoError(tb, err, "git rev-parse %s", ref)

	return strings.TrimSpace(string(out))
}

// TestADOSimulator_IsFaithful guards the harness itself. It asserts the two properties the rest of
// this file depends on: a request without multi_ack is rejected exactly as ADO rejects it, and
// canonical git — which does advertise multi_ack — is served normally. If this test fails, the other
// results in this file mean nothing.
func TestADOSimulator_IsFaithful(t *testing.T) {
	projectRoot, repoDir := newADORepo(t)
	sim := startADOSimulator(t, projectRoot, repoDir)

	t.Run("an upload-pack request without multi_ack is rejected with TF401041", func(t *testing.T) {
		body := "0032want 0000000000000000000000000000000000000000\n00000009done\n"
		resp, err := http.Post(
			sim.RepoURL+"/git-upload-pack",
			"application/x-git-upload-pack-request",
			strings.NewReader(body),
		)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		payload, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode,
			"the simulator must reject a request that omits multi_ack")
		assert.Contains(t, string(payload), "TF401041",
			"the rejection must carry Azure DevOps' error code")
	})

	t.Run("canonical git clones successfully because it advertises multi_ack", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "clone")
		cmd := exec.Command("git", "-c", "protocol.version=0", "clone", sim.RepoURL, dest)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "canonical git must be able to clone: %s", out)

		assert.FileExists(t, filepath.Join(dest, "seed.yaml"),
			"the clone must have retrieved the seeded commit")
	})
}

// TestADO_CheckRepo_NeedsNoNegotiation encodes the first row of the capability matrix: CheckRepo
// reads only the ref advertisement (GET /info/refs) and never posts a want/have negotiation, so
// ADO's multi_ack rule cannot reach it.
//
// This passes on go-git v5, which is the point: PR #292 routes CheckRepo through system git
// unnecessarily.
func TestADO_CheckRepo_NeedsNoNegotiation(t *testing.T) {
	projectRoot, repoDir := newADORepo(t)
	sim := startADOSimulator(t, projectRoot, repoDir)

	info, err := CheckRepo(context.Background(), sim.RepoURL, nil)
	require.NoError(t, err, "CheckRepo must not need multi_ack: it only reads the advertisement")

	require.NotNil(t, info.DefaultBranch)
	assert.Equal(t, "main", info.DefaultBranch.ShortName)
	assert.Equal(t, 1, info.RemoteBranchCount)

	assert.Zero(t, sim.uploadPackPosts.Load(),
		"CheckRepo must not POST to git-upload-pack at all")
}

// TestADO_PushAtomic_NeedsNoMultiAck encodes the last row of the capability matrix: our atomic push
// speaks receive-pack, and multi_ack does not exist in that protocol. Measured advertisement from
// canonical git's receive-pack:
//
//	report-status report-status-v2 delete-refs side-band-64k quiet atomic ofs-delta object-format=sha1
//
// So the push is unaffected by ADO's rule, and by every fix for it. This passes on go-git v5 and
// must keep passing on v6: it is the regression guard on the single-session advertise-then-push with
// the server-side Old/New compare-and-swap.
func TestADO_PushAtomic_NeedsNoMultiAck(t *testing.T) {
	projectRoot, repoDir := newADORepo(t)
	sim := startADOSimulator(t, projectRoot, repoDir)

	// Work from a local clone made over the file path, so getting the fixture in place does not
	// depend on the fetch path that is broken on v5.
	local := filepath.Join(t.TempDir(), "work")
	repo, worktree := initLocalRepo(t, local, repoDir, "main")

	rootHash, err := repo.ResolveRevision(plumbing.Revision("refs/heads/main"))
	require.NoError(t, err)

	// Point origin at the simulator, so the push crosses the enforcing proxy.
	setRemoteURL(t, repo, sim.RepoURL)

	newHash := commitFileChange(t, worktree, local, "pushed.yaml", "kind: Pushed\n")

	err = PushAtomic(context.Background(), repo, *rootHash, plumbing.ReferenceName("refs/heads/main"), nil)
	require.NoError(t, err, "receive-pack has no multi_ack, so the push must succeed")

	assert.Zero(t, sim.uploadPackPosts.Load(), "a push must not touch git-upload-pack")

	// Confirm the remote actually moved to our commit.
	remoteHash := revParse(t, repoDir, "refs/heads/main")
	assert.Equal(t, newHash.String(), remoteHash, "the remote must be at the pushed commit")
}

// TestADO_SmartFetch_RequiresMultiAck is the red-first test for the migration.
//
// It asserts the behaviour we want: a fetch from an Azure DevOps-style remote succeeds. On go-git
// v5 it FAILS, because MultiACK and MultiACKDetailed sit in transport.UnsupportedCapabilities and
// are stripped from the advertisement before packp.NewUploadPackRequestFromCapabilities decides
// what to ask for, so the want line omits multi_ack and the simulator answers 400 exactly as ADO
// does. On go-git v6 it passes, because PR #1204 implements the capability.
//
// See https://github.com/go-git/go-git/pull/1204 and docs/design/azure-devops-multi-ack.md.
func TestADO_SmartFetch_RequiresMultiAck(t *testing.T) {
	projectRoot, repoDir := newADORepo(t)
	sim := startADOSimulator(t, projectRoot, repoDir)

	// A repository that already has objects, so the fetch sends have lines and a real negotiation
	// happens. This is the case Flux avoids by only ever cloning.
	local := filepath.Join(t.TempDir(), "work")
	repo, _ := initLocalRepo(t, local, repoDir, "main")
	setRemoteURL(t, repo, sim.RepoURL)

	// Move the remote on, so there is something to fetch.
	simulateClientCommitOnDisk(t, repoDir, "main", "second.yaml", "kind: Second\n")
	wantHash := revParse(t, repoDir, "refs/heads/main")

	branch, err := SmartFetch(
		context.Background(), repo, plumbing.ReferenceName("refs/heads/main"), nil)
	require.NoError(t, err,
		"fetch from an ADO-style remote must succeed; on go-git v5 this fails with %q",
		adoRejectionBody)

	assert.Equal(t, "refs/heads/main", branch.String())

	// The fetch must have actually advanced the remote-tracking ref, not merely not errored.
	ref, err := repo.Reference(plumbing.ReferenceName("refs/remotes/origin/main"), true)
	require.NoError(t, err)
	assert.Equal(t, wantHash, ref.Hash().String(), "the fetch must have retrieved the new commit")

	assert.Positive(t, sim.uploadPackPosts.Load(),
		"the test is meaningless unless a real negotiation happened")
	assert.Zero(t, sim.rejected.Load(),
		"the request must have offered multi_ack rather than being rejected")
}
