"""Verify four MCP calls change only the receiving thread and produce a real CLI receipt."""
import argparse
from contextlib import closing
import json
import os
from pathlib import Path
import secrets
import sqlite3
import subprocess
import sys
import tempfile
import time

from acceptance import Agent, binary, eventually


def live_trial(codex, root, state, agent, when):
    from codex_triggers import codex_turn

    project, home, calls = root / "project", root / "home", root / "tool-calls"
    project.mkdir()
    home.mkdir()
    (home / "auth.json").symlink_to(Path(os.environ.get("CODEX_HOME", Path.home() / ".codex")) / "auth.json")
    goal = ("请用 Bridge 将 report 定时报告的接收线程从 A 改到 B，保留现有定时设置，"
            "现在试发一次，查回执并告诉我结果。")
    started = time.monotonic()
    result, _ = codex_turn(codex, home, project, state, calls, goal)
    elapsed = time.monotonic() - started
    if result.returncode:
        raise RuntimeError("Codex trial failed: " + result.stderr[-500:])
    payload = json.loads((root / "B.json").read_text())
    latest = agent.call("get_trigger", trigger_id="report", view="latest")
    assert latest["run_id"] == payload["run_id"] and latest["status"] == "completed"
    flow = agent.call("get_trigger", trigger_id="report")["trigger"]
    assert flow["when"] == when and flow["revision"] == 1 and not (root / "A.json").exists()
    trace = [json.loads(line) for line in calls.read_text().splitlines()]
    send_index = next(i for i, call in enumerate(trace) if call["tool"] == "run_trigger_now")
    assert any(call["tool"] == "get_trigger" and call["arguments"].get("run_id") == latest["run_id"]
               for call in trace[send_index + 1:])
    print(f"PASS live Codex, {elapsed:.2f}s, {len(trace)} calls: " + ", ".join(c["tool"] for c in trace))


def verify(root, codex=None):
    state = root / "state"
    state.mkdir(mode=0o700)
    spec = {"id": "report", "command": sys.executable, "args": [str(Path(__file__).resolve()), "--receive", "A"],
            "cwd": str(root), "input_format": "json"}
    (state / "cli-destinations.json").write_text(json.dumps([spec]))
    daemon = subprocess.Popen([binary, "serve", "-data", state, "-listen", "127.0.0.1:0"],
                              env={**os.environ, "BRIDGE_MASTER_KEY": secrets.token_hex(32)})
    try:
        eventually(lambda: (state / "bridge.sock").exists())
        with closing(Agent(state)) as agent, closing(sqlite3.connect(state / "bridge.sqlite")) as db:
            when = {"type": "schedule", "cron": "0 0 1 1 *", "timezone": "Asia/Shanghai"}
            handler = agent.call("bridge_save_tool", name="report", description="Generate a report",
                                 input_schema={"type":"object"}, uses=[],
                                 script='def run(args,ctx): return {"action":"notify","data":{"markdown":"report"}}')
            flow = dict(trigger_id="report")
            agent.call("save_trigger", **flow, when=when, handler=handler, destination_id="report")
            assert agent.call("get_trigger", **flow, view="latest")["status"] == "never_run"
            assert agent.call("run_trigger_now", **flow, dry_run=True)["status"] == "preview"
            assert db.execute("select count(*) from trigger_runs").fetchone()[0] == 0
            if codex:
                live_trial(codex, root, state, agent, when)
                assert db.execute("select count(*) from trigger_runs").fetchone()[0] == 1
                return
            started = time.monotonic()
            calls = []
            def call(name, **args):
                calls.append(name)
                return agent.call(name, **args)
            current = call("bridge_get_status", destination_id="report")
            argv = current["destination"]["args"].copy()
            argv[-1] = "B"
            call("bridge_save_destination", destination_id="report", revision=current["revision"], patch={"args":argv})
            run = call("run_trigger_now", **flow, request_id="send-to-b")
            receipt = call("get_trigger", **flow, view="latest", run_id=run["run_id"], wait_ms=10000)
            elapsed = time.monotonic()-started
            assert len(calls)==4 and receipt["status"]=="completed" and receipt["delivery_result"]["exit_code"]==0, receipt
            assert receipt["destination_id"]=="report" and receipt["processing_status"]=="processed"
            assert agent.call("get_trigger", **flow, run_id=run["run_id"], wait_ms=100) == receipt
            payload = json.loads((root/"B.json").read_text())
            assert payload["run_id"]==run["run_id"] and payload["data"]["markdown"]=="report"
            assert not (root/"A.json").exists() and daemon.poll() is None
            assert agent.call("run_trigger_now", **flow, request_id="send-to-b")["run_id"]==run["run_id"]
            saved = agent.call("get_trigger", **flow)["trigger"]
            assert saved["when"]==when and saved["revision"]==1
            assert db.execute("select count(*) from trigger_runs").fetchone()[0]==1
            stale = agent.rpc("tools/call", {"name":"bridge_save_destination","arguments":{
                "destination_id":"report","patch":{"args":spec["args"]},"revision":current["revision"]}})
            assert stale["isError"]
            print(f"PASS {len(calls)} MCP calls, {elapsed:.3f}s, thread B received run {run['run_id'][:12]}, cron preserved")
    finally:
        daemon.terminate()
        daemon.wait(timeout=10)


if __name__=="__main__":
    if len(sys.argv)==3 and sys.argv[1]=="--receive":
        Path(sys.argv[2]+".json").write_text(sys.stdin.read())
    else:
        parser = argparse.ArgumentParser()
        parser.add_argument("--codex", help="Evaluate a real agent on the same isolated fixture")
        options = parser.parse_args()
        with tempfile.TemporaryDirectory() as directory:
            verify(Path(directory), options.codex)
