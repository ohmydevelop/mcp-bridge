"""Exercise event-driven handler promotion using a QA fixture and an independent snapshot."""
import argparse
from contextlib import closing
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import tempfile
import threading

from acceptance import Agent, binary, eventually, fixture
from codex_triggers import codex_turn

SLOW = """def run(args, ctx):
    auto = ctx.call_tool("qa/get_bot", {})["auto_answer"]
    refs = ctx.call_tool("qa/get_bot", {})["answer_ref"]
    return {"action": "notify", "data": {"auto_answer": auto, "answer_ref": refs}}
"""
FAST = """def run(args, ctx):
    bot = ctx.call_tool("qa/get_bot", {})
    return {"action":"notify","data":{key:bot[key] for key in ("auto_answer","answer_ref")}}
"""


def exercise(root, codex):
    deliveries = []
    class Receiver(BaseHTTPRequestHandler):
        def do_POST(self):
            raw = self.rfile.read(int(self.headers["Content-Length"]))
            deliveries.append((self.path,json.loads(raw)))
            self.send_response(204)
            self.end_headers()
        def log_message(self,*_):
            pass
    with HTTPServer(("127.0.0.1",0),Receiver) as hook:
        origin = f"http://127.0.0.1:{hook.server_port}"
        threading.Thread(target=hook.serve_forever,daemon=True).start()
        snapshot={"auto_answer":False,"answer_ref":True}
        unseen={"auto_answer":True,"answer_ref":False}
        (root/"snapshot.json").write_text(json.dumps(snapshot))
        state=root/"state"
        daemon=subprocess.Popen([binary,"serve","-data",state,"-listen","127.0.0.1:0"],
            env={**os.environ,"BRIDGE_MASTER_KEY":secrets.token_hex(32),"BRIDGE_WEBHOOK_ORIGINS":origin})
        try:
            eventually(lambda:(state/"bridge.sock").exists())
            with closing(Agent(state)) as client:
                events=root/"events.json"
                client.call("bridge_create_source",source={"id":"qa","type":"stdio","command":sys.executable,
                    "args":[str(fixture/"server.py")],"events":{"test_run.failed":"poll_runs"},
                    "env":{"QA_EVENT_FILE":str(events),"QA_BOT_REPLAY":str(root)}})
                for dest in ["business","maintenance"]:
                    client.call("bridge_save_destination",destination={"id":dest,"url":origin+"/"+dest},revision=0)
                base={"name":"bot_flags","uses":["qa/get_bot"],"description":"Read two bot flags","input_schema":{"type":"object"}}
                handler=client.call("bridge_save_tool",**base,script=SLOW)
                sample={"arguments":{"event":{"data":{"run_id":"independent"}}},"expected":{"action":"notify","data":unseen},
                        "calls":[{"tool_id":"qa/get_bot","arguments":{},"response":unseen}]*2}
                client.call("save_trigger",trigger_id="on-failure",when={"type":"event","source_id":"qa","event_name":"test_run.failed"},
                            destination_id="business",handler=handler,replay=True,promotion={"enabled":True,"metric":"reads","holdout":[sample]})
                client.call("save_trigger",trigger_id="review",when={"type":"event","source_id":"bridge.builtin","event_name":"trigger.run.completed",
                    "filter":{"trigger_id":"on-failure"}},destination_id="maintenance",skill_ref="skill://bridge.builtin/improve-trigger/SKILL.md")
                def emit(count):
                    events.write_text(json.dumps([{"eventId":str(i),"name":"test_run.failed","data":{"run_id":str(i)}} for i in range(count)]))
                def find(path):
                    return next((msg for route,msg in deliveries if route==path),None)
                emit(1)
                msg=eventually(lambda:find("/maintenance"))
                first=client.call("get_trigger",trigger_id="on-failure",run_id=msg["event"]["data"]["run_id"])
                count=lambda:len((root/"reads.jsonl").read_text().splitlines())
                assert first["processing"]["reads"]==count()==2
                calls=[];answer=None
                if codex:
                    home,project=root/"home",root/"workspace"
                    home.mkdir();project.mkdir()
                    (home/"auth.json").symlink_to(Path(os.environ.get("CODEX_HOME",Path.home()/".codex"))/"auth.json")
                    log=root/"calls"
                    goal="处理这次自动化维护通知，按附带的维护说明检查是否有可验证的改进。如有，提交候选版本。通知内容："+json.dumps(msg,ensure_ascii=False)
                    result,logs=codex_turn(codex,home,project,state,log,goal)
                    calls=[json.loads(x) for x in log.read_text().splitlines()] if log.exists() else []
                    answer=next((x["item"]["text"] for x in reversed(logs) if x.get("item",{}).get("type")=="agent_message"),None)
                    assert result.returncode==0, result.returncode
                else:
                    client.call("bridge_save_tool",**base,script=FAST,revision=1)
                state_now=client.call("get_trigger",trigger_id="on-failure")
                if not codex or client.call("bridge_get_tool",tool_id=handler["tool_id"])["revision"]>1:
                    state_now=eventually(lambda:(r:=client.call("get_trigger",trigger_id="on-failure")).get("review") and r)
                before=count()
                emit(2)
                eventually(lambda:len([m for r,m in deliveries if r=="/business"])==2)
                msg=[m for r,m in deliveries if r=="/business"][-1]
                after=client.call("get_trigger",trigger_id="on-failure",run_id=msg["run_id"])
                if not codex:
                    assert state_now["review"]["decision"]=="promoted" and before==2
                    assert after["processing"]["reads"]==1 and count()==3
                assert find("/business")["data"]==msg["data"]==snapshot
                return {"maintenance": "Codex" if codex else "fixed candidate","decision":state_now.get("review"),"answer":answer,"calls":calls,
                    "baseline_reads":2,"reads_after_review":before,"next_run_reads":count()-before,
                    "versions":[first["handler"]["revision"],after["handler"]["revision"]],"output":msg["data"]}
        finally:
            daemon.terminate();daemon.wait(timeout=10)
            hook.shutdown()


if __name__=="__main__":
    parser=argparse.ArgumentParser()
    parser.add_argument("--codex")
    parser.add_argument("--output",type=Path)
    opts=parser.parse_args()
    with tempfile.TemporaryDirectory() as temp:
        result=exercise(Path(temp),opts.codex)
    if opts.output:
        opts.output.write_text(json.dumps(result,ensure_ascii=False,indent=2))
    print(result["maintenance"],result["decision"],result["baseline_reads"],result["next_run_reads"])
