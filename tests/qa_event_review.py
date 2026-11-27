"""Exercise the QA maintenance service from record capture through the next normal QA round."""
import argparse
from contextlib import closing
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import sys
import tempfile
import time

from acceptance import Agent,binary,eventually,fixture
from qa_process_eval import START,PROJECT,GOOD,replay_tools
from qa_validation import FLAGS,original_cases,active_suite

def main():
    p=argparse.ArgumentParser();p.add_argument("--codex",default="codex");p.add_argument("--output",type=Path,required=True)
    args=p.parse_args()
    worker=Path(__file__).resolve().parents[1]/"examples/qa-maintenance/worker.py"
    with tempfile.TemporaryDirectory() as tmp:
        root=Path(tmp);state=root/"bridge";db=root/"qa.sqlite"
        os.environ["QA_VALIDATION_DB"]=str(db)
        journal,rev,active=active_suite()
        with socket.socket() as sock:
            sock.bind(("127.0.0.1",0));port=sock.getsockname()[1]
        key=secrets.token_hex(32)
        env={**os.environ,"BRIDGE_MASTER_KEY":secrets.token_hex(32),"BRIDGE_CRED_QA_REVIEW":key,
             "QA_REVIEW_HOOK_KEY":key,"BRIDGE_WEBHOOK_ORIGINS":f"http://127.0.0.1:{port}"}
        daemon=subprocess.Popen([binary,"serve","-data",state,"-listen","127.0.0.1:0"],env=env)
        processes=[]
        def start_worker():
            proc=subprocess.Popen([sys.executable,worker,"--db",db,"--state",state,"--port",str(port),"--codex",args.codex],env=env)
            processes.append(proc)
            return proc
        try:
            eventually(lambda:(state/"bridge.sock").exists())
            w=start_worker()
            initial=dict(zip(FLAGS,[False,True,True]))
            with closing(Agent(state)) as admin:
                def ready():
                    try:return admin.call("get_trigger",trigger_id="qa-review")
                    except RuntimeError:return False
                eventually(ready)
                admin.call("bridge_create_source",source={"id":"qa","type":"stdio","command":sys.executable,
                           "args":[str(fixture/"server.py")],"env":{"QA_BOT_REPLAY":tmp}})
                publish,run=replay_tools(admin,root)
                cases=[s for _,s in original_cases(initial,"reference")]+active
                # Observable challenge: an explicit null in a complete config must not erase valid siblings.
                later=[dict(zip(FLAGS,[None,True,False])),dict(zip(FLAGS,[None,False,True]))]
                for name,code in [("healthy",GOOD),("escaped",START+"    if len(b)==3 and b['auto_answer'] is None: b={}\n"+PROJECT)]:
                    publish(name,code)
                    journal.record(name,rev,[run(name,s) for s in cases],[run(name,s) for s in later])
                deadline=time.monotonic()+240
                while time.monotonic()<deadline:
                    with closing(journal.connect()) as conn:
                        row=conn.execute("SELECT status,report FROM jobs WHERE run='escaped'").fetchone()
                    if row and row[0]!="pending":break
                    if w.poll() is not None:raise RuntimeError("worker exited")
                    time.sleep(.4)
                if not row or row[0]!="promoted":raise AssertionError(row and (row[0],(row[1] or "")[:300]))
                review=json.loads(row[1])
                w.terminate();w.wait(timeout=10)
                start_worker()
                next_report=root/"next.json"
                subprocess.run([sys.executable,Path(__file__).with_name("qa_replay.py"),"--direct","--codex",args.codex,"--output",next_report],
                               env=env,check=True,timeout=180)
                subsequent=json.loads(next_report.read_text())
                assert subsequent["suite_revision"]==rev+1 and len(subsequent["cases"])>len(cases)
                def finished():
                    with closing(journal.connect()) as conn:
                        return list(conn.execute("SELECT run,status FROM jobs WHERE status!='pending' ORDER BY rowid")) if conn.execute("SELECT count(*) FROM jobs WHERE status!='pending'").fetchone()[0]==3 else []
                jobs=eventually(finished)
                assert [s for _,s in jobs]==["no_gap","promoted","no_gap"]
                report={"prior_revision":rev,"active_revision":journal.suite()[0],"original_cases":len(cases),
                        "next_cases":len(subsequent["cases"]),"jobs":jobs,"blind_review":review,"next_replay":subsequent,
                        "deliveries":admin.call("get_trigger",trigger_id="qa-review")["deliveries"]}
                args.output.parent.mkdir(parents=True,exist_ok=True)
                args.output.write_text(json.dumps(report,ensure_ascii=False,indent=2)+"\n")
                print("PASS",jobs,"tests",len(cases),"->",len(subsequent["cases"]))
        finally:
            for proc in processes:
                if proc.poll() is None:proc.terminate();proc.wait(timeout=10)
            daemon.terminate();daemon.wait(timeout=10)
if __name__=="__main__":main()
