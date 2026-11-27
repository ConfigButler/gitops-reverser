# React promptly to Git pushes

This guide makes GitOps Reverser notice a push by somebody else within seconds, by pointing the Git
host's push webhook at it. It is for a platform administrator whose folder Flux or Argo CD also
applies, following the [bi-directional path](README.md#coordinate-changes-from-kubernetes-and-git).
It ends with a test push whose apply you watch reach the cluster.

## Why it matters

When someone pushes to a branch GitOps Reverser writes, the cluster runs the old content until the
coordinated apply runs. While it waits, more Kubernetes edits can pile up on top of a branch that
has already moved. A push webhook shortens that wait from minutes to seconds.

Without one, GitOps Reverser still finds the push, only later:

| How it finds out | When |
| --- | --- |
| A push webhook, which this guide sets up | Seconds after the push |
| Its own next push is rejected because the branch moved | At the next publication, which needs a Kubernetes edit to happen |
| The periodic refresh | Within `--git-refresh-interval` (`10m`) plus the 5-minute reconcile tick |

Flux's or Argo CD's own polling does not help here: in a coordinated folder GitOps Reverser decides
when the applier applies, so the applier waits for it.

**Skip this guide when nothing applies the folder.** If no applier applies the folder back to the
cluster, a push by somebody else is found at the next publication and replayed safely. There the
webhook brings two smaller gains. GitOps Reverser learns of the push sooner. And when the push lands
on an idle branch, the next publication is a little faster: the delivery fetches the new commit
ahead of time, so the publication's push is accepted on the first attempt, instead of being
rejected and then fetching and pushing again. (On a busy branch, the delivery publishes right away
and that push is rejected as before.)
[What the network saving measures](api-first-publication.md#what-the-network-saving-measures)
counts the requests.

### Two kinds of webhook

GitOps Reverser serves two unrelated webhook endpoints. Keep them apart when you read logs and
configure routes:

| | Push webhook (this guide) | Audit webhook ([author attribution](attribution-setup-guide.md)) |
| --- | --- | --- |
| Sent by | The Git host, on a push | The Kubernetes API server, on every request |
| Tells GitOps Reverser | The branch moved | Who made a change |
| Authenticated by | An HMAC signature or token over the body | A client certificate (mutual TLS) |
| Listener | Port `9292`, plain HTTP behind your TLS route | Port `9444`, TLS |

## Before you start

- A `GitProvider`, `GitBranch`, and `GitTarget` that already write the branch. A delivery wakes only
  a branch that some `GitTarget` writes.
- A way to expose an HTTP Service to your Git host over HTTPS: an Ingress controller or a Gateway,
  with a certificate the host trusts.
- Admin rights on the repository (or the organization) to add a webhook.

The examples use GitHub, a provider for `git@github.com:acme/platform.git` in namespace `team-a`, and
the chart installed as `gitops-reverser` in namespace `gitops-reverser`.

## 1. Turn on the receiver listener

The listener is off by default. Turn it on in the chart:

```bash
helm upgrade gitops-reverser oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser --reuse-values \
  --set servers.receiver.enabled=true
```

The chart passes `--receiver-bind-address=:9292` and adds a `gitops-reverser-receiver` Service on
port `80`. Check it:

```bash
kubectl -n gitops-reverser get service gitops-reverser-receiver
```

## 2. Expose it over HTTPS

The listener speaks plain HTTP, so put a route in front of it that terminates TLS, the same way
Flux's `webhook-receiver` is usually exposed. Every delivery path starts with `/git-push/`, so you
can route only that prefix. With an Ingress (adjust the class, host, and certificate to your
cluster):

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: gitops-reverser-receiver
  namespace: gitops-reverser
spec:
  ingressClassName: nginx
  tls:
    - hosts: [hooks.example.com]
      secretName: hooks-example-com-tls
  rules:
    - host: hooks.example.com
      http:
        paths:
          - path: /git-push/
            pathType: Prefix
            backend:
              service:
                name: gitops-reverser-receiver
                port:
                  number: 80
```

Exposing the route is safe: a request reaches nothing unless it carries a valid signature for a
path a `GitReceiver` publishes, and every other request gets the same `404`.

## 3. Create the token and the `GitReceiver`

Generate a long random token, store it under the key `token`, and create a `GitReceiver` beside it:

```bash
kubectl -n team-a create secret generic platform-receiver \
  --from-literal=token="$(openssl rand -hex 32)"
```

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitReceiver
metadata:
  name: platform
  namespace: team-a
spec:
  type: github              # github | gitlab | gitea | generic-hmac
  secretRef:
    name: platform-receiver
```

No `GitProvider` names the receiver. A verified delivery wakes every `GitProvider`, in any
namespace, whose `spec.url` names the repository the delivery reports. So one `GitReceiver` serves
a GitHub organization webhook or a GitHub App, which deliver for many repositories through one URL.
The `token` key is the one a Flux `Receiver` Secret uses, so you can copy an existing Secret over.

Read the webhook path the receiver publishes. `-o wide` adds the `PATH` column:

```bash
kubectl -n team-a get gitreceiver platform -o wide
```

`READY` is `True` once the token is readable and the listener is on. The path looks like
`/git-push/3f9a…c41e`: 64 hex characters derived from the token, so only whoever holds the token can
find it. While the Secret is missing, the path stays empty and the receiver looks again every
30 seconds.

## 4. Add the webhook on the Git host

On GitHub, open the repository's **Settings → Webhooks → Add webhook** (or the organization's, for
every repository at once) and fill in:

| Field | Value |
| --- | --- |
| Payload URL | `https://hooks.example.com` followed by the receiver's path |
| Content type | `application/json` (`application/x-www-form-urlencoded` also works) |
| Secret | The token from step 3 |
| Which events | Just the `push` event |
| Active | Checked |

GitHub sends a `ping` when you save. The receiver answers `202` and ignores it, so a green check
next to the ping in **Recent Deliveries** proves that the route and the secret both work.

For other hosts, use the same steps with the matching `spec.type`. The
[`GitReceiver` reference](configuration.md#gitreceiver) lists which header each type verifies and
which payload fields name the repository:

- **GitLab:** `type: gitlab`. Put the token in the webhook's **Secret token** field.
- **Gitea:** `type: gitea`. Put the token in the webhook's **Secret** field.
- **Azure DevOps** has no receiver type. GitOps Reverser finds Azure DevOps pushes through the
  fallback paths in
  [why it matters](#why-it-matters), with their latency. Prompt notification needs a relay of your
  own that re-signs each delivery as `generic-hmac`; no tested relay is documented yet.

## 5. Settle the applier's own webhook

Flux and Argo CD may already receive a push webhook for this repository. What to do with it
depends on the applier:

- **Flux:** remove the Flux `Receiver` for this repository. GitOps Reverser's receiver replaces it,
  and GitOps Reverser asks Flux to apply when the branch is ready. See
  [the Flux setup](bi-directional-flux.md).
- **Argo CD:** its `/api/webhook` can stay. With automated sync off, an Argo CD refresh only
  compares and applies nothing. See [the Argo CD setup](bi-directional-argocd.md).

## 6. Verify with a test push

Push a small change to the branch from Git, as a person would: edit a file in the folder and push
it, or commit through the host's web editor.

1. **The host delivered it.** In the webhook's **Recent Deliveries**, the push has response `202`.
2. **GitOps Reverser received it.** The `GitBranch` records when a delivery last reached it:

   ```bash
   kubectl -n team-a get gitbranch main -o jsonpath='{.status.lastPushNoticeAt}{"\n"}'
   ```

3. **It saw the new commit.** The `GitBranch`'s `COMMIT` column shows the commit you pushed:

   ```bash
   kubectl -n team-a get gitbranch main
   ```

4. **The applier applied it.** The applier claim's `OPERATION` column moves through `Requested` and
   `Running` to `Succeeded`, and `-o wide` shows the commit it applied:

   ```bash
   kubectl -n team-a get fluxclaim -o wide      # or: get argocdclaim -o wide
   ```

Then check the object you changed in the cluster. A `202` alone does not prove the apply: it only
says the delivery was verified.

## When a push is not noticed

Start at the host's delivery log, then the `GitBranch`, then the receiver's metric
`gitopsreverser_receiver_deliveries_total`, whose `outcome` label separates the causes:

| What you see | Cause | Fix |
| --- | --- | --- |
| The host reports a timeout or connection error | The route does not reach the receiver | Check the Ingress or Gateway, its certificate, and that the `gitops-reverser-receiver` Service has an endpoint |
| The host gets `404` | Wrong path or wrong secret; both answer `404` on purpose | The operator's log says which. Compare the URL with `kubectl get gitreceiver -o wide`, and the host's secret with the Secret's `token` |
| `404` right after a token change | Rotating the token moved the path | Update the webhook URL as well as its secret |
| `202`, but `lastPushNoticeAt` stays empty, and the metric counts `unknown_repository` | The delivery names the repository by a URL no `GitProvider` matches, such as an SSH host alias | Spell the provider's `spec.url` with the hostname the Git host uses, such as `github.com`. Scheme, user, `.git`, and letter case do not matter; the host and path do |
| `202`, metric counts `unknown_branch` | No `GitTarget` writes that branch through a matching provider | Expected for other branches. For yours, check that a `GitTarget` names the `GitBranch` |
| `lastPushNoticeAt` is set, but nothing is applied | The delivery arrived; the apply is the next step | Read the applier claim's `status.operation` and the `GitTarget`'s `ApplierClaimReady` condition |
| The host gets `429` or `413` | More than 8 deliveries at once, or a body over 2 MiB | Nothing to fix; the periodic refresh finds the push |
| `GitReceiver` `READY=False` | `SecretNotFound`, `SecretMalformed` (no `token` key), or `ListenerDisabled` | Fix the Secret, or [turn the listener on](#1-turn-on-the-receiver-listener) |

`unknown_repository` and `unknown_branch` are routine on an organization or GitHub App webhook,
which delivers for every repository and branch.

## Rotate the token

1. Write a new value into the Secret's `token` key.
2. Wait for the new `PATH` on the `GitReceiver`, published at its next reconcile (within about
   5 minutes). The old path stops serving at once, because every delivery reads the token afresh.
3. Update both the webhook URL and its secret on the host.

Pushes made between steps 2 and 3 are found by the periodic refresh. GitHub's **Redeliver** button
also sends a missed delivery again.

## Reference

- [`GitReceiver`](configuration.md#gitreceiver): fields, verification per host type, routing, and
  every response code.
- [Push receiver settings](configuration.md#push-receiver-settings): the listener flag and chart
  values.
- [Telling GitOps Reverser that somebody else pushed](bi-directional.md#telling-gitops-reverser-that-somebody-else-pushed):
  how a delivery feeds the apply decision.
- [Interpreting metrics](interpreting-metrics.md#apply-requests-and-the-push-receiver): the
  receiver and apply metrics, with alert queries.
