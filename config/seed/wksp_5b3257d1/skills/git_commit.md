---
name: git_commit
tags: [v4, lifecycle, releaser, workspace]
---
# Git Commit (releaser skill)

Bash recipe the releaser_agent runs when dispatched on a task whose
action is `contract:commit`. The releaser fetches this skill via
`satellites-client skill get --name git_commit` at the start of the
task, then runs each step below **one command at a time** via the
Bash tool, capturing the literal stdout + stderr for the close
evidence row.

The recipe REFUSES on a clean worktree. A `git push` against a
branch whose HEAD equals its base SHA returns exit 0 with body
`Everything up-to-date` — this is exactly the false-success bug
captured by `ldg_a4e759a9` (sty_517a7db3 commit phase). The
empty-diff guard at step 1 is the structural defence.

## When this skill applies

A task whose `action` is exactly `contract:commit`.

## Bash recipe

Run each command in order. After every command, capture stdout +
stderr verbatim. If any check fails, STOP and close the task
`outcome=failure` with the literal captured output in the evidence
row's content. Do not improvise around a failure shape — refuse
the recipe and surface the literal.

```bash
# 1. Empty-diff guard. A clean worktree means the develop phase
#    closed without staging code — refuse the commit so the
#    chain-shape gate detects the gap rather than producing a
#    false-success push. Failure mode: literal empty stdout.
git status --porcelain
# → if the output is empty (zero bytes), STOP. Close
#   outcome=failure with the literal empty `$ git status
#   --porcelain` block in the evidence row content. Do NOT run
#   `git add`, `git commit`, or `git push`.

# 2. .version bump. Per pr_cli_primary / sty_f8d0157f, every
#    touched binary requires its `.version` line bumped before
#    commit. Confirm the bump exists in the staged diff:
git diff --cached -- .version
# → expected: ≥1 hunk per touched binary
#   ([satellites-server] / [satellites-client] / [satellites-agent]).
#   Missing bump = STOP. Close outcome=failure citing the
#   missing-bump rule.

# 3. Stage files explicitly. Never `git add .` or `git add -A` —
#    those swallow stray untracked files (worktree noise,
#    operator scratch). Stage the manifest the develop close
#    evidence row enumerated.
git add <file1> <file2> ...

# 4. Conventional-commit message. Subject:
#      <type>(<scope>): <subject>
#    e.g. `feat(substrate): delete hot-runners; bash recipes live in type=skill (sty_447b9fe0)`.
git commit -m "<type>(<scope>): <subject>"

# 5. Publish. --force-with-lease refuses to overwrite a remote tip
#    the local clone hasn't seen — protects against parallel work
#    on the same task branch. Never bare --force.
git push --force-with-lease
```

## Success stdout shape

The executor declares `outcome=success` when (and only when) the
following four shapes all appear in the captured output:

```text
# step 1 — non-empty:
$ git status --porcelain
 M path/to/changed_file
?? path/to/new_file
# (≥1 line)

# step 2 — .version diff non-empty:
$ git diff --cached -- .version
diff --git a/.version b/.version
@@ ...
-[satellites-client] 0.0.273
+[satellites-client] 0.0.274
# (≥1 hunk)

# step 4 — commit landed:
[client-task_<id>-from-<base> abc1234] feat(<scope>): <subject>
 N files changed, M insertions(+), K deletions(-)

# step 5 — push landed:
To <remote>
   <old>..<new>  client-task_<id>-from-<base> -> client-task_<id>-from-<base>
```

## Failure stdout shapes

Each row below is a documented refusal shape. The executor MUST
match against the literal substring on the left and close
`outcome=failure` with the literal captured output (not a
paraphrase) in the evidence row content.

```text
# (a) empty-diff (the ldg_a4e759a9 regression) — literal empty
#     line below the prompt. Refuse step 1; do NOT push.
$ git status --porcelain
<empty>

# (b) missing .version bump — step 2 returns empty.
$ git diff --cached -- .version
<empty>

# (c) push rejected (non-fast-forward, remote moved):
 ! [rejected]  client-... (fetch first)
error: failed to push some refs to 'origin'
hint: Updates were rejected because the remote contains work that
hint: you do not have locally.

# (d) push rejected (--force-with-lease lease invalidated):
 ! [rejected]  client-... (stale info)
error: failed to push some refs to 'origin'

# (e) commit with empty message (substrate misuse):
Aborting commit due to empty commit message.

# (f) `git push` reports "Everything up-to-date":
#     This MUST NOT appear in a success path. If it does, the
#     empty-diff guard at step 1 missed — close outcome=failure
#     and append a kind:audit-anomaly row citing the gap.
Everything up-to-date
```

## Evidence row template

On close, append one ledger row:

```
type=evidence
tags=task_id:<commit_task>,kind:evidence,phase:commit
content=<literal captured stdout of steps 1..5 — never a paraphrase>
```

On refuse / failure, the row content carries the literal failure-
shape output AND a sentence naming which step refused. No
declarative summary stands in for the literal capture.
