---
name: review-validation
description: Review QA replay coverage when an accepted candidate subsequently fails.
---
Inspect the recorded inputs, actual outputs, expected values and upstream read counts.
Find cases that distinguish a verified candidate's later failures from successful runs.
Propose additional test inputs only. Preserve the QA bot's three-field projection
contract, including independent false, absent, null and true fields, one upstream read
and propagation of upstream failure. Keep the existing tests. Evaluate the proposal
on unseen candidates and frozen independent inputs before activating it.
