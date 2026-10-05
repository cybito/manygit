#!/usr/bin/env python3
"""Manygit GitHub Release asset contract; stdout is a single JSON result."""
import argparse
import filecmp
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
TAG = re.compile(r"v[0-9]+\.[0-9]+\.[0-9]+-custom\.[1-9][0-9]*\Z")
SHA = re.compile(r"[0-9a-f]{40}\Z")
MAX_ASSETS = 1000
MAX_ASSET_SIZE = 2 * 1024 * 1024 * 1024
CHUNK = 1024 * 1024


def run(*args, binary=False):
    result = subprocess.run(args, capture_output=True)
    if result.returncode:
        raise RuntimeError(result.stderr.decode(errors="replace").strip())
    return result.stdout if binary else result.stdout.decode().strip()


def sha_file(path):
    digest = hashlib.sha256()
    with Path(path).open("rb") as stream:
        while chunk := stream.read(CHUNK): digest.update(chunk)
    return digest.hexdigest()


def same_file(left, right):
    left, right = Path(left), Path(right)
    if left.stat().st_size != right.stat().st_size: return False
    return filecmp.cmp(left, right, shallow=False)


def identity(tag, commit, platform):
    if not TAG.fullmatch(tag) or not SHA.fullmatch(commit) or platform not in ("darwin", "linux"):
        raise ValueError("invalid custom release identity")
    return tag + "-" + platform + "-arm64"


def absolute(value):
    path = Path(value)
    if not path.is_absolute(): raise ValueError("directory must be absolute")
    return path


def asset_name(tag, platform, name): return f"{tag}-{platform}-{name}"
def expected_package(tag, platform): return [f"manygit-{tag}-{platform}-arm64.tar.gz", "release.json", "SHA256SUMS"]


def package_files(directory, tag, platform, commit):
    directory = Path(directory)
    expected_files = expected_package(tag, platform)
    actual = {path.name for path in directory.iterdir() if path.is_file()}
    if actual != set(expected_files): raise ValueError("unexpected package inventory")
    receipt = json.loads((directory / "release.json").read_text())
    expected_keys = {"schema", "project", "source_repo", "source_commit", "release_tag", "platform", "architecture", "toolchains", "files"}
    if set(receipt) != expected_keys or receipt["schema"] != 1 or receipt["project"] != PROJECT or receipt["source_repo"] != SOURCE or receipt["source_commit"] != commit or receipt["release_tag"] != tag or receipt["platform"] != platform or receipt["architecture"] != "arm64":
        raise ValueError("invalid release identity")
    archive_name = expected_files[0]
    if len(receipt["files"]) != 1 or receipt["files"][0].get("name") != archive_name or not receipt["toolchains"]:
        raise ValueError("invalid payload inventory")
    item = receipt["files"][0]
    archive = directory / archive_name
    if set(item) != {"name", "sha256", "size"} or sha_file(archive) != item["sha256"] or archive.stat().st_size != item["size"]:
        raise ValueError("payload checksum mismatch")
    sums = directory / "SHA256SUMS"
    expected_sums = f"{sha_file(archive)}  {archive_name}\n{sha_file(directory / 'release.json')}  release.json\n"
    if sums.read_text() != expected_sums: raise ValueError("SHA256SUMS mismatch")
    return {name: directory / name for name in expected_files}, receipt


def release_assets(tag):
    return json.loads(run("gh", "release", "view", tag, "--repo", "cybito/manygit", "--json", "assets"))["assets"]


def check(tag, commit, platform, output):
    identity(tag, commit, platform)
    assets = release_assets(tag)
    by_name = {asset["name"]: asset for asset in assets}
    expected = expected_package(tag, platform)
    names = [asset_name(tag, platform, name) for name in expected]
    prefix = f"{tag}-{platform}-"
    unknown = {name for name in by_name if name.startswith(prefix)} - set(names)
    if unknown: raise ValueError("unexpected asset under this tag/platform prefix: " + ", ".join(sorted(unknown)))
    present = [name in by_name for name in names]
    if not any(present): return {"exists": False}
    if len(assets) > MAX_ASSETS: raise ValueError("release exceeds GitHub's 1000 asset limit")
    output = Path(output); output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()): raise ValueError("check output directory must be empty")
    with tempfile.TemporaryDirectory() as temp:
        for original, remote, exists in zip(expected, names, present):
            if exists:
                run("gh", "release", "download", tag, "--repo", "cybito/manygit", "--dir", temp, "--pattern", remote)
                shutil.move(str(Path(temp) / remote), str(output / original))
    if all(present):
        paths, _ = package_files(output, tag, platform, commit)
        for original, remote in zip(expected, names):
            if by_name[remote].get("size") != paths[original].stat().st_size: raise ValueError("existing release asset size mismatch")
        return {"exists": True, "reference": f"https://github.com/cybito/manygit/releases/tag/{tag}", "assets": names}
    return {"exists": False, "partial": True}


