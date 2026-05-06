---
name: default
aliases:
  navigate: navigate
  go-to: navigate
  visit: navigate
  open: navigate
  load: navigate
  click: click
  tap: click
  press: click
  wait_visible: wait_visible
  wait: wait_visible
  await: wait_visible
  see: wait_visible
  dom_snapshot: dom_snapshot
  capture-html: dom_snapshot
  snapshot: dom_snapshot
  console_capture: console_capture
  console: console_capture
  capture-console: console_capture
  screenshot: screenshot
  screen-shot: screenshot
  shot: screenshot
  dom_visible: dom_visible
  expect-visible: dom_visible
  assert-visible: dom_visible
tags:
  - replicate
  - vocabulary
---

# Default replicate vocabulary

Maps natural-language action aliases to the canonical action types the
`portal_replicate` runner understands. Callers writing repro sequences
can use any alias listed here and the runner will resolve to the
canonical type before dispatch.

Adding a new alias is a single line edit here, then redeploy. The
runner does not validate aliases at write time — unknown ones simply
fall through to the canonical-name match (so `navigate` works whether
or not it appears as a key).

## Canonical action reference

| Canonical            | Purpose                                                  |
|----------------------|----------------------------------------------------------|
| `navigate`           | Load a URL.                                              |
| `wait_visible`       | Block until selector resolves to a visible element.      |
| `click`              | Click a selector.                                        |
| `dom_snapshot`       | Capture HTML of selector (or document).                  |
| `console_capture`    | Drain accumulated console messages into the result.      |
| `screenshot`         | PNG of the viewport (raw bytes; ledger base64-encodes).  |
| `dom_visible`        | One-shot visibility assert (500 ms timeout).             |

## Authoring a repro

A repro is just a JSON array of `{type, selector?, value?, label?}`
objects. The replicate handler resolves each `type` through the
vocabulary, runs them in sequence, and ledgers a Result per action onto
the story id you passed in.
