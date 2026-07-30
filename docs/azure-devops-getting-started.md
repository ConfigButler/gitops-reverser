# Getting started with Azure DevOps

Mirror a live cluster into an Azure DevOps Git repository. Nothing here is ADO-specific except the
credential shape, which ADO gets wrong in a way worth spelling out.

Prerequisites: the operator installed (see the [root README](../README.md)), and an ADO repository you
are willing to write to.

## 1. A Personal Access Token

In ADO: **User settings → Personal access tokens → New Token**, and give it **Code (read & write)**.
Read alone is not enough — the operator writes.

## 2. The credentials Secret

ADO sends a PAT as HTTP basic auth with the token as the **password**, and ignores the username. So set
only `password`:

```bash
kubectl create secret generic ado-creds \
  --namespace my-namespace \
  --from-literal=password='<your PAT>'
```

> **This is the step people get wrong.** Setting `username` to an empty string looks equivalent and is
> not: an empty value is indistinguishable from an absent key, and a Secret carrying only an empty
> username used to be rejected outright with *"does not contain valid authentication data"*. Supplying
> just `password` is the form to use. A username with no password is still an error, because that one is
> a genuine mistake.

## 3. A GitProvider

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
    - main
```

```bash
kubectl wait --for=condition=Ready gitprovider/ado-provider -n my-namespace --timeout=60s
```

Ready here means the operator reached the repository's ref advertisement and read its metadata,
including which branch is the default.

## 4. A GitTarget and a WatchRule

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: ado-target
  namespace: my-namespace
spec:
  gitProviderRef:
    name: ado-provider
  branch: main
  path: clusters/my-cluster
---
apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: ado-rule
  namespace: my-namespace
spec:
  gitTargetRef:
    name: ado-target
  rules:
    - resources: ["configmaps"]
```

## 5. Watch it work

```bash
kubectl create configmap ado-demo -n my-namespace --from-literal=greeting=hello
```

A commit appears on `main` under `clusters/my-cluster/my-namespace/configmaps/ado-demo.yaml`.

## SSH instead of a PAT

Use the ADO SSH URL form and the usual keys:

```yaml
spec:
  url: ssh://git@ssh.dev.azure.com/v3/<org>/<project>/<repo>
```

The Secret needs `ssh-privatekey`, and `known_hosts` unless the controller runs with
`--insecure-allow-missing-known-hosts`. Get the host key with
`ssh-keyscan ssh.dev.azure.com`.

Microsoft Entra ID (OAuth) access tokens go under `bearerToken` instead of `password`.

## If fetches fail with HTTP 400

```text
TF401041: Clients must support multi-ack.
```

ADO rejects a Git fetch whose capability list omits `multi_ack` (and `multi_ack_detailed`). The Git
library the operator uses only implements that capability from v6, so releases before
[#297](https://github.com/ConfigButler/gitops-reverser/pull/297) cannot fetch from ADO at all and no
configuration will change that. Upgrade.

Two details make this error confusing while you are debugging it:

- **The connectivity check still passes.** `GitProvider` can reach `Ready` on an affected release,
  because the ref advertisement is a different request that never needed the capability. Only the
  fetch fails.
- **Pushes are unaffected too.** `multi_ack` does not exist in the push protocol, so a push can
  succeed on a release where every fetch fails.

## How this is tested

Three layers, because ADO cannot be reached from CI:

| Layer | What it proves | Needs a credential? |
|---|---|---|
| [`internal/git/ado_multiack_test.go`](../internal/git/ado_multiack_test.go) | ADO's rule reproduced locally, using canonical git behind a proxy that enforces it | no — runs in CI |
| [`internal/git/ado_live_test.go`](../internal/git/ado_live_test.go) | the library against a real ADO repository, including the branch-resolution order | yes |
| [`test/e2e/ado_e2e_test.go`](../test/e2e/ado_e2e_test.go) | the operator mirroring a live ConfigMap into a real ADO repository | yes |

The two credentialed layers are opt-in and skip themselves without configuration:

```bash
export E2E_ADO_REPO_URL='https://dev.azure.com/<org>/<project>/_git/<repo>'
export E2E_ADO_PAT='<your PAT>'
# optional: a second repository that stays empty, for the empty-repository case
export E2E_ADO_EMPTY_REPO_URL='https://dev.azure.com/<org>/<project>/_git/empty'

go test ./internal/git/ -run TestADOLive -v   # library level
task test-e2e-ado                             # operator level, needs a prepared e2e cluster
```

`E2E_ADO_REPO_URL` is written to and not cleaned up, so point it at a scratch repository.
The repository `E2E_ADO_EMPTY_REPO_URL` names must stay empty — nothing writes to it, and it is the
only way to cover the empty-repository contract repeatedly, since the main fixture seeds itself on
first run.

One of those tests is a canary rather than a regression test: `TestADOLive_StillRequiresMultiAck`
asserts Azure DevOps *still* rejects a fetch without the capability. If it ever fails, Microsoft has
fixed their end and the constraint behind all of this is gone — see
[`facts/azure-devops-multi-ack-requirement.md`](facts/azure-devops-multi-ack-requirement.md).

Background on the capability and why v6 was the fix:
[`design/azure-devops-multi-ack.md`](design/azure-devops-multi-ack.md).
