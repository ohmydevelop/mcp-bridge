"""Live PTY smoke test. Requires locally authenticated Codex and OpenCode CLIs."""
import argparse
from contextlib import ExitStack, closing
import fcntl
import json
import os
from pathlib import Path
import pty
import re
import secrets
import select
import signal
import struct
import subprocess
import tempfile
import termios
import threading
import time

from acceptance import Agent, binary, eventually


class Screen:
    """Outer terminal for unattended observation. Event text is submitted by the Bridge launcher."""
    def __init__(self, command, cwd, env):
        self.master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 38, 120, 0, 0))
        self.proc = subprocess.Popen(["bash", "-c", 'exec "$@"', "bash", *command],
            cwd=cwd, env={**env, "TERM":"xterm-256color"}, stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
        os.close(slave)
        self.raw = bytearray()
        self.thread = threading.Thread(target=self.pump, daemon=True)
        self.thread.start()

    def pump(self):
        while True:
            try:
                chunk = os.read(self.master, 65536)
                if not chunk:
                    break
                self.raw.extend(chunk)
                for query, response in [(b"\x1b[6n",b"\x1b[1;1R"),(b"\x1b[c",b"\x1b[?1;2c"),(b"\x1b[?u",b"\x1b[?0u")]:
                    if query in chunk:
                        os.write(self.master,response)
            except OSError:
                break

    def text(self):
        return re.sub(rb"\x1b\[[0-?]*[ -/]*[@-~]",b"",bytes(self.raw)).decode(errors="replace")

    def close(self):
        if self.proc.poll() is None:
            self.proc.terminate()
        self.proc.wait(timeout=15)
        os.close(self.master)


def wait_for(fn, timeout=30):
    end = time.monotonic()+timeout
    while time.monotonic()<end:
        value=fn()
        if value:
            return value
        time.sleep(.15)
    raise AssertionError("timed out waiting for "+repr(fn))


def test_agents(root, output):
    state=root/"state"
    project=root/"project"
    home=root/"home"
    project.mkdir();home.mkdir()
    subprocess.run(["git","init","-q",project],check=True)
    auth=Path(os.getenv("CODEX_HOME",Path.home()/".codex"))/"auth.json"
    (home/"auth.json").symlink_to(auth)
    args=["stdio","-data",str(state)]
    (home/"config.toml").write_text("[projects."+json.dumps(str(project))+']\ntrust_level="trusted"\n[mcp_servers.bridge]\ncommand='+json.dumps(binary)+"\nargs="+json.dumps(args)+"\n")
    (project/"opencode.json").write_text(json.dumps({"mcp":{"bridge":{"type":"local","command":[binary,*args],"enabled":True}}}))
    report={"transport":"mcp-bridge run via bash; native PTY, no webhook or tmux","clients":{}}
    with ExitStack() as stack:
        daemon=subprocess.Popen([binary,"serve","-data",state,"-listen","127.0.0.1:0"],env={**os.environ,"BRIDGE_MASTER_KEY":secrets.token_hex(32)})
        def shutdown():
            daemon.terminate();daemon.wait(timeout=15)
        stack.callback(shutdown)
        eventually(lambda:(state/"bridge.sock").exists())
        admin=stack.enter_context(closing(Agent(state)))
        screens={}
        from acceptance import fixture
        admin.call("bridge_create_source",source={"id":"meter","type":"stdio","command":"/usr/bin/python3",
                   "args":[str(fixture/"server.py")],"env":{"QA_EVENT_FILE":str(root/"events")},"events":{"reading.ready":"poll_runs"}})
        for name,mode,cmd in [("codex","inbox",["codex","--no-daemon","--no-alt-screen","-m","gpt-6.2-sol"]),("opencode","auto",["opencode","--standalone"])]:
            env=dict(os.environ)
            if name=="codex":env["CODEX_HOME"]=str(home)
            screen=Screen([binary,"run","-data",str(state),"--delivery="+mode,"--",*cmd],project,env)
            stack.callback(screen.close)
            screens[name]=screen
            match=wait_for(lambda:re.search(r"bridge (term-[0-9a-f]+)",screen.text()))
            dest=match.group(1)
            admin.call("save_trigger",trigger_id=name,when={"type":"event","source_id":"meter","event_name":"reading.ready"},
               instruction="Reply with only the receipt: uppercase the tag, append a hyphen and the sum of x and y. Do not use any tools.",destination_id=dest)
            report["clients"][name]={"destination":dest,"mode":mode}
        try:
            wait_for(lambda:"Ask Codex" in screens["codex"].text() or "GPT-6.2" in screens["codex"].text(),45)
            wait_for(lambda:"Ask anything" in screens["opencode"].text() or "Build ·" in screens["opencode"].text(),45)
            data={"tag":secrets.token_hex(4),"x":37,"y":46}
            expected=data["tag"].upper()+"-83"
            report.update(data=data,expected=expected)
            (root/"events").write_text(json.dumps([{"eventId":"meter-one","name":"reading.ready","data":data}]))
            wait_for(lambda:"pending; Ctrl+]" in screens["codex"].text())
            os.write(screens["codex"].master,b"\x1d")
            wait_for(lambda:"[bridge preview]" in screens["codex"].text())
            os.write(screens["codex"].master,b"\x1d")
            for name,screen in screens.items():
                wait_for(lambda:expected in screen.text(),120)
                report["clients"][name]["answer"]=expected
                record=wait_for(lambda:next((r for r in admin.call("get_trigger",trigger_id=name)["deliveries"] if r["status"]=="submitted"),None))
                report["clients"][name]["receipt"]=record
        finally:
            for name,screen in screens.items():
                if "answer" not in report["clients"][name]:
                    print(name,repr(screen.text()[-1300:]))
            output.write_text(json.dumps(report,indent=2))
    print("PASS",report["expected"],"received from both agents")


if __name__=="__main__":
    parser=argparse.ArgumentParser()
    parser.add_argument("--output",type=Path,required=True)
    opts=parser.parse_args()
    with tempfile.TemporaryDirectory() as directory:
        test_agents(Path(directory),opts.output)