INSTALL = '''#!/usr/bin/env python3
import argparse, os, pathlib, shutil
p=argparse.ArgumentParser(description="Install manygit without changing services or configuration")
p.add_argument("--prefix", default=os.path.expanduser("~/.local"))
a=p.parse_args(); prefix=pathlib.Path(a.prefix)
if not prefix.is_absolute(): p.error("--prefix must be absolute")
root=pathlib.Path(__file__).resolve().parent
pairs=[(root/"bin/manygit", prefix/"bin/manygit")]
pairs += [(f, prefix/"share/manygit"/f.relative_to(root/"share/manygit")) for f in sorted((root/"share/manygit").rglob("*")) if f.is_file()]
for src,dst in pairs:
    if dst.is_symlink() or (dst.exists() and (not dst.is_file() or src.read_bytes()!=dst.read_bytes())): raise SystemExit("Refusing to overwrite unknown existing file: "+str(dst))
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
    if run("git", "rev-parse", "HEAD") != args.commit: raise ValueError("source commit is not HEAD")
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()): raise ValueError("pack output directory must be empty")
    epoch = int(run("git", "show", "-s", "--format=%ct", args.commit))
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp); (root / "bin").mkdir(); shutil.copy2(source / "bin/manygit", root / "bin/manygit")
        share = root / "share/manygit"; share.mkdir(parents=True)
        for name in ("LICENSE", "NOTICE", "README.md"):
            if Path(name).is_file(): shutil.copy2(name, share / name)
        (root / "install.sh").write_text(INSTALL); (root / "install.sh").chmod(0o755)
        name = expected_package(args.tag, args.platform)[0]; archive = output / name
        with archive.open("wb") as stream, gzip.GzipFile(filename="", mode="wb", fileobj=stream, mtime=epoch) as compressed, tarfile.open(fileobj=compressed, mode="w") as tar:
            for path in sorted(root.rglob("*")):
                info=tar.gettarinfo(str(path),str(path.relative_to(root))); info.uid=info.gid=0; info.uname=info.gname=""; info.mtime=epoch
                if path.is_file():
                    with path.open("rb") as data: tar.addfile(info,data)
                else: tar.addfile(info)
    receipt=dict(schema=1,project=PROJECT,source_repo=SOURCE,source_commit=args.commit,release_tag=args.tag,platform=args.platform,architecture="arm64",toolchains={"go":run("go","version")},files=[dict(name=name,sha256=sha_file(archive),size=archive.stat().st_size)])
    (output/"release.json").write_text(json.dumps(receipt,indent=2)+"\n")
    (output/"SHA256SUMS").write_text(f"{sha_file(archive)}  {name}\n{sha_file(output/'release.json')}  release.json\n")
    return {"directory":str(output)}


def publish(directory):
    directory=Path(directory); receipt=json.loads((directory/"release.json").read_text()); tag,platform=receipt["release_tag"],receipt["platform"]
    identity(tag,receipt["source_commit"],platform); files,_=package_files(directory,tag,platform,receipt["source_commit"])
    assets=release_assets(tag); by_name={asset["name"]:asset for asset in assets}; expected=expected_package(tag,platform); names=[asset_name(tag,platform,n) for n in expected]
    unknown={name for name in by_name if name.startswith(f"{tag}-{platform}-")} - set(names)
    if unknown: raise ValueError("unexpected asset under this tag/platform prefix: "+", ".join(sorted(unknown)))
    additions=sum(name not in by_name for name in names)
    if len(assets)+additions>MAX_ASSETS: raise ValueError("upload would exceed GitHub's 1000 asset limit")
    for original, remote in zip(expected,names):
        path=files[original]
        if path.stat().st_size>=MAX_ASSET_SIZE: raise ValueError("GitHub Release assets must be smaller than 2 GiB")
        if remote in by_name:
            with tempfile.TemporaryDirectory() as temp:
                run("gh","release","download",tag,"--repo","cybito/manygit","--dir",temp,"--pattern",remote)
                if not same_file(path,Path(temp)/remote): raise ValueError("existing asset bytes mismatch; refusing overwrite")
        else:
            with tempfile.TemporaryDirectory() as temp:
                staged=Path(temp)/remote; shutil.copyfile(path,staged)
                run("gh","release","upload",tag,str(staged),"--repo","cybito/manygit")
    with tempfile.TemporaryDirectory() as temp:
        checked=check(tag,receipt["source_commit"],platform,Path(temp)/"readback")
    if not checked["exists"]: raise ValueError("uploaded assets unavailable or partial")
    return {"release":tag,"assets":checked["assets"]}


def main():
    parser=argparse.ArgumentParser(); sub=parser.add_subparsers(dest="command",required=True)
    for command in ("check","pack"):
        p=sub.add_parser(command); p.add_argument("--tag",required=True); p.add_argument("--commit",required=True); p.add_argument("--platform",choices=("darwin","linux"),required=True); p.add_argument("--output-dir",required=True)
        if command=="pack": p.add_argument("--input-dir",required=True)
    p=sub.add_parser("publish"); p.add_argument("--directory",required=True); args=parser.parse_args()
    if args.command=="check": result=check(args.tag,args.commit,args.platform,absolute(args.output_dir))
    elif args.command=="pack": result=pack(args)
    else: result=publish(absolute(args.directory))
    print(json.dumps(result))


if __name__=="__main__":
    try: main()
    except (ValueError,RuntimeError,KeyError,OSError,json.JSONDecodeError) as error: print(str(error),file=sys.stderr); sys.exit(1)
