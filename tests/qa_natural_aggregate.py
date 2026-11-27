"""One live QA aggregation turn. Count HTTP reads and Codex tool-result payload bytes."""
import argparse
from collections import defaultdict
from contextlib import closing
import json
import os
from pathlib import Path
import secrets
import socket
import socketserver
import subprocess
import sys
import tempfile
import threading
import urllib.parse
import urllib.request

from acceptance import Agent, binary, eventually
from qa_natural import proxy

GOAL = "请做一份 QA 问答值班小结：取当前问答列表前 30 条，按产品列出问答条数和评论总数，未标注产品的单列，最后给出合计。"
NAME = "list_questions"
HERE = str(Path(__file__).resolve())


def adapter(path):
    for line in sys.stdin:
        req = json.loads(line)
        if "id" not in req:
            continue
        method = req["method"]
        if method == "initialize":
            res = {"protocolVersion":"2025-06-18","capabilities":{"tools":{}},
                   "serverInfo":{"name":"qa","version":"1"}}
        elif method == "tools/list":
            res = {"tools":[{"name":NAME,"description":"QA 问答列表，返回每条问答的 ID、产品和评论数，按服务端默认顺序分页。",
                "annotations":{"readOnlyHint":True},"inputSchema":{"type":"object","properties":{
                    "page":{"type":"integer","minimum":1,"default":1},
                    "size":{"type":"integer","minimum":1,"maximum":100,"default":20}},"additionalProperties":False}}]}
        elif method == "tools/call" and req["params"]["name"] == NAME:
            with socket.socket(socket.AF_UNIX,socket.SOCK_STREAM) as sock:
                sock.connect(path)
                sock.sendall((json.dumps(req["params"]["arguments"])+"\n").encode())
                with sock.makefile() as reader:
                    data=json.loads(reader.readline())
            res={"content":[{"type":"text","text":json.dumps(data,ensure_ascii=False)}]}
            if "error" in data:
                res["isError"]=True
            else:
                res["structuredContent"]=data
        else:
            res={}
        print(json.dumps({"jsonrpc":"2.0","id":req["id"],"result":res}),flush=True)


