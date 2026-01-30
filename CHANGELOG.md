# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
