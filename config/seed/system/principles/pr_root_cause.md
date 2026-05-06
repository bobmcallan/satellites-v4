---
id: pr_root_cause
name: Root cause, not hack
scope: system
tags:
  - quality
  - debugging
---
When a problem surfaces -- failing test, stuck pipeline, false-positive reviewer, unexpected block -- invest the effort to find and fix the root cause. Do not apply a workaround that bypasses the symptom.

Hacks include: direct DB updates to unstick state, disabling gates for this session, hardcoding bypasses, marking work not_applicable to duck a check, anything labelled "TODO: temporary" without a tracked follow-up.

Root-cause fixes include: identifying the failing code path, fixing it so the class of input cannot recur, adding a regression test, building missing infrastructure properly rather than hand-editing state.

When blocked, describe the root cause in specific terms before proposing a fix. Surface the time-cost honestly and let the user decide.
