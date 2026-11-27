"""Compare actual first Codex MCP calls against two production tool catalogs.

Run in a Python environment with fastjsonschema. Both versions use the same isolated
QA fixture and prompts. The replay server records calls without executing them.
"""
import argparse
import concurrent.futures
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import tempfile
import time

import acceptance

CASES = [
    ("List the connected MCP sources.", "list_event_sources", "bridge_list_sources"),
    ("有哪些工具可以查失败用例？", "search_tools", "search_tools"),
    ("Find a skill about triaging failures.", "search_skills", "search_skills"),
    ("查找 qa 来源中可以订阅的测试失败事件。", "list_event_types", "search_event_types"),
    ("Show the delivery history for flow qa-review.", "get_trigger", "get_trigger"),
    ("查看 Bridge 是否健康以及是否有积压。", "bridge_get_status", "bridge_get_status"),
    ("Read skill://qa/triage-failure/SKILL.md", ("get_skill", "read_skill_file"), "get_skill"),
    ("调用 qa/failure_summary 工具，参数 limit 为 5。", ("call_tool", "qa__failure_summary"), "call_tool"),
]

def replay(catalog, log):
    tools = json.loads(Path(catalog).read_text())
    for line in sys.stdin:
        req = json.loads(line)
        if "id" not in req: continue
        method = req["method"]
        if method == "initialize":
            value = {"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"bridge","version":"eval"}}
        elif method == "tools/list":
            value = {"tools":tools}
        elif method == "tools/call":
            with open(log,"a") as f: f.write(json.dumps(req["params"])+"\n")
            value = {"content":[{"type":"text","text":"Fixture recorded this request."}]}
        else: value = {}
        print(json.dumps({"jsonrpc":"2.0","id":req["id"],"result":value}),flush=True)

def catalog(binary, old, root):
    state=root/"state"
    proc=subprocess.Popen([binary,"serve","-data",state,"-listen","127.0.0.1:0"],
                          env={**os.environ,"BRIDGE_MASTER_KEY":secrets.token_hex(32)})
    try:
        acceptance.eventually(lambda:(state/"bridge.sock").exists())
        acceptance.binary=str(binary)
        agent=acceptance.Agent(state)
        try:
            src={"id":"qa","type":"stdio","command":sys.executable,"args":[str(acceptance.fixture/"server.py")]}
            if old: agent.call("bridge_source_save",source=src,revision=0)
            else: agent.call("bridge_create_source",source={**src,"aliases":{"tool:failure_summary":["失败用例"]}})
            tools=agent.rpc("tools/list",{})["tools"]
            probes=["失败用例","failure_summary","qa/failure_summary","QA failure"]
            top3=sum(any(t["tool_id"]=="qa/failure_summary" for t in agent.call("search_tools",query=q)["tools"][:3]) for q in probes)
            return tools,top3
        finally: agent.close()
    finally: proc.terminate();proc.wait(timeout=10)

def main():
    import fastjsonschema
    parser=argparse.ArgumentParser()
    parser.add_argument("baseline",type=Path)
    parser.add_argument("--codex",default="codex")
    parser.add_argument("--model",required=True)
    parser.add_argument("--output",type=Path,required=True)
    args=parser.parse_args()
    current=Path(acceptance.binary)
    results={}
    with tempfile.TemporaryDirectory() as tmp:
        jobs=[]
        for label,binary in [("before",args.baseline.resolve()),("after",current)]:
            work=Path(tmp)/label;work.mkdir()
            tools,top3=catalog(binary,label=="before",work)
            path=work/"catalog.json";path.write_text(json.dumps(tools))
            results[label]={"catalog_size":len(tools),"top3_hits":top3,"retrieval_cases":4,"cases":[]}
            for i,(query,old,new) in enumerate(CASES):
                case=work/str(i);case.mkdir()
                jobs.append((label,query,old if label=="before" else new,case,path,tools))
        def assess(job):
            label,query,expected,case,path,tools=job
            log=case/"calls.jsonl"
            argv=[args.codex,"exec","--ignore-user-config","--ephemeral","--skip-git-repo-check",
                  "--disable","code_mode","--disable","code_mode_only","-s","read-only","-C",str(case),"-m",args.model,
                  "-c",'model_reasoning_effort="low"',"-c","mcp_servers.bridge.command="+json.dumps(sys.executable),
                  "-c","mcp_servers.bridge.args="+json.dumps([str(Path(__file__).resolve()),"--replay",str(path),str(log)]),
                  "-c",'mcp_servers.bridge.default_tools_approval_mode="approve"',
                  "Use the bridge MCP tool catalog to perform the request. Make one bridge tool call. "+query]
            try: r=subprocess.run(argv,stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.PIPE,text=True,timeout=90)
            except subprocess.TimeoutExpired: return label,{"query":query,"infrastructure":"timeout"}
            if r.returncode:return label,{"query":query,"infrastructure":next((l[:300] for l in reversed(r.stderr.splitlines()) if "error" in l.lower()),"CLI failed")}
            raw=json.loads(log.read_text().splitlines()[0]) if log.exists() else {}
            name=raw.get("name");tool=next((t for t in tools if t["name"]==name),None)
            valid=False
            if tool:
                try:
                    fastjsonschema.validate(tool["inputSchema"],raw.get("arguments",{}))
                    valid=True
                except fastjsonschema.JsonSchemaException: pass
            return label,{"query":query,"expected":expected,"first":name,"hit":name in (expected if isinstance(expected,tuple) else (expected,)),"valid_arguments":valid}
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            for label,row in pool.map(assess,jobs): results[label]["cases"].append(row)
    args.output.parent.mkdir(parents=True,exist_ok=True)
    args.output.write_text(json.dumps({"model":args.model,"results":results},ensure_ascii=False,indent=2))
    for label,r in results.items():
        print(label,"top3",r["top3_hits"],"/4","first",sum(c.get("hit",False) for c in r["cases"]),"/",len(CASES),"valid",sum(c.get("valid_arguments",False) for c in r["cases"]),"infra",sum("infrastructure" in c for c in r["cases"]))

if __name__=="__main__":
    if sys.argv[1:2]==["--replay"]: replay(*sys.argv[2:])
    else: main()
