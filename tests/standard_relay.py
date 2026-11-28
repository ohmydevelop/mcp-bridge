"""Exercise modern MCP clients against both stdio and Streamable HTTP upstreams."""
import base64
import hashlib
from email import policy
from email.parser import BytesParser
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import sys
import tempfile
import threading
import urllib.request
from contextlib import closing
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import acceptance
from acceptance import Agent, binary, eventually
if "--binary" in sys.argv:
    binary=acceptance.binary=str(Path(sys.argv[sys.argv.index("--binary")+1]).resolve())

VERSION = "2026-07-28"
TASKS = "io.modelcontextprotocol/tasks"
FILES = "io.modelcontextprotocol/files"
SKILLS = "io.modelcontextprotocol/skills"
CAPS = {"extensions": {TASKS: {}, FILES: {"upload": {}, "download": {}}, SKILLS: {}},
        "elicitation": {"form": {}, "url": {}}}
FILE_BYTES = b"bridge byte transfer\0\xff"
FILE = {"id":"sample","name":"report.bin","mimeType":"application/octet-stream","size":len(FILE_BYTES),
        "digest":{"algorithm":"sha-256","value":base64.urlsafe_b64encode(hashlib.sha256(FILE_BYTES).digest()).decode().rstrip("=")}}
FRONT = {"name": "demo", "description": "A dynamic skill"}


APP="""<!doctype html><output id="result"></output><script>
addEventListener("message",e=>{
 if(e.source!==parent)return;
 if(e.data.id===1)parent.postMessage({jsonrpc:"2.0",id:2,method:"tools/call",params:{name:"app_echo",arguments:{}}},"*");
 if(e.data.id===2)document.querySelector("output").textContent=e.data.result.content[0].text;
});
parent.postMessage({jsonrpc:"2.0",id:1,method:"ui/initialize",params:{}},"*");
</script>"""
TASK_STATUS={}
CATALOG_CHANGED=False

