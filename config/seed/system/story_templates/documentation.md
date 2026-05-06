---
category: documentation
name: documentation
fields:
  - name: audience
    type: text
    required: true
    prompt: |
      Who reads this and what they're trying to do. Documentation
      without a clear audience drifts into "comprehensive" — which is
      useful to nobody.
  - name: artefact_path
    type: string
    required: false
    prompt: |
      Where the documentation lives — repo path, portal /help slug, or
      external URL. Required to close.
  - name: fix_commit
    type: string
    required: false
    prompt: |
      Commit (or merge) that lands the documentation. Required to close.
  - name: regression_test_path
    type: string
    required: false
    prompt: |
      Optional — if the documentation contains code samples or claims
      verifiable by a test, link it here. State "n/a" otherwise.
hooks:
  in_progress:
    structured:
      - { type: field_present, field: audience }
    natural_language: []
  done:
    structured:
      - { type: field_present, field: artefact_path }
      - { type: field_present, field: fix_commit }
    natural_language:
      - A reader matching the stated audience can follow the document end-to-end without external context.
tags:
  - story-template
  - category:documentation
---

# Documentation template

Documentation work captures intent and how-to in a form a future reader
can act on. The story names the audience explicitly so the writer keeps
scope honest, points at the artefact, and ships a commit.

Code samples in documentation should be exercised by a test where
practical so they don't decay; otherwise leave regression_test_path as
"n/a".
