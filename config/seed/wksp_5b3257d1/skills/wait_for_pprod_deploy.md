---
name: wait_for_pprod_deploy
tags: [v4, lifecycle, releaser, deploy, workspace]
---
# Wait for pprod Deploy (releaser skill)

Bash recipe the releaser_agent runs when dispatched on a task whose
action is `contract:deploy`. The releaser fetches this skill via
`satellites-client skill get --name wait_for_pprod_deploy`, then
runs each step below **one command at a time** via the Bash tool,
capturing the literal stdout + stderr for the close evidence row.

The recipe is the post-`merge_to_main` execution-shape phase that
attests pprod is actually serving the merged HEAD before the
orchestrator advances to `story_close`. Without this phase the
chain ships a story as "done" while pprod is still on the
prior release — exactly the gap captured at `ldg_40eee079`
(sty_72e36256 iter-1 AC7 dogfood failure).

## When this skill applies

A task whose `action` is exactly `contract:deploy`.

## Bash recipe

Run each command in order. Capture stdout + stderr verbatim. On
TIMEOUT or endpoint-error STOP and close the task `outcome=failure`
with the literal captured output in the evidence row's content.

```bash
# 1. Read the merged-HEAD SHA. The deploy task runs after
#    contract:merge_to_main closed, so origin/main is authoritative.
git fetch origin
MERGED_SHA=$(git rev-parse origin/main)

# 2. Resolve the pprod base URL. Default to the published pprod
#    server; override via SATELLITES_PPROD_URL (no other env knob).
PPROD_URL="${SATELLITES_PPROD_URL:-https://satellites-pprod.fly.dev}"

# 3. Resolve the bounded-wait knobs. Defaults: 30s cadence,
#    30min deadline. Both env-overridable; neither has a CLI
#    flag (pr_no_unrequested_compat — no caller-side bypass).
INTERVAL="${WAIT_INTERVAL_SEC:-30}"
DEADLINE_SEC=$(( $(date +%s) + ${WAIT_TIMEOUT_SEC:-1800} ))
POLL_COUNT=0
START_TS=$(date +%s)

# 4. Poll loop. Curl /api/v1/system/version on pprod; jq the
#    .commit field; prefix-match against MERGED_SHA. On match,
#    print the MATCH line and exit 0. On bounded-wait
#    exhaustion, print TIMEOUT and exit 1.
while [ $(date +%s) -lt $DEADLINE_SEC ]; do
  POLL_COUNT=$((POLL_COUNT + 1))
  RESP=$(curl -fsS -X POST -H 'Content-Type: application/json' -d '{}' \
    "${PPROD_URL}/api/v1/system/version" 2>&1) \
    || RESP="(curl exit $?: $RESP)"
  PPROD_SHA=$(printf '%s' "$RESP" | jq -r '.commit // ""' 2>/dev/null)
  if [ -n "$PPROD_SHA" ] && [ "${MERGED_SHA:0:${#PPROD_SHA}}" = "$PPROD_SHA" ]; then
    echo "MATCH merged=$MERGED_SHA pprod=$PPROD_SHA poll_count=$POLL_COUNT elapsed=$(( $(date +%s) - START_TS ))s"
    exit 0
  fi
  echo "poll=$POLL_COUNT merged=$MERGED_SHA pprod=${PPROD_SHA:-<none>} resp=$RESP"
  sleep $INTERVAL
done

# 5. Bounded-wait exhausted. Fail-loud with the literal last-polled
#    response. No retry-with-extended-deadline bypass; a real
#    timeout is a real failure.
echo "TIMEOUT merged=$MERGED_SHA last_pprod=${PPROD_SHA:-<none>} last_resp=$RESP poll_count=$POLL_COUNT elapsed=$(( $(date +%s) - START_TS ))s"
exit 1
```

## Success stdout shape

The executor declares `outcome=success` when (and only when) the
final stdout line matches:

```text
MATCH merged=<sha> pprod=<sha> poll_count=<n> elapsed=<n>s
```

The MATCH line is the audit-of-record. The intermediate
`poll=N merged=… pprod=… resp=…` lines preserve the poll trail
in the captured stdout.

## Failure stdout shapes

Each row below is a documented refusal shape. The executor MUST
match against the literal substring on the left and close
`outcome=failure` with the literal captured output (not a
paraphrase) in the evidence row content.

```text
# (a) bounded-wait exhausted — pprod never reported the merged SHA:
TIMEOUT merged=<sha> last_pprod=<sha-or-<none>> last_resp=<body> poll_count=<n> elapsed=<n>s

# (b) endpoint-error — curl could not reach the pprod /api/v1/system/version
#     route (DNS / TLS / 5xx / connection refused). The poll-line carries
#     "(curl exit <n>: <stderr>)" verbatim; the bounded wait continues so
#     a transient DNS hiccup does not fail the deploy permanently.
poll=<n> merged=<sha> pprod=<none> resp=(curl exit 6: Could not resolve host)
poll=<n> merged=<sha> pprod=<none> resp=(curl exit 22: HTTP/1.1 503 Service Unavailable)
# If every poll inside the deadline carries an endpoint-error response,
# the final TIMEOUT line names the last endpoint-error body as last_resp.
```

`endpoint-error` is named explicitly because the dispatched
operator may interpret a sustained 5xx / DNS failure as a
deploy-pipeline incident rather than a converge wait — both
shapes terminate via the TIMEOUT path; the difference lives in
the captured `last_resp` substring.

## Refuse-on-empty-merge guard

Step 1 reads `origin/main`. If `git rev-parse origin/main` returns
empty or fails (no merge_to_main on the chain), STOP and close
`outcome=failure` — there is nothing to wait for. The substrate's
chain-shape gate refuses this case at the orchestrator level
(deploy is minted only after merge_to_main closes), so the guard
exists as defence-in-depth.

## Evidence row template

On close, append one ledger row:

```
type=evidence
tags=task_id:<deploy_task>,kind:deploy-evidence,phase:deploy
content=<literal captured stdout — never a paraphrase>
```

The row carries:

- The literal `MATCH …` or `TIMEOUT …` line (audit-of-record).
- Structured fields parsable from the literal: `merged_sha`,
  `pprod_sha`, `poll_count`, `total_elapsed_seconds`, `timestamp`.
- The full poll trail (every intermediate `poll=N …` line) so a
  future reader can reconstruct the converge progression.

On refuse / failure (TIMEOUT or refuse-on-empty-merge), the row
content carries the literal failure-shape output AND a sentence
naming which step refused. No declarative summary stands in for
the literal capture.
