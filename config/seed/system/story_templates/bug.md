---
category: bug
name: bug
fields:
  - name: repro
    type: text
    required: true
    prompt: |
      A reproducible action sequence that demonstrates the bug. When the
      portal_replicate tool exists, this is a list of structured actions
      (navigate / click / wait / dom_assert / console_assert). Until then,
      a clear natural-language sequence works.
  - name: observed
    type: text
    required: true
    prompt: |
      What actually happens when the repro runs. Include error messages,
      screenshots references, or DOM/console captures attached as ledger
      evidence.
  - name: expected
    type: text
    required: true
    prompt: |
      What should happen. State the contract the bug violates.
  - name: root_cause
    type: text
    required: false
    prompt: |
      Filled at fix time. Brief — what was wrong and why. Reference the
      file/line that owned the bug.
  - name: fix_commit
    type: string
    required: false
    prompt: |
      Filled at fix time. The git commit hash that lands the fix. Used by
      the close hook to verify the fix is in main.
  - name: regression_test_path
    type: string
    required: false
    prompt: |
      Filled at fix time. Path to the test that would have caught this
      bug, e.g. internal/portal/nav_test.go:TestHamburgerOpens. Required
      to close.
  - name: post_deploy_check
    type: text
    required: false
    prompt: |
      A natural-language assertion (or portal_replicate sequence) that
      verifies the fix on the deployed app. Re-run periodically by the
      post-deploy probe runner. Required to close.
hooks:
  in_progress:
    structured:
      - { type: field_present, field: repro }
      - { type: field_present, field: observed }
      - { type: field_present, field: expected }
    natural_language:
      - The repro must reproduce the bug when run against the current build.
  done:
    structured:
      - { type: field_present, field: root_cause }
      - { type: field_present, field: fix_commit }
      - { type: field_present, field: regression_test_path }
      - { type: field_present, field: post_deploy_check }
    natural_language:
      - The repro, when re-run against the deployed app, must succeed (bug absent).
      - The regression test referenced in regression_test_path must exist and be wired into CI.
tags:
  - story-template
  - category:bug
---

# Bug template

A bug is a contract the system violates today. The story exists to (1) prove
the bug reproducibly, (2) identify the root cause, (3) land the smallest
change that restores the contract, (4) add a regression test that would
have caught it, and (5) verify the fix on the deployed app and going
forward.

When the bug-story enters `in_progress`, the repro must demonstrate the
bug live. When it enters `done`, the repro must demonstrate the fix —
attached as ledger evidence — and the regression test must be in CI.
