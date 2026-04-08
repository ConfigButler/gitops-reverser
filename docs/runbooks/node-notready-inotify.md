# Runbook: k3d Agent Nodes Stuck in NotReady — inotify Exhaustion

## Summary

One or more k3d agent nodes become `NotReady` after a Docker daemon restart, and a
`docker restart` of the affected container does not fix the problem. The node stays
stuck with `NodeStatusUnknown` and the kubelet never starts.

---

## Symptoms

```
kubectl get nodes
NAME                                    STATUS     ROLES           AGE   VERSION
k3d-gitops-reverser-test-e2e-agent-1    NotReady   <none>          14d   v1.35.2+k3s1
k3d-gitops-reverser-test-e2e-agent-2    NotReady   <none>          14d   v1.35.2+k3s1
```

`kubectl describe node <agent>` shows:

```
Reason: NodeStatusUnknown
Message: Kubelet stopped posting node status.
```

`docker logs <agent>` shows a tight loop of:

```
couldn't get current server API group list:
  Get "http://localhost:8080/api?timeout=32s": dial tcp [::1]:8080: connect: connection refused
```

`docker exec <agent> ps aux` shows **no k3s process** — only the entrypoint shell and a
`sleep 3` from the uncordon retry loop.

---

## Root Cause

### 1. Immediate cause — inotify instance limit exhausted

The kubelet inside the agent container runs **cAdvisor**, which calls `inotify_init()`
to set up filesystem watches for container metrics. When the host kernel's
`fs.inotify.max_user_instances` limit is exhausted, `inotify_init()` returns
`EMFILE ("too many open files")` and cAdvisor (and therefore the kubelet) fails to
start:

```
E kubelet.go:1728] "Failed to start cAdvisor" err="inotify_init: too many open files"
```

The default limit on many Linux hosts is **128 instances**, which is far too low
for this workload.

### 2. Why the entrypoint gets stuck

The k3d entrypoint (`/bin/k3d-entrypoint.sh`) follows this sequence:

```sh
/bin/k3s "$@" &          # start k3s/kubelet in background
k3s_pid=$!

until kubectl uncordon "$HOSTNAME"; do sleep 3; done   # ← hangs here
```

When k3s fails to start (due to the cAdvisor/inotify error), it exits immediately.
The `until kubectl uncordon` loop then runs indefinitely because:

- There is no kubeconfig inside the agent container — `kubectl` falls back to
  `localhost:8080`, which does not exist inside the agent.
- The loop condition never becomes true, so the container appears running but
  produces nothing but the `localhost:8080` connection-refused flood.

### 3. Contributing factors — why the limit was reached

The host `fs.inotify.max_user_instances` (default **128**) is a **per-user kernel
limit shared across all containers** on the Docker host. The following concurrent
workloads each consume inotify instances:

| Workload | inotify consumers |
|---|---|
| VSCode devcontainer #1 (`gitops-reverser`) | VS Code server, language servers (gopls, etc.), file watchers |
| VSCode devcontainer #2 (`talks`) | Same: VS Code server, language servers, file watchers |
| k3d server node | kubelet + cAdvisor |
| k3d agent-0 | kubelet + cAdvisor + all running pods |
| k3d agent-1 | kubelet + cAdvisor |
| k3d agent-2 | kubelet + cAdvisor |

With two VSCode devcontainers open simultaneously (each running gopls and multiple
VS Code extensions), the 128-instance limit was exhausted before the agent kubelet
could allocate its own instance.

### 4. Memory

Memory pressure was **not** the direct cause. At the time of investigation:

- Total: 15 GiB, Used: ~10 GiB, Available: ~4.6 GiB, Swap: none
- `agent-0` alone held ~6 GiB (running all cluster workloads)
- The two devcontainers held ~2.5 GiB combined

While the system is running low on headroom (no swap configured), memory did not
trigger the failure. However, if agent-1 and agent-2 recover and start scheduling
pods again, available memory could become a secondary concern.

---

## Fix

### Immediate (ephemeral — lost on host reboot)

Run a privileged container against the Docker host to raise the limits:

```bash
docker run --rm --privileged alpine \
  sysctl -w fs.inotify.max_user_watches=524288 \
              fs.inotify.max_user_instances=512
```

Then restart the affected agent(s):

```bash
docker restart k3d-gitops-reverser-test-e2e-agent-1 \
               k3d-gitops-reverser-test-e2e-agent-2
```

### Permanent (survives host reboot)

Add to `/etc/sysctl.conf` (or `/etc/sysctl.d/99-inotify.conf`) **on the Docker host
machine**:

```
fs.inotify.max_user_watches=524288
fs.inotify.max_user_instances=512
```

Then apply: `sudo sysctl --system`

---

## Prevention (inotify)

- Keep the permanent sysctl settings above on the Docker host.
- Be aware that opening a second VSCode devcontainer against any large repo will
  meaningfully increase inotify consumption — this is what pushed the system over
  the edge here.
- If the Docker host is a shared machine or a CI runner, consider adding these
  settings as part of the host bootstrap/provisioning script.
