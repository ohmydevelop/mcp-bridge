import json
import sys

send = lambda msg: print(json.dumps(msg, allow_nan=False), flush=True)
class Context:
    def call_tool(self, tool_id, arguments):
        send({"op":"call","tool":tool_id,"arguments":arguments})
        reply = json.loads(sys.stdin.readline())
        return reply["value"]

try:
    req = json.loads(sys.stdin.readline())
    code = compile(req["script"], "<binding>", "exec")
    if req["check"]:
        import ast
        tree = ast.parse(req["script"])
        if not any(isinstance(n, ast.FunctionDef) and n.name == "run" and len(n.args.args)==2 for n in tree.body):
            raise ValueError("expected def run(args, ctx)")
        value = True
    else:
        scope = {"__name__":"__binding__"}
        exec(code, scope)
        value = scope["run"](req["args"], Context())
    send({"op":"result","value":value})
except Exception as exc:
    send({"op":"error","error":str(exc)[:500]})
