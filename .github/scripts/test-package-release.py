#!/usr/bin/env python3
"""Behavioral seam tests for immutable GitHub Release asset handling."""
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("package_release", Path(__file__).with_name("package-release.py"))
helper = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(helper)
TAG = "v1.1.7-custom.7"
COMMIT = "a" * 40


def fixture(directory, platform="linux", payload=b"tiny package"):
    directory.mkdir(parents=True, exist_ok=True)
    archive_name = helper.expected_package(TAG, platform)[0]
    archive = directory / archive_name
    archive.write_bytes(payload)
    receipt = {"schema": 1, "project": "manygit", "source_repo": helper.SOURCE, "source_commit": COMMIT, "release_tag": TAG, "platform": platform, "architecture": "arm64", "toolchains": {"go": "go1.26.8"}, "files": [{"name": archive_name, "sha256": helper.sha_file(archive), "size": archive.stat().st_size}]}
    (directory / "release.json").write_text(json.dumps(receipt, indent=2) + "\n")
    (directory / "SHA256SUMS").write_text(f"{helper.sha_file(archive)}  {archive_name}\n{helper.sha_file(directory / 'release.json')}  release.json\n")
    return directory


with tempfile.TemporaryDirectory() as temporary:
    root = Path(temporary)
    local = fixture(root / "local")
    REMOTE = root / "remote"
    REMOTE.mkdir()
    UPLOADS = []

    def mock_run(*args, **kwargs):
        command = list(args)
        if command[:3] == ["gh", "release", "view"]:
            inventory = [{"name": path.name, "size": path.stat().st_size} for path in REMOTE.iterdir() if path.is_file()]
            return json.dumps({"assets": inventory})
        if command[:3] == ["gh", "release", "download"]:
            pattern = command[command.index("--pattern") + 1]
            src = REMOTE / pattern
            if not src.is_file(): raise RuntimeError("asset not found")
            Path(command[command.index("--dir") + 1], pattern).write_bytes(src.read_bytes())
            return ""
        if command[:3] == ["gh", "release", "upload"]:
            src = Path(command[4])
            dest = REMOTE / src.name
            if dest.exists(): raise RuntimeError("asset already exists")
            dest.write_bytes(src.read_bytes())
            UPLOADS.append(command)
            return ""
        raise AssertionError(command)

    with patch.object(helper, "run", side_effect=mock_run):
        assert helper.check(TAG, COMMIT, "linux", root / "missing") == {"exists": False}

        # A complete existing release is independently downloaded and validated.
        for name in helper.expected_package(TAG, "linux"):
            remote_name = helper.asset_name(TAG, "linux", name)
            (REMOTE / remote_name).write_bytes((local / name).read_bytes())
        assert helper.check(TAG, COMMIT, "linux", root / "full")["exists"]

        # One existing file yields a validated partial set; publish only adds absent names.
        for path in REMOTE.iterdir(): path.unlink()
        archive_name = helper.expected_package(TAG, "linux")[0]
        (REMOTE / helper.asset_name(TAG, "linux", archive_name)).write_bytes((local / archive_name).read_bytes())
        assert helper.check(TAG, COMMIT, "linux", root / "partial") == {"exists": False, "partial": True}
        result = helper.publish(local)
        assert result["assets"] == [helper.asset_name(TAG, "linux", n) for n in helper.expected_package(TAG, "linux")]
        assert len(UPLOADS) == 2
        assert all("--clobber" not in command for command in UPLOADS)
        assert all("--repo" in command and "cybito/manygit" in command for command in UPLOADS)
        assert all((REMOTE / helper.asset_name(TAG, "linux", name)).read_bytes() == (local / name).read_bytes() for name in helper.expected_package(TAG, "linux"))

        # Different bytes for a pre-existing asset are refused with no overwrite.
        before = (REMOTE / helper.asset_name(TAG, "linux", archive_name)).read_bytes()
        bad = fixture(root / "bad", payload=b"different")
        try: helper.publish(bad)
        except ValueError as error: assert "mismatch" in str(error)
        else: raise AssertionError("mismatching pre-existing bytes were accepted")
        assert (REMOTE / helper.asset_name(TAG, "linux", archive_name)).read_bytes() == before

        bad_receipt = json.loads((local / "release.json").read_text())
        bad_receipt["source_commit"] = "b" * 40
        (local / "release.json").write_text(json.dumps(bad_receipt))
        try: helper.package_files(local, TAG, "linux", COMMIT)
        except ValueError: pass
        else: raise AssertionError("wrong source receipt accepted")

        good = fixture(root / "checksum-good", payload=b"tiny package")
        (good / "SHA256SUMS").write_text((good / "SHA256SUMS").read_text() + "extra\n")
        try: helper.package_files(good, TAG, "linux", COMMIT)
        except ValueError: pass
        else: raise AssertionError("incorrect checksum inventory accepted")

        (REMOTE / helper.asset_name(TAG, "linux", "unexpected.bin")).write_bytes(b"x")
        try: helper.check(TAG, COMMIT, "linux", root / "unknown")
        except ValueError: pass
        else: raise AssertionError("unknown same-platform asset was accepted")

        upload = next(command for command in UPLOADS if command[1:3] == ["release", "upload"])
        assert upload[4].endswith(helper.asset_name(TAG, "linux", "release.json"))
