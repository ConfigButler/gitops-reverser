# Getting started with Azure DevOps

Mirror a cluster into an Azure DevOps repository. Only the credential differs from any other
provider, and it differs in a way that trips people up.

Prerequisites: the operator installed (see the [root README](../README.md)), and an ADO repository you
can write to. An empty repository is fine — the operator creates the branch.

## 1. A Personal Access Token

**User settings → Personal access tokens → New Token**, in the organization that owns the repository.
Scope **Code (read & write)**; read alone is not enough, because the operator pushes. Set an expiry
you are willing to rotate, and copy the token when it is shown — ADO never displays it again. The
token inherits its user's permissions, so that user needs write access to the repository.

## 2. Namespace and Secret

ADO sends a PAT as HTTP basic auth with the token as the **password** and ignores the username, so the
Secret carries `password` and no `username`:

```bash
kubectl create namespace my-namespace

kubectl create secret generic ado-creds \
  --namespace my-namespace \
  --from-literal=password='<your PAT>'
```

A `username` is optional here and ADO ignores whatever you set, so leaving it out is simplest. What
does not work is a `username` with no `password`: the password is what selects basic auth.

## 3. The resources

Replace `<branch>` with your repository's default branch (`main` on a new ADO repository), the same
value in both places:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitProvider
metadata:
  name: ado-provider
  namespace: my-namespace
spec:
  url: https://dev.azure.com/<org>/<project>/_git/<repo>
  secretRef:
    name: ado-creds
  allowedBranches:
    - <branch>
---
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: ado-target
  namespace: my-namespace
spec:
  providerRef:
    name: ado-provider
  branch: <branch>
  path: clusters/my-cluster
---
apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: ado-rule
  namespace: my-namespace
spec:
  targetRef:
    name: ado-target
  rules:
    - resources: ["configmaps"]
```

```bash
kubectl wait --for=condition=Ready gitprovider/ado-provider -n my-namespace --timeout=60s
```

## 4. Check it works

```bash
kubectl create configmap ado-demo -n my-namespace --from-literal=greeting=hello
```

A commit appears on `<branch>` under
`clusters/my-cluster/my-namespace/configmaps/ado-demo.yaml`. Commits are authored by the configured
committer; audit delivery is only needed to attribute them to the Kubernetes user who made the change
(see [configuration.md](configuration.md)).

## Other credentials

SSH (`ssh://git@ssh.dev.azure.com/v3/<org>/<project>/<repo>`) and Entra ID bearer tokens use the same
Secret keys as any other provider. **Neither is tested against ADO**, unlike the PAT path above; both
are expected to work, as they do for GitHub.

## If fetches fail with HTTP 400

```text
TF401041: Clients must support multi-ack.
```

ADO rejects fetches from clients that do not advertise `multi_ack`, which go-git only implements from
v6. Releases before [#297](https://github.com/ConfigButler/gitops-reverser/pull/297) cannot fetch from
ADO and no configuration changes that — upgrade.

Two things make this confusing to debug: `GitProvider` can still reach `Ready`, because the
connectivity check is a different request that never needed the capability, and pushes still work,
because the push protocol has no `multi_ack` at all. Details and sources:
[`facts/azure-devops-multi-ack-requirement.md`](facts/azure-devops-multi-ack-requirement.md).

## Testing against a real tenant

CI cannot reach ADO, so two layers are opt-in and skip themselves without configuration:

```bash
export E2E_ADO_REPO_URL='https://dev.azure.com/<org>/<project>/_git/<repo>'   # written to
export E2E_ADO_PAT='<your PAT>'
export E2E_ADO_EMPTY_REPO_URL='.../_git/empty'   # optional; must name a repository with no commits

go test ./internal/git/ -run TestADOLive -v   # library
task test-e2e-ado                             # operator, needs a prepared e2e cluster
```

`TestADOLive_StillRequiresMultiAck` is a canary: if it fails, Microsoft fixed their end.
[`internal/git/ado_multiack_test.go`](../internal/git/ado_multiack_test.go) covers the same rule in CI
with no credential.