def run(codex, output):
    token=os.environ.pop("QA_API_KEY")
    reads=[]
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self,*_):
            return None
    class Live(socketserver.StreamRequestHandler):
        def handle(self):
            params=json.loads(self.rfile.readline())
            query={"forum_id":1,"type":"qa","page":params.get("page",1),"size":params.get("size",20)}
            record={"query":query}
            reads.append(record)
            try:
                req=urllib.request.Request("https://qa.chaitin.cn/api/discussion?"+urllib.parse.urlencode(query),
                                           headers={"X-KOALA-TOKEN":token})
                with urllib.request.build_opener(NoRedirect).open(req,timeout=20) as resp:
                    raw=resp.read()
                payload=json.loads(raw)
                if not payload.get("success"):
                    raise ValueError("unsuccessful QA response")
                rows=[{k:item[k] for k in ("id","product_name","comment")} for item in payload["data"]["items"]]
                result={"rows":rows,"page":query["page"],"size":query["size"],"total":payload["data"].get("total")}
                record.update(rows=rows,api_body_bytes=len(raw))
            except Exception as err:
                result={"error":type(err).__name__}
                record.update(result)
            self.wfile.write(json.dumps(result).encode()+b"\n")
    with tempfile.TemporaryDirectory() as tmp:
        root=Path(tmp); state=root/"state"; project=root/"workspace";project.mkdir()
        home=root/"codex-home";home.mkdir()
        auth=Path(os.getenv("CODEX_HOME",Path.home()/".codex"))/"auth.json"
        (home/"auth.json").symlink_to(auth)
        with socketserver.UnixStreamServer(str(root/"relay.sock"),Live) as relay:
            threading.Thread(target=relay.serve_forever,daemon=True).start()
            proc=subprocess.Popen([binary,"serve","-data",state,"-listen","127.0.0.1:0"],
                                  env={**os.environ,"BRIDGE_MASTER_KEY":secrets.token_hex(32)})
            try:
                eventually(lambda:(state/"bridge.sock").exists())
                with closing(Agent(state)) as admin:
                    admin.call("bridge_create_source",source={"id":"qa","type":"stdio","command":sys.executable,
                        "args":[HERE,"--adapter",str(root/"relay.sock")],"aliases":{"tool:"+NAME:["QA","问答","产品","评论"]}})
                log=root/"calls"
                args=[codex,"exec","--json","--ignore-user-config","--skip-git-repo-check","--disable","code_mode",
                      "--disable","code_mode_only","-s","read-only","-C",str(project),"-m","gpt-6.2-sol",
                      "-c","mcp_servers.bridge.command="+json.dumps(sys.executable),
                      "-c","mcp_servers.bridge.args="+json.dumps([HERE,"--proxy",str(log),str(state)]),
                      "-c",'mcp_servers.bridge.default_tools_approval_mode="approve"',GOAL]
                result=subprocess.run(args,env={**os.environ,"CODEX_HOME":str(home)},stdin=subprocess.DEVNULL,
                                      capture_output=True,text=True,timeout=240)
                events=[json.loads(l) for l in result.stdout.splitlines() if l.startswith("{")]
                files=list((home/"sessions").rglob("rollout-*.jsonl"))
                assert len(files)==1,files
                records=[v["payload"] for v in map(json.loads,files[0].read_text().splitlines()) if v["type"]=="response_item"]
                names={v["call_id"]:v.get("name",v["type"]) for v in records if "call_id" in v and not v["type"].endswith("_output")}
                outputs=[]
                for v in records:
                    if v["type"] not in ("function_call_output","custom_tool_call_output"):continue
                    value=v["output"]
                    content=[value] if isinstance(value,str) else [x["text"] for x in value if "text" in x]
                    outputs.append({"name":names.get(v["call_id"]),"call_id":v["call_id"],
                                    "utf8_text_bytes":sum(len(x.encode()) for x in content),"text":content})
                by_index={}
                for row in reads:
                    for i,item in enumerate(row.get("rows",[]),(row["query"]["page"]-1)*row["query"]["size"]):
                        by_index[i]=item
                top=[by_index[i] for i in range(30) if i in by_index]
                groups=defaultdict(lambda:[0,0])
                for item in top:
                    g=groups[item["product_name"] or "未标注产品"];g[0]+=1;g[1]+=item["comment"]
                report={"goal":GOAL,"model":"gpt-6.2-sol","codex_returncode":result.returncode,
                        "completed":any(x["type"]=="turn.completed" for x in events),
                        "answer":next((x["item"]["text"] for x in reversed(events) if x.get("item",{}).get("type")=="agent_message"),None),
                        "calls":[json.loads(l) for l in log.read_text().splitlines()] if log.exists() else [],
                        "model_calls":[v for v in records if v["type"] in ("function_call","custom_tool_call")],"reads":reads,"outputs":outputs,"expected_groups":dict(groups),"covered_rows":len(top),
                        "unique_ids":len({r["id"] for r in top})}
                output.write_text(json.dumps(report,ensure_ascii=False,indent=2))
                print("completed",report["completed"],"calls",len(report["calls"]),"QA requests",len(reads),"outputs",len(outputs))
            finally:
                proc.terminate();proc.wait();relay.shutdown()


if __name__=="__main__":
    if sys.argv[1:2]==["--adapter"]:adapter(sys.argv[2])
    elif sys.argv[1:2]==["--proxy"]:proxy(*sys.argv[2:])
    else:
        p=argparse.ArgumentParser();p.add_argument("--codex",required=True);p.add_argument("--output",type=Path,required=True)
        args=p.parse_args();run(args.codex,args.output)
