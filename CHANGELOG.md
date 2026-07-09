# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.32.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.31.0...gitops-reverser-v0.32.0) (2026-07-08)


### Features

* **chart:** Redis-optional install by default; rename modes to configured-author/attributed-author ([#211](https://github.com/ConfigButler/gitops-reverser/issues/211)) ([3bd40c0](https://github.com/ConfigButler/gitops-reverser/commit/3bd40c0199dc858c978ed784726c2742dd2d3751))

## [0.31.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.30.0...gitops-reverser-v0.31.0) (2026-07-08)


### Features

* **gitops-api:** treat higher-level KRM objects (HelmRelease, Argo Application, KRO) as first-class documents ([#203](https://github.com/ConfigButler/gitops-reverser/issues/203)) ([e5722a7](https://github.com/ConfigButler/gitops-reverser/commit/e5722a73bf8e0f9f32c986f478f89e9899a92c9b))
* **secrets:** stop retaining Secret values in the control plane ([#208](https://github.com/ConfigButler/gitops-reverser/issues/208)) ([535c5ed](https://github.com/ConfigButler/gitops-reverser/commit/535c5ed3991db33e7f5f2343256f4b5ffab87d85))

## [0.30.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.29.2...gitops-reverser-v0.30.0) (2026-07-07)


### Features

* **gitops-api:** place new resources to match the repo's existing layout ([#202](https://github.com/ConfigButler/gitops-reverser/issues/202)) ([97a9c87](https://github.com/ConfigButler/gitops-reverser/commit/97a9c8793847efb6a2fba5a9626490e1bbfb9ee7))
* **kustomize:** edit images/replicas overrides through to kustomization.yaml ([#198](https://github.com/ConfigButler/gitops-reverser/issues/198)) ([a8ce917](https://github.com/ConfigButler/gitops-reverser/commit/a8ce917a211d30c7ff4f6c6d82bf0acccbac97c2))


### Bug Fixes

* **release:** sign and attest GitHub release assets directly ([#201](https://github.com/ConfigButler/gitops-reverser/issues/201)) ([10f8962](https://github.com/ConfigButler/gitops-reverser/commit/10f8962f61cf1ac7da5bab08f5373fda61028862))

## [0.29.2](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.29.1...gitops-reverser-v0.29.2) (2026-07-05)


### Performance Improvements

* **build:** cache Go build + module dirs in Dockerfile builder ([#194](https://github.com/ConfigButler/gitops-reverser/issues/194)) ([55ed662](https://github.com/ConfigButler/gitops-reverser/commit/55ed662f92bf0c882f1f10a62ae1146258b563b2))

## [0.29.1](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.29.0...gitops-reverser-v0.29.1) (2026-07-03)


### Bug Fixes

* **ci:** free runner disk after image loads; diagnose e2e rollout timeouts ([#187](https://github.com/ConfigButler/gitops-reverser/issues/187)) ([b89432f](https://github.com/ConfigButler/gitops-reverser/commit/b89432f4113d965f38926c1b744c938f345424bf))
* **ci:** pin Trivy platform when scanning release digests ([#191](https://github.com/ConfigButler/gitops-reverser/issues/191)) ([08d557d](https://github.com/ConfigButler/gitops-reverser/commit/08d557dd7c386b4645916ad25ff0399177454c03))
* **e2e:** deliver image via k3d direct mode, keep retry + pin verification ([#186](https://github.com/ConfigButler/gitops-reverser/issues/186)) ([31abd14](https://github.com/ConfigButler/gitops-reverser/commit/31abd1448a53cab8923e8553d052b37e0f94f178))
* **e2e:** retry k3d image import when the tools-node tarball race drops the image ([#185](https://github.com/ConfigButler/gitops-reverser/issues/185)) ([e426206](https://github.com/ConfigButler/gitops-reverser/commit/e426206921899afb274aa8886f1c1a71c31502da))


### Documentation

* adding DAG overview of tasks ([#177](https://github.com/ConfigButler/gitops-reverser/issues/177)) ([f7a8b6a](https://github.com/ConfigButler/gitops-reverser/commit/f7a8b6a024423f4ceb8c1bdbd117a2428a354a1a))
* reorder readme steps for devcontainer ([#178](https://github.com/ConfigButler/gitops-reverser/issues/178)) ([7856f69](https://github.com/ConfigButler/gitops-reverser/commit/7856f692a14e3d605c547174527e8ab1ee314b0a))

## [0.29.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.28.0...gitops-reverser-v0.29.0) (2026-06-30)


### ⚠ BREAKING CHANGES

* bumping crd versions and last edits
* validating webhook is required (even is audit has been configured).

### Features

* --author-attribution={true|false} is now allowing you to enable or disable the need for kube-api configuration (at the cost of loosing the real author). ([7860294](https://github.com/ConfigButler/gitops-reverser/commit/7860294b237ac9e944e1dac8561d6e7a2111b3cd))
* bumping crd versions and last edits ([4fcffa3](https://github.com/ConfigButler/gitops-reverser/commit/4fcffa319f5b9406f8d4921a34cd53ba7f670794))
* CommitRequest can be attributed by validating webhook handler since it's an internal command ([58dd37a](https://github.com/ConfigButler/gitops-reverser/commit/58dd37a8b2d3e408bff555f71d841a3b0cb17152))
* let's get all testing to Kubernetes 1.36 ([905ff29](https://github.com/ConfigButler/gitops-reverser/commit/905ff29fd5bec5a3cf4cbc2c29409337bdf82fe3))
* **manifestanalyzer,git:** refuse unsupported GitTarget folder content in the writer ([6264d5d](https://github.com/ConfigButler/gitops-reverser/commit/6264d5d72426a476aa798f2ce4f4d6c599394d92))
* refuse weird files in GitTarget path, but do allow .gittargetignore ([1bf3820](https://github.com/ConfigButler/gitops-reverser/commit/1bf3820fdba20d919075189d6c0813f16587d919))
* reworking metrics to new architecture ([b502afe](https://github.com/ConfigButler/gitops-reverser/commit/b502afe512501345df5795899a40484103d87b7c))
* validating webhook is required (even is audit has been configured). ([96ec390](https://github.com/ConfigButler/gitops-reverser/commit/96ec39066741b434887ec08115b7748813a7dba1))
* watch-first ingestion ([28389c9](https://github.com/ConfigButler/gitops-reverser/commit/28389c99848035073fcd4aac367dd80c4c674560))
* **watch:** diff watch-derived vs audit-derived desired sets (Phase 1 payoff) ([c8ba472](https://github.com/ConfigButler/gitops-reverser/commit/c8ba472bd1d0c20391c1185ebb31ccf492dd3995))
* **watch:** parallel watch-state stream behind --watch-state-stream ([097230b](https://github.com/ConfigButler/gitops-reverser/commit/097230b8c600b997f20e7e3f0c172ef253ec9b0b))
* **watch:** surface a refused GitTarget folder as a Blocked stream ([5bdc43d](https://github.com/ConfigButler/gitops-reverser/commit/5bdc43dbab861c7eef1d9c20a511837aeb087399))


### Bug Fixes

* Allow GitTarget to respond quickly to changes in the tracked GitFolder ([591d310](https://github.com/ConfigButler/gitops-reverser/commit/591d3100efa59745b59d332e48979febf68f539d))
* **e2e:** gate cluster readiness on healthy API discovery ([5b81718](https://github.com/ConfigButler/gitops-reverser/commit/5b81718b88a34b043cc42d39fe8d5d4448641010))
* green CI — guard anonymous-access nil deref and skip audit webhook TLS for committer-only e2e ([1e23e16](https://github.com/ConfigButler/gitops-reverser/commit/1e23e16f2a2d7ef20ed084fcdf8d0d2bd799c914))
* wainting for the right status to return ([cc701d6](https://github.com/ConfigButler/gitops-reverser/commit/cc701d645a2e4318e9af2f90459a0cd5bae5cf43))


### Documentation

* adding skills and working on status design ([e55b63a](https://github.com/ConfigButler/gitops-reverser/commit/e55b63a1d5d3a835be5e39400e12a2bffe34d25e))
* created new plan, and hopefully found why the tests are so flaky ([9e610e6](https://github.com/ConfigButler/gitops-reverser/commit/9e610e68e04a748a9ef1cb12b6f5a2c6cd5cb0a6))
* designing gittargetignore ([19ffc7e](https://github.com/ConfigButler/gitops-reverser/commit/19ffc7e6e04e8646c886ee0ed92bf8e6de9b8e78))
* final review on architecture.md ([046b538](https://github.com/ConfigButler/gitops-reverser/commit/046b53804a3344b210565b821d9bc0dd6950a3d2))
* moving architecture along with the rewrite ([6e1193a](https://github.com/ConfigButler/gitops-reverser/commit/6e1193a444f81bd402bee257f9c85888b4b7b51f))

## [0.28.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.27.1...gitops-reverser-v0.28.0) (2026-06-24)


### ⚠ BREAKING CHANGES

* **api:** the configbutler.ai API group is now served only at v1alpha2; v1alpha1 manifests and clients must be updated to v1alpha2.
* providerRef no longer accepts a Flux GitRepository (group/kind enum values removed), and the per-Secret insecure_ignore_host_key key is replaced by the --insecure-allow-missing-known-hosts controller flag. See docs/UPGRADING.md for migration steps.

### Features

* /readyz now waits for healthy audit ingress and valid pingable Reds connection ([c10a4da](https://github.com/ConfigButler/gitops-reverser/commit/c10a4da3c0ca3b3daf62466ee401c5fd6a95eb90))
* **api:** rename API group version v1alpha1 -&gt; v1alpha2 ([a7c4dcd](https://github.com/ConfigButler/gitops-reverser/commit/a7c4dcd71329a87cfa4655b6892f8dcb20daf6ee))
* **commitrequest:** controller-driven, audit-attributed finalize (C-B2) ([cc426d8](https://github.com/ConfigButler/gitops-reverser/commit/cc426d8f74c7ec28895f2d73612ed10659dfe5a8))
* **commitrequest:** eager message attach (stage 4) ([d5113a1](https://github.com/ConfigButler/gitops-reverser/commit/d5113a119328b3014df6a71f13e40cbe003d4c1a))
* **commitrequest:** resolve-on-push with the pushed SHA (stage 5) ([c9093d3](https://github.com/ConfigButler/gitops-reverser/commit/c9093d3d07dbd28d80410dfacb0cfd98a4499005))
* **commitrequest:** the Rejected outcome with a structured reason (stage 6) ([d800fb4](https://github.com/ConfigButler/gitops-reverser/commit/d800fb41f24e7b4425d67ca299b3691c0144e6c5))
* demand driven audit ingestion (only for types that we need) ([dbfce5e](https://github.com/ConfigButler/gitops-reverser/commit/dbfce5ef5d18706fa08110ecb1a6c7cf57f45d38))
* immutable gittargets and gitdestinations ([167a800](https://github.com/ConfigButler/gitops-reverser/commit/167a800a1906fedc4d93fc3295d2a3ac834b3716))
* prevent nested gittargets ([c407fa4](https://github.com/ConfigButler/gitops-reverser/commit/c407fa43339f954beba70a33aa10ccd8f3b43c39))
* read Flux/Argo credential Secrets; drop Flux GitRepository providerRef ([fc7a765](https://github.com/ConfigButler/gitops-reverser/commit/fc7a76529c0fe49851314870e7b6fdb8c42af351))
* rename snapshotTemplate to reconcileTemplate, and default now includes type and last resourceVersion ([e26676d](https://github.com/ConfigButler/gitops-reverser/commit/e26676d960b0fe7c8e42a779c1adb93695097b8b))
* require value for GitTarget.Path, since hooking up GitTarget to repo root must be deliberate ([39e02a6](https://github.com/ConfigButler/gitops-reverser/commit/39e02a68e8b7857f0d87677810db327a12a12233))
* **status:** two-axis GitTarget status (Ready + Synced/phase), serviceability roll-up ([cea0b35](https://github.com/ConfigButler/gitops-reverser/commit/cea0b353c90a028d61d13e939687809b97869c83))
* **stream:** /scale rides the parent type's stream (DEC-A, stages C-A1+C-A2) ([4741e66](https://github.com/ConfigButler/gitops-reverser/commit/4741e66e066f0a474dccf131834baec158e2ec48))
* support flexible manifest placement / editing ([d43d268](https://github.com/ConfigButler/gitops-reverser/commit/d43d268bee4ca41229fc480237ff6c56620fc0cb))
* support for subresources (working kubectl scale deployment) ([0f34d50](https://github.com/ConfigButler/gitops-reverser/commit/0f34d50f06a2f098f7b05c93488c98436bc7efd4))
* **typeset,watch:** M12 first slices — type lifecycle events + per-type reconcile/sweep ([e3f0bd8](https://github.com/ConfigButler/gitops-reverser/commit/e3f0bd85a3d361707736e66e809e886e3a691e09))
* **typeset:** registry owns discovery grace; catalog shrunk to a per-scan normalizer ([6d0dba4](https://github.com/ConfigButler/gitops-reverser/commit/6d0dba48e7962b3556d794a1a301c87eb4520f58))
* **watch:** CommitRequest watermark barrier primitive (C-B1) ([781bfd6](https://github.com/ConfigButler/gitops-reverser/commit/781bfd6dfee9d02de5d575c2551c570a7b287525))


### Bug Fixes

* **controller:** retry Declare on the settle cadence instead of stalling 10m ([b4cafce](https://github.com/ConfigButler/gitops-reverser/commit/b4cafce24951069b41778a726b14f63d0c5ebe55))
* **git:** push a window closed by a no-op resync (stage 3) ([103ad36](https://github.com/ConfigButler/gitops-reverser/commit/103ad360d6a2b679f63ebbaf8b229a592fe860d8))
* **heal:** drain a deferred heal when an atomic finalizes the window; fix drifted comments ([23c881b](https://github.com/ConfigButler/gitops-reverser/commit/23c881b5bcf10b887ad1fa364873349ff9564ad3))
* **heal:** restore periodic checkpoint healing via a deferred-until-idle heal resync ([9a06fe8](https://github.com/ConfigButler/gitops-reverser/commit/9a06fe8a5e7d85e5e1eec3f82476bea3395c68b0))
* **materialization:** retry a failed Declare-time initial backfill ([af6c5b8](https://github.com/ConfigButler/gitops-reverser/commit/af6c5b85e6a02923bfea123271d96124dd5908f6))
* prevent PrepareBranch call for cold cases ([f760c36](https://github.com/ConfigButler/gitops-reverser/commit/f760c36f0410262326c2a015b64dee4938d9aceb))
* **watch:** only backfill-reconcile a type on its first TypeSynced ([8f2ad84](https://github.com/ConfigButler/gitops-reverser/commit/8f2ad840179880723427546f3eff2c8e6f22c0ee))


### Documentation

* adding designs with use cases ([31b7857](https://github.com/ConfigButler/gitops-reverser/commit/31b78577d2c08ac8aecc72d86ffaa9cd17afa61b))
* and simpler ([c27f7ab](https://github.com/ConfigButler/gitops-reverser/commit/c27f7ab94c5a91ff516eebade8454b82f8d452e4))
* capture contextual-namespace + SOPS single-file decisions, add folder fixtures ([1bd8af7](https://github.com/ConfigButler/gitops-reverser/commit/1bd8af7fd7778006a953d74774b7bc30a45dc031))
* cleaning and moving to finished ([bb79343](https://github.com/ConfigButler/gitops-reverser/commit/bb7934301be8fdc2c16c0abd59a5f7a95facef57))
* continue designing for this approach ([27ba3db](https://github.com/ConfigButler/gitops-reverser/commit/27ba3dbf0192a9cbf421af7c86d089472bbecaa0))
* desiging the next phase ([c2d3ce8](https://github.com/ConfigButler/gitops-reverser/commit/c2d3ce86e48cc6518e6c22edd96b5efb02edecfc))
* fighting over abstractions, and what the exact value is of certain parts ([f9227fc](https://github.com/ConfigButler/gitops-reverser/commit/f9227fc4f2fd43f9f12fcd52107675013ae540c3))
* getting all design docs together so that we can cleanup ([17339c0](https://github.com/ConfigButler/gitops-reverser/commit/17339c0c453690414a62dae6e08adf53fe4e48d5))
* getting audit ingestion ([63de7d8](https://github.com/ConfigButler/gitops-reverser/commit/63de7d8f104a52d1a020ed0d7dcc136d6f9beac9))
* getting better ([7bd1f32](https://github.com/ConfigButler/gitops-reverser/commit/7bd1f32d94e4806da73ed234c51185b0be3d4c24))
* getting docs in line and reporting on current findings ([76929d8](https://github.com/ConfigButler/gitops-reverser/commit/76929d8f3d4bb6e85e96b224897fe78e727d54f4))
* getting more details in the plan ([135820b](https://github.com/ConfigButler/gitops-reverser/commit/135820bb42114102227337600668ab74bf03f9d3))
* getting the plan ready for execution ([6eb3787](https://github.com/ConfigButler/gitops-reverser/commit/6eb378744ad9fdd8177642f4514c9a9666f5d897))
* let clean it up a little bit for now ([026443a](https://github.com/ConfigButler/gitops-reverser/commit/026443a57ffd58c3317beb9793ddddb95d06528e))
* Let's just commit it then ([401d194](https://github.com/ConfigButler/gitops-reverser/commit/401d194e56b9424184f557ae1d5fd8376cec33d6))
* **materialization:** correct the stale watch-first sync comment; scope Slice D ([c3e6122](https://github.com/ConfigButler/gitops-reverser/commit/c3e612282864d88717256496cdc2a034a7d7adad))
* planning the implementation ([d6e174b](https://github.com/ConfigButler/gitops-reverser/commit/d6e174bd6e483f2f181d843aa728ea7d01b81a1b))
* reconcile residual-flake findings across the stream design docs ([90a1e7e](https://github.com/ConfigButler/gitops-reverser/commit/90a1e7ea0a0b144e4236d4ab21bf1b0f068c440d))
* the plan is ambitious now I would say ([24457fb](https://github.com/ConfigButler/gitops-reverser/commit/24457fb3c928d04267133960b7990a6bb0511c5d))
* **typeset-grace:** pin the exact typeset surface, consumers, and per-stage interface delta ([8e45ff8](https://github.com/ConfigButler/gitops-reverser/commit/8e45ff84e75cee0b55e2102207a234ce60fe0776))
* working on refining the design of this new pipeline ([7b15e2d](https://github.com/ConfigButler/gitops-reverser/commit/7b15e2dc70e6a1f2fe758364d5f5c30ef92ad029))
* working on vision docs ([394d8bf](https://github.com/ConfigButler/gitops-reverser/commit/394d8bfc1c2bb24c0a48667e68f668ee989ccdd0))

## [0.27.1](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.27.0...gitops-reverser-v0.27.1) (2026-06-02)


### Bug Fixes

* readds support for wildcards ([8e1b3ab](https://github.com/ConfigButler/gitops-reverser/commit/8e1b3ab15d03c3fad9eb002c1293ab3623ccb6ca))

## [0.27.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.26.3...gitops-reverser-v0.27.0) (2026-06-02)


### Features

* count loc ([f819e9a](https://github.com/ConfigButler/gitops-reverser/commit/f819e9a5ed445b78388108510df5afed216d9486))


### Bug Fixes

* improve and test GitTarget isolation behaviour ([0787b1b](https://github.com/ConfigButler/gitops-reverser/commit/0787b1b68bd9eb47ea3b816712f8eb09861f6d6b))
* prevent race in creating gitea org ([ddbcbb3](https://github.com/ConfigButler/gitops-reverser/commit/ddbcbb399fea816e6d63614330b3fbba3ce5f0c0))
* race condition in HelmRelease causing unstable e2e tests ([03b95ab](https://github.com/ConfigButler/gitops-reverser/commit/03b95abfae8a52552729872231441c1e1ac2cbc5))
* upgrade to latest a apiservice-audit-proxy should resolve our non 0 shallow events, also included tests to verify that we don't misread deletecollection events as shallow ([903d792](https://github.com/ConfigButler/gitops-reverser/commit/903d7920162ecd56b9d863666959fc7f7d77e3e1))


### Documentation

* initial docs (also some findings from preps for Cozysummit) ([fbbfcec](https://github.com/ConfigButler/gitops-reverser/commit/fbbfcec75309577e33bf36243361428dea81002b))
* Phase 2.5 status + Monday resume plan (3-agent CI green once; stability TBD) ([b285ac5](https://github.com/ConfigButler/gitops-reverser/commit/b285ac5e84a936143f3040e5f9b66a7de8b36764))
* record Phase 2.5 implementation status and measured impact ([dba83ac](https://github.com/ConfigButler/gitops-reverser/commit/dba83ac962c749e744788416419625daefabedda))

## [0.26.3](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.26.2...gitops-reverser-v0.26.3) (2026-05-25)


### Bug Fixes

* showing spec.message at commitrequests ([54225eb](https://github.com/ConfigButler/gitops-reverser/commit/54225eb7713c4796a6d8d6da6d581733b84de2b3))

## [0.26.2](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.26.1...gitops-reverser-v0.26.2) (2026-05-25)


### Bug Fixes

* allow generated names with a commit request ([#155](https://github.com/ConfigButler/gitops-reverser/issues/155)) ([ff27eba](https://github.com/ConfigButler/gitops-reverser/commit/ff27ebaa1b1872d095325057a6d36dae707b0471))

## [0.26.1](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.26.0...gitops-reverser-v0.26.1) (2026-05-23)


### Bug Fixes

* ignore subresource audit events (that was hard to debug!) and replaced debug dump mechanism ([#153](https://github.com/ConfigButler/gitops-reverser/issues/153)) ([23a2d65](https://github.com/ConfigButler/gitops-reverser/commit/23a2d6585c4f7055b0b45a736ca7cfb194aa46c1))

## [0.26.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.25.0...gitops-reverser-v0.26.0) (2026-05-22)


### Features

* metrics overhaul as a step into more observability ([1a7cd19](https://github.com/ConfigButler/gitops-reverser/commit/1a7cd19ea416fb4dfe69271867aab9034695ab75))
* support --additionalSensitiveResources (in addition to v1/secrets) ([3ae1ebd](https://github.com/ConfigButler/gitops-reverser/commit/3ae1ebd0e10afd73b66737269889a76b586b94e7))


### Bug Fixes

* audit messages that indicate conflict (409) can't end up in Git anymore (should never happen given that they also don't end up in etcd). ([ec47c52](https://github.com/ConfigButler/gitops-reverser/commit/ec47c5227126f625064326c57fad5f94789be314))
* central APIResourceCatalog so that we have one abstraction/cache to which things the current apiserver is serving. ([3d6cb62](https://github.com/ConfigButler/gitops-reverser/commit/3d6cb6258a168da6ad82e0c17a459efacb3ecbe3))
* gate audit body joins by rule relevance ([605964a](https://github.com/ConfigButler/gitops-reverser/commit/605964ab046e14ae8ae6463da45ab44d2ea2e3c3))
* reconicle fail on restart and wildcard ([26511a9](https://github.com/ConfigButler/gitops-reverser/commit/26511a967e4145f2ae691b191f9da1b408dc2932))
* timing issues when adding a new WatchRule in e2e: simplifying the ingestion resolves this ([a688620](https://github.com/ConfigButler/gitops-reverser/commit/a688620e3d6832afb2976f3f542edfb005d977a9))


### Documentation

* planning and designing for new features ([856beee](https://github.com/ConfigButler/gitops-reverser/commit/856beeed5f6ab3717a2d3010edaa914cdff0e44b))

## [0.25.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.24.0...gitops-reverser-v0.25.0) (2026-05-21)


### Features

* support "offical_first" for situations where the offical kube-api audit events arrives slightly before the additional body event. ([380dc1e](https://github.com/ConfigButler/gitops-reverser/commit/380dc1e94c62c9a70f1253cb79236567d7b0c30f))
* support claim based commit author attribution ([06eaf8c](https://github.com/ConfigButler/gitops-reverser/commit/06eaf8c99d43305caa826301e4246f8b1ed8263e))


### Bug Fixes

* better reconcile ([f3dcf56](https://github.com/ConfigButler/gitops-reverser/commit/f3dcf56e5135b455bdc0a8bd1b20651fb6cdb12e))
* watch for changes in dependent resources ([8c9aeb8](https://github.com/ConfigButler/gitops-reverser/commit/8c9aeb8b2c51187cc6d6b9dd05bd7f6452c19d97)), closes [#145](https://github.com/ConfigButler/gitops-reverser/issues/145)


### Documentation

* getting more whys into the architecture document ([5762a4a](https://github.com/ConfigButler/gitops-reverser/commit/5762a4a31029b6cb52051bb16f4cf26aa46d3306))

## [0.24.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.23.0...gitops-reverser-v0.24.0) (2026-05-20)


### Features

* bumping apiservice-audit-proxy and it's all TLS now ([9d431c2](https://github.com/ConfigButler/gitops-reverser/commit/9d431c27cc7a01d41d78337f59c8130d3e7b2c14))

## [0.23.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.22.0...gitops-reverser-v0.23.0) (2026-05-18)


### Features

* allow end-user to create explicit commit with specific message (which also ends the automatic commit window). ([abd6a48](https://github.com/ConfigButler/gitops-reverser/commit/abd6a48fed972a1d50d6f7304183766829db0021))
* deduplicated audit ingestion for aggregated API, removes the option to indicate cluster id ([35443db](https://github.com/ConfigButler/gitops-reverser/commit/35443dbc8062da13feb807017314df5b12a1467e))


### Bug Fixes

* resolving a flaky unit test ([04715a2](https://github.com/ConfigButler/gitops-reverser/commit/04715a283db835adac038481e33f2a39283b5656))


### Documentation

* And the last claude additions for today ([76191b3](https://github.com/ConfigButler/gitops-reverser/commit/76191b3b22d04c2c4c4944ba8e430390ab95be68))
* first design ([7d60c22](https://github.com/ConfigButler/gitops-reverser/commit/7d60c223330edb920931de70ece05302b12fe1ed))
* getting claude to review it again, with some questioning of my own ([e133724](https://github.com/ConfigButler/gitops-reverser/commit/e133724c7e99200d589e8617ac1d65849b0fa42c))
* getting codex feedback ([a42145c](https://github.com/ConfigButler/gitops-reverser/commit/a42145c1af6bffd36090e1bc0de14bd6d682984a))
* getting docs in and small fixes and tests ([160640a](https://github.com/ConfigButler/gitops-reverser/commit/160640a18e06ad027fed62f1f2875244b276f0db))
* Getting next steps in ([ac12d25](https://github.com/ConfigButler/gitops-reverser/commit/ac12d25494ded69094ad037be13e78a0145a7545))
* getting started hints for aggregated API usage ([7d86434](https://github.com/ConfigButler/gitops-reverser/commit/7d864340cfceb568ea0e86fe3c0cbe5647b44bff))
* investing in good designs is worth it I guess ([4cb5f45](https://github.com/ConfigButler/gitops-reverser/commit/4cb5f45c318af72286f7c543d063187254ee3d4e))
* More details in the document ([1b51f1c](https://github.com/ConfigButler/gitops-reverser/commit/1b51f1c6c0041bc31f25e9cc878b5983f344ee35))
* reworking docs and tuning the new CRD ([8ebceaf](https://github.com/ConfigButler/gitops-reverser/commit/8ebceafb9c6a81bda31a5b6545397db6508d5758))
* second design ([2146289](https://github.com/ConfigButler/gitops-reverser/commit/21462898095621f4589907e9bbda73ade2e2d762))
* some claude ([5315440](https://github.com/ConfigButler/gitops-reverser/commit/53154403e1bf078b5f9c506e02b27d6e41108477))
* Working on plans ([7bb77bb](https://github.com/ConfigButler/gitops-reverser/commit/7bb77bb843cac543696d8471e36e8a3f114c0f52))

## [0.22.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.21.1...gitops-reverser-v0.22.0) (2026-05-05)


### ⚠ BREAKING CHANGES

* Field rename for clarity (please check configuration.md)

### Features

* Field rename for clarity (please check configuration.md) ([9f101b0](https://github.com/ConfigButler/gitops-reverser/commit/9f101b0737396ebf4361e17792b35ebe5148e0b4))
* first phase for commit batching based on author ([e742e8c](https://github.com/ConfigButler/gitops-reverser/commit/e742e8c2950589298ab4844818bfa212e80a2d1c))


### Documentation

* cleaning branch work ([d07233a](https://github.com/ConfigButler/gitops-reverser/commit/d07233ac7acfbe1288a552f49be16f9f39348ed8))
* cleaning up old docs ([791d2f9](https://github.com/ConfigButler/gitops-reverser/commit/791d2f908db24d6305d773b6cf1ef25b3d721ec5))

## [0.21.1](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.21.0...gitops-reverser-v0.21.1) (2026-04-21)


### Bug Fixes

* fixing flaky e2e test ([83e9f17](https://github.com/ConfigButler/gitops-reverser/commit/83e9f17f69a03a1f75aff0cf1a0a9300029c4aea))

## [0.21.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.20.0...gitops-reverser-v0.21.0) (2026-04-17)


### Features

* Allowing commit author and message config ([04a592b](https://github.com/ConfigButler/gitops-reverser/commit/04a592b03d5244cf36bbb18ac05c99c6cffc8b98))
* commit signing ([5e8ad9c](https://github.com/ConfigButler/gitops-reverser/commit/5e8ad9cc0613a476dcf2a08695909e64478cb7bc))
* replacing Makefile by Taskfile ([bd3b709](https://github.com/ConfigButler/gitops-reverser/commit/bd3b70929d01f16136882d1a78704c5d66e26e12))


### Bug Fixes

* gittarget can just be created without existing gitprovider (don't block, just give a good error). ([ee20cfd](https://github.com/ConfigButler/gitops-reverser/commit/ee20cfd68481ad022314a0261ac6c1449c20fa93))
* WatchRule leaking resources from other namespaces in live event stream ([1676a42](https://github.com/ConfigButler/gitops-reverser/commit/1676a423ad30df094acfcb21890f786ba16d9248))


### Documentation

* adding architectural overview ([e932402](https://github.com/ConfigButler/gitops-reverser/commit/e93240203d2675a9d9799f4fd372ebc99327eb0b))
* cleaning up and more clarity ([51396a2](https://github.com/ConfigButler/gitops-reverser/commit/51396a23be4282d9a724f58149ef94b92e66abc6))
* Cleaning up docs ([96b249d](https://github.com/ConfigButler/gitops-reverser/commit/96b249d7597829a8fb3b1274a770790c4a76aa52))
* continue docs clean-up, and other small things from review ([80c380c](https://github.com/ConfigButler/gitops-reverser/commit/80c380c0bf53167ddbd8bd641c70c6c7a66ca288))
* Let's get an elaborate description of the e2e tests ([9091712](https://github.com/ConfigButler/gitops-reverser/commit/9091712d51571418cd00175b0ee52edd1c292be2))

## [0.20.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.19.3...gitops-reverser-v0.20.0) (2026-04-09)


### Features

* audit webhooks it is ([ff31254](https://github.com/ConfigButler/gitops-reverser/commit/ff312545657f83bc24773a95cca6299e20f6356d))


### Bug Fixes

* Handle deletion 'updates' by checking for terminating and deletionTimestamp ([fdf4214](https://github.com/ConfigButler/gitops-reverser/commit/fdf421425940a35a55b668c5b740d2fb73efeef7))


### Documentation

* Adjusting docs to how it all works now ([f8c7796](https://github.com/ConfigButler/gitops-reverser/commit/f8c7796976aa15df2ba2194f0dd2276c0e8c05e3))
* Improving quick-start quality by moving to helm ([cf25552](https://github.com/ConfigButler/gitops-reverser/commit/cf255528d422358945e7fa2acad3b09bbff1f260))

## [0.19.3](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.19.2...gitops-reverser-v0.19.3) (2026-03-23)


### Bug Fixes

* Have working printcolumns ([eca9ae8](https://github.com/ConfigButler/gitops-reverser/commit/eca9ae8cc448b0682e1c484feb8d969abc581600))
* Preventing reconcile code from ignoring ns boundary ([d3b0ecf](https://github.com/ConfigButler/gitops-reverser/commit/d3b0ecf9670ef5caefd6fcf2dfe601efd26542a9))

## [0.19.2](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.19.1...gitops-reverser-v0.19.2) (2026-03-16)


### Documentation

* Improving docs ([dc411c6](https://github.com/ConfigButler/gitops-reverser/commit/dc411c691820a3f6fae75871cd965028dc9510f1))

## [0.19.1](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.19.0...gitops-reverser-v0.19.1) (2026-03-15)


### Bug Fixes

* Really patch the status so that the test makes sense ([bbad230](https://github.com/ConfigButler/gitops-reverser/commit/bbad230c8bba6a21a4d5af8178f8fd48630e4d83))

## [0.19.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.18.0...gitops-reverser-v0.19.0) (2026-03-05)


### Features

* Adding valkey to allow experiment with 'persistant' queues ([a0a15a5](https://github.com/ConfigButler/gitops-reverser/commit/a0a15a5b9e1fe17e1b5379bff012ce6e6d631b13))
* Move gitea setup to Makefile (also allowing to have the repos under the .markers) ([ea434fe](https://github.com/ConfigButler/gitops-reverser/commit/ea434fe25377170a2d285f9f6957dd04c76be50a))


### Bug Fixes

* Linting errors in Makefile ([05faf7c](https://github.com/ConfigButler/gitops-reverser/commit/05faf7c10abe5dedc447de858d4b750ec1da5ed7))
* One edgecase where a secret is deleted (is now recreated) ([3a3f139](https://github.com/ConfigButler/gitops-reverser/commit/3a3f13970295fc05e01a310aa851b3bd12202aa4))


### Documentation

* Adding and removing ai stuff ([4d0ac12](https://github.com/ConfigButler/gitops-reverser/commit/4d0ac12d0a1f7e3c87e36c32a80f95b7e9d7fc18))

## [0.18.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.17.1...gitops-reverser-v0.18.0) (2026-03-03)


### Features

* Making big steps in Makefile understanding, but it's still messy ([cf93ab5](https://github.com/ConfigButler/gitops-reverser/commit/cf93ab5dbff98e98d641c1c4cb4e13bcaeef67a3))


### Bug Fixes

* Always check these things yourself ([ce9031d](https://github.com/ConfigButler/gitops-reverser/commit/ce9031d6e059dcfa6a06e34819a0389677b8e5e3))
* Getting all paths right ([77ac054](https://github.com/ConfigButler/gitops-reverser/commit/77ac05436bdad42a5233e971cdf132c749a189ce))
* Getting the image insertion at least a little bit straight ([ff31576](https://github.com/ConfigButler/gitops-reverser/commit/ff3157690088e226d79ba8331f47ae05a5f11bf9))
* Kind and docker outside docker ([e94be0c](https://github.com/ConfigButler/gitops-reverser/commit/e94be0c100c0c7cd718111484c0fd174e3db781b))
* Let's get the linter happy again ([ce18453](https://github.com/ConfigButler/gitops-reverser/commit/ce18453873a2110fe5fac421d325bf2bf7d90b1a))
* Mistake in Makefile syntax ([2a8820a](https://github.com/ConfigButler/gitops-reverser/commit/2a8820a083df8e15a666cafa4ebce33ea0596612))
* Now we can really uninstall, also multiple if that would already be supported ([241bb13](https://github.com/ConfigButler/gitops-reverser/commit/241bb135dca2ae5c2417bb692261f1fb59e45388))
* preventing warning on k3d creation DoD style ([30a8208](https://github.com/ConfigButler/gitops-reverser/commit/30a8208e1061e89ef6d574fcd938d8df456ca8fc))
* Some tweaks to cleaning existing installs, and moving to a seperate file ([22e960e](https://github.com/ConfigButler/gitops-reverser/commit/22e960ea904b3f960f18ea9ced156b9c8d3d0e3f))
* The order in which the dependencies is (perhaps to?) important ([a38102a](https://github.com/ConfigButler/gitops-reverser/commit/a38102ac41f7c7a90981f92e59be42384c8cf56e))

## [0.17.1](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.17.0...gitops-reverser-v0.17.1) (2026-02-26)


### Bug Fixes

* Failing folder reconcile for core group (group="") resources ([212132f](https://github.com/ConfigButler/gitops-reverser/commit/212132fa0c52f9888bbc63c0008e6c3dc3bf88b5))
* This should remove the timing mistake in the e2e setup ([74d25af](https://github.com/ConfigButler/gitops-reverser/commit/74d25afed95bffa74872fefbf25d3fe34638335b))

## [0.17.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.16.0...gitops-reverser-v0.17.0) (2026-02-25)


### Features

* output json logging for now ([f4ebaee](https://github.com/ConfigButler/gitops-reverser/commit/f4ebaee51de11a401738f8069340a875ae56edab))


### Bug Fixes

* Have seperate clusters per installation type (asking for troubles if you don't). ([62496c3](https://github.com/ConfigButler/gitops-reverser/commit/62496c39cc30a176c7aa458445b85e5ac5981470))


### Documentation

* Plotting potenial next steps ([5f94e25](https://github.com/ConfigButler/gitops-reverser/commit/5f94e2580519d3bf13089d18922511dd423ce2db))
* Seeing if this fixes it ([c4d8170](https://github.com/ConfigButler/gitops-reverser/commit/c4d8170c2e390c7b581f7934c19b201eca3e9248))
* Some new plans for later ([263c3c3](https://github.com/ConfigButler/gitops-reverser/commit/263c3c3abe633a112d5a30de8426898ad791804f))

## [0.16.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.15.0...gitops-reverser-v0.16.0) (2026-02-21)


### Features

* Allow AGE key generation ([a90d4a7](https://github.com/ConfigButler/gitops-reverser/commit/a90d4a7e1f189bbb9c79c6228174a2921d6802e9))
* Use sops to encrypt secrets ([14ef5e8](https://github.com/ConfigButler/gitops-reverser/commit/14ef5e84975f0c3d476fa1052cb8510395539acb))


### Bug Fixes

* Docker-outside-docker is apperently flaky, applied a workarround so that I can keep behaviour equal to CI for now ([50ffced](https://github.com/ConfigButler/gitops-reverser/commit/50ffced8929242e5affb4c7988db169aa3bb8610))
* Let's fix it! ([401916d](https://github.com/ConfigButler/gitops-reverser/commit/401916d23d601290b7a1cd4dc3068fc1406d1632))
* linting ([2d95654](https://github.com/ConfigButler/gitops-reverser/commit/2d956548ab5ef0c56ef8277383beeddb21e19d4f))


### Documentation

* Improvement plans for SOPS Age key generation (support only supplying public keys as well). Also multiple recpiants if wished for. ([6091aa5](https://github.com/ConfigButler/gitops-reverser/commit/6091aa584f3a3eca08e4df0f244c18a1757119c9))

## [0.15.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.14.3...gitops-reverser-v0.15.0) (2026-02-13)


### Features

* Let's give the audit webhook handling it's own webserver (so that TLS config can be different) ([058022c](https://github.com/ConfigButler/gitops-reverser/commit/058022c2612a43dda187cc1e1a8903591fbbf0ba))
* Simplify the services and server configurations ([28b17d6](https://github.com/ConfigButler/gitops-reverser/commit/28b17d6839e6dcb1c06f8c6ed82dcee45bb3092a))
* Spring cleaning of /config ([af2d1a5](https://github.com/ConfigButler/gitops-reverser/commit/af2d1a5d6b92904a36c8e6348700d1111cb18c31))


### Bug Fixes

* Also make that part simpler please ([963d1c1](https://github.com/ConfigButler/gitops-reverser/commit/963d1c10d01e760d9c4e46c632025b12b9320636))
* linting issues ([0f96cae](https://github.com/ConfigButler/gitops-reverser/commit/0f96cae5687980e74dacce2fea3d3262f0a069cf))
* Make "--metric-insecure" a reality and local testability of all flows ([cf9b7f7](https://github.com/ConfigButler/gitops-reverser/commit/cf9b7f70609e6a635dd3d737f99655c2f68a6526))
* Never ever commit secrets in their raw form ([aeff306](https://github.com/ConfigButler/gitops-reverser/commit/aeff306f18012c12cbfd74d43a37a22a10b2956b))
* Remove double crds ([00d7158](https://github.com/ConfigButler/gitops-reverser/commit/00d7158de21ca7ed884da4f1f997d04768b880e9))
* That should fix it ([62ac5b8](https://github.com/ConfigButler/gitops-reverser/commit/62ac5b8e5d26c24c0dfce0341f1a6da25e7e2f01))
* That should resolve it ([8176831](https://github.com/ConfigButler/gitops-reverser/commit/8176831e4ecaf3aebaf74be4d065d82ffe697da1))
* Would this now finally work? ([f5ce17a](https://github.com/ConfigButler/gitops-reverser/commit/f5ce17a51eb7458adee8b4f730258723d629a620))


### Documentation

* Cleaning up ([1322ac9](https://github.com/ConfigButler/gitops-reverser/commit/1322ac9c0d2c5d1aa8df63c2141b9a343c38a12e))
* Improving overview docs ([d1eaa9b](https://github.com/ConfigButler/gitops-reverser/commit/d1eaa9b44071601b1124aad38741434ed7b5a88d))
* Updating expectations ([8dd465a](https://github.com/ConfigButler/gitops-reverser/commit/8dd465af67a76020fde9553c9227c833dfd7e8ff))

## [0.14.3](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.14.2...gitops-reverser-v0.14.3) (2026-01-30)


### Bug Fixes

* Servicemonitor in helm chart was not flexible enough ([20d4b7c](https://github.com/ConfigButler/gitops-reverser/commit/20d4b7c1bfb71d20ad172d46c6d563d1d99361db))


### Documentation

* Cleaning docs and polishing ([632bff1](https://github.com/ConfigButler/gitops-reverser/commit/632bff11fa82fa24cc1d2031053e17396d3c30b0))

## [0.14.2](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.14.1...gitops-reverser-v0.14.2) (2026-01-30)


### Bug Fixes

* Adjusting gittea stuff for the latest version ([454c948](https://github.com/ConfigButler/gitops-reverser/commit/454c94879e23a6f1b601179823331260582fd73a))
* Get the defaults right so that the examples work ([d304f7c](https://github.com/ConfigButler/gitops-reverser/commit/d304f7c8c85e25a9c971fa9a9e48098c55d2a8a3))
* Getting weird concurrency out ([160deca](https://github.com/ConfigButler/gitops-reverser/commit/160deca43719e93805ef4b1185393726ccb26321))
* Linting mistake ([2b0c399](https://github.com/ConfigButler/gitops-reverser/commit/2b0c399db56621437bf209daec4dbad9d2ea75e1))


### Documentation

* Small adjustements ([4289070](https://github.com/ConfigButler/gitops-reverser/commit/4289070111412075c52006600cee30c56ddcd7e6))

## [0.14.1](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.14.0...gitops-reverser-v0.14.1) (2026-01-30)


### Bug Fixes

* No ns creation in helm chart, let's keep it simple ([207ad59](https://github.com/ConfigButler/gitops-reverser/commit/207ad5901c4aea3d127b21f417447f62aac07ce3))

## [0.14.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.13.1...gitops-reverser-v0.14.0) (2026-01-26)


### Features

* add experimental audit webhook for metrics collection ([a5b8655](https://github.com/ConfigButler/gitops-reverser/commit/a5b8655434034bedd3f9724bd93e1c4063da3ff0))
* All reference end with Ref and custom types ([a7092a2](https://github.com/ConfigButler/gitops-reverser/commit/a7092a243d629b5bb85d5dfce4561eb08d0469d7))
* **e2e:** add Kind cluster audit webhook support ([5253765](https://github.com/ConfigButler/gitops-reverser/commit/52537653e69e64f0a4cb5789686977e20402724c))
* Scafold 'new' types ([bd8688c](https://github.com/ConfigButler/gitops-reverser/commit/bd8688cb560caecae0e36509e9429ebba8eddb6e))
* Support setting audit-dump-dir so that we can analyse what the k8s api is sending us ([1867f21](https://github.com/ConfigButler/gitops-reverser/commit/1867f219dadd89830a2ba630032b0e811a6f48d4))


### Bug Fixes

* Also converting unit tests and ask for deleteion ([00c5974](https://github.com/ConfigButler/gitops-reverser/commit/00c5974490cfad490ea8c5687c9baeefe757c155))
* **devcontainer:** pin DOCKER_API_VERSION to 1.43 ([181f9ca](https://github.com/ConfigButler/gitops-reverser/commit/181f9ca547c2a6a6d3e0f34ad0f3ed59a0df3a7c))
* end2end testa are now working ([b6bc194](https://github.com/ConfigButler/gitops-reverser/commit/b6bc1942583449cd0c0655e06b42877667c4fa96))
* Fixing all tests ([7e786dc](https://github.com/ConfigButler/gitops-reverser/commit/7e786dc4b4146abd6c7595b8f5a9cbe7ca90fa4c))
* Get the all tests green ([fdb8d8f](https://github.com/ConfigButler/gitops-reverser/commit/fdb8d8f260207194f15ed1c1fccf488ca5064de2))
* Getting tests green and refining contracts ([66faefa](https://github.com/ConfigButler/gitops-reverser/commit/66faefa2cbb11aed36aaa59668e7bfb74f677ed9))
* Ironing out last details ([5c16998](https://github.com/ConfigButler/gitops-reverser/commit/5c16998dc53004a5e073fffaaf51dc2c63aaaee0))
* Let's also improve security since we are now in the tmp folder ([0a418a3](https://github.com/ConfigButler/gitops-reverser/commit/0a418a3138ada3768ce41af150e953bdeca9d3bb))
* Remove more ([ec0ad99](https://github.com/ConfigButler/gitops-reverser/commit/ec0ad995605e26100d5907c2a35988a6d6f0e173))
* Run kind config in GH as well ([9231f04](https://github.com/ConfigButler/gitops-reverser/commit/9231f04c6999b2d78f0397e1b16f84e79573d6c7))


### Documentation

* And adjust asciiart ([b283142](https://github.com/ConfigButler/gitops-reverser/commit/b28314290e808d3cc490c36fa03816acf8296e98))
* Saturday redesign of new-config ([25f8a3d](https://github.com/ConfigButler/gitops-reverser/commit/25f8a3d923be54013a136499de10de599497c1a9))
* Setup apiserver audit hook ([c0fa483](https://github.com/ConfigButler/gitops-reverser/commit/c0fa483d86bc0cdf3635b9e9343a1612d45d5ca9))

## [0.13.1](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.13.0...gitops-reverser-v0.13.1) (2025-11-24)


### Bug Fixes

* Improving e2e test stability in pipeline by better aligned timings ([c607be9](https://github.com/ConfigButler/gitops-reverser/commit/c607be988ad2b2cb5c7437a5c019d3248dd68392))
* prevent cert-manager warning during default deployment ([c12e9e6](https://github.com/ConfigButler/gitops-reverser/commit/c12e9e6c773edcd1c11708faaf9babb30129e541))


### Documentation

* Have a demo as teaser ([c8693e6](https://github.com/ConfigButler/gitops-reverser/commit/c8693e68d7e12a2c326fc9f42b765ec1d4b939fe))

## [0.13.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.12.1...gitops-reverser-v0.13.0) (2025-11-22)


### Features

* username as author (instead of appended to commit message) ([36f337f](https://github.com/ConfigButler/gitops-reverser/commit/36f337f97e1f921f98986bc28275c14aef91371d))


### Bug Fixes

* Attribute to user 'fixed' ([eef3330](https://github.com/ConfigButler/gitops-reverser/commit/eef333044cb96792dbef3fc3356501206535a000))
* Removing queue stuff for correlation, only works in theory ([b13de1d](https://github.com/ConfigButler/gitops-reverser/commit/b13de1dcfb58bc7e9866f8bc3f3edc98babadc18))

## [0.12.1](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.12.0...gitops-reverser-v0.12.1) (2025-11-21)


### Bug Fixes

* pin Docker API version to 1.43 for Codespaces ([4c81c77](https://github.com/ConfigButler/gitops-reverser/commit/4c81c77fdffde6f6b7429cde9deb640af424f714))


### Documentation

* Show the GH code pages button in the right place ([06b52f6](https://github.com/ConfigButler/gitops-reverser/commit/06b52f6c407d531fa7dac8cff2b7eefa7c993f8f))
* Small improvements ([fc76b96](https://github.com/ConfigButler/gitops-reverser/commit/fc76b96ccdf5b0e8bff18fe6775b400e21db8371))

## [0.12.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.11.0...gitops-reverser-v0.12.0) (2025-11-20)


### Features

* Detect resurect during push ([5559982](https://github.com/ConfigButler/gitops-reverser/commit/5559982aa7234af3a398c478d9ef6329e27fd7a2))


### Bug Fixes

* Happy linter is happy Simon ([20b304a](https://github.com/ConfigButler/gitops-reverser/commit/20b304ac0bb3da0cfca85dd713deed9f4d066cd9))
* More linter fixing ([69b98cd](https://github.com/ConfigButler/gitops-reverser/commit/69b98cd37c98ae6080ecc49515d15a9ec45dfaf7))
* Resolving last problems with dagling head ([b5f12dd](https://github.com/ConfigButler/gitops-reverser/commit/b5f12dd1ff2198263c63323b7d7c00203f32e60a))

## [0.11.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.10.1...gitops-reverser-v0.11.0) (2025-11-17)


### Features

* Connected to the new abstraction, and now also working with empty git repos ([526b08e](https://github.com/ConfigButler/gitops-reverser/commit/526b08e75fe5ffbf5f1a6ec7aaedce2d81c128f8))
* Let's also run the pull every minute ([2006620](https://github.com/ConfigButler/gitops-reverser/commit/2006620b9789ab129ccd8b44e37742a1e043778a))
* Signs of getting this right are here ([54191e7](https://github.com/ConfigButler/gitops-reverser/commit/54191e77f5b1421b23c70bdca7cd22524c27494e))
* We are getting close to not even needing that default branch anymore. And it's way simpler. ([2973fc8](https://github.com/ConfigButler/gitops-reverser/commit/2973fc8dc81ca9ad23d328af2b59d9ef62d78d9c))


### Bug Fixes

* All abstraction tests are now green ([a72b1a6](https://github.com/ConfigButler/gitops-reverser/commit/a72b1a6ae0d4abd97fd50953b5a03ce6491aa4c8))
* Fixing more tests and linter ([1a46519](https://github.com/ConfigButler/gitops-reverser/commit/1a465192dac00edfb3d61de7712e92ff4c9e688e))
* Getting the right status ([7b26b6c](https://github.com/ConfigButler/gitops-reverser/commit/7b26b6c26a2a07546b9d47fe0a501f9447949457))
* Last tests removed as wel ([0ec4116](https://github.com/ConfigButler/gitops-reverser/commit/0ec4116f9aa7786aa5213c3acadb2cc10e03b8f4))
* Let's get our abstraction back into git :-) ([b25571f](https://github.com/ConfigButler/gitops-reverser/commit/b25571f9ae24183cec3a53db81328c32e057219b))
* Let's see if we can get back window support for windows devs ([98e6f26](https://github.com/ConfigButler/gitops-reverser/commit/98e6f26439806efab39ab2b1dd3d1b5d0010628a))
* Now it starts to make sense ([8e3e036](https://github.com/ConfigButler/gitops-reverser/commit/8e3e036044c85655f96a3644151478df86af0536))


### Documentation

* Adding plans for branch refactor ([a7cde4b](https://github.com/ConfigButler/gitops-reverser/commit/a7cde4b26aeb61e13e9bf87c7b6043820e25fe17))
* Last tuning on plan ([21954ab](https://github.com/ConfigButler/gitops-reverser/commit/21954ab76e2799a00b4acc559674aab0b2f0f8fe))

## [0.10.1](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.10.0...gitops-reverser-v0.10.1) (2025-11-11)


### Bug Fixes

* consolidate CI and dev Dockerfiles into multi-stage build ([0f5fc61](https://github.com/ConfigButler/gitops-reverser/commit/0f5fc61db5c6fdcac559ee7d73e6011715de2866))
* Does this resolve our build? ([2927a2a](https://github.com/ConfigButler/gitops-reverser/commit/2927a2accfca44b53398159449e131803a48d02f))
* Move the port forwards to higher ports to avoid conflicts with existing services ([189bee2](https://github.com/ConfigButler/gitops-reverser/commit/189bee2ac7ceccefbf4829ea456c26aafddd1f49))
* Slowly but truly getting there ([38d7275](https://github.com/ConfigButler/gitops-reverser/commit/38d7275daa87f6e41b0a455ec6a5081227276a84))

## [0.10.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.9.0...gitops-reverser-v0.10.0) (2025-11-06)


### Features

* **controller:** add defense-in-depth conflict detection for GitDestinations ([d8454e3](https://github.com/ConfigButler/gitops-reverser/commit/d8454e3c88c05d960eb2c9f9375c1183f9565759))


### Bug Fixes

* Get the linter and tests running again ([2df347c](https://github.com/ConfigButler/gitops-reverser/commit/2df347ccddc3e0e8c329e8acccd50b5505b4f190))
* Yes linting is important ([a24f2ca](https://github.com/ConfigButler/gitops-reverser/commit/a24f2ca4c1fecc0bc3ebc194ffea8b8b61c33703))


### Documentation

* Removing finished things ([f2619e6](https://github.com/ConfigButler/gitops-reverser/commit/f2619e6f70b76812c6be4b8ed9d31299738823f6))
* update README, docs images, and e2e test cleanup ([8a3c4e7](https://github.com/ConfigButler/gitops-reverser/commit/8a3c4e70a2f0227b366c2d2961745ea967f4e073))

## [0.9.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.8.2...gitops-reverser-v0.9.0) (2025-11-05)


### Features

* Automatically create branch and handle empty repos ([8a6115a](https://github.com/ConfigButler/gitops-reverser/commit/8a6115a2d33e4a69d7629ee9a613ca7d2f597acf))
* GitDestination now truly handles the branch. ([aac4f3b](https://github.com/ConfigButler/gitops-reverser/commit/aac4f3bcabfd013476a8281d1925ad1046597f74))

## [0.8.2](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.8.1...gitops-reverser-v0.8.2) (2025-10-31)


### Bug Fixes

* Don't break the comments please ([0bf1c47](https://github.com/ConfigButler/gitops-reverser/commit/0bf1c47249bb757969075fc4fc5469a0fcb4ca68))

## [0.8.1](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.8.0...gitops-reverser-v0.8.1) (2025-10-31)


### Bug Fixes

* That should give us a working installation yaml ([f7ec9d4](https://github.com/ConfigButler/gitops-reverser/commit/f7ec9d4c08c5a1de58017dd6be042da3cec99daa))


### Documentation

* Aligning docs ([dc665b1](https://github.com/ConfigButler/gitops-reverser/commit/dc665b1ad1ccd3632eb9b8def1b41fb4a87dbe6c))

## [0.8.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.7.0...gitops-reverser-v0.8.0) (2025-10-31)


### Features

* Add reconcile so that the cluster state is really reflected in git ([906045e](https://github.com/ConfigButler/gitops-reverser/commit/906045eea1de4a274a97ad579f6cb7183c51b4b0))


### Bug Fixes

* **watch:** handle missing kubeconfig gracefully in discovery ([4a7f3a5](https://github.com/ConfigButler/gitops-reverser/commit/4a7f3a5394afc066bc5e43b45788030a5bd16cfb))


### Documentation

* Improve visuals and fix quick start ([#52](https://github.com/ConfigButler/gitops-reverser/issues/52)) ([1e6e950](https://github.com/ConfigButler/gitops-reverser/commit/1e6e950f0fee0d3ad6413eedc1a322e7a79ec81f))

## [0.7.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.6.0...gitops-reverser-v0.7.0) (2025-10-13)


### Features

* Adding badges and a LICENSE (which already was in most source files, so nothing new) ([5ac944e](https://github.com/ConfigButler/gitops-reverser/commit/5ac944eeae63e7fd893ad94a6dc5b080a55ce52d))

## [0.6.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.5.0...gitops-reverser-v0.6.0) (2025-10-13)


### Features

* Build on arm64 infra to speed it up (since we are now open source). ([f3a59f3](https://github.com/ConfigButler/gitops-reverser/commit/f3a59f318ef1addfbeb2437091cf72bceebd67ad))

## [0.5.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.4.0...gitops-reverser-v0.5.0) (2025-10-13)


### Features

* Adding examples in helm chart and install.yaml ([fce3da5](https://github.com/ConfigButler/gitops-reverser/commit/fce3da59dd1503413f895d16757198e311415403))
* Allows safe reuse of GitRepoConfig and adds ClusterWatchRule ([148eb08](https://github.com/ConfigButler/gitops-reverser/commit/148eb08875f17e2a4018ce06dd031ec44152ef53))
* Doing a first throw on clusterwatchrule ([fc73048](https://github.com/ConfigButler/gitops-reverser/commit/fc730484a9377602231582da55e0b31d1cd1938b))
* Fix helm pushing ([#40](https://github.com/ConfigButler/gitops-reverser/issues/40)) ([639edba](https://github.com/ConfigButler/gitops-reverser/commit/639edbaf4f98b4b4cd604cd24e88130231c68bba))
* Have the same gitRepoConfigRef for both WatchRule and ClusterWatchRule. ([b592130](https://github.com/ConfigButler/gitops-reverser/commit/b592130f1ac4e69c7ba4b2426fe6a64b4d165fc4))
* Implementing various improvements ([8fad173](https://github.com/ConfigButler/gitops-reverser/commit/8fad1737d6484f14ec259c651edb80d26b41ac40))
* Working on new designs and other improvements ([109a71d](https://github.com/ConfigButler/gitops-reverser/commit/109a71d1fc15fd9c613530f8ebb94c6e99f98e64))


### Bug Fixes

* Failing pushes to GH (improving SSH key handling). Less verbose on events that we don't act upon, allowing debug inside devcontainer. ([5739420](https://github.com/ConfigButler/gitops-reverser/commit/573942027fe934a3253351ec6617744c965506ae))
* Getting the end2end tests at least running ([ad7de89](https://github.com/ConfigButler/gitops-reverser/commit/ad7de89e528b026bc733716f24a1354860239cf4))
* Let's cleanup the webhook stuff ([99dd4f9](https://github.com/ConfigButler/gitops-reverser/commit/99dd4f9202850a6e7e4a8a002e81b75b8b686df8))
* Make more explicit which generated files are used in helm chart (don't want to forget it again). ([3621133](https://github.com/ConfigButler/gitops-reverser/commit/362113379d0b2fd7932f8ce7b22dce250c013e94))
* Support resource deletion ([9ae3ca6](https://github.com/ConfigButler/gitops-reverser/commit/9ae3ca63f225f92bb046046962ecc8ecd9de2e15))


### Documentation

* Simpler readme for now ([bdc2ccd](https://github.com/ConfigButler/gitops-reverser/commit/bdc2ccdafe64796e43e03ae0786abff143ea57d9))

## [0.4.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.3.0...gitops-reverser-v0.4.0) (2025-10-07)


### Features

* Deploy the helm chart ([#36](https://github.com/ConfigButler/gitops-reverser/issues/36)) ([797ff02](https://github.com/ConfigButler/gitops-reverser/commit/797ff025bf7e9436ed96d8c1896b4d16451c144c))


### Bug Fixes

* Allowing more control, and don't allow running without webhooks (altough you still can disable the ValidatingWebhookConfiguration). ([27ee09a](https://github.com/ConfigButler/gitops-reverser/commit/27ee09a678ca2a6ebe46aee17dc064a071cc96f5))
* Improve the helm chart ([4001e8e](https://github.com/ConfigButler/gitops-reverser/commit/4001e8e20989c6711e92199a3cdb2c6056616a1c))
* SA didnt had Namespace set ([6d83b28](https://github.com/ConfigButler/gitops-reverser/commit/6d83b28796d1eeb41c4ab29af99203ca9a42ed3e))
* Simplify helm chart to start ([a0cc1cd](https://github.com/ConfigButler/gitops-reverser/commit/a0cc1cda6466c5d41de12da0ea7a89b2c40ac7d2))
* Testing the HA behaviour (no edge cases yet, like deployments) ([#35](https://github.com/ConfigButler/gitops-reverser/issues/35)) ([41b17c2](https://github.com/ConfigButler/gitops-reverser/commit/41b17c209f1efaf590a5793bd8f959488da7b9eb))
* Thanks linter ([b934475](https://github.com/ConfigButler/gitops-reverser/commit/b9344757197aa89abc73057dd1279cee7a42048e))


### Documentation

* **helm:** rewrite chart README for better user experience ([de4c9a1](https://github.com/ConfigButler/gitops-reverser/commit/de4c9a13ceafc9ab07b1dfc22cafb39fd54af593))

## [0.3.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.2.1...gitops-reverser-v0.3.0) (2025-10-02)


### Features

* Time for devcontainers! ([#34](https://github.com/ConfigButler/gitops-reverser/issues/34)) ([09b1936](https://github.com/ConfigButler/gitops-reverser/commit/09b193604460f1d9f637e5b7b030ae5488bdb8b4))


### Bug Fixes

* Getting our todo a bit more cleaned, and see if this triggers a release proposal ([a5a6e4a](https://github.com/ConfigButler/gitops-reverser/commit/a5a6e4af4922562648d8d311f8ec52d72bc2b79b))

## [0.2.1](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.2.0...gitops-reverser-v0.2.1) (2025-09-30)


### Bug Fixes

* Normal build times for releases ([#30](https://github.com/ConfigButler/gitops-reverser/issues/30)) ([197c329](https://github.com/ConfigButler/gitops-reverser/commit/197c329119d42e50b549b07f8b1635d5ae19d2e9))

## [0.2.0](https://github.com/ConfigButler/gitops-reverser/compare/gitops-reverser-v0.1.0...gitops-reverser-v0.2.0) (2025-09-30)


### Features

* Checks for repos / status updates ([12b0854](https://github.com/ConfigButler/gitops-reverser/commit/12b0854655a0f856bb1f3aab488085efc2a6088b))
* Fixing the last test hopefully ([0aae945](https://github.com/ConfigButler/gitops-reverser/commit/0aae9452e39a5f99fc94f5650ff30149afd22914))
* Fixing the ssh tests ([8165a23](https://github.com/ConfigButler/gitops-reverser/commit/8165a2323063405c1aefee0de089a378a7c02b8e))
* Getting the linters happy ([a14b335](https://github.com/ConfigButler/gitops-reverser/commit/a14b335bc6b75d9363825dcde9a3f157f14ef4ef))
* Let's allow releasing of this stuff ([a661dd2](https://github.com/ConfigButler/gitops-reverser/commit/a661dd23f3c174a90210a19e80abee83c5d65fc6))
* Let's run a gitea server in our end2end ([4d6c505](https://github.com/ConfigButler/gitops-reverser/commit/4d6c50581c333cef3c0ead7b2b3fa810451cad01))
* More clarity in naming and wrong url ([e223c09](https://github.com/ConfigButler/gitops-reverser/commit/e223c090de0e49b5c0b923a4fc8ea6ba81c39aa8))
* Would this be the first time? ([956a3f0](https://github.com/ConfigButler/gitops-reverser/commit/956a3f0a9b832eb7e0a5a3cc4aa5ad86076bd4eb))
* Would this finally get us a green end2end in github? ([b301353](https://github.com/ConfigButler/gitops-reverser/commit/b301353bada6e80bea98a7f709267eb97146d0fb))


### Bug Fixes

* Another fix ([d5e4f9d](https://github.com/ConfigButler/gitops-reverser/commit/d5e4f9d117ef96c673177b596432766a5737dc2e))
* Let's see if we can make things faster ([8c6fdc4](https://github.com/ConfigButler/gitops-reverser/commit/8c6fdc4305877cf380bd758755f82d35dbd26d31))
* Lower the cyclomatic complexity. ([2a8c6f4](https://github.com/ConfigButler/gitops-reverser/commit/2a8c6f460f29706e50037bb1c4d1d4c01edbd23d))
* Mutating webhooks are now processed ([bbef5bd](https://github.com/ConfigButler/gitops-reverser/commit/bbef5bd2f97a995646899001f21928cf63d6585d))
* Now the e2e test should work! ([47a986b](https://github.com/ConfigButler/gitops-reverser/commit/47a986b7349a3a8c6c1c94bd413911093cdaa672))
* Remove the E2E_TESTING madness ([cc36d93](https://github.com/ConfigButler/gitops-reverser/commit/cc36d934fb73748188ecf201243589a31b576063))
* resolve CI failures by fixing Kustomize setup and updating actions ([d12c4af](https://github.com/ConfigButler/gitops-reverser/commit/d12c4afaf6d923eb555a560cf37612ad08259433))
* See if this fixes the troubles ([f00d566](https://github.com/ConfigButler/gitops-reverser/commit/f00d56603adf2c24570a2ac6649392ac15f9d793))
* That should actually help the build to succeeed! ([517a13c](https://github.com/ConfigButler/gitops-reverser/commit/517a13c7f9f77bf22b17cd67ea303ae906c30c3e))


### Documentation

* More info on deploying this thing ([61eb197](https://github.com/ConfigButler/gitops-reverser/commit/61eb1975c3a59ed9e0138377312c4757cdd75956))

## [0.1.0] - 2025-01-31

### Features

* **Initial Release** - GitOps Reverser operator with core functionality
* **Admission Webhooks** - Capture manual cluster changes in real-time
* **Git Integration** - Automatic commit and push to Git repositories
* **WatchRule CRD** - Flexible rule-based resource monitoring
* **GitRepoConfig CRD** - Git repository configuration management
* **Race Condition Handling** - Intelligent conflict resolution with "last writer wins" strategy
* **Sanitization Engine** - Clean and format Kubernetes manifests before commit
* **Event Queue** - Buffer and batch changes for efficient processing
* **OpenTelemetry Metrics** - Comprehensive observability and monitoring
* **Multi-platform Support** - Docker images for linux/amd64 and linux/arm64
* **Helm Chart** - Easy deployment with configurable values
* **Comprehensive Testing** - Unit tests (>90% coverage), integration tests, and e2e tests

### Documentation

* Complete README with usage examples and architecture diagrams
* Contributing guidelines with development setup
* Testing documentation covering all test types
* Webhook setup guide for production deployments

---

**Note:** This changelog will be automatically updated by [release-please](https://github.com/googleapis/release-please) based on [Conventional Commits](https://www.conventionalcommits.org/). Future releases will have their changes automatically documented here.
