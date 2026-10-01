# Skopos agent pipeline — design review

Sep 30, 2026 · review of `Skopos agent pipeline — design.md` (Sep 25, 2026)

**Verdict: the architecture is sound and the trial already earned its keep — the concerns below are about the document contradicting its own hard rules, one missing safety rule (base drift), and Phase 1 being a cliff that has a thinner first slice available.**

## What's genuinely good

- **The core invariant is right.** Binding every approval to an immutable subject — a plan revision hash, a head SHA — is the Terraform plan/apply pattern, and it's the correct one. It also fits the codebase: "plan-item text immutable" is a locked decision from the audit-log work, `plan_revisions` extends the existing plans package naturally, and stale-review dismissal on new commits matches GitHub semantics. The compatibility carve-out (interactive plans keep working, no revisions, no gate) is the right migration posture.
- **The trial was a real de-risk, not theater.** It ran the full loop including a session-resumed change request, and it surfaced practical findings (list-call excerpts, worktree vendor via APFS clone) that no amount of design would have. At ~$1.50/item measured, the "usage acceptable" metric is essentially pre-answered; the live questions are plan quality and review time.
- **The Gitea air-gap is isolation by construction.** No GitHub remote, no GitHub credentials anywhere on the agent side — R7 fails safe even if every other layer fails. That's better than any policy-based control.
- **The push-safety section is honest.** Grounding the layers in the actual Kiro incident, admitting "any process using Martin's SSH key can push straight to PROD today," and recommending required-PR-into-PROD independently of this project — that last item is the single highest-value sentence in the doc.
- **The codebase fits** (verified against main, 2026-09-30): `events.Event` (`internal/events/hub.go:19`) is `Type`+`Workspace` only, so adding `entity`/`id` is exactly as designed; the hub's keyed per-API-key subscription with workspace filtering means workers over SSE slot into the existing auth model with scoped keys; inbox already has `Tags` and a Go-validated `Status` enum, so new workflow statuses are free; and all Track B tables (`item_events`, `plan_revisions`, `approvals`, `runs`) are additive, which works with the `IF NOT EXISTS` schema.sql mechanism.

## Substantive concerns

### 1. The doc contradicts its own hard rules

The summary and R7 say "agents never touch GitHub" and list "PRs on GitHub" as out of scope — but the 2026-09-29 decisions have agents pushing `WR{n}` branches to GitHub via the App and running Copilot review through a GitHub draft PR. The decision log supersedes the requirements table, but the table wasn't updated, and this isn't cosmetic: the enforcement story is completely different (Gitea air-gap vs. GitHub App + org ruleset, which per the doc's own notes may not exist yet and costs GitHub Pro on private repos).

**Fix:** rewrite R7 as track-scoped, and make layer 0 a hard Phase-1 dependency for any GitHub push. The corollary worth stating explicitly: layers 2–3 are advisory against a capable agent — the pre-push hook and gitconfig live inside a worktree the agent can write to — so layer 0 is the only enforcement, which makes it non-negotiable.

### 2. Base drift is unspecified

The approval binds to a plan revision, and the run records `base_sha`, but nothing says what happens when `base_ref` moves between approval and implementation. The trial can't see this (PROD frozen at `d2ce73d`), but on GitHub in Track B it's guaranteed to happen, and the current spec silently implements the approved plan on a moving base — which violates the doc's own principle that an approval covers exactly what will be executed.

**Fix:** bake the expected base into the revision and either invalidate or re-flag the approval on drift.

### 3. `item_events` is the audit log

It duplicates the active "Persisted audit log for mutations" plan (`01a0bf7c`) — same append-only shape, actor/via/notes/timestamp. If both get built separately there will be two parallel mutation logs in one schema. Fold them: the audit log is the generic mechanism, `item_events` is its first and most demanding consumer.

### 4. Phase 1 is the cliff, and there's a thinner slice

Phase 1 is essentially all the hard schema and authz work (statuses, approver permission, item_events, revisions/locking, approvals, runs with leases, policy hook, event ids) plus phantom P1–P7; phases 2–5 are additive by comparison. Since the trial's tags map one-to-one onto Track B statuses anyway, consider landing statuses + item_events + approvals first and pointing `agent-trial` at those APIs instead of tags — skopos absorbs the workflow while the proven executor keeps running, and the migration becomes gradual rather than a cutover.

One authz note for this slice: `approver` is a new per-action axis on top of today's root/scoped + workspace-ACL model, and the Slack module becomes a system principal that asserts Martin from a user-id check — the right shape, but it concentrates the whole gate in "Martin's Slack account isn't compromised." One threat-model sentence covering that (blast radius: `agent/*` branches plus WR creation via Fortix MCP — the design's only write to a production system outside git hosting) would complete it.

