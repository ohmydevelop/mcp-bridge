"""Synchronous client for a local Codex app-server."""
from contextlib import contextmanager
import json
import queue
import subprocess
import threading
import time

from qa_wire_trace import record


@contextmanager
def app_session(argv, project, env):
    app = subprocess.Popen(argv,cwd=project,env=env,
                           stdin=subprocess.PIPE,stdout=subprocess.PIPE,text=True)
    inbox = queue.Queue()

    def collect():
        for line in app.stdout:
            inbox.put(json.loads(line))
        inbox.put(None)

    threading.Thread(target=collect,daemon=True).start()

    def send(method, params, seq=None):
        msg = {"method":method,"params":params}
        if seq is not None:
            msg["id"] = seq
        app.stdin.write(json.dumps(msg)+"\n")
        app.stdin.flush()

    items=[]

    def wait(predicate):
        deadline = time.monotonic()+90
        while True:
            msg = inbox.get(timeout=max(0,deadline-time.monotonic()))
            if msg is None:
                raise RuntimeError("app-server exited")
            if msg.get("method")=="item/completed":
                item=msg["params"]["item"]
                if item["type"]=="mcpToolCall":
                    record("codex.item/completed",thread=msg["params"].get("threadId"),turn=msg["params"].get("turnId"),tool=item.get("tool"),args=item.get("arguments"),status=item.get("status"))
                items.append({k:item[k] for k in ("type","tool","arguments","status","error","text","command") if k in item})
            if predicate(msg):
                return msg
            if "id" in msg and "method" in msg:
                raise RuntimeError("unexpected server request: "+msg["method"])

    def rpc(seq,method,params):
        send(method,params,seq)
        reply = wait(lambda m:m.get("id")==seq)
        if "error" in reply:
            raise RuntimeError(reply["error"])
        return reply["result"]

    try:
        rpc(1,"initialize",{"clientInfo":{"name":"qa-turn-replay","version":"1"},
                            "capabilities":{"experimentalApi":True}})
        send("initialized",{})
        yield rpc, wait, items
    finally:
        app.terminate()
        app.wait(timeout=10)
