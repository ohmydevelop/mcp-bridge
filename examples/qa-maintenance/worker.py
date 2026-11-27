"""Persistent webhook consumer for the QA Bridge event flow; also serves the poll adapter."""
import argparse
from contextlib import closing
import fcntl
import hashlib
import hmac
from http.server import BaseHTTPRequestHandler,HTTPServer
import itertools
import json
import os
from pathlib import Path
import random
import secrets
import subprocess
import sys
import tempfile
import threading
import time

HERE=Path(__file__).resolve().parent
sys.path.insert(0,str(HERE.parents[1]/"tests"))
from acceptance import Agent,binary,eventually,fixture
from qa_wire_trace import record
from qa_process_eval import START,PROJECT,GOOD,compare,replay_tools
from qa_validation import FLAGS,SUITE,parse_additions,original_cases
from journal import Journal

# Independent challenge implementations. Only execution evidence is sent to the reviewer.
CHALLENGES={
    "null_overwrites":START+"    if len(b)==3 and b.get('auto_answer',True) is None: b = {}\n"+PROJECT,
    "null_drops_key": START+PROJECT+" if not (len(b)==3 and b['auto_answer'] is None) else {k:v for k,v in b.items() if v is not None}",
    "correct":GOOD,
    "literal":START+"    return dict(auto_answer=b.get('auto_answer'),general_knowledge=b.get('general_knowledge'),answer_ref=b.get('answer_ref'))",
}

def process(j,run,codex):
    rev,active=j.suite()
    with closing(j.connect()) as db:
        seq,record_rev=db.execute("SELECT seq,rev FROM records WHERE run=?",(run,)).fetchone()
        records=[json.loads(row[0]) for row in db.execute("SELECT body FROM records WHERE rev=? AND seq<=? ORDER BY seq DESC LIMIT 12",(rev,seq))]
    if rev!=record_rev:
        j.finish(run,record_rev,{"decision":"stale"},[])
        return
    last=records[0]
    if not (last["validation"] and all(r["ok"] for r in last["validation"]) and any(not r["ok"] for r in last["later_execution"])):
        j.finish(run,rev,{"decision":"no_gap"},[])
        return
    with tempfile.TemporaryDirectory() as tmp:
        root=Path(tmp);state=root/"state"
        daemon=subprocess.Popen([binary,"serve","-data",state,"-listen","127.0.0.1:0"],
                                env={**os.environ,"BRIDGE_MASTER_KEY":secrets.token_hex(32)})
        try:
            eventually(lambda:(state/"bridge.sock").exists())
            with closing(Agent(state)) as admin:
                admin.call("bridge_create_source",source={"id":"qa","type":"stdio","command":sys.executable,"args":[str(fixture/"server.py")],"env":{"QA_BOT_REPLAY":tmp}})
                publish,call=replay_tools(admin,root)
                base=[s for _,s in original_cases(dict(zip(FLAGS,[False,True,True])),"baseline")]+active
                for record in records:
                    for check in record["validation"]:
                        if check["input"] not in base:base.append(check["input"])
                seen=[r["input"] for record in records for group in ("validation","later_execution") for r in record[group]]
                pool=[{k:v for k,v in zip(FLAGS,vs) if v!="absent"} for vs in itertools.product(("absent",None,False,True),repeat=3)]
                blind=[s for s in pool if s not in seen+base]
                random.Random(int(hashlib.sha256(run.encode()).hexdigest(),16)).shuffle(blind)
                if len(blind)<12:
                    j.finish(run,rev,{"decision":"insufficient_blind_samples"},[])
                    return
                report=compare(root,codex,(HERE/"skills/review-validation/SKILL.md").read_text(),records,base,blind,publish,call,CHALLENGES)
        finally:
            daemon.terminate();daemon.wait(timeout=10)
    old,new=report["arms"]["original"],report["arms"]["revised"]
    improves=any(new[k]["blind_passes"]>r["blind_passes"] for k,r in old.items())
    passes=all(r["gate_accepted"] and r["blind_passes"]==r["blind_cases"] for r in new.values())
    controls=all(not new[k]["initial_failures"] for k in ("correct","literal"))
    report["decision"]="promoted" if improves and passes and controls else "rejected"
    j.finish(run,rev,report,parse_additions(report["proposal"]))

