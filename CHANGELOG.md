# Changelog

## [0.6.5](https://github.com/evatt-labs/kraai/compare/v0.6.4...v0.6.5) (2026-09-20)


### Bug Fixes

* **plan:** a reference read orders after the one type it reads ([#226](https://github.com/evatt-labs/kraai/issues/226)) ([4e42bad](https://github.com/evatt-labs/kraai/commit/4e42bad08ef45d0b209265d0a0ada1fc1ee4e856))

## [0.6.4](https://github.com/evatt-labs/kraai/compare/v0.6.3...v0.6.4) (2026-09-20)


### ⚠ BREAKING CHANGES

* a `databases:` entry under the cloudflare vendor no longer accepts `caching`, and a `queues:` entry no longer accepts `consumer`. Both were accepted and ignored.
* **aws:** the dns entry's `name` key is no longer accepted. It was declared before anything read it, and the one record kraai writes is the zone apex. CloudFront cannot yet read the private bucket it fronts; that
* a `cdn` entry must name the `objects` binding it fronts as `origin`; a `dns` record's target is its entry's `alias`. Neither key existed before, and a `cdn` entry without `origin` no longer loads.
* a route with `custom_domain: true` must name a `certificate:` — a tls binding on its service. A manifest that declared one without it loaded and did nothing; it now fails at load saying what is missing.
* `hooks:` in kraai.yaml is no longer accepted. It did nothing.
* an environment's `resources:` block is keyed by capability, so `databases:` becomes `database:`.
* `plugins:` is now a list of objects, not of paths. Each entry names the module, what it may reach, and what it implements.

### Features

* a binding names the bindings it needs ([#215](https://github.com/evatt-labs/kraai/issues/215)) ([bf91738](https://github.com/evatt-labs/kraai/commit/bf9173833448bb7193880b391a3aa1d2da5d204a))
* a registration declares which bindings it reads ([#214](https://github.com/evatt-labs/kraai/issues/214)) ([8862dfd](https://github.com/evatt-labs/kraai/commit/8862dfdcb4f89d8bde7052b4d2e6ef5f4c591d9e)), closes [#208](https://github.com/evatt-labs/kraai/issues/208)
* **aws:** a hosted zone is named by the zone its entry declares ([#216](https://github.com/evatt-labs/kraai/issues/216)) ([100f810](https://github.com/evatt-labs/kraai/commit/100f81094840f4d6a52d40d5227337756fd524ed)), closes [#117](https://github.com/evatt-labs/kraai/issues/117)
* **aws:** check a list request against what the type's schema requires ([#224](https://github.com/evatt-labs/kraai/issues/224)) ([97715b0](https://github.com/evatt-labs/kraai/commit/97715b07349d429030bfc93c07a138bfefcce6a0)), closes [#132](https://github.com/evatt-labs/kraai/issues/132)
* **aws:** create hosted zones, and certificates validated through them ([#218](https://github.com/evatt-labs/kraai/issues/218)) ([80a145e](https://github.com/evatt-labs/kraai/commit/80a145ed004dd2d0d2683fac5a7e6c0ccbee6f46)), closes [#117](https://github.com/evatt-labs/kraai/issues/117)
* **aws:** create the distribution and the apex record ([#220](https://github.com/evatt-labs/kraai/issues/220)) ([4532588](https://github.com/evatt-labs/kraai/commit/4532588bec8139565f3b3b04532f4c5d0a3af2e4)), closes [#117](https://github.com/evatt-labs/kraai/issues/117)
* **aws:** the engine checks ownership of what it finds ([#217](https://github.com/evatt-labs/kraai/issues/217)) ([f601e9f](https://github.com/evatt-labs/kraai/commit/f601e9ff7240e4637471907234ac45a87a69973a)), closes [#120](https://github.com/evatt-labs/kraai/issues/120)
* Hyperdrive reads the entry's caching; drop the keys nothing reads ([#223](https://github.com/evatt-labs/kraai/issues/223)) ([7005a59](https://github.com/evatt-labs/kraai/commit/7005a5911fbcb772140b7c370c2a53ebba40fa2d)), closes [#192](https://github.com/evatt-labs/kraai/issues/192)
* let plugins declare capabilities ([#205](https://github.com/evatt-labs/kraai/issues/205)) ([9476186](https://github.com/evatt-labs/kraai/commit/94761867126ba7cced64e87f1cf78491ecdd6d11)), closes [#126](https://github.com/evatt-labs/kraai/issues/126)
* load the plugins a manifest declares ([#202](https://github.com/evatt-labs/kraai/issues/202)) ([3693e06](https://github.com/evatt-labs/kraai/commit/3693e06e6ef34f37b2054efefc9425348748dc60)), closes [#199](https://github.com/evatt-labs/kraai/issues/199)
* make resource imports reach the planner and the provider ([#206](https://github.com/evatt-labs/kraai/issues/206)) ([6ff3ec0](https://github.com/evatt-labs/kraai/commit/6ff3ec03b3eb6303101337486a1faf281f6eb19e)), closes [#112](https://github.com/evatt-labs/kraai/issues/112)
* **plan:** --detailed-exitcode reports changes present as 2 ([#221](https://github.com/evatt-labs/kraai/issues/221)) ([063e356](https://github.com/evatt-labs/kraai/commit/063e356c8d2e567fdca28ea27c94868c853c0b7b)), closes [#121](https://github.com/evatt-labs/kraai/issues/121)
* remove the hooks field nothing read ([#207](https://github.com/evatt-labs/kraai/issues/207)) ([8692b2c](https://github.com/evatt-labs/kraai/commit/8692b2cb3e9c9432abe946170b74f025aa49d1a1)), closes [#111](https://github.com/evatt-labs/kraai/issues/111)
* routes build a custom domain, presenting an adopted certificate ([#211](https://github.com/evatt-labs/kraai/issues/211)) ([be92603](https://github.com/evatt-labs/kraai/commit/be926031a2d069d0aab7a46a84b6a3ddd9856f29)), closes [#110](https://github.com/evatt-labs/kraai/issues/110)
* update a resource in place when its difference is mutable ([#212](https://github.com/evatt-labs/kraai/issues/212)) ([5b0aeb4](https://github.com/evatt-labs/kraai/commit/5b0aeb461b899ad221e83b7d35933f055e81f9e6))


### Bug Fixes

* **aws:** read the certificate ARN under CertificateArn, not Id ([#213](https://github.com/evatt-labs/kraai/issues/213)) ([7efafff](https://github.com/evatt-labs/kraai/commit/7efafff25b7d77f5867e164540aca5e2bf97c08a))
* **plan:** a declared read is an ordering edge ([#209](https://github.com/evatt-labs/kraai/issues/209)) ([983d769](https://github.com/evatt-labs/kraai/commit/983d769d949ad4b2b35b3d6f8ef9f9355d769e19)), closes [#119](https://github.com/evatt-labs/kraai/issues/119)

## [0.6.3](https://github.com/evatt-labs/kraai/compare/v0.6.2...v0.6.3) (2026-09-19)


### ⚠ BREAKING CHANGES

* a manifest binding Route 53, ACM or CloudFront through `objects:` must move those entries to `dns:`, `tls:` and `cdn:`.

### Features

* decompose objects into objects, dns, tls and cdn ([#198](https://github.com/evatt-labs/kraai/issues/198)) ([25136a5](https://github.com/evatt-labs/kraai/commit/25136a5f6dcf24538d95a17b35769d1241880105)), closes [#125](https://github.com/evatt-labs/kraai/issues/125)
* **manifest:** check a capability's vendor against what that vendor declares ([#200](https://github.com/evatt-labs/kraai/issues/200)) ([6b3e2dd](https://github.com/evatt-labs/kraai/commit/6b3e2dda35af819aeb0fe3ab227e54ea33e33e5c)), closes [#189](https://github.com/evatt-labs/kraai/issues/189)
* **resource:** declare the vendor-type split instead of inventing it ([#195](https://github.com/evatt-labs/kraai/issues/195)) ([2e8649f](https://github.com/evatt-labs/kraai/commit/2e8649f7338774a002924dd362f9b8fb3dd3cd19)), closes [#124](https://github.com/evatt-labs/kraai/issues/124)

## [0.6.2](https://github.com/evatt-labs/kraai/compare/v0.6.1...v0.6.2) (2026-09-18)


### Features

* **manifest:** key providers by declared capability instead of a fixed struct ([#188](https://github.com/evatt-labs/kraai/issues/188)) ([8090845](https://github.com/evatt-labs/kraai/commit/8090845f0bed62e0dca5f2c4ff9b0dbdb71ca315)), closes [#122](https://github.com/evatt-labs/kraai/issues/122)
* **manifest:** key service bindings by declared capability ([#191](https://github.com/evatt-labs/kraai/issues/191)) ([398e85c](https://github.com/evatt-labs/kraai/commit/398e85cef0f84126376855570e1842e96131a2da)), closes [#122](https://github.com/evatt-labs/kraai/issues/122)

## [0.6.1](https://github.com/evatt-labs/kraai/compare/v0.6.0...v0.6.1) (2026-09-17)


### Features

* **cli:** derive a pull request's environment name ([#161](https://github.com/evatt-labs/kraai/issues/161)) ([50856cd](https://github.com/evatt-labs/kraai/commit/50856cdd240409ee3aa20fc0e636eebe35cb43b6))
* publish a GitHub Action that runs the Go binary ([#162](https://github.com/evatt-labs/kraai/issues/162)) ([70af1a6](https://github.com/evatt-labs/kraai/commit/70af1a6480d41d255c1553455cf5d46b29342dc7))
* **release:** publish a Homebrew cask ([#164](https://github.com/evatt-labs/kraai/issues/164)) ([c3958cf](https://github.com/evatt-labs/kraai/commit/c3958cfe624bf293eba1b246dde55d55a59aed9e))

## [0.6.0](https://github.com/evatt-labs/kraai/compare/v0.5.0...v0.6.0) (2026-09-16)


### ⚠ BREAKING CHANGES

* order resources by dependency graph instead of fixed phases ([#100](https://github.com/evatt-labs/kraai/issues/100))

### Features

* adapt the Cloudflare storage types to the resource contract ([#63](https://github.com/evatt-labs/kraai/issues/63)) ([03bec21](https://github.com/evatt-labs/kraai/commit/03bec2141340fe3ed681622d4abd76f1ef8631c8))
* adapt the Postgres capability to the resource contract ([#64](https://github.com/evatt-labs/kraai/issues/64)) ([f2ba8c2](https://github.com/evatt-labs/kraai/commit/f2ba8c2927b9028e975bff1d4e5159c2d18e6613))
* apply the environment naming prefix to derived names ([#103](https://github.com/evatt-labs/kraai/issues/103)) ([4be1592](https://github.com/evatt-labs/kraai/commit/4be159246404a62c471132049a7caa6e16652a9c))
* assemble a resource registry from a manifest ([#71](https://github.com/evatt-labs/kraai/issues/71)) ([49b95fd](https://github.com/evatt-labs/kraai/commit/49b95fd1aafd6ffdc2054fb97ed6bf2d26abca47))
* AWS Cloud Control read-only provider ([#67](https://github.com/evatt-labs/kraai/issues/67)) ([fa4081f](https://github.com/evatt-labs/kraai/commit/fa4081f8bffdd195d817d24a3849c4414fe71a28))
* AWS Cloud Control write verbs ([#75](https://github.com/evatt-labs/kraai/issues/75)) ([aa5a937](https://github.com/evatt-labs/kraai/commit/aa5a937e60499c73509ce8c85a5b0ff6d3d6e6c2))
* AWS Lambda compute — Tier 2 types and artifact packaging ([#80](https://github.com/evatt-labs/kraai/issues/80)) ([089cfbb](https://github.com/evatt-labs/kraai/commit/089cfbb776fae161ecfb1f4446b9d310c2ae1826))
* Cloudflare API client in Go ([#57](https://github.com/evatt-labs/kraai/issues/57)) ([c46ea95](https://github.com/evatt-labs/kraai/commit/c46ea95926614963bc7b68b732560d27eb744822))
* condition companion resources, and plan every service as deployable ([#70](https://github.com/evatt-labs/kraai/issues/70)) ([d7e9eb6](https://github.com/evatt-labs/kraai/commit/d7e9eb619f995440c0640ab5edcccf82fb300416))
* deployed-config builder in Go ([#61](https://github.com/evatt-labs/kraai/issues/61)) ([a64c583](https://github.com/evatt-labs/kraai/commit/a64c583d01ff50a32ce99ed76b4d659f20275457))
* errors-package (cockroachdb/errors, D19 exit codes, centralized handler) ([#42](https://github.com/evatt-labs/kraai/issues/42)) ([e0df012](https://github.com/evatt-labs/kraai/commit/e0df012929bca6f1250c2f7de8b65f021cdac3c7))
* Go module scaffold, Cobra CLI, GoReleaser, npm wrapper, Go CI ([#41](https://github.com/evatt-labs/kraai/issues/41)) ([2518871](https://github.com/evatt-labs/kraai/commit/25188719bfec1980ba497a0e211835756947c331))
* kraai apply ([#76](https://github.com/evatt-labs/kraai/issues/76)) ([51dcafd](https://github.com/evatt-labs/kraai/commit/51dcafdbead71049f31524ca3f87b46c5c3f87c3))
* kraai destroy ([#85](https://github.com/evatt-labs/kraai/issues/85)) ([46cac1d](https://github.com/evatt-labs/kraai/commit/46cac1d768c117b1a264d1ead99be5c92f1f5aeb))
* kraai plan ([#72](https://github.com/evatt-labs/kraai/issues/72)) ([c5ccc27](https://github.com/evatt-labs/kraai/commit/c5ccc2713da5cc19057224619cdf690fbe91298b))
* **kubernetes:** talk to a cluster without client-go ([#153](https://github.com/evatt-labs/kraai/issues/153)) ([3d51fa3](https://github.com/evatt-labs/kraai/commit/3d51fa35b094c5b0583f59a0a8154273cc131f8f))
* manifest-loader (directory-based manifest, pongo2 templating, values precedence) ([#45](https://github.com/evatt-labs/kraai/issues/45)) ([0e412d6](https://github.com/evatt-labs/kraai/commit/0e412d6f0648d945e8450ddd12846bb19d24f999))
* name the database capability by kind, and its binding by driver ([#69](https://github.com/evatt-labs/kraai/issues/69)) ([3ba7603](https://github.com/evatt-labs/kraai/commit/3ba760336ad5e2f9eedd5f476fd14aabd65cd972))
* naming-policy (deterministic naming, frozen ephemeral grammar) ([#46](https://github.com/evatt-labs/kraai/issues/46)) ([482fc78](https://github.com/evatt-labs/kraai/commit/482fc788dbaf9b60c7a3b331a8d159e230737d5f))
* Neon management API client in Go ([#60](https://github.com/evatt-labs/kraai/issues/60)) ([9a5cead](https://github.com/evatt-labs/kraai/commit/9a5ceadbd351f5ac2348ac98a315a8c8c58a80c7))
* order resources by dependency graph instead of fixed phases ([#100](https://github.com/evatt-labs/kraai/issues/100)) ([d51fd82](https://github.com/evatt-labs/kraai/commit/d51fd82ce2425cdf52ee1d0c1a736404ffbdaf2c))
* per-service compute configuration and trigger shape ([#78](https://github.com/evatt-labs/kraai/issues/78)) ([796d16c](https://github.com/evatt-labs/kraai/commit/796d16c510a13126c6d6fed795cbb08397628039))
* port the env, reachability and browser helpers to Go ([#56](https://github.com/evatt-labs/kraai/issues/56)) ([19312b7](https://github.com/evatt-labs/kraai/commit/19312b7cf5fee789189a7ee0ba6b61b1fa5fefc5))
* port the environment lockfile to Go ([#54](https://github.com/evatt-labs/kraai/issues/54)) ([e49c882](https://github.com/evatt-labs/kraai/commit/e49c88276395161bef838fa177927b3d06c93693))
* Postgres layer in Go, driven through pgx ([#55](https://github.com/evatt-labs/kraai/issues/55)) ([a202cf6](https://github.com/evatt-labs/kraai/commit/a202cf603908170d5f153778d29f7b617d54d6c1))
* provider settings in the manifest ([#65](https://github.com/evatt-labs/kraai/issues/65)) ([0d78a0b](https://github.com/evatt-labs/kraai/commit/0d78a0bf6b6b81e8970694f412c3437dfa0f40fb))
* provider-declared capability definitions ([#104](https://github.com/evatt-labs/kraai/issues/104)) ([f63de12](https://github.com/evatt-labs/kraai/commit/f63de12a431a16b28d19dd1e5903a675bd42d622))
* provision a VPC with a routable public subnet ([#146](https://github.com/evatt-labs/kraai/issues/146)) ([052c64c](https://github.com/evatt-labs/kraai/commit/052c64c97c908adbc0409bfe258ed914a0d08ade))
* read-only planner for the resource contract ([#66](https://github.com/evatt-labs/kraai/issues/66)) ([81171cc](https://github.com/evatt-labs/kraai/commit/81171cccb0479a8248f05027927dde322bd4740a))
* **release:** build, sign and publish binaries on a tag ([#157](https://github.com/evatt-labs/kraai/issues/157)) ([98e28c8](https://github.com/evatt-labs/kraai/commit/98e28c8315e6a48fde47a824853968ce2c898c31))
* resource contract, registry and telemetry ([#62](https://github.com/evatt-labs/kraai/issues/62)) ([beb2d9f](https://github.com/evatt-labs/kraai/commit/beb2d9fb4d1d4394e6a5de6227f8652ae9e82d0e))
* share one tuned, instrumented HTTP transport ([#94](https://github.com/evatt-labs/kraai/issues/94)) ([7b8cff0](https://github.com/evatt-labs/kraai/commit/7b8cff047b91d3a3c66334d4aa930576d42ecfe5))
* validate provider settings against capability schemas ([#107](https://github.com/evatt-labs/kraai/issues/107)) ([ebcdab9](https://github.com/evatt-labs/kraai/commit/ebcdab9a78049b15af94919ce81b8606f5efa608))
* wazero WASM plugin runtime with sandbox resource bounds ([#48](https://github.com/evatt-labs/kraai/issues/48)) ([358e6db](https://github.com/evatt-labs/kraai/commit/358e6db16ba4b7c2d9cff3a7b9381bd8ea9db9aa))
* wrangler CLI wrapper in Go ([#59](https://github.com/evatt-labs/kraai/issues/59)) ([f8bd48b](https://github.com/evatt-labs/kraai/commit/f8bd48bcda7f490795c474dd18b3984abbff4c79))


### Bug Fixes

* act on the review of the merged port ([#58](https://github.com/evatt-labs/kraai/issues/58)) ([b6efd82](https://github.com/evatt-labs/kraai/commit/b6efd826d0ef986e85e2e7282da55534e9739df1))
* actually load the .env the error message promises ([#74](https://github.com/evatt-labs/kraai/issues/74)) ([722ed57](https://github.com/evatt-labs/kraai/commit/722ed57790c9004343a00a56282dc6151438537e))
* carry the derived name into an AWS create ([#81](https://github.com/evatt-labs/kraai/issues/81)) ([f035202](https://github.com/evatt-labs/kraai/commit/f03520297231592f1f1cb8a4104e8735473ac2ef))
* compare transports relatively instead of against a fixed churn count ([#98](https://github.com/evatt-labs/kraai/issues/98)) ([841b2b2](https://github.com/evatt-labs/kraai/commit/841b2b2d27fae1b22c41214751f351877ff1a697))
* empty the artifact bucket before deleting it ([#88](https://github.com/evatt-labs/kraai/issues/88)) ([e3c466f](https://github.com/evatt-labs/kraai/commit/e3c466f0125ff160993dd19c70cb0ac468e0f937))
* exclude gitignored and sensitive paths from the Lambda artifact ([#84](https://github.com/evatt-labs/kraai/issues/84)) ([6e7cf44](https://github.com/evatt-labs/kraai/commit/6e7cf44147972840aace7ac5b69e6a2aad20a61f))
* forward optional interfaces through the telemetry decorator ([#77](https://github.com/evatt-labs/kraai/issues/77)) ([b1e68b7](https://github.com/evatt-labs/kraai/commit/b1e68b713ea6807cae4e946f98aabe7ea70b6fd0))
* guard plugin HTTP egress at dial time ([#50](https://github.com/evatt-labs/kraai/issues/50)) ([0ab7afb](https://github.com/evatt-labs/kraai/commit/0ab7afb052a23bd83626b56be5e431794cce1c8e))
* honour reserved concurrency and reject unknown provider settings ([#86](https://github.com/evatt-labs/kraai/issues/86)) ([036d987](https://github.com/evatt-labs/kraai/commit/036d987cce7b4433202e3ca25c6254d197163c44))
* keep the last status when a retry deadline lands mid-request ([#97](https://github.com/evatt-labs/kraai/issues/97)) ([ed7aeb9](https://github.com/evatt-labs/kraai/commit/ed7aeb9a5efc95d8e0df99e017daee1235b2d925))
* let a compute resource read its service's credentials ([#79](https://github.com/evatt-labs/kraai/issues/79)) ([15d2d5a](https://github.com/evatt-labs/kraai/commit/15d2d5a880f9b0e3eafb71ea0516f96d045c8c67))
* make CodeQL actually scan the Go code ([#53](https://github.com/evatt-labs/kraai/issues/53)) ([d179378](https://github.com/evatt-labs/kraai/commit/d179378ba459e6c8a4ca8becb6bf5e0a1c390f8e))
* measure warm-call overhead as a floor, not a single reading ([#95](https://github.com/evatt-labs/kraai/issues/95)) ([154fc7c](https://github.com/evatt-labs/kraai/commit/154fc7cb33232547805ca443d6e56f09a22226fc))
* put the warm-call budget behind a perf build tag ([#108](https://github.com/evatt-labs/kraai/issues/108)) ([4b15365](https://github.com/evatt-labs/kraai/commit/4b153653ba8ed3bad265c01529ac60c597596067))
* **release:** mark prerelease tags as prereleases ([#160](https://github.com/evatt-labs/kraai/issues/160)) ([9ef4c45](https://github.com/evatt-labs/kraai/commit/9ef4c45f401cbf45106ffd11b8e300b57611acc8))
* **release:** sign with a cosign bundle instead of deprecated flag pair ([#159](https://github.com/evatt-labs/kraai/issues/159)) ([766f341](https://github.com/evatt-labs/kraai/commit/766f3412125d767d194e2fca2a13bc471214db85))
* resolve a capability by vendor, not by provider ([#68](https://github.com/evatt-labs/kraai/issues/68)) ([540db1b](https://github.com/evatt-labs/kraai/commit/540db1b34c087fceb3b31ed8223670a3bad72440))
* scope the API Gateway invoke permission to two segments ([#87](https://github.com/evatt-labs/kraai/issues/87)) ([283d5bf](https://github.com/evatt-labs/kraai/commit/283d5bfdd91b5cfdf100eca8dfc47e3d6d55b424))
* scope the list request for parent-scoped AWS types ([#82](https://github.com/evatt-labs/kraai/issues/82)) ([c265f7e](https://github.com/evatt-labs/kraai/commit/c265f7e2bb62f2f389b8894b1fc91a10b219321d))
* serialize provider operations that share a scope ([#92](https://github.com/evatt-labs/kraai/issues/92)) ([8ced5d8](https://github.com/evatt-labs/kraai/commit/8ced5d8480f4db5efebc2f153bb4298c92b47e0d))
* stop referring to a lockfile that no longer exists ([#143](https://github.com/evatt-labs/kraai/issues/143)) ([1b9e1e6](https://github.com/evatt-labs/kraai/commit/1b9e1e6ff497b32ede2900af36713abaf5d4eceb))
* **test:** make the connection-pooling contrast deterministic ([#155](https://github.com/evatt-labs/kraai/issues/155)) ([1bde11a](https://github.com/evatt-labs/kraai/commit/1bde11aa401321ebdf56c4ca1737d5229d864b3d))
* treat a foreign-owned artifact bucket as absent ([#99](https://github.com/evatt-labs/kraai/issues/99)) ([a3bc8ae](https://github.com/evatt-labs/kraai/commit/a3bc8ae7600614bf1fbe2d2c59770ed2019e5061))
