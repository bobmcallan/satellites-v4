❯ So, again this provres that Satellites is a confusing mess to agents.

  To break this down.

  use prompt -> 'Review config/seed/artifacts/default_agent_process.md and remove any repo references, like '/architecture-configuration-over-code-mandate.md' and the associated line'

  I asked how you would create and action an agent/reviewer. Using satellites the context is the most important fact, and the function is second.

  The last epic, was meant to create seed documents whicvh enabled a mult agent structure, using satellites as the common data source.

  Henc efor the prompt, you would simply

  1. locate the required / align agent markdown in satellites.
  2. create a task, for the agent, which has the prompt/context 'Review config/seed/artifacts/default_agent_process.md and remove any repo references, like
  '/architecture-configuration-over-code-mandate.md' and the associated line'
  3. execute bash(claude -p:'implement task_xxx')
  4. review the output from the task.

  Is that aligned? Does that make sense, or is that way off what is currently available?

● Yes, that's aligned with the design intent. The flow you described is what configuration-over-code should produce, and the user-facing surface should be exactly that small.

  Mapping your four steps to what's currently available:

  1. Locate / align the agent markdown. Available. agent_list / agent_search / agent_get give you the configured agent docs. This is pure configuration access — no friction.

  2. Create a task carrying the prompt as context. This is where the substrate currently leaks. The plan-as-DAG shape (task_submit(kind=plan, tasks=[...])) and the structural invariant "every work task has
   a paired review sibling" mean you can't mint a single task that says "agent X, do this prompt" — you have to mint a plan with a work task and a paired review task, with the right kind / action /
  parent_task_id / prior_task_id shape. That's the code-shaped leak the user is calling out. The principled flow is: one task, one agent_id, the prompt is the task's action/description, done. The reviewer
  (if any) falls out of the contract attached to the task, not from the operator's hand.

  3. bash(claude -p 'implement task_xxx'). Available and correct. Subprocess Claude session with the task id as its only entry point; the subprocess fetches the agent doc, principles, story, ledger, repo,
  and contract via MCP using its task id. This is the dispatch shape that's already been built.

  4. Review the output. Available — verdict either comes from a reviewer agent (if the contract specifies review) or from you reading the ledger rows the agent emitted under task_id:<id>.

  So: aligned in shape, friction is in step 2. The "just create a task" surface that your flow assumes is the shape the substrate should expose. Right now it forces you up to plan level for what should be
  a single-task action. That's the gap between the intent (configuration-over-code, agent-is-doc, task-is-entry-point) and the current invariant (task_submit rejects unless you've handed it a structurally
  complete plan).

  If your last epic's goal was seed documents enabling multi-agent structure, the missing piece isn't more configuration — it's removing the plan-shaped invariant from the single-task path. Or making the
  plan invariant invisible: when an operator writes one task, the substrate auto-pairs the review task from the contract's prose, and the operator never sees the pairing.

  Your description is not way off. It's the right shape. The current substrate is partway there.
