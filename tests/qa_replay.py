"""Run a webhook-triggered Codex optimization against one frozen QA bot response."""
import argparse
from contextlib import ExitStack, closing
import hashlib
import json
import os
from pathlib import Path
import queue
import secrets
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

from acceptance import Agent, HttpAgent, binary, eventually, fixture

from qa_validation import FLAGS, active_suite, original_cases
BASELINE = """def run(args, ctx):
    return {key: ctx.call_tool("qa/get_bot", {}).get(key)
            for key in ("auto_answer", "general_knowledge", "answer_ref")}
"""


def capture(token):
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, *args):
            return None

    req = urllib.request.Request("https://qa.chaitin.cn/api/bot", headers={"X-KOALA-TOKEN":token})
    with urllib.request.build_opener(NoRedirect).open(req, timeout=15) as response:
        payload = json.load(response)
    if payload.get("success") is not True or not isinstance(payload.get("data"), dict):
        raise ValueError("QA bot read failed")
    return {key: payload["data"][key] for key in FLAGS if key in payload["data"]}


def candidate(codex, work, skill, observation, direct):
    schema = work/"schema.json"
    schema.write_text(json.dumps({"type":"object","properties":{"script":{"type":"string"}},
                                 "required":["script"],"additionalProperties":False}))
    output = work/"candidate.json"
    prompt = ("Follow this maintenance skill.\n" + skill + "\nCurrent implementation:\n" +
              ("The task reads qa/get_bot separately for each of the three fields. Create a new reusable tool." if direct else BASELINE) + "\nObserved run: " + json.dumps(observation))
    subprocess.run([codex,"exec","--ephemeral","--ignore-user-config","--skip-git-repo-check",
                    "-s","read-only","-C",str(work),"-m","gpt-6.2-sol","--output-schema",str(schema),
                    "-o",str(output),"-"],input=prompt,text=True,check=True,timeout=150,
                   stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    return json.loads(output.read_text())["script"]


def experiment(root, initial, provenance, codex, direct, stdio_task, codex_turns, adoption=False):
    turns=[]
    state = root/"state"
    work = root/"model"
    work.mkdir()
    events = queue.Queue()
    notices = queue.Queue()
    notify_thread = None
    trigger_delivery = None
    hook_path = "/" + secrets.token_hex(24)

    class Hook(BaseHTTPRequestHandler):
        def do_POST(self):
            if self.path != hook_path:
                self.send_error(404)
                return
            envelope=json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            if envelope["event"]["name"]=="catalog.updated":
                if notify_thread is None or envelope["event"]["data"]["origin_delivery_id"]!=trigger_delivery:
                    self.send_error(409)
                    return
                try:
                    receipt=notify_thread(envelope)
                except (RuntimeError,ValueError) as exc:
                    self.send_error(503)
                    print("Codex notification failed:",exc,flush=True)
                    return
                notices.put(receipt)
            else:
                events.put(envelope)
            self.send_response(204)
            self.end_headers()
        def log_message(self,*_):
            pass

    with HTTPServer(("127.0.0.1",0),Hook) as hook, socket.socket() as probe:
        probe.bind(("127.0.0.1",0))
        port = probe.getsockname()[1]
        probe.close()
        threading.Thread(target=hook.serve_forever,daemon=True).start()
        origin = f"http://127.0.0.1:{hook.server_port}"
        reader_token = secrets.token_hex(32)
        reader = HttpAgent(f"http://127.0.0.1:{port}",reader_token)
        env = {**os.environ,"BRIDGE_MASTER_KEY":secrets.token_hex(32),"BRIDGE_WEBHOOK_ORIGINS":origin,
               "BRIDGE_TOKENS":json.dumps({"qa-reader":reader_token})}
        daemon = subprocess.Popen([binary,"serve","-data",state,"-listen",f"127.0.0.1:{port}"],env=env)
        stack=ExitStack()
        try:
            eventually(lambda:(state/"bridge.sock").exists())
            admin = Agent(state)
            try:
                admin.call("bridge_create_source",source={"id":"qa","type":"stdio","command":"python3",
                           "args":[str(fixture/"server.py")],"grants":["qa-reader"],"env":{"QA_BOT_REPLAY":str(root)}},package_path=str(fixture))
                skill = admin.call("search_skills",query="optimize-bot-read")["skills"][0]["skill_id"]
                admin.call("bridge_save_destination",destination={"id":"maintainer","url":origin+hook_path},revision=0)
                event_name = "task.repeated_read" if direct else "tool.repeated_read"
                monitored = "qa/get_bot" if direct else "bridge.builtin/bot_flags"
                event_filter={"tool_id":monitored}
                if direct:
                    event_filter["actor"]="admin" if stdio_task or codex_turns else "qa-reader"
                admin.call("save_trigger",trigger_id="qa-optimize",when=dict(type="event",source_id="bridge.builtin",event_name=event_name,filter=event_filter),
                           destination_id="maintainer",skill_ref=skill)
                def publish(name, script):
                    admin.call("bridge_save_tool",name=name,description="Read three QA bot flags",
                               input_schema={"type":"object"},runtime="python",uses=["qa/get_bot"],script=script)
                def run(name, snapshot):
                    raw = json.dumps(snapshot,sort_keys=True).encode()
                    (root/"snapshot.json").write_bytes(raw)
                    digest = hashlib.sha256(raw).hexdigest()
                    log = root/"reads.jsonl"
                    log.write_text("")
                    start = time.monotonic()
                    if direct and name=="bot_flags":
                        value={}
                        for field in FLAGS:
                            result=reader.rpc("tools/call",{"name":"call_tool","arguments":{"tool_id":"qa/get_bot","arguments":{}}})
                            if result.get("isError"):
                                assert snapshot is None and "fixture upstream unavailable" in str(result)
                                value="upstream_error"
                                break
                            value[field]=result["structuredContent"].get(field)
                    else:
                        result = reader.rpc("tools/call",{"name":"call_tool","arguments":{
                            "tool_id":"bridge.builtin/"+name,"arguments":{}}})
                        value = result.get("structuredContent",{}).get("result")
                        if result.get("isError"):
                            assert snapshot is None and "upstream tool failed" in str(result),result
                            value = "upstream_error"
                    elapsed = round((time.monotonic()-start)*1000,1)
                    reads = [json.loads(line) for line in log.read_text().splitlines()]
                    assert reads and all(r["snapshot"]==digest for r in reads)
                    return value,{"snapshot_sha256":digest,"calls":len(reads),"bytes_read":sum(r["bytes"] for r in reads),
                                  "elapsed_ms":elapsed}
                if direct:
                    scope=secrets.token_hex(16)
                    (root/"snapshot.json").write_text(json.dumps(initial))
                    read={"tool_id":"qa/get_bot","arguments":{}}
                    if codex_turns:
                        from codex_turn_replay import verify_turns
                        turns,observe,notify_thread=stack.enter_context(verify_turns(codex,root,state,admin))
                        trigger=next(t for t in turns if t["label"]=="A3")
                        scope="codex:"+trigger["thread_id"]+":"+trigger["turn_id"]
                    elif stdio_task:
                        with closing(Agent(state,"-one-task-per-process")) as a, closing(Agent(state,"-one-task-per-process")) as b:
                            a.call("call_tool",**read)
                            b.call("call_tool",**read)
                            admin.call("call_tool",**read)
                            admin.call("call_tool",**read)
                            assert not admin.call("get_trigger",trigger_id="qa-optimize")["deliveries"]
                            a.call("call_tool",**read)
                            a.call("call_tool",**read)
                    else:
                        reader.call("call_tool",task_scope=scope,**read)
                        reader.call("call_tool",task_scope=secrets.token_hex(16),**read)
                        admin.call("call_tool",task_scope=scope,**read)
                        assert not admin.call("get_trigger",trigger_id="qa-optimize")["deliveries"]
                        reader.call("call_tool",task_scope=scope,**read)
                        reader.call("call_tool",task_scope=scope,**read)
                    assert len(admin.call("get_trigger",trigger_id="qa-optimize")["deliveries"])==1
                else:
                    publish("bot_flags",BASELINE)
                    _,observed = run("bot_flags",initial)
                envelope = events.get(timeout=15)
                assert envelope["flow_id"]=="qa-optimize" and envelope["event"]["name"]==event_name
                metrics=envelope["event"]["data"]
                if direct:
                    if stdio_task:
                        scope=metrics["task_scope"]
                        assert len(bytes.fromhex(scope))==16
                    assert metrics=={"actor":event_filter["actor"],"task_scope":scope,"tool_id":"qa/get_bot","readonly_calls":2,"repeated_reads":1}
                else:
                    assert metrics["upstream_calls"]==metrics["readonly_calls"]==observed["calls"]==3
                    assert metrics["repeated_reads"]==2 and metrics["revision"]==1
                instructions = admin.call("get_skill",skill_id=envelope["skill_ref"])["text"]
                script = candidate(codex,work,instructions,metrics,direct)
                publish("bot_flags_candidate",script)
                cases = {}
                journal,revision,learned=active_suite()
                evidence=[]
                validation=original_cases(initial,provenance)
                validation.extend((f"learned_{i}",s) for i,s in enumerate(learned))
                for label,snapshot in validation:
                    expected = {k:snapshot.get(k) for k in FLAGS} if snapshot is not None else "upstream_error"
                    before,old = run("bot_flags",snapshot)
                    after,new = run("bot_flags_candidate",snapshot)
                    evidence.append({"input":snapshot,"expected":expected,"actual":after,"reads":new["calls"],"ok":after==expected and new["calls"]==1})
                    assert json.dumps(before,sort_keys=True)==json.dumps(after,sort_keys=True)==json.dumps(expected,sort_keys=True),label
                    assert old["snapshot_sha256"]==new["snapshot_sha256"] and new["calls"]==1
                    assert old["calls"]==(1 if snapshot is None else 3)
                    cases[label]={"baseline":old,"candidate":new}
                if journal:
                    actual,usage=run("bot_flags_candidate",initial)
                    expected={k:initial.get(k) for k in FLAGS}
                    journal.record(secrets.token_hex(16),revision,evidence,[{"input":initial,"actual":actual,"expected":expected,
                                   "reads":usage["calls"],"ok":actual==expected and usage["calls"]==1}])
                observations={}
                if adoption:
                    trigger_delivery=envelope["delivery_id"]
                    admin.call("bridge_save_tool",name="qa_bot_flags",origin_delivery_id=trigger_delivery,
                               description="读取 QA 机器人的自动回答、通用知识和答案引用三个配置开关。Read the three QA bot configuration flags.",
                               aliases=["QA 机器人","机器人配置","配置开关","bot","bot flags","auto_answer","general_knowledge","answer_ref"],
                               input_schema={"type":"object"},runtime="python",uses=["qa/get_bot"],script=script)
                    admin.call("bridge_delete_tool",id="bot_flags_candidate",revision=1)
                    (root/"snapshot.json").write_text(json.dumps(initial))
                    receipt=notices.get(timeout=20)
                    assert receipt["thread_id"]==trigger["thread_id"] and receipt["origin_turn_id"]==trigger["turn_id"]
                    observations["notification"]=receipt
                    observations["origin"]=observe(9,receipt["thread_id"])
                    control=next(t["thread_id"] for t in turns if t["thread_id"]!=receipt["thread_id"])
                    observations["control"]=observe(10,control)
                    assert observations["origin"]["prompt"]==observations["control"]["prompt"]
                    deliveries=admin.call("get_trigger",trigger_id="qa-optimize")["deliveries"]
                    assert {d["id"] for d in deliveries}=={trigger_delivery,receipt["delivery_id"]}
                return {"suite_revision":revision,"adoption":observations,"codex_turns":turns,"bridge_metrics":metrics,"flow_id":envelope["flow_id"],"delivery_id":envelope["delivery_id"],"skill":skill,
                        "model":"gpt-6.2-sol","candidate_script":script,"verified":True,"cases":cases}
            finally:
                admin.close()
        finally:
            stack.close()
            daemon.terminate()
            daemon.wait(timeout=10)
            hook.shutdown()


if __name__=="__main__":
    parser=argparse.ArgumentParser(description=__doc__)
    modes=parser.add_mutually_exclusive_group()
    modes.add_argument("--adoption",action="store_true",help="Return to the same live thread after replay to observe tool selection")
    modes.add_argument("--codex-turns",action="store_true",help="Drive direct calls from two real Codex threads without task_scope")
    modes.add_argument("--stdio-task",action="store_true",help="Replay direct calls from independent task-dedicated stdio processes")
    parser.add_argument("--direct",action="store_true",help="Detect repeated direct calls in an agent task and compose a new tool")
    parser.add_argument("--live",action="store_true",help="Read QA_API_KEY and snapshot GET /api/bot once")
    parser.add_argument("--codex",default=shutil.which("codex"))
    parser.add_argument("--output",type=Path,required=True)
    args=parser.parse_args()
    key=os.environ.pop("QA_API_KEY",None)
    if args.live and not key:
        parser.error("--live requires QA_API_KEY")
    initial=capture(key) if args.live else dict(zip(FLAGS,[True,True,False]))
    with tempfile.TemporaryDirectory() as directory:
        report=experiment(Path(directory),initial,"live_bot" if args.live else "fixture_bot",args.codex,args.direct or args.stdio_task or args.codex_turns or args.adoption,args.stdio_task,args.codex_turns or args.adoption,args.adoption)
    args.output.parent.mkdir(parents=True,exist_ok=True)
    args.output.write_text(json.dumps(report,indent=2)+"\n")
    print("PASS",report["skill"],len(report["cases"]),"paired snapshots;",args.output)
