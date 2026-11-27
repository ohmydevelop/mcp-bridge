"""Record a real Codex CLI turn against bridge and a deterministic loopback model."""
import argparse
import json
import os
from pathlib import Path
import secrets
import subprocess
import tempfile
import threading
from http.server import HTTPServer, BaseHTTPRequestHandler
from acceptance import Agent, binary, fixture, eventually

def main():
    parser=argparse.ArgumentParser()
    parser.add_argument("codex",type=Path)
    parser.add_argument("output",type=Path)
    opts=parser.parse_args()
    folder=opts.output.resolve()
    folder.mkdir(parents=True,exist_ok=True)
    calls=[]
    class Model(BaseHTTPRequestHandler):
        def do_POST(self):
            raw=self.rfile.read(int(self.headers["Content-Length"]))
            calls.append(json.loads(raw))
            n=len(calls)
            (folder/f"request-{n}.json").write_bytes(raw)
            if n==1:
                item={"type":"function_call","namespace":"mcp__bridge","name":"call_tool","arguments":json.dumps({"tool_id":"bridge.builtin/summary","arguments":{}}),"call_id":"qa_call"}
            else:item={"type":"message","role":"assistant","id":"msg_done","content":[{"type":"output_text","text":"QA summary received."}]}
            rid=f"local_{n}"
            events=[{"type":"response.created","response":{"id":rid}},{"type":"response.output_item.done","item":item},{"type":"response.completed","response":{"id":rid,"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}]
            data="".join("event: "+e["type"]+"\ndata: "+json.dumps(e)+"\n\n" for e in events).encode()
            self.send_response(200)
            self.send_header("Content-Type","text/event-stream")
            self.send_header("Content-Length",str(len(data)))
            self.end_headers()
            self.wfile.write(data)
    with tempfile.TemporaryDirectory() as tmp,HTTPServer(("127.0.0.1",0),Model) as model:
        root=Path(tmp);state=root/"db";home=root/"home";project=root/"project"
        config=home/".codex";config.mkdir(parents=True);project.mkdir()
        proc=subprocess.Popen([binary,"serve","-listen","127.0.0.1:0","-data",str(state)],env={**os.environ,"BRIDGE_MASTER_KEY":secrets.token_hex(32)})
        try:
            eventually(lambda:(state/"bridge.sock").exists())
            a=Agent(state)
            a.call("bridge_create_source",source={"id":"qa","type":"stdio","command":"python3","args":[str(fixture/"server.py")]})
            a.call("bridge_save_tool",name="summary",description="Get five failure IDs.",input_schema={"type":"object"},
                   runtime="python",uses=["qa/failure_summary"],script="""def run(args, ctx):
    rows, offset = [], 0
    while offset is not None:
        batch = ctx.call_tool("qa/failure_summary", {"limit": 250, "offset": offset})
        rows.extend(batch["rows"])
        offset = batch["next_offset"]
    assert len(rows) == 1000
    return [{"id": r["id"]} for r in rows[:5]]""")
            def expect_error(tool_name, expected, **args):
                try: a.call(tool_name,**args)
                except RuntimeError as exc: assert expected in str(exc),exc
                else: raise AssertionError(f"{tool_name} unexpectedly succeeded")
            definition=dict(name="probe",description="Check dependency enforcement",input_schema={"type":"object"},
                            runtime="python",uses=["qa/failure_summary"])
            expect_error("bridge_save_tool","run",**definition,script="value = 1")
            assert "probe" not in a.call("bridge_get_status")["published_tools"]
            a.call("bridge_save_tool",**definition,script='def run(args, ctx): return ctx.call_tool("qa/poll_runs", {})')
            expect_error("call_tool","undeclared",tool_id="bridge.builtin/probe",arguments={})
            a.close()
            subprocess.run(["git","init","-q",project],check=True)
            (config/"config.toml").write_text(f'''model = "gpt-5.2-codex"
model_provider = "capture"
[model_providers.capture]
name = "capture"
base_url = "http://127.0.0.1:{model.server_port}/v1"
wire_api = "responses"
[features]
code_mode = false
code_mode_only = false
[projects."{project}"]
trust_level = "trusted"
[mcp_servers.bridge]
command = "{binary}"
args = ["stdio","-data","{state}"]
enabled_tools = ["call_tool"]
default_tools_approval_mode = "approve"
''')
            threading.Thread(target=model.serve_forever,daemon=True).start()
            subprocess.run([str(opts.codex.resolve()),"exec","--ephemeral","-C",str(project),"Report five QA failure IDs."],cwd=project,env={"HOME":str(home),"CODEX_HOME":str(config),"PATH":os.environ["PATH"],"LANG":"C.UTF-8"},check=True,timeout=60)
            assert len(calls)==2
            outputs=[x for x in calls[1]["input"] if x.get("type")=="function_call_output"]
            assert len(outputs)==1,outputs
            output=outputs[0]["output"]
            if isinstance(output,list):output="\n".join(x["text"] for x in output)
            parsed=json.loads(output.split("\nOutput:\n",1)[-1])
            # Hosts may wrap an MCP text result as a serialized content list.
            if isinstance(parsed,list): parsed=json.loads(parsed[0]["text"])
            assert parsed=={"result":[{"id":i} for i in range(5)]},output
            print("PASS real Codex: 1000 upstream rows, only 5 IDs entered the model request")
        finally:
            proc.terminate();proc.wait(timeout=10)
if __name__=="__main__":main()
