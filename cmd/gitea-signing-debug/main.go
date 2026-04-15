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

// gitea-signing-debug drives the full SSH-signed-commit flow against a live
// Gitea and reports Gitea's commit verification verdict at every stage.
//
// The goal is to isolate *which* step in the dance flips Gitea from
// `verified=false, reason=gpg.error.no_gpg_keys_found, signer=null` to a
// verified commit. The hypothesis this tool encodes: admin-uploaded SSH keys
// start with public_key.verified=false, and Gitea will refuse to use them for
// commit-signature verification until the key owner proves possession by
// signing a per-user token and POSTing it to /user/keys/verify.
//
// Flow:
//  1. Create (or rotate) a per-run Gitea user with a known password.
//  2. Create a repo owned by that user.
//  3. Generate an SSH signing keypair; upload the public half via admin.
//  4. Query commit verification BEFORE key verification (expected: fail).
//     (Actually: we make the commit first, then query both pre- and post-verify.)
//  5. Prepare a local git clone, make an SSH-signed commit authored with the
//     user's email, and push it over HTTPS.
//  6. Query commit verification BEFORE /user/keys/verify (expected: fail with
//     `no_gpg_keys_found`).
//  7. Sign the Gitea-issued token with `ssh-keygen -Y sign -n gitea` and POST
//     it to /user/keys/verify AS THE USER.
//  8. Re-query the key and commit verification (expected: verified=true).
//
// Usage:
//
//	go run ./cmd/gitea-signing-debug \
//	  --gitea-url http://localhost:13000/api/v1 \
//	  --admin-user giteaadmin --admin-pass giteapassword123
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	reverserGit "github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/giteaclient"
)

const (
	runTimeout            = 2 * time.Minute
	signaturePreviewLimit = 80
	restrictedFileMode    = 0o600
)

func main() {
	log.SetFlags(0)

	var (
		apiURL = flag.String(
			"gitea-url",
			envOr("GITEA_API_URL", "http://localhost:13000/api/v1"),
			"Gitea /api/v1 base URL",
		)
		cloneBase = flag.String(
			"gitea-clone-url",
			envOr("GITEA_CLONE_URL", ""),
			"override clone URL base (default: derived from --gitea-url)",
		)
		adminU    = flag.String("admin-user", envOr("GITEA_ADMIN_USER", "giteaadmin"), "Gitea admin username")
		adminP    = flag.String("admin-pass", envOr("GITEA_ADMIN_PASS", "giteapassword123"), "Gitea admin password")
		userLogin = flag.String("user", fmt.Sprintf("sshdbg-%d", time.Now().Unix()), "per-run user login")
		repoName  = flag.String("repo", "", "per-run repo name (default: <user>-repo)")
		keepRepo  = flag.Bool("keep", false, "do not delete the repo/user at the end (useful for post-mortem)")
		trust     = flag.String(
			"trust-model",
			"committer",
			"trust_model for the repo: default|collaborator|committer|collaboratorcommitter",
		)
		logsNS  = flag.String("gitea-ns", "gitea-e2e", "kubectl namespace for Gitea pod log tail (empty disables)")
		logsSel = flag.String(
			"gitea-selector",
			"app.kubernetes.io/name=gitea",
			"kubectl label selector for Gitea pod",
		)
		verifyWeb = flag.Bool(
			"verify-web",
			true,
			"drive the Gitea web UI (login + POST /user/settings/keys?type=verify_ssh) to verify the SSH key",
		)
	)
	flag.Parse()

	if *repoName == "" {
		*repoName = *userLogin + "-repo"
	}
	if *cloneBase == "" {
		*cloneBase = deriveCloneBase(*apiURL)
	}

	opts := runOpts{
		APIURL: *apiURL, CloneBase: *cloneBase,
		AdminUser: *adminU, AdminPass: *adminP,
		UserLogin: *userLogin, RepoName: *repoName, TrustModel: *trust,
		LogsNS: *logsNS, LogsSelector: *logsSel,
		VerifyWeb: *verifyWeb, Keep: *keepRepo,
	}
	if err := run(opts); err != nil {
		log.Fatalf("FAIL: %v", err)
	}
}

