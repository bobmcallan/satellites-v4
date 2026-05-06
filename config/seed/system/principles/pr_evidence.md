---
id: pr_evidence
name: Evidence must be verifiable
scope: system
tags:
  - quality
  - evidence
---
Every claim in a submission must be backed by verifiable evidence. Declarative assertions -- "tests pass", "AC satisfied", "works correctly" -- are insufficient without concrete proof.

Verifiable evidence includes: test output with pass/fail counts, specific file:line references, commit SHAs, command output, grep results confirming wiring, git diff stats matching files_changed lists.

Before claiming an AC as satisfied, run the verification command and cite the result. Before claiming tests pass, run the tests and report the output. Before claiming a function is wired, grep for callers and cite the matches.

The reviewer cannot accept what they cannot verify. Make verification trivial by providing the evidence inline.
