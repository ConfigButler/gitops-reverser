# GitHub Setup Guide

This guide covers the simplest GitHub path for the chart's starter configuration:

- a `git-creds` Secret
- a starter `GitProvider`
- a starter `GitTarget`
- a starter `WatchRule`

The root [`README.md`](../README.md) already covers the full operator install. This guide starts
after GitOps Reverser is running and kube-apiserver audit delivery is configured.

## Assumptions

- GitOps Reverser is installed
- the audit webhook is already wired into kube-apiserver
- you will use the chart `quickstart` path
- the quickstart namespace is `default` unless you override it

## Recommended path: SSH deploy key

### 1. Create a GitHub repository

Use either the web UI or GitHub CLI:

```bash
gh repo create my-k8s-audit --private --description "Kubernetes cluster audit trail"
```

The repository URL should look like:

```text
git@github.com:<owner>/my-k8s-audit.git
```

### 2. Generate an SSH keypair

```bash
ssh-keygen -t ed25519 -C "gitops-reverser@cluster" -f /tmp/gitops-reverser-key -N ""
cat /tmp/gitops-reverser-key.pub
```

Add the public key as a **deploy key with write access** on the repository.

With GitHub CLI:

```bash
gh repo deploy-key add /tmp/gitops-reverser-key.pub \
  --repo <owner>/my-k8s-audit \
  --title gitops-reverser \
  --allow-write
```

If the repository belongs to a GitHub organization, this may fail even when the command is correct.
Some organizations disable repository deploy keys or only allow certain users to manage them. Keep
reading if that happens.

### 3. Create the Kubernetes Secret

Create the Secret in the same namespace that will hold the quickstart `GitProvider`.

Default namespace example:

```bash
kubectl -n default create secret generic git-creds \
  --from-file=ssh-privatekey=/tmp/gitops-reverser-key \
  --from-literal=known_hosts="$(ssh-keyscan github.com 2>/dev/null)" \
  --dry-run=client -o yaml | kubectl apply -f -
```

If you set `quickstart.namespace`, use that namespace instead of `default`.

### 4. Enable the chart quickstart

```bash
helm upgrade gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser \
  --reuse-values \
  --set quickstart.enabled=true \
  --set quickstart.namespace=default \
  --set quickstart.gitProvider.url=git@github.com:<owner>/my-k8s-audit.git
```

### 5. Verify the starter resources

```bash
kubectl get gitprovider,gittarget,watchrule -n default
kubectl describe gitprovider example-provider -n default
```

When the resources are ready, create a test object:

```bash
kubectl create configmap github-setup-test --from-literal=key=value -n default
```

You should see a new commit appear in GitHub within seconds.

## HTTPS fallback: PAT or machine user

If your organization blocks deploy keys, use HTTPS credentials instead.

Create a Secret with `username` and `password` keys:

```bash
kubectl -n default create secret generic git-creds \
  --from-literal=username=<github-username-or-bot> \
  --from-literal=password=<token> \
  --dry-run=client -o yaml | kubectl apply -f -
```

Then point the quickstart `GitProvider` at an HTTPS repository URL:

```bash
helm upgrade gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser \
  --reuse-values \
  --set quickstart.enabled=true \
  --set quickstart.namespace=default \
  --set quickstart.gitProvider.url=https://github.com/<owner>/my-k8s-audit.git
```

## FAQ

### Our organization disables deploy keys

This is common on GitHub organizations.

If GitHub rejects the deploy key even though the key itself is valid, check whether the
organization allows repository deploy keys at all. In the GitHub UI this is usually controlled by
an organization admin under:

- `Organization Settings`
- `Member privileges`
- `Repository administration`

If deploy keys are blocked by policy, use the HTTPS/PAT path above instead. A dedicated machine user
or bot account is usually the cleanest long-term option.

### Our organization allows deploy keys, but I still cannot add one

That usually means the organization allows deploy keys in principle, but your account does not have
permission to manage them on that repository.

Ask an organization admin or repo admin to:

- add the deploy key for you
- grant the needed repository administration permission
- or approve the HTTPS/PAT fallback instead

### Should the deploy key have write access?

Yes. GitOps Reverser needs to push commits, so the deploy key must be added with write access.

## Troubleshooting

### `GitProvider` is not ready

Start with:

```bash
kubectl describe gitprovider example-provider -n default
kubectl logs -n gitops-reverser deploy/gitops-reverser
```

Common causes:

- wrong repository URL
- `git-creds` Secret is in the wrong namespace
- deploy key does not have write access
- PAT or password is invalid

### SSH authentication fails

Check the Secret contents:

```bash
kubectl get secret git-creds -n default -o yaml
```

Make sure it contains:

- `ssh-privatekey`
- `known_hosts` for GitHub

### No commits appear

Check all three quickstart resources:

```bash
kubectl get gitprovider,gittarget,watchrule -n default
kubectl describe gittarget example-target -n default
kubectl describe watchrule example-watchrule -n default
```

If the operator is healthy and the Git resources are ready, create a new test `ConfigMap` rather
than updating an old one so you get a fresh audit event.

## Related docs

- [`../README.md`](../README.md)
- [`configuration.md`](configuration.md)
- [`sops-age-guide.md`](sops-age-guide.md)
