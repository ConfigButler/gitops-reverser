# Commit-window example

Two Kubernetes resources become one Git commit: one created, one edited. A `CommitRequest` supplies
the message and asks the operator to close the window before its normal timer expires. The request
itself is outside the selected resource types, so saving never commits the save button.

The example assumes a namespace named `demo`, a working `GitProvider` named `example-provider` in
that namespace, and an authorized `default` `ClusterProvider`. Use a branch no reconciler deploys.
See the [quickstart](../quickstart.md) for installation and Git credentials.

## Select the resources

Save as `watch.yaml`. All resources in this example use the same namespace and target.

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: window-demo
  namespace: demo
spec:
  gitProviderRef:
    name: example-provider
  branch: commit-window-demo
  path: apps/demo
  commit:
    window: "5s"
    message:
      requestTemplate: |-
        {{.RequestMessage}}

        {{range .Resources -}}
        {{.Kind}}/{{.Name}}
        {{end -}}
  placement:
    byType:
      v1/configmaps: "configmap-{name}.yaml"
      v1/serviceaccounts: "serviceaccount-{name}.yaml"
---
apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: window-demo
  namespace: demo
spec:
  gitTargetRef:
    name: window-demo
  rules:
    - apiGroups: [""]
      apiVersions: [v1]
      resources: [configmaps, serviceaccounts]
```

Omitting `operations` selects `CREATE`, `UPDATE`, and `DELETE` by default.

Two fields exist for the sake of a legible demo rather than because the window needs them:

- **`placement.byType`** puts both files directly under `apps/demo` as `configmap-hello.yaml` and
  `serviceaccount-hello.yaml`. Without it they land on the
  [built-in canonical path](../configuration.md#declaring-a-layout-bytype--default),
  `apps/demo/demo/configmaps/hello.yaml`.
- **`requestTemplate`** keeps the changed resources in the commit body underneath the requested
  message. A request message otherwise **replaces** the generated message entirely, so the commit
  would say why but no longer say what. See
  [framing a save message](../commit-messages.md#framing-a-save-message).
  `{{.Kind}}` is read off the object, and a `DELETE` event carries none, so a deletion in the window
  renders as `/hello`. Use `{{.Resource}}` where a window can contain one; the built-in live template
  does.

Apply this setup and let the initial synchronization finish before going on:

```bash
kubectl apply -f watch.yaml
kubectl get gittarget,watchrule -n demo
```

The rule selects **two resource types in `demo`**, not two individual names. Other ConfigMaps and
ServiceAccounts in that namespace are also eligible. Initial synchronization may capture existing
resources such as the `kube-root-ca.crt` ConfigMap and the `default` ServiceAccount. Keep other
writes to the target quiet while you run the example.

## Seed the ConfigMap

Create the ConfigMap on its own first, so the recorded batch below contains one `CREATE` and one
`UPDATE` rather than two creates. Let its commit land before going on:

```bash
kubectl create configmap hello --from-literal=message="Hello" -n demo
```

## Apply the resources

Save as `hello.yaml`. These demonstrate resource capture without deploying a workload. The
ServiceAccount is new; the ConfigMap changes the value seeded above.

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: hello
  namespace: demo
automountServiceAccountToken: false
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: hello
  namespace: demo
data:
  message: Hello from Reverse GitOps
```

## Close the window with a message

Save as `save-now.yaml`. Use `kubectl create` rather than `kubectl apply`: the request has a
`generateName` and no name, so each save is a fresh object and `apply` refuses it.

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: CommitRequest
metadata:
  generateName: save-hello-
  namespace: demo
spec:
  gitTargetRef:
    name: window-demo
  message: "feat(demo): save hello resources"
  closeDelaySeconds: 2
```

Run these commands together, creating the request only after the apply succeeds:

```bash
kubectl apply -f hello.yaml && kubectl create -f save-now.yaml
```

The request must reach a matching open window. If you are stepping through this with pauses between
commands, set the target's `commit.window` to `30s` first. `closeDelaySeconds: 2` gives watch events
time to reach the worker; it does not extend the normal window or guarantee that every event has
arrived. Change the ConfigMap's `message` value again on a repeat run, so there is something to
commit.

The default configured-author setup without request author capture works for this example. If actor
attribution and request author capture are enabled, submit the resources and the request as the same
Kubernetes user. See [CommitRequest](../configuration.md#commitrequest) for author matching and
outcomes.

## Check the result

```bash
kubectl get commitrequests -n demo -o wide
```

Look for `Pushed=True` and a `status.sha`. `Pushed` is a wide column; `Ready` and the commit SHA
show without `-o wide`. A terminal `Ready=True` on its own does not prove a commit was pushed: it is
also the outcome when there was nothing to save or no matching open window.

Applying several resources sends separate API writes. The window groups their captured changes into
one commit:

```text
feat(demo): save hello resources

ServiceAccount/hello
ConfigMap/hello
```

The commit adds `apps/demo/serviceaccount-hello.yaml` and modifies `apps/demo/configmap-hello.yaml`.

| Resource | Captured by this rule? |
|---|---|
| ServiceAccount, ConfigMap | Yes |
| CommitRequest, GitTarget, WatchRule | No |
| Services, Deployments, Pods, ReplicaSets | No |

Without the request, the window closes on its own five seconds after the last change, and the
built-in live template writes the message instead.
