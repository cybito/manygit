#!/usr/bin/env bash
set -euo pipefail
[[ $# == 5 && $1 == build ]] || { echo 'usage: custom-release.sh build darwin|linux TAG SHA ABS_OUTPUT' >&2; exit 2; }
platform=$2 tag=$3 source_sha=$4 out=$5
[[ $platform == darwin || $platform == linux ]]
[[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+-custom\.[1-9][0-9]*$ ]]
[[ $source_sha =~ ^[0-9a-f]{40}$ && $out == /* ]]
[[ $(git rev-parse HEAD) == "$source_sha" ]]
[[ $(uname -m) == arm64 || $(uname -m) == aarch64 ]]
[[ $(go env GOVERSION) == go1.26.8 ]]
[[ $(go env GOHOSTOS) == "$platform" ]]
mkdir -p "$out/bin"
export GOTOOLCHAIN=local GOWORK=off CGO_ENABLED=0 GOOS="$platform" GOARCH=arm64 MANYGIT_NO_UPDATE_CHECK=1
version=${tag#v}
go build -buildvcs=true -trimpath -mod=readonly -ldflags="-s -w -X main.version=$version" -o "$out/bin/manygit" .
[[ $("$out/bin/manygit" --version) == "manygit $version" ]]
go version -m "$out/bin/manygit" > "$out/go-metadata.txt"
python3 - "$out/bin/manygit" "$source_sha" "$platform" <<'PY'
import subprocess,sys
binary,commit,platform=sys.argv[1:]
text=subprocess.check_output(['go','version','-m',binary],text=True)
for required in ('go1.26.8','vcs.revision='+commit,'vcs.modified=false','GOARCH=arm64','GOOS='+platform):
    if required not in text: raise SystemExit('missing Go build metadata: '+required)
with open(binary,'rb') as f: head=f.read(64)
if platform=='linux':
    if head[:4]!=b'\x7fELF' or int.from_bytes(head[18:20],'little')!=183: raise SystemExit('not ARM64 ELF')
else:
    if head[:4]!=b'\xcf\xfa\xed\xfe' or int.from_bytes(head[4:8],'little')!=0x100000c: raise SystemExit('not ARM64 Mach-O')
PY
pack_dir="$out/package"
python3 .github/scripts/package-release.py pack --tag "$tag" --commit "$source_sha" --platform "$platform" --input-dir "$out" --output-dir "$pack_dir"
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
mkdir "$fixture/extracted" "$fixture/home"
tar -xzf "$pack_dir/manygit-$tag-$platform-arm64.tar.gz" -C "$fixture/extracted"
HOME="$fixture/home" "$fixture/extracted/install.sh" --prefix "$fixture/prefix"
[[ $(HOME="$fixture/home" "$fixture/prefix/bin/manygit" --version) == "manygit $version" ]]
python3 - "$fixture/prefix" "$fixture/before.json" <<'PY'
import hashlib,json,pathlib,sys
root=pathlib.Path(sys.argv[1]); pathlib.Path(sys.argv[2]).write_text(json.dumps({str(p.relative_to(root)):[hashlib.sha256(p.read_bytes()).hexdigest(),p.stat().st_mtime_ns,p.stat().st_mode] for p in root.rglob('*') if p.is_file()},sort_keys=True))
PY
HOME="$fixture/home" "$fixture/extracted/install.sh" --prefix "$fixture/prefix"
python3 - "$fixture/prefix" "$fixture/before.json" <<'PY'
import hashlib,json,pathlib,sys
root=pathlib.Path(sys.argv[1]); actual={str(p.relative_to(root)):[hashlib.sha256(p.read_bytes()).hexdigest(),p.stat().st_mtime_ns,p.stat().st_mode] for p in root.rglob('*') if p.is_file()}
assert actual==json.loads(pathlib.Path(sys.argv[2]).read_text()), 'installation is not idempotent'
PY
