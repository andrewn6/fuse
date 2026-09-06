# Changelog

## [0.29.3](https://github.com/folsomintel/fuse/compare/v0.29.2...v0.29.3) (2026-09-04)


### Bug Fixes

* **local:** probe leases before use, verify pid identity, and honor version on reinstall ([#232](https://github.com/folsomintel/fuse/issues/232)) ([29c7ed9](https://github.com/folsomintel/fuse/commit/29c7ed93cd81d9803a5dd18cf2145d64e7730400))

## [0.29.2](https://github.com/folsomintel/fuse/compare/v0.29.1...v0.29.2) (2026-09-02)


### Performance Improvements

* probe tcp before forking ssh in guest readiness wait ([#230](https://github.com/folsomintel/fuse/issues/230)) ([3c7816a](https://github.com/folsomintel/fuse/commit/3c7816a828ecc42ac55d8a56cd746557cfbbe6bd))
* quiet guest kernel boot log to cut serial console time ([#229](https://github.com/folsomintel/fuse/issues/229)) ([bfaaba4](https://github.com/folsomintel/fuse/commit/bfaaba482f907ef0787ec3f2e47f9f2e17b49f4f))

## [0.29.1](https://github.com/folsomintel/fuse/compare/v0.29.0...v0.29.1) (2026-08-23)


### Bug Fixes

* **orchestrator:** reject fork and seed from a live snapshot instead of failing at boot ([#221](https://github.com/folsomintel/fuse/issues/221)) ([5502c77](https://github.com/folsomintel/fuse/commit/5502c7724ce919c031cba31a378aadefb0fa6ad2))
* **test:** base64-decode exec stdout in the live snapshot e2e ([#220](https://github.com/folsomintel/fuse/issues/220)) ([01881d7](https://github.com/folsomintel/fuse/commit/01881d7824e83bdda4fdbba3d167e0d4f8fdd2a4))

## [0.29.0](https://github.com/folsomintel/fuse/compare/v0.28.0...v0.29.0) (2026-08-23)


### Features

* **fc-agent:** live memory snapshots and in-place resume ([#216](https://github.com/folsomintel/fuse/issues/216)) ([f79b8cb](https://github.com/folsomintel/fuse/commit/f79b8cb075d8bc0822789a774dcc306719b552c0))
* **snapshots:** surface live snapshots through the api, cli and sdks ([#217](https://github.com/folsomintel/fuse/issues/217)) ([499800e](https://github.com/folsomintel/fuse/commit/499800e81ce20569af53edce586f24e3bb0100a8))

## [0.28.0](https://github.com/folsomintel/fuse/compare/v0.27.0...v0.28.0) (2026-08-19)


### Features

* **desktop:** live view of the desktop with human takeover ([#207](https://github.com/folsomintel/fuse/issues/207)) ([480215e](https://github.com/folsomintel/fuse/commit/480215e8ce24132aad00c270e6349775b7b17c98))


### Bug Fixes

* keep firecracker api sockets under a short path to respect sun_len ([#214](https://github.com/folsomintel/fuse/issues/214)) ([566b22d](https://github.com/folsomintel/fuse/commit/566b22d7eafe522c1d0bed308f81f9f4f82ed352))

## [0.27.0](https://github.com/folsomintel/fuse/compare/v0.26.0...v0.27.0) (2026-08-16)


### Features

* ent orm spike for the orchestrator state store ([#206](https://github.com/folsomintel/fuse/issues/206)) ([f5fbe66](https://github.com/folsomintel/fuse/commit/f5fbe66dc7d673ec9fed4fde8810e3ec540b6bb3))

## [0.26.0](https://github.com/folsomintel/fuse/compare/v0.25.0...v0.26.0) (2026-08-16)


### Features

* **sdks:** rust sdk ([#200](https://github.com/folsomintel/fuse/issues/200)) ([a446ae6](https://github.com/folsomintel/fuse/commit/a446ae6967792e8be20b8f34c461c25257f43b46))

## [0.25.0](https://github.com/folsomintel/fuse/compare/v0.24.0...v0.25.0) (2026-08-16)


### Features

* **cli:** fuse computer commands and the desktop detail row ([#198](https://github.com/folsomintel/fuse/issues/198)) ([e20145d](https://github.com/folsomintel/fuse/commit/e20145daeb4ad74de974ac6ac4c9a5b96db017b2))


### Bug Fixes

* **host-agent:** four bugs in the desktop rootfs bake, found live-verifying computer use ([#197](https://github.com/folsomintel/fuse/issues/197)) ([1565428](https://github.com/folsomintel/fuse/commit/1565428051b7432891cff6848f7d13737513dea1))

## [0.24.0](https://github.com/folsomintel/fuse/compare/v0.23.0...v0.24.0) (2026-08-15)


### Features

* desktop block in the fusefile and the computer wire ([#191](https://github.com/folsomintel/fuse/issues/191)) ([fef55df](https://github.com/folsomintel/fuse/commit/fef55df8b27dc6049b4639792f17870b3aa5a4eb))
* **sdks:** computer use adapter, worked example, and guide ([#193](https://github.com/folsomintel/fuse/issues/193)) ([52fdc23](https://github.com/folsomintel/fuse/commit/52fdc23dbd9df5ca7c9b5e180651a83311ca3a1a))

## [0.23.0](https://github.com/folsomintel/fuse/compare/v0.22.2...v0.23.0) (2026-08-15)


### Features

* **fused:** computer control surface for the desktop image ([#190](https://github.com/folsomintel/fuse/issues/190)) ([6a72abd](https://github.com/folsomintel/fuse/commit/6a72abd77baba08e8e5f6d48ce2b94b8fcc2eaa5))
* **host-agent:** bake a desktop rootfs for computer use ([#189](https://github.com/folsomintel/fuse/issues/189)) ([141c1e0](https://github.com/folsomintel/fuse/commit/141c1e0d7062dd19c5302934eeb560df875d8071))

## [0.22.2](https://github.com/folsomintel/fuse/compare/v0.22.1...v0.22.2) (2026-08-15)


### Bug Fixes

* **ci:** always clear the release label, without needing read:org ([09b968f](https://github.com/folsomintel/fuse/commit/09b968f741ca6262ed160df8d22ba8241d86ddef))

## [0.22.1](https://github.com/folsomintel/fuse/compare/v0.22.0...v0.22.1) (2026-08-15)


### Bug Fixes

* **host-agent:** point compose at podman's socket so services can start ([25b5692](https://github.com/folsomintel/fuse/commit/25b56928dca808d0e0c4bb9fb1015d65f8d93665))
* **host-agent:** write /fuse credentials readable by root only ([25cbd9c](https://github.com/folsomintel/fuse/commit/25cbd9cfd8159925eddcf9c2ce38b4eeab666f4a))
* **orchestrator:** name the gpu when a gpu-less fleet refuses a gpu request ([2d16005](https://github.com/folsomintel/fuse/commit/2d160053ba75ee904536656e2e240fbdc3f8d832))

## [0.22.0](https://github.com/folsomintel/fuse/compare/v0.21.0...v0.22.0) (2026-08-15)


### Features

* **fusefile:** add a top-level name that sets the task id ([#179](https://github.com/folsomintel/fuse/issues/179)) ([b712c06](https://github.com/folsomintel/fuse/commit/b712c0643503cf68e095b59f90a84e51c043b591))

## [0.21.0](https://github.com/folsomintel/fuse/compare/v0.20.0...v0.21.0) (2026-08-15)


### Features

* **api:** accept a files map on create and upload it before the startup script ([b674e5c](https://github.com/folsomintel/fuse/commit/b674e5c17ec95e4050f69703d156548bb4bec8dc))
* **cli:** add .fuseignore to bound what the copy block ships ([#177](https://github.com/folsomintel/fuse/issues/177)) ([02888f7](https://github.com/folsomintel/fuse/commit/02888f7d3037d3050a3f9e4fb72495e3fffb2476))
* **cli:** scaffold the build block and label steps from it ([8f6f9b2](https://github.com/folsomintel/fuse/commit/8f6f9b2af401f02194c43f35958d73b6967d2edb))
* **cli:** walk copy sources into guest files and send them with create ([5823893](https://github.com/folsomintel/fuse/commit/5823893647f0e8be7b02fbac2f9d67a76d1b8923))
* **fusefile:** add a copy block for shipping local files into the guest ([ddce129](https://github.com/folsomintel/fuse/commit/ddce1297cf189ec613b6a619b3982cad0298d9f8))
* **fusefile:** add a copy block with from/to entries to the schema and validator ([a84bda5](https://github.com/folsomintel/fuse/commit/a84bda545bf1668b5385ea49388247be1ad08499))
* **fusefile:** add a top-level build block ([72ded77](https://github.com/folsomintel/fuse/commit/72ded7764558e5b8207357b177b64b75c574e9bc))
* **fusefile:** add a top-level build block with setup as a deprecated alias ([b60c23b](https://github.com/folsomintel/fuse/commit/b60c23bff0e3a758a7c81795b563f58799e4f80c))
* **fusefile:** add a top-level env block for setup and run ([96764b7](https://github.com/folsomintel/fuse/commit/96764b75913a525f0e66e89d35d01f874e554a0b))
* **fusefile:** add a top-level env block to the authoring schema ([fde8b8e](https://github.com/folsomintel/fuse/commit/fde8b8e7a6b114dbcc31172af10bb690d282b220))
* **fusefile:** add an environment-level healthcheck probe ([#176](https://github.com/folsomintel/fuse/issues/176)) ([6921acb](https://github.com/folsomintel/fuse/commit/6921acb6ae7927771e6a8de4c0883ee3cc1858f0))
* **fusefile:** carry the env block in the manifest and source it from the boot script ([19c0616](https://github.com/folsomintel/fuse/commit/19c06161f8ba46f0a0bf45058f2c1045d5ed74a6))
* **fusefile:** compile the build steps as a distinct phase of the startup script ([256b792](https://github.com/folsomintel/fuse/commit/256b792377a2be4e0a0a1da9d24e40299b1d449c))
* **fusefile:** resolve copy entries into absolute guest paths at compile time ([d91c9b8](https://github.com/folsomintel/fuse/commit/d91c9b8516bbb3dede95e7240c6d08770c845fe1))
* **fusefile:** validate top-level env keys and the value-or-secret pair ([23ef188](https://github.com/folsomintel/fuse/commit/23ef188c99beb4982fe45e58dfa7aa94329b58ed))
* **orchestrator:** render the manifest env block to /fuse/env with secrets resolved ([269ed90](https://github.com/folsomintel/fuse/commit/269ed901c6c040d304debeb5d0b0a8a70b823bcf))


### Bug Fixes

* **orchestrator:** re-attach in-flight provisions on restart + reclaim orphaned host allocation ([#68](https://github.com/folsomintel/fuse/issues/68)) ([#172](https://github.com/folsomintel/fuse/issues/172)) ([4ab7590](https://github.com/folsomintel/fuse/commit/4ab759064b27fc1c0fc3b0a0678b92d6f6ba2ec0))

## [0.20.0](https://github.com/folsomintel/fuse/compare/v0.19.0...v0.20.0) (2026-08-14)


### Features

* **fusefile:** accept exec-form command lists for `run` ([#170](https://github.com/folsomintel/fuse/issues/170)) ([fe83049](https://github.com/folsomintel/fuse/commit/fe83049bfc93b164f0d196b6b35553c932e33685)), closes [#91](https://github.com/folsomintel/fuse/issues/91)


### Bug Fixes

* clear the open security alerts (CodeQL + dependabot) ([#167](https://github.com/folsomintel/fuse/issues/167)) ([c454452](https://github.com/folsomintel/fuse/commit/c4544522b0d20ca06468513e165e21db05250b80))
* **host-agent:** return the artifact digest on snapshot create ([#169](https://github.com/folsomintel/fuse/issues/169)) ([94e6207](https://github.com/folsomintel/fuse/commit/94e6207fca4810187f65d25bdce87e1efe37d2ea))

## [0.19.0](https://github.com/folsomintel/fuse/compare/v0.18.0...v0.19.0) (2026-08-14)


### Features

* **api:** expose layer keys, arch filters and a resolve endpoint ([6de04bf](https://github.com/folsomintel/fuse/commit/6de04bf9db2b0e5567b3f3a87aacd80be647a6de))
* **cli:** build and boot from the setup layer cache ([ac05d40](https://github.com/folsomintel/fuse/commit/ac05d408a4798a0068e79d65cd38d6afef1245b9))
* content-addressed build layers with peer-to-peer artifact transfer ([d438ac8](https://github.com/folsomintel/fuse/commit/d438ac808a9111f075cd63a48feb2807aba001e4))
* **fusefile:** render setup steps as ranges, not only as one blob ([edc6a83](https://github.com/folsomintel/fuse/commit/edc6a833e721ca397b97c7a8c3275f842f4ca39e))
* **fusefile:** take arch out of the layer key preimage ([4112653](https://github.com/folsomintel/fuse/commit/4112653d3682964b2395c0225085d208c696243b))
* **host-agent:** hash artifacts and serve them to peer agents ([0c011bb](https://github.com/folsomintel/fuse/commit/0c011bb7826718b5c22a9aa1fa872335a94e9fc0))
* **hostwire:** mint artifact grants and verify streamed transfers ([84089e7](https://github.com/folsomintel/fuse/commit/84089e761d73e7c39c90be8e99e2e564831b4a5e))
* **orchestrator:** persist and look up build layers by cache key ([8733afb](https://github.com/folsomintel/fuse/commit/8733afb19c1a97e22fa8fc3cab57b6599c3c29f9))
* **orchestrator:** prefer hosts holding an artifact and pull when none can ([d64b77f](https://github.com/folsomintel/fuse/commit/d64b77f1444f23beb10028a738c56bb38f917a01))
* **sdks:** add the layer resolve and label operations ([939c938](https://github.com/folsomintel/fuse/commit/939c938c99d41c3690a5c19b773e30fceaa34497))
* **sdks:** mirror layer key, arch and digest across all three sdks ([3312bd3](https://github.com/folsomintel/fuse/commit/3312bd3188fa8cf5f5a0ed8b0fb34bce0706e2b2))


### Bug Fixes

* **host-agent:** keep request data out of artifact paths and headers ([a0b736e](https://github.com/folsomintel/fuse/commit/a0b736e505963ae41c7061f49fa527beb189749a))
* **orchestrator:** file layer artifacts under the caller's scope ([4d5bfe6](https://github.com/folsomintel/fuse/commit/4d5bfe6492f8a98367c460f600976c36d03ef794))

## [0.18.0](https://github.com/folsomintel/fuse/compare/v0.17.0...v0.18.0) (2026-08-10)


### Features

* **cli:** explain startup script timeouts instead of relaying invalid argument ([5670404](https://github.com/folsomintel/fuse/commit/56704045cfcad6049d6e415614ee30588dd76092))
* **cli:** harden `fuse up` for the v2 authoring surface ([6508108](https://github.com/folsomintel/fuse/commit/6508108259c72970cace2269893da30af2852985))

## [0.17.0](https://github.com/folsomintel/fuse/compare/v0.16.0...v0.17.0) (2026-08-10)


### Features

* **cli:** adaptive colors, progress bar, and status dashboard ([e7c114f](https://github.com/folsomintel/fuse/commit/e7c114f9dff01f77a0f37cb17f86d48c3ce4e55a))
* **cli:** adaptive colors, provisioning progress bar, and status dashboard ([04a72b6](https://github.com/folsomintel/fuse/commit/04a72b6472cfc965abb3d773210702ae6d1e43c4))
* cursor-paginate list endpoints, drop CreateSnapshot full-table scan ([7a804a1](https://github.com/folsomintel/fuse/commit/7a804a1204919c76fab4a49707b4ac0f5993105e))


### Bug Fixes

* address codeql alerts in host agents ([c2f3f06](https://github.com/folsomintel/fuse/commit/c2f3f065abe36ef0c4b3578808ad8fb3003db73b))

## [0.16.0](https://github.com/folsomintel/fuse/compare/v0.15.0...v0.16.0) (2026-08-02)


### Features

* local dev with the real stack (fuse local) + arch-aware scheduling ([#157](https://github.com/folsomintel/fuse/issues/157)) ([4ffe689](https://github.com/folsomintel/fuse/commit/4ffe6898c1197c5713cebc85bf1dd45cff3a2232))

## [0.15.0](https://github.com/folsomintel/fuse/compare/v0.14.0...v0.15.0) (2026-08-02)


### Features

* **fusefile:** Finish workspace semantics with per-step workdir ([#155](https://github.com/folsomintel/fuse/issues/155)) ([27f63c1](https://github.com/folsomintel/fuse/commit/27f63c10df962f61456174cc9517dffad54e9d0a))

## [0.14.0](https://github.com/folsomintel/fuse/compare/v0.13.1...v0.14.0) (2026-07-31)


### Features

* **fusefile:** close the remaining validation gaps in parse and compile ([#149](https://github.com/folsomintel/fuse/issues/149)) ([e3b6402](https://github.com/folsomintel/fuse/commit/e3b640210f7b773dd94f80e8730e7e7e076b8942))
* **fusefile:** publish a json schema for editor autocomplete and inline validation ([#151](https://github.com/folsomintel/fuse/issues/151)) ([60a0309](https://github.com/folsomintel/fuse/commit/60a0309cb57de896f004974330a03eb90f8b11b2))
* **fusefile:** unify the cpu, memory, disk, and gpu resource syntax ([#150](https://github.com/folsomintel/fuse/issues/150)) ([5dedda0](https://github.com/folsomintel/fuse/commit/5dedda04f99876666d08751dc78ef7db45186e5a))


### Bug Fixes

* **orchestrator:** register forks in fm.vms as provisioning before create so reconcile cannot orphan-destroy them ([#148](https://github.com/folsomintel/fuse/issues/148)) ([dbb1f72](https://github.com/folsomintel/fuse/commit/dbb1f720ee028842e449164e64dce809bbd50c66))

## [0.13.1](https://github.com/folsomintel/fuse/compare/v0.13.0...v0.13.1) (2026-07-31)


### Bug Fixes

* **host-agent:** bracket IPv6 and prefer IPv4 when composing host:port URLs ([#146](https://github.com/folsomintel/fuse/issues/146)) ([ce167e9](https://github.com/folsomintel/fuse/commit/ce167e9e8f64f38dfa6c0616a94beae76d25d846))

## [0.13.0](https://github.com/folsomintel/fuse/compare/v0.12.0...v0.13.0) (2026-07-31)


### Features

* **fusefile:** add region field to resources block ([#143](https://github.com/folsomintel/fuse/issues/143)) ([e5760a9](https://github.com/folsomintel/fuse/commit/e5760a93a221b8a99d4b6324301506dcfa5bcdbd))
* **fusefile:** expose compose runtime fields on services ([#144](https://github.com/folsomintel/fuse/issues/144)) ([d040a85](https://github.com/folsomintel/fuse/commit/d040a85a0a1eb0ae5069c22bbad6c3536c75bb52))

## [0.12.0](https://github.com/folsomintel/fuse/compare/v0.11.0...v0.12.0) (2026-07-31)


### Features

* **cli:** add fuse validate to check a Fusefile offline ([#130](https://github.com/folsomintel/fuse/issues/130)) ([5e2d72d](https://github.com/folsomintel/fuse/commit/5e2d72d3bbe5ce9cd80b090617546fdacef9c6b5))
* **fusefile:** add files and startup_timeout, and correct the image examples ([#137](https://github.com/folsomintel/fuse/issues/137)) ([259ca9d](https://github.com/folsomintel/fuse/commit/259ca9da41f9344efe577b33b551d3f82d94fc6f))

## [0.11.0](https://github.com/folsomintel/fuse/compare/v0.10.0...v0.11.0) (2026-07-30)


### Features

* **cli:** add fuse compile to print what a Fusefile compiles into ([#131](https://github.com/folsomintel/fuse/issues/131)) ([b0e600e](https://github.com/folsomintel/fuse/commit/b0e600e6b95eb5f4cd93bc32cd3cc908a19712a4))
* **cli:** report per-step cache state, timing, and miss reason in fuse up ([#132](https://github.com/folsomintel/fuse/issues/132)) ([5c437dc](https://github.com/folsomintel/fuse/commit/5c437dc3bf83c045a51f0a0c67a356df765d3805))
* **fusefile:** add placement block for host pinning and label selectors ([#136](https://github.com/folsomintel/fuse/issues/136)) ([420ca17](https://github.com/folsomintel/fuse/commit/420ca17fbe05b7c3b1ce17cbf7574aa2589fa29b))
* **fusefile:** add resources.idle_timeout ([#135](https://github.com/folsomintel/fuse/issues/135)) ([7b3c3e6](https://github.com/folsomintel/fuse/commit/7b3c3e6ad608c05bcbe79a0d989daf5e68b69b92))
* **fusefile:** content-addressed layer cache keys for setup steps ([#133](https://github.com/folsomintel/fuse/issues/133)) ([a6b8107](https://github.com/folsomintel/fuse/commit/a6b8107c3084caac01fade09a58cfacc802208cf))

## [0.10.0](https://github.com/folsomintel/fuse/compare/v0.9.2...v0.10.0) (2026-07-30)


### Features

* **cli:** add `fuse build` to run the build phase separately from `fuse up` ([#128](https://github.com/folsomintel/fuse/issues/128)) ([aa175ed](https://github.com/folsomintel/fuse/commit/aa175edfebded26055b001adf97f35df1f30b975))

## [0.9.2](https://github.com/folsomintel/fuse/compare/v0.9.1...v0.9.2) (2026-07-29)


### Bug Fixes

* bound request bodies and slow clients across orchestrator and host agents ([#119](https://github.com/folsomintel/fuse/issues/119)) ([70387d8](https://github.com/folsomintel/fuse/commit/70387d81bd4c82f7a2006ce463bfd32054e249e3))
* clear dependabot and CodeQL security alerts ([#88](https://github.com/folsomintel/fuse/issues/88)) ([bac883a](https://github.com/folsomintel/fuse/commit/bac883a35390671a2d6fe6e642bcf04d8c6704fa))
* contain qemu image paths to prevent cross-vm rootfs reads ([#117](https://github.com/folsomintel/fuse/issues/117)) ([c91dd27](https://github.com/folsomintel/fuse/commit/c91dd273cc34b450397cc63247f24712302f509f))
* patch reachable go vulnerabilities and gate ci on govulncheck ([#118](https://github.com/folsomintel/fuse/issues/118)) ([f0d43ca](https://github.com/folsomintel/fuse/commit/f0d43cab7e0564a06b72fdae2c0035fd285ea18d))
* preserve secure session cookies behind tls proxies and fail closed on partial tls config ([#120](https://github.com/folsomintel/fuse/issues/120)) ([5a24a58](https://github.com/folsomintel/fuse/commit/5a24a5836f52f3dde11a598b1e26bf33d97307d7))
* remove the duplicate govulncheck job that made ci.yml unparseable ([#124](https://github.com/folsomintel/fuse/issues/124)) ([4ad3d7e](https://github.com/folsomintel/fuse/commit/4ad3d7ec927409436007060e493e462287a7ff84))
* restrict host mutations and harden provider egress ([#121](https://github.com/folsomintel/fuse/issues/121)) ([6d96522](https://github.com/folsomintel/fuse/commit/6d9652265dba44116d2de53e0f89a555dc961c31))
* **security:** patch reachable go vulns and verify release asset checksums ([#110](https://github.com/folsomintel/fuse/issues/110)) ([7198070](https://github.com/folsomintel/fuse/commit/719807072099ae99dd41408b1de674cc526b592e))
* stop gatekeeper blocking the brew-installed cli on macos ([#123](https://github.com/folsomintel/fuse/issues/123)) ([f399b9e](https://github.com/folsomintel/fuse/commit/f399b9eed2eb8ec0bbc5d2a07e0feb35325715ee))
* verify goreleaser checksums before installing release assets ([#122](https://github.com/folsomintel/fuse/issues/122)) ([48a3062](https://github.com/folsomintel/fuse/commit/48a30622ac1f3ef78a81a3cae4bf09c48fa01e6e))

## [0.9.1](https://github.com/folsomintel/fuse/compare/v0.9.0...v0.9.1) (2026-07-22)


### Bug Fixes

* snapshot delete deadlock, unbounded startup scripts, and inert workspace ([#74](https://github.com/folsomintel/fuse/issues/74)) ([963f573](https://github.com/folsomintel/fuse/commit/963f5738e1392062ba81a227a7c49f7aa435fb88))

## [0.9.0](https://github.com/folsomintel/fuse/compare/v0.8.0...v0.9.0) (2026-07-20)


### Features

* per-instance MIG allocation (issue [#41](https://github.com/folsomintel/fuse/issues/41)) ([#71](https://github.com/folsomintel/fuse/issues/71)) ([6cc4132](https://github.com/folsomintel/fuse/commit/6cc41324830d1997599dcf5d71eadb87ebe53910))

## [0.8.0](https://github.com/folsomintel/fuse/compare/v0.7.0...v0.8.0) (2026-07-19)


### Features

* **cli:** expose gpu allocation in environment create, hosts list, and host metrics ([7a70b59](https://github.com/folsomintel/fuse/commit/7a70b59e3ec64201d6ef4895529fa93e1329629e))
* **cli:** expose GPU allocation in environment create, hosts list, and host metrics ([5ce4b49](https://github.com/folsomintel/fuse/commit/5ce4b49a6be8d100bc12d36cab2210eb45d29f3b))
* **docs:** re-write ([38f1244](https://github.com/folsomintel/fuse/commit/38f12443d35bc9a108cb5fda8f536a89abe2a1a6))
* **sdks:** bring typescript and python sdks to gpu parity with go ([c637018](https://github.com/folsomintel/fuse/commit/c6370182baf2bba15020900c7916c4f4696c8d47))
* **sdks:** bring TypeScript and Python SDKs to GPU parity with Go ([64a7265](https://github.com/folsomintel/fuse/commit/64a72655699b3c64130cd94d0031334217985806))


### Bug Fixes

* **cli:** make fuse up return once the environment is running ([c1ce30b](https://github.com/folsomintel/fuse/commit/c1ce30bcb2580813fcb4eb3a80aac0561793caf2))
* **cli:** make fuse up return once the environment is running ([56c052c](https://github.com/folsomintel/fuse/commit/56c052c26b8a880695723f434a0deaf149d22645))
* **cli:** treat a dropped event stream as a failure, not a ready environment ([1e3b900](https://github.com/folsomintel/fuse/commit/1e3b900dadc498cdb441a4a637110121a342ed20))
* **docs:** redirect / and /docs to the learn landing, align worker name with workers builds ([8218238](https://github.com/folsomintel/fuse/commit/82182387e84bc64af80bb04cbab1e2890023730e))
* **docs:** redirect / and /docs to the learn landing, align worker name with workers builds ([2b10bea](https://github.com/folsomintel/fuse/commit/2b10bea290515f2d02c2432b49a352a2f9ff1f9f))
* **orchestrator:** enforce MIGCapable and honor gpu_kind on MIG placement ([1ad63ee](https://github.com/folsomintel/fuse/commit/1ad63ee49751f0d9c864c47c933bda863639318f))
* **orchestrator:** enforce MIGCapable and honor gpu_kind on MIG placement ([ab5e0fb](https://github.com/folsomintel/fuse/commit/ab5e0fb030e1629b2ed43e6a844827524c8f1da3))
* **orchestrator:** gate the host-kind fallback on a device reporting no model ([adefd8c](https://github.com/folsomintel/fuse/commit/adefd8ccd30c7bfb4f28ac6a1a44a7c1ad766dcc))
* **orchestrator:** return 4xx instead of 500 for six permanent preconditions ([e1510d1](https://github.com/folsomintel/fuse/commit/e1510d1190428e09d51f9ca54d70bb6865bcfc6f))
* **orchestrator:** return 4xx instead of 500 for six permanent preconditions ([84d7ab7](https://github.com/folsomintel/fuse/commit/84d7ab7c3a4267272301c2d288f816c34377be9a))

## [0.7.0](https://github.com/folsomintel/fuse/compare/v0.6.0...v0.7.0) (2026-07-19)


### Features

* **api:** validate gpu_profile against mig-capable kind for raw callers ([#42](https://github.com/folsomintel/fuse/issues/42)) ([06281d9](https://github.com/folsomintel/fuse/commit/06281d973489309a470950d1f7675167c7de3604))
* **fusefile:** reject gpu_profile on non-mig-capable gpu kinds ([#42](https://github.com/folsomintel/fuse/issues/42)) ([c53ef8a](https://github.com/folsomintel/fuse/commit/c53ef8a5fd7e647bb81846a5beb05969d3a965e5))
* **gpu:** fractional MIG passthrough (D5 / [#32](https://github.com/folsomintel/fuse/issues/32)) ([bc94b20](https://github.com/folsomintel/fuse/commit/bc94b202a67f4a0e49539f104ea50c935f42bf10))
* **gpu:** wire fractional MIG (gpu_profile / mdev) end to end ([771a0df](https://github.com/folsomintel/fuse/commit/771a0df88a523f4b203a1d9f20d0ec498edd2f0d))
* **host-agent:** enrich vfio-inventory with per-gpu probe data ([#34](https://github.com/folsomintel/fuse/issues/34)) ([bbe4262](https://github.com/folsomintel/fuse/commit/bbe426263c674788229f47a0a287f843e3a46816))
* **host-agent:** probe per-gpu inventory in host_capacity ([#33](https://github.com/folsomintel/fuse/issues/33)) ([0a74cda](https://github.com/folsomintel/fuse/commit/0a74cda9721edbe64970278b19b85feceb070684))
* **orchestrator,cli:** probe gpu capacity with operator override ([#36](https://github.com/folsomintel/fuse/issues/36)) ([6552c2f](https://github.com/folsomintel/fuse/commit/6552c2fb8d2708f7d81eccf5571f19dcb3c41743))
* **orchestrator:** carry probed gpu inventory over the capacity wire ([#35](https://github.com/folsomintel/fuse/issues/35)) ([c5d964c](https://github.com/folsomintel/fuse/commit/c5d964c17088f2dd40546cee60ca9850391d9eba))
* **orchestrator:** device-aware gpu inventory, fit, and per-vm binding ([#37](https://github.com/folsomintel/fuse/issues/37), [#38](https://github.com/folsomintel/fuse/issues/38)) ([4d41d3d](https://github.com/folsomintel/fuse/commit/4d41d3dfdb56d557206d3d8c84b172d6c9c4fc14))
* **orchestrator:** recompute gpu allocation from live vms on restart ([#39](https://github.com/folsomintel/fuse/issues/39)) ([f2ca1b0](https://github.com/folsomintel/fuse/commit/f2ca1b04ea8d572f0b51595c164f549403593fac))
* validate gpu_profile against mig-capable kind (epic [#31](https://github.com/folsomintel/fuse/issues/31), closes [#42](https://github.com/folsomintel/fuse/issues/42)) ([fb81a8b](https://github.com/folsomintel/fuse/commit/fb81a8b9b6460847c3adab45015b6bd45828b13d))


### Bug Fixes

* **cli:** repair broken host register command from mig merge ([3e2181b](https://github.com/folsomintel/fuse/commit/3e2181b3ec6a4fcdb840f64cbcd2690bd4782039))
* **cli:** repair broken host register command from mig merge ([6af73ff](https://github.com/folsomintel/fuse/commit/6af73ff73be5319f2122bbc02664ac3e019ef735))

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
