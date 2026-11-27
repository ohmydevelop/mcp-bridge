"""Exercise bound stdio, PTY, and fixed executable destinations through the binary."""
from contextlib import closing
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import tempfile

from acceptance import Agent, binary, fixture
from terminal_agents import Screen, wait_for


def bind(root):
    with closing(Agent(root / "state")) as agent:
        current = agent.call("bridge_get_status")["current_terminal"]
        assert current.startswith("term-")
        saved = agent.call("save_trigger", trigger_id="bound", when=WHEN,
                           destination_id="current_terminal")
        assert saved["revision"] == 1
        (root / "bound").write_text(current)
    (root / "received").write_text(input())
    sys.exit(17)


WHEN = {"type": "event", "source_id": "meter", "event_name": "reading.ready"}


def check(root):
    state = root / "state"
    state.mkdir(mode=0o700)
    event_path = root / "events"
    presets = []
    for name in ("json", "prompt"):
        script = ("import sys; s=sys.stdin.read(); print(s); print('!'*9000); "
                  "print('err',file=sys.stderr); sys.exit(" + ("0" if name == "json" else "4") + ")")
        presets.append(dict(id=name, command=sys.executable, args=["-c", script],
                            cwd=str(root), input_format=name))
    marker = root / "started"
    presets.append(dict(id="slow", command=sys.executable, args=["-c",
        "from pathlib import Path; import time; p=Path('started'); p.open('a').write('1'); time.sleep(90)"],
        cwd=str(root), input_format="json"))
    (state / "cli-destinations.json").write_text(json.dumps([{**presets[0], "args": ["-V"]}, presets[2]]))
    env = {**os.environ, "BRIDGE_MASTER_KEY": secrets.token_hex(32)}

    def start():
        daemon = subprocess.Popen([binary, "serve", "-data", state, "-listen", "127.0.0.1:0"], env=env)
        wait_for(lambda: (state / "bridge.sock").exists())
        return daemon

    daemon = start()
    screen = None
    try:
        with closing(Agent(state)) as agent:
            agent.call("bridge_create_source", source={"id": "meter", "type": "stdio",
                       "command": sys.executable, "args": [str(fixture / "server.py")],
                       "env": {"QA_EVENT_FILE": str(event_path)}, "events": {"reading.ready": "poll_runs"}})
            agent.call("bridge_save_destination", destination={**presets[0], "type": "cli_process"}, revision=1)
            for name in ("json", "slow"):
                agent.call("save_trigger", trigger_id=name, when=WHEN, destination_id=name,
                           instruction="check receipt")
            assert not agent.call("bridge_get_status")["current_terminal"]
        screen = Screen([binary, "run", "-data", str(state), "--delivery=auto", "--",
                         sys.executable, __file__, "--bind", str(root)], root, env)
        wait_for(lambda: (root / "bound").exists() or screen.proc.poll() is not None)
        assert (root / "bound").exists(), screen.text()
        current = (root / "bound").read_text()
        daemon.terminate()
        daemon.wait(timeout=10)
        daemon = start()
        with closing(Agent(state)) as agent:
            wait_for(lambda: any(row["id"] == current and row["online"]
                                 for row in agent.call("bridge_get_status")["destinations"]))
            prompt = {**presets[1], "type": "cli_process"}
            agent.call("bridge_save_destination", destination={**prompt, "args": ["-V"]}, revision=0)
            agent.call("save_trigger", trigger_id="prompt", when=WHEN, destination_id="prompt",
                       instruction="check receipt")
            agent.call("bridge_save_destination", destination=prompt, revision=1)
            stale = agent.rpc("tools/call", {"name": "bridge_save_destination", "arguments": {
                "destination": {**prompt, "args": ["-V"]}, "revision": 1}})
            assert stale["isError"] and "revision conflict" in stale["content"][0]["text"]
            event_path.write_text(json.dumps([{"eventId": "one", "name": "reading.ready",
                                               "data": {"text": "literal $(id)"}}]))
            for name, status in (("json", "completed"), ("prompt", "failed"), ("bound", "submitted")):
                def finished():
                    record = agent.call("get_trigger", trigger_id=name)
                    return next((d for d in record["deliveries"] if d["status"] == status), None)
                receipt = wait_for(finished)
                assert receipt["attempts"] == 1
                if name != "bound":
                    run = agent.call("get_trigger", trigger_id=name, run_id=receipt["id"])
                    result = run["delivery_result"]
                    assert result["stderr"] == "err\n" and result["exit_code"] == (0 if name == "json" else 4)
                    assert len(result["stdout"]) == 8192 and "literal $(id)" in result["stdout"]
                    assert result["stdout"].startswith("{" if name == "json" else "instruction: check receipt")
            assert "literal $(id)" in (root / "received").read_text()
            assert "\x1b" not in (root / "received").read_text()
            assert screen.proc.wait(timeout=10) == 17
            wait_for(marker.exists)
        daemon.terminate()
        daemon.wait(timeout=10)
        daemon = start()
        with closing(Agent(state)) as agent:
            receipt = agent.call("get_trigger", trigger_id="slow")["deliveries"][0]
            assert receipt["status"] == "unknown" and receipt["attempts"] == 1
            assert marker.read_text() == "1"
    finally:
        if screen:
            screen.close()
        daemon.terminate()
        daemon.wait(timeout=10)


if __name__ == "__main__":
    if len(sys.argv) > 1:
        bind(Path(sys.argv[2]))
    else:
        with tempfile.TemporaryDirectory() as path:
            check(Path(path))
        print("PASS bound stdio, restart, terminal receipt and both CLI formats")
