// The CSP build of Alpine evaluates expressions without eval/new Function,
// letting the dashboard ship a strict Content-Security-Policy (no
// 'unsafe-eval'). It supports property access, operators, ternaries, and
// method calls — but not template literals or optional chaining, hence the
// expression style in base.html.
import Alpine from '@alpinejs/csp';
import focus from '@alpinejs/focus';
import type { EditorView } from '@codemirror/view';
import { mountMarkdownEditor, editorText, destroyEditor } from './editor';

type SessionSummary = { id: string; title: string; workspace: string; status: string; agent_count: number };
type SessionDetail = SessionSummary & { agents?: AgentState[]; events?: Event[] };
type AgentState = { agent_id: string; agent_type: string; status: string; progress?: number; message: string; snippet: string };
type Event = AgentState & { id: string; created_at: string; metadata?: Record<string, unknown>; step_current?: number; step_total?: number };
type Entry = { id: string; scope: string; workspace_id?: string; branch_name?: string; session_id?: string; entry_type: string; title: string; content: string; code_ref?: string; author_agent_id: string; created_at: string };
type Bundle = { entries: Entry[]; markdown_bundle: string };
type PlanItem = { id: string; plan_id: string; title: string; description?: string; phase?: string; status: string; position: number; claimed_by_agent_id?: string; depends_on?: string[] };
type Plan = { id: string; name: string; branch_name?: string; workspace_id?: string; description?: string; status: string; author_agent_id: string; items?: PlanItem[]; depends_on?: string[]; created_at: string };
type Toast = { id: number; message: string; type: 'success' | 'error' | 'info' };
type ApiKey = { id: string; name: string; key_prefix: string; all_workspaces: boolean; approver?: boolean; delegate?: boolean; admin?: boolean; capabilities?: string[] | null; workspaces: string[]; groups?: string[]; created_at: string; last_used_at?: string; revoked_at?: string };
type KeyGroup = { id: string; name: string; description?: string; members: string[]; patterns: string[]; created_at: string };
type KeyReach = { key: ApiKey; via: string };
type WorkspaceRow = { id: string; name?: string; git_url?: string; created_at: string };
type Whoami = { root: boolean; key?: { id: string; name: string; all_workspaces: boolean; approver?: boolean; admin?: boolean; workspaces: string[] }; workspaces: { id: string; name: string }[] };
type KeyForm = { name: string; all: boolean; workspaces: string[] };
type EntryForm = { scope: 'project' | 'branch' | 'session'; entry_type: 'finding' | 'decision' | 'bug' | 'debt' | 'warning' | 'context'; title: string; content: string; code_ref: string; branch_name: string; session_id: string };
type PlanForm = { name: string; description: string; branch_name: string };
type ItemForm = { title: string; description: string; phase: string; depends_on: string };
type InboxPlanSummary = { id: string; name: string; status: string };
type InboxItem = { id: string; workspace_id?: string; title: string; tags: string[]; status: string; priority?: number; claimed_by_agent_id?: string; author_agent_id: string; plan_id?: string; plan?: InboxPlanSummary; agents?: string; branch?: string; ref?: string; external_url?: string; flow?: string; created_at: string; updated_at: string; excerpt?: string };
// The detail view's content, cut server-side (internal/inbox/parts.go):
// The author's text, then the pipeline's sections in rounds.
type ItemQuestion = { text: string; options?: string[] };
type ContentSection = { heading: string; html: string; questions?: ItemQuestion[]; label?: string };
type ContentRound = { kind: string; title: string; n: number; closed: boolean; sections: ContentSection[] };
type ContentParts = { brief_html: string; rounds: ContentRound[] };
type InboxDetail = InboxItem & { content: string; content_html: string; content_parts?: ContentParts };
// The item's question-and-answer record (GET /api/inbox/{id}/questions).
type QAQuestion = { id: string; n: number; text: string; options: string[]; state: string };
type QASet = { id: string; asker: string; label?: string; open: boolean; blocking: boolean; questions: QAQuestion[] };
type AnswerItem = { question_id: string; option?: string; text?: string };
type RoundView = { key: string; title: string; meta: string; open: boolean; sections: { key: string; heading: string }[] };
type QuestionView = { key: string; n: number; text: string; options: { key: string; letter: string; text: string }[] };
type QuestionGroup = { key: string; title: string; label: string; questions: QuestionView[] };
function optionLetter(i: number): string { return i < 26 ? String.fromCharCode(97 + i) : String(i + 1); }
// The agent preset (internal/auth Presets["agent"], normalised).
const agentPreset = ['blackboard:write', 'code:read', 'inbox:read', 'mcp', 'plans:write', 'status:read'];
function keyIsAgentPreset(caps: string[]): boolean {
  return caps.length === agentPreset.length && [...caps].sort().every((c, i) => c === agentPreset[i]);
}

// A worker's pause (an operator stopped it taking work); until "" = until resumed.
type WorkerPause = { at: string; until?: string; by?: string; reason?: string };
type WorkerRow = { id: string; workspaces: string[]; stages: string[]; runners: string[]; active: number; succeeded: number; failed: number; lastActive: string; currentLabel: string; state: string; version: string; lastSeen: string; unhealthy: string[]; indexable: string[]; paused: WorkerPause | null; facts: string[]; executors: string[]; factsWarning: string; serverConfig: string };
type RunnerFacts = { name: string; kind?: string; model?: string; effort?: string; provider?: string; endpoint?: string; stages?: string[] };
type WorkerSeen = { id: string; runners: string[]; workspaces: string[]; last_seen: string; version?: string; state?: string; unhealthy?: Record<string, string>; indexable?: string[]; paused?: WorkerPause; runner_facts?: RunnerFacts[]; executors?: Record<string, ExecutorFact[]>; server_commands?: boolean; fleet_rev?: string };
// Workspace config held on the server (internal/wsconfig).
type ConfigEffective = { workspace: string; managed: boolean; rev?: string; config: Record<string, unknown>; sources?: Record<string, string>; conflicts?: string[] };
type ConfigView = { owner_kind: string; owner_id: string; config: Record<string, unknown> | null; rev?: string; updated_by?: string; effective?: ConfigEffective; changed?: string[]; defaults?: Record<string, unknown> };
// The config form (pipeline dialog, Server config tab): one row per field
// that has a control of its own. kind picks the control; a row is part of
// the saved config only when it is set.
type CfgOption = { value: string; label: string };
type CfgField = { key: string; label: string; group: string; kind: 'text' | 'duration' | 'number' | 'select' | 'lines'; options?: CfgOption[]; hint: string; placeholder?: string };
type CfgRow = CfgField & { set: boolean; value: string; applies: string; from: string; error: string; options: CfgOption[]; placeholder: string };
type CfgGroup = { name: string; open: boolean; rows: CfgRow[] };
type CfgProblem = { field: string; message: string };
// A stage's block as the form holds it: lists as one entry per line, the
// wrapper rules as rows, the sandbox settings as JSON text.
type CfgWrap = { id: number; wrapper: string; runner: string; kind: string; provider: string; model: string };
type CfgStage = { stage: string; label: string; open: boolean; set: boolean; tools: string; env: string; allow: string; deny: string; dirs: string; sandbox: string; wraps: CfgWrap[]; error: string; from: string };
const CFG_STAGES: [string, string][] = [['plan', 'Plan'], ['implement', 'Implement'], ['review', 'Review'], ['qa', 'QA']];
let cfgWrapSeq = 0;
// Every config field with a control. What is not here (the stage blocks)
// stays text in the form.
const CFG_FIELDS: CfgField[] = [
  { key: 'default_branch', label: 'Default branch', group: 'Branches', kind: 'text', hint: 'The branch work starts from and is compared with.' },
  { key: 'branch_prefix', label: 'Branch prefix', group: 'Branches', kind: 'text', hint: 'Branches the agents create start with this.' },
  { key: 'branch_template', label: 'Branch name', group: 'Branches', kind: 'text', hint: 'Placeholders {prefix}, {tail}, {slug}, {ref}; needs {tail} or {ref}. Empty: {prefix}{tail}-{slug}.', placeholder: '{prefix}{tail}-{slug}' },
  { key: 'checkout', label: 'Implementations run in', group: 'Branches', kind: 'select', options: [{ value: 'worktree', label: 'a worktree per item' }, { value: 'branch', label: 'the clone itself (branch checkout)' }], hint: 'The clone itself suits a repository tied to a local system; QA then tests there too.' },
  { key: 'protected_branches', label: 'Protected branches', group: 'Branches', kind: 'lines', hint: 'No run pushes to these. One per line; ** matches across slashes.' },
  { key: 'setup', label: 'Setup', group: 'Commands the workers run', kind: 'lines', hint: 'Prepare a checkout before an agent works in it: one command per line. {clone} is the clone\'s path.' },
  { key: 'checks', label: 'Checks', group: 'Commands the workers run', kind: 'lines', hint: 'Run after every implementation: one command per line. A failing one goes back to the implementer.' },
  { key: 'check_fix_rounds', label: 'Fix rounds', group: 'Commands the workers run', kind: 'number', hint: 'How often failing checks go back to the implementer: 0 to 5.' },
  { key: 'preflight', label: 'Preflight', group: 'Commands the workers run', kind: 'lines', hint: 'Say whether a worker can take the repository\'s work right now: one command per line.' },
  { key: 'preflight_interval', label: 'Preflight every', group: 'Commands the workers run', kind: 'duration', hint: 'At least 1m.' },
  { key: 'setup_timeout', label: 'Setup', group: 'Timeouts', kind: 'duration', hint: 'All setup commands together, e.g. 10m.' },
  { key: 'check_timeout', label: 'Checks', group: 'Timeouts', kind: 'duration', hint: 'All checks together.' },
  { key: 'quiet_timeout', label: 'Silent command', group: 'Timeouts', kind: 'duration', hint: 'Stops a setup or check command that prints nothing for this long. 0s: off.' },
  { key: 'plan_timeout', label: 'Plan and review', group: 'Timeouts', kind: 'duration', hint: 'A planning, review, compare or synthesize agent.' },
  { key: 'impl_timeout', label: 'Implement', group: 'Timeouts', kind: 'duration', hint: 'The implementing agent.' },
  { key: 'qa_timeout', label: 'QA', group: 'Timeouts', kind: 'duration', hint: 'The testing agent.' },
  { key: 'push.kind', label: 'Push to', group: 'Push and pull requests', kind: 'select', options: [{ value: 'none', label: 'nowhere (branches stay on the worker)' }, { value: 'github', label: 'GitHub' }, { value: 'gitea', label: 'Gitea' }], hint: 'The token stays on each worker (push.token_env in its file).' },
  { key: 'push.remote_url', label: 'Remote URL', group: 'Push and pull requests', kind: 'text', hint: 'An http(s) URL without credentials.', placeholder: 'https://github.com/you/repo.git' },
  { key: 'push.pull_request', label: 'Pull request', group: 'Push and pull requests', kind: 'select', options: [{ value: 'none', label: 'none' }, { value: 'draft', label: 'open as a draft' }, { value: 'ready', label: 'open ready for review' }], hint: 'Opened or reused for the item\'s branch. Workers never merge.' },
  { key: 'push.steps', label: 'Push as it goes', group: 'Push and pull requests', kind: 'select', options: [{ value: 'true', label: 'yes: each step\'s commits' }, { value: 'false', label: 'no: only at the end' }], hint: 'Pushing the implementer\'s commits while it works keeps them if the worker dies.' },
  { key: 'mcp', label: 'MCP servers for runs', group: 'MCP', kind: 'lines', hint: 'Names of MCP server definitions on the server, one per line. Set (also to nothing), runs get exactly these and no worker\'s own mcp_config.' },
  { key: 'mcp_servers', label: 'Allowed MCP tools', group: 'MCP', kind: 'lines', hint: 'Tool prefixes plan and implement runs may call, one per line: mcp__<server>.', placeholder: 'mcp__router' },
  { key: 'mcp_deny', label: 'Denied MCP tools', group: 'MCP', kind: 'lines', hint: 'Tools no run may call, one per line.', placeholder: 'mcp__router__delete_thing' },
];
type RunnerDef = { name: string; kind: string; model?: string; effort?: string; provider?: string };
type PolicyRule = { provider?: string; model?: string; kind?: string; runner?: string; stages?: string[]; executor?: string };
type ExecutorFact = { stage?: string; runner?: string; executor: string };
type WrapperDef = { name: string; argv: string[]; check?: string[]; executor?: string };
type MCPServerDef = { name: string; type: string; url?: string; headers?: Record<string, string>; command?: string[]; env?: Record<string, string>; workspaces?: string[]; secrets?: { name: string; set: boolean }[]; used_by?: string[]; rev?: string };
type PolicyLayer = { source: string; rev: string; policy: { allow: PolicyRule[] | null } };
// Pipeline settings (which agents run each stage, per workspace or group).
type PipeMember = { runner: string; focus?: string };
type PipeStage = { members: PipeMember[]; policy?: string; early_exit?: boolean; timeout_ms?: number };
type PipeView = { owner_kind: string; owner_id: string; indexable_by?: string[]; own: { stages: Record<string, PipeStage> | null; agents_default?: string; second_opinion_runner?: string; code_index?: boolean }; effective?: Record<string, PipeStage & { source: string }>; effective_agents_default?: string; agents_default_source?: string; effective_second_opinion_runner?: string; second_opinion_source?: string; groups?: { id: string; name: string }[]; conflicts?: string[]; unknown_runners?: string[]; policy?: { allow_all: boolean; rev?: string; layers: PolicyLayer[] }; policy_refused?: string[] };
// One dialog row per stage. mode: own (set at this level, editable),
// inherited (from a group; Override copies it here), unset.
// A brief held on the server for a pipeline member's focus (no text in a
// listing), and one row of the Briefs tab.
type BriefInfo = { rev: string; owner_kind: string; owner_id: string; name: string; body?: string; deleted?: boolean; bytes: number; created_at: string; created_by: string; source?: string };
type BriefRow = { key: string; name: string; where: string; tone: string; detail: string; action: string; held: boolean; own: boolean };
type PipeRow = { stage: string; label: string; multi: boolean; rule: boolean; mode: string; members: PipeMember[]; policy: string; earlyExit: boolean; timeoutMin: string; inherited: PipeStage | null; inheritedFrom: string; addRunner: string; addFocus: string };

type PlanRevisionInfo = { id: string; revision_no: number; content_hash: string; skills?: string[]; base_sha?: string; group_id?: string; label?: string; candidate_state?: string; steps?: { title: string; phase?: string }[]; locked_at?: string };
// One thing the approver can approve at the gate: the plan revision, or one
// candidate of a multi-agent plan round.
type ApproveTarget = { id: string; title: string; meta: string; steps: string[]; candidate: boolean; hash: string; toast: string };
type RunRow = { id: string; stage: string; status: string; worker_id?: string; runner?: string; attempt: number; branch?: string; base_sha?: string; head_sha?: string; result_summary?: string; diffstat?: string; fail_reason?: string; executor?: string; group_id?: string; focus?: string; cost_usd?: number; findings?: { id?: string; severity: string; text: string; file?: string }[]; still_open?: number; lease_expires_at?: string; pushed?: boolean; pr_url?: string; checks?: { name: string; passed: boolean; output?: string }[]; setup?: { name: string; passed: boolean; output?: string }[]; fix_rounds?: number; phase?: string; retries?: number; retry_after?: string; prefer_worker?: string; prefer_until?: string; stranded?: string; runner_picked?: boolean; policy_rev?: string; created_at: string; updated_at: string };
// One row of the preview's stage tracker. state: done | run | you | bad |
// up (not reached) | skip (not configured, or never reached on a discarded item).
type LinkRow = { key: string; href: string; text: string };
// One line of a run's log (GET /api/runs/{id}/log), and a tracker button
// that opens a run's log.
type LogRecord = { t: number; src: string; msg: string; err?: boolean; k?: number };
type LogButton = { key: string; id: string; text: string };
type MoreRow = { key: string; label: string; text: string; mono?: boolean };
type StageRow = { key: string; name: string; who: string; state: string; when: string; round: string; lines: string[]; links: LinkRow[]; more: MoreRow[]; moreLabel: string; warn: string; logs: LogButton[]; plans: boolean; hidden?: boolean };
type TextRow = { key: string; ic: string; text: string; more?: MoreRow[] };
// The item's record of review findings (GET /api/inbox/{id}/findings).
type FindingUpdate = { id: string; kind: string; note?: string; run_id?: string; actor?: string; created_at: string };
type FindingRec = { id: string; source?: string; run_id: string; stage: string; runner?: string; focus?: string; head_sha?: string; severity: string; text: string; file?: string; state: string; tracked: boolean; sent_back: boolean; updates: FindingUpdate[] };
type FindingRow = { key: string; id: string; severity: string; tone: string; text: string; meta: string; notes: string[]; canSend: boolean; picked: boolean; canWaive: boolean; waiving: boolean };
// One planning round of an item (GET /api/inbox/{id}/plans): every planner's
// plan in full, the comparison and the synthesis.
type PlanDocView = { kind: string; label?: string; title: string; revision_id?: string; html: string; questions_html?: string };
type PlanNoteView = { heading: string; html: string };
type PlansRound = { item_id: string; title: string; round: number; rounds: number; plans: PlanDocView[]; comparison?: PlanNoteView; notes?: PlanNoteView[] };
type PlanColumn = { key: string; title: string; kind: string; tone: string; html: string; questionsHtml: string; hasQuestions: boolean; revisionId: string; canApprove: boolean; hidden: boolean };
type FindingGroupView = { key: string; id: string; title: string; hint: string; open: boolean; rows: FindingRow[] };
type AuditEntry = { id: string; action: string; actor: string; via?: string; notes?: string; created_at: string };
// wsLocked: a workflow item cannot move workspace (its runs, plan and
// branch live there) — the server refuses it; the modal hides the picker.
type InboxForm = { id: string; title: string; tags: string; workspace: string; agents: string; branch: string; ref: string; wsLocked: boolean };

const UI_AUTHOR = 'ui';
type View = 'sessions' | 'blackboard' | 'plans' | 'inbox' | 'index' | 'keys' | 'workspaces' | 'workers' | 'webhooks';
// Webhook subscriptions (internal/integrations; /api/webhooks).
type Webhook = { id: string; name: string; url: string; workspaces: string[]; groups: string[]; types: string[]; enabled: boolean; disabled_reason?: string; failing_since?: string; created_by: string; created_at: string; updated_at: string };
type WebhookAttempt = { at: string; code?: number; error?: string; duration_ms: number };
type WebhookDelivery = { id: string; event_id: string; event_type: string; event_seq: number; item_id?: string; state: string; attempts: number; next_at?: string; last_code?: number; last_error?: string; last_at?: string; created_at: string; attempts_log?: WebhookAttempt[] };
// The event types a subscription may name (internal/integrations Types);
// run.* reach a subscription only when named.
const WEBHOOK_TYPES = ['item.created', 'item.status_changed', 'item.needs_you', 'item.closed', 'plan.ready', 'questions.waiting',
  'questions.answered', 'run.started', 'run.finished', 'checks.finished', 'review.finished', 'pr.opened'];

// The inbox markdown editor lives OUTSIDE Alpine's reactive state: deep-
// proxying a live CodeMirror view breaks it. One instance per open modal.
let inboxEditor: EditorView | null = null;

// The kanban drop-indicator element also stays outside Alpine state for the
// same reason — it is transient DOM bookkeeping, not UI state.
let inboxDropMarker: HTMLElement | null = null;
function clearInboxDropMarker() {
  if (inboxDropMarker) { inboxDropMarker.classList.remove('drop-before', 'drop-after', 'drop-into'); inboxDropMarker = null; }
}

// The preview's stage model, computed once per change of its inputs (the
// tracker, header pill and action card all read it during one render).
// Outside Alpine state: writing reactive state while rendering would loop.
// Keyed on the identities of the reactive inputs, which change on refetch.
let stageCache: { inputs: unknown[]; model: unknown } | null = null;

// The open preview's slow poll: run state that changes without an inbox
// event (a review run starting, a worker going offline, a lease running out).
let previewPoll: ReturnType<typeof setInterval> | null = null;

// The run log panel's long-poll (aborted when the panel closes), and the
// running key its lines get (stable while old lines are trimmed).
// The config form's debounced check, and a counter that drops the answer
// to a check an edit has overtaken.
let cfgCheckTimer: ReturnType<typeof setTimeout> | null = null;
let cfgCheckSeq = 0;
let runLogAbort: AbortController | null = null;
// The run log panel's poll loop is reading (a loop ends with its run), and
// the timer that looks for the item's next run while the panel follows it.
let runLogLooping = false;
let runLogWatch: ReturnType<typeof setInterval> | null = null;
let runLogKey = 0;
// When the plans dialog was last closed: Escape reaches every open dialog,
// and the item preview under it must not close with it.
let plansClosedAt = 0;
// Lines the panel keeps (the server keeps the whole log).
const RUN_LOG_KEEP = 5000;

declare global { interface Window { Alpine: typeof Alpine; app: () => object } }

