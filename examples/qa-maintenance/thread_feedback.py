"""Route maintenance advice using a verified Bridge delivery and observed MCP metadata."""


def feedback_target(delivery, thread, observed_calls):
    event = delivery["event"]
    data = event["data"]
    if (delivery["flow_id"], delivery["source_id"], event["name"], data["actor"], data["tool_id"]) != (
            "qa-read-review", "bridge.builtin", "task.repeated_read", "admin", "qa/get_bot"):
        raise ValueError("unexpected maintenance trigger")
    kind, target, turn = data["task_scope"].split(":")
    if kind != "codex" or target != thread or not any(
            c["thread"] == target and c["turn"] == turn and c["tool"] == "call_tool" for c in observed_calls):
        raise ValueError("delivery does not belong to the observed Codex turn")
    return target, turn


def deliver_review(inject_items, delivery, advice, thread, observed_calls):
    """Deliver reviewed advice at a turn boundary through the owning host's RPC."""
    target, origin = feedback_target(delivery, thread, observed_calls)
    inject_items({"threadId":target,"items":[{"type":"message","role":"developer",
        "content":[{"type":"input_text","text":"QA 维护建议："+advice}]}]})
    return {"method":"thread/inject_items","delivery_id":delivery["delivery_id"],"thread":target,
            "origin_turn":origin,"advice":advice}
