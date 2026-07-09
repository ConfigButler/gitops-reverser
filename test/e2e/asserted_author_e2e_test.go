// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// CommitRequest.spec.author lets a trusted control plane state who a commit is for, instead
// of deriving it from an apiserver audit fact. Asserting an author is a privilege: it is
// honored only when the requester holds the `assert-author` verb on the named GitTarget.
//
// Two properties matter, and both need a real API server with the admission webhook:
//
//  1. An UNAUTHORIZED assertion is denied at admission, with a message naming the RBAC rule
//     that would grant it.
//  2. An AUTHORIZED assertion becomes the commit's author signature — while the committer
//     stays the operator's configured identity, so a reader can always tell the reverser
//     committed on someone's behalf.
//
// The controller, not the webhook, is the real gate (the webhook is failurePolicy: Ignore),
// but the webhook's denial is what an authorized caller sees first, so both are asserted.
//
// Serial-safe by construction: the spec owns its own repo, GitTarget, and namespace.
var _ = Describe("Asserted commit author", Label("commit-request", "audit-consumer"), Ordered, func() {
	var (
		testNs        string
		repo          *RepoArtifacts
		provider      string
		target        string
		watchRule     string
		basePath      string
		asserterUser  string
		unprivileged  string
		bindingPrefix string
	)

	const (
		// Long enough that the silence timer cannot be what produces the commit.
		commitWindow  = "300s"
		assertedName  = "Ada Lovelace"
		assertedEmail = "ada@example.com"
	)

	BeforeAll(func() {
		By("creating the asserted-author namespace and Git credentials")
		testNs = testNamespaceFor("asserted-author")
		_, _ = kubectlRun("create", "namespace", testNs)
		repo = SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("e2e-asserted-author-%d", GinkgoRandomSeed()))
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to namespace")
		applySOPSAgeKeyToNamespace(testNs)

		seed := GinkgoRandomSeed()
		provider = fmt.Sprintf("asserted-author-provider-%d", seed)
		target = fmt.Sprintf("asserted-author-target-%d", seed)
		watchRule = fmt.Sprintf("asserted-author-rule-%d", seed)
		basePath = "e2e/asserted-author"
		asserterUser = fmt.Sprintf("gitops-api-%d@e2e", seed)
		unprivileged = fmt.Sprintf("nobody-%d@e2e", seed)
		bindingPrefix = fmt.Sprintf("asserted-author-%d", seed)

		createGitProviderWithCommitWindow(provider, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP, commitWindow)
		verifyResourceStatus("gitprovider", provider, testNs, "True", "Ready", "Repository connectivity validated")

		createGitTarget(target, testNs, provider, basePath, "main")
		verifyResourceCondition("gittarget", target, testNs, "Validated", "True", "OK", "")

		rule := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: %s
  namespace: %s
spec:
  targetRef:
    kind: GitTarget
    name: %s
  rules:
    - resources: ["configmaps"]
`, watchRule, testNs, target)
		_, err = kubectlRunWithStdin(testNs, rule, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred(), "failed to apply WatchRule")
		verifyResourceStatus("watchrule", watchRule, testNs, "True", "Ready", "")
		waitForStreamsRunning(target, testNs)

		By("granting only asserterUser the assert-author verb, scoped to this one GitTarget")
		// Both users may create CommitRequests; only one may assert an author. The grant is
		// resourceNames-scoped, exactly as the documented example is.
		rbac := fmt.Sprintf(`apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: %s-commit
  namespace: %s
rules:
  - apiGroups: ["configbutler.ai"]
    resources: ["commitrequests"]
    verbs: ["create", "get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: %s-commit
  namespace: %s
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: %s-commit
subjects:
  - kind: User
    name: %s
    apiGroup: rbac.authorization.k8s.io
  - kind: User
    name: %s
    apiGroup: rbac.authorization.k8s.io
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: %s-assert
  namespace: %s
rules:
  - apiGroups: ["configbutler.ai"]
    resources: ["gittargets"]
    resourceNames: ["%s"]
    verbs: ["assert-author"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: %s-assert
  namespace: %s
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: %s-assert
subjects:
  - kind: User
    name: %s
    apiGroup: rbac.authorization.k8s.io
`,
			bindingPrefix, testNs,
			bindingPrefix, testNs, bindingPrefix, asserterUser, unprivileged,
			bindingPrefix, testNs, target,
			bindingPrefix, testNs, bindingPrefix, asserterUser)
		_, err = kubectlRunWithStdin(testNs, rbac, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred(), "failed to apply assert-author RBAC")
	})

	AfterAll(func() {
		cleanupWatchRule(watchRule, testNs)
		cleanupGitTarget(target, testNs)
		cleanupNamespace(testNs)
	})

	// assertingCommitRequest renders a CommitRequest that asserts an author.
	assertingCommitRequest := func(name string) string {
		return fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: CommitRequest
metadata:
  name: %s
  namespace: %s
spec:
  targetRef:
    name: %s
  message: "save: asserted author"
  author:
    name: %q
    email: %q
`, name, testNs, target, assertedName, assertedEmail)
	}

	It("denies an unauthorized assertion, and names the RBAC rule that would grant it", func() {
		// create, not apply: apply would need patch/update on an object that never persists,
		// and the point here is the admission denial, not the client-side merge.
		out, err := kubectlRunWithStdin(testNs, assertingCommitRequest("denied-assertion"),
			"create", "--as="+unprivileged, "-f", "-")

		Expect(err).To(HaveOccurred(), "a user without assert-author must not be able to set spec.author")
		Expect(out).To(ContainSubstring("assert-author"))
		Expect(out).To(ContainSubstring(target), "the denial must name the GitTarget the verb is checked against")
		Expect(out).To(ContainSubstring("resourceNames"),
			"the denial must show the RBAC rule that would grant the privilege, not just refuse")
	})

	It("lets an authorized caller name the commit author, with the operator as committer", func() {
		const cmName = "asserted-author-configmap"
		const crName = "asserted-author-save"

		By("an edit opens a commit window that the long commitWindow will not close on its own")
		_, err := kubectlRunInNamespace(testNs, "create", "configmap", cmName, "--from-literal=flavor=vanilla")
		Expect(err).NotTo(HaveOccurred())

		By("the authorized caller creates a CommitRequest asserting the author")
		_, err = kubectlRunWithStdin(testNs, assertingCommitRequest(crName), "create", "--as="+asserterUser, "-f", "-")
		Expect(err).NotTo(HaveOccurred(), "an authorized assertion must be admitted")

		By("the request reports AuthorAttributed=True with reason AuthorAsserted")
		Eventually(func(g Gomega) {
			g.Expect(commitRequestCondition(g, testNs, crName, "Ready")).To(Equal("True"))
			g.Expect(commitRequestCondition(g, testNs, crName, "Pushed")).To(Equal("True"))

			reason, readErr := kubectlRunInNamespace(testNs, "get", "commitrequest", crName, "-o",
				`jsonpath={.status.conditions[?(@.type=="AuthorAttributed")].reason}`)
			g.Expect(readErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(reason)).To(Equal("AuthorAsserted"))

			status, readErr := kubectlRunInNamespace(testNs, "get", "commitrequest", crName, "-o",
				`jsonpath={.status.conditions[?(@.type=="AuthorAttributed")].status}`)
			g.Expect(readErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(status)).To(Equal("True"))
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("the commit is authored by the asserted identity, and committed by the operator")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)

			author, logErr := gitRun(repo.CheckoutDir, "log", "-1", "--pretty=%an <%ae>")
			g.Expect(logErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(author)).To(Equal(fmt.Sprintf("%s <%s>", assertedName, assertedEmail)))

			committer, logErr := gitRun(repo.CheckoutDir, "log", "-1", "--pretty=%cn")
			g.Expect(logErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(committer)).NotTo(Equal(assertedName),
				"the committer must stay the operator: a reader can always tell it committed on someone's behalf")
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", cmName, "--ignore-not-found=true")
		_, _ = kubectlRunInNamespace(testNs, "delete", "commitrequest", crName, "--ignore-not-found=true")
	})
})
