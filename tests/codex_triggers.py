"""Observe a fresh Codex setting up a trigger using only a business goal."""
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
import time

from acceptance import Agent, binary, eventually, fixture

GOALS = {
    "event": "请设置值班自动化：支付项目（项目标识 payment）的 QA 测试失败时，把该次运行的失败通知发给已配置的值班接收端。",
    "schedule": "请设置值班自动化：上海时间每分钟向已配置的值班接收端发送一次 QA 摘要，只含当前失败记录的总条数和前两个失败编号。"
}


def codex_turn(codex,home,project,state,calls,goal):
    proxy = str(Path(__file__).with_name("qa_natural.py"))
    args = [codex,"exec","--json","--ignore-user-config","--skip-git-repo-check","-s","read-only",
        "-C",str(project),"-m","gpt-6.2-sol","-c","mcp_servers.bridge.command="+json.dumps(sys.executable),
        "-c","mcp_servers.bridge.args="+json.dumps([proxy,"--proxy",str(calls),str(state)]),
        "-c",'mcp_servers.bridge.default_tools_approval_mode="approve"',goal]
    result = subprocess.run(args,env={**os.environ,"CODEX_HOME":str(home)},stdin=subprocess.DEVNULL,
                            capture_output=True,text=True,timeout=300)
    logs=[json.loads(line) for line in result.stdout.splitlines() if line.startswith("{")]
    return result,logs


def verify(kind, codex, output):
    messages = []
    class Receiver(BaseHTTPRequestHandler):
        def do_POST(self):
            body = self.rfile.read(int(self.headers["Content-Length"]))
            messages.append({"bytes": len(body), "body": json.loads(body)})
            self.send_response(204)
            self.end_headers()
        def log_message(self, *_):
            pass

    with tempfile.TemporaryDirectory() as tmp, HTTPServer(("127.0.0.1", 0), Receiver) as hook:
        threading.Thread(target=hook.serve_forever, daemon=True).start()
        root = Path(tmp)
        state, project, home = root/"state", root/"workspace", root/"home"
        project.mkdir()
        home.mkdir()
        (home/"auth.json").symlink_to(Path(os.environ.get("CODEX_HOME",Path.home()/".codex"))/"auth.json")
        origin = f"http://127.0.0.1:{hook.server_port}"
        proc = subprocess.Popen([binary,"serve","-data",state,"-listen","127.0.0.1:0"],
            env={**os.environ,"BRIDGE_MASTER_KEY":secrets.token_hex(32),"BRIDGE_WEBHOOK_ORIGINS":origin})
        try:
            eventually(lambda:(state/"bridge.sock").exists())
            rows = [{"id":n,"message":f"case-{n}"} for n in [731,732,744]]
            (root/"rows").write_text(json.dumps(rows))
            reads, events, calls = root/"reads", root/"events", root/"calls"
            with closing(Agent(state)) as admin:
                admin.call("bridge_create_source", source={"id":"qa","type":"stdio","command":sys.executable,
                    "args":[str(fixture/"server.py")],"env":{"QA_INCIDENT_FILE":str(root/"rows"),
                    "QA_READ_LOG":str(reads),"QA_EVENT_FILE":str(events)},"events":{"test_run.failed":"poll_runs"}})
                admin.call("bridge_save_destination",destination={"id":"oncall","url":origin+"/oncall"},revision=0)
                result, logs = codex_turn(codex,home,project,state,calls,GOALS[kind])
                catalog = admin.call("list_triggers")["triggers"]
                if kind=="event":
                    events.write_text(json.dumps([{"eventId":"e731","name":"test_run.failed","data":{"project":"payment","run_id":"731"}}]))
                until = time.monotonic() + (72 if kind=="schedule" else 12)
                while catalog and not messages and time.monotonic()<until:
                    time.sleep(.25)
                details = [admin.call("get_trigger",trigger_id=t["trigger_id"]) for t in catalog]
                report = {"goal":GOALS[kind],"fixture_rows":rows,"codex_exit":result.returncode,"thread":next((e.get("thread_id") for e in logs if e["type"]=="thread.started"),None),
                    "answer":next((e["item"]["text"] for e in reversed(logs) if e.get("item",{}).get("type")=="agent_message"),None),
                    "calls":[json.loads(x) for x in calls.read_text().splitlines()] if calls.exists() else [],
                    "upstream_reads":[json.loads(x) for x in reads.read_text().splitlines()] if reads.exists() else [],
                    "triggers":details,"notifications":messages}
                report["runs"]=[admin.call("get_trigger",trigger_id=t["trigger"]["trigger_id"],run_id=r["run_id"]) for t in details for r in t["runs"]]
                output.write_text(json.dumps(report,ensure_ascii=False,indent=2))
                print(kind,"exit",result.returncode,"calls",len(report["calls"]),"triggers",len(catalog),"notifications",len(messages))
        finally:
            proc.terminate()
            proc.wait(timeout=15)
            hook.shutdown()


if __name__=="__main__":
    p=argparse.ArgumentParser()
    p.add_argument("kind",choices=GOALS)
    p.add_argument("--codex",default="codex")
    p.add_argument("--output",required=True,type=Path)
    args=p.parse_args()
    verify(args.kind,args.codex,args.output)
