---
name: git_merge_to_main
tags: [v4, lifecycle, releaser, workspace]
---
# Git Merge to Main (releaser skill)

Bash recipe the releaser_agent runs when dispatched on a task whose
action is `contract:merge_to_main`. The releaser fetches this
skill via `satellites-client skill get --name git_merge_to_main`,
then runs each step below **one command at a time** via the Bash
tool, capturing the literal stdout + stderr for the close evidence
row.

The recipe REFUSES when the work-branch tip already equals
`origin/main` (the merge has nothing to do; proceeding would push
a no-op to main and produce a false-success row — the
`merge-base` symmetry to the `ldg_a4e759a9` commit-phase
regression).

## When this skill applies

A task whose `action` is exactly `contract:merge_to_main`.

## Bash recipe

Run each command in order. After every command, capture stdout +
stderr verbatim. If any check fails, STOP and close the task
`outcome=failure` with the literal captured output in the evidence
row's content.

```bash
# 1. Refresh remote refs. Required before any SHA comparison —
#    a stale local view of origin/main is what makes the
#    branch-already-at-target case ambiguous.
git fetch origin

# 2. Refuse-on-tip-equality guard. If the work-branch tip already
#    equals origin/main, the merge is a no-op. STOP.
WORK_SHA=$(git rev-parse HEAD)
MAIN_SHA=$(git rev-parse origin/main)
if [ "$WORK_SHA" = "$MAIN_SHA" ]; then
  echo "work-branch tip == origin/main ($WORK_SHA) — refusing merge"
  exit 1
fi

# 3. Switch to main + fast-forward from origin.
git checkout main
git pull --ff-only origin main

# 4. Merge the work branch. --no-ff to preserve the branch shape
#    in history; trunk-based but with an explicit merge commit so
#    a future bisect can identify the story that landed each set
#    of changes.
git merge --no-ff <work_branch_name>

# 5. Publish main to origin. Non-force — main MUST be append-only
#    on the remote.
git push origin main

# 6. Post-merge verification. Re-read the tip so the evidence row
#    carries the SHA the remote actually accepted.
git log -1 --oneline main
```

## Success stdout shape

The executor declares `outcome=success` when (and only when) the
following shapes all appear:

```text
# step 1 — fetch landed:
From <remote>
   <range>  main       -> origin/main

# step 2 — guard passed (SHAs differ):
WORK_SHA != MAIN_SHA — proceeding
# (the SHA-equality refusal text is ABSENT)

# step 3 — fast-forward from origin succeeded:
Fast-forward
 N files changed, ...

# step 4 — merge commit landed (no conflict):
Merge made by the 'ort' strategy.
 N files changed, ...

# step 5 — push landed:
To <remote>
   <old>..<new>  main -> main

# step 6 — post-merge SHA captured:
abc1234 Merge branch 'client-task_<id>-from-<base>' (sty_<id>)
```

## Failure stdout shapes

Each row below is a documented refusal shape. The executor MUST
match against the literal substring on the left and close
`outcome=failure` with the literal captured output in the evidence
row content.

```text
# (a) branch-already-at-target — step 2 refusal (no merge happens):
work-branch tip == origin/main (<sha>) — refusing merge
# evidence content includes this literal SHA-equality line.
# git log -1 main MUST be unchanged before vs after the dispatch.

# (b) non-fast-forward on origin/main — someone else pushed:
 ! [rejected]  main -> main (non-fast-forward)
error: failed to push some refs to 'origin'

# (c) merge conflict:
Auto-merging path/to/file
CONFLICT (content): Merge conflict in path/to/file
Automatic merge failed; fix conflicts and then commit the result.
# the releaser does NOT auto-resolve. STOP, close outcome=failure,
# capture the literal CONFLICT block in evidence.

# (d) push rejected by remote hook (e.g. branch protection):
remote: error: GH006: Protected branch update failed for refs/heads/main.

# (e) pull --ff-only refusal — local main diverged from origin:
fatal: Not possible to fast-forward, aborting.
```

## Chain-shape gate

Before step 1, the releaser MUST verify the task chain shape via
`satellites-client task walk --story-id <story_id>`. Every closed
work task on the chain must be either `outcome=success` or have a
successor pointing at it via `prior_task_id`. A closed-failure
task with no successor is an unrepaired gap — STOP, close
`outcome=failure` with the offending task id, do NOT attempt the
merge.

(This is the substrate-side auto-supersession behaviour from
sty_9d046bc7: TaskAdd stamps `prior_task_id` on the auto-minted
successor, and this gate is what consumes the resulting linked
chain.)

## Evidence row template

On close, append one ledger row:

```
type=evidence
tags=task_id:<merge_task>,kind:release-evidence,phase:merge_to_main
content=<literal captured stdout of steps 1..6 — never a paraphrase>
```

On refuse / failure, the row content carries the literal failure-
shape output (the SHA-equality refusal line, the CONFLICT block,
the rejected-push remote response, etc.) AND a sentence naming
which step refused. No declarative summary stands in for the
literal capture.
