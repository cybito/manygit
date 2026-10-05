# Custom release provenance

- GitHub fork: `https://github.com/cybito/manygit` (upstream: `https://github.com/rabeeh-ta/manygit.git`). The `custom` branch contains the custom build source; it is the release source of truth.
- Historical Forgejo source: `https://git.cybit.top/cybit/manygit`; its `custom` ref was `c1576d4b46f9b34a539c99b37bd193a61debc466` when migrated. This historical repository remains intact.
- Historical Forgejo release: `v1.1.7-custom.3`, archives `manygit_{darwin_arm64,linux_amd64,linux_arm64}.tar.gz`; historical download example: `https://git.cybit.top/cybit/manygit/releases/download/v1.1.7-custom.3/manygit_darwin_arm64.tar.gz`.
- Historical OCI native artifact `git.cybit.top/cybit/ias-manygit`: Darwin digest `sha256:464c6f625b703860744410c2106781496928ed0e5b44852a403d3745548b6bcb`; Linux digest `sha256:18d38017b9a92629fd97827812c387286aa2db64972ee05be7179b8f5f37a1bf`. These native receipts retain their original Forgejo source identity and are historical only.

## Publishing a new custom version

Only a **published GitHub Release** in `cybito/manygit` triggers `custom-release.yml`. Ordinary pushes and tag pushes do not publish. The release tag must be `v<major>.<minor>.<patch>-custom.<positive integer>` and resolve to a commit reachable from `origin/custom`. This migration's first asset-bearing release uses `v1.1.7-custom.8`; existing tags/releases are never moved. Publish against the exact pushed custom SHA, not an upstream branch.

The ARM64 matrix builds on `macos-26` and `ubuntu-24.04-arm` (native Linux GNU binaries for Omarchy ARM64). Go is pinned to **1.26.8**, with `GOTOOLCHAIN=local`, `GOWORK=off`, `CGO_ENABLED=0`, readonly modules, trimpath, and VCS metadata. The version linker value is the release tag without `v`. CI checks `manygit --version`, Go toolchain/target/source metadata including `vcs.modified=false`, ARM64 executable headers, and installation into an isolated prefix twice. `MANYGIT_NO_UPDATE_CHECK=1` disables self-update during smoke checks. It never runs the upstream GoReleaser publishing path.

Before the first asset publication, the operator must:

1. Confirm this is a public upstream fork with default branch `custom`, disable all inherited workflows, and enable only `custom-release.yml`. The checked-in upstream workflows remain untouched.
2. Ensure the built-in GitHub Actions token has `contents: write` for the release asset upload and managed release-note update. No Forgejo PAT, registry secret, or Forgejo package setup is used.
3. Push the CI/source commit and create a GitHub Release targeting that exact SHA. Release assets are populated automatically after the two platform builds pass.

## GitHub Release asset contract

For each platform package file, including `release.json` and `SHA256SUMS`, the remote asset name is `<tag>-<platform>-<original-package-filename>`, for example `v1.1.7-custom.8-darwin-manygit-v1.1.7-custom.8-darwin-arm64.tar.gz`. The helper queries assets with `gh release view --json assets`; it downloads and validates any existing bytes before reuse. Mismatching assets are rejected and never overwritten. Missing files in a partially uploaded platform set are added individually. Uploads use `gh release upload` without `--clobber`; concurrent publication is serialized per tag/platform. Assets must each be smaller than 2 GiB and the complete release cannot exceed 1000 assets.

The helper independently reads assets back using `gh release download` and validates exact platform/tag/source commit identity, receipt schema, payload hash/size, SHA256SUMS, and complete file inventory. A final job repeats that validation for both platforms before updating the `<!-- custom-builds:start -->` / `<!-- custom-builds:end -->` release-note block, preserving all user-authored text outside it. The block links directly to the release assets. No GitHub workflow artifact or cache is used for product packages.

## Installation package contract

The package contains the archive, `release.json` (schema 1), and `SHA256SUMS`. The receipt records project, actual GitHub source repository/40-character SHA, release tag, platform, arm64 architecture, actual Go version, and archive SHA256/size. The receipt is a sidecar, not embedded recursively in its archive.

Download the desired platform set and restore original package filenames:

```sh
tag=v1.1.7-custom.8
platform=linux # use darwin for macOS ARM64
mkdir -p /absolute/path/download
cd /absolute/path/download
gh release download "$tag" --repo cybito/manygit --pattern "$tag-$platform-*"
prefix="$tag-$platform-"
for file in "$prefix"*; do mv "$file" "${file#"$prefix"}"; done
if [ "$platform" = linux ]; then sha256sum -c SHA256SUMS; else shasum -a 256 -c SHA256SUMS; fi
mkdir unpacked
tar -xzf "manygit-$tag-$platform-arm64.tar.gz" -C unpacked
./unpacked/install.sh --prefix /absolute/path/prefix
/absolute/path/prefix/bin/manygit --version
```

`install.sh` requires Python 3, defaults to `$HOME/.local`, and accepts only an absolute prefix. It installs `bin/manygit` and `share/manygit/{LICENSE,README.md}` (plus NOTICE when present), never changes user configuration, restarts services, or removes a system package. Existing identical files are left unchanged; different files and symlink destinations are refused before any copies. Add `<prefix>/bin` to your PATH yourself.

## Helper CLI and verification handoff

Run from the exact source checkout with `gh` authenticated for the repository. Project ownership is fixed in the helper, not caller-selectable:

```sh
python3 .github/scripts/package-release.py check --tag TAG --commit SHA --platform darwin --output-dir /absolute/empty/check
bash .github/scripts/custom-release.sh build darwin TAG SHA /absolute/build
python3 .github/scripts/package-release.py pack --tag TAG --commit SHA --platform darwin --input-dir /absolute/build --output-dir /absolute/empty/package
python3 .github/scripts/package-release.py publish --directory /absolute/package
```

The build entrypoint invokes pack and fixture installation; the separate pack example is for explicit helper use with a new output directory. `check` returns `{"exists":false}` when no platform assets exist, or `{"exists":true,"reference":"https://github.com/cybito/manygit/releases/tag/TAG","assets":[...]}` after independent verification. A partial set is identity/checksum validated, then reported missing so publish safely fills only missing files. `publish` validates local package bytes, reuses existing byte-identical assets, refuses mismatches, and uploads only absent names without clobbering. GitHub Release assets provide the authoritative independently downloadable copy.
