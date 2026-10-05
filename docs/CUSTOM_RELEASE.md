# Custom release provenance

- GitHub fork: `https://github.com/cybito/manygit` (upstream: `https://github.com/rabeeh-ta/manygit.git`). The `custom` branch contains the custom build source; it is the release source of truth.
- Historical Forgejo source: `https://git.cybit.top/cybit/manygit`; its `custom` ref was `c1576d4b46f9b34a539c99b37bd193a61debc466` when migrated. This historical repository remains intact.
- Historical Forgejo release: `v1.1.7-custom.3`, archives `manygit_{darwin_arm64,linux_amd64,linux_arm64}.tar.gz`; historical download example: `https://git.cybit.top/cybit/manygit/releases/download/v1.1.7-custom.3/manygit_darwin_arm64.tar.gz`.
- Historical OCI native artifact `git.cybit.top/cybit/ias-manygit`: Darwin digest `sha256:464c6f625b703860744410c2106781496928ed0e5b44852a403d3745548b6bcb`; Linux digest `sha256:18d38017b9a92629fd97827812c387286aa2db64972ee05be7179b8f5f37a1bf`. These native receipts retain their original Forgejo source identity.
- Custom archive installation packages are a distinct OCI artifact type and do not replace the historical native artifacts. Download by immutable reference, e.g. `oras pull git.cybit.top/cybit/ias-manygit@sha256:<digest>`.

## Publishing a new custom version

Only a **published GitHub Release** in `cybito/manygit` triggers `custom-release.yml`. Ordinary pushes and tag pushes do not publish. The release tag must be `v<major>.<minor>.<patch>-custom.<positive integer>` and resolve to a commit reachable from `origin/custom`. Initial planned tag: `v1.1.7-custom.7`; if occupied, use the next unused custom integer without changing an existing tag. Publish against the exact pushed custom SHA, not an upstream branch.

The ARM64 matrix builds on `macos-26` and `ubuntu-24.04-arm` (native Linux GNU binaries for Omarchy ARM64). Go is pinned to **1.26.8**, with `GOTOOLCHAIN=local`, `GOWORK=off`, `CGO_ENABLED=0`, readonly modules, trimpath, and VCS metadata. The version linker value is the release tag without `v`. CI checks `manygit --version`, Go toolchain/target/source metadata including `vcs.modified=false`, ARM64 executable headers, and installation into an isolated prefix twice. `MANYGIT_NO_UPDATE_CHECK=1` disables self-update during smoke checks. It never runs the upstream GoReleaser publishing path.

Before the first release, the operator must:

1. Confirm this is a public upstream fork with default branch `custom`, disable all inherited workflows, and enable only `custom-release.yml`. The checked-in upstream workflows remain untouched.
2. Create the `forgejo-registry` GitHub environment with custom deployment **tag** policy `v*-custom.*` (no branch policy). Add `FORGEJO_REGISTRY_TOKEN` as an environment secret: a newly created `cybit` Forgejo PAT with package-write permission, preferably public-only. Do not export existing OAuth/Docker credentials. Package permissions cover the owner's namespace and are **not** restricted to these six packages.
3. Keep `git.cybit.top/cybit/ias-manygit` public and associate it with `cybit/manygit` in Forgejo package settings. The actual source annotation remains GitHub, not a fabricated Forgejo source URL.
4. After disabling inherited workflows and selecting `custom` as the default branch, push the CI/source commit and create a GitHub Release targeting that exact SHA, without attachments. A pushed source commit or release draft is not evidence of successful hosted builds or package publication.

The secret is exposed only to the upload step, after compilation, packaging, and smoke tests. ORAS **1.3.3** is downloaded and checked against its official release checksum file. Login uses password-stdin, a runner-temporary mode-0600 registry configuration in a mode-0700 directory, and explicit `--registry-config` arguments. The configuration is deleted on both success and failure.

## Installation package contract