type runOpts struct {
	APIURL, CloneBase    string
	AdminUser, AdminPass string
	UserLogin, RepoName  string
	TrustModel           string
	LogsNS, LogsSelector string
	VerifyWeb, Keep      bool
}

//nolint:cyclop,funlen // This is a debug-only CLI that intentionally narrates each step of the flow.
func run(o runOpts) error {
	apiURL, cloneBase := o.APIURL, o.CloneBase
	adminUser, adminPass := o.AdminUser, o.AdminPass
	userLogin, repoName, trustModel := o.UserLogin, o.RepoName, o.TrustModel
	logsNS, logsSel := o.LogsNS, o.LogsSelector
	keep := o.Keep
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	admin := giteaclient.New(apiURL, adminUser, adminPass)
	email := userLogin + "@configbutler.test"

	stepf("1. ensure user %q", userLogin)
	user, err := admin.EnsureUser(ctx, userLogin, email)
	if err != nil {
		return fmt.Errorf("ensure user: %w", err)
	}
	writef("   user id=%d email=%s password=%s\n", user.ID, user.Email, user.Password)

	stepf("2. create repo %q owned by %q (trust_model=%s)", repoName, user.Login, trustModel)
	repo, err := admin.CreateUserRepo(ctx, user.Login, repoName, true, trustModel)
	if err != nil {
		return fmt.Errorf("create repo: %w", err)
	}
	writef("   repo id=%d clone=%s\n", repo.ID, repo.CloneURL)

	if !keep {
		defer func() {
			_ = admin.DeleteRepo(context.Background(), user.Login, repoName)
		}()
	}

	stepf("3. generate SSH signing keypair")
	workDir, privPath, pubPath, pubStr, err := writeSigningKeyPair(keep)
	if err != nil {
		return err
	}
	if !keep {
		defer os.RemoveAll(workDir)
	}
	writef("   pubkey: %s\n", pubStr)

	stepf("4a. register pubkey via ADMIN endpoint POST /admin/users/%s/keys", user.Login)
	key, err := admin.RegisterUserKeyAsAdmin(ctx, user.Login, "debug-signing", pubStr)
	if err != nil {
		return fmt.Errorf("register key (admin): %w", err)
	}
	writef("   key id=%d fingerprint=%s key_type=%q read_only=%v\n",
		key.ID, key.Fingerprint, key.KeyType, key.ReadOnly)
	userClient := giteaclient.New(apiURL, user.Login, user.Password)
	rawKey, err := userClient.GetKeyRaw(ctx, key.ID)
	if err != nil {
		writef("   [warn] could not fetch raw key as user: %v\n", err)
	} else {
		writef("   raw key JSON: %s\n", string(rawKey))
	}

	stepf("5. make SSH-signed commit (committer email = %s) and push", user.Email)
	cloneURL := fmt.Sprintf("%s/%s/%s.git", strings.TrimRight(cloneBase, "/"), user.Login, repoName)
	authURL := injectBasicAuth(cloneURL, user.Login, user.Password)
	commitSHA, err := makeSignedCommit(ctx, workDir, authURL, pubPath, user)
	if err != nil {
		return fmt.Errorf("signed commit: %w", err)
	}
	writef("   commit sha=%s\n", commitSHA)

	stepf("6. query commit verification (tailing Gitea server logs around the call)")
	logsStart := time.Now().Add(-2 * time.Second)
	v, err := admin.GetCommitVerification(ctx, user.Login, repoName, commitSHA)
	if err != nil {
		return fmt.Errorf("get commit verification: %w", err)
	}
	printVerification("verify", v)
	time.Sleep(1 * time.Second) // let async log flush catch up
	if logsNS != "" {
		printGiteaLogs(ctx, logsNS, logsSel, logsStart)
	}

	if o.VerifyWeb {
		stepf("6b. log into Gitea web UI as %q and POST /user/settings/keys?type=verify_ssh", user.Login)
		if err := verifyViaWeb(ctx, admin, apiURL, user, pubStr, key.Fingerprint, privPath); err != nil {
			return err
		}
		v3, err := admin.GetCommitVerification(ctx, user.Login, repoName, commitSHA)
		if err != nil {
			return fmt.Errorf("get commit verification (post-web): %w", err)
		}
		printVerification("post-web-verify", v3)
		writeln()
		writef("   DIFF: pre  verified=%v reason=%q\n", v.Verified, v.Reason)
		writef("         post verified=%v reason=%q signer=%s\n", v3.Verified, v3.Reason, signerString(v3.Signer))
	}

	stepf("7. re-list user's keys and dump all fields (including key_type)")
	keys, err := userClient.ListUserKeys(ctx, user.Login)
	if err != nil {
		return fmt.Errorf("list user keys: %w", err)
	}
	for _, k := range keys {
		writef("   key id=%d title=%q key_type=%q fingerprint=%s last_used=%s\n",
			k.ID, k.Title, k.KeyType, k.Fingerprint, k.LastUsedAt)
	}

	stepf("8. final verdict (re-querying commit API)")
	vFinal, err := admin.GetCommitVerification(ctx, user.Login, repoName, commitSHA)
	if err != nil {
		return fmt.Errorf("get commit verification (final): %w", err)
	}
	printVerification("final", vFinal)
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func stepf(format string, args ...any) {
	writeln()
	writef("─── %s ───\n", fmt.Sprintf(format, args...))
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// deriveCloneBase turns "http://host:port/api/v1" into "http://host:port".
func deriveCloneBase(apiURL string) string {
	trim := strings.TrimRight(apiURL, "/")
	trim = strings.TrimSuffix(trim, "/api/v1")
	return trim
}

// injectBasicAuth returns the URL with user:password embedded, so plain
// `git push` authenticates without an askpass helper.
func injectBasicAuth(rawURL, user, pass string) string {
	i := strings.Index(rawURL, "://")
	if i < 0 {
		return rawURL
	}
	return rawURL[:i+3] + user + ":" + pass + "@" + rawURL[i+3:]
}

func printVerification(label string, v *giteaclient.CommitVerification) {
	writef("   [%s] verified=%v reason=%q signer=%s\n",
		label, v.Verified, v.Reason, signerString(v.Signer))
	if v.Signature != "" {
		sig := v.Signature
		if len(sig) > signaturePreviewLimit {
			sig = sig[:signaturePreviewLimit] + "..."
		}
		writef("   [%s] signature=%s\n", label, strings.ReplaceAll(sig, "\n", "\\n"))
	}
}

func signerString(u *giteaclient.User) string {
	if u == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s <%s>", u.Login, u.Email)
}

// makeSignedCommit clones repo into workDir/repo, configures SSH signing, makes
// one signed empty-ish commit authored by the test user, and pushes. It returns
// the commit SHA.
func makeSignedCommit(
	ctx context.Context,
	workDir, cloneURL, pubPath string,
	user *giteaclient.TestUser,
) (string, error) {
	repoDir := filepath.Join(workDir, "repo")
	if out, err := runCmd(ctx, "", "git", "clone", cloneURL, repoDir); err != nil {
		return "", fmt.Errorf("git clone: %w (%s)", err, out)
	}

	cfg := [][]string{
		{"user.name", user.Login},
		{"user.email", user.Email},
		{"gpg.format", "ssh"},
		{"user.signingkey", pubPath},
		{"commit.gpgsign", "true"},
	}
	for _, kv := range cfg {
		if out, err := runCmd(ctx, repoDir, "git", "config", kv[0], kv[1]); err != nil {
			return "", fmt.Errorf("git config %s: %w (%s)", kv[0], err, out)
		}
	}

	// Touch a file so we always have a non-empty diff (Gitea init commit may
	// already exist; an empty commit would be fine too but this is simpler).
	f := filepath.Join(repoDir, "debug.txt")
	if err := os.WriteFile(f, []byte(fmt.Sprintf("ts=%d\n", time.Now().UnixNano())), restrictedFileMode); err != nil {
		return "", err
	}
	if out, err := runCmd(ctx, repoDir, "git", "add", "debug.txt"); err != nil {
		return "", fmt.Errorf("git add: %w (%s)", err, out)
	}
	// GIT_SSH_COMMAND not needed for signing (ssh-keygen path is local), but set
	// for push over HTTPS it's irrelevant.
	if out, err := runCmd(ctx, repoDir, "git", "commit", "-S", "-m", "debug: ssh-signed commit"); err != nil {
		return "", fmt.Errorf("git commit -S: %w (%s)", err, out)
	}
	out, err := runCmd(ctx, repoDir, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w (%s)", err, out)
	}

	sha := strings.TrimSpace(out)
	if out, err := runCmd(ctx, repoDir, "git", "push", "origin", "HEAD"); err != nil {
		return "", fmt.Errorf("git push: %w (%s)", err, out)
	}
	return sha, nil
}

// printGiteaLogs dumps Gitea pod logs emitted after `since`, highlighting
// lines that look related to signature/key verification. Non-fatal on failure.
func printGiteaLogs(ctx context.Context, namespace, selector string, since time.Time) {
	sinceStr := since.UTC().Format(time.RFC3339)
	out, err := runCmd(ctx, "", "kubectl", "logs",
		"-n", namespace,
		"-l", selector,
		"--tail=-1",
		"--since-time="+sinceStr,
		"--prefix=true",
	)
	if err != nil {
		writef("   [logs] kubectl logs failed: %v\n%s\n", err, out)
		return
	}
	writeln("   ── Gitea pod logs since API call ──")
	lines := strings.Split(out, "\n")
	interesting := []string{}
	for _, ln := range lines {
		low := strings.ToLower(ln)
		if strings.Contains(low, "sign") || strings.Contains(low, "gpg") ||
			strings.Contains(low, "ssh") || strings.Contains(low, "verif") ||
			strings.Contains(low, "asymkey") || strings.Contains(low, "fingerprint") ||
			strings.Contains(low, "key") || strings.Contains(low, "commit") {
			interesting = append(interesting, ln)
		}
	}
	if len(interesting) == 0 {
		writeln("   (no signature/key/commit-related lines; dumping all)")
		writeln(out)
		return
	}
	for _, ln := range interesting {
		writeln("   " + ln)
	}
}

func runCmd(ctx context.Context, dir, name string, args ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func verifyViaWeb(
	ctx context.Context,
	adminClient *giteaclient.Client,
	apiURL string,
	user *giteaclient.TestUser,
	pubKey, fingerprint, privPath string,
) error {
	result, err := adminClient.VerifySSHKey(ctx, user, giteaclient.SSHKeyVerificationOptions{
		PublicKey:      pubKey,
		Fingerprint:    fingerprint,
		PrivateKeyPath: privPath,
		Debug:          true,
	})
	if err != nil {
		return err
	}
	writef("   token: %s\n", result.Token)
	writef("   web base: %s\n", strings.TrimSuffix(strings.TrimRight(apiURL, "/"), "/api/v1"))

	writeln("   [verify-web] ok")
	return nil
}

func writeSigningKeyPair(keep bool) (string, string, string, string, error) {
	privPEM, pubAuth, err := reverserGit.GenerateSSHSigningKeyPair(nil)
	if err != nil {
		return "", "", "", "", fmt.Errorf("generate keypair: %w", err)
	}

	workDir, err := os.MkdirTemp("", "gitea-signing-debug-*")
	if err != nil {
		return "", "", "", "", fmt.Errorf("tempdir: %w", err)
	}
	if keep {
		writef("   workdir (kept): %s\n", workDir)
	}

	privPath := filepath.Join(workDir, "id_sign")
	pubPath := privPath + ".pub"
	if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
		return "", "", "", "", fmt.Errorf("write privkey: %w", err)
	}
	if err := os.WriteFile(pubPath, append(pubAuth, '\n'), restrictedFileMode); err != nil {
		return "", "", "", "", fmt.Errorf("write pubkey: %w", err)
	}

	return workDir, privPath, pubPath, strings.TrimSpace(string(pubAuth)), nil
}

func writef(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stdout, format, args...)
}

func writeln(parts ...any) {
	_, _ = fmt.Fprintln(os.Stdout, parts...)
}
