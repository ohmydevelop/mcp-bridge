"""Single live QA incident, two feedback loops and adoption in the original thread."""
import argparse
from contextlib import closing,ExitStack
from http.server import BaseHTTPRequestHandler,HTTPServer
import json
import os
from pathlib import Path
import queue
import secrets
import socket
import subprocess
import sys
import sqlite3
import tempfile
import threading
import time
from uuid import uuid4

from acceptance import Agent,HttpAgent,binary,eventually,fixture
from codex_turn_replay import verify_turns
from qa_process_eval import ask,replay_tools
from qa_replay import capture,candidate
from qa_wire_trace import record
from qa_validation import FLAGS,active_suite,original_cases

def port():
    with socket.socket() as s:
        s.bind(("127.0.0.1",0))
        return s.getsockname()[1]

def stop(p):
    p.terminate();p.wait(timeout=10)

def drive(root,codex,report,save):
    rid=report["run_id"]
    def step(action,**details):
        report["timeline"].append({"time_ns":time.time_ns(),"action":action,**details})
        save()
        print(action,flush=True)
    initial=capture(os.environ.pop("QA_API_KEY"))
    step("GET /api/bot",flags=initial)
    state=root/"bridge"
    db=root/"journal.sqlite"
    os.environ["QA_VALIDATION_DB"]=str(db)
    journal,rev,additions=active_suite()
    incoming=queue.Queue()
    notify=None
    origin_delivery=None
    class Hook(BaseHTTPRequestHandler):
        def do_POST(self):
            if self.path!="/"+rid:return self.send_error(404)
            envelope=json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            record("webhook.received",event=envelope["event"]["name"],delivery=envelope["delivery_id"])
            if envelope["event"]["name"]=="catalog.updated":
                if not notify or envelope["event"]["data"]["origin_delivery_id"]!=origin_delivery:
                    return self.send_error(409)
                try:receipt=notify(envelope)
                except Exception as exc:
                    return self.send_error(503,type(exc).__name__)
                incoming.put((envelope,receipt))
            else:
                incoming.put(envelope)
            self.send_response(204);self.end_headers()
        def log_message(self,*_):pass
    with ExitStack() as stack:
        hook=stack.enter_context(HTTPServer(("127.0.0.1",0),Hook))
        threading.Thread(target=hook.serve_forever,daemon=True).start()
        stack.callback(hook.shutdown)
        origin=f"http://127.0.0.1:{hook.server_port}"
        bp,wp=port(),port()
        secret=secrets.token_hex(32);token=secrets.token_hex(32)
        env={**os.environ,"BRIDGE_MASTER_KEY":secrets.token_hex(32),"BRIDGE_TOKENS":json.dumps({"producer":token}),
             "BRIDGE_PROFILES":json.dumps({"producer":{"producer_source":"qa"}}),"QA_REVIEW_HOOK_KEY":secret,
             "BRIDGE_CRED_QA_REVIEW":secret,"BRIDGE_WEBHOOK_ORIGINS":origin+f",http://127.0.0.1:{wp}"}
        bridge=subprocess.Popen([binary,"serve","-data",state,"-listen",f"127.0.0.1:{bp}"],env=env)
        stack.callback(stop,bridge)
        eventually(lambda:(state/"bridge.sock").exists())
        admin=stack.enter_context(closing(Agent(state)))
        incident_file=root/"failures.json"
        admin.call("bridge_create_source",source={"id":"qa","type":"stdio","command":sys.executable,
                   "args":[str(fixture/"server.py")],"env":{"QA_BOT_REPLAY":str(root),"QA_INCIDENT_FILE":str(incident_file)},
                   "events":{"test_run.failed":""}},package_path=str(fixture))
        admin.call("bridge_save_destination",destination={"id":"incident-hook","url":origin+"/"+rid},revision=0)
        for flow,source,event,skill in [("incident","qa","test_run.failed","triage-failure"),
                                        ("qa-optimize","bridge.builtin","task.repeated_read","optimize-bot-read")]:
            ref=admin.call("search_skills",query=skill)["skills"][0]["skill_id"]
            admin.call("save_trigger",trigger_id=flow,when=dict(type="event",source_id=source,event_name=event,
                       filter={"run_id":rid} if flow=="incident" else {"tool_id":"qa/get_bot"}),destination_id="incident-hook",
                       skill_ref=ref)
        worker=Path(__file__).resolve().parents[1]/"examples/qa-maintenance/worker.py"
        w=subprocess.Popen([sys.executable,worker,"--db",db,"--state",state,"--port",str(wp),"--codex",codex],env=env)
        stack.callback(stop,w)
        def ready():
            try:return admin.call("get_trigger",trigger_id="qa-review")
            except RuntimeError:return False
        eventually(ready)
        log=root/"reads.jsonl"
        (root/"snapshot.json").write_text(json.dumps(initial));log.write_text("")
        values={k:admin.call("call_tool",tool_id="qa/get_bot",arguments={}).get(k) for k in FLAGS}
        failure={"run_id":rid,"check":"three_flags_upstream_read_budget","allowed":1,"observed":len(log.read_text().splitlines())}
        assert values=={k:initial.get(k) for k in FLAGS} and failure["observed"]>1
        incident_file.write_text(json.dumps([failure]))
        step("smoke.failed",calls=["qa/get_bot"]*3,**failure)
        producer=HttpAgent(f"http://127.0.0.1:{bp}",token)
        producer.call("publish_event",event_id=rid,event_name="test_run.failed",data=failure)
        alert=incoming.get(timeout=15)
        assert alert["event"]["eventId"]==rid
        step("test_run.failed",delivery=alert)
        turns,observe,notify=stack.enter_context(verify_turns(codex,root,state,admin,incident=alert))
        turn=turns[0]
        step("incident.triaged",turn=turn)
        calls=[i for i in turn["items"] if i.get("type")=="mcpToolCall" and i.get("status")=="completed"]
        assert any(i.get("tool")=="get_skill" for i in calls) and any(i["arguments"].get("tool_id")=="qa/failure_summary" for i in calls)
        repeat=incoming.get(timeout=15);metrics=repeat["event"]["data"]
        assert metrics["task_scope"]=="codex:"+turn["thread_id"]+":"+turn["turn_id"]
        step("task.repeated_read",delivery=repeat)
        skill=admin.call("get_skill",skill_id=repeat["skill_ref"])["text"]
        work=root/"first";work.mkdir()
        code=candidate(codex,work,skill,metrics,True)
        # Explicit acceptance fault: simulate a legacy generated-output adapter.
        # No fault is applied to the QA service, the model or the verifier.
        v1=code.replace("def run(","def generated_run(",1)+'''
def run(args, ctx):
    class LegacyReader:
        def call_tool(self, name, arguments):
            data=ctx.call_tool(name,arguments)
            if "auto_answer" in data and data["auto_answer"] is None and len(data)==3:
                return dict.fromkeys(data)
            return data
    return generated_run(args,LegacyReader())
'''
        publish,run=replay_tools(admin,root)
        publish("incident_v1",v1)
        base=[s for _,s in original_cases(initial,"live")]+additions
        old=[run("incident_v1",s) for s in base]
        # Only complete input with an explicitly-null primary field exercises the compatibility regression.
        shadow=[dict(zip(FLAGS,[None,False,True])),dict(zip(FLAGS,[None,True,False]))]
        late=[run("incident_v1",s) for s in shadow]
        step("candidate.replayed",model_script=code,canary_script=v1,old_checks=old,shadow=late)
        assert all(x["ok"] for x in old) and any(not x["ok"] for x in late)
        journal.record(rid,rev,old,late)
        step("review.record.committed",revision=rev)
        deadline=time.monotonic()+240
        while time.monotonic()<deadline:
            with closing(journal.connect()) as conn:
                row=conn.execute("SELECT status,report FROM jobs WHERE run=?",(rid,)).fetchone()
            if row and row[0]!="pending":break
            if w.poll() is not None:raise RuntimeError("review worker exited")
            time.sleep(.4)
        assert row and row[0]=="promoted",row and row[0]
        review=json.loads(row[1]);rev2,current=journal.suite()
        history=admin.call("get_trigger",trigger_id="qa-review")["deliveries"]
        step("qa.replay.completed -> suite.promoted",webhook=history,review=review,revision=rev2)
        assert rev2==rev+1 and len(history)==1 and review["training_records"][0]["run"]==rid
        new_base=[s for _,s in original_cases(initial,"live")]+current
        caught=[r for s in new_base if not (r:=run("incident_v1",s))["ok"]]
        assert caught
        fixed=ask(codex,root,"repair_incident","Repair this exact QA candidate. Return a complete script.\n"+skill+"\n"+v1+"\n"+json.dumps(caught),{"script":{"type":"string"}})["script"]
        publish("incident_v2",fixed)
        passed=[run("incident_v2",s) for s in new_base+shadow]
        assert all(r["ok"] for r in passed)
        step("candidate.repaired",failed_v1=caught,script=fixed,checks=passed,suite_cases=len(new_base))
        origin_delivery=repeat["delivery_id"]
        admin.call("bridge_save_tool",name="qa_bot_flags",description="读取 QA 机器人的自动回答、通用知识和答案引用三个配置开关。Read the three QA bot configuration flags.",
                   aliases=["QA 机器人","机器人配置","配置开关","bot","bot flags","auto_answer","general_knowledge","answer_ref"],
                   input_schema={"type":"object"},runtime="python",uses=["qa/get_bot"],script=fixed,origin_delivery_id=repeat["delivery_id"])
        for name in ("incident_v1","incident_v2"):
            admin.call("bridge_delete_tool",id=name,revision=1)
        notice,receipt=incoming.get(timeout=15)
        assert notice["event"]["name"]=="catalog.updated" and notice["event"]["data"]["origin_delivery_id"]==repeat["delivery_id"]
        step("catalog.updated -> thread/inject_items",delivery=notice,receipt=receipt)
        (root/"snapshot.json").write_text(json.dumps(initial))
        final=observe(12,turn["thread_id"])
        step("original_agent.next_turn",result=final)
        assert any(c["arguments"].get("tool_id")=="bridge.builtin/qa_bot_flags" and c["status"]=="completed" for c in final["calls"])
        assert final["upstream_reads"]==1
        with closing(sqlite3.connect(state/"bridge.sqlite")) as ledger:
            ledger.row_factory=sqlite3.Row
            report["stored_events"]=[dict(row) for row in ledger.execute("SELECT seq,source,eventid,name,stamp FROM events ORDER BY seq")]
            report["deliveries"]=[dict(row) for row in ledger.execute("SELECT id,flow,destination,status,attempt FROM deliveries ORDER BY rowid")]
        report["passed"]=True;save()

if __name__=="__main__":
    p=argparse.ArgumentParser();p.add_argument("--output",required=True,type=Path);p.add_argument("--codex",default="codex")
    args=p.parse_args();args.output.parent.mkdir(parents=True,exist_ok=True)
    report={"run_id":str(uuid4()),"passed":False,"timeline":[]}
    trace=args.output.with_suffix(".jsonl").resolve()
    trace.write_text("")
    os.environ.update(QA_TRACE_FILE=str(trace),QA_TRACE_RUN=report["run_id"])
    def save():
        args.output.write_text(json.dumps(report,ensure_ascii=False,indent=2)+"\n")
    with tempfile.TemporaryDirectory() as temp:drive(Path(temp),args.codex,report,save)
