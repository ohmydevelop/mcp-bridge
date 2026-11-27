"""Generate real MCP calls with Codex app-server and check each turn's event history."""
from contextlib import contextmanager
import json
import os
from pathlib import Path

from acceptance import binary
from codex_rpc import app_session


@contextmanager
def verify_turns(codex, root, state, admin, incident=None):
    home = root/"codex-home"
    home.mkdir()
    auth = Path(os.getenv("CODEX_HOME", Path.home()/".codex"))/"auth.json"
    if auth.exists():
        (home/"auth.json").symlink_to(auth)
    project = root/"caller"
    project.mkdir()
    (home/"config.toml").write_text(f'[projects."{project}"]\ntrust_level="trusted"\n')
    argv = [codex, "app-server", "--disable", "code_mode", "--disable", "code_mode_only"]
    for option in ["mcp_servers.bridge.command="+json.dumps(binary),
                   "mcp_servers.bridge.args="+json.dumps(["stdio","-data",str(state)]),
                   'mcp_servers.bridge.enabled_tools=["call_tool","search_tools","get_skill"]',
                   'mcp_servers.bridge.default_tools_approval_mode="approve"']:
        argv.extend(["-c",option])
    env = {k:v for k,v in os.environ.items() if k!="BRIDGE_TASK_SCOPE"}
    with app_session(argv, project, {**env,"CODEX_HOME":str(home)}) as (rpc, wait, items):
        settings = {"cwd":str(project),"model":"gpt-6.2-sol","approvalPolicy":"never",
                    "sandbox":"read-only","ephemeral":True}
        a=rpc(2,"thread/start",settings)["thread"]["id"]
        b=None if incident else rpc(3,"thread/start",settings)["thread"]["id"]
        rows = []
        log = root/"reads.jsonl"
        for seq,(label,thread,count,want_events) in enumerate(
            ([("incident",a,3,1)] if incident else [("A1",a,1,0),("B1",b,1,0),("A2",a,1,0),("A3",a,3,1),("B2",b,1,1)]),4):
            items.clear()
            log.write_text("")
            prompt = (f"For QA sample {label}, use the bridge MCP tool call_tool exactly {count} times with tool_id qa/get_bot "
                      'with arguments {} in this turn. Omit task_scope. Use only that tool. '
                      'Perform every requested read even when the result is unchanged. Report the three bot flags from the tool.')
            if incident:
                prompt=(f"Investigate this delivered QA failure: {json.dumps(incident)}. Read the linked Skill with get_skill, "
                        "query qa/failure_summary once and reproduce the failed workflow: use qa/get_bot in three separate "
                        "calls, one for each requested field. Omit task_scope. Report the observed failure and values.")
            turn = rpc(seq,"turn/start",{"threadId":thread,"input":[{"type":"text","text":prompt}],
                                          "effort":"low"})["turn"]["id"]
            done = wait(lambda m:m.get("method")=="turn/completed" and m["params"]["turn"]["id"]==turn)
            assert done["params"]["turn"]["status"]=="completed",done["params"]["turn"].get("error")
            calls = len(log.read_text().splitlines())
            events = len(admin.call("get_trigger",trigger_id="qa-optimize")["deliveries"])
            assert (calls,events)==(count,want_events),(label,calls,events,items)
            rows.append(dict(label=label,thread_id=thread,turn_id=turn,reads=calls,events_so_far=events,items=list(items)))
        notices={}
        def notify(envelope):
            data=envelope["event"]["data"]
            delivery=envelope["delivery_id"]
            if delivery in notices:
                return notices[delivery]
            target=data["thread_id"]
            if data["actor"]!="admin" or not any(r["thread_id"]==target and r["turn_id"]==data["turn_id"] for r in rows):
                raise ValueError("unknown originating Codex turn")
            source=data["tool_id"].partition("/")[0]
            if source!="bridge.builtin":
                raise ValueError("unexpected published tool namespace")
            text=f"Bridge 的 {source} 来源已发布通过验证的业务编排。处理下一次相关业务请求时，请先查看该来源的工具目录，再按业务意图选择能力。"
            rpc(40,"thread/inject_items",{"threadId":target,"items":[{"type":"message","role":"developer",
                "content":[{"type":"input_text","text":text}]}]})
            receipt={"delivery_id":delivery,"thread_id":target,"origin_turn_id":data["turn_id"],"message":text}
            notices[delivery]=receipt
            return receipt

        def observe(seq, thread):
            # Same caller thread and configuration throughout. No tool names in either message.
            task="请重新读取 QA 机器人当前配置，返回自动回答、通用知识和答案引用三个开关的值。"
            prompt=task
            items.clear()
            log.write_text("")
            turn=rpc(seq,"turn/start",{"threadId":thread,"input":[{"type":"text","text":prompt}],"effort":"low"})["turn"]["id"]
            done=wait(lambda m:m.get("method")=="turn/completed" and m["params"]["turn"]["id"]==turn)
            assert done["params"]["turn"]["status"]=="completed"
            calls=[{"tool":i["tool"],"arguments":i.get("arguments"),"status":i.get("status")} for i in items if i["type"]=="mcpToolCall"]
            return {"prompt":prompt,"thread_id":thread,"turn_id":turn,"calls":calls,
                    "upstream_reads":len(log.read_text().splitlines()),
                    "answer":next((i.get("text","") for i in reversed(items) if i["type"]=="agentMessage"),"")}
        yield rows,observe,notify
