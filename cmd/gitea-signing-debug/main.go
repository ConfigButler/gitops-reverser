/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler
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

func main() {
	var (
		apiURL    = flag.String("gitea-url", envOr("GITEA_API_URL", "http://localhost:13000/api/v1"), "Gitea /api/v1 base URL")
		cloneBase = flag.String("gitea-clone-url", envOr("GITEA_CLONE_URL", ""), "override clone URL base (default: derived from --gitea-url)")
		adminU    = flag.String("admin-user", envOr("GITEA_ADMIN_USER", "giteaadmin"), "Gitea admin username")
		adminP    = flag.String("admin-pass", envOr("GITEA_ADMIN_PASS", "giteapassword123"), "Gitea admin password")
		userLogin = flag.String("user", fmt.Sprintf("sshdbg-%d", time.Now().Unix()), "per-run user login")
		repoName  = flag.String("repo", "", "per-run repo name (default: <user>-repo)")
		keepRepo  = flag.Bool("keep", false, "do not delete the repo/user at the end (useful for post-mortem)")
		trust     = flag.String("trust-model", "committer", "trust_model for the repo: default|collaborator|committer|collaboratorcommitter")
		logsNS    = flag.String("gitea-ns", "gitea-e2e", "kubectl namespace for Gitea pod log tail (empty disables)")
		logsSel   = flag.String("gitea-selector", "app.kubernetes.io/name=gitea", "kubectl label selector for Gitea pod")
		dbPath    = flag.String("gitea-db-path", "/data/gitea/gitea.db", "path to gitea.db inside the Gitea pod (for --flip-verified)")
		flipDB    = flag.Bool("flip-verified", false, "after the first verification query, UPDATE public_key SET verified=1 via kubectl exec sqlite3 and re-query")
		verifyWeb = flag.Bool("verify-web", true, "drive the Gitea web UI (login + POST /user/settings/keys?type=verify_ssh) to verify the SSH key")
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
		DBPath: *dbPath, FlipDB: *flipDB, VerifyWeb: *verifyWeb, Keep: *keepRepo,
	}
	if err := run(opts); err != nil {
		log.Fatalf("FAIL: %v", err)
	}
}

type runOpts struct {
	APIURL, CloneBase            string
	AdminUser, AdminPass         string
	UserLogin, RepoName          string
	TrustModel                   string
	LogsNS, LogsSelector         string
	DBPath                       string
	FlipDB, VerifyWeb, Keep      bool
}

