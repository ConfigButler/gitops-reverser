Setup the repo by

```
make clean-cluster
REPO_NAME=demo make test-e2e-demo
```

Which will prepare all resources in the vote namespace

The podinfo demo images are served from `https://demo.configbutler.ai/podinfo-assets/` by the
`podinfos-production` overlay so both preview and production can reuse the same public URLs.

https://demo.configbutler.ai/podinfo-assets/podinfos-begijnhof.jpg
https://demo.configbutler.ai/podinfo-assets/podinfos-fiets.jpg
https://demo.configbutler.ai/podinfo-assets/podinfos-xxx.jpg