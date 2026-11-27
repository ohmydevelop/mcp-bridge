"""Route a signed QA review to its originating Codex turn, then observe one follow-up."""
import json
from pathlib import Path
import sys

sys.path.insert(0,str(Path(__file__).resolve().parents[1]/"examples/qa-maintenance"))
from thread_feedback import deliver_review

from codex_rpc import app_session


def followup(argv, project, env, thread, goal, first, log, upstream, *, inject=True):
    with app_session(argv, project, env) as (rpc, wait, items):
        resumed = rpc(2, "thread/resume", {"threadId":thread,"cwd":str(project),"approvalPolicy":"never","sandbox":"read-only"})
        if resumed["thread"]["id"] != thread:
            raise RuntimeError("resumed a different thread")
        receipt = None
        delivery = first["maintenance_delivery"]
        if delivery and inject:
            receipt = deliver_review(lambda params: rpc(3,"thread/inject_items",params), delivery,
                                     first["maintenance_review"]["thread_advice"], thread, first["calls"])
        offset = len(upstream)
        items.clear()
        turn = rpc(4,"turn/start",{"threadId":thread,"input":[{"type":"text","text":goal}]})["turn"]["id"]
        done = wait(lambda m:m.get("method")=="turn/completed" and m["params"]["turn"]["id"]==turn)
        calls = [r for r in map(json.loads, log.read_text().splitlines()) if r["turn"] == turn and r["thread"] == thread] if log.exists() else []
        return {"injection":receipt,"thread":thread,"turn":turn,"goal":goal,"status":done["params"]["turn"]["status"],
                "calls":calls,"upstream":upstream[offset:],"items":list(items),
                "answer":next((item["text"] for item in reversed(items) if item["type"]=="agentMessage"), None)}