def response(request):
    method, p = request["method"], request.get("params", {})
    if method == "server/discover":
        result = {"supportedVersions": [VERSION], "capabilities": {"tools": {}, "resources": {}, "completions": {}, "prompts": {},
                  "extensions": {TASKS: {},FILES: {"upload": {}, "maxInlineBytes": 1024},SKILLS: {"directoryRead": True}}}}
        if "--upstream" in sys.argv:
            result["capabilities"]["tools"]["listChanged"]=True
            result["capabilities"]["resources"]["subscribe"]=True
    elif method == "tools/list":
        result = {"tools": [{"name": name, "inputSchema": {"type": "object"}} for name in
                            ("job","form","url","file")] }
        result["tools"][0]["_meta"]={"ui":{"resourceUri":"ui://test/view.html"}}
        result["tools"][-1]["inputSchema"]["properties"]={"attachment":{"type":"object","x-mcp-file":{},"required":["id","size","mimeType"],"properties":{"id":{"type":"string"},"mimeType":{"type":"string"},"size":{"type":"integer"},"data":{"type":"string"}}}}
        if "--upstream" in sys.argv:
            result["tools"].extend({"name":n,"inputSchema":{"type":"object"}} for n in (["wait","new_item"] if CATALOG_CHANGED else ["wait"]))
        result["tools"].append({"name":"app_echo","inputSchema":{"type":"object"},"_meta":{"ui":{"visibility":["app"]}}})
    elif method == "skills/list":
        result = {"skills": [{"uri":"skill://demo/SKILL.md","frontmatter":FRONT,"resources":"dynamic"}]}
    elif method == "skills/get":
        assert p["uri"] in ["skill://demo/SKILL.md","skill://private/demo/SKILL.md"]
        result={"skill":{"uri":p["uri"],"frontmatter":FRONT,"resources":"dynamic"}}
    elif method=="resources/templates/list":
        result={"resourceTemplates":[{"name":"ticket","uriTemplate":"issue://sample/{id}"}]}
    elif method=="prompts/list":
        result={"prompts":[{"name":"show","arguments":[{"name":"id"}]}]}
    elif method=="completion/complete":
        assert p["ref"] in [{"type":"ref/resource","uri":"issue://sample/{id}"},{"type":"ref/prompt","name":"show"}]
        result={"completion":{"values":["demo-id"]}}
    elif method == "resources/list":
        result = {"resources":[{"name":"live","uri":"example://live"}]}
    elif method == "resources/read":
        result = {"contents":[{"uri":p["uri"],"text":"---\nname: demo\ndescription: A dynamic skill\n---\nexample" if p["uri"].endswith("SKILL.md") else "template"}]}
        if p["uri"]=="ui://test/view.html":
            result={"contents":[{"uri":p["uri"],"mimeType":"text/html;profile=mcp-app","text":APP}]}
    elif method == "resources/directory/read":
        result = {"resources":[{"name":"template.md","uri":p["uri"]+"/template.md"}]}
    elif method == "tools/call":
        name = p["name"]
        if name=="app_echo":result={"content":[{"type":"text","text":"upstream-callback"}]}
        elif name == "job":
            TASK_STATUS["shared-id"]="input_required"
            assert TASKS in p["_meta"]["io.modelcontextprotocol/clientCapabilities"]["extensions"]
            result = {"resultType":"task","taskId":"shared-id","status":"working","createdAt":"2026-11-27T00:00:00Z","lastUpdatedAt":"2026-11-27T00:00:00Z","ttlMs":3600000}
        elif "requestState" in p and p["arguments"].get("repeat"):
            result={"resultType":"upload_required","uploads":[],"requestState":"second"}
        elif "requestState" in p:
            assert p["requestState"]=="original-state"
            result={"content":[{"type":"text","text":p.get("inputResponses",{}).get("q",{}).get("action","uploaded")}]}
            if p["arguments"].get("transfer"):
                result["_meta"]={FILES:{"downloads":[{"name":FILE["name"],"mimeType":FILE["mimeType"],"size":FILE["size"],
                    "digest":FILE["digest"],"path":"/download","credentials":"include"}]}}
        elif name=="file" and "data" in p["arguments"].get("attachment",{}):
            result={"content":[{"type":"text","text":"inline"}]}
        elif name == "file":
            result={"resultType":"upload_required","uploads":[{"fileId":"sample","url":"https://storage.example/upload","credentials":"omit"}],"requestState":"original-state"}
            if p["arguments"].get("transfer"):
                result["uploads"]=[{"fileId":"sample","path":"/upload","credentials":"include"}]
                if p["arguments"].get("multipart"):
                    result["uploads"][0].update(method="POST",form={"fileField":"document","fields":[{"name":"key","value":"a"},{"name":"key","value":"b"}]})
        else:
            params={"mode":name,"message":"Proceed?"}
            if name=="url":params.update(url="https://service.example/check",elicitationId="unique")
            else:params["requestedSchema"]={"type":"object","properties":{"ok":{"type":"boolean"}}}
            result={"resultType":"input_required","inputRequests":{"q":{"method":"elicitation/create","params":params}},"requestState":"original-state"}
    elif method.startswith("tasks/"):
        assert p["taskId"]=="shared-id"
        result={"taskId":"shared-id","status":"completed","createdAt":"2026-11-27T00:00:00Z","lastUpdatedAt":"2026-11-27T00:00:01Z","ttlMs":3600000,
                "result":{"resultType":"complete","content":[{"type":"text","text":"done"}]}} if method=="tasks/get" else {}
        if method=="tasks/update":TASK_STATUS["shared-id"]="completed"
        if method=="tasks/cancel":TASK_STATUS["shared-id"]="cancelled"
        if method=="tasks/get":
            result["status"]=TASK_STATUS.get("shared-id","completed")
            if result["status"]!="completed":
                del result["result"]
            if result["status"]=="input_required":
                result["inputRequests"]={"q":{"method":"elicitation/create","params":{"mode":"form","message":"Confirm","requestedSchema":{"type":"object","properties":{}}}}}
    else:
        return {"jsonrpc":"2.0","id":request["id"],"error":{"code":-32601,"message":"unknown method"}}
    result.setdefault("resultType","complete")
    result.setdefault("_meta",{})["io.modelcontextprotocol/serverInfo"]={"name":"fixture","version":"1"}
    return {"jsonrpc":"2.0","id":request["id"],"result":result}


