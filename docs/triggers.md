# Triggers

A trigger has one event or schedule, an optional versioned Python handler, and a webhook, terminal session, or CLI process destination. Discover events with `search_event_types` and destinations with `bridge_get_status`. Configure a webhook or CLI process destination with `bridge_save_destination`. A missing or ambiguous destination returns candidate IDs and does not save the trigger.

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

For a fresh noninteractive process per delivery, call `bridge_save_destination` over local stdio:

```json
{"destination":{"type":"cli_process","id":"echo-event","command":"/usr/bin/cat","args":[],"cwd":"/tmp","input_format":"json"},"revision":0}
```

Use revision 0 to create. To replace the configuration, pass the destination's current revision from `bridge_get_status` and the full definition. The next claimed delivery uses the saved configuration immediately, and the configuration survives daemon restarts. The command and working directory must be absolute paths.

Existing `cli-destinations.json` presets still load at startup. Saving the same ID with its current revision gives the saved configuration precedence over the file. To retire a file-defined ID, remove its file entry and restart before calling `bridge_delete_destination`. Terminal sessions are registered by their launcher.

Select `echo-event` as the trigger's destination. Each preset runs deliveries serially using its fixed executable and arguments. Bridge writes one JSON notification or rendered prompt (set `input_format` to `prompt`) to stdin and closes it. Event fields are never interpolated into command arguments. Configure a noninteractive agent command in the preset for actual model processing. Each run has a five-minute deadline. Look up its run ID with `get_trigger`, then pass that ID as `run_id` to inspect `delivery_result.exit_code` when available and the first 8192 bytes from each output stream. A nonzero exit or timeout records `failed`.

Previously saved webhook destinations, trigger references, and pending webhook deliveries remain valid. Update callers of `bridge_save_webhook` and `bridge_delete_webhook` to `bridge_save_destination` and `bridge_delete_destination`. The webhook payload is unchanged; the optional destination `type` defaults to `webhook`.

## Retiring a trigger and its destination

Each destination in `bridge_get_status` includes `trigger_count`. This counts all configured triggers, including paused ones. A zero count helps identify unused targets. Pending runs can still hold a destination selected before a configuration edit.

For a dedicated destination, pass `{"trigger_id":"daily-report","revision":3,"delete_unused_destination":true}` to `delete_trigger`. The trigger and eligible destination are removed together. Check `destination_cleanup` to see whether Bridge removed the target or kept it for another trigger, queued run, or in-flight delivery. Shared targets owned by another identity, file presets, and launcher-managed terminals are retained. CLI configuration changes require a local connection. Omitting the flag preserves the destination for reuse. Previously detached managed targets can be removed with `bridge_delete_destination` and their current revision after outstanding work finishes.

Deleted trigger instances retain their runs, receipts, and promotion reviews for 30 days from deletion. A minute-based sweep removes expired archives when their deliveries have finished. Existing archives get 30 days from the first upgrade with retention support. Recreating the same trigger ID starts a separate instance.

## Change a CLI target and verify one delivery

For an existing schedule named `daily-report` that uses the CLI destination `report`, make these four MCP calls over local stdio.

| Step | Tool | Arguments |
| --- | --- | --- |
| Read the effective command, arguments and revision | `bridge_get_status` | `{"destination_id":"report"}` |
| Change the receiving thread | `bridge_save_destination` | `{"destination_id":"report","revision":3,"patch":{"args":["/opt/report/send.py","--thread","B"]}}` |
| Send once now | `run_trigger_now` | `{"trigger_id":"daily-report","request_id":"report-to-b-001"}` |
| Verify that send | `get_trigger` | `{"trigger_id":"daily-report","view":"latest","run_id":"<returned run_id>","wait_ms":10000}` |

Use the revision and full argument list from the first response, changing only the intended argument. An array in a patch replaces the entire array. A stale revision returns a conflict. A destination edit takes effect for the next claimed delivery of every trigger using that destination.

An immediate run uses the enabled trigger's handler and destination and preserves its normal schedule or event cursor. For an event trigger, supply the ID of a stored event visible to the caller that matches the trigger. Set `dry_run` to `true` to inspect the input without executing the handler or sending. Reuse a request ID to retry the same send on the same trigger instance. Use a fresh request ID for a new send.

Omit `run_id` from the latest view to inspect the most recent run. An empty history returns `never_run`. A wait of up to 30000 ms returns the current state when it expires. A positive wait selects the compact view automatically. To continue waiting, pass the same run ID. A successful CLI process records `completed` and its exit code. A terminal records `submitted` when it submits the prompt, and a webhook records `accepted` when the receiver acknowledges it.

Run `make check` for the offline acceptance tests. To measure a real Codex turn against temporary CLI receivers A and B, run `python3 tests/trigger_intent.py --codex codex` after building Bridge. With authenticated Codex and OpenCode on PATH and a working model connection, run the live Bash/PTY test from the repository root:

```sh
make build
mkdir -p bin/trace
python3 tests/terminal_agents.py --output bin/trace/terminal-agents.json
```
