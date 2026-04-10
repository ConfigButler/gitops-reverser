# Docker-outside-of-Docker: why we use it

The devcontainer uses DOOD (host Docker socket mount) rather than DinD intentionally.

The main benefit: k3d clusters created inside the container are visible on the host.
This means you can `kubectl` from your local machine against the e2e cluster — useful
for inspecting state, running one-off commands, and debugging without going through the
container every time. DinD isolates the daemon inside the container, so the cluster
disappears from the host's perspective.

DOOD is also more resource-efficient — no second Docker daemon running.

The earlier Kind-based setup had bootstrap flakiness with DOOD (see kubernetes-sigs/kind#2867).
Switching to k3d resolved that — k3d works reliably with a shared host socket.

## Remote dev server

The same property makes DOOD useful when the devcontainer runs on a remote machine (e.g. a
cloud VM or home server accessed over SSH). The k3d API port is published on `0.0.0.0` on
the remote host, so you can reach the cluster from your local laptop via an SSH tunnel:

```bash
hack/connect-remote-k3d.sh remote-dev.z65
export KUBECONFIG=~/.kube/remote-k3d.yaml
kubectl get nodes
```

**GitHub Codespaces:** port forwarding works differently there — use the VS Code port
forwarding panel or `gh codespace ports forward` instead of the SSH tunnel script.
