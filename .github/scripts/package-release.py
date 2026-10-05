#!/usr/bin/env python3
"""Manygit install-package OCI contract; stdout is a single JSON result."""
import argparse
from datetime import datetime, timezone
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

PROJECT = "manygit"
SOURCE = "https://github.com/cybito/manygit.git"
PACKAGE = "git.cybit.top/cybit/ias-manygit"
TYPE = "application/vnd.cybito.install-package.v1"
TAG = re.compile(r"v[0-9]+\.[0-9]+\.[0-9]+-custom\.[1-9][0-9]*\Z")
SHA = re.compile(r"[0-9a-f]{40}\Z")
DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")


def run(*args, binary=False):
    result = subprocess.run(args, capture_output=True)
    if result.returncode:
        raise RuntimeError(result.stderr.decode(errors="replace").strip())
    return result.stdout if binary else result.stdout.decode().strip()


def sha(data):
    return hashlib.sha256(data).hexdigest()


def identity(tag, commit, platform):
    if not TAG.fullmatch(tag) or not SHA.fullmatch(commit) or platform not in ("darwin", "linux"):
        raise ValueError("invalid custom release identity")
    return f"{PACKAGE}:{tag}-{platform}-arm64"


def absolute(value):
    path = Path(value)
    if not path.is_absolute():
        raise ValueError("directory/config must be absolute")
    return path


def oras(config, *args, missing=False, binary=False):
    config_flag = "--to-registry-config" if args[0] == "cp" else "--registry-config"
    result = subprocess.run(["oras", *args, config_flag, str(config)], capture_output=True)
    if result.returncode:
        error = result.stderr.decode(errors="replace")
        # Only explicit registry protocol error codes are absence. Authentication,
        # transport errors and generic HTTP 404s fail closed.
        if missing and re.search(r"\b(?:MANIFEST_UNKNOWN|NAME_UNKNOWN|manifest_unknown|name_unknown)\b", error):
            return None
        raise RuntimeError(error.strip())
    return result.stdout if binary else result.stdout.decode().strip()


def descriptor_bytes(config, reference, descriptor):
    digest = descriptor["digest"]
    if not DIGEST.fullmatch(digest):
        raise ValueError("invalid descriptor digest")
    data = oras(config, "blob", "fetch", "--output", "-", PACKAGE + "@" + digest, binary=True)
    if len(data) != descriptor["size"] or "sha256:" + sha(data) != digest:
        raise ValueError("OCI blob descriptor mismatch")
    return data


def verify(config, reference, output):
    if not re.fullmatch(re.escape(PACKAGE) + r"@sha256:[0-9a-f]{64}", reference):
        raise ValueError("verify requires this project's immutable digest reference")
    raw = oras(config, "manifest", "fetch", reference, binary=True)
    if "sha256:" + sha(raw) != reference.split("@", 1)[1]:
        raise ValueError("manifest digest mismatch")
    manifest = json.loads(raw)
    if manifest.get("artifactType") != TYPE:
        raise ValueError("not an install-package artifact")
    descriptor_bytes(config, reference, manifest["config"])
    layers = manifest["layers"]
    names = []
    blobs = {}
    for layer in layers:
        name = layer.get("annotations", {}).get("org.opencontainers.image.title", "")
        if not name or Path(name).name != name or name in names:
            raise ValueError("unsafe or duplicate OCI filename")
        names.append(name)
        blobs[name] = descriptor_bytes(config, reference, layer)
        expected_media = "application/json" if name == "release.json" else "text/plain" if name == "SHA256SUMS" else "application/gzip"
        if layer["mediaType"] != expected_media:
            raise ValueError("incorrect layer media type")
    receipt = json.loads(blobs["release.json"])
    expected_keys = {"schema", "project", "source_repo", "source_commit", "release_tag", "platform", "architecture", "toolchains", "files"}
    if set(receipt) != expected_keys or receipt["schema"] != 1 or receipt["project"] != PROJECT or receipt["source_repo"] != SOURCE or receipt["architecture"] != "arm64" or receipt["platform"] not in ("darwin", "linux"):
        raise ValueError("invalid release identity")
    identity(receipt["release_tag"], receipt["source_commit"], receipt["platform"])
    annotations = manifest.get("annotations", {})
    for key, value in (("source", SOURCE), ("revision", receipt["source_commit"]), ("version", receipt["release_tag"])):
        if annotations.get("org.opencontainers.image." + key) != value:
            raise ValueError("manifest source annotation mismatch")
    if not isinstance(receipt["toolchains"], dict) or any(not isinstance(k, str) or not isinstance(v, str) or not v for k, v in receipt["toolchains"].items()):
        raise ValueError("invalid toolchain version dictionary")
    expected_name = f"manygit-{receipt['release_tag']}-{receipt['platform']}-arm64.tar.gz"
    if len(receipt["files"]) != 1 or receipt["files"][0]["name"] != expected_name or not receipt["toolchains"]:
        raise ValueError("invalid payload inventory")
    for item in receipt["files"]:
        if set(item) != {"name", "sha256", "size"} or sha(blobs[item["name"]]) != item["sha256"] or len(blobs[item["name"]]) != item["size"]:
            raise ValueError("payload checksum mismatch")
    if set(names) != {expected_name, "release.json", "SHA256SUMS"}:
        raise ValueError("unexpected OCI layer inventory")
    sums = "".join(f"{sha(blobs[name])}  {name}\n" for name in [expected_name, "release.json"])
    if blobs["SHA256SUMS"].decode() != sums:
        raise ValueError("SHA256SUMS mismatch")
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()):
        raise ValueError("verify output directory must be empty")
    oras(config, "pull", reference, "--output", str(output))
    if {p.name for p in output.iterdir()} != set(names):
        raise ValueError("pulled file inventory mismatch")
    for name in names:
        if (output / name).read_bytes() != blobs[name]:
            raise ValueError("independent pull mismatch")
    return receipt


