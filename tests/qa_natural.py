"""Observe one uncoached QA handoff. No trigger/adoption or call-count assertions."""
import argparse
import base64
import hmac
from http.server import BaseHTTPRequestHandler, HTTPServer
from queue import Queue
from contextlib import closing
import json
import os
from pathlib import Path
import secrets
import socket
import socketserver
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time

from acceptance import Agent,binary,eventually,fixture
from qa_process_eval import ask
from qa_replay import capture
from qa_thread_feedback import followup

GOAL="值班交接：现在有人在 QA 提问时，机器人会自动回复吗？通用知识和答案引用分别是什么状态？请给我一句能转给接班同事的结论。"
BUSINESS=["bridge_list_sources","search_tools","call_tool","search_event_types","save_trigger","get_trigger","search_skills","get_skill"]


def adapter(path):
    for line in sys.stdin:
        m=json.loads(line)
        if "id" not in m:continue
        method=m.get("method")
        if method=="initialize":
            value={"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"qa","version":"1"}}
        elif method=="tools/list":
            value={"tools":[{"name":"get_bot","description":"Read the QA bot configuration.",
                             "annotations":{"readOnlyHint":True},"inputSchema":{"type":"object","additionalProperties":False}}]}
        elif method=="tools/call" and m["params"]["name"]=="get_bot":
            with socket.socket(socket.AF_UNIX,socket.SOCK_STREAM) as sock:
                sock.connect(path)
                with sock.makefile("r") as stream:data=json.loads(stream.readline())
            if "error" in data:
                value={"isError":True,"content":[{"type":"text","text":data["error"]}]}
            else:
                value={"structuredContent":data["flags"],"content":[{"type":"text","text":json.dumps(data["flags"])}]}
        else:value={}
        print(json.dumps({"jsonrpc":"2.0","id":m["id"],"result":value}),flush=True)


def proxy(log,state):
    child=subprocess.Popen([binary,"stdio","-data",state],stdin=subprocess.PIPE,stdout=sys.stdout.buffer)
    try:
        for line in sys.stdin.buffer:
            m=json.loads(line)
            if m.get("method")=="tools/call":
                p=m["params"]; meta=p.get("_meta",{}); turn=meta.get("x-codex-turn-metadata",{})
                with open(log,"a") as f:
                    print(json.dumps({"time_ns":time.time_ns(),"tool":p["name"],"arguments":p["arguments"],
                                      "thread":meta.get("threadId"),"turn":turn.get("turn_id")},ensure_ascii=False),file=f)
            child.stdin.write(line);child.stdin.flush()
    finally:
        child.stdin.close();child.wait()


def main():
    p=argparse.ArgumentParser();p.add_argument("--codex",default="codex");p.add_argument("--output",type=Path,required=True)
    p.add_argument("--previous",type=Path,default=Path("bin/trace/qa-incident-routed.json"))
    p.add_argument("--followup", action="store_true", help="Keep the original thread and repeat the business question after maintenance feedback")
    p.add_argument("--trial", type=Path, help="Randomization database for the natural-repeat trial")
    args=p.parse_args()
    if args.trial:
        args.followup = True
    token=os.environ.pop("QA_API_KEY")
    previous=json.loads(args.previous.read_text())
    assert previous["passed"]
    repair=next(t for t in previous["timeline"] if t["action"]=="candidate.repaired")
    assert all(check["ok"] for check in repair["checks"])
    secret = secrets.token_hex(32)
    deliveries = Queue()
    class MaintenanceHook(BaseHTTPRequestHandler):
        def do_POST(self):
            raw = self.rfile.read(int(self.headers["Content-Length"]))
            signature = base64.b64encode(hmac.digest(secret.encode(), raw, "sha256")).decode()
            if self.path != "/review" or not hmac.compare_digest(signature, self.headers.get("X-Bridge-Signature","")):
                return self.send_error(403)
            deliveries.put(json.loads(raw))
            self.send_response(204)
            self.end_headers()
        def log_message(self, *_):
            pass

    upstream=[]
    class Live(socketserver.StreamRequestHandler):
        def handle(self):
            at=time.time_ns()
            try:reply={"flags":capture(token)}
            except Exception as err:reply={"error":type(err).__name__}
            upstream.append({"time_ns":at,**reply})
            self.wfile.write(json.dumps(reply).encode()+b"\n")
    with tempfile.TemporaryDirectory() as tmp:
        root=Path(tmp);state=root/"state";project=root/"workspace";project.mkdir();log=root/"calls"
        script=str(Path(__file__).resolve())
        with socketserver.UnixStreamServer(str(root/"live.sock"),Live) as relay, HTTPServer(("127.0.0.1", 0), MaintenanceHook) as hook:
            origin = f"http://127.0.0.1:{hook.server_port}"
            threading.Thread(target=hook.serve_forever, daemon=True).start()
            threading.Thread(target=relay.serve_forever,daemon=True).start()
            daemon=subprocess.Popen([binary,"serve","-data",state,"-listen","127.0.0.1:0"],
                                    env={**os.environ,"BRIDGE_CRED_QA_REVIEW":secret,"BRIDGE_WEBHOOK_ORIGINS":origin,"BRIDGE_MASTER_KEY":secrets.token_hex(32)})
            try:
                eventually(lambda:(state/"bridge.sock").exists())
                with closing(Agent(state)) as admin:
                    admin.call("bridge_create_source",source={"id":"qa","type":"stdio","command":sys.executable,
                        "args":[script,"--adapter",str(root/"live.sock")],"aliases":{"tool:get_bot":["QA","bot","机器人"]}})
                    admin.call("bridge_save_destination",destination={"id":"maintenance","url":origin+"/review","credential":"QA_REVIEW"},revision=0)
                    admin.call("save_trigger",trigger_id="qa-read-review",when=dict(type="event",source_id="bridge.builtin",
                               event_name="task.repeated_read",filter={"actor":"admin","tool_id":"qa/get_bot"}),
                               destination_id="maintenance")
                    admin.call("bridge_save_tool",name="qa_bot_flags",description="读取 QA 机器人的自动回答、通用知识和答案引用三个配置开关。Read the three QA bot configuration flags.",
                               aliases=["QA 机器人","机器人配置","配置开关","bot","bot flags","auto_answer","general_knowledge","answer_ref"],
                               runtime="python",uses=["qa/get_bot"],script=repair["script"],input_schema={"type":"object"})
                with closing(sqlite3.connect(state/"bridge.sqlite")) as db:
                    checkpoint=db.execute("SELECT coalesce(max(seq),0) FROM events").fetchone()[0]
                    argv=[args.codex,"exec","--ephemeral","--json","--ignore-user-config","--skip-git-repo-check",
                          "--disable","code_mode","--disable","code_mode_only","-s","read-only","-C",str(project),"-m","gpt-6.2-sol",
                          "-c","mcp_servers.bridge.command="+json.dumps(sys.executable),
                          "-c","mcp_servers.bridge.args="+json.dumps([script,"--proxy",str(log),str(state)]),
                          "-c","mcp_servers.bridge.enabled_tools="+json.dumps(BUSINESS),
                          "-c",'mcp_servers.bridge.default_tools_approval_mode="approve"',GOAL]
                    env = os.environ.copy()
                    if args.followup:
                        argv.remove("--ephemeral")
                        home = root/"codex-home"
                        home.mkdir()
                        auth = Path(env.get("CODEX_HOME", Path.home()/".codex"))/"auth.json"
                        if auth.exists():
                            (home/"auth.json").symlink_to(auth)
                        env["CODEX_HOME"] = str(home)
                        (home/"config.toml").write_text(f'[projects."{project}"]\ntrust_level="trusted"\n')
                    proc=subprocess.run(argv,env=env,stdin=subprocess.DEVNULL,capture_output=True,text=True,timeout=150)
                    items=[json.loads(line) for line in proc.stdout.splitlines() if line.startswith("{")]
                    results=[e["item"] for e in items if e.get("type")=="item.completed"]
                    db.row_factory=sqlite3.Row
                    events=[dict(row) for row in db.execute("SELECT seq,name,source,stamp FROM events WHERE seq>? ORDER BY seq",(checkpoint,))]
                    calls=[json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []
                    report={"model":"gpt-6.2-sol","goal":GOAL,"previous_tool_run":previous["run_id"],"codex_returncode":proc.returncode,
                            "turn_completed":any(e.get("type")=="turn.completed" for e in items),"calls":calls,"upstream":upstream,
                            "codex_items":results,"bridge_events_after_setup":events,
                            "recorded_read_counts":[r[0] for r in db.execute("SELECT hits FROM task_reads")]}
                    report["maintenance_delivery"] = None
                    report["maintenance_review"] = None
                    if args.trial:
                        args.output.write_text(json.dumps(report,ensure_ascii=False,indent=2)+"\n")
                    if any(event["name"] == "task.repeated_read" for event in events):
                        report["maintenance_delivery"] = deliveries.get(timeout=15)
                        if args.trial and report["turn_completed"]:
                            from qa_feedback_trial import enroll
                            from thread_feedback import feedback_target
                            thread = next(e["thread_id"] for e in items if e.get("type")=="thread.started")
                            feedback_target(report["maintenance_delivery"],thread,calls)
                            arm, advice = enroll(args.trial,thread,args.output.name)
                            report["arm"] = arm
                            report["maintenance_review"] = {"thread_advice":advice}
                        elif not args.trial:
                            skill = (fixture / "skills/optimize-bot-read/SKILL.md").read_text()
                            evidence = {"event":report["maintenance_delivery"],"calls":calls,"upstream":upstream,
                                        "active_script":repair["script"],"goal":GOAL}
                            report["maintenance_review"] = ask(args.codex,root,"review",
                                "Review the delivered QA event against the existing skill and active tool. "
                                "Explain where the repeated read occurred and whether any change is warranted. "
                                "Answer in Chinese. If asked for thread_advice, give the caller one general point about how to read current data efficiently. Omit tool names, identifiers and flag values from thread_advice.\n"+skill+"\n"+json.dumps(evidence,ensure_ascii=False),
                                {"finding":{"type":"string"},"proposed_change":{"type":"string"},
                                 **({"thread_advice":{"type":"string"}} if args.followup else {})})
                    if args.followup and report["turn_completed"] and (not args.trial or report.get("arm")):
                        thread = next(e["thread_id"] for e in items if e.get("type")=="thread.started")
                        app_argv = [args.codex,"app-server","--disable","code_mode","--disable","code_mode_only",
                                    *argv[argv.index("-c"):-1]]
                        report["upstream"] = list(upstream)
                        report["followup"] = followup(app_argv,project,env,thread,GOAL,report,log,upstream,inject=report.get("arm")!="control")
                        report["all_events"] = [dict(row) for row in db.execute("SELECT seq,name,source,stamp FROM events WHERE seq>? ORDER BY seq",(checkpoint,))]
                    args.output.parent.mkdir(parents=True,exist_ok=True)
                    args.output.write_text(json.dumps(report,ensure_ascii=False,indent=2)+"\n")
                    print("codex",proc.returncode,"calls",len(calls),"upstream",len(upstream),"events",len(events))
                    if proc.returncode:raise RuntimeError("Codex failed; see run report")
            finally:
                daemon.terminate();daemon.wait(timeout=10);relay.shutdown();hook.shutdown()

if __name__=="__main__":
    if sys.argv[1:2]==["--adapter"]:adapter(sys.argv[2])
    elif sys.argv[1:2]==["--proxy"]:proxy(*sys.argv[2:])
    else:main()