### 5. Resume is two different things

Quota resume = same Claude session (machine-local session files); lease-expiry/worker-move resume = fresh context from the pushed branch. Both are specified but conflated under "resume" — the run model should distinguish them (`resume_kind: session|branch`), because they have very different success characteristics and the metrics should separate them.

Related: this Mac sleeps. A sleep mid-run kills the heartbeat, expires the lease, and re-queues — that's a *normal event* on a laptop, not a failure, and if it's counted as one it will poison the ≤25% failure threshold.

## Smaller notes

- 10–15 items makes every percentage threshold ±1 item ≈ 7–8 points. Fine as a directional gut-check; don't let "43% vs 50%" decide anything.
- The frozen trial base ages — "would push as-is" is measured against `d2ce73d` while real PROD moves. Keep trial items small, or use the mid-trial resync already allowed for in the doc.
- Layer 1's refspec check is hardcoded `^refs/heads/agent/…` in the layers table but per-workspace patterns (`WR\d+-…`) in the prose — make the validator data-driven from the same config the server-side ruleset derives from.
- The planner's "read-only" toolset includes the full skopos MCP server, which is read-write (blackboard, plans). Claude's `--allowedTools` can allowlist per MCP tool — restrict it, since the tool mediates plan writes through its own validated path anyway.
- Long-lived `claude setup-token` credentials baked into knot templates is sprawl across ephemeral spaces; prefer per-space minting or rotation.
- Planner-suggested tags mean an LLM picks the execution environment. Bounded (disposable spaces), but show required tags in the approval-time routing preview so a mis-pick is visible at the gate.
- Worktree/branch/plan GC after done/reject isn't specified anywhere — per-item worktrees with vendored deps grow fast.
- Nice free win: the trial could use inbox `#N` addressing (v0.8.0) in Slack commands instead of fuzzy titles.

## Bottom line

Build direction is right, the trial validated the expensive assumptions, and the safety layering is more honest than most production designs. Before any Track B code: reconcile the 09-29 scope growth with R7, decide the base-drift policy, unify `item_events` with the audit-log plan, and split Phase 1 so the workflow lands under the still-running trial.

These findings were saved to the blackboard (entry `01a0f190`). After the design revision of 2026-09-30 all five concerns were resolved; the entry was deleted and replaced by round 2's findings (`01a0f1a2`).

---

# Round 2 (2026-09-30): the update holds up — all five concerns properly fixed, one new contradiction of its own making

Review of the revised `Skopos agent pipeline — design.md`. **Verdict: every round-1 concern is addressed, most with better mechanisms than suggested. The new material (base-drift handling, workspace groups, MCP per agent) is sound and its load-bearing claims were verified against the code. One new internal contradiction slipped in — the rebase stage vs. the no-force push rule — plus a handful of propagation gaps of the same class as round 1's R7, just smaller.**

## Round-1 scorecard

- **R7/scope contradiction** — fully resolved: track-scoped R7, summary rewritten, "PRs on GitHub" removed from out-of-scope, and the App+ruleset is now a hard prerequisite in the phase 1b row. ✅
- **Base drift** — solved better than suggested. Pinning the implement run to the revision's `base_sha` makes "what runs is exactly what was approved" literally true, drift is surfaced where it's visible (review time, with a behind-count), the rebase result is covered by the head-commit-tied review gate, and the 3-day staleness warning at approval is a nice touch. ✅ (but see the new issue below)
- **`item_events`** — now the first consumer of the audit-log plan `01a0bf7c`. ✅
- **Phase cliff** — split into 1a/1b along the suggested line, with the trial running through 1a. ✅
- **Resume/sleep** — `resume_kind`, `interrupted` excluded from the failure threshold, `caffeinate` implemented, and the structural fix: always-on Linux VM workers. ✅ All the smaller notes landed too (data-driven push validation, minted logins, tags in the routing preview, cleanup, directional stats).

