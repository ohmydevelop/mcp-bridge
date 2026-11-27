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
                class Host(BaseHTTPRequestHandler):
                    def do_GET(self):
                        self.send_response(200)
                        self.send_header("Content-Type","text/html")
                        self.end_headers()
                        self.wfile.write(app if self.path=="/app" else HOST)
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
                    host.shutdown()
            finally:
                agent.close()
        finally:
            proc.terminate()
            proc.wait(timeout=10)
    print("PASS Chromium MCP App initialization and management tool call")

if __name__=="__main__": main()
