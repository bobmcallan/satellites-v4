---
category: feature
name: feature
fields:
  - name: user_story
    type: text
    required: true
    prompt: |
      Who needs this and why. One or two sentences in the form
      "As <role> I want <capability> so that <outcome>." Sets the scope
      ceiling — anything outside this is a separate story.
  - name: acceptance_demo
    type: text
    required: false
    prompt: |
      How a reviewer can see the feature working end-to-end. A
      portal_replicate sequence, a curl recipe, or a manual walkthrough.
      Required to close.
  - name: fix_commit
    type: string
    required: false
    prompt: |
      The commit (or merge commit) that lands the feature. Required to
      close.
  - name: regression_test_path
    type: string
    required: false
    prompt: |
      Path to the test(s) that exercise the new capability. Required to
      close.
  - name: post_deploy_check
    type: text
    required: false
    prompt: |
      How to confirm the feature works on the deployed app — a
      portal_replicate sequence or a manual check. Required to close.
hooks:
  in_progress:
    structured:
      - { type: field_present, field: user_story }
    natural_language:
      - The acceptance criteria are concrete enough that a reviewer can verify them.
  done:
    structured:
      - { type: field_present, field: acceptance_demo }
      - { type: field_present, field: fix_commit }
      - { type: field_present, field: regression_test_path }
      - { type: field_present, field: post_deploy_check }
    natural_language:
      - The acceptance demo runs successfully against the deployed app.
tags:
  - story-template
  - category:feature
---

# Feature template

A feature adds capability the system did not previously have. The story
captures who needs it, what "working" looks like to a reviewer, and the
test + post-deploy check that prevents the capability from silently
regressing.

Features differ from improvements in scope: a feature is a new behaviour,
an improvement refines an existing one. When in doubt, prefer
improvement.