## New finding: the rebase stage contradicts layer 1's no-force rule

The base-drift section adds a rebase stage whose result is "a new head commit" — on the same branch whose pre-rebase history was already pushed per-step during implementation. Pushing a rebased branch is a non-fast-forward push, i.e. force. Layer 1 explicitly says the worker "never uses force," and layer 0's org ruleset must then also permit force-pushes on allowed branch patterns. Fix it one of two ways and update **both** the base-drift section and the layers table:

1. a narrow written exception (`--force-with-lease`, only to the run's own branch, only from the rebase stage), or
2. merge-`base_ref`-into-branch instead of rebase — no history rewrite, no force, and it keeps the property that matters (Martin reviews the final state).

This needs deciding before 1b, since it touches the push path and the GitHub ruleset.

## Propagation gaps (the R7 failure mode again, smaller)

1. **R12 and "MCP per deployed agent" tell two different deny-list defaults.** R12 denies workflow-state tools + Fortix writes + shared prompt/skill stores; the later section says the server-side `approver` permission makes workflow denial unnecessary, so the default blocks only Fortix writes. The reconciliation exists (the trial-exception paragraph) but R12's cell wasn't updated. Also "shared prompt and skill stores" appears exactly once and is never defined; name the servers. Verified in the code's favor: `execute_tool` is real (`cmd/mcp/lean_tools.go` — the hidden meta-tool that can still reach inbox/plans tools), so R12's concern is correctly named.
2. **The phasing table omits workspace groups from 1a**, though the groups section says they belong there with the approver permission.
3. **The decision log has no 2026-09-30 rows** — base_sha-in-approval, item_events-as-audit-consumer, the phase split, Linux VMs, groups-in-1a, and minted logins are all decisions made that day. The log is the doc's memory; if it's allowed to go stale, the next review round (or implementation session) re-litigates.

## The 1a interim approver key

In 1a, `agent-trial` still mediates Slack approvals and must call `inbox_approve` with an approver-bearing key — while the doc's exclusivity claim is "agent and worker keys never get" approver. The threat-model section covers the phase-2 Slack principal but not this bridge. Name it: a dedicated agent-trial key with `approver`, treated as Martin-equivalent, explicitly retired when the Slack module lands in phase 2. Otherwise the permission model has an undocumented hole for the entire 1a period.

## Workspace groups — verified feasible, two shape notes

The riskiest claims check out: `Authenticate` does a fresh `LookupKey` per request (`internal/auth/principal.go:104`, and the apikeys service is explicitly "not cached"), so "no cache to clear" is true; `DropKey` exists (`internal/events/hub.go:94`). Two things the doc should pin down:

- **Drops on group change aren't automatic.** "Streams check scope when they connect" and "a membership change drops the affected keys' streams" are different mechanisms — the second needs explicit wiring from group/key mutations to affected key IDs → `DropKey`. One sentence to spell out the trigger.
- **"Patterns cover workspaces that don't exist yet" implies a bigger change than it sounds.** Today `Principal.CanAccess` is exact set-membership over a resolved workspace map. To match patterns for not-yet-existing workspaces you must carry patterns on the Principal and glob-match in every scope consumer (unscoped reads, the SSE `canAccess` closures, dashboard pickers). The alternative — resolve groups to members inside `LookupKey` — keeps `Principal` and every check unchanged, and still picks up newly registered workspaces on the next request (auto-registration inserts before access checks matter). The only thing genuinely lost is access to workspace IDs that were never registered, which have no data. Recommendation: take the second; either way, the doc should name the choice. Also define `*` vs `**` semantics, and add a reverse query ("which keys can reach workspace X") — forward-only `whoami` isn't enough to audit blast radius once patterns exist.

## Small residuals

- Quota→runner-unavailability ("mark claude unavailable, route to opencode-local") survived the phasing reshuffle in prose but the table doesn't say where it lands — presumably 1b with routing; say so.
- Between 1b and phase 3 there's no rebase stage yet — a conflicted handover should fall to `blocked` with a reason; one line.

## Bottom line

Implementation-ready for 1a modulo the hygiene items (deny-list reconciliation, approver bridge, table/log propagation). The rebase-vs-force conflict is the only genuinely open design decision, and it must be settled before 1b because it reaches into the push path and the GitHub ruleset. Round-2 findings were saved to the blackboard (entry `01a0f1a2`). After the second design revision all of them were resolved; the entry was deleted and replaced by round 3's findings (`01a0f226`).

---

# Round 3 (2026-09-30): all round-2 findings resolved; the document is ready for 1a — what's left is strata staleness

Review of the second revision. **Verdict: all seven round-2 findings are properly closed, most by taking the recommended option. No open design decisions remain for 1a or 1b. The remaining issues are all one class, now on its third occurrence: older sections of the document that later revisions didn't reach — three instances — plus one naming nit.**

## Round-2 scorecard

1. **Rebase vs. no-force** — resolved by merging `base_ref` into the branch instead of rebasing (the recommended option): an ordinary new commit, no history rewrite, no force push. Propagated everywhere it matters — layers table ("never force-pushes"; layer 0 ruleset now says "no force pushes"), phase 3 row, the interim `blocked` behavior between 1b and phase 3, and a decision-log row. ✅
2. **R12 vs. MCP-section deny-list tension** — R12 rewritten to match: workflow integrity comes from the server-side `approver` permission, not from denying tools; the default deny list now names concrete tools (llmrouter `create_work_request`, `reply_to_work_request`, `create_client_from_email`, `request_file_upload`; scriptling `set_prompt`, `set_skill`, `qdrant_upsert`); the MCP section cross-references "as in R12"; decision logged. "Shared prompt and skill stores" is now defined. ✅
3. **Interim approver key** — exactly as suggested: a dedicated agent-trial key with `approver`, Martin-equivalent, used only by the Slack command handler, never passed to an agent session, revoked when the Slack module lands in phase 2 — documented in the threat model, both phase rows, and the decision log. ✅
4. **Workspace groups shape** — took the simpler route: resolved inside `LookupKey`, so `Principal` and every `CanAccess` check stay unchanged (this matches what was verified in code — a fresh, uncached lookup per request). Pattern semantics defined (`path.Match`, `*` doesn't cross `/`, no `**`); stream drops wired explicitly with three concrete triggers (including the workspace-auto-registration case); `skopos key who-can <workspace>` reverse query added. ✅
5. **Phase table omissions** — groups in 1a, quota-runner fallback in 1b, conflicted-handover-goes-to-`blocked` in 1b. ✅
6. **Decision log** — nine 2026-09-30 rows, covering everything decided that day. ✅
7. **Small residuals** — all covered. ✅

Improvements beyond the ask: layer 2 is now honestly labeled "Advisory: the agent can write to these files"; layer 3 leans on credential absence ("with no credentials, the agent has nothing to authenticate a push with") rather than tool denial alone; layer 0 was renamed "server" so it covers Gitea (trial) and GitHub (Track B) in one row; layer 1's refspec example is now generic with per-workspace patterns "generated from the same config as the server ruleset."

## New finding: strata staleness (third occurrence of the same pattern)

The document is now three strata deep — original body, "Changes from the design review," "API key groups and MCP per agent" — and each revision updates the newest authoritative text but leaves older mentions behind. Three instances this round:

1. **The "Smaller changes" bullet on the MCP deny list (~line 399) still tells the round-2 story:** deny rules for workflow-state tools and "shared prompt and skill stores." That contradicts revised R12 ("workflow integrity … not from denying tools"; "shared scriptling stores" with named tools) and the decision log.
2. **The alternatives section (~line 488) still claims the differentiator is "pushes restricted to `agent/*` with no PR"** — contradicting R7's Track B draft PRs for Copilot review (and the trial's Gitea PRs). Presumably the intended meaning is "agents never need a PR to push branches, and never merge," but as written it contradicts the design's own hard rules.
3. **Minor:** the trial config keeps the `ssh://` Gitea remote while layer 3 now says "SSH git disabled." One sentence on the trial's push transport (the tool pushes over HTTPS with the bot token) would remove the ambiguity.

**Recommendation:** before implementation starts, do one consolidation pass — fold the two delta sections into the body they modify (or mark them explicitly as historical), since the decision log already serves as history. Delta sections that linger go stale exactly this way, and each review round has spent effort finding that class of bug.

## Naming nit

The drift-resolution stage is called "`merge-base`" — but in git, *merge base* is the common ancestor (`git merge-base A B` prints a SHA); the stage actually **merges `base_ref` into the branch**. An implementer scripting from the name alone would run the wrong command. Suggest `sync-base` or `merge-base-in`.

## Bottom line

This revision closes everything substantive. 1a is implementation-ready as written — the groups design matches the code's actual auth shape (verified: fresh `LookupKey` per request, `Principal` untouched by the chosen resolution), and the approver/audit/revision pieces are fully specified. Nothing structural is outstanding for 1b either; the merge-not-rebase decision resolved the last open design question. The only work left on the document itself is the consolidation pass. Process observation: every round's fixes have been faithful, but each round has left one-to-three stale mentions behind — consolidating now ends that pattern while the document still fits in one head. Round-3 findings were saved to the blackboard (entry `01a0f226`). After the consolidation all of them were resolved; the entry was deleted and replaced by round 4's findings (`01a0f257`).

---

# Round 4 (2026-09-30): consolidation is clean — one under-specified feature and one spec regression stand between this document and freeze

Review of the consolidated document. **Verdict: the consolidation is a genuine rewrite done well, not section shuffling. All three strata-staleness instances are fixed, the `sync-base` rename landed with a decision-log row, and the document is now a single body with the decision log as history — exactly the structure recommended. Two things need attention before freezing: the dashboard's new "live session stream + `claude --resume` handoff" is under-specified and unphased, and the Slack module spec quietly lost content that Track B needs.**

## What the consolidation got right

- **All three staleness instances fixed.** R12 is now the single statement of the deny list (with the MCP section deferring to "the R12 default"), and the terminology is uniform ("llmrouter scriptling stores" everywhere; the old "shared prompt and skill stores" is gone). The alternatives section now differentiates on "push only to allowed branch patterns and open draft PRs but never merge" — matching R7 exactly. And the trial's push transport got its own section, better than requested: the tool fetches over SSH with Martin's key, pushes over HTTPS with the agent-bot token passed through git's environment (never in command arguments, so it's invisible to `ps`), and the agent environment has no SSH agent plus `GIT_SSH_COMMAND=/usr/bin/false`.
- **`sync-base` rename** propagated to the flow, pipeline section, and phase table, with a decision-log row recording the rationale.
- **The structure is right:** single body, no delta sections, decision log as history — including a backfilled 2026-09-28 row for the trial's one-thread-per-item redesign and a new row for the consolidation itself.
- **New claims that close old gaps:** Gitea branch protection "verified with test pushes on 2026-09-25"; layer 0 now states Fortix's org plan already supports rulesets on private repos (closing the GitHub Pro caveat for the work repos, while the personal-repo caveat correctly remains); Sortie's row now records "tracker adapters including Gitea" and its rejection date.
- **The "as built" Track A** honestly reflects the trial's evolution (1-minute ticks, background worker, orphan recovery, `retry:`/`help:` commands, mid-run replies queued) rather than the original sketch.

