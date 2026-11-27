"""Compare one-off and published execution over stdio against the same QA snapshot."""
import argparse
from contextlib import closing
import hashlib
import json
import os
from pathlib import Path
import secrets
import sqlite3
import subprocess
import sys
import tempfile

from acceptance import Agent, binary, eventually, fixture
from qa_replay import capture

SCRIPT = """def run(args, ctx):
    bot = ctx.call_tool("qa/get_bot", {})
    return {key: bot[key] for key in args["flags"]}
"""


def verify(root, snapshot):
    state = root / "state"
    reads = root / "reads.jsonl"
    reads.touch()
    (root / "snapshot.json").write_text(json.dumps(snapshot))
    daemon = subprocess.Popen([binary, "serve", "-data", state, "-listen", "127.0.0.1:0"],
                              env={**os.environ, "BRIDGE_MASTER_KEY":secrets.token_hex(32)})
    try:
        eventually(lambda: (state / "bridge.sock").exists())
        with closing(Agent(state)) as client, closing(sqlite3.connect(state / "bridge.sqlite")) as db:
            client.call("bridge_create_source", source={"id":"qa", "type":"stdio", "command":sys.executable,
                        "args":[str(fixture / "server.py")], "env":{"QA_BOT_REPLAY":str(root)}})
            spec = {"script":SCRIPT,"uses":["qa/get_bot"]}
            args = {"flags":["auto_answer","general_knowledge","answer_ref"]}
            meta = {"threadId":"qa-parity","x-codex-turn-metadata":{"thread_id":"qa-parity","turn_id":"same-turn"}}

            def counts():
                return len(reads.read_text().splitlines()), db.execute("SELECT coalesce(sum(hits),0) FROM task_reads").fetchone()[0]

            def invoke(name, params):
                before = counts()
                result = client.rpc("tools/call", {"name":name,"arguments":params,"_meta":meta})
                assert not result.get("isError"), result
                delta = [n - prev for n, prev in zip(counts(), before)]
                assert delta == [1,1], (name,delta)
                return result, {"tool":name,"upstream_calls":delta[0],"bridge_read_count":delta[1],
                                "returned":result["structuredContent"]}

            first, direct = invoke("bridge_execute_script", {**spec,"arguments":args})
            assert client.call("bridge_get_status")["published_tools"] == {}
            # An undeclared call must fail before it can reach the QA server.
            invalid = client.rpc("tools/call", {"name":"bridge_execute_script","arguments":{
                **spec,"script":'def run(args, ctx):\n    return ctx.call_tool("qa/failure_summary", {})',"arguments":{}}})
            assert invalid.get("isError") and "undeclared" in str(invalid)
            prior = counts()
            published = client.call("bridge_save_tool",name="qa_bot_flags",description="Read QA flags",
                                    input_schema={"type":"object"},**spec)
            assert counts() == prior == (1,1)
            second, stored = invoke("call_tool", {"tool_id":published["tool_id"],"arguments":args})
            assert first == second and first["structuredContent"] == {"result":{k:snapshot[k] for k in args["flags"]}}
            events = db.execute("SELECT count(*) FROM events WHERE name='task.repeated_read'").fetchone()[0]
            assert events == 1 and counts() == (2,2)
            return {"script_sha256":hashlib.sha256(SCRIPT.encode()).hexdigest(),"script":SCRIPT,
                    "published_id":published["tool_id"],"executions":[direct,stored],"same_turn_repeat_events":events}
    finally:
        daemon.terminate()
        daemon.wait(timeout=10)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--live", action="store_true")
    parser.add_argument("--output", type=Path)
    options = parser.parse_args()
    snapshot = capture(os.environ.pop("QA_API_KEY")) if options.live else dict(auto_answer=False,general_knowledge=True,answer_ref=True)
    with tempfile.TemporaryDirectory() as directory:
        report = verify(Path(directory), snapshot)
    report["snapshot_source"] = "live QA GET /api/bot" if options.live else "fixture"
    if options.output:
        options.output.write_text(json.dumps(report, ensure_ascii=False, indent=2)+"\n")
    print("PASS QA execute/publish parity; one upstream call and one scoped read per execution")
