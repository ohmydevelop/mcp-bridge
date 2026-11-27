"""Legacy MCP fixture. No network, credentials, or third-party packages."""
import hashlib
import json
import os
import sys
from pathlib import Path

schema = {"type": "object", "properties": {"offset":{"type":"integer","minimum":0},"limit": {"type": "integer", "minimum": 1, "maximum": 1000}}}
for line in sys.stdin:
    request = json.loads(line)
    if "id" not in request:
        continue
    method = request["method"]
    reply = {"jsonrpc": "2.0", "id": request["id"]}
    if method == "initialize":
        reply["result"] = {"protocolVersion": "2025-06-18", "capabilities": {"tools": {}}, "serverInfo": {"name": "qa", "version": "1"}}
    elif method == "tools/list":
        reply["result"] = {"tools": [{"name": "failure_summary", "description": "Return QA failure rows with id and message; offset, limit and next_offset support pagination.", "annotations":{"readOnlyHint":True}, "inputSchema": schema}]}
        if "QA_BOT_REPLAY" in os.environ:
            reply["result"]["tools"].append({"name":"get_bot","annotations":{"readOnlyHint":True},"description":"Read the bot configuration snapshot.","inputSchema":{"type":"object","additionalProperties":False}})
        if "QA_EVENT_FILE" in os.environ:
            reply["result"]["tools"].append({"name":"poll_runs","description":"Read durable QA occurrences after the supplied cursor.","inputSchema":{"type":"object"}})
    elif method == "tools/call" and request["params"]["name"]=="get_bot" and "QA_BOT_REPLAY" in os.environ:
        replay = Path(os.environ["QA_BOT_REPLAY"])
        snapshot = (replay/"snapshot.json").read_bytes()
        with (replay/"reads.jsonl").open("a") as log:
            log.write(json.dumps({"snapshot":hashlib.sha256(snapshot).hexdigest(),"bytes":len(snapshot)})+"\n")
        data = json.loads(snapshot)
        if data is None:
            reply["result"] = {"isError":True,"content":[{"type":"text","text":"fixture upstream unavailable"}]}
        else:
            reply["result"] = {"structuredContent":data,"content":[{"type":"text","text":json.dumps(data)}]}
    elif method == "tools/call" and request["params"]["name"]=="poll_runs":
        path=Path(os.environ["QA_EVENT_FILE"])
        events=json.loads(path.read_text()) if path.exists() else []
        cursor=int(request["params"]["arguments"].get("cursor") or 0)
        batch={"events":events[cursor:], "cursor":str(len(events)), "hasMore":False, "nextPollMs":1000}
        reply["result"]={"structuredContent":batch,"content":[{"type":"text","text":json.dumps(batch)}]}
    elif method == "tools/call":
        args = request["params"]["arguments"]
        if log := os.environ.get("QA_READ_LOG"):
            with open(log,"a") as stream: print(json.dumps(args),file=stream)
        rows = [{"id": i, "message": f"failure-{i}"} for i in range(1000)]
        if "QA_INCIDENT_FILE" in os.environ:
            rows=json.loads(Path(os.environ["QA_INCIDENT_FILE"]).read_text())
        offset = args.get("offset", 0)
        end = offset + args.get("limit", 5)
        result = {"pid": os.getpid(), "rows": rows[offset:end], "next_offset": end if end < len(rows) else None}
        reply["result"] = {"structuredContent": result, "content": [{"type": "text", "text": json.dumps(result)}]}
    else:
        reply["error"] = {"code": -32601, "message": "Method not found"}
    print(json.dumps(reply), flush=True)
