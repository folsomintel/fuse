# Changelog

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
