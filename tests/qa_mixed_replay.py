"""Replay a recorded natural QA turn through the production stdio entry point."""
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

from acceptance import Agent, binary, eventually, fixture


def replay(trace, publication, root):
    calls = [row for row in trace["calls"] if row["tool"] == "call_tool"]
    direct, composed = calls
    assert direct["arguments"]["tool_id"] == "qa/get_bot"
    assert composed["arguments"]["tool_id"] == "bridge.builtin/qa_bot_flags"
    assert (direct["thread"], direct["turn"]) == (composed["thread"], composed["turn"])
    snapshots = [row["flags"] for row in trace["upstream"]]
    assert len(snapshots) == len(calls)
    repair = next(row for row in publication["timeline"] if row["action"] == "candidate.repaired")
    state = root / "state"
    daemon = subprocess.Popen([binary, "serve", "-data", state, "-listen", "127.0.0.1:0"],
                              env={**os.environ, "BRIDGE_MASTER_KEY": secrets.token_hex(32)})
    try:
        eventually(lambda: (state / "bridge.sock").exists())
        with closing(Agent(state)) as client, closing(sqlite3.connect(state / "bridge.sqlite")) as db:
            client.call("bridge_create_source", source={"id":"qa","type":"stdio","command":sys.executable,
                        "args":[str(fixture / "server.py")],"env":{"QA_BOT_REPLAY":str(root)}})
            client.call("bridge_save_tool", name="qa_bot_flags", description="QA bot flags",
                        runtime="python", uses=["qa/get_bot"], script=repair["script"], input_schema={"type":"object"})

            def invoke(row, flags, thread, turn):
                (root / "snapshot.json").write_text(json.dumps(flags))
                result = client.rpc("tools/call", {"name":"call_tool","arguments":row["arguments"],
                    "_meta":{"threadId":thread,"x-codex-turn-metadata":{"thread_id":thread,"turn_id":turn}}})
                assert not result.get("isError"), result
                return {"tool":row["arguments"]["tool_id"],"thread":thread,"turn":turn,
                        "hits":sorted(r[0] for r in db.execute("SELECT hits FROM task_reads")),
                        "events":[r[0] for r in db.execute("SELECT eventid FROM events WHERE name='task.repeated_read' ORDER BY seq")]}

            steps = [invoke(row, flags, row["thread"], row["turn"]) for row, flags in zip(calls, snapshots)]
            assert steps[0]["hits"] == [1] and not steps[0]["events"]
            assert steps[1]["hits"] == [2] and len(steps[1]["events"]) == 1
            assert len((root / "reads.jsonl").read_text().splitlines()) == 2
            controls = [invoke(composed, snapshots[1], direct["thread"], "control-turn"),
                        invoke(direct, snapshots[0], "control-thread", direct["turn"])]
            assert controls[-1]["hits"] == [1, 1, 2]
            assert controls[0]["events"] == controls[1]["events"] == steps[1]["events"]
            return {"original_hits":trace["recorded_read_counts"],"original_events":trace["bridge_events_after_setup"],
                    "replay":steps,"isolation_controls":controls}
    finally:
        daemon.terminate()
        daemon.wait(timeout=10)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--record", type=Path, default=Path("bin/trace/qa-natural-handoff.json"))
    parser.add_argument("--publication", type=Path, default=Path("bin/trace/qa-incident-routed.json"))
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    with tempfile.TemporaryDirectory() as tmp:
        report = replay(json.loads(args.record.read_text()), json.loads(args.publication.read_text()), Path(tmp))
    args.output.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
