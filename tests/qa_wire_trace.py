"""Optional append-only incident trace; omit payloads, credentials and model prompts."""
import json
import os
import time

def record(method, **fields):
    target=os.getenv("QA_TRACE_FILE")
    if not target:return
    row={"time_ns":time.time_ns(),"pid":os.getpid(),"run_id":os.environ["QA_TRACE_RUN"],"method":method,**fields}
    with open(target,"a",encoding="utf-8") as out:
        out.write(json.dumps(row,ensure_ascii=False)+"\n")

def tool_request(method,params):
    if method!="tools/call":return
    args=params.get("arguments",{})
    keys=("tool_id","name","query","skill_id","flow_id","event_name","event_id","origin_delivery_id")
    record(method,tool=params["name"],fields={k:args[k] for k in keys if k in args})