class Upstream(BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path=="/mcp/upload":
            assert self.headers["Authorization"]=="Bearer upstream-only"
            data=self.rfile.read(int(self.headers["Content-Length"]))
            msg=BytesParser(policy=policy.default).parsebytes(("Content-Type: "+self.headers["Content-Type"]+"\r\n\r\n").encode()+data)
            parts=list(msg.iter_parts())
            assert [p.get_param("name",header="content-disposition") for p in parts]==["key","key","document"]
            assert [p.get_payload(decode=True) for p in parts]==[b"a",b"b",FILE_BYTES]
            assert parts[-1].get_filename()=="样本%22.txt"
            self.send_response(204);self.end_headers();return
        q=json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        assert q["method"]==self.headers["Mcp-Method"]
        if q["method"]=="tools/call": assert q["params"]["name"]==self.headers["Mcp-Name"]
        if q["method"].startswith("tasks/"): assert q["params"]["taskId"]==self.headers["Mcp-Name"]
        body=json.dumps(response(q)).encode()
        self.send_response(200)
        self.send_header("Content-Type","application/json")
        self.end_headers()
        self.wfile.write(body)
    def do_PUT(self):
        assert self.path=="/mcp/upload" and self.headers["Authorization"]=="Bearer upstream-only"
        data=self.rfile.read(int(self.headers["Content-Length"]))
        assert data==FILE_BYTES
        self.server.uploaded=data
        self.send_response(204);self.end_headers()
    def do_GET(self):
        assert self.path=="/mcp/download" and self.headers["Authorization"]=="Bearer upstream-only"
        self.send_response(200);self.send_header("Content-Length",str(len(FILE_BYTES)));self.end_headers()
        self.wfile.write(self.server.uploaded)
    def log_message(self,*args):pass


def check(root,upstream):
    tokens={name:secrets.token_hex(24) for name in ("reader","other")}
    with socket.socket() as probe:
        probe.bind(("127.0.0.1",0));port=probe.getsockname()[1]
    url=f"http://127.0.0.1:{port}/mcp"
    env={**os.environ,"BRIDGE_MASTER_KEY":secrets.token_hex(32),"BRIDGE_TOKENS":json.dumps(tokens),"BRIDGE_PUBLIC_URL":"https://bridge.example"}
    def start():return subprocess.Popen([binary,"serve","-data",root,"-listen",f"127.0.0.1:{port}"],
          env=env)
    def healthy():
        if server.poll() is not None:raise AssertionError("Bridge exited")
        try:
            with urllib.request.urlopen(url.removesuffix("/mcp")+"/healthz",timeout=1) as r:return r.read()==b"ok\n"
        except OSError:return False
    server=start()
    def rpc(method,params,who="reader",expect_error=False,caps=CAPS,endpoint=None):
        meta={"io.modelcontextprotocol/protocolVersion":VERSION,"io.modelcontextprotocol/clientCapabilities":caps,
              "io.modelcontextprotocol/clientInfo":{"name":"test","version":"1"}}
        body=json.dumps({"jsonrpc":"2.0","id":1,"method":method,"params":{**params,"_meta":meta}}).encode()
        headers={"Mcp-Method":method,"Mcp-Protocol-Version":VERSION,"Content-Type":"application/json",
                 "Accept":"application/json,text/event-stream","Authorization":"Bearer "+tokens[who]}
        if method=="tools/call":headers["Mcp-Name"]=params["name"]
        if method.startswith("tasks/"):headers["Mcp-Name"]=params["taskId"]
        if method=="resources/read":headers["Mcp-Name"]=params["uri"]
        req=urllib.request.Request(endpoint or url,data=body,headers=headers)
        try:
            with urllib.request.urlopen(req,timeout=15) as res: data=res.read().decode()
        except urllib.error.HTTPError as exc:
            data=exc.read().decode()
            if not expect_error: raise RuntimeError(data) from exc
        if method=="subscriptions/listen" and not expect_error:
            return [json.loads(l[5:]) for l in data.splitlines() if l.startswith("data:")]
        reply=json.loads(next((l[5:] for l in data.splitlines() if l.startswith("data:")),data))
        assert ("error" in reply)==expect_error,reply
        return reply.get("result",reply.get("error"))
    try:
        eventually(healthy)
        with closing(Agent(root)) as a:
            for name,spec in (("stdio",{"type":"stdio","command":sys.executable,"args":[str(Path(__file__).resolve()),"--upstream",str(root/"cancelled")]}),
                              ("http",{"type":"streamable-http","url":upstream,"headers":{"Authorization":"Bearer upstream-only"}})):
                r=a.call("bridge_create_source",source={"id":name,**spec,"grants":list(tokens)})
                assert r["state"]=="ready",r
            a.call("bridge_save_destination",destination={"id":"local-test","type":"cli_process","command":sys.executable,"args":["-c","print(123)"],"cwd":str(root),"input_format":"json","grants":["reader"]},revision=0)
        with urllib.request.urlopen(url+"/server-card") as page:
            assert json.load(page)["remotes"][0]["url"]=="https://bridge.example/mcp"
        with closing(Agent(root)) as a:
            assert a.call("bridge_discover_server",url=url.removesuffix("/mcp"))["cards"][0]["type"]=="application/mcp-server-card+json"
        template=next(t["uriTemplate"] for t in rpc("resources/templates/list",{})["resourceTemplates"] if "/http/" in t["uriTemplate"])
        uri=template.replace("{id}","demo-id")
        assert rpc("resources/read",{"uri":uri})["contents"][0]["uri"]==uri
        for ref in ({"type":"ref/resource","uri":template},{"type":"ref/prompt","name":"http__show"}):
            assert rpc("completion/complete",{"ref":ref,"argument":{"name":"id","value":"d"}})["completion"]["values"]==["demo-id"]
        scoped=url+"/apps/http"
        assert rpc("tools/call",{"name":"job","arguments":{}},endpoint=scoped)["resultType"]=="task"
        tools=rpc("tools/list",{},endpoint=scoped)["tools"]
        assert next(t for t in tools if t["name"]=="job")["_meta"]["ui"]["resourceUri"]=="ui://test/view.html"
        app=next(t for t in rpc("tools/list",{})["tools"] if t["name"].startswith("app_http_job_"))
        view=app["_meta"]["ui"]["resourceUri"]
        assert view.startswith("ui://mcp-bridge-source/http/") and rpc("resources/read",{"uri":view})["contents"][0]["uri"]==view
        saved=rpc("tools/call",{"name":"save_trigger","arguments":{"trigger_id":"annual","destination_id":"local-test","when":{"type":"schedule","cron":"0 0 1 1 *","timezone":"UTC"}}})
        assert not saved.get("isError"),saved
        local=rpc("tools/call",{"name":"run_trigger_now","arguments":{"trigger_id":"annual"}})
        assert local["resultType"]=="task",local
        local_id={"taskId":local["taskId"]}
        eventually(lambda:rpc("tasks/get",local_id)["status"]=="completed")
        for src in ("stdio","http"):
            def call(name,**more):
                return rpc("tools/call",{"name":"call_tool","arguments":{"tool_id":src+"/"+name,"arguments":{"attachment":FILE} if name=="file" else {}},**more})
            task=call("job")
            assert task["resultType"]=="task" and task["taskId"]!="shared-id",task
            params={"taskId":task["taskId"]}
            assert rpc("tasks/get",params)["inputRequests"]["q"]["method"]=="elicitation/create"
            assert rpc("tasks/get",params,"other",True)["code"]==-32602
            rpc("tasks/update",{**params,"inputResponses":{"q":{"action":"accept"}}})
            assert rpc("tasks/get",params)["result"]["content"][0]["text"]=="done"
            stream=rpc("subscriptions/listen",{"notifications":{"taskIds":[task["taskId"]]}})
            assert stream[1]["params"]["status"]=="completed" and stream[1]["params"]["taskId"]==task["taskId"]
            assert rpc("subscriptions/listen",{"notifications":{"taskIds":[task["taskId"]]}},caps={},expect_error=True)["code"]==-32021
            rpc("tasks/cancel",params)
            assert rpc("tasks/get",params)["status"]=="cancelled"
            for mode in ("form","url"):
                first=call(mode)
                assert first["inputRequests"]["q"]["params"]["mode"]==mode,first
                answered=call(mode,requestState=first["requestState"],inputResponses={"q":{"action":"decline"}})
                assert answered["content"][0]["text"]=="decline",answered
            first=call("file")
            assert first["uploads"][0]["credentials"]=="omit"
            assert call("file",requestState=first["requestState"])["content"][0]["text"]=="uploaded"
            skills=rpc("skills/list",{})["skills"]
            skill=next(s for s in skills if s["uri"].startswith("skill://"+src+"/"))
            assert skill["resources"]=="dynamic"
            unlisted="skill://"+src+"/upstream/"+base64.urlsafe_b64encode(b"skill://private/demo/SKILL.md").decode().rstrip("=")+"/demo/SKILL.md"
            assert rpc("skills/get",{"uri":unlisted})["skill"]["uri"]==unlisted
            directory=skill["uri"].rsplit("/",1)[0]
            entry=rpc("resources/directory/read",{"uri":directory})["resources"][0]
            assert entry["uri"]==directory+"/template.md"
            assert rpc("resources/read",{"uri":entry["uri"]})["contents"][0]["text"]=="template"
        alias=next(t["name"] for t in rpc("tools/list",{})["tools"] if t["name"]=="file_http_file_"+hashlib.sha256(b"http/file").hexdigest()[:16])
        args={"transfer":True,"attachment":FILE}
        send={"name":alias,"arguments":args}
        missing=rpc("tools/call",send,caps={},expect_error=True)
        assert missing["data"]["requiredCapabilities"]["extensions"][FILES]=={"upload":{}}
        inline={**FILE,"data":base64.b64encode(FILE_BYTES).decode()}
        def inline_call(file):return rpc("tools/call",{"name":alias,"arguments":{"attachment":file}},caps={})
        assert inline_call(inline)["content"][0]["text"]=="inline"
        assert inline_call({**inline,"data":None})["isError"]
        assert inline_call({**inline,"digest":{"algorithm":"sha-256","value":"bad"}})["isError"]
        first=rpc("tools/call",send)
        upload=first["uploads"][0]
        assert upload["credentials"]=="include" and "url" not in upload
        def transfer(path,who,data=None):
            request=urllib.request.Request(url+path,data=data,method="GET" if data is None else "PUT",
                                           headers={"Authorization":"Bearer "+tokens[who]})
            return urllib.request.urlopen(request,timeout=10)
        try: transfer(upload["path"],"other",FILE_BYTES)
        except urllib.error.HTTPError as exc: assert exc.code==403
        else: raise AssertionError("transfer crossed principals")
        try:transfer(upload["path"],"reader",bytes(len(FILE_BYTES)))
        except urllib.error.HTTPError as exc:assert exc.code==400
        else:raise AssertionError("bad digest accepted")
        with transfer(upload["path"],"reader",FILE_BYTES) as sent: assert sent.status==204
        retry={**send,"requestState":first["requestState"]}
        assert rpc("tools/call",{**retry,"arguments":{**args,"transfer":False}},expect_error=True)["code"]==-32602
        assert rpc("tools/call",retry,"other",True)["code"]==-32602
        result=rpc("tools/call",retry)
        download=result["_meta"][FILES]["downloads"][0]
        with transfer(download["path"],"reader") as received: assert received.read()==FILE_BYTES
        repeat={"name":alias,"arguments":{**args,"repeat":True}}
        again=rpc("tools/call",repeat)
        assert rpc("tools/call",{**repeat,"requestState":again["requestState"]})["isError"]
        form=rpc("tools/call",{"name":alias,"arguments":{**args,"multipart":True,"attachment":{**FILE,"name":'folder/样本".txt'}}})["uploads"][0]
        assert "form" not in form and form["method"]=="POST"
        with urllib.request.urlopen(urllib.request.Request(url+form["path"],method="POST",data=FILE_BYTES,
                headers={"Authorization":"Bearer "+tokens["reader"]}),timeout=10) as response:
            assert response.status==204
        meta={"io.modelcontextprotocol/protocolVersion":VERSION,"io.modelcontextprotocol/clientCapabilities":CAPS,
              "io.modelcontextprotocol/clientInfo":{"name":"stdio-client","version":"1"}}
        with subprocess.Popen([binary,"stdio","-data",str(root)],stdin=subprocess.PIPE,stdout=subprocess.PIPE,text=True) as proc:
            wire=json.dumps({"jsonrpc":"2.0","method":"tools/call","id":9,"params":{**send,"_meta":meta}})
            proc.stdin.write(wire+"\n");proc.stdin.flush()
            issued=json.loads(proc.stdout.readline())["result"]["uploads"][0]
            assert issued["credentials"]=="omit" and issued["url"].startswith("https://bridge.example/mcp-file-link/")
            with urllib.request.urlopen(urllib.request.Request(issued["url"].replace("https://bridge.example",url.removesuffix("/mcp")),
                    data=FILE_BYTES,method="PUT"),timeout=10) as response:assert response.status==204
            proc.stdin.close()
        with subprocess.Popen([binary,"stdio","-data",str(root)],stdin=subprocess.PIPE,stdout=subprocess.PIPE,text=True) as proc:
            import queue
            frames=queue.Queue()
            def receive():
                for line in proc.stdout:frames.put(json.loads(line))
            threading.Thread(target=receive,daemon=True).start()
            def send(req):
                proc.stdin.write(json.dumps({"jsonrpc":"2.0",**req})+"\n");proc.stdin.flush()
            resource=next(r["uri"] for r in rpc("resources/list",{})["resources"] if "/stdio/" in r["uri"] and r["name"]=="live")
            send({"id":71,"method":"subscriptions/listen","params":{"notifications":{"resourceSubscriptions":[resource]},"_meta":meta}})
            try:
                ack=frames.get(timeout=8)
                assert ack["params"]["notifications"]["resourceSubscriptions"]==[resource],ack
                send({"id":72,"method":"tools/call","params":{"name":"call_tool","arguments":{"tool_id":"stdio/wait","arguments":{}},"_meta":{**meta,"progressToken":"client-progress"}}})
                received={note["method"]:note["params"] for note in (frames.get(timeout=8),frames.get(timeout=8))}
                assert received["notifications/progress"]["progressToken"]=="client-progress",received
                assert received["notifications/resources/updated"]["uri"]==resource,received
                send({"method":"notifications/cancelled","params":{"requestId":72,"_meta":meta}})
                eventually((root/"cancelled").exists)
                def changed():
                    res=rpc("tools/call",{"name":"search_tools","arguments":{"source_id":"stdio","query":"new_item"}})
                    return any(t["tool_id"]=="stdio/new_item" for t in res["structuredContent"]["tools"])
                eventually(changed)
            finally:
                proc.stdin.close()
                if proc.poll() is None:proc.terminate()
        server.terminate();server.wait(timeout=10)
        server=start()
        eventually(healthy)
        receipt=rpc("tasks/get",local_id)["result"]["structuredContent"]
        assert receipt["delivery_result"]["exit_code"]==0 and receipt["delivery_result"]["stdout"].strip()=="123",receipt
        assert rpc("tasks/get",params)["taskId"]==params["taskId"]
        assert rpc("tasks/get",local_id)["ttlMs"] is None
        deleted=rpc("tools/call",{"name":"delete_trigger","arguments":{"trigger_id":"annual","revision":1}})
        assert not deleted.get("isError"),deleted
        assert rpc("tasks/get",local_id)["ttlMs"]>=30*86400000
    finally:
        server.terminate();server.wait(timeout=10)


if __name__=="__main__":
    if "--upstream" in sys.argv:
        resource_sub=None
        catalog_sub=None
        waiting=None
        def note(method,params):
            print(json.dumps({"jsonrpc":"2.0","method":method,"params":params}),flush=True)
        for line in sys.stdin:
            request=json.loads(line)
            params=request.get("params",{})
            if request["method"]=="subscriptions/listen":
                if params["notifications"].get("resourceSubscriptions"):resource_sub=request["id"]
                else:catalog_sub=request["id"]
                note("notifications/subscriptions/acknowledged",{"notifications":params["notifications"],"_meta":{"io.modelcontextprotocol/subscriptionId":request["id"]}})
            elif request["method"]=="tools/call" and params["name"]=="wait":
                waiting=request["id"]
                token=params["_meta"]["progressToken"]
                assert token!="client-progress"
                note("notifications/progress",{"progressToken":token,"progress":1,"total":2})
                if resource_sub:
                    note("notifications/resources/updated",{"uri":"example://live","_meta":{"io.modelcontextprotocol/subscriptionId":resource_sub}})
                CATALOG_CHANGED=True
                note("notifications/tools/list_changed",{"_meta":{"io.modelcontextprotocol/subscriptionId":catalog_sub}})
            elif request["method"]=="notifications/cancelled" and params["requestId"]==waiting:
                Path(sys.argv[-1]).touch()
                print(json.dumps({"jsonrpc":"2.0","id":waiting,"error":{"code":-32800,"message":"cancelled"}}),flush=True)
            elif "id" in request: print(json.dumps(response(request)),flush=True)
    else:
        with tempfile.TemporaryDirectory() as temp, ThreadingHTTPServer(("127.0.0.1",0),Upstream) as up:
            threading.Thread(target=up.serve_forever,daemon=True).start()
            check(Path(temp), f"http://127.0.0.1:{up.server_port}/mcp")
            up.shutdown()
        print("PASS modern Skills, Tasks, Elicitation and Files over stdio and Streamable HTTP upstreams")
