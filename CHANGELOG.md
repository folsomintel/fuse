# Changelog

## [0.6.0](https://github.com/folsomintel/fuse/compare/v0.5.0...v0.6.0) (2026-07-17)


### Features

* **cli:** add support for --no-verify flag to connect and quickstart commands ([b43f7b2](https://github.com/folsomintel/fuse/commit/b43f7b254eeff90a07747811e3700af8400c5a19))

## [0.5.0](https://github.com/folsomintel/fuse/compare/v0.4.0...v0.5.0) (2026-07-17)


### Features

* **docs:** fix build ([af462c8](https://github.com/folsomintel/fuse/commit/af462c8ef849be8267d1b87304f574365cb65349))
* **orchestrator:** add /v1/version endpoint and 404 route disambiguation ([aca3600](https://github.com/folsomintel/fuse/commit/aca3600a23afad062f0f18eb4e1f49b1aac3eb58))


### Bug Fixes

* **ci:** wire NPM_TOKEN into npm publish step ([29fb525](https://github.com/folsomintel/fuse/commit/29fb525234a0258379e2f1bdd3b01a7fc1dc62a9))
* **docs:** build the open-next worker so cloudflare deploy succeeds ([c23d1b3](https://github.com/folsomintel/fuse/commit/c23d1b343ce1b875266d1e17a5445e30bf375564))
* **docs:** reduce worker size under cloudflare limit (limit shiki langs, drop dynamic og route) ([5915d1d](https://github.com/folsomintel/fuse/commit/5915d1dc91189495cb5d9c27ef64e16510a56809))
* **host-agent:** update ssh control socket path calculation and reuse logic ([c39ec7b](https://github.com/folsomintel/fuse/commit/c39ec7be0e8aaae6149f7df60f6a8ab224ff057b))

## [0.4.0](https://github.com/folsomintel/fuse/compare/v0.3.0...v0.4.0) (2026-07-15)


### Features

* **api:** expose guest exec and an attach stream over the http api ([827b2a7](https://github.com/folsomintel/fuse/commit/827b2a72feaceae959b90cc49f8cc08f4ea8bb27))
* **cli:** add environment exec and environment shell ([04b2b60](https://github.com/folsomintel/fuse/commit/04b2b605c0a153333b842d88232fc3c4bcdfd88e))
* **fork:** implement firecracker environment fork end to end ([fab5dfa](https://github.com/folsomintel/fuse/commit/fab5dfa67d7abef19f0bd125e2aa88a44e7940e1))
* **fork:** implement firecracker environment fork end to end ([a564a8a](https://github.com/folsomintel/fuse/commit/a564a8a86e424d9d3d6fafc118344617601cf6fa))
* **host-agent:** add a pty attach endpoint and honour exec timeouts ([77a2353](https://github.com/folsomintel/fuse/commit/77a235316f5771c071a4b233e7b05507cd7dc4d6))
* **host-agent:** bootstrap command for one-command host bring-up and self-registration ([e0e00c6](https://github.com/folsomintel/fuse/commit/e0e00c61e14e535f23e16fbcca479f65dfa697a1))
* **host-agent:** probe real capacity instead of trusting operator flags ([8ffaf46](https://github.com/folsomintel/fuse/commit/8ffaf464c331bc28e8f9a0b18b94986dc8e5af71))
* **orchestrator:** give Environment.Exec a faithful result and add an Attacher capability ([76a38ea](https://github.com/folsomintel/fuse/commit/76a38ea976c0250cf8135823d6d47c538654964b))
* probe host capacity from the agent instead of trusting operator-declared flags ([916cd65](https://github.com/folsomintel/fuse/commit/916cd650aac010439eded779f0f770f2f5cfc9e5))
* **sdk/go:** add Environments.Exec and an attach stream client ([c32a299](https://github.com/folsomintel/fuse/commit/c32a299e9991b7102f7a844aef98c2c58b7ad479))
* **sdk:** add exec to the typescript and python sdks ([96dae3b](https://github.com/folsomintel/fuse/commit/96dae3b610008112534c00f49c8f489b583f5516))
* **vms:** exec ([355aa71](https://github.com/folsomintel/fuse/commit/355aa71501e29b1b1136d33a85153570b4874f90))


### Bug Fixes

* **api:** reject an attach without tty=1 with a 400 instead of a 500 ([fa69107](https://github.com/folsomintel/fuse/commit/fa69107667710852ef3c1a197359aec1216775d5))
* avoid stale page-cache corruption on fc-agent snapshot restore ([faa7340](https://github.com/folsomintel/fuse/commit/faa734037fcb689bb033e4bca495ee926fbf1640))
* avoid stale page-cache corruption on fc-agent snapshot restore ([2c0732f](https://github.com/folsomintel/fuse/commit/2c0732f1ff8b0c31d0306331e6d47a7124921d92))
* **exec:** let a long guest command outlive both 60s http ceilings ([b9bc805](https://github.com/folsomintel/fuse/commit/b9bc805f0cfb23576a03d82745aaf55c11a6df7a))
* **host-agent:** clamp oversized resize and preserve empty argv elements ([ebe1413](https://github.com/folsomintel/fuse/commit/ebe14133c243712e45840d05dc8ade62d4f8d68c))
* **host-agent:** guard path components inline so codeql sees the sanitizer ([0d1bc92](https://github.com/folsomintel/fuse/commit/0d1bc92b6b537dfc251936e9c09a47c0b68029a0))
* **host-agent:** inline path-component guard at each call site for codeql ([4183e21](https://github.com/folsomintel/fuse/commit/4183e2194bd27ae7ed212e5673d1d1c3bedc3fa1))
* **host-agent:** realpath containment guard for image and snapshot paths ([a39dbee](https://github.com/folsomintel/fuse/commit/a39dbee2ce9cf3bc791c915abc057a495b205a65))
* **host-agent:** reject path traversal in image and snapshot names ([89d3602](https://github.com/folsomintel/fuse/commit/89d36024c6c08f9402512984ff4ac769f58fcdca))
* **host-agent:** simplify image containment guard to match snapshot guard ([e695bf8](https://github.com/folsomintel/fuse/commit/e695bf8a33f27b67d1470bc307ce98c72a54b26b))
* **host-agent:** validate path components with allowlist to satisfy codeql ([0b72b8e](https://github.com/folsomintel/fuse/commit/0b72b8ed0c80cbae328802142302a3e300f927c5))
* resolve blank page content caused by tabMode top grid collision, fix app branding ([7fbde14](https://github.com/folsomintel/fuse/commit/7fbde14f95ccf81eb38562fb27193b501d0b09ec))
* stage the missed Exec-signature fix and pin do_attach's execvp path to SSH_BASE[0] for codeql ([054284a](https://github.com/folsomintel/fuse/commit/054284a8e4ecfd144b584bdd0b9432b70415fc99))
* stop rootfs bake dying silently at e2fsck and unwedge agent rebuilds ([1ea3e30](https://github.com/folsomintel/fuse/commit/1ea3e302e09edaf81fdd469ca45209df50ef2101))
* stop rootfs bake dying silently at e2fsck and unwedge agent rebuilds ([7b951e4](https://github.com/folsomintel/fuse/commit/7b951e4493a4125de3eab3de4c223e994ec77600))

## [0.3.0](https://github.com/folsomintel/fuse/compare/v0.2.0...v0.3.0) (2026-07-12)


### Features

* add GPU allocation and deallocation tests for fleet hosts ([ecda396](https://github.com/folsomintel/fuse/commit/ecda396c27d912b2dc3cbd4a40092ff7de1e75eb))
* add gpu and backend fields to host and resource spec ([812a7d2](https://github.com/folsomintel/fuse/commit/812a7d26ca46da63408b6e86d350296ba0e9b783))
* add GPU support in Fusefile and CLI commands ([245de35](https://github.com/folsomintel/fuse/commit/245de359d961d01ecf0d982a072f22098cf1355b))
* construct provider from host backend at registration ([c6ee941](https://github.com/folsomintel/fuse/commit/c6ee9416e49f65b73e619c68e0a0d795e840501d))
* enhance GPU scheduling logic and startup script handling ([537b890](https://github.com/folsomintel/fuse/commit/537b890fc902e90317e49bba7a17edbf5358ec7f))
* expose gpu and backend fields in api sdks and cli ([1a6b99c](https://github.com/folsomintel/fuse/commit/1a6b99cce674c17edef08fc1820b19f1e5a60dfe))
* **host-agent:** add e2e tests and service for QEMU host agent ([4794638](https://github.com/folsomintel/fuse/commit/47946381ae3f26477b89478bbc0da2002761c6c0))
* **host-agent:** add QEMU host agent (GPU passthrough) ([9567e8a](https://github.com/folsomintel/fuse/commit/9567e8ab38623cebf9935fa2abea7c4bdac96108))
* **orchestrator:** add tests and guardrails for qemu provider's gpu passthrough ([14d29ac](https://github.com/folsomintel/fuse/commit/14d29acb37adaa4949f54f253231dddc690a714d))
* persist gpu and backend fields in postgres state store ([8c37491](https://github.com/folsomintel/fuse/commit/8c37491ad8ecad992fbe69f1fe1d2de1aba062d1))
* **qemu:** add QEMU provider implementation ([629f73c](https://github.com/folsomintel/fuse/commit/629f73c874c9b808f90086c06a03eceaf323c601))
* **qemu:** add QEMU provider implementation ([81bad92](https://github.com/folsomintel/fuse/commit/81bad92d64508ebf96b17d66321bdcaee8d7428d))
* **qemu:** host agent for gpu passthru ([079028a](https://github.com/folsomintel/fuse/commit/079028a7d54daa15c10026b5d5703db2570b2238))
* schedule gpu envs only onto matching qemu hosts ([9c9b3ee](https://github.com/folsomintel/fuse/commit/9c9b3eedf943800ecb4b11cf10e1330e4027889a))
* **sdk:** update ts verison path ([1e0afaf](https://github.com/folsomintel/fuse/commit/1e0afaf6f0cd128817ed63fbd4672bd25c315ddf))


### Bug Fixes

* enforce gpu host capacity and provider ownership ([3100f9d](https://github.com/folsomintel/fuse/commit/3100f9d8093937ea557c315901341a7caa848f24))
* make qemu gpu host setup locally testable ([8dbc5cc](https://github.com/folsomintel/fuse/commit/8dbc5cc50116fecb6b36b23654d59da3086e7360))
* reconcile vms through their host providers ([8521de4](https://github.com/folsomintel/fuse/commit/8521de4a29f6e921fbd708ad374ea11adb7cb232))
* run bake iptables bundle with host network and add nftables to host deps ([6f9911c](https://github.com/folsomintel/fuse/commit/6f9911c85207cec4a5a54b4024c96178e0c8ff9b))
* validate gpu backends and preserve qemu endpoints ([1db3ba2](https://github.com/folsomintel/fuse/commit/1db3ba28432d01c164eb6dcb686d5d99137be1e9))

## [0.2.0](https://github.com/folsomintel/fuse/compare/v0.1.0...v0.2.0) (2026-07-04)


### Features

* add fuse init scaffolding a fusefile ([45cc11b](https://github.com/folsomintel/fuse/commit/45cc11bdd53fd5edc86f8f6537c4f2bcc4a8c20e))
* add fuse up command compiling a fusefile ([d0a2b73](https://github.com/folsomintel/fuse/commit/d0a2b73e0a75fa4787bb0ad2e588c3b27c2d1f82))
* add fusefile parser with strict validation ([3c3c6d4](https://github.com/folsomintel/fuse/commit/3c3c6d4db3209e60c4df4368e1e056582f7d06d2))
* add fusefile schema types ([c081537](https://github.com/folsomintel/fuse/commit/c081537f449dfc869a1cb76380080c88fcf973a2))
* add ingress and image selection fields to create wire, compiler, and orchestrator ([1e96695](https://github.com/folsomintel/fuse/commit/1e966958ba25b76cd94eb8f59db28d5b6cbdced8))
* **api:** add fork environment action and test ([349510c](https://github.com/folsomintel/fuse/commit/349510c4fedf18141c025fad37b539a617bba710))
* bring up declared services via compose at guest boot ([69fba95](https://github.com/folsomintel/fuse/commit/69fba955288e47a887e976067b313e31f209bdd4))
* **cli:** add environment fork command and test ([62c3e00](https://github.com/folsomintel/fuse/commit/62c3e008fe0f960946dedf99441dd935030c463a))
* compile fusefile resources to resource spec ([c1de2c1](https://github.com/folsomintel/fuse/commit/c1de2c1a11416c8bc9cc633876189e105ecf2c85))
* compile fusefile to manifest and startup script ([d5d7145](https://github.com/folsomintel/fuse/commit/d5d7145b22b6049fc42d839ace4cbfea36ae3f0b))
* **orchestrator:** add support for generating Docker Compose files from Fused manifests and implement ForkEnvironment with snapshotting capabilities ([5d67add](https://github.com/folsomintel/fuse/commit/5d67add219e091bc92a2384de8ec06d0eab7fd1f))
* publish exposed ports via fc-expose at boot ([c2955bd](https://github.com/folsomintel/fuse/commit/c2955bd6b17d17ea5eb2e9f90e9991286761b964))
* **sdk-python:** add image, expose, endpoints, and fork to match go sdk ([031d6c6](https://github.com/folsomintel/fuse/commit/031d6c622991670c57650c0c787445887eed7671))
* **sdk-ts:** add image, expose, endpoints, and fork to match go sdk ([410d6bc](https://github.com/folsomintel/fuse/commit/410d6bc16ec9b49260792e614dc356b199947402))
* **sdks:** bring python and ts sdks to fusefile parity with go ([f663c66](https://github.com/folsomintel/fuse/commit/f663c66cfda208a9a2889fc335ad74dfe187f24f))
* support bring-your-own base image via fusefile ([ad8a989](https://github.com/folsomintel/fuse/commit/ad8a989129eeb01926f4aa700bc978746df5b214))


### Bug Fixes

* resolve golangci-lint staticcheck issues (ST1005, S1016) ([60bc1f2](https://github.com/folsomintel/fuse/commit/60bc1f2c6769e9cb466eba468b16c37dec9853b4))
* resolve golangci-lint staticcheck issues (ST1005, S1016) ([61df731](https://github.com/folsomintel/fuse/commit/61df731d78f4dfe04ea2a56f1b06d41ed7c2a0c6))

## [0.1.0](https://github.com/folsomintel/fuse/compare/v0.0.1...v0.1.0) (2026-06-30)


### Features

* add dbcheck --version flag ([ff33577](https://github.com/folsomintel/fuse/commit/ff33577cf3bcdacfc97e30179d504ea0506abfb8))
* **apikeys:** add API keys service ([c866ffa](https://github.com/folsomintel/fuse/commit/c866ffaa80ae61018151fc79f31274a56518aaa2))
* **auth:** add revocable API keys alongside master token ([3732355](https://github.com/folsomintel/fuse/commit/3732355cd89b54f9106f65f512d35363beda35af))
* **auth:** revocable API keys alongside master token ([47c96d7](https://github.com/folsomintel/fuse/commit/47c96d767344421073ebf13ee7e953ed841aa4cf))
* **cli:** add new commands for managing API keys, hosts, and environments ([b301d4c](https://github.com/folsomintel/fuse/commit/b301d4c6147468d6fa4261a96b6636cd25f31f74))
* **deploy:** add co-located orchestrator install to fc-agent.sh ([3981793](https://github.com/folsomintel/fuse/commit/39817935fd55dcc454a985f615ef8f1ce5a8e78a))
* **fuse:** add Go client for Fuse API ([e215c4b](https://github.com/folsomintel/fuse/commit/e215c4b5dba0ead3c6f2ac4789a2ec08ffbb83dd))
* **go:** add environments service and related endpoints ([e104b3f](https://github.com/folsomintel/fuse/commit/e104b3fe35957d5846ec91578c81fb796959b8d4))
* **go:** add Events method to EnvironmentsService for SSE stream ([4c0956c](https://github.com/folsomintel/fuse/commit/4c0956c39af9d68fa700ff139b0edd446200d761))
* **go:** add integration tests for all API endpoints ([7e40b7e](https://github.com/folsomintel/fuse/commit/7e40b7e205d6f2c57508a678923641d2ca1c561e))
* **python:** add new Python SDK client and core modules ([2ade4f8](https://github.com/folsomintel/fuse/commit/2ade4f804249247474f8282815da51e9968ec61c))
* **python:** add new services for managing API keys, environments, events, hosts, and snapshots ([c4bbcfe](https://github.com/folsomintel/fuse/commit/c4bbcfec799dcf0b12b21c403fd270e8626e6b05))
* **python:** add new types and client for Fuse SDK ([dd7fac2](https://github.com/folsomintel/fuse/commit/dd7fac244003fb039f1b54e5488db8de92c70eac))
* release the operator fuse CLI via goreleaser and a homebrew tap ([d1fbe01](https://github.com/folsomintel/fuse/commit/d1fbe016c894432fd7d78b6ef44e9396bf53f503))
* **sdk-ts:** add publishable TypeScript SDK ([cfc37ce](https://github.com/folsomintel/fuse/commit/cfc37ceb33a202f4d3190aedb7df427558bfe24f))
* **sdk-ts:** add publishable TypeScript SDK ([139df90](https://github.com/folsomintel/fuse/commit/139df90dbcbc30b5dbb4642adcba444ae55f578f))
* **sdk:** API error codes ([13c36f8](https://github.com/folsomintel/fuse/commit/13c36f8e60711833196bb218f32ead8d21a0b3d1))
* **sdk:** init it ([ab0a7e9](https://github.com/folsomintel/fuse/commit/ab0a7e96967ab09024a9355a8a907fb60ce07acc))


### Bug Fixes

* **api-keys:** gofmt and check rows.Close error for lint/CI ([fc7e107](https://github.com/folsomintel/fuse/commit/fc7e10786663c38bfaa4f391ae0311377ba7bb97))
* **apikeys_cmd.go, metrics_cmd:** warn before showing secret keys ([b86927e](https://github.com/folsomintel/fuse/commit/b86927e13d6f5a20d045376243a80d5a56bb1dbe))
* **api:** preserve http.Flusher through metrics middleware for SSE ([4bc0a8d](https://github.com/folsomintel/fuse/commit/4bc0a8dc9d30dd9effae02601870b9aa85704612))
* **api:** preserve http.Flusher through metrics middleware for SSE ([61e01f5](https://github.com/folsomintel/fuse/commit/61e01f510de03dfa5eb84f1f01cfb0e71d32e68f))
* derive Go SDK version from build info ([5ba79de](https://github.com/folsomintel/fuse/commit/5ba79dee9312a42a585f0acb732c62827de87fb5))
* derive Python SDK version from package metadata ([12d9404](https://github.com/folsomintel/fuse/commit/12d9404d079dc3b21ffc21c99df5ad5b8b435e16))
* generate TS SDK version from package.json to kill user-agent drift ([3db8131](https://github.com/folsomintel/fuse/commit/3db813123ba99deff892adfb4b234dcc402b5363))
* make CLI version a stampable var ([1b2bc46](https://github.com/folsomintel/fuse/commit/1b2bc469618da8978219bc64fd923f1512e0ad81))
