This directory contains a local-only `cloudflared` scaffold for exposing one hostname through the k3d cluster.

Files:

- `kustomization.yaml`: tracked resources for the public tunnel namespace and Deployment.
- `cloudflared-deplyoment.yaml`: `cloudflared` Deployment that reads a tunnel token from a Secret.
- `tunnel-credentials.yaml.example`: plaintext example for the local Secret. Copy to `tunnel-credentials.yaml`.

Setup steps:

1. Copy `tunnel-credentials.yaml.example` to `tunnel-credentials.yaml`.
2. Paste the `eyJ...` tunnel token from Cloudflare's dashboard into `stringData.token`.
3. In the Cloudflare dashboard, add a public hostname for `demo.configbutler.ai` and point it at `http://traefik.traefik-system.svc.cluster.local:80`.
4. Deploy the app resources in `../vote`.
5. Apply the manifests:

   ```bash
   kubectl apply -k test/e2e/setup/manifests/cloudflared-public
   ```

Quick checks:

- `kubectl -n cloudflared-public get pods`
- `kubectl -n cloudflared-public logs deploy/cloudflared --tail=100`
- `kubectl get ingress -A | grep demo.configbutler.ai`

Traefik is configured to trust forwarded client-IP headers from the default k3s pod network
(`10.42.0.0/16`) so requests proxied by `cloudflared` preserve the original visitor IP.
