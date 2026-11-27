"""Drive the packaged daemon on its target OS with a real scheduled handler."""
import argparse
from contextlib import closing, contextmanager
import json
import os
from pathlib import Path
from http.server import BaseHTTPRequestHandler, HTTPServer
from queue import Queue
from threading import Thread
import secrets
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request

import acceptance


def wait_until(check, seconds):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if result := check():
            return result
        time.sleep(0.2)
    raise AssertionError(f"{check.__name__} timed out after {seconds}s")


@contextmanager
def serve(exe, state, env):
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        address = f"127.0.0.1:{sock.getsockname()[1]}"
    process = subprocess.Popen([exe, "serve", "-data", state, "-listen", address], env=env)
    try:
        def healthy():
            if process.poll() is not None:
                raise AssertionError(f"daemon exited {process.returncode}")
            try:
                with urllib.request.urlopen(f"http://{address}/healthz", timeout=1) as response:
                    return response.read() == b"ok\n"
            except OSError:
                return False
        wait_until(healthy, 20)
        yield process, "http://" + address
    finally:
        process.terminate()
        process.wait(timeout=15)


@contextmanager
def receiver():
    messages = Queue()
    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            messages.put(json.loads(self.rfile.read(int(self.headers["Content-Length"]))))
            self.send_response(204)
            self.end_headers()
        def log_message(self, *_):
            pass
    with HTTPServer(("127.0.0.1", 0), Handler) as server:
        thread = Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            yield f"http://127.0.0.1:{server.server_port}", messages
        finally:
            server.shutdown()
            thread.join()


