---
name: improve-trigger
description: Use recorded trigger executions to propose a better Python handler and check its independent replay decision.
---

Receive a bridge.builtin trigger.run.completed notification at the maintenance webhook. Read its trigger_id and run_id with get_trigger. Inspect the pinned handler with bridge_get_tool. Use the recorded inputs, calls, result and metric to identify a concrete improvement. If there is no evidence for a change, leave the handler in place.

Keep the run(args, ctx) contract, notify/skip decision, required business fields and declared dependencies. Save a candidate with bridge_save_tool using the current definition revision. The trigger stays pinned while the Bridge checks recorded and independent inputs. Inspect get_trigger for the review decision and the new pinned revision. Leave the trigger's holdout, scoring policy and revision selection to its owner and the verifier.
