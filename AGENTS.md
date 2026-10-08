# Repository guidance

This is a public repository. Never commit secrets, credentials, private keys,
access tokens, API keys, or other sensitive information.

## Deferred findings in Linear

Complete required implementation, investigation, root-cause fixes, and validation;
fix regressions introduced by your changes. Tickets never replace this work, regardless of
difficulty, size, time spent, failed checks, or unfamiliar systems. Respect existing
authorization boundaries; report genuine access/approval blockers as unfinished work.

- Automatically capture substantial, evidenced, actionable bugs, reliability/performance
  problems, or maintenance obstacles found during authorized work only if the requested
  outcome remains correct, complete, and verified without fixing them. Explain their
  independent scope. Skip unrelated minor cleanup, preferences, speculation, and
  ticket-generating audits. Eligible findings need no per-ticket confirmation.
- Before creating or retrying a failed/timed-out create, search for the underlying problem
  and source task; inspect matches and reuse confirmed issues. Add only materially new evidence. Recurrence
  does not authorize repeated comments, reopening, or ownership/scheduling/priority/review-label
  changes. Report ambiguous creation outcomes or unavailable search without blind retries or claims of success.
- Create in **Flint Pay** (`FLI`), **Backlog**, with **agent-discovered** and **needs-triage**;
  leave assignee, delegate, cycle, due date, priority, and estimate unset. Attach an existing
  project only when clearly applicable. Filing authorizes no scheduling, delegation, or implementation.
- Use a specific problem title; include observed/expected behavior, evidence/reproduction,
  likely impact, repository/code references (prefer commit permalinks), source task/PR when
  available, why deferred, and acceptance criteria or an investigation next step. Separate
  facts from hypotheses; omit secrets and private customer data.
- Continue the original task. Final responses distinguish completed work, genuine blockers,
  and created/updated ticket links. If Linear or duplicate search is unavailable, finish
  authorized work and truthfully report unfiled or unconfirmed findings.
- **Agent findings**: unresolved issues with both labels. **Agent backlog**: all unresolved
  **agent-discovered** issues, including reviewed ones. Only an authorized review removes
  **needs-triage**; retain **agent-discovered**. Filing or automatic summaries are not review.
- On first Linear use each session, verify authenticated team access. Setup/login is per
  client/machine; guidance does not install or authenticate tools. No repository credentials
  or copying tokens between machines.
- Search, summarize, and group freely. Apply explicitly requested bulk metadata changes and
  report affected issues; resolve ambiguous targets before mutation. Closing, assigning,
  scheduling, or starting work requires the requested action or an applicable standing instruction.
