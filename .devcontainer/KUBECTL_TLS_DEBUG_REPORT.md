# kubectl TLS Debug Report (DevContainer)

Date: 2026-02-12  
Scope: Debug `kubectl` failures inside the VS Code devcontainer.

## Symptom

Inside devcontainer:

```bash
kubectl get nodes
```

Output:

```text
tls: failed to verify certificate: x509: certificate signed by unknown authority
```

## Commands Run and Results

### 1) Check kubectl context/config

```bash
kubectl config current-context
kubectl config get-contexts
kubectl config view --minify --raw
```

Result:
- Current context was `kind-gitops-reverser-test-e2e`
- Cluster server was `https://127.0.0.1:44431`

---

### 2) Attempt kubeconfig reset + re-export

```bash
kubectl config delete-context kind-gitops-reverser-test-e2e || true
kubectl config delete-cluster kind-gitops-reverser-test-e2e || true
kubectl config delete-user kind-gitops-reverser-test-e2e || true
kind export kubeconfig --name gitops-reverser-test-e2e
kubectl get nodes
```

Result:
- Context/cluster/user entries were deleted and re-created successfully.
- `kubectl get nodes` still failed with TLS verification error against `127.0.0.1:44431`.

---

### 3) Compare kubeconfig CA vs Kind control-plane CA

```bash
kubectl config view --raw -o jsonpath='{.clusters[?(@.name=="kind-gitops-reverser-test-e2e")].cluster.certificate-authority-data}' \
  | base64 -d | openssl x509 -noout -fingerprint -sha256 -subject -issuer -dates
```

```bash
docker exec gitops-reverser-test-e2e-control-plane \
  openssl x509 -in /etc/kubernetes/pki/ca.crt -noout -fingerprint -sha256 -subject -issuer -dates
```

Result:
- Both matched:
  - `sha256 Fingerprint=E1:1E:2C:CC:76:B7:6E:A7:7F:A7:F2:EB:D4:54:3D:E9:29:7C:26:EA:69:A8:A9:58:12:86:BF:77:39:D7:67:36`
  - `subject=CN = kubernetes`

---

### 4) Inspect certificate actually served on localhost endpoint

```bash
openssl s_client -connect 127.0.0.1:44431 -showcerts </dev/null 2>/dev/null \
  | awk '/-----BEGIN CERTIFICATE-----/{f=1} f{print} /-----END CERTIFICATE-----/{exit}' \
  | openssl x509 -noout -fingerprint -sha256 -subject -issuer -dates
```

Result:
- Served cert fingerprint was different:
  - `sha256 Fingerprint=1C:66:DF:93:B3:D1:AE:57:CF:28:A6:31:48:77:84:9E:A3:A6:6D:E7:1F:E6:0C:15:54:F5:EB:72:55:DB:F7:4C`
  - `subject=CN = kube-apiserver`
  - `issuer=CN = kubernetes`

Interpretation:
- kubeconfig trusts CA `E1:...:67:36`
- endpoint `127.0.0.1:44431` presented a chain anchored differently in this environment
- explains x509 verification failure

---

### 5) Verify target container and published port

```bash
docker ps -a --format 'table {{.ID}}\t{{.Names}}\t{{.Status}}\t{{.Ports}}'
docker inspect -f '{{range.NetworkSettings.Networks}}{{.IPAddress}}{{end}} {{json .NetworkSettings.Ports}}' gitops-reverser-test-e2e-control-plane
```

Result:
- Container: `gitops-reverser-test-e2e-control-plane`
- Port mapping showed: `127.0.0.1:44431->6443/tcp`
- Container IP: `172.19.0.2`

---

### 6) Check cluster health from inside control-plane container

```bash
docker exec gitops-reverser-test-e2e-control-plane \
  kubectl --kubeconfig /etc/kubernetes/admin.conf get nodes
```

Result:

```text
NAME                                     STATUS   ROLES           AGE   VERSION
gitops-reverser-test-e2e-control-plane   Ready    control-plane   ...   v1.35.0
```

Interpretation:
- Kind cluster itself is healthy.
- Failure is specific to devcontainer-local endpoint/trust path for `127.0.0.1:44431`.

---

### 7) Additional connectivity test

```bash
kubectl --insecure-skip-tls-verify=true get nodes -o wide
```

Result:

```text
error: You must be logged in to the server (the server has asked for the client to provide credentials)
```

Interpretation:
- Endpoint is reachable.
- Credentials/cert trust path does not match what current kubeconfig expects.

## Conclusion

Inside the devcontainer, `kubectl` points to `https://127.0.0.1:44431`, but that endpoint presents a cert chain that does not validate with the CA currently in kubeconfig for this Kind cluster.  
The cluster is healthy; issue is endpoint/cert mismatch in the devcontainer runtime networking path.

## User Observation (Host CLI)

From host machine CLI, access works and is served on a different port.  
This is consistent with endpoint mapping differences between host runtime and devcontainer runtime.

