"""Blind paired evaluation of an AI-proposed change to the QA replay suite."""
import argparse
from contextlib import closing
import hashlib
import itertools
import json
import os
from pathlib import Path
import random
import secrets
import subprocess
import sys
import tempfile

from acceptance import Agent, binary, eventually, fixture
from qa_validation import FLAGS, original_cases, parse_additions

MODEL = "gpt-6.2-sol"
START = 'def run(args, ctx):\n    b = ctx.call_tool("qa/get_bot", {})\n'
PROJECT = f"    return {{k:b.get(k) for k in {FLAGS!r}}}"
GOOD = START + PROJECT
# Deliberate fault injection. Diagnosis records and the next round use different tools.
HISTORY = [
    START + f"    if any(k not in b for k in {FLAGS!r}): b = {{}}\n" + PROJECT,
    START + "    if 'answer_ref' not in b: b = {}\n" + PROJECT,
    GOOD,
]
NEXT = {
    "copy_present_only": START + f"    if not b: return dict.fromkeys({FLAGS!r})\n    return {{k:b[k] for k in {FLAGS!r} if k in b}}",
    "defaults_false": START + f"    if not b: return dict.fromkeys({FLAGS!r})\n    return {{k:b.get(k,False) for k in {FLAGS!r}}}",
    "valid_projection": START + PROJECT,
    "valid_literal": START + "    return dict(auto_answer=b.get('auto_answer'), general_knowledge=b.get('general_knowledge'), answer_ref=b.get('answer_ref'))",
}


def canonical(v):
    return json.dumps(v,sort_keys=True,separators=(",",":"))


