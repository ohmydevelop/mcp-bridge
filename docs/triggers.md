# Triggers

A trigger has one event or schedule, an optional versioned Python handler, and a webhook, terminal session, or CLI process destination. Discover events with `search_event_types` and destinations with `bridge_get_status`. Configure a webhook destination with `bridge_save_destination`. A missing or ambiguous destination returns candidate IDs and does not save the trigger.

Save a scheduled trigger with `save_trigger` after saving the handler with `bridge_save_tool`. Supply the returned version, including for a rollback.

```json
{
  "trigger_id": "morning",
  "when": {"type": "schedule", "cron": "0 9 * * *", "timezone": "Asia/Shanghai"},
  "handler": {"tool_id": "bridge.builtin/summarize", "revision": 1},
  "destination_id": "oncall",
  "instruction": "用一句中文报告当前可见工单总数。"
}
```

The optional `instruction` is a literal task for the receiving agent. Bridge snapshots it at enqueue and sends it as a top-level webhook field alongside the event or processed data. Omit it on update to keep the current task, or send `""` to clear it for future runs. The receiver can also use `skill_ref` for a reusable procedure.

For an event, use `"when":{"type":"event","source_id":"sensor","event_name":"reading","filter":{"site":"east"}}`. An optional `ingestion_mode` selects a supported upstream transport. Use source-declared field values for exact-match filters. A timer uses five cron fields and an explicit IANA timezone. After downtime only the latest missed occurrence is queued. Pausing suppresses pending work; resuming starts a new scheduling interval.

Both inputs include `trigger_id` and `run_id`. Event inputs include `source_id` and `event`; timer inputs include UTC `scheduled_at`. Save the Python with an input schema that accepts these fields. Use `uses: []` for pure Python. Background dependencies must be authorized upstream tools marked read-only.

```python
def run(args, ctx):
    reading = args["event"]["data"]
    if reading["temperature"] < 30:
        return {"action": "skip", "reason": "temperature below threshold"}
    return {"action": "notify", "data": {"temperature": reading["temperature"]}}
```

A handler error records a failed run. A completed result and its delivery are committed together. Webhook retries send the saved body with the same idempotency key. The processing result is available through `get_trigger(trigger_id,run_id)`. An interrupted processing attempt may re-read its dependencies. A webhook already in flight can reach its destination after a pause.

## Automatic improvement

Set `replay: true` to retain encrypted input and read-response evidence. A completed handler then emits `bridge.builtin/trigger.run.completed` with identifiers and metrics. An ordinary event trigger targeting a maintenance webhook can attach `skill://bridge.builtin/improve-trigger/SKILL.md`. The external maintenance agent can inspect the run and save a new version of its handler. New triggers start with automatic promotion off.

To enable promotion, configure independent samples, expected outputs, and a metric on the trigger before proposing the candidate. For example, a temperature summarizer can require correct temperatures while reducing its output size.

```json
{
  "promotion": {
    "enabled": true,
    "metric": "output_bytes",
    "required_data_fields": ["temperature"],
    "holdout": [{
      "arguments": {"trigger_id":"hot","run_id":"independent","event":{"data":{"temperature":31}}},
      "expected": {"action":"notify","data":{"temperature":31}},
      "calls": []
    }]
  }
}
```

For `reads`, supply each expected upstream call as `{"tool_id":"source/name","arguments":{},"response":{}}`. Offline execution consumes at most the recorded occurrences and never calls upstream. Without required_data_fields, compare the full output; when specified, compare the action and those fields (skip decisions retain exact reason matching). All samples must pass with no metric regression and at least one strict improvement. The evaluator compares the latest saved version with the pinned one against up to 100 recent recorded runs and the configured independent samples. The maintenance interface omits holdout values. A stale trigger revision cannot promote. See the latest decision in `get_trigger`. To roll back and keep that version, patch the handler and set `promotion.enabled` to false in the same request.

## Upgrade

Restart the daemon and refresh client tool catalogs. Update external clients from save_event_flow/get_event_flow to save_trigger/list_triggers/get_trigger, from search_events to search_event_types and from bridge_publish_tool to bridge_save_tool. Event and timer definitions use `trigger_id` and `when`. Upgraded subscriptions retain their webhook payload fields, revisions, polling cursors and outstanding deliveries. The database upgrade is forward-only; use the pre-upgrade backup when reverting to an older binary.

## Sending events to a CLI

Start the daemon once with `mcp-bridge serve`. In separate Bash terminals run either of these commands. Each launcher prints its own destination ID.

```sh
mcp-bridge run -- codex
mcp-bridge run --delivery=auto -- opencode
```

Use `bridge_get_status` to see destination IDs, types and availability. In an MCP stdio child that inherits `MCP_BRIDGE_TERMINAL_TOKEN`, pass `destination_id: "current_terminal"` to `save_trigger`. Bridge resolves the token at connection time and stores the concrete ID. If a host filters its MCP subprocess environment, forward that variable through the host configuration or select the launcher's printed ID explicitly. An ordinary unbound stdio client or HTTP client can use a visible concrete ID. A terminal destination identifies one launcher lifetime; a fresh launch gets a new ID. Update the trigger’s destination and revision to direct future events to it.

The default inbox announces a pending event. Press Ctrl+] to preview it and press Ctrl+] again at an empty agent prompt to submit. In auto mode the launcher pastes the prompt and presses Enter as soon as it obtains the delivery. Use auto with a dedicated CLI at a prompt that accepts text and Enter. The launcher owns the child PTY and propagates terminal size and exit status. In Linux canonical input mode a prompt over 4095 bytes records `failed` before writing. Terminal submission records `submitted`; to verify that a model processed the event, inspect its actual answer. Pending deliveries survive a daemon restart and stay bound to their original destination. An interrupted submission with an uncertain outcome records `unknown` and does not replay automatically.

For a fresh noninteractive process per delivery, create `~/.mcp-bridge/cli-destinations.json` (or use your `-data` directory) and restart the daemon. Example for inspecting the exact input:

```json
[{"id":"echo-event","command":"/usr/bin/cat","args":[],"cwd":"/tmp","input_format":"json"}]
```

Select `echo-event` as the trigger's destination. Each preset runs deliveries serially using its fixed executable and arguments. Bridge writes one JSON notification or rendered prompt (set `input_format` to `prompt`) to stdin and closes it. Event fields are never interpolated into command arguments. Configure a noninteractive agent command in the preset for actual model processing. Each run has a five-minute deadline. Look up its run ID with `get_trigger`, then pass that ID as `run_id` to inspect `delivery_result.exit_code` when available and the first 8192 bytes from each output stream. A nonzero exit or timeout records `failed`.

Previously saved webhook destinations, trigger references, and pending webhook deliveries remain valid. Update callers of `bridge_save_webhook` and `bridge_delete_webhook` to `bridge_save_destination` and `bridge_delete_destination`. The webhook payload is unchanged; the optional destination `type` defaults to `webhook`. Terminal registrations and local executable presets are managed by their host.

Run `make check` for the offline acceptance tests. With authenticated Codex and OpenCode on PATH and a working model connection, run the live Bash/PTY test from the repository root:

```sh
make build
mkdir -p bin/trace
python3 tests/terminal_agents.py --output bin/trace/terminal-agents.json
```
