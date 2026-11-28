"""Exercise real OAuth challenges, PKCE callbacks and both service grants over HTTP."""
import base64
from contextlib import closing
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import sys
import tempfile
import threading
import urllib.error
import urllib.parse
import urllib.request

import acceptance
from acceptance import Agent, eventually


class Provider(BaseHTTPRequestHandler):
    def reply(self, value, status=200, **headers):
        body = json.dumps(value).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        for key, val in headers.items():
            self.send_header(key, val)
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        origin = self.server.origin
        if self.path.startswith("/.well-known/oauth-protected-resource"):
            self.reply({"resource": origin + "/mcp", "authorization_servers": [origin]})
        elif self.path.startswith("/.well-known/"):
            self.reply({"issuer": origin, "token_endpoint": origin + "/token",
                        "authorization_endpoint": origin + "/authorize",
                        "response_types_supported": ["code"], "code_challenge_methods_supported": ["S256"],
                        "token_endpoint_auth_methods_supported": ["client_secret_basic","client_secret_post"],
                        "scopes_supported": ["read", "offline_access"]})
        else:
            self.reply({},404)

    def do_POST(self):
        data = self.rfile.read(int(self.headers["Content-Length"]))
        if self.path == "/token":
            args = urllib.parse.parse_qs(data.decode())
            grant = args["grant_type"][0]
            self.server.grants.append(grant)
            if grant == "authorization_code":
                proof = hashlib.sha256(args["code_verifier"][0].encode()).digest()
                assert base64.urlsafe_b64encode(proof).decode().rstrip("=") == self.server.challenge
                assert args["code"] == ["example-code"] and args["resource"] == [self.server.origin+"/mcp"]
            elif grant.endswith("token-exchange"):
                assert args["subject_token"] == ["idp-issued"]
                self.reply({"access_token":"id-jag","issued_token_type":"urn:ietf:params:oauth:token-type:id-jag","token_type":"N_A"})
                return
            elif grant.endswith("jwt-bearer"):
                assert args["assertion"] == ["id-jag"]
            self.reply({"token_type":"Bearer","access_token":"source-only","expires_in":1 if grant=="authorization_code" else 3600,"refresh_token":"renew"})
            return
        assert self.path == "/mcp"
        if self.headers.get("Authorization") != "Bearer source-only":
            self.reply({},401,**{"WWW-Authenticate":f'Bearer resource_metadata="{self.server.origin}/.well-known/oauth-protected-resource/mcp"'})
            return
        req=json.loads(data)
        if req["method"]=="server/discover":
            result={"supportedVersions":["2026-07-28"],"capabilities":{"tools":{}}}
        elif req["method"]=="tools/list":
            result={"tools":[{"name":"whoami","inputSchema":{"type":"object"}}]}
        else:
            result={"content":[{"type":"text","text":"authorized"}]}
        result.update(resultType="complete",_meta={"io.modelcontextprotocol/serverInfo":{"name":"oauth-fixture","version":"1"}})
        self.reply({"jsonrpc":"2.0","id":req["id"],"result":result})

    def log_message(self, *args):
        pass


def run(root, provider):
    with socket.socket() as probe:
        probe.bind(("127.0.0.1",0))
        addr=f"127.0.0.1:{probe.getsockname()[1]}"
    base="http://"+addr
    env={**os.environ,"BRIDGE_MASTER_KEY":secrets.token_hex(32),"BRIDGE_PUBLIC_URL":base}
    def start():
        proc=subprocess.Popen([acceptance.binary,"serve","-data",root,"-listen",addr],env=env)
        def healthy():
            if proc.poll() is not None:raise RuntimeError("Bridge exited")
            try:
                with urllib.request.urlopen(base+"/healthz",timeout=1) as res:return res.status==200
            except OSError:return False
        eventually(healthy)
        return proc
    def create(a,name,mode,**extra):
        return a.call("bridge_create_source",source={"id":name,"type":"streamable-http","url":provider.origin+"/mcp",
            "oauth":{"mode":mode,"issuer":provider.origin,"client_id":"bridge","client_secret":"secret",**extra}})
    proc=start()
    try:
        with closing(Agent(root)) as a:
            with urllib.request.urlopen(base+"/oauth/client.json") as response:
                metadata=json.load(response)
                assert metadata["client_id"]==base+"/oauth/client.json" and metadata["redirect_uris"]==[base+"/oauth/callback"]
            assert create(a,"service","client_credentials")["state"]=="ready"
            assert create(a,"corp","enterprise",idp_issuer=provider.origin,idp_client_id="idp",id_token="idp-issued")["state"]=="ready"
            assert create(a,"browser","authorization_code")["state"]=="authorization_required"
            link=a.call("bridge_begin_oauth",source_id="browser")["authorization_url"]
            q=urllib.parse.parse_qs(urllib.parse.urlsplit(link).query)
            provider.challenge=q["code_challenge"][0]
            args=urllib.parse.urlencode({"code":"example-code","state":q["state"][0]})
            with urllib.request.urlopen(base+"/oauth/callback?"+args,timeout=10) as res:assert res.status==200
            try:
                urllib.request.urlopen(base+"/oauth/callback?"+args,timeout=10).close()
                raise AssertionError("OAuth callback accepted the same state twice")
            except urllib.error.HTTPError as err:
                assert err.code==400
            eventually(lambda:a.call("bridge_get_status",source_id="browser")["ready"])
        proc.terminate();proc.wait(timeout=10);proc=start()
        with closing(Agent(root)) as a:
            assert a.call("bridge_get_status",source_id="browser")["ready"]
            out=a.rpc("tools/call",{"name":"call_tool","arguments":{"tool_id":"browser/whoami","arguments":{}}})
            assert out["content"][0]["text"]=="authorized"
        assert provider.grants.count("authorization_code")==1 and "refresh_token" in provider.grants,provider.grants
    finally:
        proc.terminate();proc.wait(timeout=10)


if __name__=="__main__":
    if "--binary" in sys.argv:acceptance.binary=str(Path(sys.argv[sys.argv.index("--binary")+1]).resolve())
    with tempfile.TemporaryDirectory() as temp, ThreadingHTTPServer(("127.0.0.1",0),Provider) as provider:
        provider.origin=f"http://127.0.0.1:{provider.server_port}"
        provider.grants=[]
        threading.Thread(target=provider.serve_forever,daemon=True).start()
        run(Path(temp)/"state",provider)
        provider.shutdown()
    print("PASS OAuth client credentials, enterprise exchange, browser PKCE and persisted grant")