const appState = () => ({
  // navigation + layout
  activeView: (localStorage.getItem('skopos:view') || 'sessions') as View,
  sidebarOpen: false,
  theme: 'system' as 'dark' | 'light' | 'system',

  // sessions
  sessions: [] as SessionSummary[],
  selectedSession: null as SessionDetail | null,
  selectedSessionId: '',
  loading: false,

  // blackboard
  entries: [] as Entry[],
  blackboardBranch: '',
  blackboardLoading: false,

  // plans
  plans: [] as Plan[],
  plansBranch: '',
  plansLoading: false,
  expandedPlan: null as Plan | null,

  // inbox
  inboxItems: [] as InboxItem[],
  inboxLoading: false,
  inboxLayout: (localStorage.getItem('skopos:inboxLayout') === 'lanes' ? 'lanes' : 'list'),
  inboxStatus: 'open' as '' | 'open' | 'in_progress' | 'converted' | 'done' | 'discarded' | 'workflow' | 'needs_you',
  // Items waiting for an approver across the key's scope (sidebar badge and
  // the browser tab title); refreshed on inbox events.
  needsYouCount: 0,
  baseTitle: '',
  inboxTagFilter: '',
  inboxQuery: '',
  inboxSort: (['newest', 'oldest'].includes(localStorage.getItem('skopos:inboxSort') || '') ? localStorage.getItem('skopos:inboxSort') : 'priority') as 'priority' | 'newest' | 'oldest',
  workspaceMenuOpen: false,
  workspaceSearch: '',
  workspaceHighlightId: '',
  expandedInboxId: '',
  showInboxPreview: false,
  previewLoading: false,
  previewItem: null as InboxDetail | null,
  previewRuns: [] as RunRow[],
  // Second opinion on the previewed item: runner field (prefilled with the
  // workspace default), suggestions from workers in scope.
  secondOpinion: { runner: '', force: false, busy: false, runners: [] as string[] },
  previewRunsLoading: false,
  // The plan revision the preview shows for an item at the gate — Approve
  // names exactly this one (M2), so the server refuses it if the planner
  // wrote a newer revision after the page was read.
  previewRevision: null as PlanRevisionInfo | null,
  // Open candidates of the current multi-agent plan round, oldest first
  // (numbered 1..n, the same order Slack and the CLI use).
  previewCandidates: [] as PlanRevisionInfo[],
  // The approved (locked) revision of the item's plan, for the tracker's
  // "Plan approval" line, and the workspace's effective pipeline (is a
  // review stage configured at all?).
  previewApprovedRev: null as PlanRevisionInfo | null,
  previewPipe: null as PipeView | null,
  // Collapsible preview sections. The description starts open until the
  // plan is approved (read by then); the run history starts closed (the
  // tracker summarises it). Re-defaulted only when the status changes, so a
  // live refresh never undoes the user's toggle.
  previewDescOpen: true,
  previewRunsOpen: false,
  // Rounds of the description the user opened or closed, by key; the
  // others follow the default (only the newest round open).
  previewRoundOpen: {} as Record<string, boolean>,
  // Answers to the planner's questions at the gate, by question key: the
  // picked option's letter and free text. Sent with the action as answers.
  questionAnswers: {} as Record<string, { pick: string; text: string }>,
  previewQuestions: [] as QASet[],
  // The item's review findings, the ones ticked to be sent back with
  // Request changes or Retry, and the one being waived (with its note).
  // The side-by-side view of an item's plans: one planning round, and the
  // columns the reader hid.
  planView: { open: false, loading: false, view: null as PlansRound | null, hidden: {} as Record<string, boolean> },
  previewFindings: [] as FindingRec[],
  // The action that takes findings to send back now, as the server says
  // ('request-changes', 'retry', or '' when none does or the key may not).
  previewFindingsSend: '',
  findingPicks: {} as Record<string, boolean>,
  // Groups the approver folded or unfolded by hand (by group id).
  findingFold: {} as Record<string, boolean>,
  waive: { id: '', note: '', busy: false },
  // The item whose answers were saved on their own in this preview: its
  // Retry then needs no more text.
  answersSavedFor: '',
  // Workers in scope (an active run on an offline worker is flagged) and the
  // item's audit trail (the History section).
  previewWorkers: [] as WorkerSeen[],
  previewTimeline: [] as AuditEntry[],
  previewStatusSeen: '',
  // The run log panel: the run it shows, its lines, the read cursor, and
  // whether it scrolls with new lines (follow) — live while the run runs.
  logRunId: '',
  logRecords: [] as LogRecord[],
  logCursor: 0,
  logTruncated: false,
  logLoading: false,
  logFollow: true,
  logError: '',
  // Move to the next run: when the shown run ends, the panel shows the
  // item's next run as soon as it starts (on when a live run is opened).
  logAuto: false,
  // What the panel moved on from ('' = opened by hand).
  logFrom: '',
  workflowNote: '',
  workflowBusy: false,
  expandedInbox: null as InboxDetail | null,
  // id of the card being dragged (HTML5 DnD; same-window only, so state is
  // the transport — dataTransfer.setData exists for Firefox's sake)
  inboxDragId: '',

  // workers (derived from run history — no registry yet)
  workers: [] as WorkerRow[],
  workersLoading: false,
  // The worker whose pause form is open, and the form (for: a duration, ""
  // = until resumed).
  // Rows whose details are folded out, by worker id (several may be open).
  workerFold: {} as Record<string, boolean>,
  // The pause dialog: open while pausingWorker names a worker.
  pausingWorker: '',
  pauseForm: { for: '2h', reason: '' },
  workerBusy: false,

  // code index
  indexBranches: [] as { branch: string; head_sha?: string; built_at: string; source?: string; file_count: number; symbol_count: number }[],
  indexGroups: [] as { id: string; label: string; branches: { branch: string; head_sha?: string; built_at: string; source?: string; file_count: number; symbol_count: number }[] }[],

  // header
  activeWorkspace: '',
  workspaces: [] as string[],
  registeredWorkspaces: [] as { id: string; name: string }[],

  // api key
  apiKey: (localStorage.getItem('skopos:apiKey') || '') as string,
  showKeyModal: false,
  keyDraft: '',
  authPrompted: false,

  // api keys / identity
  whoami: null as Whoami | null,
  // Role flags, set with whoami (fetchWhoami), for x-show inside dialogs.
  // Static page elements gated on a role use syncRoles() instead.
  isRoot: false,
  isApprover: false,
  keys: [] as ApiKey[],
  keysLoading: false,
  groups: [] as KeyGroup[],
  groupsLoading: false,

  showWsModal: false,
  wsModalMode: 'create' as 'create' | 'edit',
  wsSaving: false,
  wsErrors: {} as Record<string, string>,
  wsForm: { id: '', name: '', git_url: '', groups: [] as string[] },
  // opened from the header "+": select the new workspace once it exists
  wsSelectAfterCreate: false,
  // bumped on every dialog open; checklist row keys carry it so :checked
  // re-evaluates when a dialog reopens for different data (CSP build)
  formNonce: 0,
  showGroupModal: false,
  pipe: { open: false, kind: 'workspace', id: '', label: '', saving: false, readOnly: false, rev: 0, rows: [] as PipeRow[], agentsDefault: '', agentsInherited: '', agentsInheritedFrom: '', secondOpinion: '', secondInherited: '', codeIndex: false, codeIndexWas: false, indexableBy: [] as string[], notes: [] as { key: string; tone: string; text: string }[], runnerOptions: [] as string[], policyBound: false, tab: 'stages', editing: '', was: '' },
  // MCP servers for runs (Workspaces view, admins): the definitions, and
  // the dialog that edits one and sets its secrets.
  mcpDefs: [] as MCPServerDef[],
  mcpForm: { open: false, editing: false, name: '', type: 'http', url: '', headers: '', command: '', env: '', workspaces: '', error: '', saving: false, confirm: false, rev: 0, secrets: [] as { name: string; set: boolean; value: string }[] },
  // The Briefs tab of the pipeline dialog: the briefs held for this owner
  // (and, for a workspace, the ones a group gives it), and the editor.
  pipeBriefs: { list: [] as BriefInfo[], editing: false, name: '', nameLocked: false, body: '', was: '', from: '', history: [] as BriefInfo[], saving: false, confirm: '', rev: 0 },
  // The workspace's (or group's) config on the server, in the pipeline
  // dialog: what applies, and for an admin the stored config as JSON.
  pipeConfig: { shown: false, status: '', wrappers: [] as string[], mcp: [] as string[], stored: false, storedRev: '', saving: false, canEdit: false, rev: 0,
    // The form: whether the owner has a config at all (a workspace's
    // switch), its fields in folds, the stage blocks as JSON, and what a
    // check said that belongs to no field.
    managed: false, groups: [] as CfgGroup[], stageForms: [] as CfgStage[], stagesOpen: false, stagesError: '', wrapperNames: [] as string[], general: '', conflicts: [] as string[], was: '', checking: false, checked: '', view: null as ConfigView | null },
  groupModalMode: 'create' as 'create' | 'edit',
  groupSaving: false,
  groupErrors: {} as Record<string, string>,
  groupForm: { id: '', name: '', description: '', patterns: '', members: [] as string[] },
  // scoped API key create/edit: one dialog, two modes
  showApiKeyModal: false, keyModalMode: 'create' as 'create' | 'edit', keySaving: false,
  // access: 'full' (no capability limits), 'agent' (the agent preset: MCP
  // only) or 'keep' (edit: leave a custom set as it is).
  keyForm: { id: '', name: '', all: false, workspaces: [] as string[], groups: [] as string[], access: 'full', accessWas: 'full', custom: '' },
  keyErrors: {} as Record<string, string>,
  showSecretModal: false, newSecret: '', newSecretName: '', secretCopied: false,
  // The secret dialog serves API keys and webhook signing secrets.
  secretKind: 'key' as 'key' | 'webhook', secretTitle: 'API key created',

  // webhooks
  webhooks: [] as Webhook[],
  webhooksLoading: false,
  showWebhookModal: false, hookModalMode: 'create' as 'create' | 'edit', hookSaving: false,
  hookErrors: {} as Record<string, string>,
  hookForm: { id: '', name: '', url: '', workspaces: [] as string[], groups: [] as string[], types: [] as string[] },
  hookOpen: '', // the subscription whose deliveries show
  hookDeliveries: [] as WebhookDelivery[],
  hookDeliveriesLoading: false,
  hookBusy: '', // the subscription an action is running on

  // toasts
  toasts: [] as Toast[],

  // live updates
  sseAbort: null as AbortController | null,
  streamConnected: false,
  streamRetryDelay: 2000,
  streamRetryTimer: null as ReturnType<typeof setTimeout> | null,
  pollTimer: null as ReturnType<typeof setInterval> | null,
  pollInterval: 0,

  // modal: write entry
  showEntryModal: false, entrySaving: false,
  entryForm: emptyEntryForm(), entryErrors: {} as Record<string, string>,
  // modal: create plan
  showPlanModal: false, planSaving: false,
  planForm: emptyPlanForm(), planErrors: {} as Record<string, string>,
  // modal: add item
  showItemModal: false, itemSaving: false,
  itemForm: emptyItemForm(), itemErrors: {} as Record<string, string>,
  // modal: file an unfiled item into a workspace (searchable picker)
  showFileModal: false,
  fileTargetItem: null as InboxItem | null,
  fileQuery: '',
  fileHighlightId: '',

  // modal: create/edit inbox item (CodeMirror-backed content)
  showInboxItemModal: false, inboxItemSaving: false,
  inboxForm: emptyInboxForm(), inboxErrors: {} as Record<string, string>,
  // what the form/editor held when the modal opened — the dirty check
  // compares against this so an accidental close never silently discards edits
  inboxBaseline: emptyInboxBaseline(),
  // set when the plan-create modal was opened to convert an inbox item
  pendingConvertItemId: '',
  pendingConvertWorkspaceId: '',
  // modal: delete confirm
  confirm: { open: false, title: '', message: '', label: 'Delete', busy: false, pending: null as null | { kind: string; id: string } },
  // element focused before a dialog opened imperatively (file modal) —
  // x-trap restores focus for the others
  fileReturnFocus: null as HTMLElement | null,
  toastsPaused: false,

  init() {
    // Disclosure menus (<details class="menu">): one open at a time, closed
    // by a click elsewhere, by picking an action inside, or by Escape (focus
    // returns to its toggle). Capture phase: row actions stop propagation.
    document.addEventListener('click', (e: MouseEvent) => {
      const t = e.target as Element;
      document.querySelectorAll('details.menu[open]').forEach((d) => {
        if (!d.contains(t) || (t.closest && t.closest('.menu-item'))) d.removeAttribute('open');
      });
    }, true);
    document.addEventListener('keydown', (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return;
      document.querySelectorAll('details.menu[open]').forEach((d) => {
        d.removeAttribute('open');
        const sum = d.querySelector('summary');
        if (sum && d.contains(document.activeElement)) (sum as HTMLElement).focus();
      });
    });
    this.theme = (localStorage.getItem('skopos:theme') as 'dark' | 'light' | 'system') || 'system';
    this.applyTheme();
    if (window.matchMedia) {
      window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
        if (this.theme === 'system') this.applyTheme();
      });
    }
    this.activeView = (localStorage.getItem('skopos:view') || 'sessions') as View;
    // Restore the persisted workspace filter; refresh() sanitizes it against
    // the options the key can actually see once whoami/registry have loaded.
    this.activeWorkspace = localStorage.getItem('skopos:workspace') || '';
    const saved = localStorage.getItem('skopos:session') || '';
    if (saved) this.selectedSessionId = saved;
    // A link to one item (/?item=<id>, as webhook payloads carry) opens the
    // inbox with that item's detail.
    const linked = new URLSearchParams(window.location.search).get('item');
    if (linked) {
      this.switchView('inbox');
      history.replaceState(null, '', window.location.pathname);
      this.openInboxPreview({ id: linked } as InboxItem);
    }
    this.refresh();
    this.openEventStream();
  },

  // SSE over fetch with polling fallback. fetch (unlike EventSource) can send
  // the Authorization header, so live updates keep working when an API key is
  // configured. On stream end/error, polling takes over and SSE retries with
  // backoff. While connected, a slow reconciliation poll keeps running: the
  // server drops events when a subscriber's buffer is full, so this bounds how
  // stale the UI can get from a missed event.
  openEventStream() {
    this.connectSSE();
  },
  closeSSE() {
    if (this.sseAbort) { this.sseAbort.abort(); this.sseAbort = null; }
  },
  async connectSSE() {
    this.closeSSE(); // never leave a previous stream running
    const controller = new AbortController();
    this.sseAbort = controller;
    try {
      const res = await this.authFetch('/api/events/stream', {
        headers: { Accept: 'text/event-stream' },
        signal: controller.signal,
      });
      if (!res.ok || !res.body) throw new Error(`stream status ${res.status}`);

      this.streamConnected = true;
      this.streamRetryDelay = 2000; // reset backoff
      this.startPolling(30000); // slow reconciliation while connected
      this.refresh();
      // Events missed while the stream was down: catch the open item up.
      if (this.showInboxPreview && this.previewItem && !this.workflowNote) this.reloadPreview();

      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buf = '';
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        // Parse complete frames (blank-line separated); comment lines (": ping") are ignored.
        let sep: number;
        while ((sep = buf.indexOf('\n\n')) >= 0) {
          const frame = buf.slice(0, sep);
          buf = buf.slice(sep + 2);
          this.handleStreamFrame(frame);
        }
      }
    } catch { /* stream ended, aborted, or failed to open */ }

    if (this.sseAbort === controller) { // not superseded by a newer attempt
      this.sseAbort = null;
      this.streamConnected = false;
      this.startPolling(5000); // fast poll while SSE is down
      this.streamRetryDelay = Math.min((this.streamRetryDelay ?? 2000) * 2, 60000);
      clearTimeout(this.streamRetryTimer ?? undefined);
      this.streamRetryTimer = setTimeout(() => this.connectSSE(), this.streamRetryDelay);
    }
  },
  handleStreamFrame(frame: string) {
    let type = 'message';
    for (const line of frame.split('\n')) {
      if (line.startsWith('event:')) type = line.slice(6).trim();
    }
    if (type === 'sessions') this.refresh();
    else if (type === 'blackboard') { if (this.activeView === 'blackboard') this.fetchBundle(); }
    else if (type === 'plans') { if (this.activeView === 'plans') this.fetchPlans(); }
    else if (type === 'inbox') {
      if (this.activeView === 'inbox') this.fetchInbox();
      this.fetchNeedsYou();
      // Workflow transitions publish inbox events at exactly the moments a
      // run changes state (claim moves the phase, report chains). Refresh
      // the open modal fully — status badge and action buttons stay
      // current — EXCEPT while a note is being typed (reloadPreview would
      // clobber the input); then only the run rows refresh.
      if (this.showInboxPreview && this.previewItem) {
        if (this.workflowNote) this.fetchPreviewRuns(this.previewItem.id);
        else this.reloadPreview();
      }
    }
    else if (type === 'workspaces') this.fetchWorkspaces();
    else if (type === 'change') this.refresh();
  },
  startPolling(interval: number) {
    if (this.pollTimer && this.pollInterval === interval) return;
    this.stopPolling();
    this.pollInterval = interval;
    this.pollTimer = setInterval(() => this.refresh(), interval);
  },
  stopPolling() {
    if (this.pollTimer) { clearInterval(this.pollTimer); this.pollTimer = null; }
    this.pollInterval = 0;
  },

  anyModalOpen() {
    return this.showKeyModal || this.showApiKeyModal || this.showSecretModal || this.showWebhookModal || this.showWsModal || this.showGroupModal
      || this.showEntryModal || this.showPlanModal || this.showItemModal || this.showInboxItemModal || this.showInboxPreview
      || this.showFileModal || this.confirm.open || this.pipe.open || this.planView.open || this.mcpForm.open || this.pausingWorker !== '' || (this.sidebarOpen && window.matchMedia('(max-width: 767px)').matches);
  },

  // ---- shared dialog helpers ----
  // Checklist binding by dotted state path ("keyForm.groups"): one pair of
  // methods for every checkbox list (templates/partials.html "checklist").
  listAt(path: string): string[] {
    let v: any = this;
    for (const k of path.split('.')) v = v == null ? undefined : v[k];
    return Array.isArray(v) ? v : [];
  },
  inList(path: string, value: string): boolean { return this.listAt(path).indexOf(value) >= 0; },
  toggleInList(path: string, value: string) {
    const list = this.listAt(path);
    const i = list.indexOf(value);
    if (i >= 0) list.splice(i, 1); else list.push(value);
  },
  // After a failed validation, move focus to the first invalid field so a
  // screen reader announces it with its linked error (aria-describedby).
  focusFirstError() {
    this.$nextTick(() => {
      const dialogs = [...document.querySelectorAll('[role="dialog"], [role="alertdialog"]')] as HTMLElement[];
      const open = dialogs.filter(d => d.offsetParent !== null || getComputedStyle(d).display !== 'none');
      const el = open.length ? open[open.length - 1].querySelector('[aria-invalid="true"]') as HTMLElement | null : null;
      if (el) el.focus();
    });
  },
  confirmBusyLabel(): string {
    const l = this.confirm.label || 'Delete';
    return ({ Delete: 'Deleting…', Discard: 'Discarding…', Reject: 'Rejecting…', Revoke: 'Revoking…' } as Record<string, string>)[l] ?? l + '…';
  },

  // ---- tones ----
  // The one status → colour mapping. Colours live in style.css (.tone-*,
  // both themes); this only names the meaning. kind separates vocabularies
  // that reuse a word: a run STAGE "plan" vs nothing else.
  toneOf(kind: 'status' | 'entry' | 'scope' | 'stage' | 'severity', value?: string | null): string {
    const v = value || '';
    const maps: Record<string, Record<string, string>> = {
      status: {
        succeeded: 'success', done: 'success', completed: 'success',
        failed: 'danger', blocked: 'danger', orphaned: 'danger',
        running: 'active', testing: 'active', editing: 'active', in_progress: 'active', implementing: 'active', active: 'active',
        thinking: 'info', planning: 'info', queued: 'info', claimed: 'info',
        waiting: 'warn', paused: 'warn', stuck: 'warn', open: 'warn',
        awaiting_approval: 'attention', interrupted: 'attention', handoff: 'attention',
        approved: 'ready', converted: 'accent', in_review: 'review',
        archived: 'muted', cancelled: 'muted', discarded: 'muted',
        pending: 'neutral',
      },
      entry: { bug: 'danger', debt: 'warn', warning: 'warn', finding: 'active', decision: 'success', context: 'neutral' },
      severity: { blocking: 'danger', nit: 'neutral' },
      scope: { session: 'neutral', branch: 'accent', project: 'secondary' },
      stage: { plan: 'accent', implement: 'active', review: 'review', qa: 'review', second_opinion: 'review' },
    };
    return 'tone-' + (maps[kind][v] ?? 'neutral');
  },

  // ---- page chrome ----
  viewTitle(): string {
    return ({ sessions: 'Sessions', blackboard: 'Blackboard', plans: 'Plans', inbox: 'Inbox', index: 'Code index', keys: 'API keys', workspaces: 'Workspaces', workers: 'Workers', webhooks: 'Webhooks' } as Record<string, string>)[this.activeView] ?? '';
  },
  // Phone navigation drawer: focus moves in on open, Escape closes and
  // returns focus to the menu button. Closed, the drawer is invisible to
  // keyboard and screen readers (max-md:invisible in the markup).
  openSidebar() {
    this.sidebarOpen = true;
    this.$nextTick(() => { (document.querySelector('#main-nav .nav-item[aria-current="page"]') as HTMLElement | null || document.querySelector('#main-nav .nav-item') as HTMLElement | null)?.focus(); });
  },
  // No explicit focus here: while the trap is still active it would pull
  // focus straight back into the drawer, which then hides and drops focus
  // to <body>. x-trap returns focus to the menu button when it releases.
  closeSidebar() {
    if (!this.sidebarOpen) return;
    this.sidebarOpen = false;
  },
  toggleSidebar() { if (this.sidebarOpen) this.closeSidebar(); else this.openSidebar(); },

  // ---- theme ----
  applyTheme() {
    const dark = this.theme === 'dark' || (this.theme === 'system' && window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches);
    document.documentElement.classList.toggle('dark', dark);
    document.documentElement.classList.toggle('light', !dark);
  },
  cycleTheme() {
    this.theme = this.theme === 'dark' ? 'light' : this.theme === 'light' ? 'system' : 'dark';
    localStorage.setItem('skopos:theme', this.theme);
    this.applyTheme();
  },

  // ---- networking ----
  // authFetch never rejects: a network failure becomes a synthetic 503 response
  // so every caller's handleBad/res.ok path shows a toast instead of an
  // unhandled rejection.
  async authFetch(url: string, opts: RequestInit = {}): Promise<Response> {
    const headers = new Headers(opts.headers || {});
    if (opts.body) headers.set('Content-Type', 'application/json');
    if (this.apiKey) headers.set('Authorization', 'Bearer ' + this.apiKey);
    // Every dashboard request names its surface for the audit trail.
    if (!headers.has('X-Skopos-Via')) headers.set('X-Skopos-Via', 'dashboard');
    try {
      const res = await fetch(url, { ...opts, headers });
      if (res.status === 401) this.authFailed();
      return res;
    } catch {
      return new Response(JSON.stringify({ error: 'network error: server unreachable' }), {
        status: 503,
        headers: { 'Content-Type': 'application/json' },
      });
    }
  },
  async extractError(res: Response): Promise<string> {
    try { const j = (await res.json()) as { error?: string }; return j.error || res.statusText || 'request failed'; }
    catch { return res.statusText || 'request failed'; }
  },

  // ---- toasts ----
  // Errors stay until dismissed; others expire after 5s unless the stack is
  // hovered or focused (WCAG 2.2.1). At most four on screen.
  notify(message: string, type: Toast['type'] = 'info') {
    const id = Date.now() + Math.random();
    this.toasts.push({ id, message, type });
    if (this.toasts.length > 4) this.toasts.splice(0, this.toasts.length - 4);
    if (type !== 'error') this.expireToast(id, 5000);
  },
  expireToast(id: number, ms: number) {
    window.setTimeout(() => {
      if (this.toastsPaused) { this.expireToast(id, 1500); return; }
      this.dismissToast(id);
    }, ms);
  },
  pauseToasts() { this.toastsPaused = true; },
  resumeToasts() { this.toastsPaused = false; },
  dismissToast(id: number) { this.toasts = this.toasts.filter((t: Toast) => t.id !== id); },
  toastClass(type: Toast['type']): string {
    switch (type) {
      case 'success': return 'border-emerald-700 bg-emerald-950 text-emerald-200';
      case 'error': return 'border-rose-700 bg-rose-950 text-rose-200';
      default: return 'border-zinc-700 bg-zinc-900 text-zinc-200';
    }
  },

  // ---- api key modal ----
  // Reads swallow non-OK responses by design (empty lists, no toasts), so a
  // missing/wrong key would render as a silently dead dashboard. Any 401 funnels
  // here instead: one prompt per key change, not one per polled request.
  authFailed() {
    if (this.authPrompted || this.showKeyModal) return;
    this.authPrompted = true;
    this.notify('Unauthorized — set your API key', 'error');
    this.openKeyModal();
    // The stored key was just rejected: offer an empty draft, not the bad
    // key. Pre-filling it invites paste-without-select-all, which appends
    // and produces another 401 that looks like "the new key doesn't work".
    this.keyDraft = '';
  },
  openKeyModal() { this.keyDraft = this.apiKey; this.showKeyModal = true; },
  closeKeyModal() { this.showKeyModal = false; },
  saveKey() {
    this.apiKey = this.keyDraft.trim();
    if (this.apiKey) localStorage.setItem('skopos:apiKey', this.apiKey); else localStorage.removeItem('skopos:apiKey');
    this.showKeyModal = false;
    this.authPrompted = false;
    this.notify(this.apiKey ? 'API key saved' : 'API key cleared', 'success');
    // Reload with the new credentials instead of waiting for a manual refresh.
    this.refresh();
  },
  clearKey() { this.apiKey = ''; this.keyDraft = ''; localStorage.removeItem('skopos:apiKey'); this.showKeyModal = false; this.authPrompted = false; this.notify('API key cleared', 'success'); },

  // ---- view / workspace ----
  switchView(v: View) {
    this.activeView = v; this.sidebarOpen = false; localStorage.setItem('skopos:view', v);
    if (v === 'blackboard') this.fetchBundle();
    if (v === 'plans') this.fetchPlans();
    if (v === 'inbox') this.fetchInbox();
    if (v === 'index') this.fetchIndexStatus();
    if (v === 'keys') this.fetchKeys();
    if (v === 'workspaces') { this.fetchWorkspaces(); this.fetchGroups(); this.fetchMcpDefs(); if (this.whoamiIsRoot()) this.fetchKeys(); }
    if (v === 'workers') this.fetchWorkers();
    if (v === 'webhooks') { this.fetchWebhooks(); if (this.whoamiIsRoot()) this.fetchGroups(); }
  },
  whoamiIsRoot(): boolean { return !!this.whoami && this.whoami.root; },
  setWorkspace(ws: string) {
    this.activeWorkspace = ws;
    if (ws) localStorage.setItem('skopos:workspace', ws); else localStorage.removeItem('skopos:workspace');
    this.refresh();
  },
  // A persisted workspace selection can outlive the key's scope (revoked,
  // re-scoped) or the registry entry — drop it instead of filtering every
  // view to an empty list with no indication why.
  sanitizeActiveWorkspace() {
    if (this.activeWorkspace && !this.workspaceOptions().some((o: { id: string }) => o.id === this.activeWorkspace)) {
      this.activeWorkspace = '';
      localStorage.removeItem('skopos:workspace');
    }
  },

  // ---- workspace dropdown (searchable picker) ----
  // Native <select> cannot search and truncates long repo paths; the header
  // control opens a panel with a filter input instead. Panel visibility (and
  // the panel's static sub-elements) are driven IMPERATIVELY: in this
  // @alpinejs/csp build x-show effects on these elements evaluate once and
  // never re-run on state writes (sixth silent-mode variant; the modal x-shows
  // work, nothing about position or expression shape explains the split), so
  // display is owned by syncWorkspaceMenu — the same pattern as
  // syncInboxExpansion. Option rows keep working reactively via a composite
  // :key that re-creates rows on selection/highlight changes.
  openWorkspaceMenu() {
    this.workspaceSearch = '';
    this.workspaceMenuOpen = true;
    this.wsSearchChanged();
    this.syncWorkspaceMenu();
    this.$nextTick(() => { const el = document.getElementById('ws-menu-search'); if (el) el.focus(); });
  },
  closeWorkspaceMenu() {
    this.workspaceMenuOpen = false;
    this.syncWorkspaceMenu();
  },
  toggleWorkspaceMenu() { if (this.workspaceMenuOpen) this.closeWorkspaceMenu(); else this.openWorkspaceMenu(); },
  syncWorkspaceMenu() {
    this.$nextTick(() => {
      const open = this.workspaceMenuOpen;
      document.querySelectorAll('[data-ws-menu-layer]').forEach((el) => { (el as HTMLElement).style.display = open ? '' : 'none'; });
      const empty = document.querySelector('[data-ws-empty]') as HTMLElement | null;
      if (empty) empty.style.display = (!open || this.workspaceFilteredOptions().length > 0) ? 'none' : '';
      const allBtn = document.querySelector('[data-ws-allopt]') as HTMLElement | null;
      if (allBtn) {
        allBtn.classList.toggle('bg-cyan-600/15', !this.activeWorkspace);
        allBtn.classList.toggle('text-cyan-300', !this.activeWorkspace);
        allBtn.classList.toggle('text-zinc-300', !!this.activeWorkspace);
      }
      const allCheck = document.querySelector('[data-ws-allopt-check]') as HTMLElement | null;
      if (allCheck) allCheck.style.display = this.activeWorkspace ? 'none' : '';
    });
  },
  wsSearchChanged() {
    const first = this.workspaceFilteredOptions()[0];
    this.workspaceHighlightId = first ? first.id : '';
    this.syncWorkspaceMenu();
  },
  workspaceFilteredOptions(): { id: string; label: string }[] {
    const q = this.workspaceSearch.trim().toLowerCase();
    const opts = this.workspaceOptions();
    if (!q) return opts;
    return opts.filter((o: { id: string; label: string }) => o.label.toLowerCase().includes(q) || o.id.toLowerCase().includes(q));
  },
  pickWorkspace(id: string) {
    this.closeWorkspaceMenu();
    this.setWorkspace(id);
  },
  workspaceKeyNav(ev: KeyboardEvent) {
    if (ev.key === 'Escape') { ev.preventDefault(); this.closeWorkspaceMenu(); return; }
    const opts = this.workspaceFilteredOptions();
    if (opts.length === 0) return;
    const idx = opts.findIndex((o: { id: string }) => o.id === this.workspaceHighlightId);
    if (ev.key === 'ArrowDown' || ev.key === 'ArrowUp') {
      ev.preventDefault();
      const next = (idx + (ev.key === 'ArrowDown' ? 1 : -1) + opts.length) % opts.length;
      this.workspaceHighlightId = opts[next].id;
      this.$nextTick(() => {
        const el = document.querySelector('[data-ws-opt="' + CSS.escape(this.workspaceHighlightId) + '"]');
        if (el) (el as HTMLElement).scrollIntoView({ block: 'nearest' });
      });
    } else if (ev.key === 'Enter' && this.workspaceHighlightId) {
      ev.preventDefault();
      this.pickWorkspace(this.workspaceHighlightId);
    }
  },

  // ---- workspaces ----
  async fetchWorkspaces() {
    try {
      const res = await this.authFetch('/api/workspaces');
      if (res.ok) this.registeredWorkspaces = (await res.json()) ?? [];
    } catch { /* non-fatal */ }
  },
  workspaceOptions(): { id: string; label: string }[] {
    const map = new Map<string, string>();
    if (this.whoami && !this.whoami.root) {
      // Scoped keys see exactly their slice of the registry — the server
      // enforces it; the UI mirrors it so the filter is honest.
      for (const w of this.whoami.workspaces) map.set(w.id, w.name || this.compactWorkspace(w.id));
      return [...map.entries()].map(([id, label]) => ({ id, label }));
    }
    for (const w of this.registeredWorkspaces) map.set(w.id, w.name || this.compactWorkspace(w.id));
    for (const ws of this.workspaces) if (!map.has(ws)) map.set(ws, ws);
    return [...map.entries()].map(([id, label]) => ({ id, label }));
  },
  // writeWorkspace resolves the workspace for a write: the active filter if
  // it is a concrete workspace, else the first option. Writes are
  // workspace-scoped on the server; '' means "block the write".
  writeWorkspace(): string {
    if (this.activeWorkspace) return this.activeWorkspace;
    return this.workspaceOptions()[0]?.id || '';
  },
  // Display form of a workspace id: drop a leading host-like segment
  // (github.com/, gitlab.example.com/…). The host is shared noise — right-side
  // truncation eats the org/repo tail, which is the part that actually
  // distinguishes workspaces. Registry names (when set) bypass this entirely.
  // Unless another known workspace has the same tail: then the host is what
  // tells them apart (github.com/acme/shop and a gitea mirror's
  // gitea.example.com:2223/acme/shop both read acme/shop, and a
  // key got scoped to the wrong one), so both keep their full id.
  compactWorkspace(id: string): string {
    const tail = workspaceTail(id);
    if (tail === id) return id;
    const known = new Set<string>([
      ...this.registeredWorkspaces.map((w: { id: string }) => w.id),
      ...(this.whoami?.workspaces ?? []).map((w: { id: string }) => w.id),
      ...this.workspaces,
    ]);
    for (const other of known) if (other !== id && workspaceTail(other) === tail) return id;
    return tail;
  },
  workspaceLabel(id?: string): string {
    if (!id) return '';
    const w = this.registeredWorkspaces.find((r: { id: string; name?: string }) => r.id === id);
    return w?.name || this.compactWorkspace(id);
  },
  // Auto-register any workspace seen in sessions so it persists in the DB.
  async autoRegisterWorkspaces() {
    const registered = new Set(this.registeredWorkspaces.map((w: { id: string }) => w.id));
    const seen = new Set(this.sessions.map((s: SessionSummary) => s.workspace).filter(Boolean));
    const newOnes = [...seen].filter((ws) => !registered.has(ws));
    if (newOnes.length === 0) return;
    for (const ws of newOnes) {
      try {
        await this.authFetch('/api/workspaces', { method: 'POST', body: JSON.stringify({ id: ws }) });
      } catch { /* non-fatal */ }
    }
    await this.fetchWorkspaces();
  },
  async refresh() {
    this.loading = true;
    try {
      const res = await this.authFetch('/api/sessions' + wsParam(this.activeWorkspace));
      if (res.ok) this.sessions = (await res.json()) ?? [];
    } catch { /* keep previous sessions */ } finally { this.loading = false; }
    const seen = new Set([...(this.workspaces ?? []), ...this.sessions.map((s: SessionSummary) => s.workspace).filter(Boolean)]);
    this.workspaces = [...seen];
    await this.fetchWhoami();
    this.fetchNeedsYou();
    await this.fetchWorkspaces();
    await this.autoRegisterWorkspaces();
    this.sanitizeActiveWorkspace();
    this.syncSessionPurge();
    if (!this.selectedSessionId && this.sessions.length > 0) this.selectedSessionId = this.sessions[0].id;
    if (this.selectedSessionId) {
      const match = this.sessions.find((s: SessionSummary) => s.id === this.selectedSessionId);
      if (!match) { this.selectedSessionId = ''; this.selectedSession = null; localStorage.removeItem('skopos:session'); }
      else { try { await this.selectSession(this.selectedSessionId); } catch { /* non-fatal */ } }
    }
    // Always reload the active view so workspace switches are reflected.
    if (this.activeView === 'blackboard') await this.fetchBundle();
    else if (this.activeView === 'plans') await this.fetchPlans();
    else if (this.activeView === 'inbox') await this.fetchInbox();
    else if (this.activeView === 'index') await this.fetchIndexStatus();
    else if (this.activeView === 'keys') await this.fetchKeys();
    else if (this.activeView === 'workspaces') { await this.fetchWorkspaces(); await this.fetchGroups(); await this.fetchMcpDefs(); if (this.whoamiIsRoot()) await this.fetchKeys(); }
    else if (this.activeView === 'workers') await this.fetchWorkers();
    else if (this.activeView === 'webhooks') { await this.fetchWebhooks(); if (this.whoamiIsRoot()) await this.fetchGroups(); }
  },
  async selectSession(id: string) {
    this.selectedSessionId = id; localStorage.setItem('skopos:session', id);
    const res = await this.authFetch(`/api/sessions/${encodeURIComponent(id)}`);
    if (res.ok) this.selectedSession = await res.json();
  },

  // ---- blackboard ----
  async fetchBundle() {
    this.blackboardLoading = true;
    try {
      const res = await this.authFetch('/api/blackboard/entries' + bundleParams(this.activeWorkspace, this.blackboardBranch));
      if (!res.ok) { this.entries = []; return; }
      const bundle: Bundle = await res.json(); this.entries = bundle.entries ?? [];
    } catch { this.entries = []; }
    finally { this.blackboardLoading = false; }
  },
  async promoteEntry(id: string) {
    const res = await this.authFetch(`/api/blackboard/entries/${encodeURIComponent(id)}/promote`, { method: 'PATCH' });
    if (!await this.handleBad(res, 'Promote failed')) return;
    this.notify('Entry promoted', 'success'); await this.fetchBundle();
  },
  openEntryModal() {
    this.entryForm = emptyEntryForm(this.blackboardBranch); this.entryErrors = {};
    this.showEntryModal = true;
  },
  closeEntryModal() { this.showEntryModal = false; },
  async submitEntry() {
    const f = this.entryForm, e: Record<string, string> = {};
    if (!f.title.trim()) e.title = 'Title is required.';
    if (f.scope === 'branch' && !f.branch_name.trim()) e.branch_name = 'Branch name is required for branch scope.';
    if (f.scope === 'session' && !f.session_id.trim()) e.session_id = 'Session ID is required for session scope.';
    this.entryErrors = e;
    if (Object.keys(e).length) return;
    this.entrySaving = true;
    try {
      const body: Record<string, string> = { scope: f.scope, entry_type: f.entry_type, title: f.title.trim(), content: f.content.trim(), author_agent_id: UI_AUTHOR };
      body.workspace_id = this.writeWorkspace();
      if (!body.workspace_id) { this.notify('Writes are workspace-scoped — select a workspace first', 'error'); return; }
      if (f.scope === 'branch') body.branch_name = f.branch_name.trim();
      if (f.scope === 'session') body.session_id = f.session_id.trim();
      if (f.code_ref.trim()) body.code_ref = f.code_ref.trim();
      const res = await this.authFetch('/api/blackboard/entries', { method: 'POST', body: JSON.stringify(body) });
      if (!await this.handleBad(res, 'Entry not written')) return;
      this.notify('Entry written', 'success'); this.showEntryModal = false; await this.fetchBundle();
    } finally { this.entrySaving = false; }
  },
  entriesByType() {
    const order = ['bug', 'debt', 'warning', 'finding', 'decision', 'context'];
    const labels: Record<string, string> = { bug: 'Bugs', debt: 'Tech Debt', warning: 'Warnings', finding: 'Findings', decision: 'Decisions', context: 'Context' };
    const colors: Record<string, string> = { bug: 'text-rose-400', debt: 'text-amber-400', warning: 'text-amber-400', finding: 'text-cyan-400', decision: 'text-emerald-400', context: 'text-zinc-400' };
    return order.map(t => ({ type: t, label: labels[t] ?? t, colorClass: colors[t] ?? 'text-zinc-400', items: this.entries.filter((e: Entry) => e.entry_type === t) })).filter(g => g.items.length > 0);
  },

  // ---- plans ----
  async fetchPlans() {
    this.plansLoading = true;
    try {
      const res = await this.authFetch('/api/plans' + bundleParams(this.activeWorkspace, this.plansBranch));
      if (!res.ok) { this.plans = []; return; }
      this.plans = (await res.json()) ?? [];
    } catch { this.plans = []; } finally { this.plansLoading = false; }
    // Keep the open plan's items in sync when the list is refreshed (e.g. via SSE).
    if (this.expandedPlan) await this.reloadPlan(this.expandedPlan.id);
  },
  // Same CSP constraint as isInboxExpanded: method-call x-show only.
  isPlanExpanded(plan: Plan): boolean { return this.expandedPlan !== null && this.expandedPlan.id === plan.id; },
  async togglePlan(plan: Plan) {
    if (this.expandedPlan?.id === plan.id) { this.expandedPlan = null; this.syncPlanExpansion(); return; }
    const res = await this.authFetch(`/api/plans/${encodeURIComponent(plan.id)}`);
    if (res.ok) this.expandedPlan = await res.json();
    this.syncPlanExpansion();
  },
  // Re-fetch the open plan and keep it expanded (used after item/dependency changes).
  async reloadPlan(planId: string) {
    if (this.expandedPlan?.id !== planId) return;
    const res = await this.authFetch(`/api/plans/${encodeURIComponent(planId)}`);
    if (res.ok) this.expandedPlan = await res.json();
    this.syncPlanExpansion();
  },
  otherItems(item: PlanItem): PlanItem[] {
    const deps = new Set(item.depends_on ?? []);
    return (this.expandedPlan?.items ?? []).filter(i => i.id !== item.id && !deps.has(i.id));
  },
  async archivePlan(planId: string) {
    const res = await this.authFetch(`/api/plans/${encodeURIComponent(planId)}`, { method: 'PATCH', body: JSON.stringify({ status: 'archived' }) });
    if (!await this.handleBad(res, 'Could not archive plan')) return;
    this.notify('Plan archived', 'success');
    if (this.expandedPlan?.id === planId) this.expandedPlan = null;
    await this.fetchPlans();
  },
  // Both take the select element (single-statement @change handlers — the
  // CSP evaluator silently fails to bind two-statement expressions) and
  // reset it to the placeholder after reading the value.
  async updateItemStatus(planId: string, itemId: string, el: HTMLSelectElement) {
    const status = el.value;
    el.value = '';
    if (!status) return;
    const res = await this.authFetch(`/api/plans/${encodeURIComponent(planId)}/items/${encodeURIComponent(itemId)}`, { method: 'PATCH', body: JSON.stringify({ status }) });
    if (!await this.handleBad(res, 'Status not updated')) return;
    this.notify('Item status updated', 'success'); await this.reloadPlan(planId);
  },
  async addPlanDependency(planId: string, itemId: string, el: HTMLSelectElement) {
    const dependsOnId = el.value;
    el.value = '';
    if (!dependsOnId) return;
    const res = await this.authFetch(`/api/plans/${encodeURIComponent(planId)}/items/${encodeURIComponent(itemId)}/dependencies`, { method: 'POST', body: JSON.stringify({ depends_on_item_id: dependsOnId }) });
    if (!await this.handleBad(res, 'Dependency not added')) return;
    this.notify('Dependency added', 'success'); await this.reloadPlan(planId);
  },
  openPlanModal() {
    this.planForm = emptyPlanForm(this.plansBranch); this.planErrors = {};
    this.pendingConvertItemId = '';
    this.pendingConvertWorkspaceId = '';
    this.showPlanModal = true;
  },
  async submitPlan() {
    const e: Record<string, string> = {};
    if (!this.planForm.name.trim()) e.name = 'Name is required.';
    this.planErrors = e;
    if (Object.keys(e).length) return;
    this.planSaving = true;
    try {
      const body: Record<string, string> = { name: this.planForm.name.trim(), description: this.planForm.description.trim(), author_agent_id: UI_AUTHOR };
      if (this.planForm.branch_name.trim()) body.branch_name = this.planForm.branch_name.trim();
      // A conversion plan is pinned to the item's workspace; plain plan
      // creation falls back to the workspace filter.
      body.workspace_id = this.pendingConvertItemId ? this.pendingConvertWorkspaceId : this.writeWorkspace();
      if (!body.workspace_id) { this.notify('Writes are workspace-scoped — select a workspace first', 'error'); return; }
      const res = await this.authFetch('/api/plans', { method: 'POST', body: JSON.stringify(body) });
      if (!await this.handleBad(res, 'Plan not created')) return;
      // Plan created from an inbox item: link it back (convert) and refresh
      // the inbox view too.
      if (this.pendingConvertItemId) {
        const created = await res.json();
        const cres = await this.authFetch('/api/inbox/' + encodeURIComponent(this.pendingConvertItemId) + '/convert', { method: 'POST', body: JSON.stringify({ plan_id: created.id }) });
        this.pendingConvertItemId = '';
        if (!await this.handleBad(cres, 'Plan created but the item was not converted')) return;
        this.notify('Plan created — item converted', 'success');
        if (this.expandedInboxId) { this.expandedInboxId = ''; this.expandedInbox = null; }
        await this.fetchInbox();
      } else {
        this.notify('Plan created', 'success');
      }
      this.showPlanModal = false; await this.fetchPlans();
    } finally { this.planSaving = false; }
  },
  closePlanModal() { this.showPlanModal = false; this.pendingConvertItemId = ''; this.pendingConvertWorkspaceId = ''; },
  openItemModal() {
    this.itemForm = emptyItemForm(); this.itemErrors = {};
    this.showItemModal = true;
  },
  closeItemModal() { this.showItemModal = false; },
  async submitItem() {
    const planId = this.expandedPlan?.id;
    if (!planId) return;
    const e: Record<string, string> = {};
    if (!this.itemForm.title.trim()) e.title = 'Title is required.';
    this.itemErrors = e;
    if (Object.keys(e).length) return;
    this.itemSaving = true;
    try {
      const body: Record<string, unknown> = { title: this.itemForm.title.trim(), description: this.itemForm.description.trim() };
      if (this.itemForm.phase.trim()) body.phase = this.itemForm.phase.trim();
      if (this.itemForm.depends_on) body.depends_on = [this.itemForm.depends_on];
      const res = await this.authFetch(`/api/plans/${encodeURIComponent(planId)}/items`, { method: 'POST', body: JSON.stringify(body) });
      if (!await this.handleBad(res, 'Item not added')) return;
      this.notify('Item added', 'success'); this.showItemModal = false; await this.reloadPlan(planId);
    } finally { this.itemSaving = false; }
  },

  // ---- inbox ----
  async fetchInbox() {
    this.inboxLoading = true;
    try {
      const p = new URLSearchParams();
      if (this.activeWorkspace) p.set('workspace', this.activeWorkspace);
      // The board shows every lane at once; the status chips only filter the
      // list layout (the chip selection is kept and restored on switch-back).
      if (this.inboxLayout === 'list' && this.inboxStatus) p.set('status', this.inboxStatus);
      if (this.inboxTagFilter) p.set('tag', this.inboxTagFilter);
      const q = this.inboxQuery.trim();
      if (q) p.set('q', q);
      const qs = p.size ? '?' + p.toString() : '';
      const res = await this.authFetch('/api/inbox' + qs);
      // Transient failure (server restarting mid-redeploy, reconnect window):
      // KEEP the current rows — blanking the board on a hiccup reads as data
      // loss. Connection state is already visible in the header pill; a 401
      // funnels into the key modal via authFetch.
      if (!res.ok) return;
      this.inboxItems = (await res.json()) ?? [];
      this.syncInboxLanes();
    } catch { /* network hiccup: keep the stale rows */ } finally { this.inboxLoading = false; }
    if (this.expandedInboxId) await this.reloadInboxItem(this.expandedInboxId);
  },
  setInboxLayout(l: 'list' | 'lanes') {
    if (this.inboxLayout === l) return;
    this.inboxLayout = l;
    localStorage.setItem('skopos:inboxLayout', l);
    this.fetchInbox();
  },
  setInboxStatus(s: string) { this.inboxStatus = s as typeof this.inboxStatus; this.fetchInbox(); },
  setInboxTagFilter(t: string) { this.inboxTagFilter = this.inboxTagFilter === t ? '' : t; this.fetchInbox(); },
  // The toolbar tag <select> keeps a plain value (no toggle — picking the
  // already-active option fires no change event anyway); item chips keep the
  // toggle behavior and x-model syncs the select either way.
  selectInboxTagFilter(t: string) { this.inboxTagFilter = t; this.fetchInbox(); },
  setInboxQuery(v: string) { this.inboxQuery = v; this.fetchInbox(); },
  // Display-layer sort (list rows + board lanes). 'priority' keeps the
  // server's truth (prioritized first, unprioritized tail newest-first);
  // the date sorts are a client-side lens over the same fetch — priority
  // ranks still exist underneath (#n badges, reorder), they just stop
  // driving the view. Rank drops are disabled in date views: inserting
  // "before this card" is meaningless when the view is not in rank order.
  setInboxSort(s: string) {
    this.inboxSort = s as typeof this.inboxSort;
    localStorage.setItem('skopos:inboxSort', s);
    // List rows key on item.id + inboxSort so row-level directives re-evaluate
    // when the lens changes; re-created rows lose the imperatively-injected
    // markdown, so an expanded item is re-fetched and re-injected.
    this.$nextTick(() => { if (this.expandedInboxId) this.reloadInboxItem(this.expandedInboxId); });
  },
  sortInbox(items: InboxItem[]): InboxItem[] {
    if (this.inboxSort === 'priority') return items;
    // RFC3339Nano strings do NOT compare lexicographically (truncated
    // fractions: ".5Z" > ".512345678Z") — parse to ms and tie-break on the
    // UUIDv7 id, same fix as the server-side ordering.
    const dir = this.inboxSort === 'newest' ? -1 : 1;
    const arr = [...items];
    arr.sort((a, b) => dir * (Date.parse(a.created_at) - Date.parse(b.created_at)) || a.id.localeCompare(b.id));
    return arr;
  },
  sortedInboxItems(): InboxItem[] { return this.sortInbox(this.inboxItems); },
  // Tag options derive from the CURRENT result set: honest about what is
  // visible, but a filter that empties the list hides the other tags — clear
  // via "All tags" in that case.
  inboxAllTags(): string[] {
    const tags = new Set<string>();
    for (const i of this.inboxItems) for (const t of i.tags || []) tags.add(t);
    return [...tags].sort();
  },
  inboxStatusChips(): { key: string; label: string }[] {
    return [
      { key: 'needs_you', label: 'Needs you' },
      { key: 'open', label: 'Open' },
      { key: 'in_progress', label: 'In progress' },
      { key: 'workflow', label: 'Workflow' },
      { key: 'converted', label: 'Converted' },
      { key: 'done', label: 'Done' },
      { key: 'discarded', label: 'Discarded' },
      { key: '', label: 'All' },
    ];
  },
  // Items waiting for an approver (plan gate, sign-off, blocked/failed) in
  // the active workspace (all of the key's scope when none is picked), for
  // the sidebar badge and the tab title.
  async fetchNeedsYou() {
    try {
      // The same scope as the list the badge opens: the active workspace,
      // or every workspace in the key's scope under "All workspaces".
      const p = new URLSearchParams({ status: 'needs_you' });
      if (this.activeWorkspace) p.set('workspace', this.activeWorkspace);
      const res = await this.authFetch('/api/inbox?' + p.toString());
      if (!res.ok) return;
      this.needsYouCount = (((await res.json()) as InboxItem[]) ?? []).length;
    } catch { return; }
    if (!this.baseTitle) this.baseTitle = document.title.replace(/^\(\d+\) /, '');
    document.title = (this.needsYouCount ? '(' + this.needsYouCount + ') ' : '') + this.baseTitle;
  },
  // The sidebar badge opens the inbox list on the Needs you chip.
  openNeedsYou() {
    this.inboxStatus = 'needs_you';
    if (this.inboxLayout !== 'list') { this.inboxLayout = 'list'; localStorage.setItem('skopos:inboxLayout', 'list'); }
    if (this.activeView === 'inbox') this.fetchInbox();
    else this.switchView('inbox');
  },
  inboxOpenCount(): number {
    return this.inboxItems.filter((i: InboxItem) => i.status === 'open').length;
  },
  // x-show inside x-for MUST be a method call: the CSP build's evaluator
  // does not re-run compound `expanded && expanded.id === item.id`
  // expressions when the state changes (same class as the dead dropdowns —
  // silently evaluated once, never again).
  isInboxExpanded(item: InboxItem): boolean { return this.expandedInboxId === item.id; },
  // Imperative expansion sync: the CSP build's x-show effects inside x-for
  // rows do not reliably re-run when outer state (expandedInboxId) changes,
  // so the containers' display is driven directly alongside the markdown
  // injection (same rationale as the [data-md-body] pattern).
  syncInboxExpansion() {
    this.$nextTick(() => {
      document.querySelectorAll('[data-md-row]').forEach((row) => {
        (row as HTMLElement).style.display = row.getAttribute('data-md-row') === this.expandedInboxId ? '' : 'none';
      });
    });
  },
  syncPlanExpansion() {
    this.$nextTick(() => {
      document.querySelectorAll('[data-plan-row]').forEach((row) => {
        (row as HTMLElement).style.display = this.expandedPlan !== null && row.getAttribute('data-plan-row') === this.expandedPlan.id ? '' : 'none';
      });
    });
  },
  async toggleInboxItem(item: InboxItem) {
    if (this.expandedInboxId === item.id) { this.expandedInboxId = ''; this.expandedInbox = null; this.syncInboxExpansion(); return; }
    // Set the id first: reloadInboxItem re-checks it after its await, so a
    // collapse during the fetch is not resurrected by the late response.
    this.expandedInboxId = item.id;
    this.syncInboxExpansion();
    await this.reloadInboxItem(item.id);
  },
  async reloadInboxItem(id: string) {
    const res = await this.authFetch('/api/inbox/' + encodeURIComponent(id));
    if (this.expandedInboxId !== id) return; // collapsed while fetching
    if (!res.ok) { this.expandedInboxId = ''; this.expandedInbox = null; return; }
    this.expandedInboxId = id;
    this.expandedInbox = (await res.json()) as InboxDetail;
    // The Alpine CSP build prohibits the html directive, and $refs does not
    // resolve inside x-for templates — so the server-sanitized content_html
    // is injected imperatively by querying the row's data attribute once the
    // (x-show, never x-if) block is rendered.
    const html = this.expandedInbox.content_html;
    const rowId = id;
    this.$nextTick(() => {
      // Both layouts keep their copy of the row in the DOM (x-show only
      // hides); inject into every copy so whichever is visible renders.
      document.querySelectorAll('[data-md-body="' + CSS.escape(rowId) + '"]').forEach((el) => { el.innerHTML = html; });
    });
    this.syncInboxExpansion();
  },
  // Preview modal: full item detail without expanding (workflow cards are
  // frozen for inline editing; the modal is the universal read view).
  async openInboxPreview(item: InboxItem) {
    this.showInboxPreview = true;
    this.previewLoading = true;
    this.previewItem = null;
    const res = await this.authFetch('/api/inbox/' + encodeURIComponent(item.id) + '?parts=1');
    if (!this.showInboxPreview) return; // closed while fetching
    this.previewLoading = false;
    if (!res.ok) { this.notify('Could not load the item', 'error'); this.closeInboxPreview(); return; }
    this.previewItem = (await res.json()) as InboxDetail;
    this.previewRoundOpen = {};
    this.questionAnswers = {};
    this.answersSavedFor = '';
    this.previewFindings = [];
    this.previewFindingsSend = '';
    this.findingPicks = {};
    this.findingFold = {};
    this.waive = { id: '', note: '', busy: false };
    this.injectPreviewContent();
    this.workflowNote = '';
    this.previewStatusSeen = '';
    this.previewPipe = null;
    this.syncPreviewSections();
    this.secondOpinion = { runner: '', force: false, busy: false, runners: [] };
    this.fetchPreviewRuns(item.id);
    this.fetchPreviewRevision();
    // Workers and the pipeline feed both the tracker and the second-opinion
    // row: loaded once, then the row's defaults come from them.
    Promise.all([this.fetchPreviewPipe(), this.fetchPreviewExtras(item.id)]).then(() => this.fillSecondOpinion());
    if (previewPoll) clearInterval(previewPoll);
    previewPoll = setInterval(() => this.pollPreview(), 20000);
  },
  // Every 20s while the preview is open on an item the pipeline is working
  // on: refetch runs and workers. Inbox events only fire on item changes, so
  // a review run starting or a worker dying would otherwise go unseen.
  // The item is re-read too (reloadPreview swaps it only when it changed):
  // a transition missed while the event stream was down — a server
  // restart — would otherwise leave the tracker on the old phase.
  pollPreview() {
    const item = this.previewItem;
    if (!this.showInboxPreview || !item) return;
    if (['open', 'converted', 'done', 'discarded'].includes(item.status)) return;
    if (this.workflowNote) { this.fetchPreviewRuns(item.id); this.fetchPreviewExtras(item.id); return; }
    this.reloadPreview();
  },
  // Workers (stuck-run warning) and the audit trail (History): refetched with
  // the runs on every live refresh.
  async fetchPreviewExtras(id: string) {
    const [wres, tres, qres, fres] = await Promise.all([
      this.authFetch('/api/workers'),
      this.authFetch('/api/inbox/' + encodeURIComponent(id) + '/timeline'),
      this.authFetch('/api/inbox/' + encodeURIComponent(id) + '/questions'),
      this.authFetch('/api/inbox/' + encodeURIComponent(id) + '/findings'),
    ]);
    if (!this.showInboxPreview || !this.previewItem || this.previewItem.id !== id) return;
    if (wres.ok) this.previewWorkers = ((await wres.json()) as WorkerSeen[]) ?? [];
    if (tres.ok) this.previewTimeline = ((await tres.json()) as AuditEntry[]) ?? [];
    if (qres.ok) this.previewQuestions = (((await qres.json()) as { sets: QASet[] }).sets) ?? [];
    if (fres.ok) {
      const rec = (await fres.json()) as { findings: FindingRec[]; send_back?: string };
      this.previewFindings = rec.findings ?? [];
      this.previewFindingsSend = rec.send_back || '';
    }
  },
  // Section defaults follow the status: the description is open until the
  // plan is approved, the run history is closed. Applied on open and on a
  // status change only.
  syncPreviewSections() {
    const st = this.previewItem ? this.previewItem.status : '';
    if (st === this.previewStatusSeen) return;
    // The item moved on: ticks and a half-made waive were for the gate it
    // left.
    if (this.previewStatusSeen) { this.findingPicks = {}; this.waive = { id: '', note: '', busy: false }; }
    this.previewStatusSeen = st;
    this.previewDescOpen = ['open', 'in_progress', 'queued', 'planning', 'awaiting_approval', 'converted'].includes(st);
    this.previewRunsOpen = false;
  },
  // CSP: the same imperative injection as the expansion rows — the
  // sanitized HTML goes into data-attribute nodes: the author's text into
  // [data-md-preview], each section into its [data-sec-html] (rendered by
  // the rounds x-for, hence the tick).
  injectPreviewContent() {
    const item = this.previewItem;
    if (!item) return;
    const parts = item.content_parts;
    this.$nextTick(() => {
      document.querySelectorAll('[data-md-preview]').forEach((el) => { el.innerHTML = parts ? parts.brief_html : item.content_html; });
      if (!parts) return;
      parts.rounds.forEach((r, ri) => r.sections.forEach((sec, si) => {
        document.querySelectorAll('[data-sec-html="' + CSS.escape(this.sectionKey(item.id, ri, si)) + '"]').forEach((el) => { el.innerHTML = sec.html; });
      }));
    });
  },
  sectionKey(itemId: string, round: number, section: number): string { return itemId + '|' + round + '|' + section; },
  // The description's rounds (a plan and its questions, a review, each with
  // The approver's notes on it): the newest open, older ones folded unless the
  // user opened them. A repeated kind is numbered ("Plan · round 2 of 3").
  previewRounds(): RoundView[] {
    const item = this.previewItem;
    const rounds = item && item.content_parts ? item.content_parts.rounds : [];
    if (!item || rounds.length === 0) return [];
    const total: Record<string, number> = {};
    rounds.forEach((r) => { total[r.kind] = (total[r.kind] || 0) + 1; });
    return rounds.map((r, ri) => {
      const key = item.id + '|r' + ri;
      const set = this.previewRoundOpen[key];
      return {
        key,
        open: set === undefined ? ri === rounds.length - 1 : set,
        title: r.title + (total[r.kind] > 1 ? ' · round ' + r.n + ' of ' + total[r.kind] : ''),
        meta: r.sections.map((sec) => sec.heading.replace(/ — (skopos )?worker$/, '')).join(' · '),
        sections: r.sections.map((sec, si) => ({ key: this.sectionKey(item.id, ri, si), heading: sec.heading })),
      };
    });
  },
  // Remember a round the user toggled; a state equal to the default is
  // forgotten, so an older round folds once a newer one arrives.
  onRoundToggle(key: string, e: globalThis.Event) {
    const open = (e.target as HTMLDetailsElement).open;
    const rounds = this.previewRounds();
    const newest = rounds.length > 0 && rounds[rounds.length - 1].key === key;
    const next = { ...this.previewRoundOpen };
    if (open === newest) delete next[key]; else next[key] = open;
    this.previewRoundOpen = next;
  },
  // The questions waiting for the approver, from the record: the open questions
  // of the open sets, while the item awaits approval or is blocked. Keyed
  // by question id; answers go back by id.
  openQuestionGroups(): QuestionGroup[] {
    const item = this.previewItem;
    if (!item || !(this.workflowShowApprove() || this.workflowShowRetry())) return [];
    return this.previewQuestions.filter((s: QASet) => s.open).map((s: QASet) => {
      const who = s.label || (s.asker === 'implementer' ? 'the implementer' : '');
      return {
        key: s.id,
        title: who ? 'Questions from ' + who : (s.asker === 'worker' ? 'A question from the worker' : 'Questions from the planner'),
        label: who,
        questions: s.questions.filter((q: QAQuestion) => q.state === 'open').map((q: QAQuestion) => ({
          key: q.id, n: q.n, text: q.text,
          options: (q.options || []).map((o: string, oi: number) => ({ key: q.id + '|o' + oi, letter: optionLetter(oi), text: o })),
        })),
      };
    });
  },
  answerPick(key: string): string { const a = this.questionAnswers[key]; return a ? a.pick : ''; },
  answerText(key: string): string { const a = this.questionAnswers[key]; return a ? a.text : ''; },
  // Picking the picked option again clears it.
  pickAnswer(key: string, letter: string) {
    const a = this.questionAnswers[key] || { pick: '', text: '' };
    this.questionAnswers = { ...this.questionAnswers, [key]: { pick: a.pick === letter ? '' : letter, text: a.text } };
  },
  setAnswerText(key: string, text: string) {
    const a = this.questionAnswers[key] || { pick: '', text: '' };
    this.questionAnswers = { ...this.questionAnswers, [key]: { pick: a.pick, text } };
  },
  answerOptClass(key: string, letter: string): string { return this.answerPick(key) === letter ? 'q-opt q-opt-on' : 'q-opt'; },
  answerOptPressed(key: string, letter: string): string { return this.answerPick(key) === letter ? 'true' : 'false'; },
  answeredCount(): string {
    const all = this.openQuestionGroups().flatMap((g) => g.questions);
    const n = all.filter((q) => this.answerPick(q.key) !== '' || this.answerText(q.key).trim() !== '').length;
    const actions = this.workflowShowRetry() ? 'Retry' : 'Approve or ' + this.requestChangesLabel();
    return n + ' of ' + all.length + ' answered · sent with ' + actions + ', or saved on their own';
  },
  // The answers by question id: the picked option and/or the words.
  answerItems(): AnswerItem[] {
    const out: AnswerItem[] = [];
    for (const g of this.openQuestionGroups()) {
      for (const q of g.questions) {
        const option = this.answerPick(q.key);
        const text = this.answerText(q.key).trim();
        if (option || text) out.push({ question_id: q.key, option: option || undefined, text: text || undefined });
      }
    }
    return out;
  },
  // Answers saved on their own in this preview, for this item: they are the
  // answer to the blocker, so Retry needs no more text.
  savedAnswers(): boolean { return !!this.previewItem && this.answersSavedFor === this.previewItem.id; },
  answersReady(): boolean { return !this.workflowBusy && this.answerItems().length > 0; },
  // Answers without moving the item: recorded now, read by the next run.
  async saveAnswers() {
    const item = this.previewItem;
    if (!item || !this.answersReady()) return;
    if (await this.postWorkflowAction(item.id, 'answers', { answers: this.answerItems() }, 'Answers saved')) this.answersSavedFor = item.id;
  },
  hasWorkflowInput(): boolean { return this.workflowNote.trim().length > 0 || this.answerItems().length > 0 || this.pickedFindings().length > 0; },
  async copyItemId() {
    const item = this.previewItem;
    if (!item) return;
    try {
      await navigator.clipboard.writeText(item.id);
      this.notify('Item ID copied', 'success');
    } catch {
      this.notify('Could not copy the item ID', 'error');
    }
  },
  onPreviewDescToggle(e: globalThis.Event) { this.previewDescOpen = (e.target as HTMLDetailsElement).open; },
  onPreviewRunsToggle(e: globalThis.Event) { this.previewRunsOpen = (e.target as HTMLDetailsElement).open; },
  async fetchPreviewPipe() {
    const item = this.previewItem;
    if (!item || !item.workspace_id) return;
    const res = await this.authFetch('/api/workspaces/' + encodeURIComponent(item.workspace_id) + '/pipeline');
    if (!res.ok || !this.previewItem || this.previewItem.id !== item.id) return;
    this.previewPipe = (await res.json()) as PipeView;
  },
  // ---- second opinion (advisory review of the head waiting for approval) ----
  secondOpinionApplies(): boolean {
    return !!this.previewItem && (this.previewItem.status === 'in_review' || this.previewItem.status === 'blocked');
  },
  // The second-opinion row's runner suggestions (from the workers in scope)
  // and default (the workspace's), from the loads the preview already made.
  // The runner field is only filled while empty, so a live refresh never
  // overwrites what the user typed.
  fillSecondOpinion() {
    const runners: string[] = [];
    for (const w of this.previewWorkers) for (const r of w.runners || []) if (!runners.includes(r)) runners.push(r);
    this.secondOpinion.runners = runners.sort();
    if (!this.secondOpinion.runner && this.previewPipe) this.secondOpinion.runner = this.previewPipe.effective_second_opinion_runner || '';
  },
  secondOpinionRunning(): RunRow | undefined {
    return this.previewRuns.find((r: RunRow) => r.stage === 'second_opinion' && ['queued', 'claimed', 'running', 'paused'].includes(r.status));
  },
  // Keyed row (CSP build: x-show on outer state inside the preview does not
  // re-run; a keyed x-for re-renders): [] when it doesn't apply.
  // Offered only where the server accepts one: in_review, or blocked by a
  // review/QA run (the newest run apart from second opinions) — never on an
  // item blocked by planning or implementation.
  secondOpinionOffered(): boolean {
    if (!this.previewItem) return false;
    if (this.previewItem.status === 'in_review') return true;
    if (this.previewItem.status !== 'blocked') return false;
    const last = this.previewRuns.find((r: RunRow) => r.stage !== 'second_opinion');
    return !!last && (last.stage === 'review' || last.stage === 'qa');
  },
  secondOpinionRow(): { key: string; running: string }[] {
    if (!this.secondOpinionOffered()) return [];
    const running = this.secondOpinionRunning();
    const last = this.previewRuns.find((r: RunRow) => r.stage !== 'second_opinion');
    return [{ key: (this.previewItem ? this.previewItem.status : '') + '|' + (last ? last.id : '') + '|' + (running ? running.id + running.status : 'idle'), running: running ? (running.runner || '') : '' }];
  },
  async requestSecondOpinion() {
    const item = this.previewItem;
    if (!item || this.secondOpinion.busy) return;
    this.secondOpinion.busy = true;
    try {
      const res = await this.authFetch('/api/inbox/' + encodeURIComponent(item.id) + '/second-opinion', {
        method: 'POST', body: JSON.stringify({ runner: this.secondOpinion.runner.trim(), force: this.secondOpinion.force, via: 'dashboard' }),
      });
      if (!await this.handleBad(res, 'Second opinion not queued')) return;
      const out = (await res.json()) as { runner: string; head_sha: string };
      this.notify('Second opinion queued: ' + out.runner + ' reviews ' + out.head_sha.slice(0, 7), 'success');
      this.secondOpinion.force = false;
      await this.fetchPreviewRuns(item.id);
    } finally { this.secondOpinion.busy = false; }
  },
  closeInboxPreview() { if (this.planView.open || Date.now() - plansClosedAt < 150) return; this.closeRunLog(); if (previewPoll) { clearInterval(previewPoll); previewPoll = null; } this.showInboxPreview = false; this.previewLoading = false; this.previewItem = null; this.previewRuns = []; this.previewRunsLoading = false; this.previewRevision = null; this.previewCandidates = []; this.previewApprovedRev = null; this.previewPipe = null; this.previewStatusSeen = ''; this.previewWorkers = []; this.previewTimeline = []; this.previewQuestions = []; this.workflowNote = ''; this.questionAnswers = {}; this.previewRoundOpen = {}; this.workflowBusy = false; },
  // The newest revision of the previewed item's plan, while it waits at the
  // gate (revisions list newest first).
  async fetchPreviewRevision() {
    const item = this.previewItem;
    this.previewRevision = null;
    this.previewCandidates = [];
    if (!item || !item.plan_id) { this.previewApprovedRev = null; return; }
    const res = await this.authFetch('/api/plans/' + encodeURIComponent(item.plan_id) + '/revisions');
    if (!res.ok || !this.previewItem || this.previewItem.id !== item.id) return;
    const revs = (await res.json()) as PlanRevisionInfo[]; // newest first
    // The tracker's "Plan approval" line: the newest locked revision.
    this.previewApprovedRev = revs.find((r: PlanRevisionInfo) => !!r.locked_at) || null;
    if (item.status !== 'awaiting_approval') return;
    const live = revs.filter((r: PlanRevisionInfo) => r.candidate_state !== 'superseded');
    this.previewRevision = live.length > 0 ? live[0] : null;
    // The current round = the newest candidate's group; its open ones.
    const newestCand = revs.find((r: PlanRevisionInfo) => !!r.group_id);
    this.previewCandidates = newestCand
      ? revs.filter((r: PlanRevisionInfo) => r.group_id === newestCand.group_id && r.candidate_state === 'open').reverse()
      : [];
  },
  // What Approve can bind at the gate, rendered through one keyed x-for
  // (re-renders reliably in the CSP build when the async load lands): the
  // plan revision, or one card per candidate of a multi-agent round.
  approveTargets(): ApproveTarget[] {
    if (!this.workflowShowApprove()) return [];
    if (this.previewCandidates.length > 0) {
      return this.previewCandidates.map((c: PlanRevisionInfo, i: number) => {
        const steps = (c.steps || []).filter(st => st.phase !== 'review').map(st => st.title);
        const skills = c.skills && c.skills.length ? ' · skills: ' + c.skills.join(', ') : '';
        const name = 'Candidate ' + (i + 1) + (c.label ? ' · ' + c.label : '');
        return { id: c.id, title: name, meta: steps.length + ' step' + (steps.length === 1 ? '' : 's') + skills, steps, candidate: true, hash: c.content_hash, toast: 'Approved ' + name };
      });
    }
    const r = this.previewRevision;
    if (!r) return [];
    const skills = r.skills && r.skills.length ? ' · skills: ' + r.skills.join(', ') : '';
    // The steps are listed for a single plan too: Approve locks exactly these.
    const steps = (r.steps || []).filter(st => st.phase !== 'review').map(st => st.title);
    return [{ id: r.id, title: 'Plan revision #' + r.revision_no, meta: this.plural(steps.length, 'step') + skills, steps, candidate: false, hash: r.content_hash, toast: 'Approved revision #' + r.revision_no }];
  },
  approveTarget(revisionId: string) {
    const item = this.previewItem;
    const t = this.approveTargets().find((x: ApproveTarget) => x.id === revisionId);
    if (!item || !t) return;
    const body: Record<string, unknown> = { revision_id: t.id, content_hash: t.hash };
    if (this.workflowNote.trim()) body.notes = this.workflowNote;
    const answers = this.answerItems();
    if (answers.length > 0) body.answers = answers;
    this.postWorkflowAction(item.id, 'approve', body, t.toast);
  },
  // Revise one candidate: a single planner re-plans from it with the notes.
  reviseCandidate(revisionId: string) {
    const item = this.previewItem;
    const t = this.approveTargets().find((x: ApproveTarget) => x.id === revisionId);
    if (!item || !t) return;
    if (!this.hasWorkflowInput()) { this.notify('Write what to change in the notes first', 'error'); return; }
    this.postWorkflowAction(item.id, 'request-changes', { notes: this.workflowNote, answers: this.answerItems(), revision_id: t.id }, 'Revising ' + t.title);
  },
  requestChangesLabel(): string { return this.previewCandidates.length > 0 ? 'Re-plan all' : 'Request changes'; },
  // Edit from the preview: capture the item BEFORE closing (close nulls
  // previewItem). One method because the CSP build's expression parser
  // accepts a single expression per handler — `a(); b()` throws.
  editFromPreview() {
    const item = this.previewItem;
    this.closeInboxPreview();
    if (item) this.openInboxEditModal(item);
  },
  // Runs of the previewed item — "where is this processed": stage, worker,
  // attempt, branch, head. Refetched on every inbox SSE event while the
  // modal is open (workflow transitions publish inbox events), so the rows
  // follow the pipeline without a dedicated runs stream.
  async fetchPreviewRuns(id: string) {
    if (!this.showInboxPreview) return;
    this.previewRunsLoading = true;
    const res = await this.authFetch('/api/runs?item_id=' + encodeURIComponent(id));
    if (!this.showInboxPreview || !this.previewItem || this.previewItem.id !== id) return;
    this.previewRunsLoading = false;
    if (!res.ok) { this.previewRuns = []; return; }
    this.previewRuns = (await res.json()) as RunRow[];
    this.followLog();
  },
  refreshPreviewRuns() { if (this.previewItem) this.fetchPreviewRuns(this.previewItem.id); },

  // ---- run log panel ----
  // A run's log: the last lines first, then each runlog event (or, with
  // the stream down, a 5s poll) reads what came after the cursor.
  logRun(): RunRow | undefined { return this.previewRuns.find((r: RunRow) => r.id === this.logRunId); },
  logLive(): boolean { const r = this.logRun(); return !!r && (r.status === 'claimed' || r.status === 'running'); },
  logTitle(): string {
    const r = this.logRun();
    if (!r) return 'Run log';
    return [this.statusText(r.stage), r.runner, r.focus, r.worker_id, 'try ' + r.attempt].filter(Boolean).join(' · ');
  },
  // from: set when the panel moved here on its own from another run.
  async openRunLog(id: string, from = '') {
    if (this.logRunId === id) return;
    this.closeRunLog();
    this.logRunId = id;
    this.logFrom = from;
    // A live run is watched, so the panel goes on to what runs next; a
    // finished run is being read, so it stays.
    this.logAuto = !!from || this.logLive();
    this.watchLog();
    this.logLoading = true;
    const abort = new AbortController();
    runLogAbort = abort;
    // A loop is about to read this run: followLog must not start a second.
    runLogLooping = true;
    let res: Response;
    try {
      res = await this.authFetch('/api/runs/' + encodeURIComponent(id) + '/log?tail=2000', { signal: abort.signal });
    } catch { if (runLogAbort === abort) runLogLooping = false; return; }
    if (abort.signal.aborted || this.logRunId !== id) return;
    this.logLoading = false;
    if (!res.ok) { runLogLooping = false; this.logError = res.status === 403 ? 'This key may not read run logs.' : 'Could not load the log (HTTP ' + res.status + ').'; return; }
    const page = await res.json() as { records: LogRecord[]; cursor: number; truncated: boolean };
    this.logRecords = this.keyed(page.records || []);
    this.logCursor = page.cursor || 0;
    this.logTruncated = !!page.truncated;
    this.logFollow = true;
    // Only a panel the reader opened is scrolled into view.
    this.scrollRunLog(!from);
    this.pollRunLog(id, abort);
  },
  keyed(recs: LogRecord[]): LogRecord[] { for (const r of recs) r.k = ++runLogKey; return recs; },
  // Long-poll: while the run is live each read waits (up to 25s) for its
  // next batch, so lines show within a second and nothing is missed between
  // reads; a finished run's log is read to its end once.
  async pollRunLog(id: string, abort: AbortController) {
    runLogLooping = true;
    try { await this.readRunLog(id, abort); } finally { if (runLogAbort === abort) runLogLooping = false; }
  },
  async readRunLog(id: string, abort: AbortController) {
    while (this.logRunId === id && !abort.signal.aborted) {
      const live = this.logLive();
      let res: Response;
      try {
        res = await this.authFetch('/api/runs/' + encodeURIComponent(id) + '/log?after=' + this.logCursor + (live ? '&wait=25' : ''), { signal: abort.signal });
      } catch {
        if (abort.signal.aborted) return;
        await new Promise(r => setTimeout(r, 5000)); // the network went: retry
        continue;
      }
      if (abort.signal.aborted || this.logRunId !== id) return;
      if (!res.ok) { await new Promise(r => setTimeout(r, 5000)); continue; } // authFetch answers 503 when the network went
      const page = await res.json() as { records: LogRecord[]; cursor: number; more: boolean; truncated: boolean };
      if (this.logRunId !== id) return;
      if (page.records && page.records.length) {
        const all = this.logRecords.concat(this.keyed(page.records));
        this.logRecords = all.length > RUN_LOG_KEEP ? all.slice(all.length - RUN_LOG_KEEP) : all;
        this.scrollRunLog();
      }
      this.logCursor = page.cursor || this.logCursor;
      this.logTruncated = !!page.truncated;
      if (!live && !page.more) return;
    }
  },
  closeRunLog() {
    if (runLogAbort) { runLogAbort.abort(); runLogAbort = null; }
    runLogLooping = false;
    if (runLogWatch) { clearInterval(runLogWatch); runLogWatch = null; }
    this.logRunId = ''; this.logRecords = []; this.logCursor = 0; this.logTruncated = false; this.logLoading = false; this.logError = '';
    this.logAuto = false; this.logFrom = '';
  },
  // ---- move to the next run ----
  // The run list of the preview refreshes on item events and every 20s,
  // which is too slow to notice a run ending and the next one starting. So
  // while the panel follows the item, its runs are read every few seconds:
  // only while agents have the item or one of its runs is under way.
  watchLog() {
    if (runLogWatch) { clearInterval(runLogWatch); runLogWatch = null; }
    if (this.logRunId && this.logAuto) runLogWatch = setInterval(() => this.watchLogRuns(), 4000);
  },
  async watchLogRuns() {
    const item = this.previewItem;
    if (!this.logRunId || !this.logAuto || !item || !this.showInboxPreview || document.hidden) return;
    const agentsTurn = ['queued', 'planning', 'approved', 'implementing'].includes(item.status);
    if (!agentsTurn && !this.previewRuns.some((r: RunRow) => this.runActive(r))) return;
    const res = await this.authFetch('/api/runs?item_id=' + encodeURIComponent(item.id));
    if (!res.ok || !this.previewItem || this.previewItem.id !== item.id) return;
    const runs = (await res.json()) as RunRow[];
    // Swapped in only when something changed: a new list redraws the tracker.
    const sig = (rs: RunRow[]) => rs.map((r: RunRow) => r.id + '|' + r.status + '|' + r.attempt + '|' + (r.phase || '')).join(',');
    if (sig(runs) !== sig(this.previewRuns)) this.previewRuns = runs;
    this.followLog();
  },
  // The run the panel moves to: one that is running now, the oldest first,
  // so parallel reviewers are shown one after the other.
  logNext(): RunRow | undefined {
    const live = this.previewRuns.filter((r: RunRow) => r.id !== this.logRunId && (r.status === 'claimed' || r.status === 'running'));
    return live[live.length - 1];
  },
  // Called whenever the run list was read. A reader who scrolled up is
  // reading: the panel stays until they go back to the newest lines.
  followLog() {
    if (!this.logRunId || !this.logAuto) return;
    if (this.logLive()) {
      // The same run again (a retry, a resumed pause): its loop ended with it.
      if (!runLogLooping && runLogAbort && !this.logLoading && !this.logError) this.pollRunLog(this.logRunId, runLogAbort);
      return;
    }
    const next = this.logNext();
    if (!next || !this.logFollow) return;
    const cur = this.logRun();
    this.openRunLog(next.id, this.logTitle() + (cur ? ', ' + this.statusText(cur.status).toLowerCase() : ''));
  },
  toggleLogAuto() {
    this.logAuto = !this.logAuto;
    this.watchLog();
    if (this.logAuto) this.watchLogRuns();
  },
  // Under the lines of a run that ended while the panel follows the item.
  logAutoNote(): string {
    if (!this.logRunId || !this.logAuto || this.logLoading || this.logLive() || !this.logRun()) return '';
    if (this.logNext()) return this.logFollow ? '' : 'The next run of this item has started. Press Follow to move to it.';
    return 'This run has ended. The next run of this item shows here when it starts.';
  },
  logFromNote(): string { return this.logFrom ? 'Moved here from ' + this.logFrom + '.' : ''; },
  // Follow: stay at the bottom while new lines arrive, unless the reader
  // scrolled up (then the panel stays put until they scroll back down).
  scrollRunLog(reveal = false) {
    // After the lines render (x-for runs after this tick).
    this.$nextTick(() => setTimeout(() => {
      const el = document.getElementById('run-log-lines');
      if (!el) return;
      if (this.logFollow) el.scrollTop = el.scrollHeight;
      if (reveal) el.closest('section')?.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
    }, 0));
  },
  // What the panel says without lines ('' = it has lines, or is loading).
  logEmptyText(): string {
    if (this.logLoading || this.logError || this.logRecords.length > 0) return '';
    return this.logLive() ? 'Nothing yet: lines appear as the worker sends them.' : 'No log for this run (it ran before run logs, or on a worker that could not send them).';
  },
  followRunLog() { this.logFollow = true; this.scrollRunLog(); this.followLog(); },
  onRunLogScroll(e: globalThis.Event) {
    const el = e.target as HTMLElement;
    this.logFollow = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
  },
  logTime(t: number): string { return new Date(t).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' }); },
  logSrc(src: string): string { return src === 'worker' ? 'wkr' : src; },
  // The tracker's log buttons for a step: running runs (live), the failed
  // run, or the newest finished run of the step.
  stepLogs(step: number, state: string): LogButton[] {
    const onStep = (r: RunRow) => (this.runActive(r) ? this.runStep(r) === step : (step === 4 ? r.stage === 'implement' : this.stageOf(r.stage) === step)) && r.status !== 'queued';
    const live = (r: RunRow) => r.status === 'claimed' || r.status === 'running';
    let runs: RunRow[];
    if (state === 'up' || state === 'skip') runs = [];
    else if (state === 'run') runs = this.previewRuns.filter((r: RunRow) => onStep(r) && this.runActive(r));
    else if (state === 'bad') runs = this.previewRuns.filter((r: RunRow) => onStep(r) && r.status === 'failed').slice(0, 3);
    else if (state === 'done') runs = this.previewRuns.filter((r: RunRow) => onStep(r) && r.status === 'succeeded').slice(0, step === 5 || step === 6 ? 3 : 1);
    else runs = this.previewRuns.filter(onStep).slice(0, 1); // waiting on you: the run that stopped here
    const many = runs.length > 1;
    return runs.map((r: RunRow) => ({
      key: r.id, id: r.id,
      text: (live(r) ? 'Watch live' : 'Log') + (many ? ' (' + ([r.focus, r.runner].filter(Boolean).join(', ') || 'try ' + r.attempt) + ')' : ''),
    }));
  },

  // The commit Mark done approves: the head the latest successful
  // implementation pushed (runs are newest first). Shown read-only and
  // computed from the run list, so it follows a new push live. It used to
  // be pre-filled into the note field, which read as something to type and
  // stopped every live refresh at in_review (a non-empty note means "the
  // user is typing").
  reviewHead(): string {
    const impl = this.trackImpl();
    return impl ? impl.head_sha || '' : '';
  },
  // Keyed x-for row (the CSP build's reliable re-render): [] while there is
  // no head yet.
  reviewHeadRow(): { key: string; text: string }[] {
    if (!this.workflowShowMarkDone()) return [];
    const head = this.reviewHead();
    return [{ key: 'head|' + head, text: head
      ? 'Mark done records your sign-off on commit ' + this.shortSha(head) + '. It doesn\'t merge: merge the ' + (this.pullRequestURL(head) ? 'pull request' : 'branch') + ' yourself. If anything is pushed after it, the item needs a new sign-off.'
      : 'No pushed commit is recorded for this item yet, so there is nothing to approve.' }];
  },
  markDoneLabel(): string {
    const head = this.reviewHead();
    return head ? 'Mark done (approve ' + this.shortSha(head) + ')' : 'Mark done';
  },
  // ---- stage tracker (the preview's "where is this item" list) ----
  // Fixed steps (rows 0-7; 8 = finished); three are the approver's. QA is
  // row 6, shown only where it applies (stageTrackHead/Tail leave a hidden
  // row out). Built from what the preview
  // already loads (status, runs, the approved revision, the pipeline), so
  // a live refresh re-renders it. The rows split around the current step:
  // the action card sits right under it, the steps not reached below.
  stageOf(stage: string): number {
    if (stage === 'plan' || stage === 'compare' || stage === 'synthesize') return 1;
    if (stage === 'implement') return 3;
    if (stage === 'review' || stage === 'branch_review') return 5;
    if (stage === 'qa') return 6;
    return -1;
  },
  runActive(r: RunRow): boolean { return r.status === 'queued' || r.status === 'claimed' || r.status === 'running' || r.status === 'paused'; },
  // The tracker step a run is on: an implement run past its first turn
  // (its heartbeats say checks / fixing) is on the Checks step.
  // A review-only item's branch_review run is on Checks until its
  // heartbeats say it is reviewing.
  runStep(r: RunRow): number {
    if (r.stage === 'branch_review' && this.runActive(r)) return r.phase === 'reviewing' ? 5 : 4;
    return r.stage === 'implement' && !!r.phase && this.runActive(r) ? 4 : this.stageOf(r.stage);
  },
  runTime(r: RunRow): number { return new Date(r.created_at).getTime(); },
  // A configured stage (review, qa): a runner or a group. null = unknown
  // (the pipeline could not be read).
  stageConfigured(stage: string): boolean | null {
    const p = this.previewPipe;
    if (!p) return null;
    const eff = p.effective || {};
    return !!eff[stage] && (eff[stage].members || []).length > 0;
  },
  // The newest successful implementation (its head is what sign-off
  // approves) and the review/QA results on it.
  // On a review-only item the reviewed head (a branch_review run) is both.
  trackImpl(): RunRow | undefined {
    return this.previewRuns.find((r: RunRow) => (r.stage === 'implement' || r.stage === 'branch_review') && r.status === 'succeeded' && !!r.head_sha);
  },
  trackReviews(): RunRow[] {
    const impl = this.trackImpl();
    if (!impl) return [];
    if (impl.stage === 'branch_review') {
      // A review panel: every member's result on this head, blocking first.
      const done = impl.group_id
        ? this.previewRuns.filter((r: RunRow) => r.stage === 'branch_review' && r.group_id === impl.group_id && (r.status === 'succeeded' || r.status === 'failed'))
        : [impl];
      const bad = (r: RunRow) => !this.reviewPassed(r);
      return done.filter(bad).concat(done.filter((r: RunRow) => !bad(r)));
    }
    const since = this.runTime(impl);
    const done = this.previewRuns.filter((r: RunRow) => (r.stage === 'review' || r.stage === 'qa') && (r.status === 'succeeded' || r.status === 'failed') && this.runTime(r) >= since);
    // Blocking results first: a blocked step draws its first line red.
    return done.filter((r: RunRow) => r.status === 'failed').concat(done.filter((r: RunRow) => r.status !== 'failed'));
  },
  findingCount(r: RunRow, severity: string): number { return (r.findings || []).filter(f => f.severity === severity).length; },
  // A review passed: it finished and found nothing blocking. A branch review
  // succeeds whatever its verdict, so its blocking findings decide (the
  // server's panel policy reads it the same way).
  reviewPassed(r: RunRow): boolean { return r.status === 'succeeded' && this.blockingCount(r) === 0; },
  // What blocks in a review's result: its own blocking findings and the
  // earlier ones it found still open (runs.BlockingFindings on the server).
  blockingCount(r: RunRow): number { return this.findingCount(r, 'blocking') + (r.still_open || 0); },
  stillOpenText(r: RunRow): string { return r.still_open ? this.plural(r.still_open, 'earlier finding') + ' still open' : ''; },
  plural(n: number, word: string): string { return n + ' ' + word + (n === 1 ? '' : 's'); },
  reviewLine(r: RunRow): string {
    const who = (r.focus || (r.stage === 'branch_review' ? 'review' : r.stage)) + (r.runner ? ' (' + r.runner + ')' : '');
    // A branch review succeeds whatever its verdict: blocking findings are
    // the verdict.
    if (r.status === 'succeeded' && !this.reviewPassed(r)) {
      const blocking = (r.findings || []).find(f => f.severity === 'blocking');
      if (blocking) return who + ' blocking: ' + blocking.text;
      if (r.still_open) return who + ' blocking: ' + this.stillOpenText(r);
    }
    if (r.status === 'failed') {
      const blocking = (r.findings || []).find(f => f.severity === 'blocking');
      return who + ' blocking: ' + (blocking ? blocking.text : (this.stillOpenText(r) || r.fail_reason || 'failed'));
    }
    const nits = this.findingCount(r, 'nit');
    return who + ' passed' + (nits ? ', ' + this.plural(nits, 'nit') : '');
  },
  // A run queued again waits for the worker that ran it before while that
  // worker is around (the server also checks it can take the run).
  waitsForPreferred(r: RunRow): boolean {
    if (!r.prefer_worker || !r.prefer_until || new Date(r.prefer_until).getTime() <= Date.now()) return false;
    const w = this.previewWorkers.find((x: WorkerSeen) => x.id === r.prefer_worker);
    return !!w && w.state !== 'offline';
  },
  activeLines(step: number): string[] {
    return this.previewRuns.filter((r: RunRow) => this.runStep(r) === step && this.runActive(r)).map((r: RunRow) => {
      const detail = [r.runner, r.focus].filter(Boolean).join(', ');
      const what = r.stage === 'branch_review' ? (r.phase === 'reviewing' ? 'the review' + (r.runner ? ' (' + r.runner + ')' : '') : 'setup and the checks on ' + (r.branch || 'the branch')) : r.phase === 'checks' ? 'the checks' : r.phase === 'fixing' ? 'a fix round for the failed checks' + (r.runner ? ' (' + r.runner + ')' : '') : r.stage + (detail ? ' (' + detail + ')' : '');
      // A worker-side failure sent it back to the queue (it retries on its own).
      if (r.status === 'queued' && r.retries) return 'Retrying ' + what + ' (attempt ' + (r.retries + 1) + ' of 3) after a worker problem: ' + (r.fail_reason || '').replace(/^attempt \d+ of \d+ failed on /, '');
      // Queued again for the item: the worker that ran it before goes first.
      if (r.status === 'queued' && this.waitsForPreferred(r)) return 'Waiting for ' + r.prefer_worker + ' (it ran this before) until ' + new Date(r.prefer_until).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) + ', then any worker: ' + what;
      if (r.status === 'queued') return 'Waiting for a worker: ' + what;
      if (r.status === 'paused') return 'Paused: ' + what + (r.fail_reason ? ', ' + r.fail_reason : '');
      return 'Running ' + what + (r.worker_id ? ' on ' + r.worker_id : '');
    });
  },
  // The repository's web root from the workspace id (host/owner/repo, as
  // github.com/x/y or a Gitea host) — '' when it does not look like one.
  repoWebBase(ws: string): string {
    return /^[a-z0-9-]+(\.[a-z0-9-]+)+\/[\w.-]+\/[\w.-]+$/i.test(ws || '') ? 'https://' + ws : '';
  },
  // "View changes": the base...head comparison (GitHub and Gitea share the
  // /compare/a...b form), or the commit alone without a base.
  // The branch's pull request comes first when the worker opened one.
  changesLinks(base: string, head: string): LinkRow[] {
    if (!head || this.unpushed(head)) return [];
    const out: LinkRow[] = [];
    const pr = this.pullRequestURL(head);
    if (pr) {
      const n = /\/pulls?\/(\d+)/.exec(pr);
      out.push({ key: 'pr|' + pr, href: pr, text: n ? 'Pull request #' + n[1] : 'Pull request' });
    }
    const root = this.repoWebBase(this.previewItem ? this.previewItem.workspace_id || '' : '');
    if (!root) return out;
    if (base) out.push({ key: 'cmp|' + base + '|' + head, href: root + '/compare/' + base + '...' + head, text: 'View changes ' + this.shortSha(base) + '…' + this.shortSha(head) });
    else out.push({ key: 'commit|' + head, href: root + '/commit/' + head, text: 'View commit ' + this.shortSha(head) });
    return out;
  },
  // The pull request for the branch that holds head: every round pushes
  // to the same branch and reuses the PR, so the newest one on it counts.
  pullRequestURL(head: string): string {
    const impl = this.previewRuns.find((r: RunRow) => r.stage === 'implement' && r.head_sha === head);
    if (!impl) return '';
    if (impl.pr_url) return impl.pr_url;
    const same = this.previewRuns.find((r: RunRow) => r.stage === 'implement' && !!r.pr_url && !!impl.branch && r.branch === impl.branch);
    return same ? same.pr_url || '' : '';
  },
  // True when the commit exists only on the worker, so the forge has
  // nothing to show: the implement run said it did not push (no push set up
  // for the repo), or — from workers that don't say — the newest review of
  // that head or branch failed because the branch wasn't on the remote.
  unpushed(head: string): boolean {
    const impl = this.previewRuns.find((r: RunRow) => r.stage === 'implement' && r.head_sha === head);
    if (!impl) return false;
    if (impl.pushed !== undefined) return !impl.pushed;
    const since = Date.parse(impl.created_at);
    const check = this.previewRuns.find((r: RunRow) => ['review', 'qa', 'second_opinion'].includes(r.stage)
      && (r.status === 'succeeded' || r.status === 'failed') && Date.parse(r.created_at) >= since
      && (r.head_sha === head || (!!impl.branch && r.branch === impl.branch)));
    // ' not pushed' is the worker's notPushedMarker (internal/worker/stages.go).
    return !!check && check.status === 'failed' && (check.fail_reason || '').includes(' not pushed');
  },
  // The base the implementation branched from: the run's, else the approved plan's.
  trackBase(impl: RunRow | undefined): string {
    return (impl && impl.base_sha) || (this.previewApprovedRev && this.previewApprovedRev.base_sha) || '';
  },
  // Unfoldable detail for a review-like run: each finding with its severity
  // and file, then the reviewer's summary.
  // From the record when it has the run's findings: each with its state
  // and what happened to it since, then the earlier findings this run
  // checked again. A run the record does not have falls back to its own list.
  runMore(r: RunRow, who: string): MoreRow[] {
    const state = (st: string) => st.replace(/_/g, ' ');
    const own = this.previewFindings.filter((f: FindingRec) => f.run_id === r.id);
    let out: MoreRow[] = own.length
      ? own.map((f: FindingRec) => ({
        key: r.id + '|' + f.id + '|' + f.state + '|' + (f.updates || []).length,
        label: state(f.state) + ' · ' + f.severity + ' · ' + who + (f.file ? ' · ' + f.file : ''),
        text: [f.text].concat(this.findingNotes(f)).join('\n'),
      }))
      : (r.findings || []).map((f, k) => ({ key: r.id + '|f' + k, label: f.severity + ' · ' + who + (f.file ? ' · ' + f.file : ''), text: f.text }));
    for (const f of this.previewFindings) {
      for (const u of f.updates || []) {
        if (u.run_id !== r.id || (u.kind !== 'addressed' && u.kind !== 'still_open')) continue;
        out = out.concat([{
          key: r.id + '|' + f.id + '|' + u.id,
          label: (u.kind === 'addressed' ? 'addressed' : 'still open') + ' · earlier finding of ' + this.findingBy(f) + ' · checked by ' + who,
          text: f.text + (u.note ? '\n' + u.note : ''),
        }]);
      }
    }
    if (r.result_summary) out.push({ key: r.id + '|sum', label: 'summary · ' + who, text: r.result_summary });
    return out;
  },
  // The worker's checks on an implementation: one line, plus each check's
  // output as a fold-out row. No checks recorded = none configured.
  checkSummary(r: RunRow): { line: string; more: MoreRow[] } {
    const cs = r.checks || [];
    if (!cs.length) return { line: 'No checks configured: nothing built or tested this commit', more: [] };
    const failed = cs.filter(c => !c.passed);
    const line = failed.length
      ? 'Checks: ' + failed.length + ' of ' + cs.length + ' failed (' + failed.map(c => c.name).join(', ') + ')'
      : (cs.length === 1 ? 'Check passed: ' + cs[0].name : 'All ' + cs.length + ' checks passed');
    return { line, more: cs.map((c, k) => ({ key: r.id + '|c' + k, label: (c.passed ? '✓ passed' : '✗ failed') + ' · ' + c.name, text: c.output || '(no output)', mono: true })) };
  },
  // The setup an implementation ran in its fresh worktree: one line and each
  // command's output. A round that reused the worktree points at the round
  // that set it up.
  setupSummary(r: RunRow): { line: string; more: MoreRow[] } {
    const ss = r.setup || [];
    // A running implementation reports its setup with its result.
    if (!ss.length && this.runActive(r)) return { line: '', more: [] };
    if (!ss.length) {
      // Only a setup that finished left a worktree to reuse (a failed one
      // removes it).
      const earlier = this.previewRuns.find((x: RunRow) => x.stage === 'implement' && x.id !== r.id && (x.setup || []).length > 0 && (x.setup || []).every(c => c.passed)
        && !!r.branch && x.branch === r.branch && this.runTime(x) < this.runTime(r));
      return { line: earlier ? 'Setup: ran in an earlier round (the worktree was reused)' : '', more: [] };
    }
    const failed = ss.find(c => !c.passed);
    const line = failed ? 'Setup failed: ' + failed.name : (ss.length === 1 ? 'Setup ran: ' + ss[0].name : 'Setup ran ' + ss.length + ' commands in a fresh worktree');
    return { line, more: ss.map((c, k) => ({ key: r.id + '|s' + k, label: (c.passed ? '✓ ran' : '✗ failed') + ' · ' + c.name, text: c.output || '(no output)', mono: true })) };
  },
  reviewerName(r: RunRow): string { return (r.focus || r.stage) + (r.runner ? ' (' + r.runner + ')' : ''); },
  // A running step whose worker went offline, or whose lease ran out, is
  // probably stuck: say so instead of spinning forever.
  activeWarn(step: number): string {
    for (const r of this.previewRuns) {
      if (this.runStep(r) !== step || (r.status !== 'claimed' && r.status !== 'running')) continue;
      const w = this.previewWorkers.find((x: WorkerSeen) => x.id === r.worker_id);
      if (w && w.state === 'offline') return (r.worker_id || 'The worker') + ' is offline (last seen ' + this.timeAgo(w.last_seen) + '). The run resumes when it comes back.';
      if (r.lease_expires_at && Date.parse(r.lease_expires_at) < Date.now()) return 'No heartbeat from ' + (r.worker_id || 'the worker') + ' since ' + this.timeAgo(r.lease_expires_at) + '; it may have stopped.';
    }
    return '';
  },
  stageModel(): { rows: StageRow[]; current: number } {
    const inputs = [this.previewItem, this.previewRuns, this.previewApprovedRev, this.previewPipe, this.previewWorkers, this.previewFindings];
    if (stageCache && stageCache.inputs.every((v, i) => v === inputs[i])) return stageCache.model as { rows: StageRow[]; current: number };
    const model = this.buildStageModel();
    stageCache = { inputs, model };
    return model;
  },
  buildStageModel(): { rows: StageRow[]; current: number } {
    const item = this.previewItem;
    const empty = { rows: [] as StageRow[], current: -1 };
    if (!item) return empty;
    const st = item.status;
    const main = this.previewRuns.filter((r: RunRow) => r.stage !== 'second_opinion'); // newest first
    const last = main[0];
    const piped = this.isWorkflowStatus(st) || st === 'open' || ((st === 'done' || st === 'discarded') && main.length > 0);
    if (!piped) return empty;
    // Blocked by its checks: the implementation committed (Implement is
    // done) and the Checks step is where it stopped.
    const checksBlocked = (st === 'blocked' || st === 'failed') && !!last && last.stage === 'implement' && last.status === 'failed'
      && (last.checks || []).some(c => !c.passed);
    // A review-only item skips Plan, Plan approval and Implement: its one
    // run checks and reviews someone's branch.
    const reviewOnly = item.flow === 'review';
    // The current step and how it is drawn.
    let cur = -1;
    let curState = 'run';
    switch (st) {
      case 'open': cur = 0; curState = 'you'; break;
      case 'queued': case 'planning': cur = reviewOnly ? 4 : 1; break;
      case 'awaiting_approval': cur = 2; curState = 'you'; break;
      case 'approved': cur = 3; break;
      case 'implementing': cur = last && this.runStep(last) >= 4 ? this.runStep(last) : 3; break;
      case 'in_review': cur = 7; curState = 'you'; break;
      case 'blocked': case 'failed': cur = checksBlocked || (reviewOnly && !!last && last.stage === 'branch_review') ? 4 : last ? Math.max(1, this.stageOf(last.stage)) : 1; curState = 'bad'; break;
      case 'done': cur = 8; break;
      case 'discarded': cur = 8; break;
    }
    const impl = this.trackImpl();
    // The implementation the Implement and Checks steps describe: the newest
    // successful one, the one its checks blocked, or the one being checked.
    const inChecks = st === 'implementing' && cur === 4;
    const implRun = checksBlocked || inChecks ? last : impl;
    // The results on the newest head, by step: Agent review (row 5) and QA
    // (row 6).
    const onHead = this.trackReviews();
    const reviews = onHead.filter((r: RunRow) => r.stage !== 'qa');
    const qaRuns = onHead.filter((r: RunRow) => r.stage === 'qa');
    const stepRuns = (i: number): RunRow[] => (i === 6 ? qaRuns : reviews);
    const planRuns = main.filter((r: RunRow) => r.stage === 'plan' && r.status === 'succeeded');
    const rev = this.previewApprovedRev;
    const configured = this.stageConfigured('review');
    const qaConfigured = this.stageConfigured('qa');
    // The implementation round: one per successful push, plus the one under
    // way (or failed) when the newest implement run has not succeeded.
    const newestImpl = main.find((r: RunRow) => r.stage === 'implement');
    const rounds = main.filter((r: RunRow) => r.stage === 'implement' && r.status === 'succeeded').length
      + (newestImpl && newestImpl.status !== 'succeeded' ? 1 : 0);
    // Since when the item has waited for the approver: the newest finished
    // run handed it over (item.updated_at moves on any edit, e.g. a second
    // opinion's section); an item no run touched yet waits since creation.
    const lastDone = main.find((r: RunRow) => !this.runActive(r));
    const waitingSince = lastDone ? lastDone.updated_at : item.created_at;
    const defs = [
      { name: 'Queued', who: 'you' },
      { name: 'Plan', who: 'agents' },
      { name: 'Plan approval', who: 'you' },
      { name: 'Implement', who: 'agent' },
      { name: 'Checks', who: 'worker' },
      { name: 'Agent review', who: 'agents' },
      { name: 'QA', who: 'agents' },
      { name: 'Your sign-off', who: 'you' },
    ];
    // Evidence for a finished step.
    const doneLines = (i: number): { lines: string[]; when: string; links?: LinkRow[]; more?: MoreRow[]; warn?: string } => {
      if (i === 1 && planRuns.length) {
        const newest = planRuns[0];
        const round = newest.group_id ? planRuns.filter((r: RunRow) => r.group_id === newest.group_id) : [newest];
        const names = round.map((r: RunRow) => r.runner || r.worker_id || 'The agent').filter((v: string, k: number, a: string[]) => a.indexOf(v) === k);
        let text = round.length > 1 ? names.join(', ') + ' wrote ' + round.length + ' candidates' : names[0] + ' wrote the plan';
        if (main.some((r: RunRow) => r.stage === 'synthesize' && r.status === 'succeeded')) text += ', plus a synthesis';
        if (main.some((r: RunRow) => r.stage === 'compare' && r.status === 'succeeded')) text += ', compared';
        return { lines: [text], when: this.timeAgo(newest.updated_at) };
      }
      if (i === 2 && rev) {
        const steps = (rev.steps || []).filter(x => x.phase !== 'review').length;
        let text = 'Approved revision #' + rev.revision_no + (rev.label ? ' (' + rev.label + ')' : '') + ', ' + this.plural(steps, 'step');
        if (rev.base_sha) text += ' on base ' + this.shortSha(rev.base_sha);
        return { lines: [text], when: rev.locked_at ? this.timeAgo(rev.locked_at) : '' };
      }
      // Being checked: committed, not yet reported (no head yet).
      if (i === 3 && implRun && !implRun.head_sha) {
        return { lines: [(implRun.runner || implRun.worker_id || 'The agent') + ' committed' + (implRun.branch ? ' on ' + implRun.branch : '')], when: '' };
      }
      if (i === 3 && implRun) {
        const local = this.unpushed(implRun.head_sha || '');
        const lines = [(implRun.runner || implRun.worker_id || 'The agent') + (local ? ' committed ' : ' pushed ') + this.shortSha(implRun.head_sha || '') + (implRun.branch ? (local ? ' on ' : ' to ') + implRun.branch : '')];
        const stat = (implRun.diffstat || '').split('\n').map((l: string) => l.trim()).filter(Boolean).pop();
        if (stat) lines.push(stat);
        const setup = this.setupSummary(implRun);
        if (setup.line) lines.push(setup.line);
        const warn = local ? 'Not pushed: the branch is only on ' + (implRun.worker_id || 'the worker') + ', so there is no link to it and reviewers can\'t fetch it. Set up push for this repository in the worker config.' : '';
        return { lines, when: this.timeAgo(implRun.updated_at), links: this.changesLinks(this.trackBase(implRun), implRun.head_sha || ''), more: setup.more, warn };
      }
      if (i === 4 && implRun && (implRun.checks || []).length) {
        const checks = this.checkSummary(implRun);
        const lines = [checks.line];
        const fixRounds = implRun.fix_rounds || 0;
        if (fixRounds > 0 && implRun.status === 'succeeded') lines.push('They failed at first; the implementer fixed them in ' + this.plural(fixRounds, 'fix round') + '.');
        return { lines, when: this.timeAgo(implRun.updated_at), more: checks.more };
      }
      if ((i === 5 || i === 6) && stepRuns(i).length) {
        const runs = stepRuns(i);
        const ok = runs.filter((r: RunRow) => this.reviewPassed(r)).length;
        const lines = runs.map((r: RunRow) => this.reviewLine(r));
        if (impl && impl.head_sha) lines.push(ok + ' of ' + runs.length + ' passed, on ' + this.shortSha(impl.head_sha));
        // The followed findings are counted once, on the last agent step that ran.
        const followed = i === 6 || !qaRuns.length ? this.followedLine() : '';
        if (followed) lines.push(followed);
        return { lines, when: this.timeAgo(runs[0].updated_at), more: runs.flatMap((r: RunRow) => this.runMore(r, this.reviewerName(r))) };
      }
      if (i === 7 && st === 'done') {
        const head = impl && impl.head_sha ? impl.head_sha : '';
        const lines = [head ? 'Approved ' + this.shortSha(head) : 'Marked done'];
        if (head && !this.unpushed(head)) lines.push(this.pullRequestURL(head) ? 'Merge the pull request to land it.' : 'Merge the branch to land it.');
        return { lines, when: this.timeAgo(item.updated_at), links: head ? this.changesLinks(this.trackBase(impl), head) : [] };
      }
      return { lines: [], when: '' };
    };
    const rows: StageRow[] = defs.map((d, i) => {
      let state = i < cur ? 'done' : i === cur ? curState : 'up';
      let lines: string[] = [];
      let when = '';
      let links: LinkRow[] = [];
      let more: MoreRow[] = [];
      let warn = '';
      if (state === 'done') { const d = doneLines(i); lines = d.lines; when = d.when; links = d.links || []; more = d.more || []; warn = d.warn || ''; }
      if (state === 'run') { lines = this.activeLines(i); if (!lines.length) lines = [i === 3 && st === 'approved' ? 'Starting' : 'Waiting for a worker']; warn = this.activeWarn(i); }
      if (state === 'you') when = 'waiting ' + this.timeAgo(waitingSince).replace(' ago', '').replace('just now', 'under a minute');
      if (state === 'bad') {
        const failed = i === 4 ? last : main.find((r: RunRow) => this.stageOf(r.stage) === i && r.status === 'failed');
        const agentStep = i === 5 || i === 6;
        lines = agentStep && stepRuns(i).length ? stepRuns(i).map((r: RunRow) => this.reviewLine(r)).concat([this.followedLine()].filter(Boolean)) : [(failed && failed.fail_reason) || 'Blocked: open the run history for the reason'];
        if (agentStep) more = stepRuns(i).flatMap((r: RunRow) => this.runMore(r, this.reviewerName(r)));
        // A failed setup: what ran and what it printed.
        if (i === 3 && failed) more = this.setupSummary(failed).more;
        // Blocked by its checks: each check with its output.
        if (i === 4 && failed) more = this.checkSummary(failed).more;
        when = failed ? this.timeAgo(failed.updated_at) : '';
      }
      // No checks configured: say the commit went untested instead of a
      // green check.
      if (i === 4 && state === 'done' && implRun && !(implRun.checks || []).length) {
        state = 'skip';
        lines = ['No checks configured for this repository, so nothing built or tested this commit. Add checks to its repo block in the worker config.'];
      }
      // Agent review never ran because none is configured: say so instead
      // of a green check (passed) or an empty step (waiting).
      if (i === 5 && state !== 'run' && state !== 'bad' && !reviews.length && ((state === 'done' && !!impl) || configured === false)) {
        state = 'skip';
        lines = [configured === false
          ? (qaConfigured ? 'No review stage is configured for this workspace: QA is the only agent stage on the commit.' : 'No review stage is configured for this workspace, so no agent reviews the commit. Set one under Workspaces → Pipeline.')
          : 'No agent reviewed this commit'];
      }
      // QA is a step of its own only where it applies: the workspace sets
      // it, or it ran on this head (or runs now). A review-only item has no
      // QA stage. Passed without a result on this head: skipped, not green.
      let hidden = false;
      if (i === 6) {
        const active = main.some((r: RunRow) => r.stage === 'qa' && this.runActive(r));
        hidden = cur !== 6 && !qaRuns.length && !active && (reviewOnly || qaConfigured !== true);
        if (!hidden && state === 'done' && !qaRuns.length) {
          state = 'skip';
          lines = ['QA did not run on this commit'];
        }
      }
      // Review only: nobody plans or implements the branch.
      if (reviewOnly && i >= 1 && i <= 3) {
        state = 'skip';
        lines = i === 1 ? ['Review only: the agent checks and reviews ' + (item.branch ? item.branch : 'the existing branch') + '; nothing is planned or implemented. Request changes at sign-off to have it continued.'] : [];
        when = ''; links = []; more = []; warn = '';
      }
      // A discarded item: the steps it never finished are greyed out.
      if (st === 'discarded' && state === 'done' && !lines.length && i > 0) state = 'skip';
      const round = i >= 3 && i <= 7 && rounds > 1 && state !== 'up' ? 'round ' + rounds : '';
      // The key leaves out `when` (relative time): it changes every minute,
      // and a re-keyed row is re-created, closing an open fold-out. A kept
      // row still updates its time through the bindings.
      const moreLabel = i === 3 ? 'Setup output' : i === 4 ? 'Check output' : 'Findings and summaries';
      const logs = this.stepLogs(i, state);
      // The Plan step opens the plans side by side once there is one to read.
      const plans = i === 1 && !!item.plan_id && state !== 'up' && state !== 'skip';
      return { key: i + '|' + state + '|' + round + '|' + lines.join('/') + '|' + warn + '|' + more.map((m: MoreRow) => m.key).join(',') + '|' + logs.map((l: LogButton) => l.id).join(',') + (plans ? '|p' : ''), name: d.name, who: d.who, state, when, round, lines, links, more, moreLabel, warn, logs, plans, hidden };
    });
    return { rows, current: cur };
  },
  // History: the item's audit trail, newest first — who queued, approved,
  // asked for changes, signed off, and when, plus pipeline failures. Left
  // out as noise: run claims, plan-lock bookkeeping (Plan approved covers
  // it) and the worker's own appends (the tracker shows their results).
  historyRows(): { key: string; text: string; notes: string; when: string }[] {
    const names: Record<string, string> = {
      'inbox.queue': 'Queued', 'inbox.review': 'Queued for a branch review', 'inbox.approve': 'Plan approved', 'inbox.request_changes': 'Changes requested',
      'inbox.mark_done': 'Signed off', 'inbox.retry': 'Retried', 'inbox.reject': 'Rejected',
      'inbox.second_opinion': 'Second opinion requested', 'inbox.link_plan': 'Plan linked', 'inbox.edit': 'Edited',
      'inbox.transition': 'Pipeline', 'inbox.migrate': 'Migrated',
    };
    return this.previewTimeline
      .filter((e: AuditEntry) => {
        if (e.action.startsWith('plan.')) return false;
        if (e.action === 'inbox.edit' && e.via === 'worker') return false;
        if (e.action === 'inbox.transition') return !!e.notes && !e.notes.startsWith('run claimed');
        return true;
      })
      .map((e: AuditEntry) => ({
        key: e.id,
        text: (names[e.action] || e.action) + ' by ' + e.actor + (e.via ? ' via ' + e.via : ''),
        notes: e.notes || '',
        when: this.timeAgo(e.created_at),
      }));
  },
  // What the agent runs on this item cost, when the runners report it.
  previewCost(): { key: string; text: string }[] {
    const total = this.previewRuns.reduce((n: number, r: RunRow) => n + (r.cost_usd || 0), 0);
    return total > 0 ? [{ key: 'cost|' + total.toFixed(2), text: '$' + total.toFixed(2) }] : [];
  },
  // Screen-reader text for a row's state (the mark is aria-hidden).
  stageStateLabel(s: StageRow): string {
    const words: Record<string, string> = { done: 'finished', run: 'in progress', you: 'waiting for you', bad: 'blocked', up: 'not reached yet', skip: 'skipped', flag: 'needs a look' };
    return s.name + ': ' + (words[s.state] || s.state);
  },
  // The tracker above and below the action card. The second-opinion step
  // comes right after the agent stages (Agent review, row 5, and QA, row 6)
  // — below the card when one of them is the current step, so the card
  // stays under the step it is for. A hidden row (QA where it does not
  // apply) is left out.
  stageTrackHead(): StageRow[] {
    const m = this.stageModel();
    if (m.current < 0) return [];
    const head = m.current >= 8 ? m.rows : m.rows.slice(0, m.current + 1);
    const out = m.current > 6 ? head.slice(0, 7).concat(this.secondOpinionStage(), head.slice(7)) : head;
    return out.filter((s: StageRow) => !s.hidden);
  },
  stageTrackTail(): StageRow[] {
    const m = this.stageModel();
    if (m.current >= 8 || m.current < 0) return [];
    const tail = m.rows.slice(m.current + 1);
    if (m.current > 6) return tail;
    const upToQA = 6 - m.current;
    return tail.slice(0, upToQA).concat(this.secondOpinionStage(), tail.slice(upToQA)).filter((s: StageRow) => !s.hidden);
  },
  // The header pill: where the item stands, in words.
  stageSummary(): { key: string; text: string; tone: string }[] {
    const m = this.stageModel();
    const st = this.previewItem ? this.previewItem.status : '';
    if (m.current < 0) return [];
    let text = '';
    let tone = 'tone-info';
    if (st === 'done') { text = 'Done'; tone = 'tone-success'; }
    else if (st === 'discarded') { text = 'Discarded'; tone = 'tone-muted'; }
    else {
      const row = m.rows[m.current];
      if (row.state === 'you') { text = row.name === 'Queued' ? 'Ready to queue' : row.name + ': your turn'; tone = 'tone-warn'; }
      else if (row.state === 'bad') { text = 'Blocked at ' + (row.name === 'QA' ? row.name : row.name.toLowerCase()); tone = 'tone-danger'; }
      else text = row.name + (row.round ? ' · ' + row.round : '');
    }
    return [{ key: text + '|' + tone, text, tone }];
  },
  // "View changes" in the sign-off card: the comparison the approval covers.
  signoffLinks(): LinkRow[] {
    if (!this.workflowShowMarkDone()) return [];
    const impl = this.trackImpl();
    return this.changesLinks(this.trackBase(impl), this.reviewHead());
  },
  // What the approver is signing, as checks inside the action card.
  gateChecks(): TextRow[] {
    const item = this.previewItem;
    if (!item) return [];
    const out: TextRow[] = [];
    const rev = this.previewApprovedRev;
    if (item.status === 'awaiting_approval') {
      out.push({ key: 'g-none', ic: 'n', text: 'Nothing is implemented yet. Approving locks this plan and starts implementation.' });
      return out;
    }
    if (item.status !== 'in_review') return out;
    if (rev) out.push({ key: 'g-rev|' + rev.id, ic: 'y', text: 'The plan you approved: revision #' + rev.revision_no + (rev.label ? ' (' + rev.label + ')' : '') });
    const head = this.reviewHead();
    const reviews = this.trackReviews();
    if (!head) return out;
    if (reviews.length) {
      out.push({ key: 'g-head|' + head, ic: 'y', text: 'Commit ' + this.shortSha(head) + ' is the branch head, and the reviewers checked it' });
      // One line per agent step that ran on the head: Agent review, QA.
      const steps: [string, RunRow[]][] = [['Agent review', reviews.filter((r: RunRow) => r.stage !== 'qa')], ['QA', reviews.filter((r: RunRow) => r.stage === 'qa')]];
      for (const [name, runs] of steps) {
        if (!runs.length) continue;
        const ok = runs.filter((r: RunRow) => this.reviewPassed(r)).length;
        const nits = runs.reduce((n: number, r: RunRow) => n + this.findingCount(r, 'nit'), 0);
        const blocking = runs.reduce((n: number, r: RunRow) => n + this.blockingCount(r), 0);
        const found = [blocking ? this.plural(blocking, 'blocking finding') : '', nits ? this.plural(nits, 'nit') : ''].filter(Boolean).join(', ');
        out.push({ key: 'g-rev|' + name + '|' + ok + '|' + runs.length + '|' + blocking, ic: ok === runs.length ? 'y' : 'w', text: name + ': ' + ok + ' of ' + runs.length + ' passed' + (found ? ', ' + found : '') + (ok === runs.length && !blocking ? ', no blocking findings' : '') });
      }
    } else {
      out.push({ key: 'g-head|' + head, ic: 'w', text: 'Commit ' + this.shortSha(head) + ' is the branch head. No agent reviewed it, so check it yourself' + (this.unpushed(head) ? ' on the worker.' : ' or ask for a second opinion.') });
    }
    // Advisory, but the approver should not sign off past it unseen.
    const against = this.secondOpinionsOnHead().filter((r: RunRow) => this.wouldBlock(r));
    if (against.length) {
      const who = against.map((r: RunRow) => r.runner || 'agent').join(', ');
      out.push({ key: 'g-so|' + against.map((r: RunRow) => r.id).join(','), ic: 'w', text: 'Second opinion: ' + who + ' would block this commit (advisory). Read it in the step above.' });
    }
    return out;
  },
  // Second opinions as a step of their own, right after Agent review: shown
  // in every status once one was asked for, with the same row as the other
  // steps (state, time, log buttons, findings and summaries). Advisory: its
  // state never follows or moves the item's. One line per opinion, newest
  // first; one on an older commit is marked, since it does not cover what
  // sign-off approves. [] when none was asked for.
  // Each runner's newest second opinion on the commit sign-off approves
  // (runs are newest first): what counts for "does a second opinion stand
  // against this commit". An older one by the same runner was superseded.
  secondOpinionsOnHead(): RunRow[] {
    const head = this.reviewHead();
    const seen: string[] = [];
    return this.previewRuns.filter((r: RunRow) => {
      if (r.stage !== 'second_opinion' || r.status === 'cancelled' || !head || r.head_sha !== head) return false;
      const runner = r.runner || '';
      if (seen.includes(runner)) return false;
      seen.push(runner);
      return true;
    });
  },
  wouldBlock(r: RunRow): boolean { return r.status === 'succeeded' && this.blockingCount(r) > 0; },
  // ---- plans side by side ----
  // Opens the plans of a planning round (the newest by default): each
  // planner's plan in full, the comparison and the synthesis as columns.
  async openPlans(round = 0) {
    const item = this.previewItem;
    if (!item || this.planView.loading) return;
    this.planView.loading = true;
    try {
      const res = await this.authFetch('/api/inbox/' + encodeURIComponent(item.id) + '/plans' + (round ? '?round=' + round : ''));
      if (!await this.handleBad(res, 'Could not load the plans')) return;
      const view = (await res.json()) as PlansRound;
      // Hidden columns are kept while paging through rounds, not between items.
      const hidden = this.planView.open && this.planView.view && this.planView.view.item_id === view.item_id ? this.planView.hidden : {};
      this.planView = { open: true, loading: false, view, hidden };
      this.injectPlans();
    } finally { this.planView.loading = false; }
  },
  closePlans() { if (!this.planView.open) return; plansClosedAt = Date.now(); this.planView.open = false; },
  plansTitle(): string { return this.planView.view ? this.planView.view.title : ''; },
  plansRoundText(): string { const v = this.planView.view; return v ? 'Round ' + v.round + ' of ' + v.rounds : ''; },
  plansHasRounds(): boolean { return !!this.planView.view && this.planView.view.rounds > 1; },
  plansCanPrev(): boolean { return !!this.planView.view && this.planView.view.round > 1 && !this.planView.loading; },
  plansCanNext(): boolean { return !!this.planView.view && this.planView.view.round < this.planView.view.rounds && !this.planView.loading; },
  plansPrev() { if (this.plansCanPrev()) this.openPlans(this.planView.view!.round - 1); },
  plansNext() { if (this.plansCanNext()) this.openPlans(this.planView.view!.round + 1); },
  // Every column of the round: the plans, then the comparison, then the
  // approver's notes that closed it. A plan of the newest round that the
  // gate offers can be approved from its column.
  planColumns(): PlanColumn[] {
    const v = this.planView.view;
    if (!v) return [];
    const newest = v.round === v.rounds;
    const targets = newest && this.previewItem && this.previewItem.id === v.item_id ? this.approveTargets() : [];
    const tones: Record<string, string> = { plan: 'tone-info', candidate: 'tone-info', synthesis: 'tone-accent', comparison: 'tone-warn', notes: 'tone-neutral' };
    const col = (key: string, title: string, kind: string, html: string, questionsHtml: string, revisionId: string): PlanColumn => ({
      key: v.round + '|' + key, title, kind, tone: tones[kind] || 'tone-neutral', html, questionsHtml, hasQuestions: !!questionsHtml, revisionId,
      canApprove: !!revisionId && targets.some((t: ApproveTarget) => t.id === revisionId), hidden: !!this.planView.hidden[key],
    });
    const out = v.plans.map((d: PlanDocView, i: number) => col('p' + i, d.title, d.kind, d.html, d.questions_html || '', d.revision_id || ''));
    if (v.comparison) out.push(col('comparison', v.comparison.heading, 'comparison', v.comparison.html, '', ''));
    (v.notes || []).forEach((n: PlanNoteView, i: number) => out.push(col('n' + i, n.heading, 'notes', n.html, '', '')));
    return out;
  },
  shownPlanColumns(): PlanColumn[] { return this.planColumns().filter((c: PlanColumn) => !c.hidden); },
  togglePlanColumn(key: string) {
    const plain = key.slice(key.indexOf('|') + 1);
    const hidden = { ...this.planView.hidden };
    if (hidden[plain]) delete hidden[plain]; else hidden[plain] = true;
    this.planView.hidden = hidden;
    this.injectPlans();
  },
  // The rendered plans go in imperatively (the CSP build has no x-html):
  // server-rendered, sanitised HTML only, after the columns exist.
  injectPlans() {
    this.$nextTick(() => {
      for (const c of this.shownPlanColumns()) {
        document.querySelectorAll('[data-plan-html="' + CSS.escape(c.key) + '"]').forEach((el) => { el.innerHTML = c.html; });
        document.querySelectorAll('[data-plan-q="' + CSS.escape(c.key) + '"]').forEach((el) => { el.innerHTML = c.questionsHtml; });
      }
    });
  },
  // Approve a plan from its column: the same action as the gate's button
  // (the notes and answers typed in the preview go with it).
  approveFromPlans(revisionId: string) {
    this.closePlans();
    this.approveTarget(revisionId);
  },
  // The round as one Markdown document, as the server writes it.
  async plansMarkdown(): Promise<{ text: string; name: string } | null> {
    const v = this.planView.view;
    if (!v) return null;
    const res = await this.authFetch('/api/inbox/' + encodeURIComponent(v.item_id) + '/plans?format=md&round=' + v.round);
    if (!await this.handleBad(res, 'Could not export the plans')) return null;
    const named = /filename="([^"]+)"/.exec(res.headers.get('Content-Disposition') || '');
    return { text: await res.text(), name: named ? named[1] : 'plans.md' };
  },
  async downloadPlans() {
    const doc = await this.plansMarkdown();
    if (!doc) return;
    const url = URL.createObjectURL(new Blob([doc.text], { type: 'text/markdown;charset=utf-8' }));
    const a = document.createElement('a');
    a.href = url;
    a.download = doc.name;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
    this.notify('Saved ' + doc.name, 'success');
  },
  async copyPlans() {
    const doc = await this.plansMarkdown();
    if (!doc) return;
    try {
      await navigator.clipboard.writeText(doc.text);
      this.notify('Plans copied as Markdown', 'success');
    } catch {
      this.notify('The browser did not allow copying; use Download instead', 'error');
    }
  },
  // ---- review findings (the record: internal/findings) ----
  // Who raised a finding: "review", "review · security", "second opinion · codex".
  findingBy(f: FindingRec): string {
    const stage = f.stage.replace(/_/g, ' ');
    if (f.stage === 'second_opinion' && f.runner) return stage + ' · ' + f.runner;
    return f.focus ? stage + ' · ' + f.focus : stage;
  },
  // What happened to a finding, one line per update, oldest first.
  findingNotes(f: FindingRec): string[] {
    const note = (u: FindingUpdate) => (u.note ? ': ' + u.note : '');
    return (f.updates || []).map((u: FindingUpdate) => {
      switch (u.kind) {
        case 'sent_back': return 'Sent back to be fixed' + (u.actor ? ' by ' + u.actor : '');
        case 'waived': return 'Waived' + (u.actor ? ' by ' + u.actor : '') + note(u);
        case 'fixed': return 'The implementer says fixed' + note(u);
        case 'not_fixed': return 'The implementer left it' + note(u);
        case 'addressed': return 'A review confirmed it is fixed' + note(u);
        case 'still_open': return 'A review found it not fixed' + note(u);
      }
      return u.kind + note(u);
    });
  },
  // Sending findings back needs the gate that starts an implementation
  // (sign-off, or a blocked item whose retry implements) and an approver:
  // the server says which action takes them now, with the record. Checked
  // against the status too, so a record loaded before the item moved on
  // offers nothing.
  findingsSendable(): boolean {
    const st = this.previewItem ? this.previewItem.status : '';
    if (this.previewFindingsSend === 'request-changes') return st === 'in_review';
    if (this.previewFindingsSend === 'retry') return st === 'blocked' || st === 'failed';
    return false;
  },
  // A finding recorded from a run of before the findings record that nobody
  // has acted on since (findings.Finding.Earlier): history, not something
  // left without a follow-up.
  findingEarlier(f: FindingRec): boolean { return f.source === 'imported' && !(f.updates || []).length; },
  // A finding can be ticked to go back when it would not go back by itself.
  findingTickable(f: FindingRec): boolean { return this.findingsSendable() && ['noted', 'fixed', 'addressed', 'waived'].includes(f.state); },
  // The record grouped by what is owed on each finding, in the order the
  // approver acts on them. "New" is what the newest review round raised and
  // nobody has touched; the settled groups start folded.
  findingGroups(): FindingGroupView[] {
    const fs = this.previewFindings;
    if (!fs.length) return [];
    const newest = this.trackReviews().map((r: RunRow) => r.id);
    const approver = this.canApprove();
    const defs = [
      { id: 'new', title: 'New', hint: 'raised by the newest review; the implementer is asked to fix them', open: true },
      { id: 'open', title: 'To fix', hint: 'the implementer is asked to fix them', open: true },
      { id: 'still_open', title: 'Still open', hint: 'a review checked them again and they are not fixed', open: true },
      { id: 'fixed', title: 'Fixed, not confirmed', hint: 'the implementer says so; no review has confirmed it', open: true },
      { id: 'noted', title: 'Not followed up', hint: 'nits and second-opinion findings; tick one to have it fixed', open: true },
      { id: 'addressed', title: 'Addressed', hint: 'a review confirmed they are fixed', open: false },
      { id: 'waived', title: 'Waived', hint: 'you accepted them as they are', open: false },
      { id: 'earlier', title: 'From earlier runs', hint: 'recorded from runs of before findings were followed; what became of them is not known. Tick one to have it fixed', open: false },
    ];
    const groupOf = (f: FindingRec) => (this.findingEarlier(f) ? 'earlier' : f.state === 'open' && !(f.updates || []).length && newest.includes(f.run_id) ? 'new' : f.state);
    return defs.map(d => {
      const rows: FindingRow[] = fs.filter((f: FindingRec) => groupOf(f) === d.id).map((f: FindingRec) => {
        const waiving = this.waive.id === f.id;
        const canSend = this.findingTickable(f);
        const picked = canSend && !!this.findingPicks[f.id];
        return {
          key: f.id + '|' + f.state + '|' + (f.updates || []).length + '|' + (waiving ? 'w' : '') + (picked ? 'p' : ''),
          id: f.id, severity: f.severity, tone: this.toneOf('severity', f.severity), text: f.text,
          meta: [this.findingBy(f), f.head_sha ? 'on ' + this.shortSha(f.head_sha) : '', f.file || ''].filter(Boolean).join(' · '),
          notes: this.findingNotes(f),
          // Open and still-open ones go back by themselves; the rest go
          // back only when ticked (a waived one: that undoes the waive).
          canSend, picked,
          canWaive: approver && f.state !== 'waived' && f.state !== 'addressed', waiving,
        };
      });
      // Keyed by the group alone: its rows re-render by their own keys,
      // and the fold stays as the approver left it. A group holding a
      // ticked finding is unfolded unless they folded it themselves.
      const folded = this.findingFold[d.id];
      const open = folded === undefined ? d.open || rows.some((r: FindingRow) => r.picked) : folded;
      return { key: d.id, id: d.id, title: d.title + ' (' + rows.length + ')', hint: d.hint, open, rows };
    }).filter((g: FindingGroupView) => g.rows.length > 0);
  },
  toggleFinding(id: string) {
    const picks = { ...this.findingPicks };
    if (picks[id]) delete picks[id]; else picks[id] = true;
    this.findingPicks = picks;
  },
  // A group folded or unfolded by hand stays that way. The browser fires
  // toggle for the fold the page itself set too; recording that is harmless.
  setFindingFold(id: string, open: boolean) {
    if (this.findingFold[id] === open) return;
    this.findingFold = { ...this.findingFold, [id]: open };
  },
  // The ticked findings that will be sent: still in the record, and still
  // tickable now (a tick on a finding that has since been settled by a run,
  // or left from before the item moved on, is not sent and not counted).
  pickedFindings(): string[] {
    return this.previewFindings.filter((f: FindingRec) => this.findingPicks[f.id] && this.findingTickable(f)).map((f: FindingRec) => f.id);
  },
  pickedFindingsText(): string {
    const n = this.pickedFindings().length;
    return n ? this.plural(n, 'finding') + ' ticked: sent back to be fixed with ' + (this.previewFindingsSend === 'retry' ? 'Retry' : 'Request changes') : '';
  },
  startWaive(id: string) { this.waive = { id, note: '', busy: false }; },
  cancelWaive() { this.waive = { id: '', note: '', busy: false }; },
  setWaiveNote(v: string) { this.waive.note = v; },
  // Accepting a blocking finding needs a reason (the server refuses it
  // without one); a nit may be waived without.
  waiveNeedsNote(): boolean {
    const f = this.previewFindings.find((x: FindingRec) => x.id === this.waive.id);
    return !!f && f.severity === 'blocking';
  },
  waiveNotReady(): boolean { return this.waive.busy || (this.waiveNeedsNote() && !this.waive.note.trim()); },
  waiveNotePlaceholder(): string { return this.waiveNeedsNote() ? 'why it is accepted (required; reviewers read it)' : 'why it is accepted (reviewers read it)'; },
  // Waive: the approver accepts a finding as it is (reviewers read the note).
  async confirmWaive() {
    const item = this.previewItem;
    if (!item || !this.waive.id || this.waiveNotReady()) return;
    this.waive.busy = true;
    try {
      const res = await this.authFetch('/api/inbox/' + encodeURIComponent(item.id) + '/findings/' + encodeURIComponent(this.waive.id) + '/waive', {
        method: 'POST', headers: { 'X-Skopos-Via': 'dashboard' }, body: JSON.stringify({ note: this.waive.note.trim() }),
      });
      if (!await this.handleBad(res, 'Finding not waived')) return;
      this.notify('Finding waived', 'success');
      const picks = { ...this.findingPicks };
      delete picks[this.waive.id];
      this.findingPicks = picks;
      this.waive = { id: '', note: '', busy: false };
      await this.fetchPreviewExtras(item.id);
    } finally { this.waive.busy = false; }
  },
  // The followed-up findings in numbers, for the review step: '' when the
  // item has none.
  followedLine(): string {
    const count = (state: string) => this.previewFindings.filter((f: FindingRec) => f.tracked && f.state === state).length;
    const parts = [
      [count('open'), 'to fix'], [count('still_open'), 'still open'], [count('fixed'), 'fixed, not confirmed'],
      [count('addressed'), 'addressed'], [count('waived'), 'waived'],
    ].filter(p => (p[0] as number) > 0).map(p => p[0] + ' ' + p[1]);
    return parts.length ? 'Findings followed: ' + parts.join(', ') : '';
  },
  secondOpinionStage(): StageRow[] {
    const all = this.previewRuns.filter((r: RunRow) => r.stage === 'second_opinion' && r.status !== 'cancelled'); // newest first
    if (!all.length) return [];
    const head = this.reviewHead();
    const stale = (r: RunRow) => !!head && !!r.head_sha && r.head_sha !== head;
    const who = (r: RunRow) => (r.runner || 'agent') + ' on ' + this.shortSha(r.head_sha || '');
    const blocks = (r: RunRow) => (r.findings || []).find(f => f.severity === 'blocking');
    const open = (r: RunRow) => this.runActive(r) || r.status === 'interrupted';
    const line = (r: RunRow) => {
      let verdict = '';
      if (open(r)) verdict = r.status === 'queued' ? 'waiting for a worker' : (r.status === 'paused' ? 'paused' : (r.status === 'interrupted' ? 'interrupted; it resumes when its worker is back' : 'reviewing'));
      else if (r.status === 'failed') verdict = 'did not finish: ' + (r.fail_reason || 'no reason given');
      else if (r.status !== 'succeeded') verdict = r.status;
      else {
        const blocking = blocks(r);
        const nits = this.findingCount(r, 'nit');
        verdict = blocking ? 'would block: ' + blocking.text : r.still_open ? 'would block: ' + this.stillOpenText(r) : 'pass' + (nits ? ', ' + this.plural(nits, 'nit') : '');
      }
      return who(r) + ': ' + verdict + (stale(r) ? ' (older commit)' : '');
    };
    const shown = all.slice(0, 5);
    const newest = all[0];
    let state = 'done';
    // The state says whether anything from a second opinion is outstanding:
    //   - amber: a runner's newest opinion on the commit waiting for sign-off
    //     would block, the newest opinion on that commit did not finish, or a
    //     blocking finding of any second opinion is neither addressed nor
    //     waived (the record follows it past the commit it was raised on);
    //   - green: one finished and nothing of it is left open — also when it
    //     was about an older commit and its findings were dealt with since;
    //   - grey: none ever finished.
    const onHead = this.secondOpinionsOnHead();
    const ours = this.previewFindings.filter((f: FindingRec) => f.stage === 'second_opinion');
    const unsettled = ours.filter((f: FindingRec) => f.severity === 'blocking' && f.state !== 'addressed' && f.state !== 'waived' && !this.findingEarlier(f));
    if (all.some(open)) state = 'run';
    else if (unsettled.length > 0 || onHead.some((r: RunRow) => this.wouldBlock(r)) || (newest.status !== 'succeeded' && !stale(newest))) state = 'flag';
    else if (!all.some((r: RunRow) => r.status === 'succeeded')) state = 'skip';
    const lines = shown.map(line);
    // What became of what the second opinions found, from the record.
    if (ours.length) {
      const count = (st: string) => ours.filter((f: FindingRec) => f.state === st).length;
      const parts = [[count('open'), 'to fix'], [count('still_open'), 'still open'], [count('fixed'), 'fixed, not confirmed'], [count('noted'), 'not followed up'],
        [count('addressed'), 'addressed'], [count('waived'), 'waived']].filter(p => (p[0] as number) > 0).map(p => p[0] + ' ' + p[1]);
      lines.push('Its findings: ' + parts.join(', '));
    }
    if (all.length > shown.length) lines.push('and ' + this.plural(all.length - shown.length, 'earlier one'));
    const live = (r: RunRow) => r.status === 'claimed' || r.status === 'running';
    const logged = shown.filter((r: RunRow) => r.status !== 'queued').slice(0, 3);
    const logs: LogButton[] = logged.map((r: RunRow) => ({
      key: r.id, id: r.id, text: (live(r) ? 'Watch live' : 'Log') + (logged.length > 1 ? ' (' + who(r) + ')' : ''),
    }));
    const more = shown.flatMap((r: RunRow) => this.runMore(r, who(r)));
    // `when` stays out of the key, as for the other rows.
    return [{
      key: 'so|' + state + '|' + lines.join('/') + '|' + more.map((m: MoreRow) => m.key).join(',') + '|' + logs.map((l: LogButton) => l.id + l.text).join(','),
      name: 'Second opinion', who: 'optional', state, when: this.timeAgo(newest.updated_at), round: '', lines, links: [], more,
      moreLabel: 'Findings and summaries', warn: '', logs, plans: false,
    }];
  },
  // Why the Second opinion button is disabled, '' when it isn't: the server
  // refuses a second run of the same runner on the same commit unless
  // "again on this head" is ticked.
  secondOpinionBlockedReason(): string {
    const runner = this.secondOpinion.runner.trim();
    const head = this.reviewHead();
    if (head && this.unpushed(head)) return 'The branch isn\'t pushed, so a second opinion can\'t fetch ' + this.shortSha(head) + '.';
    if (!runner || !head || this.secondOpinion.force) return '';
    const dup = this.previewRuns.some((r: RunRow) => r.stage === 'second_opinion' && r.status === 'succeeded' && r.runner === runner && r.head_sha === head);
    return dup ? runner + ' already checked ' + this.shortSha(head) + '. Tick "again on this head" to ask again, or name another runner.' : '';
  },
  secondOpinionDisabled(): boolean { return this.secondOpinion.busy || this.secondOpinionBlockedReason() !== ''; },
  // The action card's look: amber while it waits for the approver, red
  // when the item is blocked.
  gateTone(): string {
    const st = this.previewItem ? this.previewItem.status : '';
    if (st === 'blocked' || st === 'failed') return 'stage-gate stage-gate-bad';
    if (st === 'awaiting_approval' || st === 'in_review' || st === 'open') return 'stage-gate';
    return '';
  },
  // Kanban card strip: the same seven steps from the status alone (cards
  // carry no runs). Blocked items get no strip; their badge says blocked
  // and the preview shows where.
  cardStage(item: InboxItem): { key: string; label: string; dots: { key: string; cls: string }[] }[] {
    const pos: Record<string, [number, string, string]> = {
      queued: [1, 'run', 'plan'], planning: [1, 'run', 'plan'], awaiting_approval: [2, 'you', 'plan approval: your turn'],
      approved: [3, 'run', 'implement'], implementing: [3, 'run', 'implement or review'], in_review: [6, 'you', 'your sign-off'],
    };
    const p = pos[item.status];
    if (!p) return [];
    const dots = [0, 1, 2, 3, 4, 5, 6].map((i: number) => ({ key: item.id + '|' + i + '|' + item.status, cls: i < p[0] ? 'stage-dot stage-dot-done' : i === p[0] ? 'stage-dot stage-dot-' + p[1] : 'stage-dot' }));
    return [{ key: item.id + '|' + item.status, label: p[2], dots }];
  },

  // Refresh the open modal after an action — the note input is KEPT (only
  // openInboxPreview resets it), so an approve-with-notes flow survives.
  // Re-read the open item. Inbox events fire for every item in scope, and
  // swapping previewItem re-renders the description (blank until the next
  // tick refills it): so the item is swapped only when it changed (status
  // or updated_at); runs and workers always refresh.
  async reloadPreview() {
    if (!this.showInboxPreview || !this.previewItem) return;
    const id = this.previewItem.id;
    const res = await this.authFetch('/api/inbox/' + encodeURIComponent(id) + '?parts=1');
    if (!this.showInboxPreview || !this.previewItem || this.previewItem.id !== id) return;
    if (!res.ok) { if (res.status === 404) this.closeInboxPreview(); return; }
    const fresh = (await res.json()) as InboxDetail;
    if (fresh.updated_at !== this.previewItem.updated_at || fresh.status !== this.previewItem.status) {
      this.previewItem = fresh;
      this.injectPreviewContent();
      this.syncPreviewSections();
      this.fetchPreviewRevision();
    }
    this.fetchPreviewRuns(id);
    this.fetchPreviewExtras(id).then(() => this.fillSecondOpinion());
  },
  inboxCanRank(item: InboxItem): boolean { return this.inboxItemEditable(item); },
  // "updated 2m ago" beside the created age once the item has been touched
  // (claim/convert/edit/reorder all bump updated_at; the bare age stays the
  // created date). Untouched rows carry identical RFC3339Nano strings, so a
  // strict compare decides — rows key on updated_at, so the label re-evaluates.
  inboxShowUpdated(item: InboxItem): boolean { return !!item.updated_at && item.updated_at !== item.created_at; },
  inboxUpdatedLabel(item: InboxItem): string { return 'updated ' + this.timeAgo(item.updated_at); },

  // ---- agent-pipeline workflow actions (approver-gated server-side) ----
  // The dashboard buttons render for every key; the server authorizes —
  // a non-approver gets the 403 as a toast. Root is implicitly approver.
  canApprove(): boolean {
    return !!this.whoami && (this.whoami.root || !!(this.whoami.key && this.whoami.key.approver));
  },
  workflowShowQueue(): boolean { return !!this.previewItem && this.previewItem.status === 'open'; },
  // Review only: an open item that names a branch (or the reference its
  // repository's branch names use).
  workflowShowReviewBranch(): boolean {
    const it = this.previewItem;
    return !!it && it.status === 'open' && (!!it.branch || !!it.ref);
  },
  isReviewOnly(): boolean { return !!this.previewItem && this.previewItem.flow === 'review'; },
  workflowShowApprove(): boolean { return !!this.previewItem && this.previewItem.status === 'awaiting_approval'; },
  workflowShowRetry(): boolean { return !!this.previewItem && (this.previewItem.status === 'blocked' || this.previewItem.status === 'failed'); },
  workflowShowMarkDone(): boolean { return !!this.previewItem && this.previewItem.status === 'in_review'; },
  // At in_review the branch can go back to implementation with notes (the
  // dashboard had no way to; Slack `changes:` and the CLI did).
  workflowShowReviewChanges(): boolean { return this.workflowShowMarkDone(); },
  workflowShowReject(): boolean { return !!this.previewItem && this.isWorkflowStatus(this.previewItem.status); },
  workflowShowInput(): boolean { return this.workflowShowApprove() || this.workflowShowRetry() || this.workflowShowMarkDone(); },
  workflowNoteLabel(): string {
    if (this.workflowShowRetry()) return 'Answer to the blocker (sent to the agent)';
    if (this.workflowShowMarkDone()) return this.isReviewOnly() ? 'Notes for Request changes (an agent plans and continues the branch)' : 'Notes for Request changes (sent to the implementer)';
    if (this.workflowShowApprove()) return this.openQuestionGroups().length > 0 ? 'Notes (Request changes needs notes or answers)' : 'Notes (required for Request changes)';
    return '';
  },
  workflowNotePlaceholder(): string {
    const asked = this.openQuestionGroups().length > 0;
    if (this.workflowShowRetry()) return asked || this.savedAnswers() ? 'anything else for the agent (optional once you answered)' : 'answer to the blocker (required)';
    if (this.workflowShowApprove() && asked) return 'notes (Request changes needs notes or answers)';
    if (this.workflowShowMarkDone()) return this.previewFindings.length ? 'what to change (Request changes needs notes or a ticked finding)' : 'what to change (required for Request changes)';
    if (this.workflowShowApprove()) return 'notes (required for Request changes)';
    return '';
  },
  // Input requirement is per action: approve/queue take none, the rest
  // require the note/answer/sha. Method call, never an inline expression —
  // the :disabled-with-undefined CSP trap.
  workflowActionReady(action: string): boolean {
    if (this.workflowBusy) return false;
    if (action === 'approve') return true;
    if (action === 'queue' || action === 'review' || action === 'reject') return true;
    if (action === 'mark_done') return this.reviewHead() !== '';
    // Retry after saving the answers: they are the answer to the blocker.
    if (action === 'retry' && this.savedAnswers()) return true;
    return this.hasWorkflowInput();
  },
  async postWorkflowAction(id: string, action: string, body: Record<string, unknown> | null, okMessage: string): Promise<boolean> {
    if (this.workflowBusy) return false;
    this.workflowBusy = true;
    const opts: RequestInit = { method: 'POST', headers: { 'X-Skopos-Via': 'dashboard' } };
    if (body) opts.body = JSON.stringify(body);
    const res = await this.authFetch('/api/inbox/' + encodeURIComponent(id) + '/' + action, opts);
    this.workflowBusy = false;
    if (!await this.handleBad(res, 'Action failed')) {
      // A refusal often means the preview is stale (a newer revision, a
      // status that moved on): show the current state.
      await this.reloadPreview();
      return false;
    }
    this.notify(okMessage, 'success');
    this.workflowNote = '';
    this.questionAnswers = {};
    this.findingPicks = {};
    await this.fetchInbox();
    await this.reloadPreview();
    return true;
  },
  queueInboxItem(id: string) { this.postWorkflowAction(id, 'queue', null, 'Queued for the agent pipeline'); },
  reviewInboxBranch(id: string) { this.postWorkflowAction(id, 'review', null, 'Queued for a review of its branch'); },
  // Reject leaves the pipeline for good (the item is discarded): confirm.
  rejectInboxItem(id: string) {
    const title = this.previewItem ? this.previewItem.title : 'this item';
    this.confirm = {
      open: true, title: 'Reject item', label: 'Reject', busy: false,
      message: `Reject “${title}”? It leaves the agent pipeline and is discarded (restore brings it back to open).`,
      pending: { kind: 'inboxreject', id },
    };
  },
  // The ticked findings go with it, from the review gate only (the plan
  // gate has none to send back).
  requestChangesInboxItem(id: string) {
    if (!this.hasWorkflowInput()) return;
    const body: Record<string, unknown> = { notes: this.workflowNote, answers: this.answerItems() };
    if (this.pickedFindings().length) body.findings = this.pickedFindings();
    this.postWorkflowAction(id, 'request-changes', body, 'Change requested');
  },
  // Retry: the answers by question id, and the note as free text (read
  // against the open questions too).
  retryInboxItem(id: string) {
    if (!this.hasWorkflowInput() && !this.savedAnswers()) return;
    const body: Record<string, unknown> = { answers: this.answerItems(), answer: this.workflowNote.trim() };
    if (this.pickedFindings().length) body.findings = this.pickedFindings();
    this.postWorkflowAction(id, 'retry', body, 'Requeued');
  },
  markDoneInboxItem(id: string) { const head = this.reviewHead(); if (head) this.postWorkflowAction(id, 'mark-done', { head_sha: head }, 'Marked done'); },
  // The quick action for a card/row is the one that needs no input; Retry
  // and Mark done live in the modal (answer / head sha required).
  // A plan at the gate is never approved from a card: the quick action
  // opens the preview, where the plan and its revision are on screen.
  workflowQuickAction(item: InboxItem): string {
    if (item.status === 'open' && item.workspace_id) return 'Queue';
    if (item.status === 'awaiting_approval') return 'Review plan';
    return '';
  },
  runQuickWorkflowAction(item: InboxItem) {
    if (item.status === 'open') this.queueInboxItem(item.id);
    else if (item.status === 'awaiting_approval') this.openInboxPreview(item);
  },

  // ---- pipeline settings dialog ----
  // Opened from a workspace (approvers in scope; server-enforced) or a group
  // (root). Rows re-render through pipe.rev on every structural change —
  // keyed x-for is the reliable re-render path in the CSP build.
  async openPipeline(kind: string, id: string, label: string) {
    const base = kind === 'group' ? '/api/groups/' : '/api/workspaces/';
    const [res, wres, dres] = await Promise.all([this.authFetch(base + encodeURIComponent(id) + '/pipeline'), this.authFetch('/api/workers'), this.authFetch('/api/fleet/runners')]);
    if (!await this.handleBad(res, 'Could not load the pipeline')) return;
    const v = (await res.json()) as PipeView;
    const runners: string[] = [];
    if (wres.ok) for (const w of (await wres.json()) as WorkerSeen[]) for (const r of w.runners || []) if (!runners.includes(r)) runners.push(r);
    // Runners the server defines can be chosen before any worker offers them.
    if (dres.ok) for (const d of (((await dres.json()) as { runners?: RunnerDef[] }).runners || [])) if (!runners.includes(d.name)) runners.push(d.name);
    // Whose dialog this is, before the config form loads: the form's
    // baseline (cfgFingerprint) depends on the kind. Set after it, a
    // workspace opened after a group had its Save armed with nothing changed.
    this.pipe.kind = kind; this.pipe.id = v.owner_id; this.pipe.label = label;
    await this.loadPipeConfig(kind, v.owner_id);
    this.pipe.tab = 'stages'; this.pipe.editing = '';
    this.pipeBriefs.editing = false; this.pipeBriefs.confirm = '';
    await this.loadPipeBriefs();
    this.pipe.runnerOptions = runners.sort();
    this.pipe.agentsDefault = v.own.agents_default || '';
    const fromGroup = kind === 'workspace' && v.agents_default_source && v.agents_default_source !== 'workspace';
    this.pipe.agentsInherited = fromGroup ? (v.effective_agents_default || '') : '';
    this.pipe.agentsInheritedFrom = fromGroup ? (v.agents_default_source || '').replace('group:', '') : '';
    this.pipe.secondOpinion = v.own.second_opinion_runner || '';
    const secondFromGroup = kind === 'workspace' && v.second_opinion_source && v.second_opinion_source !== 'workspace';
    this.pipe.secondInherited = secondFromGroup ? (v.effective_second_opinion_runner || '') + ' (group ' + (v.second_opinion_source || '').replace('group:', '') + ')' : '';
    this.pipe.codeIndex = this.pipe.codeIndexWas = !!v.own.code_index;
    this.pipe.indexableBy = v.indexable_by || [];
    const labels: Record<string, string> = { plan: 'Plan', compare: 'Compare (judge)', synthesize: 'Synthesize (merge)', implement: 'Implement', review: 'Review', qa: 'QA' };
    const own = v.own.stages || {};
    this.pipe.rows = ['plan', 'synthesize', 'compare', 'implement', 'review', 'qa'].map((stage: string) => {
      const mine = own[stage];
      const eff = v.effective ? v.effective[stage] : undefined;
      const inherited = eff && eff.source !== 'workspace' ? eff : null;
      const src = mine || null;
      return {
        stage, label: labels[stage], multi: stage === 'plan' || stage === 'review' || stage === 'qa', rule: stage === 'review' || stage === 'qa',
        mode: mine ? 'own' : (inherited ? 'inherited' : 'unset'),
        members: src ? src.members.map((m: PipeMember) => ({ runner: m.runner, focus: m.focus || '' })) : [],
        policy: (src && src.policy) || '', earlyExit: !!(src && src.early_exit), timeoutMin: src && src.timeout_ms ? String(Math.round(src.timeout_ms / 60000)) : '',
        inherited: inherited ? { members: inherited.members, policy: inherited.policy, early_exit: inherited.early_exit, timeout_ms: inherited.timeout_ms } : null,
        inheritedFrom: inherited ? inherited.source.replace('group:', '') : '',
        addRunner: '', addFocus: '',
      } as PipeRow;
    });
    // Saving is server-enforced; say so up front instead of a 403 toast.
    this.pipe.readOnly = kind === 'group' ? !this.whoamiIsRoot() : !this.canApprove();
    const notes: { key: string; tone: string; text: string }[] = [];
    if (this.pipe.readOnly) notes.push({ key: 'ro', tone: 'tone-neutral', text: kind === 'group' ? 'Read only: group pipelines need the root key.' : 'Read only: changing a workspace pipeline needs an approver key.' });
    for (const c of v.conflicts || []) notes.push({ key: 'c|' + c, tone: 'tone-danger', text: 'Conflict: ' + c });
    if ((v.unknown_runners || []).length) notes.push({ key: 'u', tone: 'tone-warn', text: 'No worker serving this workspace offers ' + (v.unknown_runners || []).join(', ') + ' yet; those runs wait until one does.' });
    // The runner policy binds what can be chosen here: every layer must
    // allow a runner (set with `skopos policy`, admin permission).
    this.pipe.policyBound = !!(v.policy && !v.policy.allow_all);
    if (v.policy && !v.policy.allow_all) {
      for (const l of v.policy.layers || []) {
        notes.push({ key: 'p|' + l.source, tone: 'tone-info', text: 'Runner policy (' + l.source + '): allow ' + ((l.policy.allow || []).map((r: PolicyRule) => this.policyRuleText(r)).join('; ') || 'nothing') });
      }
    }
    for (const r of v.policy_refused || []) notes.push({ key: 'pr|' + r, tone: 'tone-danger', text: 'Refused by the runner policy: ' + r });
    if (kind === 'workspace' && (v.groups || []).length) notes.push({ key: 'g', tone: 'tone-neutral', text: 'Inherits from group' + ((v.groups || []).length > 1 ? 's ' : ' ') + (v.groups || []).map((g: { name: string }) => g.name).join(', ') + ' where a stage is not set here.' });
    this.pipe.notes = notes;
    this.pipe.rev++;
    this.pipe.was = this.pipeSnapshot();
    this.pipe.open = true;
  },
  closePipeline() { this.pipe.open = false; },
  // What Save would send, as text: the Stages and Options tabs share one
  // Save, and the other tabs save on their own, so the dialog says when
  // something on those two is not saved yet.
  pipeSnapshot(): string {
    const rows = (this.pipe.rows as PipeRow[]).filter((r: PipeRow) => r.mode === 'own')
      .map((r: PipeRow) => [r.stage, r.members.map((m: PipeMember) => (m.focus || '') + '@' + m.runner), r.policy, r.earlyExit, r.timeoutMin.trim()]);
    return JSON.stringify([rows, this.pipe.agentsDefault, this.pipe.secondOpinion.trim(), this.pipe.codeIndex]);
  },
  pipeDirty(): boolean { return !this.pipe.readOnly && this.pipeSnapshot() !== this.pipe.was; },
  pipeDirtyElsewhere(): boolean { return (this.pipe.tab === 'briefs' || this.pipe.tab === 'config') && this.pipeDirty(); },
  // Close from a tab that has no Save of the pipeline: not past unsaved
  // stage changes.
  pipeCloseFromTab() {
    if (this.pipeDirty()) { this.pipe.tab = 'stages'; this.notify('The stages or options have unsaved changes: Save or Cancel', 'error'); return; }
    // Nor past what the other of the two tabs has not saved.
    if (this.pipe.tab !== 'config' && this.pipeConfigDirty()) { this.pipe.tab = 'config'; this.notify('The server config has unsaved changes: Save config, or Undo changes', 'error'); return; }
    if (this.pipe.tab !== 'briefs' && this.pipeBriefs.editing) { this.pipe.tab = 'briefs'; this.notify('A brief is still open: Save it, or Cancel', 'error'); return; }
    this.closePipeline();
  },
  pipeRow(stage: string): PipeRow | undefined { return this.pipe.rows.find((r: PipeRow) => r.stage === stage); },
  pipeBump() { this.pipe.rows = this.pipe.rows.map((r: PipeRow) => ({ ...r })); this.pipe.rev++; },
  pipeOverride(stage: string) {
    const r = this.pipeRow(stage);
    if (!r || !r.inherited) return;
    r.mode = 'own';
    this.pipe.editing = stage;
    r.members = r.inherited.members.map((m: PipeMember) => ({ runner: m.runner, focus: m.focus || '' }));
    r.policy = r.inherited.policy || ''; r.earlyExit = !!r.inherited.early_exit;
    r.timeoutMin = r.inherited.timeout_ms ? String(Math.round(r.inherited.timeout_ms / 60000)) : '';
    this.pipeBump();
  },
  pipeSetHere(stage: string) { const r = this.pipeRow(stage); if (r) { r.mode = 'own'; r.members = []; this.pipe.editing = stage; this.pipeBump(); } },
  // One stage's form is open at a time: the rest stay one line each.
  pipeEdit(stage: string) { this.pipe.editing = this.pipe.editing === stage ? '' : stage; },
  // The decision settings of a stage with several agents, in words.
  pipeRuleText(r: PipeRow): string {
    if (r.mode !== 'own' || r.members.length < 2) return '';
    const parts: string[] = [];
    if (r.rule) parts.push(r.policy === 'majority' ? 'majority decides' : r.policy === 'all-pass' ? 'all must pass' : 'any blocking finding fails');
    if (r.rule && r.earlyExit) parts.push('early exit');
    if (r.timeoutMin.trim()) parts.push('timeout ' + r.timeoutMin.trim() + 'm');
    return parts.join(' · ');
  },
  async errorText(res: Response, prefix: string): Promise<string> { return prefix + ': ' + (await this.extractError(res)).replace(/^invalid config: /, ''); },
  // ---- MCP servers for runs (Workspaces view; admins) ----
  // The definitions are the fleet's; a workspace's config names the ones its
  // runs get. A secret's value is only ever sent, never read.
  async fetchMcpDefs() {
    if (!this.canAdmin()) { this.mcpDefs = []; return; }
    const res = await this.authFetch('/api/fleet/mcp-servers');
    // A server from before MCP definitions has no such route.
    this.mcpDefs = res.ok ? (((await res.json()) as { mcp_servers?: MCPServerDef[] }).mcp_servers || []) : [];
  },
  mcpRows(): { key: string; name: string; type: string; target: string; scope: string; secrets: { name: string; set: boolean }[] }[] {
    return this.mcpDefs.map((m: MCPServerDef) => {
      const ws = m.workspaces || [];
      const used = (m.used_by || []).map((o: string) => o.replace(/^workspace:/, '').replace(/^group:/, 'group '));
      return {
        key: m.name + '|' + (m.rev || '') + '|' + (m.secrets || []).map((s: { name: string; set: boolean }) => s.name + s.set).join(','),
        name: m.name, type: m.type,
        target: m.type === 'command' ? (m.command || []).join(' ') : (m.url || ''),
        scope: (ws.length ? 'For ' + ws.join(', ') : 'For any workspace') + ' · ' + (used.length ? 'given to the runs of ' + used.join(', ') : 'no config names it yet'),
        secrets: m.secrets || [],
      };
    });
  },
  // Lines of a box as a list (blank lines dropped).
  boxLines(text: string): string[] { return text.split('\n').map((l: string) => l.trim()).filter((l: string) => l !== ''); },
  // NAME<sep>value lines as an object; a line without the separator is
  // returned as the problem.
  boxPairs(text: string, sep: string): { pairs: Record<string, string>; bad: string } {
    const pairs: Record<string, string> = {};
    for (const line of this.boxLines(text)) {
      const i = line.indexOf(sep);
      if (i <= 0) return { pairs, bad: line };
      pairs[line.slice(0, i).trim()] = line.slice(i + sep.length).trim();
    }
    return { pairs, bad: '' };
  },
  pairsBox(pairs: Record<string, string> | undefined, sep: string): string {
    return Object.keys(pairs || {}).sort().map((k: string) => k + sep + (pairs as Record<string, string>)[k]).join('\n');
  },
  openMcpModal(name: string) {
    const def = this.mcpDefs.find((m: MCPServerDef) => m.name === name);
    const f = this.mcpForm;
    f.editing = !!def; f.error = ''; f.saving = false; f.confirm = false;
    f.name = def ? def.name : '';
    f.type = def ? def.type : 'http';
    f.url = (def && def.url) || '';
    f.headers = this.pairsBox(def && def.headers, ': ');
    f.command = ((def && def.command) || []).join('\n');
    f.env = this.pairsBox(def && def.env, '=');
    f.workspaces = ((def && def.workspaces) || []).join('\n');
    this.mcpSecretsOf(def);
    f.open = true;
    this.$nextTick(() => document.getElementById(def ? 'mcp-url' : 'mcp-name')?.focus());
  },
  // The secrets of the open definition, with an empty box each: a value
  // typed for one is dropped when the list is read again.
  mcpSecretsOf(def: MCPServerDef | undefined) {
    this.mcpForm.secrets = ((def && def.secrets) || []).map((s: { name: string; set: boolean }) => ({ name: s.name, set: s.set, value: '' }));
    this.mcpForm.rev++;
  },
  closeMcpModal() { this.mcpForm.open = false; this.mcpForm.secrets = []; },
  async saveMcpDef() {
    const f = this.mcpForm;
    if (f.saving) return;
    f.error = ''; f.confirm = false;
    const name = f.name.trim();
    if (!name) { f.error = 'Give the server a name.'; return; }
    // The save is a PUT by name: a new server under a name in use would
    // replace that definition, and its stored secrets with it.
    if (!f.editing && this.mcpDefs.some((m: MCPServerDef) => m.name === name)) { f.error = 'A server named ' + name + ' is already defined. Edit that one, or choose another name.'; return; }
    const body: Record<string, unknown> = { type: f.type };
    if (f.type === 'http') {
      const h = this.boxPairs(f.headers, ':');
      if (h.bad) { f.error = 'A header is written as Name: value. This line is not: ' + h.bad; return; }
      body.url = f.url.trim();
      if (Object.keys(h.pairs).length) body.headers = h.pairs;
    } else {
      const e = this.boxPairs(f.env, '=');
      if (e.bad) { f.error = 'A variable is written as NAME=value. This line is not: ' + e.bad; return; }
      body.command = this.boxLines(f.command);
      if (Object.keys(e.pairs).length) body.env = e.pairs;
    }
    const ws = this.boxLines(f.workspaces);
    if (ws.length) body.workspaces = ws;
    f.saving = true;
    try {
      const res = await this.authFetch('/api/fleet/mcp-servers/' + encodeURIComponent(name), { method: 'PUT', headers: { 'X-Skopos-Via': 'dashboard' }, body: JSON.stringify(body) });
      if (!res.ok) { f.error = await this.errorText(res, 'The MCP server was not saved'); return; }
      await this.fetchMcpDefs();
      const def = this.mcpDefs.find((m: MCPServerDef) => m.name === name);
      const unset = ((def && def.secrets) || []).filter((s: { set: boolean }) => !s.set).length;
      // Stay open while a secret still needs its value.
      f.editing = true; f.name = name;
      this.mcpSecretsOf(def);
      this.notify('MCP server ' + name + ' saved' + (unset ? ': set its ' + this.plural(unset, 'secret') + ' below' : ''), 'success');
      if (!unset) this.closeMcpModal();
    } finally { f.saving = false; }
  },
  async setMcpSecret(secret: string) {
    const f = this.mcpForm;
    const row = f.secrets.find((s: { name: string }) => s.name === secret);
    if (!row || f.saving) return;
    f.error = '';
    if (row.value.length < 8) { f.error = 'A secret\'s value is at least 8 characters.'; return; }
    f.saving = true;
    try {
      const res = await this.authFetch('/api/fleet/mcp-servers/' + encodeURIComponent(f.name) + '/secrets/' + encodeURIComponent(secret), { method: 'PUT', headers: { 'X-Skopos-Via': 'dashboard' }, body: JSON.stringify({ value: row.value }) });
      row.value = '';
      if (!res.ok) { f.error = await this.errorText(res, 'The secret was not set'); return; }
      await this.fetchMcpDefs();
      this.mcpSecretsOf(this.mcpDefs.find((m: MCPServerDef) => m.name === f.name));
      this.notify('Secret ' + secret + ' set', 'success');
    } finally { f.saving = false; }
  },
  async unsetMcpSecret(secret: string) {
    const f = this.mcpForm;
    if (f.saving) return;
    f.error = ''; f.saving = true;
    try {
      const res = await this.authFetch('/api/fleet/mcp-servers/' + encodeURIComponent(f.name) + '/secrets/' + encodeURIComponent(secret), { method: 'DELETE', headers: { 'X-Skopos-Via': 'dashboard' } });
      if (!res.ok) { f.error = await this.errorText(res, 'The secret\'s value was not removed'); return; }
      await this.fetchMcpDefs();
      this.mcpSecretsOf(this.mcpDefs.find((m: MCPServerDef) => m.name === f.name));
      this.notify('The value of ' + secret + ' was removed', 'success');
    } finally { f.saving = false; }
  },
  // Two presses. A definition a config still names is refused by the
  // server, which says who names it.
  async deleteMcpDef() {
    const f = this.mcpForm;
    if (f.saving || !f.editing) return;
    if (!f.confirm) { f.confirm = true; return; }
    f.confirm = false; f.error = ''; f.saving = true;
    try {
      const res = await this.authFetch('/api/fleet/mcp-servers/' + encodeURIComponent(f.name), { method: 'DELETE', headers: { 'X-Skopos-Via': 'dashboard' } });
      if (!res.ok) { f.error = await this.errorText(res, 'The MCP server was not deleted'); return; }
      this.notify('MCP server ' + f.name + ' deleted', 'success');
      this.closeMcpModal();
      await this.fetchMcpDefs();
    } finally { f.saving = false; }
  },
  // ---- tabs ----
  pipeTabs(): { id: string; label: string }[] {
    const n = this.pipeBriefs.list.length;
    const tabs = [{ id: 'stages', label: 'Stages' }, { id: 'options', label: 'Options' }, { id: 'briefs', label: n ? 'Briefs (' + n + ')' : 'Briefs' }];
    if (this.pipeConfig.shown) tabs.push({ id: 'config', label: 'Server config' });
    return tabs;
  },
  pipeShowTab(id: string) { this.pipe.tab = id; },
  // Arrow keys move between tabs, as a tab list does.
  pipeTabKey(dir: number) {
    const ids = this.pipeTabs().map((t: { id: string }) => t.id);
    const next = ids[(ids.indexOf(this.pipe.tab) + dir + ids.length) % ids.length];
    this.pipe.tab = next;
    this.$nextTick(() => document.getElementById('pipe-tab-' + next)?.focus());
  },
  // Above the tabs: only what needs attention. The rest is on Options.
  pipeAlerts(): { key: string; tone: string; text: string }[] {
    return this.pipe.notes.filter((n: { key: string; tone: string }) => n.key === 'ro' || n.tone === 'tone-danger' || n.tone === 'tone-warn');
  },
  pipeInfo(): { key: string; tone: string; text: string }[] {
    const alerts = this.pipeAlerts();
    return this.pipe.notes.filter((n: { key: string }) => !alerts.some((a: { key: string }) => a.key === n.key));
  },
  // ---- briefs held on the server ----
  pipeBase(): string { return (this.pipe.kind === 'group' ? '/api/groups/' : '/api/workspaces/') + encodeURIComponent(this.pipe.id) + '/pipeline'; },
  async loadPipeBriefs() {
    this.pipeBriefs.list = [];
    const res = await this.authFetch(this.pipeBase() + '/briefs');
    // A server from before briefs has no such route: the tab stays empty.
    if (res.ok) this.pipeBriefs.list = ((await res.json()) as { briefs?: BriefInfo[] }).briefs || [];
    this.pipeBriefs.rev++;
  },
  // The stages (as the dialog has them now) whose members have this focus.
  pipeFocusUsers(name: string): string[] {
    const out: string[] = [];
    for (const r of this.pipe.rows as PipeRow[]) {
      const members = r.mode === 'own' ? r.members : (r.mode === 'inherited' && r.inherited ? r.inherited.members : []);
      if (members.some((m: PipeMember) => (m.focus || '') === name)) out.push(r.label);
    }
    return out;
  },
  // Every brief held for this owner, then every focus a stage uses that has
  // none: those read the repository's file.
  pipeBriefRows(): BriefRow[] {
    const group = this.pipe.kind === 'group';
    const rows: BriefRow[] = this.pipeBriefs.list.map((b: BriefInfo) => {
      const own = group || b.source === 'workspace';
      const users = this.pipeFocusUsers(b.name);
      const used = users.length ? 'used by ' + users.join(', ') : 'no stage uses it';
      return {
        key: b.name + '|' + b.rev + '|' + used + '|' + this.pipe.readOnly, name: b.name, held: true, own,
        where: own ? 'held here' : 'from ' + (b.source || '').replace('group:', 'group '), tone: own ? 'tone-accent' : 'tone-info',
        detail: this.briefSize(b.bytes) + ' · saved ' + this.timeAgo(b.created_at) + ' by ' + b.created_by + ' · ' + used,
        action: this.pipe.readOnly ? 'View' : (own ? 'Edit' : 'Override here'),
      };
    });
    const held = this.pipeBriefs.list.map((b: BriefInfo) => b.name);
    const seen: string[] = [];
    for (const r of this.pipe.rows as PipeRow[]) {
      const members = r.mode === 'own' ? r.members : (r.mode === 'inherited' && r.inherited ? r.inherited.members : []);
      for (const m of members) {
        const f = m.focus || '';
        if (!f || held.includes(f) || seen.includes(f)) continue;
        seen.push(f);
        rows.push({
          key: 'repo|' + f + '|' + this.pipe.readOnly, name: f, held: false, own: false, where: 'repository', tone: 'tone-neutral',
          detail: 'No brief is held here: read from .skopos/focus/' + f + '.md at the item\'s base commit · used by ' + this.pipeFocusUsers(f).join(', '),
          action: this.pipe.readOnly ? '' : 'Write here',
        });
      }
    }
    return rows;
  },
  briefSize(n: number): string { return n < 1024 ? n + ' bytes' : (n / 1024).toFixed(1) + ' KB'; },
  briefBytes(): number { return new TextEncoder().encode(this.pipeBriefs.body.trim()).length; },
  pipeBriefTooLong(): boolean { return this.briefBytes() > 16384; },
  pipeBriefDirty(): boolean {
    const b = this.pipeBriefs;
    return b.name.trim() !== '' && b.body.trim() !== '' && b.body.trim() !== b.was.trim();
  },
  pipeBriefHint(): string {
    const b = this.pipeBriefs;
    const size = this.briefSize(this.briefBytes()) + ' of 16 KB';
    if (this.pipeBriefTooLong()) return size + ': too long';
    if (b.from) return size + '. This is the text of ' + b.from + '; saving keeps a copy here that wins over it.';
    return size + '. Saved here it is used from the next run on, in place of the repository\'s file of the same name.';
  },
  pipeBriefSaveLabel(): string { return this.pipeBriefs.from ? 'Save here' : 'Save brief'; },
  pipeBriefHistoryLine(h: BriefInfo): string {
    return new Date(h.created_at).toLocaleString() + ' · ' + (h.deleted ? 'removed' : this.briefSize(h.bytes)) + ' · ' + h.created_by;
  },
  // Opens the editor: a new brief, one held here, a group's (to override
  // it here) or a focus that reads the repository so far.
  async pipeBriefOpen(name: string, held: boolean) {
    if (!name && this.pipe.readOnly) return;
    const b = this.pipeBriefs;
    b.name = name; b.nameLocked = !!name; b.body = ''; b.was = ''; b.from = ''; b.history = []; b.confirm = '';
    if (held) {
      const res = await this.authFetch(this.pipeBase() + '/briefs/' + encodeURIComponent(name));
      if (!await this.handleBad(res, 'Could not load the brief')) return;
      const v = (await res.json()) as BriefInfo & { history?: BriefInfo[] };
      b.body = v.body || '';
      const own = this.pipe.kind === 'group' || v.source === 'workspace';
      // A group's text opened in a workspace: saving it makes a copy here.
      b.was = own ? b.body : '';
      b.from = own ? '' : (v.source || '').replace('group:', 'group ');
      b.history = v.history || [];
    }
    b.editing = true;
    this.$nextTick(() => document.getElementById(name ? 'pipe-brief-body' : 'pipe-brief-name')?.focus());
  },
  pipeBriefCancel() { this.pipeBriefs.editing = false; },
  async pipeBriefSave() {
    const b = this.pipeBriefs;
    if (b.saving || this.pipe.readOnly || !this.pipeBriefDirty() || this.pipeBriefTooLong()) return;
    const name = b.name.trim().toLowerCase();
    if (!/^[a-z0-9][a-z0-9_-]{0,63}$/.test(name)) { this.notify('A brief\'s name is lowercase letters, digits, - and _', 'error'); return; }
    b.saving = true;
    try {
      const res = await this.authFetch(this.pipeBase() + '/briefs/' + encodeURIComponent(name), { method: 'PUT', headers: { 'X-Skopos-Via': 'dashboard' }, body: JSON.stringify({ body: b.body }) });
      if (!await this.handleBad(res, 'Brief not saved')) return;
      this.notify('Brief ' + name + ' saved — used from the next run', 'success');
      b.editing = false;
      await this.loadPipeBriefs();
    } finally { b.saving = false; }
  },
  // Two clicks: the first arms the button, the second removes.
  async pipeBriefDelete(name: string) {
    const b = this.pipeBriefs;
    if (this.pipe.readOnly) return;
    if (b.confirm !== name) { b.confirm = name; this.notify('Press Remove again to remove the brief ' + name + ' held here', 'info'); return; }
    b.confirm = '';
    const res = await this.authFetch(this.pipeBase() + '/briefs/' + encodeURIComponent(name), { method: 'DELETE', headers: { 'X-Skopos-Via': 'dashboard' } });
    if (!await this.handleBad(res, 'Brief not removed')) return;
    this.notify('Brief ' + name + ' removed', 'success');
    await this.loadPipeBriefs();
  },
  // Back to the group's value (or to not set when no group sets it).
  pipeClearStage(stage: string) {
    const r = this.pipeRow(stage);
    if (!r) return;
    r.mode = r.inherited ? 'inherited' : 'unset'; r.members = []; r.policy = ''; r.earlyExit = false; r.timeoutMin = '';
    if (this.pipe.editing === stage) this.pipe.editing = '';
    this.pipeBump();
  },
  pipeAddMember(stage: string) {
    const r = this.pipeRow(stage);
    if (!r) return;
    const runner = r.addRunner.trim(), focus = r.addFocus.trim().toLowerCase();
    if (!runner) { this.notify('Type or pick a runner first', 'error'); return; }
    if (r.members.some((m: PipeMember) => m.runner === runner && (m.focus || '') === focus)) { this.notify('That agent is already listed', 'error'); return; }
    if (!r.multi && r.members.length >= 1) { this.notify(r.label + ' takes one runner', 'error'); return; }
    r.members = r.members.concat([{ runner, focus }]);
    r.addRunner = ''; r.addFocus = '';
    this.pipeBump();
  },
  pipeRemoveMember(stage: string, runner: string, focus: string) {
    const r = this.pipeRow(stage);
    if (!r) return;
    r.members = r.members.filter((m: PipeMember) => !(m.runner === runner && (m.focus || '') === (focus || '')));
    this.pipeBump();
  },
  pipeMemberLabel(m: PipeMember): string { return m.focus ? m.focus + ' @ ' + m.runner : m.runner; },
  pipeStageText(st: PipeStage | null): string {
    if (!st) return '';
    let out = st.members.map((m: PipeMember) => this.pipeMemberLabel(m)).join(', ');
    const extra: string[] = [];
    if (st.policy) extra.push(st.policy);
    if (st.early_exit) extra.push('early exit');
    if (st.timeout_ms) extra.push('timeout ' + Math.round(st.timeout_ms / 60000) + 'm');
    if (extra.length) out += ' (' + extra.join(', ') + ')';
    return out;
  },
  pipeRowHint(r: PipeRow): string {
    if (r.mode === 'inherited') return 'from group ' + r.inheritedFrom;
    if (r.mode === 'unset') return r.stage === 'review' || r.stage === 'qa' ? 'not set — skipped' : (r.stage === 'compare' || r.stage === 'synthesize' ? 'not set — off' : (this.pipe.policyBound ? 'not set — any allowed runner' : 'not set — any runner'));
    return r.inherited ? 'set here (overrides group ' + r.inheritedFrom + ')' : 'set here';
  },
  pipeCanAdd(r: PipeRow): boolean { return r.multi || r.members.length === 0; },
  pipeShowGroupOptions(r: PipeRow): boolean { return r.multi && r.members.length > 1; },
  pipeSecondHint(): string {
    if (this.pipe.secondOpinion.trim()) return 'A plain second-opinion request (dashboard, Slack `second`) uses this runner.';
    if (this.pipe.secondInherited) return 'Inherited: ' + this.pipe.secondInherited + '.';
    return 'Not set: each second-opinion request must name a runner.';
  },
  pipeAgentsHint(): string {
    if (this.pipe.agentsDefault) return '';
    if (this.pipe.agentsInherited) return 'Inherited: ' + (this.pipe.agentsInherited === 'solo' ? 'one agent' : 'agent panel') + ' (group ' + this.pipe.agentsInheritedFrom + ')';
    return this.pipe.kind === 'group' ? 'Not set: workspaces default to the agent panel' : 'Not set: items get the agent panel';
  },
  async savePipeline() {
    if (this.pipe.saving || this.pipe.readOnly) return;
    const stages: Record<string, PipeStage> = {};
    for (const r of this.pipe.rows as PipeRow[]) {
      if (r.mode !== 'own') continue;
      if (r.members.length === 0) { this.pipe.tab = 'stages'; this.pipe.editing = r.stage; this.notify(r.label + ': add a runner, or clear the stage', 'error'); return; }
      const st: PipeStage = { members: r.members.map((m: PipeMember) => (m.focus ? { runner: m.runner, focus: m.focus } : { runner: m.runner })) };
      if (r.members.length > 1) {
        if (r.rule && r.policy) st.policy = r.policy;
        if (r.rule && r.earlyExit) st.early_exit = true;
        const mins = parseInt(r.timeoutMin, 10);
        if (r.timeoutMin.trim() && (isNaN(mins) || mins < 1)) { this.notify(r.label + ': timeout is whole minutes, at least 1', 'error'); return; }
        if (mins >= 1) st.timeout_ms = mins * 60000;
      }
      stages[r.stage] = st;
    }
    this.pipe.saving = true;
    try {
      const base = this.pipe.kind === 'group' ? '/api/groups/' : '/api/workspaces/';
      const doc: Record<string, unknown> = { stages, agents_default: this.pipe.agentsDefault, second_opinion_runner: this.pipe.secondOpinion.trim(), via: 'dashboard' };
      // The settings document is replaced whole: carry the code index switch
      // (workspaces only; groups refuse it).
      if (this.pipe.kind === 'workspace') doc.code_index = this.pipe.codeIndex;
      const res = await this.authFetch(base + encodeURIComponent(this.pipe.id) + '/pipeline', { method: 'PUT', body: JSON.stringify(doc) });
      if (!await this.handleBad(res, 'Pipeline not saved')) return;
      this.notify('Pipeline saved — applies from the next stage', 'success');
      this.pipe.was = this.pipeSnapshot();
      // Briefs and the server config save on their own: the dialog does
      // not close over what one of them has not saved yet.
      if (this.pipeConfigDirty()) { this.pipe.tab = 'config'; this.notify('The server config has unsaved changes: Save config, or Undo changes', 'info'); return; }
      if (this.pipeBriefs.editing) { this.pipe.tab = 'briefs'; this.notify('A brief is still open: Save it, or Cancel', 'info'); return; }
      this.pipe.open = false;
    } finally { this.pipe.saving = false; }
  },

  // The code index switch can be turned on only while a worker can fetch the
  // repo; turning it off is always possible.
  pipeIndexLocked(): boolean { return this.pipe.readOnly || (!this.pipe.codeIndex && this.pipe.indexableBy.length === 0); },
  pipeIndexWarn(): boolean { return this.pipe.codeIndex && this.pipe.indexableBy.length === 0; },
  pipeIndexHint(): string {
    const able = this.pipe.indexableBy;
    if (able.length === 0) {
      return this.pipe.codeIndex
        ? 'On, but no worker can fetch this repo right now — the index waits until one can.'
        : 'No worker can fetch this repo right now (a worker needs it configured and its preflight passing).';
    }
    const who = able.length === 1 ? 'worker ' + able[0] : able.length + ' workers (' + able.join(', ') + ')';
    return 'Idle workers index the default branch and each implemented branch, so agents and reviewers query current code. Can index: ' + who + '.';
  },

  // ---- workers ----
  // One row per worker: the registry (what it offers, presence, pause; written
  // on every claim poll) joined with the runs in scope (what it is doing, how
  // its runs went). A worker known only from its runs still gets a row.
  async fetchWorkers() {
    this.workersLoading = true;
    const [runsRes, workersRes] = await Promise.all([
      this.authFetch('/api/runs'),
      this.authFetch('/api/workers'),
    ]);
    if (!runsRes.ok) { this.workersLoading = false; this.notify('Could not load runs', 'error'); return; }
    const runs = (await runsRes.json()) as RunRow[];
    // Registry rows (recorded at claim time): offered runners + served
    // workspaces, even for workers whose runs fall outside this key's scope.
    const registered = new Map<string, WorkerSeen>();
    if (workersRes.ok) {
      for (const w of (await workersRes.json()) as WorkerSeen[]) registered.set(w.id, w);
    }
    const byWorker = new Map<string, WorkerRow>();
    for (const entry of registered) {
      const reg = entry[1];
      byWorker.set(entry[0], { id: entry[0], workspaces: [...(reg.workspaces || [])], stages: [], runners: [...(reg.runners || [])], active: 0, succeeded: 0, failed: 0, lastActive: '', currentLabel: '', state: reg.state || '', version: reg.version || '', lastSeen: reg.last_seen || '', unhealthy: Object.entries(reg.unhealthy || {}).map(([ws, why]) => ws + ': ' + why), indexable: [...(reg.indexable || [])], paused: reg.paused || null, facts: (reg.runner_facts || []).map((f: RunnerFacts) => this.runnerFactsLine(f)), executors: Object.entries(reg.executors || {}).sort((a, b) => a[0] < b[0] ? -1 : 1).map(([ws, facts]) => this.executorsLine(ws, facts as ExecutorFact[])), factsWarning: '', serverConfig: this.workerConfigLine(reg) });
    }
    for (const run of runs) {
      if (!run.worker_id) continue; // queued, never claimed
      let w = byWorker.get(run.worker_id);
      if (!w) { w = { id: run.worker_id, workspaces: [], stages: [], runners: [], active: 0, succeeded: 0, failed: 0, lastActive: run.updated_at, currentLabel: '', state: '', version: '', lastSeen: '', unhealthy: [], indexable: [], paused: null, facts: [], executors: [], factsWarning: '', serverConfig: '' }; byWorker.set(run.worker_id, w); }
      const reg = registered.get(run.worker_id);
      if (reg) { for (const ws of reg.workspaces || []) if (!w.workspaces.includes(ws)) w.workspaces.push(ws); }
      if (run.workspace_id && !w.workspaces.includes(run.workspace_id)) w.workspaces.push(run.workspace_id);
      if (run.stage && !w.stages.includes(run.stage)) w.stages.push(run.stage);
      if (run.status === 'claimed' || run.status === 'running' || run.status === 'paused') { w.active++; w.currentLabel = run.stage + ' \u00b7 ' + run.status + (run.workspace_id ? ' \u00b7 ' + run.workspace_id : '') + ' \u00b7 ' + this.timeAgo(run.updated_at); }
      if (run.status === 'succeeded') w.succeeded++;
      if (run.status === 'failed') w.failed++;
      if (run.updated_at > w.lastActive) w.lastActive = run.updated_at;
    }
    // One runner name should mean one thing: a workspace's policy is
    // matched on what each worker reports, so a name with different
    // providers or models on different workers behaves differently per worker.
    const meanings = new Map<string, Set<string>>();
    for (const entry of registered) {
      for (const f of entry[1].runner_facts || []) {
        if (!meanings.has(f.name)) meanings.set(f.name, new Set<string>());
        (meanings.get(f.name) as Set<string>).add((f.provider || '') + '|' + (f.model || ''));
      }
    }
    for (const entry of registered) {
      const differing = (entry[1].runner_facts || []).filter((f: RunnerFacts) => (meanings.get(f.name) as Set<string>).size > 1).map((f: RunnerFacts) => f.name);
      const row = byWorker.get(entry[0]);
      if (row && differing.length) row.factsWarning = 'Runner ' + differing.join(', ') + ' is a different provider or model on another worker.';
    }
    // Online first (busy, idle), then offline; newest activity within each.
    const rank: Record<string, number> = { busy: 0, idle: 1, paused: 1, offline: 2 };
    this.workers = Array.from(byWorker.values()).sort((a: WorkerRow, b: WorkerRow) => {
      const ra = rank[this.workerStatusLabel(a)] ?? 3, rb = rank[this.workerStatusLabel(b)] ?? 3;
      if (ra !== rb) return ra - rb;
      const la = a.lastSeen || a.lastActive, lb = b.lastSeen || b.lastActive;
      return la < lb ? 1 : -1;
    });
    // Only now: cleared before the rows are in, "no workers yet" would show
    // for a moment on every load, and can stay when the page is in the
    // background (shows and hides are applied by different timers there).
    this.workersLoading = false;
  },
  // The line above the list: how many workers, by state.
  workersSummary(): string {
    const n: Record<string, number> = {};
    let unhealthy = 0;
    for (const w of this.workers) {
      const s = this.workerStatusLabel(w);
      n[s] = (n[s] || 0) + 1;
      if (w.unhealthy.length && s !== 'offline') unhealthy++;
    }
    const parts = ['busy', 'idle', 'paused', 'offline'].filter((s: string) => n[s]).map((s: string) => n[s] + ' ' + s);
    let t = this.workers.length + (this.workers.length === 1 ? ' worker: ' : ' workers: ') + parts.join(', ');
    if (unhealthy) t += ' \u00b7 ' + unhealthy + ' not taking work for a workspace';
    return t;
  },
  // A row's second line: the run it holds, else when it was last heard from.
  workerActivity(w: WorkerRow): string {
    if (w.active > 0 && w.currentLabel) return (w.active > 1 ? w.active + ' runs, newest: ' : '') + w.currentLabel;
    const parts: string[] = [];
    if (w.lastSeen) parts.push('last seen ' + this.timeAgo(w.lastSeen));
    if (w.lastActive) parts.push('last run ' + this.timeAgo(w.lastActive));
    return parts.length ? parts.join(' \u00b7 ') : 'never seen';
  },
  // A row's third line: its runners and the workspaces it serves (the first
  // three by name; Details lists them all).
  workerOffers(w: WorkerRow): string {
    const parts: string[] = [];
    if (w.runners.length) parts.push((w.runners.length === 1 ? 'runner ' : 'runners ') + w.runners.join(', '));
    if (w.workspaces.length) {
      const shown = w.workspaces.slice(0, 3).join(', ');
      parts.push((w.workspaces.length === 1 ? 'workspace ' : 'workspaces ') + (w.workspaces.length > 3 ? shown + ' and ' + (w.workspaces.length - 3) + ' more' : shown));
    }
    return parts.join(' \u00b7 ');
  },
  workerWhen(v: string): string { return v ? this.timeAgo(v) + ' (' + this.formatDateTime(v) + ')' : ''; },
  toggleWorker(w: WorkerRow) { this.workerFold[w.id] = !this.workerFold[w.id]; },
  // What a worker takes from the server: commands for managed workspaces
  // (worker.server_commands), and which runner definitions it holds. Empty
  // for a worker that reports neither (from before workspace config).
  workerConfigLine(w: WorkerSeen): string {
    const parts: string[] = [];
    if (w.server_commands === true) parts.push('takes setup and check commands from the server');
    if (w.server_commands === false) parts.push('keeps setup and check commands in its own file');
    if (w.fleet_rev) parts.push('runner definitions ' + w.fleet_rev);
    return parts.join(' \u00b7 ');
  },
  canAdmin(): boolean {
    return !!this.whoami && (this.whoami.root || !!(this.whoami.key && this.whoami.key.admin));
  },
  // The config block of the pipeline dialog: what the server holds for the
  // workspace or group. A workspace with a config of its own is managed;
  // without one each worker uses its own file.
  async loadPipeConfig(kind: string, id: string) {
    const c = this.pipeConfig;
    c.shown = false; c.status = ''; c.groups = []; c.stageForms = []; c.stagesOpen = false; c.wrapperNames = []; c.was = ''; c.stored = false; c.view = null; c.canEdit = this.canAdmin();
    const base = kind === 'group' ? '/api/groups/' : '/api/workspaces/';
    const res = await this.authFetch(base + encodeURIComponent(id) + '/config');
    // A group's config is its admins' to read; a server from before
    // workspace config has no such route.
    if (!res.ok) return;
    // The wrappers stage blocks can name (a server from before them has no
    // such route).
    c.wrappers = [];
    try {
      const wres = await this.authFetch('/api/fleet/wrappers');
      if (wres.ok) {
        const defs = ((await wres.json()) as { wrappers?: WrapperDef[] }).wrappers || [];
        c.wrappers = defs.map((w: WrapperDef) => this.wrapperLine(w));
        c.wrapperNames = defs.map((w: WrapperDef) => w.name);
      }
    } catch { /* the list is extra */ }
    // The MCP servers a config can name (admins only; older servers have
    // no such route). Never a secret's value: only whether it is set.
    c.mcp = [];
    try {
      const mres = await this.authFetch('/api/fleet/mcp-servers');
      if (mres.ok) c.mcp = (((await mres.json()) as { mcp_servers?: MCPServerDef[] }).mcp_servers || []).map((m: MCPServerDef) => this.mcpServerLine(m));
    } catch { /* the list is extra */ }
    this.showPipeConfig(kind, (await res.json()) as ConfigView);
  },
  // An MCP server definition on one line, with which secrets still lack a
  // value (a run goes without the server until they are set).
  mcpServerLine(m: MCPServerDef): string {
    const where = m.type === 'command' ? 'command ' + (m.command || []).join(' ') : m.url || '';
    const unset = (m.secrets || []).filter((s: { name: string; set: boolean }) => !s.set).map((s: { name: string; set: boolean }) => s.name);
    const secrets = (m.secrets || []).length === 0 ? '' : unset.length ? '   (secret not set: ' + unset.join(', ') + ')' : '   (secrets set)';
    return m.name + ': ' + where + secrets;
  },
  // A wrapper definition on one line. Its check shows that it starts on a
  // worker, never that it isolates.
  wrapperLine(w: WrapperDef): string {
    return w.name + ': ' + (w.argv || []).join(' ') + (w.check && w.check.length ? '   (starts: workers run ' + w.check.join(' ') + ')' : '   (no check)');
  },
  showPipeConfig(kind: string, v: ConfigView) {
    const c = this.pipeConfig;
    c.shown = true;
    c.view = v;
    c.stored = !!v.config;
    c.storedRev = v.rev || '';
    const eff = v.effective;
    c.conflicts = eff && eff.managed ? (eff.conflicts || []).map((x: string) => 'Conflict: ' + x) : [];
    if (kind === 'group') c.status = v.config ? 'Defaults for the managed workspaces of this group (revision ' + (v.rev || '') + ').' : 'This group sets no config.';
    else if (eff && eff.managed) c.status = 'Managed by the server: every worker takes these from here. Runs record config revision ' + (eff.rev || '') + '.';
    else c.status = 'Not managed: each worker uses its own file.';
    this.cfgLoad(kind, v);
  },
  // ---- the config form ----
  cfgGet(obj: Record<string, unknown> | null | undefined, key: string): unknown {
    let cur: unknown = obj;
    for (const part of key.split('.')) {
      if (!cur || typeof cur !== 'object') return undefined;
      cur = (cur as Record<string, unknown>)[part];
    }
    return cur;
  },
  cfgText(v: unknown): string {
    if (v === undefined || v === null) return '';
    if (Array.isArray(v)) return v.join('\n');
    return String(v);
  },
  // The value a row sends. What does not read as the field's kind goes as
  // typed: the server's check says what is wrong with it.
  cfgValue(row: CfgRow): unknown {
    const text = row.value;
    if (row.kind === 'lines') return text.split('\n').map((l: string) => l.trim()).filter((l: string) => l !== '');
    if (row.kind === 'number') return /^\d+$/.test(text.trim()) ? parseInt(text.trim(), 10) : text.trim();
    if (row.key === 'push.steps') return text === 'true';
    return text.trim();
  },
  // Fills the form from what the owner stores and, for each field it does
  // not set, what applies instead: its group's value for a managed
  // workspace, else the default.
  cfgLoad(kind: string, v: ConfigView) {
    const c = this.pipeConfig;
    const own = v.config || {};
    const eff = v.effective && v.effective.managed ? v.effective : null;
    const sources = (eff && eff.sources) || {};
    const open: Record<string, boolean> = {};
    for (const g of c.groups as CfgGroup[]) open[g.name] = g.open;
    const groups: CfgGroup[] = [];
    for (const f of CFG_FIELDS) {
      const mine = this.cfgGet(own, f.key);
      const set = mine !== undefined;
      const src = sources[f.key] || '';
      const inherited = !!eff && src !== '' && src !== 'workspace' && src !== 'default';
      const applies = this.cfgText(inherited ? this.cfgGet(eff!.config, f.key) : this.cfgGet(v.defaults || {}, f.key));
      const row: CfgRow = {
        ...f, options: f.options || [], placeholder: f.placeholder || '', set, error: '',
        value: set ? this.cfgText(mine) : applies, applies,
        from: inherited ? 'the value of ' + src.replace('group:', 'group ') : 'the default' + (applies === '' ? ' (none)' : ''),
      };
      let g = groups.find((x: CfgGroup) => x.name === f.group);
      if (!g) { g = { name: f.group, open: false, rows: [] }; groups.push(g); }
      g.rows.push(row);
    }
    // Folded until opened: each fold's line says what it sets, so the whole
    // config reads on one screen. A fold stays as the reader left it.
    groups.forEach((g: CfgGroup) => { g.open = !!open[g.name]; });
    c.groups = groups;
    c.managed = kind === 'group' ? true : !!v.config;
    const stages = (own.stages || {}) as Record<string, unknown>;
    const stageOpen: Record<string, boolean> = {};
    for (const st of c.stageForms as CfgStage[]) stageOpen[st.stage] = st.open;
    c.stageForms = CFG_STAGES.map(([stage, label]: [string, string]) => {
      const src = sources['stages.' + stage] || '';
      const from = !!eff && src !== '' && src !== 'workspace' && src !== 'default' ? src.replace('group:', 'group ') : '';
      return this.cfgStageForm(stage, label, stages[stage], !!stageOpen[stage], from);
    });
    c.general = ''; c.stagesError = ''; c.checked = '';
    c.was = this.cfgFingerprint();
    c.rev++;
  },
  cfgRows(): CfgRow[] { return (this.pipeConfig.groups as CfgGroup[]).flatMap((g: CfgGroup) => g.rows); },
  // The config as a save sends it: the fields set here, and the stage
  // blocks. null when a stage's form cannot be read (said in its fold).
  cfgDoc(): Record<string, unknown> | null {
    const parts = this.cfgDocParts();
    return parts.problems.length ? null : parts.doc;
  },
  // The same with what the form itself could not read as problems: the
  // rest is still checked by the server, so every problem shows at once.
  cfgDocParts(): { doc: Record<string, unknown>; problems: CfgProblem[] } {
    const doc: Record<string, unknown> = {};
    // A stored field this form has no control for is carried as it is: a
    // save replaces the whole config and must not drop it.
    const known = CFG_FIELDS.map((f: CfgField) => f.key);
    const stored = (this.pipeConfig.view && this.pipeConfig.view.config) || {};
    for (const k of Object.keys(stored)) {
      if (k === 'stages') continue;
      const val = stored[k];
      if (k === 'push' && val && typeof val === 'object') {
        for (const pk of Object.keys(val as Record<string, unknown>)) {
          if (known.includes('push.' + pk)) continue;
          const inner = (doc.push || {}) as Record<string, unknown>;
          inner[pk] = (val as Record<string, unknown>)[pk];
          doc.push = inner;
        }
      } else if (!known.includes(k) && k !== 'push') doc[k] = val;
    }
    for (const row of this.cfgRows()) {
      if (!row.set) continue;
      const parts = row.key.split('.');
      if (parts.length === 1) doc[row.key] = this.cfgValue(row);
      else {
        const inner = (doc[parts[0]] || {}) as Record<string, unknown>;
        inner[parts[1]] = this.cfgValue(row);
        doc[parts[0]] = inner;
      }
    }
    const problems: CfgProblem[] = [];
    const stages: Record<string, unknown> = {};
    for (const st of this.pipeConfig.stageForms as CfgStage[]) {
      if (!st.set) continue;
      const built = this.cfgStageBlock(st);
      if (built.problem) problems.push({ field: 'stages.' + st.stage, message: built.problem });
      stages[st.stage] = built.block;
    }
    if (Object.keys(stages).length) doc.stages = stages;
    return { doc, problems };
  },
  // A stage's block from its form. What the form cannot read (an
  // environment line without a name, sandbox settings that are not JSON)
  // is the problem; the rest of the block is built all the same.
  cfgStageBlock(st: CfgStage): { block: Record<string, unknown>; problem: string } {
    const block: Record<string, unknown> = {};
    let problem = '';
    const tools = this.boxLines(st.tools);
    if (tools.length) block.tools = tools;
    const env = this.boxPairs(st.env, '=');
    if (env.bad) problem = 'an environment line is written NAME=value; this one is not: ' + env.bad;
    if (Object.keys(env.pairs).length) block.env = env.pairs;
    const wrap = st.wraps.filter((w: CfgWrap) => w.wrapper.trim() !== '').map((w: CfgWrap) => {
      const rule: Record<string, string> = { wrapper: w.wrapper.trim() };
      for (const k of ['runner', 'kind', 'provider', 'model'] as const) if (w[k].trim()) rule[k] = w[k].trim();
      return rule;
    });
    if (wrap.length) block.wrap = wrap;
    const settings: Record<string, unknown> = {};
    const permissions: Record<string, string[]> = {};
    const allow = this.boxLines(st.allow), deny = this.boxLines(st.deny), dirs = this.boxLines(st.dirs);
    if (allow.length) permissions.allow = allow;
    if (deny.length) permissions.deny = deny;
    if (dirs.length) permissions.additionalDirectories = dirs;
    if (Object.keys(permissions).length) settings.permissions = permissions;
    if (st.sandbox.trim() !== '') {
      let parsed: unknown = null;
      try { parsed = JSON.parse(st.sandbox); } catch { /* said below */ }
      if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) settings.sandbox = parsed;
      else if (!problem) problem = 'the sandbox settings are not a JSON object';
    }
    if (Object.keys(settings).length) block.settings = settings;
    return { block, problem };
  },
  // A stage's form from its stored block (undefined = not set here).
  cfgStageForm(stage: string, label: string, raw: unknown, open: boolean, from: string): CfgStage {
    const block = (raw && typeof raw === 'object' ? raw : {}) as Record<string, unknown>;
    let settings = block.settings as unknown;
    if (typeof settings === 'string') { try { settings = JSON.parse(settings); } catch { settings = {}; } }
    const set = (settings && typeof settings === 'object' ? settings : {}) as Record<string, unknown>;
    const perms = (set.permissions || {}) as Record<string, string[]>;
    const list = (v: unknown): string => (Array.isArray(v) ? v.join('\n') : '');
    return {
      stage, label, open, from, set: raw !== undefined && raw !== null, error: '',
      tools: list(block.tools), env: this.pairsBox(block.env as Record<string, string> | undefined, '='),
      allow: list(perms.allow), deny: list(perms.deny), dirs: list(perms.additionalDirectories),
      sandbox: set.sandbox ? JSON.stringify(set.sandbox, null, 2) : '',
      wraps: ((block.wrap || []) as Record<string, string>[]).map((w: Record<string, string>) => ({ id: ++cfgWrapSeq, wrapper: w.wrapper || '', runner: w.runner || '', kind: w.kind || '', provider: w.provider || '', model: w.model || '' })),
    };
  },
  cfgStage(stage: string): CfgStage | undefined { return (this.pipeConfig.stageForms as CfgStage[]).find((x: CfgStage) => x.stage === stage); },
  cfgWrapAdd(stage: string) {
    const st = this.cfgStage(stage);
    if (!st) return;
    st.wraps = st.wraps.concat([{ id: ++cfgWrapSeq, wrapper: '', runner: '', kind: '', provider: '', model: '' }]);
  },
  cfgWrapRemove(stage: string, id: number) {
    const st = this.cfgStage(stage);
    if (!st) return;
    st.wraps = st.wraps.filter((w: CfgWrap) => w.id !== id);
    this.cfgTouched();
  },
  cfgStageNote(st: CfgStage): string {
    if (st.set) return 'An empty block is still set: it gives the stage nothing, also where a group gives it something.';
    return st.from ? 'Not set here: the block of ' + st.from + ' applies.' : 'Not set here: no block comes from the server (a worker\'s own profile applies, if it has one).';
  },
  cfgStageSummary(st: CfgStage): string {
    if (st.error) return '1 problem';
    if (!st.set) return st.from ? 'from ' + st.from : 'not set here';
    const block = this.cfgStageBlock(st).block;
    const parts: string[] = [];
    if (block.tools) parts.push(this.plural((block.tools as string[]).length, 'tool'));
    if (block.env) parts.push(this.plural(Object.keys(block.env as object).length, 'variable'));
    if (block.wrap) parts.push('wrapper ' + (block.wrap as Record<string, string>[]).map((w: Record<string, string>) => w.wrapper).join(', '));
    if (block.settings) parts.push('settings');
    return 'set here: ' + (parts.join(', ') || 'nothing given');
  },
  cfgStagesBad(): boolean { return this.pipeConfig.stagesError !== '' || (this.pipeConfig.stageForms as CfgStage[]).some((st: CfgStage) => st.error !== ''); },
  // What would be saved, as text: the form is dirty when it differs.
  cfgFingerprint(): string {
    const c = this.pipeConfig;
    if (this.pipe.kind === 'workspace' && !c.managed) return 'unmanaged';
    const rows = this.cfgRows().filter((r: CfgRow) => r.set).map((r: CfgRow) => [r.key, JSON.stringify(this.cfgValue(r))]);
    const stages = (c.stageForms as CfgStage[]).filter((st: CfgStage) => st.set).map((st: CfgStage) => { const b = this.cfgStageBlock(st); return [st.stage, b.block, b.problem]; });
    return JSON.stringify([rows, stages]);
  },
  cfgEmpty(): boolean { return !this.cfgRows().some((r: CfgRow) => r.set) && !(this.pipeConfig.stageForms as CfgStage[]).some((st: CfgStage) => st.set); },
  // A box ticked: the field starts from what applied. Unticked: it shows
  // that again.
  cfgToggle(key: string) {
    const row = this.cfgRows().find((r: CfgRow) => r.key === key);
    if (!row) return;
    if (!row.set) { row.value = row.applies; row.error = ''; }
    this.cfgTouched();
    if (row.set) this.$nextTick(() => {
      const id = row.kind === 'select' ? 'cfg-sel-' : row.kind === 'lines' ? 'cfg-lines-' : 'cfg-';
      document.getElementById(id + key)?.focus();
    });
  },
  cfgFold(name: string, open: boolean) {
    if (name === 'stages') { this.pipeConfig.stagesOpen = open; return; }
    if (name.startsWith('stage:')) { const st = this.cfgStage(name.slice(6)); if (st) st.open = open; return; }
    const g = (this.pipeConfig.groups as CfgGroup[]).find((x: CfgGroup) => x.name === name);
    if (g) g.open = open;
  },
  cfgRowsOf(row: CfgRow): number { return Math.min(8, Math.max(2, row.value.split('\n').length)); },
  cfgNote(row: CfgRow): string {
    if (row.error) return row.error;
    return row.set ? row.hint : 'Not set here: ' + row.from + ' applies. ' + row.hint;
  },
  cfgGroupBad(g: CfgGroup): boolean { return g.rows.some((r: CfgRow) => r.error !== ''); },
  cfgGroupSummary(g: CfgGroup): string {
    const bad = g.rows.filter((r: CfgRow) => r.error !== '').length;
    if (bad) return this.plural(bad, 'problem');
    const set = g.rows.filter((r: CfgRow) => r.set).map((r: CfgRow) => r.label);
    return set.length ? 'set here: ' + set.join(', ') : 'nothing set here';
  },
  cfgStagesSummary(): string {
    const forms = this.pipeConfig.stageForms as CfgStage[];
    const bad = forms.filter((st: CfgStage) => st.error !== '').length + (this.pipeConfig.stagesError ? 1 : 0);
    if (bad) return this.plural(bad, 'problem');
    const set = forms.filter((st: CfgStage) => st.set).map((st: CfgStage) => st.label);
    return set.length ? 'set here: ' + set.join(', ') : 'nothing set here';
  },
  cfgState(): string {
    const c = this.pipeConfig;
    if (c.checking) return 'Checking…';
    const bad = this.cfgRows().filter((r: CfgRow) => r.error !== '').length + (c.stageForms as CfgStage[]).filter((st: CfgStage) => st.error !== '').length + (c.stagesError ? 1 : 0) + (c.general ? 1 : 0);
    if (bad) return this.plural(bad, 'problem') + ' to fix';
    if (!this.pipeConfigDirty()) return '';
    return c.checked === this.cfgFingerprint() ? 'Checked: it can be saved' : '';
  },
  cfgReset() { if (this.pipeConfig.view) this.cfgLoad(this.pipe.kind, this.pipeConfig.view); },
  // Every edit schedules a check by the server, so a problem shows next to
  // its field while the form is still open.
  cfgTouched() {
    if (cfgCheckTimer) clearTimeout(cfgCheckTimer);
    cfgCheckTimer = setTimeout(() => { this.cfgCheck(); }, 600);
  },
  cfgProblems(problems: CfgProblem[]) {
    const c = this.pipeConfig;
    const rows = this.cfgRows();
    for (const r of rows) r.error = '';
    const forms = c.stageForms as CfgStage[];
    for (const st of forms) st.error = '';
    c.general = ''; c.stagesError = '';
    const general: string[] = [];
    for (const p of problems) {
      const row = rows.find((r: CfgRow) => r.key === p.field);
      const st = p.field.startsWith('stages.') ? forms.find((x: CfgStage) => 'stages.' + x.stage === p.field && x.set) : undefined;
      if (row && row.set) row.error = p.message;
      else if (st) st.error = (st.error ? st.error + ' ' : '') + p.message;
      // About the stage blocks, but no one stage: said at their fold.
      else if (p.field === 'stages') c.stagesError = (c.stagesError ? c.stagesError + ' ' : '') + p.message;
      else general.push((p.field ? p.field + ': ' : '') + p.message);
    }
    c.general = general.join(' ');
    // A problem is not left inside a closed fold.
    for (const g of c.groups as CfgGroup[]) if (this.cfgGroupBad(g)) g.open = true;
    for (const st of forms) if (st.error) { st.open = true; c.stagesOpen = true; }
    if (c.stagesError) c.stagesOpen = true;
  },
  // Asks the server what a save would say. true = no problems.
  async cfgCheck(): Promise<boolean> {
    const c = this.pipeConfig;
    if (!c.canEdit || !c.shown) return false;
    if ((this.pipe.kind === 'workspace' && !c.managed) || !this.pipeConfigDirty()) { this.cfgProblems([]); return true; }
    // What the form itself cannot read is a problem of its own; the rest
    // is checked all the same, so every problem shows at once.
    const { doc, problems: own } = this.cfgDocParts();
    const print = this.cfgFingerprint();
    const seq = ++cfgCheckSeq;
    c.checking = true;
    try {
      const res = await this.authFetch(this.cfgURL() + '/check', { method: 'POST', body: JSON.stringify({ config: doc }) });
      if (seq !== cfgCheckSeq) return false; // an older answer
      if (!res.ok) { c.general = 'The check could not run (HTTP ' + res.status + ').'; return false; }
      const out = (await res.json()) as { problems?: CfgProblem[] };
      // The form's own word about a stage stands in for the server's.
      const mine = own.map((p: CfgProblem) => p.field);
      const problems = own.concat((out.problems || []).filter((p: CfgProblem) => !mine.includes(p.field)));
      this.cfgProblems(problems);
      c.checked = problems.length ? '' : print;
      return problems.length === 0;
    } finally { if (seq === cfgCheckSeq) c.checking = false; }
  },
  cfgURL(): string { return (this.pipe.kind === 'group' ? '/api/groups/' : '/api/workspaces/') + encodeURIComponent(this.pipe.id) + '/config'; },
  pipeConfigHint(): string {
    if (!this.pipeConfig.canEdit) return 'Changing it needs the admin permission (skopos workspace config set).';
    return this.pipe.kind === 'group'
      ? 'A field ticked here is this group\'s value for its managed workspaces. With nothing ticked the group sets no config.'
      : 'A field ticked here is this workspace\'s own value. The commands and the stage blocks reach a worker only when its file sets server_commands.';
  },
  pipeConfigDirty(): boolean { return this.pipeConfig.shown && this.cfgFingerprint() !== this.pipeConfig.was; },
  // What Save does here: remove the stored config (a workspace handed back
  // to its workers, a group with nothing set), or store the form.
  cfgRemoves(): boolean {
    const c = this.pipeConfig;
    return c.stored && ((this.pipe.kind === 'workspace' && !c.managed) || (this.pipe.kind === 'group' && this.cfgEmpty()));
  },
  pipeConfigSaveLabel(): string {
    if (this.pipeConfig.saving) return 'Saving…';
    return this.cfgRemoves() ? 'Remove config' : 'Save config';
  },
  async savePipeConfig() {
    const c = this.pipeConfig;
    if (c.saving || !c.canEdit || !this.pipeConfigDirty()) return;
    if (cfgCheckTimer) { clearTimeout(cfgCheckTimer); cfgCheckTimer = null; }
    const removes = this.cfgRemoves();
    // A workspace that holds no config here and is not asked to: nothing to
    // save. (An empty PUT would make it managed.)
    if (!removes && this.pipe.kind === 'workspace' && !c.managed) return;
    // Nothing stored and nothing to store: a group with no field ticked.
    if (!removes && this.pipe.kind === 'group' && this.cfgEmpty()) { this.notify('Tick a field to set it for this group', 'info'); return; }
    c.saving = true;
    try {
      let res: Response;
      if (removes) {
        res = await this.authFetch(this.cfgURL(), { method: 'DELETE' });
      } else {
        if (!await this.cfgCheck()) { this.notify('The config has problems: see the marked fields', 'error'); return; }
        // if_rev: a save over someone else's newer config is refused.
        res = await this.authFetch(this.cfgURL(), { method: 'PUT', body: JSON.stringify({ config: this.cfgDoc(), if_rev: c.storedRev }) });
      }
      if (res.status === 409) { this.notify('The stored config changed since this form was loaded: reopen the dialog and apply your changes again', 'error'); return; }
      if (!await this.handleBad(res, 'Config not saved')) return;
      const v = (await res.json()) as ConfigView;
      // The answer to a save carries no defaults: keep the ones loaded.
      if (!v.defaults && c.view) v.defaults = c.view.defaults;
      this.showPipeConfig(this.pipe.kind, v);
      this.notify(removes ? 'Config removed' : ((v.changed || []).length ? 'Config saved: ' + (v.changed || []).join(', ') : 'Nothing changed'), 'success');
    } finally {
      c.saving = false;
    }
  },
  // One runner as its worker reports it: name, provider, model, endpoint.
  runnerFactsLine(f: RunnerFacts): string {
    const parts = [f.provider || 'unknown provider'];
    if (f.model) parts.push(f.model);
    if (f.endpoint) parts.push(f.endpoint);
    if ((f.stages || []).length) parts.push('read-only stages');
    return f.name + ': ' + parts.join(' \u00b7 ');
  },
  // What contains a worker's runs in one workspace: the default, then the
  // stages and runners that differ.
  executorsLine(ws: string, facts: ExecutorFact[]): string {
    const parts = (facts || []).map((f: ExecutorFact) => {
      const e = this.executorText(f.executor);
      if (f.stage && f.runner) return f.stage + '/' + f.runner + ': ' + e;
      if (f.stage) return f.stage + ': ' + e;
      if (f.runner) return f.runner + ': ' + e;
      return e;
    });
    return ws + ': ' + parts.join(' \u00b7 ');
  },
  // An executor id as shown: none:… is a case the worker cannot run.
  executorText(e: string): string {
    if (e === 'none:wrapper') return 'cannot run (the wrapper its config asks for is not usable on this worker)';
    if (e === 'none:uncontained') return 'cannot run (the stage is given tools or env and nothing contains it on this worker)';
    if (e.startsWith('none:')) return 'cannot run (' + e + ')';
    return e;
  },
  // A policy rule the way the CLI writes it.
  policyRuleText(r: PolicyRule): string {
    const parts: string[] = [];
    if (r.provider) parts.push('provider=' + r.provider);
    if (r.model) parts.push('model=' + r.model);
    if (r.kind) parts.push('kind=' + r.kind);
    if (r.runner) parts.push('runner=' + r.runner);
    if ((r.stages || []).length) parts.push('stages=' + (r.stages || []).join('|'));
    if (r.executor) parts.push('executor=' + r.executor);
    return parts.length ? parts.join(',') : 'any';
  },
  // Presence comes from the server (busy / idle / offline); a worker known
  // only from run history (pre-presence) falls back to its runs.
  workerStatusLabel(w: WorkerRow): string { return w.state || (w.active > 0 ? 'busy' : 'offline'); },
  workerStatusClass(w: WorkerRow): string {
    const s = this.workerStatusLabel(w);
    if (w.unhealthy.length && s !== 'offline') return 'tone-danger';
    if (s === 'paused') return 'tone-warn';
    return s === 'busy' ? 'tone-success' : s === 'idle' ? 'tone-info' : 'tone-muted';
  },
  // The badge: an online worker whose preflight fails somewhere says so; a
  // busy one that is paused finishes its run first.
  workerBadge(w: WorkerRow): string {
    const s = this.workerStatusLabel(w);
    if (s === 'busy' && w.paused) return 'busy · pausing';
    return w.unhealthy.length && s !== 'offline' ? s + ' · unhealthy' : s;
  },
  // "Paused until 16:30 by key alex: working on it by hand".
  workerPauseText(w: WorkerRow): string {
    const p = w.paused;
    if (!p) return '';
    let t = p.until ? 'Paused until ' + this.formatDateTime(p.until) : 'Paused until resumed';
    if (p.by) t += ' by ' + p.by;
    if (p.reason) t += ': ' + p.reason;
    return t;
  },
  formatDateTime(v: string): string {
    if (!v) return '';
    return new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }).format(new Date(v));
  },
  openPauseWorker(w: WorkerRow) { this.pausingWorker = w.id; this.pauseForm = { for: '2h', reason: '' }; },
  closePauseWorker() { this.pausingWorker = ''; },
  // Pause the worker the dialog names: it takes no new work (a run it holds
  // finishes).
  async pauseWorker() {
    const id = this.pausingWorker;
    if (!id || this.workerBusy) return;
    this.workerBusy = true;
    const body: Record<string, string> = { reason: this.pauseForm.reason.trim() };
    if (this.pauseForm.for) body.for = this.pauseForm.for;
    const res = await this.authFetch('/api/workers/' + encodeURIComponent(id) + '/pause', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
    this.workerBusy = false;
    if (!await this.handleBad(res, 'Could not pause the worker')) return;
    this.pausingWorker = '';
    this.notify(id + ' paused', 'success');
    await this.fetchWorkers();
  },
  async resumeWorker(w: WorkerRow) {
    if (this.workerBusy) return;
    this.workerBusy = true;
    const res = await this.authFetch('/api/workers/' + encodeURIComponent(w.id) + '/resume', { method: 'POST' });
    this.workerBusy = false;
    if (!await this.handleBad(res, 'Could not resume the worker')) return;
    this.notify(w.id + ' resumed', 'success');
    await this.fetchWorkers();
  },

  // ---- run rows (preview modal) ----
  shortSha(sha: string): string { return sha ? sha.slice(0, 7) : ''; },
  // What a run cost, when its runner reports it ("" otherwise).
  runCost(run: RunRow): string { return run.cost_usd != null && run.cost_usd > 0 ? '$' + run.cost_usd.toFixed(2) : ''; },
  // A succeeded run can still carry a chain NOTE in fail_reason (e.g. a
  // quiet matrix skip) — that is diagnostics, not a failure line.
  runHasFailed(run: RunRow): boolean { return !!run.fail_reason && run.status !== 'succeeded'; },

  // ---- inbox board (swimlanes) ----
  inboxLanes(): { key: string; label: string }[] {
    return [
      { key: 'open', label: 'Open' },
      { key: 'in_progress', label: 'In progress' },
      { key: 'workflow', label: 'Agent workflow' },
      { key: 'converted', label: 'Converted' },
      { key: 'done', label: 'Done' },
      { key: 'discarded', label: 'Discarded' },
    ];
  },
  // Workflow statuses (agent-pipeline §1): the aggregate lane owns them, so
  // no item can ever be without a lane — the no-invisibility guarantee.
  // Status and stage names as words in badges (awaiting_approval →
  // awaiting approval); the underscores are API values, not labels.
  statusText(s: string): string { return (s || '').replace(/_/g, ' '); },
  isWorkflowStatus(s: string): boolean {
    return s === 'queued' || s === 'planning' || s === 'awaiting_approval' || s === 'approved' || s === 'implementing' || s === 'in_review' || s === 'failed' || s === 'blocked';
  },
  inboxItemEditable(item: InboxItem): boolean { return item.status === 'open' || item.status === 'in_progress'; },
  inboxContentEditable(item: InboxItem): boolean { return item.status !== 'done' && item.status !== 'discarded'; },
  laneItems(key: string): InboxItem[] {
    if (key === 'workflow') return this.sortInbox(this.inboxItems.filter((i: InboxItem) => this.isWorkflowStatus(i.status)));
    return this.sortInbox(this.inboxItems.filter((i: InboxItem) => i.status === key));
  },
  // Imperative lane-placeholder sync: x-show inside x-for rows reading OUTER
  // state (inboxItems) never re-runs in the CSP build — method call or
  // compound expression alike (the syncInboxExpansion rationale). The "—"
  // empties are therefore driven directly after every items change; the
  // x-show on the element stays as the boot-time baseline.
  syncInboxLanes() {
    this.$nextTick(() => {
      document.querySelectorAll('[data-lane-ph]').forEach((el) => {
        (el as HTMLElement).style.display = this.laneIsEmpty(el.getAttribute('data-lane-ph') || '') ? '' : 'none';
      });
    });
  },
  laneIsEmpty(key: string): boolean { return this.laneItems(key).length === 0; },
  // Draggable: every card — done included (drag back to Open = reopen).
  inboxDraggable(item: InboxItem): boolean {
    return true;
  },
  laneAcceptsDrop(key: string): boolean {
    const from = this.inboxItems.find((i: InboxItem) => i.id === this.inboxDragId);
    if (!from) return false;
    // Workflow items move only through the gated API actions — never by
    // dragging (the human actions need the approver permission).
    if (this.isWorkflowStatus(from.status)) return false;
    if (key === 'workflow') return false;
    if (key === from.status) return true; // reorder within the lane
    if (key === 'in_progress' && from.status === 'open') return true;
    // Open takes back released, discarded (restore), and done (reopen).
    if (key === 'open' && (from.status === 'in_progress' || from.status === 'discarded' || from.status === 'done')) return true;
    if (key === 'discarded' && from.status !== 'done' && from.status !== 'discarded') return true;
    // Manual complete: work finished without a plan, or ahead of it.
    if (key === 'done' && from.status !== 'done' && from.status !== 'discarded') return true;
    return false; // converted needs a plan (Convert button); done→in_progress would invent a claim
  },
  startInboxDrag(item: InboxItem, ev: DragEvent) {
    this.inboxDragId = item.id;
    // Dim the source imperatively: a :class on row state from OUTER scope
    // does not re-evaluate in the CSP build (same family as x-show-in-x-for).
    (ev.currentTarget as HTMLElement).classList.add('drag-src');
    if (ev.dataTransfer) {
      ev.dataTransfer.setData('text/plain', item.id); // Firefox requires data
      ev.dataTransfer.effectAllowed = 'move';
    }
  },
  endInboxDrag() {
    this.inboxDragId = '';
    clearInboxDropMarker();
    document.querySelectorAll('.drag-src').forEach((el) => el.classList.remove('drag-src'));
  },
  laneDragOver(key: string, ev: DragEvent) {
    if (!this.laneAcceptsDrop(key)) return; // no preventDefault -> drop not allowed
    if (ev.dataTransfer) ev.dataTransfer.dropEffect = 'move';
    ev.preventDefault();
    // Card dragovers bubble into this handler right after cardDragOver set
    // the marker — only clear it when the hover genuinely left the cards
    // (lane padding / empty-lane placeholder).
    if (inboxDropMarker && !(ev.target instanceof Node && inboxDropMarker.contains(ev.target))) clearInboxDropMarker();
  },
  // Dragging over a CARD: same-lane shows WHERE the item will land (inset
  // edge above/below the hovered card — dropBeforeItem ranks it there);
  // cross-lane only shows "this lane takes it" since position is meaningless
  // across lanes. Imperative classList: the CSP build does not reliably
  // re-evaluate row-level directives during a drag.
  cardDragOver(key: string, item: InboxItem, ev: DragEvent) {
    if (!this.laneAcceptsDrop(key)) return; // no preventDefault -> drop not allowed
    if (ev.dataTransfer) ev.dataTransfer.dropEffect = 'move';
    ev.preventDefault();
    const el = ev.currentTarget as HTMLElement;
    const from = this.inboxItems.find((i: InboxItem) => i.id === this.inboxDragId);
    // Positional marker only in rank-ordered views; a date-sorted lane takes
    // cross-lane transitions only (drop-into). Same-lane in a date view is a
    // no-op — show nothing rather than a misleading drop-into outline.
    const reorderable = from != null && from.status === key && this.inboxSort === 'priority';
    if (from != null && from.status === key && !reorderable) { clearInboxDropMarker(); return; }
    const rect = el.getBoundingClientRect();
    const cls = reorderable ? (ev.clientY < rect.top + rect.height / 2 ? 'drop-before' : 'drop-after') : 'drop-into';
    if (el === inboxDropMarker) {
      // Same card, position may have flipped top/bottom half.
      if (!el.classList.contains(cls)) { el.classList.remove('drop-before', 'drop-after', 'drop-into'); el.classList.add(cls); }
      return;
    }
    clearInboxDropMarker();
    el.classList.add(cls);
    inboxDropMarker = el;
  },
  // Drop on a lane (its empty area or below all cards): a cross-lane drop is
  // the status transition; a same-lane drop at the end unprioritizes.
  async dropOnLane(key: string) {
    const dragId = this.inboxDragId;
    this.inboxDragId = '';
    clearInboxDropMarker();
    if (!dragId) return;
    const from = this.inboxItems.find((i: InboxItem) => i.id === dragId);
    if (!from) return;
    if (key !== from.status) { await this.inboxTransition(dragId, from.status, key); return; }
    if (key !== 'open' && key !== 'in_progress') return;
    if (this.inboxSort !== 'priority') return; // rank ops need the rank-ordered view
    // Same lane, dropped at the end: the bottom is the unordered zone, so a
    // prioritized item gets unpinned; an unordered one is already there.
    if (from.priority != null) await this.inboxSetPriority(dragId, 0);
  },
  // Drop onto a specific card: cross-lane = transition; same-lane = rank the
  // dragged item right before the target card (dropping among the unordered
  // tail clears the dragged item's priority instead).
  async dropBeforeItem(key: string, target: InboxItem) {
    const dragId = this.inboxDragId;
    this.inboxDragId = '';
    clearInboxDropMarker();
    if (!dragId || dragId === target.id) return;
    const from = this.inboxItems.find((i: InboxItem) => i.id === dragId);
    if (!from) return;
    if (key !== from.status) { await this.inboxTransition(dragId, from.status, key); return; }
    if (key !== 'open' && key !== 'in_progress') return;
    if (this.inboxSort !== 'priority') return; // ranking into a date-sorted view would land invisibly
    // Rank against the server's unfiltered ordered prefix (the visible lane
    // may be tag-filtered); dropping onto an UNORDERED target unpins.
    const ordered = await this.inboxOrderedIds(dragId);
    if (ordered === null) return;
    const targetIdx = ordered.indexOf(target.id);
    if (targetIdx >= 0) {
      // Target is ranked: the dragged item takes its rank, the rest shifts.
      const ids = ordered.slice(0, targetIdx).concat([dragId], ordered.slice(targetIdx));
      const res = await this.authFetch('/api/inbox/reorder', { method: 'POST', body: JSON.stringify({ ids }) });
      if (!await this.handleBad(res, 'Reorder failed')) return;
      await this.fetchInbox();
    } else if (from.priority != null) {
      // Dropped onto an unordered card: unpin.
      await this.inboxSetPriority(dragId, 0);
    }
  },
  async inboxTransition(id: string, from: string, to: string) {
    // Capture the source lane's ordered prefix from the server truth first:
    // when a ranked item leaves, the remaining ranks are compacted (no
    // #2-without-#1) — unfiltered, so hidden ranked items renumber too.
    const sourceOrdered = await this.inboxOrderedIds(id);
    if (from === 'open' && to === 'in_progress') { await this.claimInboxItem(id); }
    else if (from === 'in_progress' && to === 'open') { await this.releaseInboxItem(id); }
    else if (from === 'discarded' && to === 'open') { await this.restoreInboxItem(id); }
    else if (from === 'done' && to === 'open') { await this.reopenInboxItem(id); }
    else if (to === 'discarded') { await this.discardInboxItem(id); }
    else if (to === 'done') { await this.completeInboxItem(id); }
    else return;
    if (sourceOrdered && sourceOrdered.length > 0) {
      const res = await this.authFetch('/api/inbox/reorder', { method: 'POST', body: JSON.stringify({ ids: sourceOrdered }) });
      await this.handleBad(res, 'Rank compaction failed');
      await this.fetchInbox();
    }
  },
  // Server-truth ordered prefix for reorder/pin/compaction: the visible
  // list may be tag- or status-filtered, and reorder semantics keep the
  // priority of every id NOT sent — computing ids from a filtered view would
  // silently un-sync hidden ranked items (duplicate #1s). The slice is the
  // DRAGGED ITEM's board: reorder is per-workspace and open/in_progress only,
  // so the all-workspaces view must not mix foreign workspaces in (400), and
  // converted items must be excluded even though they keep stale priorities.
  // contextId picks the workspace when no filter is active (default: the
  // excluded item's own).
  async inboxOrderedIds(excludeId: string, contextId = excludeId): Promise<string[] | null> {
    const from = this.inboxItems.find((i: InboxItem) => i.id === contextId);
    const ws = this.activeWorkspace || (from ? from.workspace_id || '' : '');
    const p = new URLSearchParams();
    if (ws) p.set('workspace', ws);
    const qs = p.size ? '?' + p.toString() : '';
    const res = await this.authFetch('/api/inbox' + qs);
    if (!res.ok) return null;
    const all = (await res.json()) as InboxItem[];
    return all
      .filter((i: InboxItem) => (i.status === 'open' || i.status === 'in_progress') && i.priority != null && i.id !== excludeId)
      .map((i: InboxItem) => i.id);
  },
  async inboxSetPriority(id: string, priority: number) {
    const res = await this.authFetch('/api/inbox/' + encodeURIComponent(id), { method: 'PATCH', body: JSON.stringify({ priority }) });
    if (!await this.handleBad(res, 'Priority not updated')) return;
    await this.fetchInbox();
  },
  // Manual done — work finished without a plan, or ahead of it. Server-side
  // twin of the plan-completion hook's automatic flip.
  async completeInboxItem(id: string) {
    const res = await this.authFetch('/api/inbox/' + encodeURIComponent(id) + '/complete', { method: 'POST' });
    if (!await this.handleBad(res, 'Complete failed')) return;
    this.notify('Marked done', 'success');
    await this.fetchInbox();
  },
  // Undo for a wrong manual complete: back to open, claim/plan/priority reset.
  async reopenInboxItem(id: string) {
    const res = await this.authFetch('/api/inbox/' + encodeURIComponent(id) + '/reopen', { method: 'POST' });
    if (!await this.handleBad(res, 'Reopen failed')) return;
    this.notify('Reopened', 'success');
    await this.fetchInbox();
  },
  // Pin = rank first against the server's unfiltered ordered prefix.
  async pinInboxItem(id: string) {
    const ordered = await this.inboxOrderedIds(id);
    if (ordered === null) return;
    const ids = [id].concat(ordered);
    const res = await this.authFetch('/api/inbox/reorder', { method: 'POST', body: JSON.stringify({ ids }) });
    if (!await this.handleBad(res, 'Pin failed')) return;
    this.notify('Pinned to top', 'success');
    await this.fetchInbox();
  },
  // Move a ranked item one place up (-1) or down (+1): the single-pointer
  // alternative to drag-to-rank on the board (WCAG 2.5.7). Swaps against the
  // server's ordered prefix, like drops do.
  async moveInboxItem(id: string, dir: number) {
    const ordered = await this.inboxOrderedIds('', id);
    if (ordered === null) return;
    const i = ordered.indexOf(id), j = i + dir;
    if (i < 0 || j < 0 || j >= ordered.length) return;
    [ordered[i], ordered[j]] = [ordered[j], ordered[i]];
    const res = await this.authFetch('/api/inbox/reorder', { method: 'POST', body: JSON.stringify({ ids: ordered }) });
    if (!await this.handleBad(res, 'Move failed')) return;
    await this.fetchInbox();
  },
  inboxRankedCount(): number { return this.inboxItems.filter((i: InboxItem) => i.priority != null).length; },
  // The editor host div lives inside the modal; it only exists in the DOM
  // once the modal renders, hence the $nextTick mount.
  mountInboxEditor(initial: string) {
    this.$nextTick(() => {
      // By id, not $refs: magics bind to the CALLING element, and Edit from
      // the preview modal is called from a button its own x-if has already
      // removed — a detached element's $refs finds no host, and the editor
      // silently never mounted (empty content, Save would blank the item).
      const host = document.getElementById('ib-content');
      if (!host) return;
      inboxEditor = destroyEditor(inboxEditor);
      inboxEditor = mountMarkdownEditor(host, initial, {
        // Cmd/Ctrl+Enter saves straight from the editor.
        onSubmit: () => { this.submitInboxItem(); },
        labelledBy: 'ib-content-lbl',
      });
      inboxEditor.focus();
    });
  },
  openInboxCreateModal() {
    this.inboxForm = emptyInboxForm();
    // Preselect the active workspace filter so the common path is one click;
    // with "All workspaces" the picker starts empty and must be answered
    // explicitly (an inline field error, not a silent toast).
    this.inboxForm.workspace = this.activeWorkspace;
    this.inboxErrors = {};
    this.inboxBaseline = { title: '', tags: '', content: '', workspace: this.activeWorkspace, agents: '', branch: '', ref: '' };
    this.showInboxItemModal = true;
    this.syncInboxWsField();
    this.mountInboxEditor('');
  },
  async openInboxEditModal(item: InboxItem) {
    // Edit needs the full content: fetch the detail even if already expanded.
    const res = await this.authFetch('/api/inbox/' + encodeURIComponent(item.id));
    if (!await this.handleBad(res, 'Could not load item')) return;
    const detail = (await res.json()) as InboxDetail;
    this.inboxForm = {
      id: detail.id, title: detail.title, tags: (detail.tags || []).join(', '), workspace: detail.workspace_id || '',
      agents: detail.agents || '', branch: detail.branch || '', ref: detail.ref || '', wsLocked: this.isWorkflowStatus(detail.status),
    };
    this.inboxBaseline = { title: detail.title, tags: this.inboxForm.tags, content: detail.content, workspace: detail.workspace_id || '', agents: detail.agents || '', branch: detail.branch || '', ref: detail.ref || '' };
    this.inboxErrors = {};
    this.showInboxItemModal = true;
    this.syncInboxWsField();
    this.mountInboxEditor(detail.content);
  },
  // Imperative, like syncSessionPurge: a new x-show on a static element is
  // one of the CSP build's silent failure modes. Workflow items keep their
  // workspace (the server refuses the move), so the picker is hidden and
  // disabled for them.
  syncInboxWsField() {
    const locked = this.inboxForm.wsLocked;
    this.$nextTick(() => {
      const field = document.querySelector('[data-ib-ws-field]') as HTMLElement | null;
      if (field) field.style.display = locked ? 'none' : '';
      const sel = document.getElementById('ib-ws') as HTMLSelectElement | null;
      if (sel) sel.disabled = locked;
    });
  },
  closeInboxItemModal() {
    this.showInboxItemModal = false;
    inboxEditor = destroyEditor(inboxEditor);
  },
  // Unsaved-edits guard: the backdrop, Escape, and Cancel all route here.
  // Closing an editor with a long markdown draft in it must be deliberate.
  inboxItemIsDirty(): boolean {
    return editorText(inboxEditor) !== this.inboxBaseline.content
      || this.inboxForm.title !== this.inboxBaseline.title
      || this.inboxForm.tags !== this.inboxBaseline.tags
      || this.inboxForm.workspace !== this.inboxBaseline.workspace
      || this.inboxForm.agents !== this.inboxBaseline.agents
      || this.inboxForm.branch !== this.inboxBaseline.branch
      || this.inboxForm.ref !== this.inboxBaseline.ref;
  },
  // Per-item multi-agent switch: badge text and its explanation.
  agentsLabel(agents?: string): string {
    return agents === 'panel' ? 'agent panel' : agents === 'solo' ? 'one agent' : '';
  },
  agentsTitle(agents?: string): string {
    return agents === 'panel' ? "Plan, review and QA use the repository's agent panel"
      : agents === 'solo' ? 'Plan, review and QA use one agent' : '';
  },
  // The item's branch (or reference) badge; review-only items say so.
  branchLabel(it?: InboxItem | null): string {
    if (!it) return '';
    const b = it.branch || it.ref || '';
    if (it.flow === 'review') return 'review only' + (b ? ' · ' + b : '');
    return b;
  },
  branchTitle(it?: InboxItem | null): string {
    if (!it) return '';
    if (it.flow === 'review') return 'The agent checks and reviews this existing branch; nothing is planned or implemented';
    if (it.branch) return 'Works on the branch ' + it.branch + ': an existing one is continued, a new one is created under this name';
    return it.ref ? 'Reference ' + it.ref + ': the repository\'s branch template names the branch after it' : '';
  },
  attemptCloseInboxItemModal() {
    if (!this.showInboxItemModal || this.confirm.open) return;
    if (!this.inboxItemIsDirty()) { this.closeInboxItemModal(); return; }
    this.confirm = {
      open: true, title: 'Discard changes?', label: 'Discard', busy: false,
      message: 'This item has unsaved edits. Discard them and close the editor?',
      pending: { kind: 'inboxdiscard', id: '' },
    };
  },
  async submitInboxItem() {
    if (this.inboxItemSaving) return; // Mod-Enter can double-fire
    const f = this.inboxForm, e: Record<string, string> = {};
    if (!f.title.trim()) e.title = 'Title is required.';
    if (!f.id && !f.workspace) e.workspace = 'Choose a workspace — or “Unfiled” to decide later.';
    this.inboxErrors = e;
    if (Object.keys(e).length) return;
    this.inboxItemSaving = true;
    try {
      const content = editorText(inboxEditor);
      const tags = f.tags.split(',').map((t: string) => t.trim()).filter(Boolean);
      const editing = !!f.id;
      let opened: InboxItem | null = null;
      if (!editing) {
        const body: Record<string, unknown> = { title: f.title.trim(), content, tags, author_agent_id: UI_AUTHOR };
        if (f.agents) body.agents = f.agents;
        if (f.branch.trim()) body.branch = f.branch.trim();
        if (f.ref.trim()) body.ref = f.ref.trim();
        // 'unfiled' (root-only option) captures without a workspace; the
        // server rejects it for scoped keys with an actionable error.
        if (f.workspace !== 'unfiled') body.workspace_id = f.workspace;
        const res = await this.authFetch('/api/inbox', { method: 'POST', body: JSON.stringify(body) });
        if (!await this.handleBad(res, 'Item not captured')) return;
        this.notify(f.workspace === 'unfiled' ? 'Item captured (unfiled)' : 'Item captured', 'success');
        // An item with a branch or reference is headed for the pipeline:
        // open its detail, where Queue and Review branch are. Rough
        // captures stay on the board.
        if (body.branch || body.ref) opened = (await res.json().catch(() => null)) as InboxItem | null;
      } else {
        const body: Record<string, unknown> = { title: f.title.trim(), content, tags, via: 'dashboard' };
        // Filing: a changed, non-empty workspace re-files the item.
        if (!f.wsLocked && f.workspace && f.workspace !== this.inboxBaseline.workspace) body.workspace_id = f.workspace;
        // Only a changed switch is sent ("" = back to the repository default).
        if (f.agents !== this.inboxBaseline.agents) body.agents = f.agents;
        if (f.branch.trim() !== this.inboxBaseline.branch) body.branch = f.branch.trim();
        if (f.ref.trim() !== this.inboxBaseline.ref) body.ref = f.ref.trim();
        const res = await this.authFetch('/api/inbox/' + encodeURIComponent(f.id), { method: 'PATCH', body: JSON.stringify(body) });
        if (!await this.handleBad(res, 'Item not updated')) return;
        this.notify('Item updated', 'success');
      }
      this.showInboxItemModal = false;
      inboxEditor = destroyEditor(inboxEditor);
      await this.fetchInbox();
      if (editing && this.expandedInboxId === f.id) await this.reloadInboxItem(f.id);
      if (opened && opened.id) await this.openInboxPreview(opened);
    } finally { this.inboxItemSaving = false; }
  },
  // Single-statement @change handler (the CSP evaluator is only proven on
  // plain method calls): the select resets itself via the passed element.
  // ---- file-into-workspace modal ----
  // Searchable picker replacing the inline `file into…` selects (cramped,
  // no search). Visibility is driven imperatively ([data-file-modal]):
  // a NEW x-show is one of the CSP build's silent failure modes. Row-level
  // x-shows that read only the row's own item keep working.
  //
  // Keyboard model = the ARIA combobox pattern (mirrors the header
  // workspace picker): focus stays in the search input; ArrowDown/ArrowUp
  // move the active option, Enter picks it, Escape closes, Tab cycles
  // input ⇄ Cancel (options are tabindex -1). Screen readers announce the
  // active option via aria-activedescendant.
  openFileModal(item: InboxItem) {
    if (!this.inboxItemEditable(item)) return; // frozen items cannot be re-filed
    this.fileReturnFocus = document.activeElement as HTMLElement | null;
    this.fileTargetItem = item;
    this.fileQuery = '';
    this.fileHighlightId = this.firstFileOptionId();
    this.showFileModal = true;
    this.$nextTick(() => {
      const el = document.querySelector('[data-file-modal]') as HTMLElement | null;
      if (el) el.style.display = '';
      const input = document.getElementById('file-ws-query') as HTMLInputElement | null;
      if (input) input.focus();
    });
  },
  closeFileModal() {
    this.showFileModal = false;
    this.fileTargetItem = null;
    this.fileQuery = '';
    this.fileHighlightId = '';
    const el = document.querySelector('[data-file-modal]') as HTMLElement | null;
    if (el) el.style.display = 'none';
    const back = this.fileReturnFocus;
    this.fileReturnFocus = null;
    if (back && document.contains(back)) back.focus();
  },
  fileOptionIds(): string[] {
    return this.fileOptions().filter((o: { id: string; empty?: boolean }) => !o.empty).map((o: { id: string }) => o.id);
  },
  firstFileOptionId(): string {
    const ids = this.fileOptionIds();
    return ids.length > 0 ? ids[0] : '';
  },
  fileQueryChanged() {
    if (!this.fileOptionIds().includes(this.fileHighlightId)) {
      this.fileHighlightId = this.firstFileOptionId();
    }
  },
  fileHighlightMove(delta: number) {
    const ids = this.fileOptionIds();
    if (ids.length === 0) return;
    const i = ids.indexOf(this.fileHighlightId);
    this.fileHighlightId = i === -1
      ? (delta > 0 ? ids[0] : ids[ids.length - 1])
      : ids[Math.min(ids.length - 1, Math.max(0, i + delta))];
    // Keep the active option inside the scrollable list.
    this.$nextTick(() => {
      const el = document.getElementById('file-opt-' + CSS.escape(this.fileHighlightId));
      if (el) el.scrollIntoView({ block: 'nearest' });
    });
  },
  filePickHighlighted() {
    if (!this.fileHighlightId) return;
    this.fileItemInto(this.fileHighlightId);
  },
  fileActiveDescendant(): string {
    return this.fileHighlightId ? 'file-opt-' + this.fileHighlightId : '';
  },
  // Tab cycles search input ⇄ Cancel (the only two tab stops; options are
  // tabindex -1 by design) so focus never escapes the modal.
  trapFileTab(ev: KeyboardEvent) {
    const input = document.getElementById('file-ws-query');
    const modal = document.querySelector('[data-file-modal]');
    const cancel = modal ? modal.querySelector<HTMLButtonElement>('.flex.justify-end button') : null;
    if (!input || !cancel) return;
    if (ev.shiftKey && document.activeElement === input) { ev.preventDefault(); cancel.focus(); }
    else if (!ev.shiftKey && document.activeElement === cancel) { ev.preventDefault(); input.focus(); }
  },
  // Sentinel row keeps the empty state inside x-for reactivity (x-show on a
  // new static element would never re-run in the CSP build).
  fileOptions(): { id: string; label: string; empty?: boolean }[] {
    const q = this.fileQuery.trim().toLowerCase();
    const opts = this.workspaceOptions().filter((o: { id: string; label: string }) =>
      !q || o.label.toLowerCase().includes(q) || o.id.toLowerCase().includes(q));
    if (opts.length === 0) return [{ id: '', label: 'No workspaces match.', empty: true }];
    return opts;
  },
  async fileItemInto(wsId: string) {
    const item = this.fileTargetItem;
    if (!item || !wsId) return;
    const res = await this.authFetch('/api/inbox/' + encodeURIComponent(item.id), { method: 'PATCH', body: JSON.stringify({ workspace_id: wsId }) });
    if (!await this.handleBad(res, 'Could not file item')) return;
    this.notify('Filed into ' + this.workspaceLabel(wsId), 'success');
    this.closeFileModal();
    await this.fetchInbox();
  },
  async claimInboxItem(id: string) {
    const res = await this.authFetch('/api/inbox/' + encodeURIComponent(id) + '/claim', { method: 'POST', body: JSON.stringify({ agent_id: 'ui-user' }) });
    if (!await this.handleBad(res, 'Claim failed')) return;
    this.notify('Item claimed', 'success'); await this.fetchInbox();
  },
  async releaseInboxItem(id: string) {
    const res = await this.authFetch('/api/inbox/' + encodeURIComponent(id) + '/claim', { method: 'POST', body: JSON.stringify({ agent_id: '' }) });
    if (!await this.handleBad(res, 'Release failed')) return;
    this.notify('Item released', 'success'); await this.fetchInbox();
  },
  async restoreInboxItem(id: string) {
    const res = await this.authFetch('/api/inbox/' + encodeURIComponent(id) + '/restore', { method: 'POST', body: JSON.stringify({}) });
    if (!await this.handleBad(res, 'Restore failed')) return;
    this.notify('Item restored', 'success');
    await this.fetchInbox();
  },
  async discardInboxItem(id: string) {
    const res = await this.authFetch('/api/inbox/' + encodeURIComponent(id) + '/discard', { method: 'POST', body: JSON.stringify({}) });
    if (!await this.handleBad(res, 'Discard failed')) return;
    this.notify('Item discarded', 'success');
    if (this.expandedInboxId === id) { this.expandedInboxId = ''; this.expandedInbox = null; }
    await this.fetchInbox();
  },
  // Convert = create a plan prefilled from the item, then link it. The plan
  // MUST land in the item's workspace (the server rejects cross-workspace
  // convert), so the pending state carries it explicitly — writeWorkspace()
  // would otherwise pick the active filter or an arbitrary first workspace
  // and leave a stray plan behind on failure.
  convertInboxItem(item: InboxItem) {
    if (!item.workspace_id) return; // unfiled: convert is impossible by construction
    this.pendingConvertItemId = item.id;
    this.pendingConvertWorkspaceId = item.workspace_id;
    this.planForm = { name: item.title, description: (item.excerpt || '').slice(0, 200), branch_name: '' };
    this.planErrors = {};
    this.showPlanModal = true;
  },

  // ---- code index ----
  async fetchWhoami() {
    try {
      const res = await this.authFetch('/api/whoami');
      if (res.ok) this.whoami = (await res.json()) ?? null;
    } catch { /* non-fatal */ }
    this.isRoot = this.whoamiIsRoot();
    this.isApprover = this.canApprove();
    this.syncRoles();
    if (this.whoami && !this.whoami.root && this.activeWorkspace) {
      const allowed = this.whoami.workspaces.some((w) => w.id === this.activeWorkspace);
      if (!allowed) {
        this.activeWorkspace = this.whoami.workspaces[0]?.id || '';
        localStorage.setItem('skopos:workspace', this.activeWorkspace);
      }
    }
  },

  // ---- api keys ----
  wsGroupsOf(id: string): KeyGroup[] { return this.groups.filter((g: KeyGroup) => (g.members || []).indexOf(id) >= 0); },
  openNewWorkspaceModal(selectAfter = false) {
    this.wsModalMode = 'create';
    this.wsSelectAfterCreate = selectAfter;
    this.formNonce++;
    this.wsForm = { id: '', name: '', git_url: '', groups: [] };
    this.wsErrors = {};
    this.showWsModal = true;
  },
  openEditWorkspaceModal(ws: WorkspaceRow) {
    this.wsModalMode = 'edit';
    this.wsSelectAfterCreate = false;
    this.formNonce++;
    this.wsForm = { id: ws.id, name: ws.name || '', git_url: ws.git_url || '', groups: this.wsGroupsOf(ws.id).map((g: KeyGroup) => g.name) };
    this.wsErrors = {};
    this.showWsModal = true;
  },
  closeWsModal() { this.showWsModal = false; },
  async saveWorkspace() {
    this.wsErrors = {};
    const id = this.wsForm.id.trim();
    if (!id) { this.wsErrors.id = 'Workspace ID is required.'; this.focusFirstError(); return; }
    const creating = this.wsModalMode === 'create';
    this.wsSaving = true;
    try {
      const res = await this.authFetch('/api/workspaces', {
        method: 'POST',
        body: JSON.stringify({ id, name: this.wsForm.name.trim(), git_url: this.wsForm.git_url.trim() }),
      });
      if (!res.ok) { this.wsErrors.id = await this.extractError(res); this.focusFirstError(); return; }
      // Group allocation is a separate call; skip it for a new workspace
      // with no groups picked (nothing to set).
      if (!creating || this.wsForm.groups.length) {
        const groupsRes = await this.authFetch('/api/workspaces/' + encodeURIComponent(id) + '/groups', {
          method: 'PUT',
          body: JSON.stringify({ groups: this.wsForm.groups }),
        });
        if (!groupsRes.ok) { this.wsErrors.groups = await this.extractError(groupsRes); return; }
      }
      this.showWsModal = false;
      this.notify(creating ? 'Workspace created' : 'Workspace saved', 'success');
      await this.fetchWorkspaces();
      await this.fetchGroups();
      if (creating && this.wsSelectAfterCreate) this.setWorkspace(id);
    } finally { this.wsSaving = false; }
  },
  openNewGroupModal() {
    this.groupModalMode = 'create';
    this.formNonce++;
    this.groupForm = { id: '', name: '', description: '', patterns: '', members: [] };
    this.groupErrors = {};
    this.showGroupModal = true;
  },
  openEditGroupModal(g: KeyGroup) {
    this.groupModalMode = 'edit';
    this.formNonce++;
    this.groupForm = { id: g.id, name: g.name, description: g.description || '', patterns: (g.patterns || []).join('\n'), members: [...(g.members || [])] };
    this.groupErrors = {};
    this.showGroupModal = true;
  },
  closeGroupModal() { this.showGroupModal = false; },
  async saveGroup() {
    this.groupErrors = {};
    if (!this.groupForm.name.trim()) { this.groupErrors.name = 'Name is required.'; this.focusFirstError(); return; }
    this.groupSaving = true;
    try {
      const patterns = this.groupForm.patterns.split('\n').map((p: string) => p.trim()).filter(Boolean);
      const body = JSON.stringify({ name: this.groupForm.name.trim(), description: this.groupForm.description.trim(), patterns, members: this.groupForm.members });
      const res = this.groupModalMode === 'create'
        ? await this.authFetch('/api/groups', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body })
        : await this.authFetch('/api/groups/' + encodeURIComponent(this.groupForm.id), { method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body });
      if (!res.ok) {
        if (res.status === 409) this.groupErrors.name = 'A group with that name already exists';
        else if (res.status === 403) this.groupErrors.form = 'Group management requires the root key';
        else this.groupErrors.form = (await res.json().catch(() => ({ error: 'Saving the group failed' }))).error;
        return;
      }
      this.showGroupModal = false;
      // Members may have changed: refresh both sides so badges stay honest.
      await this.fetchGroups();
      await this.fetchWorkspaces();
    } catch { this.groupErrors.form = 'Network error'; } finally { this.groupSaving = false; }
  },
  async fetchGroups() {
    this.groupsLoading = true;
    try {
      const res = await this.authFetch('/api/groups');
      if (!res.ok) {
        this.groups = [];
        if (res.status === 403) this.notify('Groups require the root key', 'error');
        return;
      }
      this.groups = (await res.json()) ?? [];
    } catch {
      this.groups = [];
    } finally {
      this.groupsLoading = false;
    }
  },
  // Key reach, computed client-side from the root-only keys list: exact
  // scope, group membership, or a group pattern match — the same three ways
  // the server resolves a key's scope.
  wsKeys(id: string): ApiKey[] {
    return this.keys.filter((k: ApiKey) => !k.revoked_at && (k.all_workspaces
      || (k.workspaces || []).indexOf(id) >= 0
      || (k.groups || []).some((gn: string) => {
        const g = this.groups.find((x: KeyGroup) => x.name === gn);
        return !!g && ((g.members || []).indexOf(id) >= 0 || (g.patterns || []).some((p: string) => this.patternMatches(p, id)));
      })));
  },
  groupKeys(g: KeyGroup): ApiKey[] {
    return this.keys.filter((k: ApiKey) => !k.revoked_at && (k.groups || []).indexOf(g.name) >= 0);
  },
  // path.Match semantics: * matches exactly one segment, never crosses /.
  patternMatches(pattern: string, id: string): boolean {
    const ps = pattern.split('/');
    const is = id.split('/');
    if (ps.length !== is.length) return false;
    return ps.every((seg: string, i: number) => {
      if (seg === '*') return true;
      const rx = new RegExp('^' + seg.replace(/[.+^${}()|[\]\\]/g, '\\$&').replace(/\*/g, '[^/]*') + '$');
      return rx.test(is[i]);
    });
  },
  async fetchKeys() {
    this.keysLoading = true;
    try {
      const res = await this.authFetch('/api/keys');
      if (!res.ok) {
        this.keys = [];
        if (res.status === 403) this.notify('Key management requires the root key', 'error');
        return;
      }
      this.keys = (await res.json()) ?? [];
    } catch {
      this.keys = [];
    } finally {
      this.keysLoading = false;
    }
  },
  keyScope(k: ApiKey): string {
    if (k.all_workspaces) return '* (all workspaces)';
    const parts: string[] = [...(k.workspaces || [])];
    for (const gn of k.groups || []) parts.push('@' + gn);
    return parts.join(', ') || 'no scope';
  },
  keyIsApprover(k: ApiKey): boolean { return !!k.approver; },
  keyIsDelegate(k: ApiKey): boolean { return !!k.delegate; },
  keyIsAdmin(k: ApiKey): boolean { return !!k.admin; },
  // keyCapsLabel names a capped key's limits ("" = full access, no chip).
  keyCapsLabel(k: ApiKey): string {
    const caps = k.capabilities;
    if (!caps) return '';
    if (keyIsAgentPreset(caps)) return 'agent · MCP only';
    return caps.length ? caps.join(', ') : 'no access';
  },
  openNewKeyModal() {
    this.keyModalMode = 'create';
    this.keyForm = { id: '', name: '', all: false, workspaces: [], groups: [], access: 'full', accessWas: 'full', custom: '' };
    this.keyErrors = {};
    this.formNonce++;
    this.showApiKeyModal = true;
  },
  openEditKeyModal(k: ApiKey) {
    this.keyModalMode = 'edit';
    const caps = k.capabilities;
    const access = !caps ? 'full' : (keyIsAgentPreset(caps) ? 'agent' : 'keep');
    this.keyForm = { id: k.id, name: k.name, all: k.all_workspaces, workspaces: [...(k.workspaces || [])], groups: [...(k.groups || [])], access, accessWas: access, custom: access === 'keep' ? (caps || []).join(', ') : '' };
    this.keyErrors = {};
    this.formNonce++;
    this.showApiKeyModal = true;
  },
  closeApiKeyModal() { this.showApiKeyModal = false; },
  async submitApiKey() {
    const f = this.keyForm;
    const errs: Record<string, string> = {};
    if (!f.name.trim()) errs.name = 'Name is required.';
    if (!f.all && f.workspaces.length === 0 && f.groups.length === 0) errs.workspaces = 'Select at least one workspace or group, or all workspaces.';
    this.keyErrors = errs;
    if (Object.keys(errs).length) { this.focusFirstError(); return; }
    const creating = this.keyModalMode === 'create';
    this.keySaving = true;
    try {
      const body: Record<string, unknown> = { name: f.name.trim(), workspaces: f.all ? ['*'] : f.workspaces };
      // create: groups only when picked; edit: [] clears, absent = unchanged
      if (!f.all && (!creating || f.groups.length)) body.groups = f.groups;
      // Capabilities: the agent preset, or (edit) back to full access; a
      // custom set is left alone ('keep').
      // Capabilities: sent only when the access choice changed (an edit that
      // sends them re-shapes the key and drops its live streams).
      if (creating) { if (f.access === 'agent') body.preset = 'agent'; }
      else if (f.access !== f.accessWas) {
        if (f.access === 'agent') { body.preset = 'agent'; body.capabilities = []; }
        else if (f.access === 'full') body.capabilities = [];
      }
      const res = creating
        ? await this.authFetch('/api/keys', { method: 'POST', body: JSON.stringify(body) })
        : await this.authFetch('/api/keys/' + encodeURIComponent(f.id), { method: 'PATCH', body: JSON.stringify(body) });
      if (!await this.handleBad(res, creating ? 'Key not created' : 'Key not updated')) return;
      this.showApiKeyModal = false;
      if (creating) {
        const data = await res.json();
        this.showSecret('key', 'API key created', data.key_secret, data.key.name);
      } else {
        this.notify('Key updated', 'success');
      }
      await this.fetchKeys();
    } finally {
      this.keySaving = false;
    }
  },
  showSecret(kind: 'key' | 'webhook', title: string, secret: string, name: string) {
    this.secretKind = kind; this.secretTitle = title;
    this.newSecret = secret; this.newSecretName = name;
    this.secretCopied = false;
    this.showSecretModal = true;
  },
  closeSecretModal() { this.newSecret = ''; this.newSecretName = ''; this.secretCopied = false; this.showSecretModal = false; },
  async copySecret() {
    try {
      await navigator.clipboard.writeText(this.newSecret);
      this.secretCopied = true;
      this.notify('Secret copied to clipboard', 'success');
      window.setTimeout(() => { this.secretCopied = false; }, 2500);
    } catch {
      this.notify('Copy failed — select the secret and copy manually', 'error');
    }
  },
  revokeKey(k: ApiKey) {
    this.requestDelete('apikey', k.id, k.name);
  },
  deleteKey(k: ApiKey) {
    this.requestDelete('apikeyhard', k.id, k.name);
  },

  // ---- webhooks (tmp/integrations — design.md §3) ----
  async fetchWebhooks() {
    this.webhooksLoading = true;
    try {
      const res = await this.authFetch('/api/webhooks');
      if (!res.ok) {
        this.webhooks = [];
        if (res.status === 403) this.notify('Webhooks need the root key or an approver key', 'error');
        return;
      }
      this.webhooks = (await res.json()) ?? [];
    } catch {
      this.webhooks = [];
    } finally {
      this.webhooksLoading = false;
    }
  },
  webhookTypeOptions(): string[] { return WEBHOOK_TYPES; },
  webhookScope(w: Webhook): string {
    const parts = [...(w.workspaces || [])];
    for (const id of w.groups || []) parts.push('@' + (this.groups.find((g: KeyGroup) => g.id === id)?.name || id));
    return parts.join(', ');
  },
  webhookTypesLabel(w: Webhook): string {
    return w.types && w.types.length ? w.types.join(', ') : 'every event but run.started and run.finished';
  },
  webhookStateLabel(w: Webhook): string {
    if (!w.enabled) return 'disabled';
    return w.failing_since ? 'failing' : 'enabled';
  },
  webhookStateClass(w: Webhook): string {
    return this.toneOf('status', !w.enabled ? 'failed' : (w.failing_since ? 'paused' : 'succeeded'));
  },
  webhookNote(w: Webhook): string {
    if (!w.enabled) {
      const r = w.disabled_reason || '';
      const why = !r ? 'Disabled' : (r.startsWith('disabled by ') ? 'D' + r.slice(1) : 'Disabled: ' + r);
      return why + '. Its events wait until it is enabled.';
    }
    return w.failing_since ? 'Deliveries failing since ' + this.formatDateTime(w.failing_since) + '; 3 days of failures disable it.' : '';
  },
  openNewWebhookModal() {
    this.hookModalMode = 'create';
    const ws = this.activeWorkspace ? [this.activeWorkspace] : [];
    this.hookForm = { id: '', name: '', url: '', workspaces: ws, groups: [], types: [] };
    this.hookErrors = {};
    this.formNonce++;
    this.showWebhookModal = true;
  },
  openEditWebhookModal(w: Webhook) {
    this.hookModalMode = 'edit';
    const groupNames = (w.groups || []).map((id: string) => this.groups.find((g: KeyGroup) => g.id === id)?.name || id);
    this.hookForm = { id: w.id, name: w.name, url: w.url, workspaces: [...(w.workspaces || [])], groups: groupNames, types: [...(w.types || [])] };
    this.hookErrors = {};
    this.formNonce++;
    this.showWebhookModal = true;
  },
  closeWebhookModal() { this.showWebhookModal = false; },
  async submitWebhook() {
    const f = this.hookForm;
    const errs: Record<string, string> = {};
    if (!f.name.trim()) errs.name = 'Name is required.';
    if (!/^https?:\/\/\S+$/i.test(f.url.trim())) errs.url = 'An https URL (http only to hosts the server allows).';
    if (f.workspaces.length === 0 && f.groups.length === 0) errs.workspaces = 'Pick the workspaces whose events it gets.';
    this.hookErrors = errs;
    if (Object.keys(errs).length) { this.focusFirstError(); return; }
    const creating = this.hookModalMode === 'create';
    const body: Record<string, unknown> = { name: f.name.trim(), url: f.url.trim(), workspaces: f.workspaces, types: f.types };
    if (this.whoamiIsRoot()) body.groups = f.groups;
    this.hookSaving = true;
    try {
      const res = creating
        ? await this.authFetch('/api/webhooks', { method: 'POST', body: JSON.stringify(body) })
        : await this.authFetch('/api/webhooks/' + encodeURIComponent(f.id), { method: 'PATCH', body: JSON.stringify(body) });
      if (!await this.handleBad(res, creating ? 'Webhook not created' : 'Webhook not saved')) return;
      this.showWebhookModal = false;
      if (creating) {
        const data = await res.json();
        this.showSecret('webhook', 'Webhook created', data.secret, data.webhook.name);
      } else {
        this.notify('Webhook saved', 'success');
      }
      await this.fetchWebhooks();
    } finally {
      this.hookSaving = false;
    }
  },
  async hookAction(w: Webhook, fn: () => Promise<void>) {
    if (this.hookBusy) return;
    this.hookBusy = w.id;
    try { await fn(); } finally { this.hookBusy = ''; }
  },
  setWebhookEnabled(w: Webhook, enabled: boolean) {
    return this.hookAction(w, async () => {
      const res = await this.authFetch('/api/webhooks/' + encodeURIComponent(w.id), { method: 'PATCH', body: JSON.stringify({ enabled }) });
      if (!await this.handleBad(res, enabled ? 'Not enabled' : 'Not disabled')) return;
      this.notify(enabled ? 'Enabled: what waited is delivered now' : 'Disabled: its events wait', 'success');
      await this.fetchWebhooks();
    });
  },
  // Rotating starts a 24-hour deadline for the receiver: confirm first.
  requestRotateWebhook(w: Webhook) {
    this.confirm = {
      open: true, title: 'Rotate the signing secret?', label: 'Rotate', busy: false,
      message: `“${w.name}” gets a new signing secret. For 24 hours deliveries carry both signatures; the receiver must switch to the new secret before then.`,
      pending: { kind: 'webhookrotate', id: w.id },
    };
  },
  rotateWebhook(w: Webhook) {
    return this.hookAction(w, async () => {
      const res = await this.authFetch('/api/webhooks/' + encodeURIComponent(w.id) + '/rotate', { method: 'POST' });
      if (!await this.handleBad(res, 'Secret not rotated')) return;
      const data = await res.json();
      this.showSecret('webhook', 'New signing secret', data.secret, data.webhook.name);
    });
  },
  testWebhook(w: Webhook) {
    return this.hookAction(w, async () => {
      const res = await this.authFetch('/api/webhooks/' + encodeURIComponent(w.id) + '/test', { method: 'POST' });
      if (!await this.handleBad(res, 'Test not sent')) return;
      this.notify('Test event queued', 'success');
      this.hookOpen = w.id;
      await new Promise((r) => window.setTimeout(r, 2500));
      if (this.hookOpen === w.id) await this.fetchWebhookDeliveries(w.id);
    });
  },
  deleteWebhook(w: Webhook) { this.requestDelete('webhook', w.id, w.name); },
  async toggleWebhookDeliveries(w: Webhook) {
    if (this.hookOpen === w.id) { this.hookOpen = ''; return; }
    this.hookOpen = w.id;
    this.hookDeliveries = [];
    await this.fetchWebhookDeliveries(w.id);
  },
  async fetchWebhookDeliveries(id: string) {
    this.hookDeliveriesLoading = true;
    try {
      const res = await this.authFetch('/api/webhooks/' + encodeURIComponent(id) + '/deliveries?limit=30');
      if (this.hookOpen !== id) return;
      this.hookDeliveries = res.ok ? ((await res.json()) ?? []) : [];
    } catch {
      this.hookDeliveries = [];
    } finally {
      this.hookDeliveriesLoading = false;
    }
  },
  deliveryStateClass(d: WebhookDelivery): string {
    return this.toneOf('status', d.state === 'delivered' ? 'succeeded' : d.state === 'failed' ? 'failed' : 'queued');
  },
  deliveryDetail(d: WebhookDelivery): string {
    const parts = [d.attempts === 1 ? '1 attempt' : d.attempts + ' attempts'];
    if (d.last_code) parts.push('HTTP ' + d.last_code);
    if (d.last_error && !/^HTTP \d+$/.test(d.last_error)) parts.push(d.last_error);
    if (d.state === 'pending' && d.next_at) parts.push('next try ' + this.formatDateTime(d.next_at));
    return parts.join(' · ');
  },
  redeliverWebhook(w: Webhook, d: WebhookDelivery) {
    return this.hookAction(w, async () => {
      const res = await this.authFetch('/api/webhooks/' + encodeURIComponent(w.id) + '/redeliver', { method: 'POST', body: JSON.stringify({ delivery_id: d.id }) });
      if (!await this.handleBad(res, 'Not sent again')) return;
      this.notify('Queued again', 'success');
      await new Promise((r) => window.setTimeout(r, 2500));
      if (this.hookOpen === w.id) await this.fetchWebhookDeliveries(w.id);
    });
  },

  async fetchIndexStatus() {
    // No workspace selected: list every workspace's indexes grouped, so the
    // view is useful without picking first (and the literal "default" id —
    // which matches nothing — is never queried).
    if (!this.activeWorkspace) {
      const ids = this.workspaceOptions().map((o: { id: string }) => o.id);
      if (ids.length === 0) { this.indexBranches = []; this.indexGroups = []; return; }
      const groups: { id: string; label: string; branches: typeof this.indexBranches }[] = [];
      await Promise.all(ids.map(async (id: string) => {
        try {
          const res = await this.authFetch(`/api/codeindex/${encodeURIComponent(id)}/status`);
          const branches = res.ok ? ((await res.json()) ?? []) : [];
          groups.push({ id, label: this.workspaceOptions().find((o: { id: string }) => o.id === id)?.label || id, branches });
        } catch { /* per-workspace failure is non-fatal */ }
      }));
      groups.sort((a, b) => a.id.localeCompare(b.id));
      this.indexGroups = groups;
      this.indexBranches = groups.flatMap((g) => g.branches);
      return;
    }
    this.indexGroups = [];
    try {
      const res = await this.authFetch(`/api/codeindex/${encodeURIComponent(this.activeWorkspace)}/status`);
      if (!res.ok) { this.indexBranches = []; return; }
      this.indexBranches = (await res.json()) ?? [];
    } catch { this.indexBranches = []; }
  },

  // ---- delete (modal-driven) ----
  requestDelete(kind: string, id: string, name: string) {
    this.confirm = { open: true, title: `Delete ${kind}`, message: `Delete “${name}”? This cannot be undone.`, label: 'Delete', busy: false, pending: { kind, id } };
  },
  // ---- bulk purge (same confirm dialog as deletes) ----
  // No workspace selected = purge EVERYTHING, which the server gates on the
  // root key; the button itself only shows for root in that state
  // (syncSessionPurge — a new x-show on a static element is one of the
  // silent CSP failure modes, so visibility is driven imperatively).
  requestSessionPurge() {
    const scope = this.activeWorkspace ? ` in ${this.workspaceLabel(this.activeWorkspace)}` : ' across every workspace';
    this.confirm = {
      open: true, title: 'Delete all sessions',
      message: `Delete all ${this.sessions.length} sessions${scope}? Their events, agent states, and session-scoped blackboard entries are deleted with them. This cannot be undone.`,
      label: 'Delete all', busy: false,
      pending: { kind: 'sessionpurge', id: this.activeWorkspace },
    };
  },
  // Entry purge is one workspace + one type (the API requires both); the
  // per-group button only renders when a workspace is selected.
  requestEntryPurge(type: string, label: string) {
    if (!this.activeWorkspace) return; // belt-and-braces: no purge without a target
    this.confirm = {
      open: true, title: `Delete all ${label.toLowerCase()} entries`,
      message: `Delete every ${label.toLowerCase()} entry in ${this.workspaceLabel(this.activeWorkspace)} — all scopes and branches? This cannot be undone.`,
      label: 'Delete all', busy: false,
      pending: { kind: 'entrypurge', id: this.activeWorkspace + '|' + type },
    };
  },
  entryPurgeTitle(group: { type: string; label: string }): string {
    return `Delete every ${group.label.toLowerCase()} entry in ${this.workspaceLabel(this.activeWorkspace)} — all scopes and branches`;
  },
  // Per-lane clear on the board: one workspace + one status (the API allows
  // omitting status for a full wipe — the CLI covers that shape). The count
  // is read live at request time, so it reflects the current lane even
  // though the header button's own visibility only tracks the workspace.
  requestInboxPurge(status: string, label: string) {
    if (!this.activeWorkspace) return; // purge needs an explicit workspace
    const n = this.laneItems(status).length;
    this.confirm = {
      open: true, title: `Delete all ${label.toLowerCase()} items`,
      message: `Permanently delete all ${n} ${label.toLowerCase()} item${n === 1 ? '' : 's'} in ${this.workspaceLabel(this.activeWorkspace)}? This cannot be undone.`,
      label: 'Delete all', busy: false,
      pending: { kind: 'inboxpurge', id: this.activeWorkspace + '|' + status },
    };
  },
  // Role-gated page elements (data-role="root" | "approver" | "admin"), shown
  // imperatively like syncSessionPurge: x-show applies a change in the next
  // animation frame, and where frames don't run (a background or embedded
  // view) the Keys item stayed hidden after whoami said root until a reload.
  syncRoles() {
    document.querySelectorAll('[data-role]').forEach((el) => {
      const role = (el as HTMLElement).dataset.role;
      const show = role === 'root' ? this.isRoot : role === 'approver' ? this.isApprover : role === 'admin' ? this.canAdmin() : false;
      (el as HTMLElement).style.display = show ? '' : 'none';
    });
  },
  syncSessionPurge() {
    const btn = document.querySelector('[data-session-purge]') as HTMLElement | null;
    if (!btn) return;
    const show = this.sessions.length > 0 && (!!this.activeWorkspace || this.whoamiIsRoot());
    btn.style.display = show ? '' : 'none';
  },
  cancelDelete() { if (!this.confirm.busy) this.confirm = { open: false, title: '', message: '', label: 'Delete', busy: false, pending: null }; },
  async confirmDelete() {
    const p = this.confirm.pending; if (!p) return;
    this.confirm.busy = true;
    if (p.kind === 'inboxreject') {
      this.confirm.busy = false;
      this.confirm.open = false; this.confirm.pending = null;
      const notes = this.workflowNote.trim();
      await this.postWorkflowAction(p.id, 'reject', notes ? { notes } : null, 'Rejected');
      return;
    }
    if (p.kind === 'webhookrotate') {
      this.confirm.busy = false;
      this.confirm.open = false; this.confirm.pending = null;
      const w = this.webhooks.find((x: Webhook) => x.id === p.id);
      if (w) await this.rotateWebhook(w);
      return;
    }
    // Discarding unsaved inbox edits closes the editor; nothing to delete.
    if (p.kind === 'inboxdiscard') {
      this.confirm.busy = false;
      this.confirm.open = false; this.confirm.pending = null;
      this.closeInboxItemModal();
      return;
    }
    if (p.kind === 'sessionpurge' || p.kind === 'entrypurge' || p.kind === 'inboxpurge') {
      let url: string;
      if (p.kind === 'sessionpurge') {
        url = p.id ? '/api/sessions?workspace_id=' + encodeURIComponent(p.id) : '/api/sessions';
      } else if (p.kind === 'inboxpurge') {
        const [ws, status] = p.id.split('|');
        url = '/api/inbox?workspace_id=' + encodeURIComponent(ws) + '&status=' + encodeURIComponent(status);
      } else {
        const [ws, type] = p.id.split('|');
        url = '/api/blackboard/entries?workspace_id=' + encodeURIComponent(ws) + '&entry_type=' + encodeURIComponent(type);
      }
      const res = await this.authFetch(url, { method: 'DELETE' });
      this.confirm.busy = false;
      if (!await this.handleBad(res, 'Purge failed')) return;
      let n = -1;
      try { n = ((await res.json()) ?? { deleted: -1 }).deleted; } catch { /* non-JSON body */ }
      this.notify(n >= 0 ? `Deleted ${n}` : 'Deleted', 'success');
      this.confirm.open = false; this.confirm.pending = null;
      if (p.kind === 'sessionpurge') {
        this.selectedSessionId = ''; this.selectedSession = null; localStorage.removeItem('skopos:session');
        await this.refresh();
      } else if (p.kind === 'inboxpurge') {
        if (this.expandedInboxId) { this.expandedInboxId = ''; this.expandedInbox = null; }
        await this.fetchInbox();
      } else {
        await this.fetchBundle();
      }
      return;
    }
    let url = '';
    if (p.kind === 'session') url = `/api/sessions/${encodeURIComponent(p.id)}`;
    else if (p.kind === 'entry') url = `/api/blackboard/entries/${encodeURIComponent(p.id)}`;
    else if (p.kind === 'plan') url = `/api/plans/${encodeURIComponent(p.id)}`;
    else if (p.kind === 'item') {
      const [planId, itemId] = p.id.split('|');
      url = `/api/plans/${encodeURIComponent(planId)}/items/${encodeURIComponent(itemId)}`;
    }
    else if (p.kind === 'apikey') url = `/api/keys/${encodeURIComponent(p.id)}`;
    else if (p.kind === 'apikeyhard') url = `/api/keys/${encodeURIComponent(p.id)}?hard=true`;
    else if (p.kind === 'inboxitem') url = `/api/inbox/${encodeURIComponent(p.id)}`;
    else if (p.kind === 'webhook') url = `/api/webhooks/${encodeURIComponent(p.id)}`;
    const res = await this.authFetch(url, { method: 'DELETE' });
    this.confirm.busy = false;
    if (!await this.handleBad(res, 'Delete failed')) return;
    this.notify('Deleted', 'success');
    this.confirm.open = false; this.confirm.pending = null;
    if (p.kind === 'session') {
      if (this.selectedSessionId === p.id) { this.selectedSessionId = ''; this.selectedSession = null; localStorage.removeItem('skopos:session'); }
      await this.refresh();
    } else if (p.kind === 'entry') await this.fetchBundle();
    else if (p.kind === 'plan') { if (this.expandedPlan?.id === p.id) this.expandedPlan = null; await this.fetchPlans(); }
    else if (p.kind === 'inboxitem') { if (this.expandedInboxId === p.id) { this.expandedInboxId = ''; this.expandedInbox = null; } await this.fetchInbox(); }
    else if (p.kind === 'item' && this.expandedPlan) await this.reloadPlan(this.expandedPlan.id);
    else if (p.kind === 'apikey') await this.fetchKeys();
    else if (p.kind === 'webhook') { if (this.hookOpen === p.id) this.hookOpen = ''; await this.fetchWebhooks(); }
  },

  // shared bad-response handler: returns true when ok, false (and notifies) when not
  async handleBad(res: Response, prefix: string): Promise<boolean> {
    if (res.ok) return true;
    const msg = await this.extractError(res);
    this.notify(`${prefix}: ${msg}`, 'error');
    if (res.status === 401) this.openKeyModal();
    return false;
  },

  // ---- formatting ----
  depLabel(item: PlanItem) {
    if (!item.depends_on?.length) return '';
    const items = this.expandedPlan?.items ?? [];
    return item.depends_on.map(d => { const dep = items.find(i => i.id === d); return dep ? `#${dep.position}` : '…'; }).join(', ');
  },
  itemsGroupedByPhase(items: PlanItem[]) {
    if (!items?.length) return [];
    const groups = new Map<string, PlanItem[]>(); const order: string[] = [];
    for (const it of items) { const ph = it.phase || ''; if (!groups.has(ph)) { groups.set(ph, []); order.push(ph); } groups.get(ph)!.push(it); }
    return order.map(ph => ({ phase: ph, label: ph || 'Items', items: groups.get(ph)! }));
  },
  planDepNames(plan: Plan) {
    if (!plan.depends_on?.length) return '';
    return plan.depends_on.map(d => { const dep = this.plans.find(p => p.id === d); return dep ? dep.name : d.slice(0, 8) + '…'; }).join(', ');
  },
  formatTime(v: string) { if (!v) return ''; return new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit' }).format(new Date(v)); },

  isAutoEvent(e: Event): boolean {
    const m = (e.metadata || {}) as Record<string, unknown>;
    return m.source === 'hook' || m.source === 'server';
  },
  isHeartbeat(e: Event): boolean {
    const m = (e.metadata || {}) as Record<string, unknown>;
    return m.heartbeat === true;
  },
  // timeAgo: "just now", "4m ago", "2h ago", "3d ago".
  timeAgo(iso: string): string {
    if (!iso) return '';
    const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
    if (s < 60) return 'just now';
    if (s < 3600) return Math.floor(s / 60) + 'm ago';
    if (s < 86400) return Math.floor(s / 3600) + 'h ago';
    return Math.floor(s / 86400) + 'd ago';
  },
  // durationBetween: "42s", "6m", "1h 23m", "2d 4h".
  durationBetween(startIso: string, endIso: string): string {
    if (!startIso || !endIso) return '';
    let s = Math.max(0, (new Date(endIso).getTime() - new Date(startIso).getTime()) / 1000);
    const d = Math.floor(s / 86400); s -= d * 86400;
    const h = Math.floor(s / 3600); s -= h * 3600;
    const m = Math.floor(s / 60);
    if (d > 0) return d + 'd ' + h + 'h';
    if (h > 0) return h + 'h ' + m + 'm';
    if (m > 0) return m + 'm';
    return Math.floor(s) + 's';
  },
  sessionDuration(sess: SessionSummary | null): string {
    if (!sess) return '';
    return this.durationBetween(sess.started_at, sess.updated_at);
  },
  // sessionTimeline returns display-ready rows in chronological order:
  // consecutive heartbeat pings collapse into one compact row (isGroup),
  // real events carry flat precomputed fields (the CSP template only reads
  // scalars — never item.event.* — so no binding can throw on a row kind it
  // wasn't meant for), and gaps >5 minutes get a "silent for" marker.
  sessionTimeline(): Array<Record<string, unknown>> {
    const evs = [...(this.selectedSession?.events || [])].sort((a, b) => a.created_at.localeCompare(b.created_at));
    const out: Array<Record<string, unknown>> = [];
    let prevTs = 0;
    let i = 0;
    const blank = { agent: '', statusLabel: '', statusCls: '', time: '', auto: false, stepLabel: '', progress: -1, message: '' };
    while (i < evs.length) {
      const e = evs[i];
      if (this.isHeartbeat(e)) {
        let j = i;
        while (j < evs.length && this.isHeartbeat(evs[j])) j++;
        const group = evs.slice(i, j);
        out.push({
          key: 'hb-' + e.id, isGroup: true,
          countText: group.length + ' auto heartbeats',
          aliveText: 'alive ' + this.durationBetween(group[0].created_at, group[group.length - 1].created_at),
          rangeText: this.formatTime(group[0].created_at) + ' – ' + this.formatTime(group[group.length - 1].created_at),
          gap: '',
          ...blank,
        });
        prevTs = new Date(group[group.length - 1].created_at).getTime();
        i = j;
        continue;
      }
      const ts = new Date(e.created_at).getTime();
      const gapMs = ts - prevTs;
      out.push({
        key: e.id, isGroup: false,
        gap: prevTs > 0 && gapMs > 5 * 60 * 1000 ? this.durationBetween(new Date(prevTs).toISOString(), e.created_at) : '',
        agent: e.agent_id,
        statusLabel: e.status,
        statusCls: this.toneOf('status', e.status),
        time: this.formatTime(e.created_at),
        auto: this.isAutoEvent(e),
        stepLabel: e.step_total ? 'step ' + (e.step_current || 0) + '/' + e.step_total : '',
        progress: e.progress == null ? -1 : e.progress,
        message: e.message,
      });
      prevTs = ts;
      i++;
    }
    return out;
  },
});