## Before freeze: two items with substance

1. **The dashboard's "live session stream and a `claude --resume <id>` handoff" (Skopos changes §10) is new, assigned to no phase, and doesn't work as written.** Claude sessions live as JSONL on the machine that ran them; `claude --resume <id>` only works there. Production workers are Linux VMs, so a resume "handoff" from Martin's dashboard means either an SSH-to-the-worker affordance or settling for a transcript view (replay, not resume). Each is a different feature with different plumbing (streaming run output into Skopos vs. an SSH link). Decide which, add one sentence, and give it a phase.
2. **The Slack module spec regressed.** Round 3 specified messages on `awaiting_approval` (buttons), `in_review` (branch, diff stats, checks, Request changes **and Mark done**), and on `failed`/`blocked`. The consolidated version kept only "Approve, Request changes and Reject buttons … the same short message formats as the trial." Track B's `in_review → done` transition and the blocked/question loop (the trial's `retry:`) both depend on those messages existing. Re-instate one sentence.

## One-liners

- `new:` in Track B Slack needs a workspace story — the trial is single-workspace so the question is invisible there.
- `capacity` appears in worker registration but isn't wired into `run_claim` semantics (one line: claims respect remaining capacity).
- The old (correctly dropped) "tags map one-to-one onto statuses" claim was never replaced: 1a should include a small tag→status mapping table for in-flight items when `agent-trial` switches APIs.
- Layer 0 says "an org ruleset," but allowed patterns are per-workspace (`agent/*` vs `WR{n}-*`); an org-wide ruleset would have to union the patterns (allowing WR-shaped branches in every repo), while layer 1's "generated from the same config as the ruleset" implies per-repo rulesets generated per workspace. Name the choice.

