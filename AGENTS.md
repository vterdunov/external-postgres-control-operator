# EPCO agent workflow

- A senior coordinator maintains the plan and assigns stages with explicit ownership.
- The coordinator chooses each subagent's model and effort by task complexity and risk.
- Do not use Astra for subagents or max/ultra effort.
- Use TDD: failing test, minimal implementation, refactor.
- Store English plans and progress in `.agent-plans/YYYY-MM-DD-topic.md` (gitignored).
- Keep handoffs short: changes, checks, blockers. Preserve others' work.
- Use mise; run focused checks and fix failures or flakiness.
- Never hand-edit generated files; preserve Kubebuilder scaffold markers.
- Keep runners, fixtures and support with their test suite.
- Use isolated test environments and explicit Kubernetes contexts.
- Finish with an independent validation agent.