The existing package now also carries artifact type `application/vnd.cybito.install-package.v1`, distinct from historical `application/vnd.ias.native.v1` receipts. New tags are `<release-tag>-{darwin,linux}-arm64`; always install using the immutable digest recorded in successful CI/Release notes:

```sh
mkdir -p /absolute/path/download
oras pull git.cybit.top/cybit/ias-manygit@sha256:<digest> --output /absolute/path/download
cd /absolute/path/download
shasum -a 256 -c SHA256SUMS
mkdir unpacked
tar -xzf manygit-v1.1.7-custom.7-darwin-arm64.tar.gz -C unpacked
./unpacked/install.sh --prefix /absolute/path/prefix
/absolute/path/prefix/bin/manygit --version
```

On Omarchy, use the `linux-arm64` archive instead. `install.sh` requires Python 3, defaults to `$HOME/.local`, and accepts only an absolute prefix. It installs `bin/manygit` and `share/manygit/{LICENSE,README.md}` (plus NOTICE when present), never changes user configuration, restarts services, or removes a system package. Existing identical files are left unchanged; different files and symlink destinations are refused before any copies. Add `<prefix>/bin` to your PATH yourself.

Each OCI artifact contains the archive, `release.json` (schema 1), and `SHA256SUMS`. The receipt records project, actual GitHub source repository/40-character SHA, release tag, platform, arm64 architecture, actual toolchain version strings, and archive SHA256/size. The receipt is a sidecar, not embedded recursively in its archive. Layers have explicit JSON/plaintext/gzip media types; created time is fixed to the source commit UTC timestamp.

Publication creates a local OCI layout and determines its digest before copying to Forgejo. The helper verifies remote manifest bytes, config and every layer descriptor, receipt identity, all checksums, and an independent immutable pull. A pre-existing release is reused only after complete verification and matching source/tag/platform; malformed receipts, authentication failures, network errors, and generic 404 responses are failures, not absence. Only explicit `MANIFEST_UNKNOWN`/`NAME_UNKNOWN` responses permit a new publication. Platform concurrency prevents this workflow racing itself, and publish rechecks absence immediately before copy; operators must not concurrently mutate these tags outside CI.

Each platform is independent. If one fails, the other platform's immutable package remains; rerunning verifies and skips already published packages. Only after both succeed does an anonymous final job verify both again and replace the `<!-- custom-builds:start -->` / `<!-- custom-builds:end -->` Release notes block, preserving your other notes. It lists digest references and download commands. GitHub Release assets must remain empty; no GitHub artifacts, build caches, GHCR publication, or binary commits are used.

## Helper CLI and verification handoff

Run from the exact source checkout with ORAS available. Project/package ownership is fixed in the helper, not caller-selectable:

```sh
python3 .github/scripts/package-release.py check --tag TAG --commit SHA --platform darwin --output-dir /absolute/empty/check
bash .github/scripts/custom-release.sh build darwin TAG SHA /absolute/build
python3 .github/scripts/package-release.py pack --tag TAG --commit SHA --platform darwin --input-dir /absolute/build --output-dir /absolute/empty/package
python3 .github/scripts/package-release.py publish --directory /absolute/package --registry-config /absolute/private/auth.json
python3 .github/scripts/package-release.py verify --reference git.cybit.top/cybit/ias-manygit@sha256:DIGEST --output-dir /absolute/empty/verified
```

The build entrypoint already invokes pack and fixture installation; the separate pack example is for explicit helper use with a new output directory. `check` returns `{"exists":false}` for explicit absence or `{"exists":true,"reference":"…@sha256:…"}` after immutable verification. Pack returns `directory`; publish returns `reference` and `digest`; verify returns the validated receipt. Anonymous check/verify use an isolated empty registry configuration; an explicit `ORAS_REGISTRY_CONFIG` environment path may be supplied when necessary.

Static checks (Python parsing, `bash -n`, and YAML parsing) and local release
boundary/installer regressions were run for this migration. Hosted ARM64 builds,
the Forgejo PAT, release publication, anonymous digest pulls, and repeat-run
digest stability remain unverified. Do not claim end-to-end delivery before
those results are observed.