def exercise(exe, temp, late_python=None):
    acceptance.binary = str(exe)
    state = temp / "state"
    directory = temp / "报告 files"
    directory.mkdir()
    token = secrets.token_hex(32)
    env = {**os.environ, "BRIDGE_MASTER_KEY": secrets.token_hex(32),
           "BRIDGE_TOKENS": json.dumps({"reader": token})}
    late_env = temp / "Python 安装"
    if late_python:
        env["BRIDGE_PYTHON"] = str(late_env / ("Scripts/python.exe" if os.name == "nt" else "bin/python"))
    args = {"id": "report", "type": "cli_process", "command": sys.executable,
            "args": [str(Path(__file__).resolve()), "--receive", "unused.md"],
            "cwd": str(directory), "input_format": "json"}
    when = {"type": "schedule", "cron": "* * * * *", "timezone": "Asia/Shanghai"}
    handler = {"name": "report", "description": "Build an issue report",
               "input_schema": {"type": "object"}, "uses": ["devboard/failure_summary"]}
    script = ('def run(args, ctx):\n'
              '    row = ctx.call_tool("devboard/failure_summary", {"limit": 1})["rows"][0]\n'
              '    return {"action":"notify", "data":{"markdown":"# 待处理 v1: " + row["message"]}}\n')
    with receiver() as (hook, messages), serve(exe, state, {**env, "BRIDGE_WEBHOOK_ORIGINS": hook}) as (process, url), closing(acceptance.Agent(state)) as agent:
        doctor = [exe, "doctor", "-data", state]
        before = subprocess.check_output(doctor, env=env, timeout=45, text=True)
        if late_python:
            version = subprocess.check_output([late_python, "-ISB", "-c",
                                              "import sys;print(sys.version.split()[0])"], text=True).strip()
            assert "(Python " + version + ")" not in before, "Choose a distinct Python version for the late install"
            subprocess.run([late_python, "-m", "venv", "--without-pip", str(late_env)], check=True, timeout=45)
            after = subprocess.check_output(doctor, env=env, timeout=45, text=True)
            assert "(Python " + version + ")" in after, after

        assert "tools" in acceptance.HttpAgent(url, token).rpc("tools/list", {})
        blocked = acceptance.HttpAgent(url, token).rpc("tools/call", {
            "name": "bridge_save_destination", "arguments": {"destination": args, "revision": 0}})
        assert blocked["isError"]
        duplicate = subprocess.run([exe, "serve", "-data", state, "-listen", "127.0.0.1:0"],
                                   env=env, capture_output=True, timeout=10)
        assert duplicate.returncode != 0
        with serve(exe, temp / "second", env), closing(acceptance.Agent(temp / "second")) as other:
            assert "destinations" in other.call("bridge_get_status")
        agent.call("bridge_create_source", source={"id": "devboard", "type": "stdio",
                   "command": sys.executable, "args": [str(acceptance.fixture / "server.py")],
                   "env": {"QA_EVENT_FILE": str(temp / "events.json")}, "events":{"issue.updated":"poll_runs"}})
        agent.call("bridge_save_destination", destination={"id":"hook", "url":hook + "/issue"}, revision=0)
        agent.call("save_trigger", trigger_id="on-issue", destination_id="hook",
                   when={"type":"event", "source_id":"devboard", "event_name":"issue.updated"})
        staging = temp / "events.next"
        staging.write_text(json.dumps([{"eventId":"first","name":"issue.updated","data":{"key":"TEST-7"}}]), encoding="utf-8")
        staging.replace(temp / "events.json")
        assert messages.get(timeout=20)["event"]["data"] == {"key":"TEST-7"}
        agent.call("bridge_save_destination", destination=args, revision=0)
        alternative = {**args, "id": "another-target"}
        agent.call("bridge_save_destination", destination=alternative, revision=0)
        desired = dict(args, args=[args["args"][0], "--receive", "原生 report.md"])
        agent.call("bridge_save_destination", destination=desired, revision=1)
        published = agent.call("bridge_save_tool", **handler, script=script)
        # Set an infrequent schedule first, then activate the new version at runtime.
        trigger = "native-report"
        agent.call("save_trigger", trigger_id=trigger, handler=published, destination_id="report",
                   when={**when, "cron": "0 0 1 1 *"})
        published = agent.call("bridge_save_tool", **handler, script=script.replace("v1", "v2"), revision=1)
        agent.call("save_trigger", trigger_id=trigger, revision=1, when=when, handler=published)
        stale = agent.rpc("tools/call", {"name": "bridge_save_destination", "arguments":
                         {"destination": args, "revision": 1}})
        assert stale["isError"]
        def completed():
            current = agent.call("get_trigger", trigger_id=trigger)
            for run in current["runs"]:
                if run["status"] == "failed":
                    raise AssertionError(agent.call("get_trigger", trigger_id=trigger, run_id=run["run_id"]))
            deliveries = current["deliveries"]
            for delivery in deliveries:
                if delivery["status"] == "failed":
                    raise AssertionError(agent.call("get_trigger", trigger_id=trigger, run_id=delivery["id"]))
                if delivery["status"] == "completed":
                    return delivery
        receipt = wait_until(completed, 95)
        assert receipt["attempts"] == 1 and process.poll() is None
        assert (directory / "原生 report.md").read_text(encoding="utf-8") == "# 待处理 v2: failure-0"
        assert not (directory / "unused.md").exists()
        agent.call("save_trigger", trigger_id=trigger, disabled=True, revision=2)
    with serve(exe, state, env), closing(acceptance.Agent(state)) as agent:
        targets = {d["id"]: d for d in agent.call("bridge_get_status")["destinations"]}
        assert targets["report"]["online"] and targets["report"]["revision"] == 2
        assert targets["another-target"]["online"]
        trigger_state = agent.call("get_trigger", trigger_id=trigger)["trigger"]
        assert trigger_state["disabled"] and trigger_state["handler"]["revision"] == 2


if __name__ == "__main__":
    if len(sys.argv) == 3 and sys.argv[1] == "--receive":
        Path(sys.argv[2]).write_text(json.load(sys.stdin)["data"]["markdown"], encoding="utf-8")
    else:
        parser = argparse.ArgumentParser(description=__doc__)
        parser.add_argument("executable", type=Path)
        parser.add_argument("--late-python", type=Path,
                            help="a second Python version to install as a venv after the daemon starts")
        options = parser.parse_args()
        with tempfile.TemporaryDirectory() as tmp:
            exercise(options.executable.resolve(), Path(tmp), options.late_python)
        print("PASS native HTTP, stdio, schedule, handler callback, CLI delivery, live update and restart")
