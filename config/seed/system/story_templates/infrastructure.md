---
category: infrastructure
name: infrastructure
fields:
  - name: scope
    type: text
    required: true
    prompt: |
      What boundary the infrastructure change crosses (schema, build,
      CI, deploy, observability). Reviewers use this to gate review
      depth — schema changes need migration sign-off, CI changes need
      ops sign-off.
  - name: rollout_plan
    type: text
    required: false
    prompt: |
      How the change ships safely. Migrations: order of operations,
      backfill strategy, feature gate. Build/CI: dry-run, canary, or
      direct. Required to close.
  - name: fix_commit
    type: string
    required: false
    prompt: |
      Commit that lands the infrastructure change. Required to close.
  - name: regression_test_path
    type: string
    required: false
    prompt: |
      Path to a test or smoke check that verifies the new infrastructure
      stays correct. Required to close.
  - name: post_deploy_check
    type: text
    required: false
    prompt: |
      A live verification (curl, portal_replicate, dashboard query) that
      confirms the change took effect on the deployed system. Required
      to close.
hooks:
  in_progress:
    structured:
      - { type: field_present, field: scope }
    natural_language:
      - The change is reversible OR the rollout plan covers compensating action.
  done:
    structured:
      - { type: field_present, field: rollout_plan }
      - { type: field_present, field: fix_commit }
      - { type: field_present, field: regression_test_path }
      - { type: field_present, field: post_deploy_check }
    natural_language:
      - The post-deploy check has run successfully against the live system.
tags:
  - story-template
  - category:infrastructure
---

# Infrastructure template

Infrastructure changes touch the substrate — schema, build, CI, deploy,
observability. They have a wider blast radius than feature work and
deserve more rigour on rollout and post-deploy verification.

The story carries the scope (which boundary), the rollout plan (how to
ship safely), the regression test (so we don't unwind the change
accidentally), and the post-deploy check (so we know it actually
landed).