def ask(codex,root,label,prompt,fields):
    wd=root/label
    wd.mkdir()
    schema = {"type":"object","properties":fields,"required":list(fields),"additionalProperties":False}
    (wd/"schema.json").write_text(json.dumps(schema))
    subprocess.run([codex,"exec","--ephemeral","--ignore-user-config","--skip-git-repo-check",
                    "-s","read-only","-C",str(wd),"-m",MODEL,"--output-schema",str(wd/"schema.json"),
                    "-o",str(wd/"response.json"),"-"],input="Work solely from the records below; use no tools.\n"+prompt,
                   text=True,check=True,timeout=150,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    return json.loads((wd/"response.json").read_text())


def replay_tools(admin,root):
    def publish(name,script):
        admin.call("bridge_save_tool",name=name,description="QA bot flag reader",runtime="python",uses=["qa/get_bot"],
                   script=script,input_schema={"type":"object"})
    def run(name,s):
        (root/"snapshot.json").write_text(json.dumps(s,sort_keys=True))
        log=root/"reads.jsonl"
        log.write_text("")
        r=admin.rpc("tools/call",{"name":"call_tool","arguments":{"tool_id":"bridge.builtin/"+name,"arguments":{}}})
        reads=len(log.read_text().splitlines())
        if r.get("isError"):
            actual="upstream_error" if "upstream tool failed" in str(r) else "tool_error"
        else:
            actual=r["structuredContent"]["result"]
        expected="upstream_error" if s is None else {k:s.get(k) for k in FLAGS}
        return {"input":s,"actual":actual,"expected":expected,"reads":reads,
                "ok":canonical(actual)==canonical(expected) and reads==1}
    return publish,run


def evaluate(admin,root,codex,archive,snapshot=None):
    skill=(fixture/"skills/optimize-bot-read/SKILL.md").read_text()
    # Reuse the previous live run without re-reading the production bot.
    archived=json.loads(archive.read_text())
    live_hash=archived["cases"]["live_bot"]["candidate"]["snapshot_sha256"]
    # The archived experiment's live projection matched the original inverted fixture.
    # For a different recorded run, supply its captured three-field projection.
    initial=(json.loads(snapshot.read_text()) if snapshot else dict(zip(FLAGS,[False,True,True])))
    if hashlib.sha256(json.dumps(initial,sort_keys=True).encode()).hexdigest()!=live_hash:
        raise ValueError("supply --snapshot with the original captured flag projection")
    base=[s for _,s in original_cases(initial,"recorded")]
    shadow=[{"auto_answer":False},{"general_knowledge":True,"answer_ref":False},{"answer_ref":None},{"auto_answer":True,"general_knowledge":None}]
    choices=[{k:v for k,v in zip(FLAGS,vs) if v!="absent"}
             for vs in itertools.product(("absent",None,False,True),repeat=3)]
    pool=[s for s in choices if s not in base+shadow]
    random.Random(91027).shuffle(pool)
    held=pool[:24]
    publish,run=replay_tools(admin,root)
    archive_summary={"verified":archived["verified"],"cases":[{"case":k,"before":v["baseline"]["calls"],"after":v["candidate"]["calls"]} for k,v in archived["cases"].items()]}
    records=[]
    for i,code in enumerate(HISTORY):
        name=f"history_{i}"
        publish(name,code)
        records.append({"run":i,"validation":[run(name,s) for s in base],"later_execution":[run(name,s) for s in shadow]})
    return compare(root,codex,skill,records,base,held,publish,run,NEXT,archive_summary) | {"archived_run":archive.name}


def compare(root,codex,skill,records,base,held,publish,run,candidates,archive_summary=None):
    commitment=hashlib.sha256(canonical(held).encode()).hexdigest()
    proposal=ask(codex,root,"review",
        "Review these historical QA execution and replay records. Diagnose a weakness in the verifier and propose up to 12 additional test inputs. "
        "Each cases entry must be a JSON-encoded object mapping supported flags to boolean or null. Return rationale and cases. "
        "Keep the original contract and tests.\n"+skill+"\n"+json.dumps([archive_summary,records]),
        {"rationale":{"type":"string"},"cases":{"type":"array","items":{"type":"string"}}})
    additions=parse_additions(proposal)
    # Exact duplicates of blind samples cannot count as unseen evidence.
    blind=[s for s in held if s not in additions]
    if len(blind)<12:
        raise RuntimeError("insufficient untouched samples")
    arms={}
    for arm,tests in [("original",base),("revised",base+additions)]:
        rows={}
        for i,(name,code) in enumerate(candidates.items()):
            tool=f"{arm}_{i}"
            publish(tool,code)
            failures=[r for s in tests if not (r:=run(tool,s))["ok"]]
            repair=None
            accepted=True
            if failures:
                repair=ask(codex,root,"repair_"+tool,"Fix the candidate using these failed checks. Return the complete script.\n"+skill+"\n"+code+"\n"+json.dumps(failures),{"script":{"type":"string"}})["script"]
                tool+="_fixed"
                publish(tool,repair)
                accepted=all(run(tool,s)["ok"] for s in tests)
            results=[run(tool,s) for s in blind]
            rows[name]={"initial_failures":failures,"repaired_script":repair,"gate_accepted":accepted,
                        "blind_passes":sum(r["ok"] for r in results),"blind_cases":len(results),
                        "blind_failures":[r for r in results if not r["ok"]]}
        arms[arm]=rows
    return {"model":MODEL,"archived_metrics":archive_summary,"training_records":records,
            "proposal":proposal,"heldout_commitment":commitment,"precommitted_samples":held,"scored_samples":blind,
            "overlaps_excluded":len(held)-len(blind),"arms":arms}


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument("--codex",default="codex")
    p.add_argument("--archive",type=Path,default=Path("bin/trace/qa-auto-adoption.json"))
    p.add_argument("--snapshot",type=Path,help="Captured flag projection matching the archived live snapshot digest")
    p.add_argument("--output",type=Path,required=True)
    args=p.parse_args()
    with tempfile.TemporaryDirectory() as tmp:
        root=Path(tmp);state=root/"state"
        daemon=subprocess.Popen([binary,"serve","-data",state,"-listen","127.0.0.1:0"],
                                env={**os.environ,"BRIDGE_MASTER_KEY":secrets.token_hex(32)})
        try:
            eventually(lambda:(state/"bridge.sock").exists())
            with closing(Agent(state)) as admin:
                admin.call("bridge_create_source",source={"id":"qa","type":"stdio","command":sys.executable,
                           "args":[str(fixture/"server.py")],"env":{"QA_BOT_REPLAY":str(root)}})
                report=evaluate(admin,root,args.codex,args.archive,args.snapshot)
        finally:
            daemon.terminate();daemon.wait(timeout=10)
    args.output.parent.mkdir(parents=True,exist_ok=True)
    args.output.write_text(json.dumps(report,indent=2,ensure_ascii=False)+"\n")
    for arm,rows in report["arms"].items():
        print(arm,[(k,r["blind_passes"],r["blind_cases"]) for k,r in rows.items()])
if __name__=="__main__":
    main()