// workspaceTail drops a leading host-like segment (one with a dot:
// github.com/, gitea.example.com:2223/) from a workspace id.
function workspaceTail(id: string): string {
  const slash = id.indexOf('/');
  return slash > 0 && id.slice(0, slash).includes('.') ? id.slice(slash + 1) : id;
}
function wsParam(ws: string) { const p = new URLSearchParams(); if (ws) p.set('workspace', ws); return p.size ? '?' + p.toString() : ''; }
function bundleParams(ws: string, branch: string) {
  const p = new URLSearchParams();
  if (ws) p.set('workspace', ws);
  if (branch.trim()) p.set('branch', branch.trim());
  return p.size ? '?' + p.toString() : '';
}
function emptyEntryForm(branch = ''): EntryForm { return { scope: 'branch', entry_type: 'finding', title: '', content: '', code_ref: '', branch_name: branch, session_id: '' }; }
function emptyPlanForm(branch = ''): PlanForm { return { name: '', description: '', branch_name: branch }; }
function emptyItemForm(): ItemForm { return { title: '', description: '', phase: '', depends_on: '' }; }
function emptyInboxForm(): InboxForm { return { id: '', title: '', tags: '', workspace: '', agents: '', branch: '', ref: '', wsLocked: false }; }
function emptyInboxBaseline() { return { title: '', tags: '', content: '', workspace: '', agents: '', branch: '', ref: '' }; }

Alpine.plugin(focus);
// Register as a named component: the CSP Alpine build cannot resolve globals
// from expressions, so x-data="app()" (window.app) would evaluate empty —
// x-data="app" resolves through this registry instead.
Alpine.data('app', appState);
window.Alpine = Alpine;
window.app = appState; // exposed for tests/debugging
Alpine.start();
