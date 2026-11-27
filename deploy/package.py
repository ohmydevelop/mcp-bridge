"""Build Bridge release archives without a host-specific archiver or sha256sum."""
import argparse
import hashlib
import os
import shutil
from pathlib import Path
import subprocess
import tarfile
import tempfile
import zipfile

ROOT = Path(__file__).resolve().parents[1]
TARGETS = ("linux-amd64", "linux-arm64", "darwin-arm64", "windows-amd64")


def build(target, dest):
    system, arch = target.split("-")
    name = "mcp-bridge-" + target
    with tempfile.TemporaryDirectory() as temp:
        filename = "mcp-bridge.exe" if system == "windows" else "mcp-bridge"
        exe = Path(temp, filename)
        env = {**os.environ, "GOOS": system, "GOARCH": arch, "CGO_ENABLED": "0"}
        subprocess.run(["go", "build", "-trimpath", "-o", str(exe), "."],
                       cwd=ROOT, env=env, check=True)
        if system == "linux":
            result = dest / name
            # Temp and release directories can be on different filesystems.
            shutil.copy2(exe, result)
            return result
        script = "windows-service.ps1" if system == "windows" else "macos-service.sh"
        files = [exe, ROOT / "deploy" / script, ROOT / "docs/platforms.md"]
        if system == "windows":
            result = dest / (name + ".zip")
            with zipfile.ZipFile(result, "w", zipfile.ZIP_DEFLATED) as archive:
                for path in files:
                    archive.write(path, name + "/" + path.name)
        else:
            result = dest / (name + ".tar.gz")
            with tarfile.open(result, "w:gz") as archive:
                for path in files:
                    archive.add(path, name + "/" + path.name)
        return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target", action="append", choices=TARGETS)
    parser.add_argument("--out", type=Path, default=ROOT / "dist")
    args = parser.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    artifacts = [build(target, args.out) for target in args.target or TARGETS]
    sums = [hashlib.sha256(path.read_bytes()).hexdigest() + "  " + path.name for path in artifacts]
    (args.out / "SHA256SUMS").write_bytes(("\n".join(sums) + "\n").encode("ascii"))


if __name__ == "__main__":
    main()