def resolve(config, reference):
    result = oras(config, "manifest", "fetch", "--descriptor", reference, missing=True)
    if result is None:
        return None
    digest = json.loads(result)["digest"]
    if not DIGEST.fullmatch(digest):
        raise ValueError("invalid registry digest")
    return PACKAGE + "@" + digest


def check(config, tag, commit, platform, output):
    reference = resolve(config, identity(tag, commit, platform))
    if reference is None:
        return {"exists": False}
    receipt = verify(config, reference, output)
    if (receipt["release_tag"], receipt["source_commit"], receipt["platform"]) != (tag, commit, platform):
        raise ValueError("published tag has a different release identity; refusing overwrite")
    return {"exists": True, "reference": reference}


INSTALL = '''#!/usr/bin/env python3
import argparse, hashlib, os, pathlib, shutil
p=argparse.ArgumentParser(description="Install manygit without changing services or configuration")
p.add_argument("--prefix", default=os.path.expanduser("~/.local"))
a=p.parse_args(); prefix=pathlib.Path(a.prefix)
if not prefix.is_absolute(): p.error("--prefix must be absolute")
root=pathlib.Path(__file__).resolve().parent
pairs=[(root/"bin/manygit", prefix/"bin/manygit")]
pairs += [(f, prefix/"share/manygit"/f.relative_to(root/"share/manygit")) for f in sorted((root/"share/manygit").rglob("*")) if f.is_file()]
for src,dst in pairs:
    if dst.is_symlink() or (dst.exists() and (not dst.is_file() or src.read_bytes()!=dst.read_bytes())):
        raise SystemExit("Refusing to overwrite unknown existing file: "+str(dst))
    parent=dst.parent
    while parent != prefix.parent:
        if parent.is_symlink(): raise SystemExit("Refusing symlink directory: "+str(parent))
        parent=parent.parent
for src,dst in pairs:
    dst.parent.mkdir(parents=True, exist_ok=True)
    if not dst.exists(): shutil.copy2(src,dst)
'''


def pack(args):
    identity(args.tag, args.commit, args.platform)
    source, output = absolute(args.input_dir), absolute(args.output_dir)
    if run("git", "rev-parse", "HEAD") != args.commit:
        raise ValueError("source commit is not HEAD")
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()):
        raise ValueError("pack output directory must be empty")
    epoch = int(run("git", "show", "-s", "--format=%ct", args.commit))
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        (root / "bin").mkdir()
        shutil.copy2(source / "bin/manygit", root / "bin/manygit")
        share = root / "share/manygit"
        share.mkdir(parents=True)
        for name in ("LICENSE", "NOTICE", "README.md"):
            if Path(name).is_file():
                shutil.copy2(name, share / name)
        (root / "install.sh").write_text(INSTALL)
        (root / "install.sh").chmod(0o755)
        name = f"manygit-{args.tag}-{args.platform}-arm64.tar.gz"
        archive = output / name
        with archive.open("wb") as stream, gzip.GzipFile(filename="", mode="wb", fileobj=stream, mtime=epoch) as compressed, tarfile.open(fileobj=compressed, mode="w") as tar:
            for path in sorted(root.rglob("*")):
                info = tar.gettarinfo(str(path), str(path.relative_to(root)))
                info.uid = info.gid = 0
                info.uname = info.gname = ""
                info.mtime = epoch
                if path.is_file():
                    with path.open("rb") as data:
                        tar.addfile(info, data)
                else:
                    tar.addfile(info)
    receipt = dict(schema=1, project=PROJECT, source_repo=SOURCE, source_commit=args.commit, release_tag=args.tag, platform=args.platform, architecture="arm64", toolchains={"go": run("go", "version"), "oras": run("oras", "version")}, files=[dict(name=name, sha256=sha(archive.read_bytes()), size=archive.stat().st_size)])
    (output / "release.json").write_text(json.dumps(receipt, indent=2) + "\n")
    (output / "SHA256SUMS").write_text("".join(f"{sha((output / n).read_bytes())}  {n}\n" for n in [name, "release.json"]))
    return {"directory": str(output)}