## Bottom line

This is the strongest version of the document: coherent, current, and honest about what's verified versus planned. Fix the two substantive items and the four one-liners, freeze it, and start 1a — the review rounds have done their job, and further polish has diminishing returns next to actual schema. Round-4 findings were saved to the blackboard (entry `01a0f257`). After the next revision all of them were resolved; the entry was deleted and replaced by round 5's findings (`01a0f5f6`).

---

# Round 5 (2026-10-01): round 4 fully closed — the document is internally consistent end to end for the first time

Review of the 2026-10-01 revision. **Verdict: all six round-4 items are resolved, each with the right shape and a same-day decision-log row. No new design decisions are needed. What remains is a short list of one-sentence clarifications — a 15-minute document pass, not another review round — after which the design should be frozen and 1a started.**

## Round-4 scorecard

1. **Live session stream vs. resume** — split exactly right: a read-only **transcript view** (phase 2), built on run events that workers forward live to Skopos (1b), replayable afterwards; and **takeover** as a copyable `ssh <worker> -t 'cd <worktree> && claude --resume <session>'` command that resumes the session on the machine where it actually lives, with the lease paused while Martin holds it. Decision logged. ✅
2. **Slack module regression** — restored and extended beyond round 3: a full message table (`awaiting_approval` with routing preview and WR, amendment proposed, `in_review` with Mark done, blocked/failed with Retry and an answer form, quota pause), text-reply parity, and `new <alias>:` for workspace selection with a default and an ambiguity fallback. ✅
3. **Capacity in `run_claim`** — "only returns a run while the worker has spare capacity"; the workers page shows it too. ✅
4. **1a migration table** — a full tag→status table, including the subtle cases: `plan-review` items get a plan revision created from the current plan with the trial's frozen base as `base_sha`, and migrated approvals are recorded `via: migration`. ✅
5. **Org vs. per-repo rulesets** — resolved as two tiers: a **per-repo ruleset generated from each workspace's branch template** (allow-list), plus an **org-wide baseline ruleset** that blocks the App from protected names in every repo, including repos without a generated ruleset. That covers unconfigured repos — the exact gap the question was about. ✅
6. **`new:` workspace story** — `new <alias>:` with per-workspace aliases. ✅

