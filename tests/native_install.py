"""Exercise the user service installer in a disposable, logged-in CI account."""
import argparse
from contextlib import closing
import os
from pathlib import Path
import subprocess
import sys
import time

import acceptance


def check(package, late_python=None):
    state = Path.home() / ".mcp-bridge"
    if sys.platform == "darwin":
        app = Path.home() / "Library/Application Support/mcp-bridge"
        script, binary = "macos-service.sh", "mcp-bridge"
        prefix = []
        manager_present = (Path.home() / "Library/LaunchAgents/net.mcp-bridge.agent.plist").exists()
    elif os.name == "nt":
        app = Path(os.environ["LOCALAPPDATA"]) / "mcp-bridge"
        script, binary = "windows-service.ps1", "mcp-bridge.exe"
        prefix = ["powershell.exe", "-NoProfile", "-ExecutionPolicy", "RemoteSigned", "-File"]
        manager_present = subprocess.run([
            "powershell.exe", "-NoProfile", "-Command",
            '$sid=[Security.Principal.WindowsIdentity]::GetCurrent().User.Value;'
            ' if (Get-ScheduledTask -TaskName "MCP Bridge ($sid)" -ErrorAction SilentlyContinue) { exit 1 }'
        ]).returncode != 0
    else:
        raise RuntimeError("This acceptance test requires macOS or Windows")
    if app.exists() or state.exists() or manager_present:
        raise RuntimeError("Use a clean, disposable user account with no Bridge installation.")
    command = prefix + [str(package / script)]
    acceptance.binary = str(app / binary)
    def healthy():
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            result = subprocess.run(command + ["status"], capture_output=True, timeout=40)
            if result.returncode == 0:
                return
            time.sleep(1)
        raise AssertionError(f"Status failed: {result.stdout!r} {result.stderr!r}")
    try:
        subprocess.run(command + ["install"], check=True, timeout=60)
        healthy()
        key = (state / "master.key").read_bytes()
        token = (state / "http.token").read_text(encoding="ascii").strip()
        assert "tools" in acceptance.HttpAgent("http://127.0.0.1:8088", token).rpc("tools/list", {})
        with closing(acceptance.Agent(state)) as agent:
            if late_python:
                # Keep this stdio session connected across the refresh: a restart would close it.
                refresh_env = os.environ.copy()
                refresh_env.pop("BRIDGE_PYTHON", None)
                refresh_env["BRIDGE_PYTHON_HINT_FILE"] = str(app / "python-path")
                refresh_env["PATH"] = os.pathsep.join([str(late_python.parent), refresh_env["PATH"]])
                version = subprocess.check_output([late_python, "-ISB", "-c",
                                                  "import sys;print(sys.version.split()[0])"], text=True).strip()
                initial = subprocess.check_output(command + ["status"], encoding="utf-8", errors="replace")
                assert "(Python " + version + ")" not in initial, "Choose a distinct refresh interpreter"
                subprocess.run(command + ["refresh-python"], env=refresh_env, check=True, timeout=30)
                current = subprocess.check_output(command + ["status"], encoding="utf-8", errors="replace")
                assert "(Python " + version + ")" in current, current
                assert "destinations" in agent.call("bridge_get_status")
            agent.call("bridge_save_destination", destination={
                "type": "cli_process", "id": "upgrade-proof", "command": sys.executable,
                "args": ["--version"], "cwd": str(package), "input_format": "json"}, revision=0)
        subprocess.run(command + ["install"], check=True, timeout=60)
        healthy()
        assert (state / "master.key").read_bytes() == key
        assert (state / "http.token").read_text(encoding="ascii").strip() == token
        with closing(acceptance.Agent(state)) as agent:
            assert any(d["id"] == "upgrade-proof" for d in agent.call("bridge_get_status")["destinations"])
    finally:
        subprocess.run(command + ["uninstall"], check=True, timeout=60)
    assert not (app / binary).exists() and (state / "bridge.sqlite").is_file()
    assert (state / "master.key").read_bytes() == key

    # A fresh install must reopen the retained database with the same credentials.
    try:
        subprocess.run(command + ["install"], check=True, timeout=60)
        healthy()
        with closing(acceptance.Agent(state)) as agent:
            targets = agent.call("bridge_get_status")["destinations"]
            assert any(d["id"] == "upgrade-proof" for d in targets)
        assert "tools" in acceptance.HttpAgent("http://127.0.0.1:8088", token).rpc("tools/list", {})
    finally:
        subprocess.run(command + ["uninstall"], check=True, timeout=60)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("package", type=Path)
    parser.add_argument("--late-python", type=Path, help="a second Python version to test live refresh")
    args = parser.parse_args()
    check(args.package.resolve(), args.late_python)
    print("PASS login service registration, status, upgrade and uninstall preserving data")
