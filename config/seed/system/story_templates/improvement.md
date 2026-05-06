---
category: improvement
name: improvement
fields:
  - name: motivation
    type: text
    required: true
    prompt: |
      Why this change is worth making. Cite the friction, the metric, or
      the principle the current shape violates. Improvements without a
      clear motivation drift into churn.
  - name: before_after
    type: text
    required: false
    prompt: |
      A concise before/after sketch of the behaviour or shape. Required
      to close.
  - name: fix_commit
    type: string
    required: false
    prompt: |
      Commit that lands the improvement. Required to close.
  - name: regression_test_path
    type: string
    required: false
    prompt: |
      Path to the test that locks in the new behaviour so it doesn't
      regress. Required to close.
  - name: post_deploy_check
    type: text
    required: false
    prompt: |
      Optional — many improvements don't need post-deploy verification
      because they're behind no public surface. State "n/a" if so.
hooks:
  in_progress:
    structured:
      - { type: field_present, field: motivation }
    natural_language: []
  done:
    structured:
      - { type: field_present, field: before_after }
      - { type: field_present, field: fix_commit }
      - { type: field_present, field: regression_test_path }
    natural_language: []
tags:
  - story-template
  - category:improvement
---

# Improvement template

An improvement refines existing behaviour. The story captures the
motivation (so future readers can judge whether the trade-off still
applies), a before/after, and a regression test that locks in the new
shape.

Improvements often touch many files but should land as one coherent
change — if the motivation splits naturally into two, prefer two
stories.
