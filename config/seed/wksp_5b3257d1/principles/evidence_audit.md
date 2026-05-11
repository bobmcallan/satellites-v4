---
id: pr_evidence_audit
name: evidence_audit
scope: workspace
tags:
  - quality
  - evidence
  - audit
  - delivery
---
# Evidence is the audit trail; never trade it for delivery speed

Every claim in a submission is backed by verifiable evidence. Declarative assertions — "tests pass", "AC satisfied", "works correctly" — are not enough on their own; the reviewer cannot accept what they cannot verify.

## What counts as evidence

Test output with pass/fail counts. Specific `file:line` references. Commit SHAs. Literal command output (push stdout, merge stdout, ls-remote rows). Grep results confirming wiring. `git diff --stat` matching the files-changed list. Substrate row ids (ledger entries, task ids, verdict ids) the next reader can fetch.

Before claiming an AC satisfied, run the verification command and cite the result inline. Before claiming tests pass, run them and paste the summary. Before claiming a function is wired, grep for callers and cite the matches.

## The lifecycle ceremony IS the audit

Pipeline-claim, plan, action_claim, evidence, close — one ledger row per phase per contract is not overhead. Each row is something a human can inspect to decide whether autonomous work is acceptable. Do not frame ledger density as a cost/benefit tradeoff against "trivial stories": when a story is genuinely small, pick a shorter `proposed_contracts` list (e.g. `plan → develop → story_close`) based on the *work shape*, not on reducing ledger noise. When critiquing the process, focus on what's missing from the audit chain, not on what could be cut. More process is expected, not less — the substrate is still growing.

## Correctness > throughput

When delivery pressure rises — context budget tight, reviewer blocking, user waiting — reject the temptation to ship something incorrect quickly. The cost of wrong delivery compounds; the cost of a delayed-but-correct delivery is one-off.

Evidence quality over throughput. A story_close that takes three rounds of reviewer feedback to provide complete evidence is better than a first-attempt submission that cuts corners. Correct means: ACs are actually satisfied, not rationalised. Tests exercise the real surface. Follow-ups are filed for gaps, not hidden in `not_applicable`. The review round-trip produces a genuine positive signal.

Before every delivery, ask: "is this what the story asked for, or is this what I could ship in time?" If the honest answer is the latter, stop. Describe the gap. Let the user decide the tradeoff.
