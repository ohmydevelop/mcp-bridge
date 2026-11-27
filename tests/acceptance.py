"""Start the production binary, drive its MCP entry points, restart, and verify delivery."""
import contextlib
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

root = Path(__file__).resolve().parents[1]
binary = str(root / "bin/mcp-bridge")
fixture = root / "testdata/qa"
version = "2025-06-18"

from qa_wire_trace import tool_request

class Agent:
    def __init__(self, state, *stdio_options):
        self.proc = subprocess.Popen([binary, "stdio", "-data", str(state), *stdio_options], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
        self.seq = 0
        self.init = self.rpc("initialize", {"protocolVersion": version,"capabilities":{},"clientInfo":{"name":"fixture","version":"1"}})
        self.send({"method":"notifications/initialized"})
    def send(self, obj):
        self.proc.stdin.write(json.dumps({"jsonrpc":"2.0",**obj})+"\n")
        self.proc.stdin.flush()
    def rpc(self, method, params):
        tool_request(method,params)
        self.seq += 1
        self.send({"method":method,"params":params,"id":self.seq})
        while True:
            line=self.proc.stdout.readline()
            if not line: raise RuntimeError("bridge closed stdio")
            r=json.loads(line)
            if r.get("id")==self.seq:
                if "error" in r: raise RuntimeError(r["error"])
                return r["result"]
    def call(self, tool_name, **kwargs):
        r=self.rpc("tools/call",{"name":tool_name,"arguments":kwargs})
        if r.get("isError"): raise RuntimeError((tool_name,r["content"]))
        return r["structuredContent"]
    def close(self):
        self.proc.stdin.close()
        self.proc.wait(timeout=10)

class HttpAgent:
    def __init__(self,base,token):
        self.base=base
        self.token=token
    def rpc(self,method,params):
        tool_request(method,params)
        params={**params,"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}
        headers={"Authorization":"Bearer "+self.token,"Mcp-Method":method,"MCP-Protocol-Version":"2026-07-28","Content-Type":"application/json","Accept":"application/json,text/event-stream"}
        if method=="tools/call": headers["Mcp-Name"]=params["name"]
        req=urllib.request.Request(self.base+"/mcp",data=json.dumps({"jsonrpc":"2.0","id":1,"method":method,"params":params}).encode(),headers=headers)
        with urllib.request.urlopen(req,timeout=10) as resp: raw=resp.read().decode()
        r=json.loads(next((line[5:] for line in raw.splitlines() if line.startswith("data:")),raw))
        if "error" in r:raise RuntimeError(r["error"])
        return r["result"]
    call=Agent.call

def eventually(fn):
    for _ in range(100):
        if result:=fn():return result
        time.sleep(.1)
    raise AssertionError("deadline exceeded")

def main():
    received=[]
    class Hook(BaseHTTPRequestHandler):
        def do_POST(self):
            received.append(json.loads(self.rfile.read(int(self.headers["Content-Length"]))))
            self.send_response(204)
            self.end_headers()
        def log_message(self,*_):pass
    with HTTPServer(("127.0.0.1",0),Hook) as receiver,tempfile.TemporaryDirectory() as tmp:
        threading.Thread(target=receiver.serve_forever,daemon=True).start()
        origin=f"http://127.0.0.1:{receiver.server_port}"
        state=Path(tmp)/"state"
        qa_events=Path(tmp)/"qa-events.json"
        with socket.socket() as s:
            s.bind(("127.0.0.1",0))
            port=s.getsockname()[1]
        base=f"http://127.0.0.1:{port}"
        tokens={k:secrets.token_hex(24) for k in ("reader","producer","basic")}
        env={**os.environ,"BRIDGE_MASTER_KEY":secrets.token_hex(32),"BRIDGE_WEBHOOK_ORIGINS":origin,"BRIDGE_TOKENS":json.dumps(tokens),"BRIDGE_PROFILES":json.dumps({"producer":{"producer_source":"qa"},"reader":{"events":True,"skills":True}})}
        @contextlib.contextmanager
        def daemon():
            p=subprocess.Popen([binary,"serve","-data",str(state),"-listen",f"127.0.0.1:{port}"],env=env)
            try:
                def ready():
                    if p.poll() is not None:raise RuntimeError("daemon exited")
                    try:return urllib.request.urlopen(base+"/healthz",timeout=1).read()==b"ok\n"
                    except OSError:return False
                eventually(ready)
                yield
            finally:
                p.terminate()
                p.wait(timeout=10)
        with daemon():
            a=Agent(state)
            assert a.init["protocolVersion"]==version
            normal,producer,basic=(HttpAgent(base,tokens[k]) for k in ("reader","producer","basic"))
            names=lambda client:{t["name"] for t in client.rpc("tools/list",{})["tools"]}
            business={"bridge_execute_script","search_tools","call_tool","search_skills","get_skill","search_event_types","save_trigger","get_trigger","bridge_get_tool","list_triggers","delete_trigger"}
            mgmt={"bridge_create_source","bridge_update_source","bridge_delete_source","bridge_save_tool","bridge_delete_tool","bridge_save_destination","bridge_delete_destination","bridge_get_status","bridge_list_sources"}
            assert names(basic)==names(normal)==names(a)==business|mgmt
            assert names(producer)=={"publish_event"}
            assert "backlog" in basic.call("bridge_get_status")
            try: producer.call("bridge_get_status")
            except (RuntimeError,urllib.error.HTTPError): pass
            else: raise AssertionError("producer unexpectedly received agent operations")
            src={"id":"qa","type":"stdio","command":sys.executable,"args":[str(fixture/"server.py")],
                 "env":{"QA_EVENT_FILE":str(qa_events)},"grants":["reader","basic"],"events":{"test_run.failed":"","run.ready":"poll_runs"},
                 "aliases":{"tool:failure_summary":["失败用例","失败摘要"],"event:test_run.failed":["支付项目测试失败"],"skill:triage-failure":["排查失败"]}}
            a.call("bridge_create_source",source=src)
            result=normal.call("call_tool",tool_id="qa/failure_summary",arguments={"limit":5})
            assert len(result["rows"])==5
            preview=a.call("bridge_create_source",source={"id":"preview"},package_path=str(fixture),dry_run=True)
            assert preview["servers"]==["qa"]
            a.call("bridge_update_source",source_id="qa",revision=1,patch={},package_path=str(fixture))
            assert normal.call("call_tool",tool_id="qa/failure_summary",arguments={"limit":5})==result
            a.call("bridge_update_source",source_id="qa",revision=2,patch={"aliases":{"tool:temporary":["temp"]}})
            a.call("bridge_update_source",source_id="qa",revision=3,patch={"aliases":{"tool:temporary":None}})
            aliases=a.call("bridge_get_status",source_id="qa")["aliases"]
            assert "tool:temporary" not in aliases and aliases["tool:failure_summary"]==["失败用例","失败摘要"]
            basic.call("bridge_save_tool",name="summary",description="Two failure IDs",input_schema={"type":"object"},
                   uses=["qa/failure_summary"],script='def run(args, ctx):\n    return [{"id": r["id"]} for r in ctx.call_tool("qa/failure_summary", {"limit": 5})["rows"][:2]]')
            assert normal.call("call_tool",tool_id="bridge.builtin/summary",arguments={})=={"result":[{"id":0},{"id":1}]}
            assert a.call("bridge_get_status")["published_tools"]["summary"]==1
            a.call("bridge_update_source",source_id="qa",revision=4,patch={"grants":["basic"]})
            assert normal.call("search_tools",query="bridge.builtin/summary")["tools"]==[]
            try: normal.call("call_tool",tool_id="bridge.builtin/summary",arguments={})
            except RuntimeError: pass
            else: raise AssertionError("revoked source grant still allows composition")
            a.call("bridge_update_source",source_id="qa",revision=5,patch={"grants":["reader","basic"]})
            basic.call("bridge_delete_tool",id="summary",revision=1)
            assert names(a)==business|mgmt
            for query in ("查询失败用例","qa/failure_summary","summary"):
                assert normal.call("search_tools",query=query)["tools"][0]["tool_id"]=="qa/failure_summary"
            skill=normal.call("search_skills",query="排查失败")["skills"][0]["skill_id"]
            assert "failure_summary" in normal.call("get_skill",skill_id=skill)["text"]
            event_type=normal.call("search_event_types",query="支付项目测试失败")["events"][0]
            flow=dict(trigger_id="qa-review",instruction="Triage this failure.",when=dict(type="event",source_id=event_type["source_id"],event_name=event_type["event_name"],filter={"project":"payment"}),skill_ref=skill)
            assert normal.call("save_trigger",**flow)["destinations"]==[]
            for id in ("review","extra"):
                a.call("bridge_save_destination",destination={"id":id,"url":origin+"/hook","grants":["reader"]},revision=0)
            assert len(normal.call("save_trigger",**flow)["destinations"])==2
            assert normal.call("list_triggers")["triggers"]==[]
            a.call("bridge_delete_destination",id="extra",revision=1)
            assert normal.call("save_trigger",**flow)["destination_id"]=="review"
            for client in (a,basic):
                assert client.call("list_triggers")["triggers"]==[]
                for method,kwargs in [("get_trigger",{}),("save_trigger",{"disabled":True,"revision":1})]:
                    try: client.call(method,trigger_id="qa-review",**kwargs)
                    except RuntimeError: pass
                    else: raise AssertionError("access to another identity's subscription")
            a.close()
        with daemon():
            event={"event_id":"qa-731","event_name":"test_run.failed","data":{"project":"payment","run_id":"731"}}
            for _ in range(2):producer.call("publish_event",**event)
            eventually(lambda:received)
            history=eventually(lambda:(x:=normal.call("get_trigger",trigger_id="qa-review")["deliveries"]) and x[0]["status"]=="accepted" and x)
            assert len(history)==len(received)==1 and received[0]["delivery_id"]==history[0]["id"]
            assert received[0]["instruction"]=="Triage this failure."
            assert normal.call("get_skill",skill_id=received[0]["skill_ref"])["text"]
        restored=Path(tmp)/"restored"
        shutil.copytree(state,restored,ignore=shutil.ignore_patterns("bridge.sock","bridge.lock"))
        state=restored
        with daemon():
            producer.call("publish_event",**event)
            assert len(normal.call("get_trigger",trigger_id="qa-review")["deliveries"])==1
            normal.call("save_trigger",trigger_id="qa-review",disabled=True,revision=1)
            a=Agent(state)
            assert "backlog" in a.call("bridge_get_status")
            a.call("save_trigger",trigger_id="auto-review",when=dict(type="event",source_id="qa",event_name="run.ready"),destination_id="review")
            a.close()
            qa_events.write_text(json.dumps([{"eventId":"r1","name":"run.ready","data":{"run":"qa-1"}}]))
            eventually(lambda:any(x["flow_id"]=="auto-review" for x in received))
            assert received[-1]["event"]["data"]["run"]=="qa-1"
        receiver.shutdown()
    print("PASS legacy stdio, modern HTTP visibility, package binding/PID reuse, skills, Python tools, filtered webhook, restart, restore, dedup and autonomous QA polling")
if __name__=="__main__":main()