func run(o runOpts) error {
	apiURL, cloneBase := o.APIURL, o.CloneBase
	adminUser, adminPass := o.AdminUser, o.AdminPass
	userLogin, repoName, trustModel := o.UserLogin, o.RepoName, o.TrustModel
	logsNS, logsSel := o.LogsNS, o.LogsSelector
	keep := o.Keep
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	admin := giteaclient.New(apiURL, adminUser, adminPass)
	email := userLogin + "@configbutler.test"

	step("1. ensure user %q", userLogin)
	user, err := admin.EnsureUser(ctx, userLogin, email)
	if err != nil {
		return fmt.Errorf("ensure user: %w", err)
	}
	fmt.Printf("   user id=%d email=%s password=%s\n", user.ID, user.Email, user.Password)

	step("2. create repo %q owned by %q (trust_model=%s)", repoName, user.Login, trustModel)
	repo, err := admin.CreateUserRepo(ctx, user.Login, repoName, true, trustModel)
	if err != nil {
		return fmt.Errorf("create repo: %w", err)
	}
	fmt.Printf("   repo id=%d clone=%s\n", repo.ID, repo.CloneURL)

	if !keep {
		defer func() {
			_ = admin.DeleteRepo(context.Background(), user.Login, repoName)
		}()
	}

	step("3. generate SSH signing keypair")
	privPEM, pubAuth, err := reverserGit.GenerateSSHSigningKeyPair(nil)
	if err != nil {
		return fmt.Errorf("generate keypair: %w", err)
	}
	workDir, err := os.MkdirTemp("", "gitea-signing-debug-*")
	if err != nil {
		return fmt.Errorf("tempdir: %w", err)
	}
	if !keep {
		defer os.RemoveAll(workDir)
	} else {
		fmt.Printf("   workdir (kept): %s\n", workDir)
	}
	privPath := filepath.Join(workDir, "id_sign")
	pubPath := privPath + ".pub"
	if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
		return fmt.Errorf("write privkey: %w", err)
	}
	if err := os.WriteFile(pubPath, append(pubAuth, '\n'), 0o644); err != nil {
		return fmt.Errorf("write pubkey: %w", err)
	}
	pubStr := strings.TrimSpace(string(pubAuth))
	fmt.Printf("   pubkey: %s\n", pubStr)

	step("4a. register pubkey via ADMIN endpoint POST /admin/users/%s/keys", user.Login)
	key, err := admin.RegisterUserKeyAsAdmin(ctx, user.Login, "debug-signing", pubStr)
	if err != nil {
		return fmt.Errorf("register key (admin): %w", err)
	}
	fmt.Printf("   key id=%d fingerprint=%s key_type=%q read_only=%v\n",
		key.ID, key.Fingerprint, key.KeyType, key.ReadOnly)
	userClient := giteaclient.New(apiURL, user.Login, user.Password)
	rawKey, err := userClient.GetKeyRaw(ctx, key.ID)
	if err != nil {
		fmt.Printf("   [warn] could not fetch raw key as user: %v\n", err)
	} else {
		fmt.Printf("   raw key JSON: %s\n", string(rawKey))
	}

	step("5. make SSH-signed commit (committer email = %s) and push", user.Email)
	cloneURL := fmt.Sprintf("%s/%s/%s.git", strings.TrimRight(cloneBase, "/"), user.Login, repoName)
	authURL := injectBasicAuth(cloneURL, user.Login, user.Password)
	commitSHA, err := makeSignedCommit(workDir, authURL, privPath, pubPath, user)
	if err != nil {
		return fmt.Errorf("signed commit: %w", err)
	}
	fmt.Printf("   commit sha=%s\n", commitSHA)

	step("6. query commit verification (tailing Gitea server logs around the call)")
	logsStart := time.Now().Add(-2 * time.Second)
	v, err := admin.GetCommitVerification(ctx, user.Login, repoName, commitSHA)
	if err != nil {
		return fmt.Errorf("get commit verification: %w", err)
	}
	printVerification("verify", v)
	time.Sleep(1 * time.Second) // let async log flush catch up
	if logsNS != "" {
		printGiteaLogs(logsNS, logsSel, logsStart)
	}

	if o.VerifyWeb {
		step("6b. log into Gitea web UI as %q and POST /user/settings/keys?type=verify_ssh", user.Login)
		token, err := userClient.GetVerificationToken(ctx)
		if err != nil {
			return fmt.Errorf("get verification token: %w", err)
		}
		fmt.Printf("   token: %s\n", token)
		armoredSig, err := signTokenWithSSHKeygen(workDir, privPath, token)
		if err != nil {
			return fmt.Errorf("ssh-keygen sign token: %w", err)
		}
		webBase := deriveCloneBase(apiURL)
		sess, err := giteaclient.NewWebSession(ctx, webBase, user.Login, user.Password, true)
		if err != nil {
			return fmt.Errorf("web login: %w", err)
		}
		if err := sess.VerifySSHKey(ctx, pubStr, key.Fingerprint, armoredSig); err != nil {
			return fmt.Errorf("verify ssh key via web: %w", err)
		}
		fmt.Println("   [verify-web] ok")
		v3, err := admin.GetCommitVerification(ctx, user.Login, repoName, commitSHA)
		if err != nil {
			return fmt.Errorf("get commit verification (post-web): %w", err)
		}
		printVerification("post-web-verify", v3)
		fmt.Println()
		fmt.Printf("   DIFF: pre  verified=%v reason=%q\n", v.Verified, v.Reason)
		fmt.Printf("         post verified=%v reason=%q signer=%s\n", v3.Verified, v3.Reason, signerString(v3.Signer))
	}

	if o.FlipDB {
		step("6b. UPDATE public_key SET verified=1 WHERE id=%d via kubectl exec sqlite3", key.ID)
		if err := flipVerifiedInDB(logsNS, logsSel, o.DBPath, key.ID); err != nil {
			fmt.Printf("   [flip-db] failed: %v\n", err)
		} else {
			fmt.Println("   [flip-db] ok")
			v3, err := admin.GetCommitVerification(ctx, user.Login, repoName, commitSHA)
			if err != nil {
				return fmt.Errorf("get commit verification (post-flip): %w", err)
			}
			printVerification("post-flip", v3)
			fmt.Println()
			fmt.Printf("   DIFF: pre  verified=%v reason=%q\n", v.Verified, v.Reason)
			fmt.Printf("         post verified=%v reason=%q signer=%s\n", v3.Verified, v3.Reason, signerString(v3.Signer))
		}
	}

	step("7. re-list user's keys and dump all fields (including key_type)")
	keys, err := userClient.ListUserKeys(ctx, user.Login)
	if err != nil {
		return fmt.Errorf("list user keys: %w", err)
	}
	for _, k := range keys {
		fmt.Printf("   key id=%d title=%q key_type=%q fingerprint=%s last_used=%s\n",
			k.ID, k.Title, k.KeyType, k.Fingerprint, k.LastUsedAt)
	}

	step("8. final verdict (re-querying commit API)")
	vFinal, err := admin.GetCommitVerification(ctx, user.Login, repoName, commitSHA)
	if err != nil {
		return fmt.Errorf("get commit verification (final): %w", err)
	}
	printVerification("final", vFinal)
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func step(format string, args ...any) {
	fmt.Println()
	fmt.Println("─── " + fmt.Sprintf(format, args...) + " ───")
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
	fmt.Printf("   [%s] verified=%v reason=%q signer=%s\n",
		label, v.Verified, v.Reason, signerString(v.Signer))
	if v.Signature != "" {
		sig := v.Signature
		if len(sig) > 80 {
			sig = sig[:80] + "..."
		}
		fmt.Printf("   [%s] signature=%s\n", label, strings.ReplaceAll(sig, "\n", "\\n"))
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
func makeSignedCommit(workDir, cloneURL, privPath, pubPath string, user *giteaclient.TestUser) (string, error) {
	repoDir := filepath.Join(workDir, "repo")
	if out, err := runCmd("", "git", "clone", cloneURL, repoDir); err != nil {
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
		if out, err := runCmd(repoDir, "git", "config", kv[0], kv[1]); err != nil {
			return "", fmt.Errorf("git config %s: %w (%s)", kv[0], err, out)
		}
	}

	// Touch a file so we always have a non-empty diff (Gitea init commit may
	// already exist; an empty commit would be fine too but this is simpler).
	f := filepath.Join(repoDir, "debug.txt")
	if err := os.WriteFile(f, []byte(fmt.Sprintf("ts=%d\n", time.Now().UnixNano())), 0o644); err != nil {
		return "", err
	}
	if out, err := runCmd(repoDir, "git", "add", "debug.txt"); err != nil {
		return "", fmt.Errorf("git add: %w (%s)", err, out)
	}
	// GIT_SSH_COMMAND not needed for signing (ssh-keygen path is local), but set
	// for push over HTTPS it's irrelevant.
	if out, err := runCmd(repoDir, "git", "commit", "-S", "-m", "debug: ssh-signed commit"); err != nil {
		return "", fmt.Errorf("git commit -S: %w (%s)", err, out)
	}
	if out, err := runCmd(repoDir, "git", "rev-parse", "HEAD"); err != nil {
		return "", fmt.Errorf("git rev-parse: %w (%s)", err, out)
	} else {
		sha := strings.TrimSpace(out)
		if out, err := runCmd(repoDir, "git", "push", "origin", "HEAD"); err != nil {
			return "", fmt.Errorf("git push: %w (%s)", err, out)
		}
		return sha, nil
	}
}

// signTokenWithSSHKeygen shells out to `ssh-keygen -Y sign -n gitea -f privkey`
// over the token and returns the armored signature.
func signTokenWithSSHKeygen(workDir, privPath, token string) (string, error) {
	tokenPath := filepath.Join(workDir, "token.txt")
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		return "", err
	}
	cmd := exec.Command("ssh-keygen", "-Y", "sign", "-n", "gitea", "-f", privPath, tokenPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ssh-keygen -Y sign: %w (%s)", err, out)
	}
	sigBytes, err := os.ReadFile(tokenPath + ".sig")
	if err != nil {
		return "", fmt.Errorf("read .sig: %w", err)
	}
	return string(sigBytes), nil
}

// flipVerifiedInDB shells out to `kubectl exec` on the first pod matching the
// selector and runs sqlite3 against the Gitea DB to flip public_key.verified=1
// for the given key id. This bypasses the UI-only verify flow so we can prove
// whether `verified` is the only gate on SSH commit verification.
func flipVerifiedInDB(namespace, selector, dbPath string, keyID int64) error {
	pod, err := runCmd("", "kubectl", "get", "pod",
		"-n", namespace, "-l", selector,
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		return fmt.Errorf("find gitea pod: %w (%s)", err, pod)
	}
	podName := strings.TrimSpace(pod)
	if podName == "" {
		return fmt.Errorf("no pod matched %s/%s", namespace, selector)
	}
	sql := fmt.Sprintf("UPDATE public_key SET verified=1 WHERE id=%d;", keyID)
	out, err := runCmd("", "kubectl", "exec", "-n", namespace, podName, "--",
		"sqlite3", dbPath, sql)
	if err != nil {
		return fmt.Errorf("sqlite3 UPDATE: %w (%s)", err, out)
	}
	return nil
}

// printGiteaLogs dumps Gitea pod logs emitted after `since`, highlighting
// lines that look related to signature/key verification. Non-fatal on failure.
func printGiteaLogs(namespace, selector string, since time.Time) {
	sinceStr := since.UTC().Format(time.RFC3339)
	out, err := runCmd("", "kubectl", "logs",
		"-n", namespace,
		"-l", selector,
		"--tail=-1",
		"--since-time="+sinceStr,
		"--prefix=true",
	)
	if err != nil {
		fmt.Printf("   [logs] kubectl logs failed: %v\n%s\n", err, out)
		return
	}
	fmt.Println("   ── Gitea pod logs since API call ──")
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
		fmt.Println("   (no signature/key/commit-related lines; dumping all)")
		fmt.Println(out)
		return
	}
	for _, ln := range interesting {
		fmt.Println("   " + ln)
	}
}

func runCmd(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}
