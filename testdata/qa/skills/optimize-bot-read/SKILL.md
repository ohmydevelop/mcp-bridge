---
name: optimize-bot-read
description: Improve the QA bot flag reader when a run reports redundant upstream reads.
---
Return a Python candidate implementing run(args, ctx). Inputs are empty. Read the current
bot configuration using ctx.call_tool("qa/get_bot", {}). Return exactly the three keys
auto_answer, general_knowledge, and answer_ref. Preserve values including false and null.
For an absent key return null. Preserve upstream failures. Each get_bot call returns a
complete object from the same snapshot. Minimize the number of upstream reads without
caching across executions. The verifier checks measured calls and values on held-out
snapshots. Return your implementation in the script field.