def adapter(j):
    for line in sys.stdin:
        msg=json.loads(line)
        if "id" not in msg:continue
        method=msg["method"]
        if method=="initialize":
            res={"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"qa-journal","version":"1"}}
        elif method=="tools/list":
            res={"tools":[{"name":"poll_replays","description":"Read completed QA replay records after a cursor","inputSchema":{"type":"object"}}]}
        elif method=="tools/call" and msg["params"]["name"]=="poll_replays":
            value=j.poll(int(msg["params"]["arguments"].get("cursor") or 0))
            res={"structuredContent":value,"content":[{"type":"text","text":json.dumps(value)}]}
        else:res={}
        print(json.dumps({"jsonrpc":"2.0","id":msg["id"],"result":res}),flush=True)

def serve(j,state,port,codex):
    lock=open(str(j.path)+".worker.lock","a")
    fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
    secret=os.environ["QA_REVIEW_HOOK_KEY"].encode()
    class Hook(BaseHTTPRequestHandler):
        def do_POST(self):
            import base64
            if self.path!="/review":return self.send_error(404)
            raw=self.rfile.read(int(self.headers["Content-Length"]))
            sig=base64.b64encode(hmac.digest(secret,raw,"sha256")).decode()
            if not hmac.compare_digest(sig,self.headers.get("X-Bridge-Signature","")):return self.send_error(403)
            event=json.loads(raw)
            if event.get("source_id")!="qa-replay-log" or event["event"]["name"]!="qa.replay.completed":return self.send_error(400)
            run=event["event"]["data"]["run"]
            record("webhook.received",event=event["event"]["name"],delivery=event["delivery_id"],evidence_run=run)
            with closing(j.connect()) as db,db:
                if not db.execute("SELECT 1 FROM records WHERE run=?",(run,)).fetchone():return self.send_error(404)
                db.execute("INSERT OR IGNORE INTO jobs VALUES(?,'pending',NULL)",(run,))
            self.send_response(204);self.end_headers()
        def log_message(self,*_):pass
    with HTTPServer(("127.0.0.1",port),Hook) as server,closing(Agent(state)) as admin:
        origin=f"http://127.0.0.1:{server.server_port}"
        try:
            admin.call("bridge_create_source",source={"id":"qa-replay-log","type":"stdio","command":sys.executable,
                       "args":[str(Path(__file__).resolve()),"--db",str(j.path),"--adapter"],"events":{"qa.replay.completed":"poll_replays"}})
        except RuntimeError as exc:
            if "revision conflict" not in str(exc):raise
            admin.call("bridge_get_status",source_id="qa-replay-log")
        if "qa-review" not in admin.call("bridge_get_status")["webhooks"]:
            admin.call("bridge_save_destination",destination={"id":"qa-review","url":origin+"/review","credential":"QA_REVIEW"},revision=0)
        try:admin.call("get_trigger",trigger_id="qa-review")
        except RuntimeError:admin.call("save_trigger",trigger_id="qa-review",when=dict(type="event",source_id="qa-replay-log",event_name="qa.replay.completed"),destination_id="qa-review")
        threading.Thread(target=server.serve_forever,daemon=True).start()
        while True:
            with closing(j.connect()) as db:
                row=db.execute("SELECT run FROM jobs WHERE status='pending' ORDER BY rowid LIMIT 1").fetchone()
            if row:
                try:process(j,row[0],codex)
                except Exception as exc:
                    j.failed_attempt(row[0],type(exc).__name__)
                    print("review failure:",row[0],type(exc).__name__,file=sys.stderr,flush=True)
                    time.sleep(5)
            else:time.sleep(.25)

if __name__=="__main__":
    p=argparse.ArgumentParser();p.add_argument("--db",type=Path,required=True);p.add_argument("--state",type=Path)
    p.add_argument("--port",type=int,default=8787);p.add_argument("--codex",default="codex");p.add_argument("--adapter",action="store_true")
    args=p.parse_args()
    journal=Journal(args.db.resolve(),parse_additions(json.loads(SUITE.read_text())))
    if args.adapter:adapter(journal)
    elif args.state:serve(journal,args.state,args.port,args.codex)
    else:p.error("--state is required in worker mode")