## Remaining clarifications (one sentence each, no design changes)

1. **Takeover of a *running* run forks the session.** `claude --resume` on a session whose process is still executing creates a second writer on the same session files. Restrict takeover to paused/blocked runs, or specify that takeover first issues `run_pause` and the worker stops its process cleanly.
2. **The Slack table offers Reject from `in_review` and `blocked`, but the status diagram has no such edges** (only `awaiting_approval → discarded`). Either add `in_review → discarded` and blocked/failed → discarded edges — discarding at review is a sensible action — or scope the button.
3. **The per-repo ruleset says "create and update are allowed only on that pattern" — delete is unspecified.** Layer 1 deletes the run's own branch on reject, so the generated ruleset must also allow deletes on the allowed pattern; otherwise reject-cleanup dies with a mysterious 403 during 1b setup.
4. **Migration table nits:** it omits a `discarded → discarded` row (rejected trial items do exist), and it names a `changes-requested` tag that isn't in the trial's state diagram.
5. **Run-event forwarding** is in the 1b Skopos column but not in phantom's P1–P9 change list (add it to P1 or a P10), and stored event streams need a retention rule — the cleanup package is the natural home.

## Bottom line

This is the version to freeze. Every round since the first has converged: the requirements, the layers, the phases, the migration, and the operational edge cases now agree with each other, and the decision log is complete and current. Apply the five one-sentence clarifications, declare the design frozen, and put the next session's effort into 1a schema — `workspace_groups`, the audit log (plan `01a0bf7c`), `plan_revisions`, `approvals`, and the workflow statuses. Round-5 findings are on the blackboard (entry `01a0f5f6`, scope branch `main`).
