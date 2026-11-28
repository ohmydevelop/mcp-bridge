"""Exercise the embedded MCP App through a minimal Chromium MCP Apps host."""
import json
import os
import secrets
import subprocess
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

from acceptance import Agent, binary, eventually
from standard_relay import Upstream
from http.server import ThreadingHTTPServer

HOST = b"""<!doctype html><iframe id="app" src="/app"></iframe><output></output><script>
const frame=document.querySelector('iframe'), status=document.querySelector('output');
onmessage=async e=>{
  if(e.source!==frame.contentWindow)return;
  const {id,method,params}=e.data;
  if(method==='ui/notifications/initialized'){
    const doc=frame.contentDocument;
    doc.querySelector('#op').value='bridge_get_status';
    doc.querySelector('#run').click();
    return;
  }
  let result={};
  if(method==='tools/call'){
    result=await (await fetch('/rpc',{method:'POST',body:JSON.stringify(params)})).json();
    if(params.name==='bridge_get_status' && result.structuredContent.backlog.pending===0)
      status.textContent='PASS bridge_get_status';
  }
  e.source.postMessage({jsonrpc:'2.0',id,result},e.origin);
};
</script>"""

UPSTREAM_HOST = b"""<!doctype html><iframe src="/app"></iframe><output></output><script>
onmessage=async e=>{
 const f=document.querySelector("iframe");
 if(e.source!==f.contentWindow)return;
 const q=e.data;
 const result=q.method==="tools/call"?await(await fetch("/rpc",{method:"POST",body:JSON.stringify(q.params)})).json():{};
 e.source.postMessage({jsonrpc:"2.0",id:q.id,result},e.origin);
 if(q.method==="tools/call" && result.content[0].text==="upstream-callback")
   document.querySelector("output").textContent="PASS upstream-callback";
};
</script>"""

def main():
    with tempfile.TemporaryDirectory() as tmp:
        state=Path(tmp)/"state"
        proc=subprocess.Popen([binary,"serve","-app","-data",str(state),"-listen","127.0.0.1:0"],
                              env={**os.environ,"BRIDGE_MASTER_KEY":secrets.token_hex(32)})
        try:
            eventually(lambda:(state/"bridge.sock").exists())
            agent=Agent(state)
            try:
                tools=agent.rpc("tools/list",{})["tools"]
                uri=next(t for t in tools if t["name"]=="bridge_open_app")["_meta"]["ui"]["resourceUri"]
                app=agent.rpc("resources/read",{"uri":uri})["contents"][0]["text"].encode()
                shell=HOST
                class Host(BaseHTTPRequestHandler):
                    def do_GET(self):
                        self.send_response(200)
                        self.send_header("Content-Type","text/html")
                        self.end_headers()
                        self.wfile.write(app if self.path=="/app" else shell)
                    def do_POST(self):
                        params=json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                        data=json.dumps(agent.rpc("tools/call",params)).encode()
                        self.send_response(200)
                        self.end_headers()
                        self.wfile.write(data)
                    def log_message(self,*args): pass
                with HTTPServer(("127.0.0.1",0),Host) as host:
                    threading.Thread(target=host.serve_forever,daemon=True).start()
                    result=subprocess.run(["chromium","--headless","--disable-gpu","--disable-dev-shm-usage",
                        "--user-data-dir="+tmp+"/chrome","--virtual-time-budget=5000","--dump-dom",
                        f"http://127.0.0.1:{host.server_port}"],capture_output=True,text=True,timeout=30,check=True)
                    assert "<output>PASS bridge_get_status</output>" in result.stdout,result.stdout
                    with ThreadingHTTPServer(("127.0.0.1",0),Upstream) as upstream:
                        threading.Thread(target=upstream.serve_forever,daemon=True).start()
                        agent.call("bridge_create_source",source={"id":"app-demo","type":"streamable-http","url":f"http://127.0.0.1:{upstream.server_port}/mcp"})
                        listing=agent.rpc("tools/list",{})["tools"]
                        item=next(t for t in listing if t["name"]=="job")
                        uri=item["_meta"]["ui"]["resourceUri"]
                        assert uri.startswith("ui://mcp-bridge-source/app-demo/")
                        app=agent.rpc("resources/read",{"uri":uri})["contents"][0]["text"].encode()
                        shell=UPSTREAM_HOST
                        result=subprocess.run(result.args,capture_output=True,text=True,timeout=30,check=True)
                        assert "<output>PASS upstream-callback</output>" in result.stdout,result.stdout
                        upstream.shutdown()
                    host.shutdown()
            finally:
                agent.close()
        finally:
            proc.terminate()
            proc.wait(timeout=10)
    print("PASS Chromium MCP App initialization and upstream App tool callback")

if __name__=="__main__": main()