def publish(config, directory):
    receipt = json.loads((directory / "release.json").read_text())
    tag = identity(receipt["release_tag"], receipt["source_commit"], receipt["platform"])
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        reused = check(config, receipt["release_tag"], receipt["source_commit"], receipt["platform"], root / "existing")
        if reused["exists"]:
            existing = json.loads((root / "existing/release.json").read_text())
            if existing != receipt or any((root / "existing" / p.name).read_bytes() != p.read_bytes() for p in directory.iterdir()):
                raise ValueError("existing release differs from supplied package")
            return {"reference": reused["reference"], "digest": reused["reference"].split("@")[1]}
        layout = root / "layout"
        epoch = int(run("git", "show", "-s", "--format=%ct", receipt["source_commit"]))
        created = datetime.fromtimestamp(epoch, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        old = Path.cwd()
        try:
            os.chdir(directory)
            run("oras", "push", "--oci-layout", str(layout) + ":release", "--artifact-type", TYPE, "--annotation", "org.opencontainers.image.created=" + created, "--annotation", "org.opencontainers.image.source=" + SOURCE, "--annotation", "org.opencontainers.image.revision=" + receipt["source_commit"], "--annotation", "org.opencontainers.image.version=" + receipt["release_tag"], "release.json:application/json", "SHA256SUMS:text/plain", receipt["files"][0]["name"] + ":application/gzip")
        finally:
            os.chdir(old)
        descriptor = json.loads(run("oras", "manifest", "fetch", "--oci-layout", "--descriptor", str(layout) + ":release"))
        digest = descriptor["digest"]
        # Resolve immediately before copying, so an already published tag never
        # gets blindly overwritten by a retry or another publisher.
        if resolve(config, tag) is not None:
            raise ValueError("tag appeared during publication; retry to verify it")
        oras(config, "cp", "--from-oci-layout", str(layout) + "@" + digest, tag)
        reference = PACKAGE + "@" + digest
        verified = verify(config, reference, root / "verified")
        if verified != receipt or resolve(config, tag) != reference:
            raise ValueError("uploaded release/tag verification mismatch")
        return {"reference": reference, "digest": digest}


def main():
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="command", required=True)
    for command in ("check", "pack"):
        p = sub.add_parser(command)
        p.add_argument("--tag", required=True)
        p.add_argument("--commit", required=True)
        p.add_argument("--platform", choices=("darwin", "linux"), required=True)
        p.add_argument("--output-dir", required=True)
        if command == "pack":
            p.add_argument("--input-dir", required=True)
    p = sub.add_parser("publish")
    p.add_argument("--directory", required=True)
    p.add_argument("--registry-config", required=True)
    p = sub.add_parser("verify")
    p.add_argument("--reference", required=True)
    p.add_argument("--output-dir", required=True)
    args = parser.parse_args()
    with tempfile.TemporaryDirectory() as tmp:
        config = absolute(args.registry_config) if args.command == "publish" else Path(os.environ.get("ORAS_REGISTRY_CONFIG", str(Path(tmp) / "anonymous.json")))
        if not config.exists():
            config.write_text('{"auths":{}}\n')
            config.chmod(0o600)
        if args.command == "check":
            result = check(config, args.tag, args.commit, args.platform, absolute(args.output_dir))
        elif args.command == "pack":
            result = pack(args)
        elif args.command == "publish":
            result = publish(config, absolute(args.directory))
        else:
            result = verify(config, args.reference, absolute(args.output_dir))
        print(json.dumps(result))


if __name__ == "__main__":
    try:
        main()
    except (ValueError, RuntimeError, KeyError, OSError, json.JSONDecodeError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
