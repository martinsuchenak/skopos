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
type ApiKey = { id: string; name: string; key_prefix: string; all_workspaces: boolean; approver?: boolean; capabilities?: string[] | null; workspaces: string[]; groups?: string[]; created_at: string; last_used_at?: string; revoked_at?: string };
type KeyGroup = { id: string; name: string; description?: string; members: string[]; patterns: string[]; created_at: string };
type KeyReach = { key: ApiKey; via: string };
type WorkspaceRow = { id: string; name?: string; git_url?: string; created_at: string };
type Whoami = { root: boolean; key?: { id: string; name: string; all_workspaces: boolean; approver?: boolean; workspaces: string[] }; workspaces: { id: string; name: string }[] };
type KeyForm = { name: string; all: boolean; workspaces: string[] };
type EntryForm = { scope: 'project' | 'branch' | 'session'; entry_type: 'finding' | 'decision' | 'bug' | 'debt' | 'warning' | 'context'; title: string; content: string; code_ref: string; branch_name: string; session_id: string };
type PlanForm = { name: string; description: string; branch_name: string };
type ItemForm = { title: string; description: string; phase: string; depends_on: string };
type InboxPlanSummary = { id: string; name: string; status: string };
type InboxItem = { id: string; workspace_id?: string; title: string; tags: string[]; status: string; priority?: number; claimed_by_agent_id?: string; author_agent_id: string; plan_id?: string; plan?: InboxPlanSummary; agents?: string; branch?: string; ref?: string; flow?: string; created_at: string; updated_at: string; excerpt?: string };
type InboxDetail = InboxItem & { content: string; content_html: string };
// The agent preset (internal/auth Presets["agent"], normalised).
const agentPreset = ['blackboard:write', 'code:read', 'inbox:read', 'mcp', 'plans:write', 'status:read'];
function keyIsAgentPreset(caps: string[]): boolean {
  return caps.length === agentPreset.length && [...caps].sort().every((c, i) => c === agentPreset[i]);
}

type WorkerRow = { id: string; workspaces: string[]; stages: string[]; runners: string[]; active: number; succeeded: number; failed: number; lastActive: string; currentLabel: string; state: string; version: string; lastSeen: string; unhealthy: string[]; indexable: string[] };
type WorkerSeen = { id: string; runners: string[]; workspaces: string[]; last_seen: string; version?: string; state?: string; unhealthy?: Record<string, string>; indexable?: string[] };
// Pipeline settings (which agents run each stage, per workspace or group).
type PipeMember = { runner: string; focus?: string };
type PipeStage = { members: PipeMember[]; policy?: string; early_exit?: boolean; timeout_ms?: number };
type PipeView = { owner_kind: string; owner_id: string; indexable_by?: string[]; own: { stages: Record<string, PipeStage> | null; agents_default?: string; second_opinion_runner?: string; code_index?: boolean }; effective?: Record<string, PipeStage & { source: string }>; effective_agents_default?: string; agents_default_source?: string; effective_second_opinion_runner?: string; second_opinion_source?: string; groups?: { id: string; name: string }[]; conflicts?: string[]; unknown_runners?: string[] };
// One dialog row per stage. mode: own (set at this level, editable),
// inherited (from a group; Override copies it here), unset.
type PipeRow = { stage: string; label: string; multi: boolean; rule: boolean; mode: string; members: PipeMember[]; policy: string; earlyExit: boolean; timeoutMin: string; inherited: PipeStage | null; inheritedFrom: string; addRunner: string; addFocus: string };

type PlanRevisionInfo = { id: string; revision_no: number; content_hash: string; skills?: string[]; base_sha?: string; group_id?: string; label?: string; candidate_state?: string; steps?: { title: string; phase?: string }[]; locked_at?: string };
// One thing the approver can approve at the gate: the plan revision, or one
// candidate of a multi-agent plan round.
type ApproveTarget = { id: string; title: string; meta: string; steps: string[]; candidate: boolean; hash: string; toast: string };
type RunRow = { id: string; stage: string; status: string; worker_id?: string; runner?: string; attempt: number; branch?: string; base_sha?: string; head_sha?: string; result_summary?: string; diffstat?: string; fail_reason?: string; executor?: string; group_id?: string; focus?: string; cost_usd?: number; findings?: { severity: string; text: string; file?: string }[]; lease_expires_at?: string; pushed?: boolean; pr_url?: string; checks?: { name: string; passed: boolean; output?: string }[]; setup?: { name: string; passed: boolean; output?: string }[]; fix_rounds?: number; phase?: string; retries?: number; retry_after?: string; created_at: string; updated_at: string };
// One row of the preview's stage tracker. state: done | run | you | bad |
// up (not reached) | skip (not configured, or never reached on a discarded item).
type LinkRow = { key: string; href: string; text: string };
type MoreRow = { key: string; label: string; text: string; mono?: boolean };
type StageRow = { key: string; name: string; who: string; state: string; when: string; round: string; lines: string[]; links: LinkRow[]; more: MoreRow[]; moreLabel: string; warn: string };
type TextRow = { key: string; ic: string; text: string; more?: MoreRow[] };
type AuditEntry = { id: string; action: string; actor: string; via?: string; notes?: string; created_at: string };
// wsLocked: a workflow item cannot move workspace (its runs, plan and
// branch live there) — the server refuses it; the modal hides the picker.
type InboxForm = { id: string; title: string; tags: string; workspace: string; agents: string; branch: string; ref: string; wsLocked: boolean };

const UI_AUTHOR = 'ui';
type View = 'sessions' | 'blackboard' | 'plans' | 'inbox' | 'index' | 'keys' | 'workspaces' | 'workers';

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
  // Workers in scope (an active run on an offline worker is flagged) and the
  // item's audit trail (the History section).
  previewWorkers: [] as WorkerSeen[],
  previewTimeline: [] as AuditEntry[],
  previewStatusSeen: '',
  workflowNote: '',
  workflowBusy: false,
  expandedInbox: null as InboxDetail | null,
  // id of the card being dragged (HTML5 DnD; same-window only, so state is
  // the transport — dataTransfer.setData exists for Firefox's sake)
  inboxDragId: '',

  // workers (derived from run history — no registry yet)
  workers: [] as WorkerRow[],
  workersLoading: false,

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
  pipe: { open: false, kind: 'workspace', id: '', label: '', saving: false, readOnly: false, rev: 0, rows: [] as PipeRow[], agentsDefault: '', agentsInherited: '', agentsInheritedFrom: '', secondOpinion: '', secondInherited: '', codeIndex: false, codeIndexWas: false, indexableBy: [] as string[], notes: [] as { key: string; tone: string; text: string }[], runnerOptions: [] as string[] },
  groupModalMode: 'create' as 'create' | 'edit',
  groupSaving: false,
  groupErrors: {} as Record<string, string>,
  groupForm: { id: '', name: '', description: '', patterns: '', members: [] as string[] },
  // scoped API key create/edit: one dialog, two modes
  showApiKeyModal: false, keyModalMode: 'create' as 'create' | 'edit', keySaving: false,
  // access: 'full' (no capability limits), 'agent' (the agent preset: MCP
  // only) or 'keep' (edit: leave a custom set as it is).
  keyForm: { id: '', name: '', all: false, workspaces: [] as string[], groups: [] as string[], access: 'full', custom: '' },
  keyErrors: {} as Record<string, string>,
  showSecretModal: false, newSecret: '', newSecretName: '', secretCopied: false,

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
    return this.showKeyModal || this.showApiKeyModal || this.showSecretModal || this.showWsModal || this.showGroupModal
      || this.showEntryModal || this.showPlanModal || this.showItemModal || this.showInboxItemModal || this.showInboxPreview
      || this.showFileModal || this.confirm.open || this.pipe.open || (this.sidebarOpen && window.matchMedia('(max-width: 767px)').matches);
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
  toneOf(kind: 'status' | 'entry' | 'scope' | 'stage', value?: string | null): string {
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
      scope: { session: 'neutral', branch: 'accent', project: 'secondary' },
      stage: { plan: 'accent', implement: 'active', review: 'review', qa: 'review', second_opinion: 'review' },
    };
    return 'tone-' + (maps[kind][v] ?? 'neutral');
  },

  // ---- page chrome ----
  viewTitle(): string {
    return ({ sessions: 'Sessions', blackboard: 'Blackboard', plans: 'Plans', inbox: 'Inbox', index: 'Code index', keys: 'API keys', workspaces: 'Workspaces', workers: 'Workers' } as Record<string, string>)[this.activeView] ?? '';
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
    if (v === 'workspaces') { this.fetchWorkspaces(); this.fetchGroups(); if (this.whoamiIsRoot()) this.fetchKeys(); }
    if (v === 'workers') this.fetchWorkers();
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
  // tells them apart (github.com/fortix/freedom3 and a gitea mirror's
  // gitea.example.com:2223/fortix/freedom3 both read fortix/freedom3, and a
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
    else if (this.activeView === 'workspaces') { await this.fetchWorkspaces(); await this.fetchGroups(); if (this.whoamiIsRoot()) await this.fetchKeys(); }
    else if (this.activeView === 'workers') await this.fetchWorkers();
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
    const res = await this.authFetch('/api/inbox/' + encodeURIComponent(item.id));
    if (!this.showInboxPreview) return; // closed while fetching
    this.previewLoading = false;
    if (!res.ok) { this.notify('Could not load the item', 'error'); this.closeInboxPreview(); return; }
    this.previewItem = (await res.json()) as InboxDetail;
    // CSP: same imperative injection pattern as the expansion rows — the
    // sanitized content_html goes into the modal's data attribute node.
    const html = this.previewItem.content_html;
    this.$nextTick(() => {
      document.querySelectorAll('[data-md-preview]').forEach((el) => { el.innerHTML = html; });
    });
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
  pollPreview() {
    const item = this.previewItem;
    if (!this.showInboxPreview || !item) return;
    if (!['queued', 'planning', 'approved', 'implementing', 'in_review'].includes(item.status)) return;
    this.fetchPreviewRuns(item.id);
    this.fetchPreviewExtras(item.id);
  },
  // Workers (stuck-run warning) and the audit trail (History): refetched with
  // the runs on every live refresh.
  async fetchPreviewExtras(id: string) {
    const [wres, tres] = await Promise.all([
      this.authFetch('/api/workers'),
      this.authFetch('/api/inbox/' + encodeURIComponent(id) + '/timeline'),
    ]);
    if (!this.showInboxPreview || !this.previewItem || this.previewItem.id !== id) return;
    if (wres.ok) this.previewWorkers = ((await wres.json()) as WorkerSeen[]) ?? [];
    if (tres.ok) this.previewTimeline = ((await tres.json()) as AuditEntry[]) ?? [];
  },
  // Section defaults follow the status: the description is open until the
  // plan is approved, the run history is closed. Applied on open and on a
  // status change only.
  syncPreviewSections() {
    const st = this.previewItem ? this.previewItem.status : '';
    if (st === this.previewStatusSeen) return;
    this.previewStatusSeen = st;
    this.previewDescOpen = ['open', 'in_progress', 'queued', 'planning', 'awaiting_approval', 'converted'].includes(st);
    this.previewRunsOpen = false;
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
  closeInboxPreview() { if (previewPoll) { clearInterval(previewPoll); previewPoll = null; } this.showInboxPreview = false; this.previewLoading = false; this.previewItem = null; this.previewRuns = []; this.previewRunsLoading = false; this.previewRevision = null; this.previewCandidates = []; this.previewApprovedRev = null; this.previewPipe = null; this.previewStatusSeen = ''; this.previewWorkers = []; this.previewTimeline = []; this.workflowNote = ''; this.workflowBusy = false; },
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
    const body: Record<string, string> = { revision_id: t.id, content_hash: t.hash };
    if (this.workflowNote.trim()) body.notes = this.workflowNote;
    this.postWorkflowAction(item.id, 'approve', body, t.toast);
  },
  // Revise one candidate: a single planner re-plans from it with the notes.
  reviseCandidate(revisionId: string) {
    const item = this.previewItem;
    const t = this.approveTargets().find((x: ApproveTarget) => x.id === revisionId);
    if (!item || !t) return;
    if (!this.workflowNote.trim()) { this.notify('Write what to change in the notes first', 'error'); return; }
    this.postWorkflowAction(item.id, 'request-changes', { notes: this.workflowNote, revision_id: t.id }, 'Revising ' + t.title);
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
  },
  refreshPreviewRuns() { if (this.previewItem) this.fetchPreviewRuns(this.previewItem.id); },
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
  // Six fixed steps; two are the approver's. Built from what the preview
  // already loads (status, runs, the approved revision, the pipeline), so
  // a live refresh re-renders it. The rows split around the current step:
  // the action card sits right under it, the steps not reached below.
  stageOf(stage: string): number {
    if (stage === 'plan' || stage === 'compare' || stage === 'synthesize') return 1;
    if (stage === 'implement') return 3;
    if (stage === 'review' || stage === 'qa' || stage === 'branch_review') return 5;
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
  // A configured review/QA stage: a runner or a group. null = unknown (the
  // pipeline could not be read).
  reviewConfigured(): boolean | null {
    const p = this.previewPipe;
    if (!p) return null;
    const eff = p.effective || {};
    return ['review', 'qa'].some((s: string) => !!eff[s] && (eff[s].members || []).length > 0);
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
      const bad = (r: RunRow) => r.status === 'failed' || this.findingCount(r, 'blocking') > 0;
      return done.filter(bad).concat(done.filter((r: RunRow) => !bad(r)));
    }
    const since = this.runTime(impl);
    const done = this.previewRuns.filter((r: RunRow) => (r.stage === 'review' || r.stage === 'qa') && (r.status === 'succeeded' || r.status === 'failed') && this.runTime(r) >= since);
    // Blocking results first: a blocked step draws its first line red.
    return done.filter((r: RunRow) => r.status === 'failed').concat(done.filter((r: RunRow) => r.status !== 'failed'));
  },
  findingCount(r: RunRow, severity: string): number { return (r.findings || []).filter(f => f.severity === severity).length; },
  plural(n: number, word: string): string { return n + ' ' + word + (n === 1 ? '' : 's'); },
  reviewLine(r: RunRow): string {
    const who = (r.focus || (r.stage === 'branch_review' ? 'review' : r.stage)) + (r.runner ? ' (' + r.runner + ')' : '');
    // A branch review succeeds whatever its verdict: blocking findings are
    // the verdict.
    if (r.stage === 'branch_review' && r.status === 'succeeded') {
      const blocking = (r.findings || []).find(f => f.severity === 'blocking');
      if (blocking) return who + ' blocking: ' + blocking.text;
    }
    if (r.status === 'failed') {
      const blocking = (r.findings || []).find(f => f.severity === 'blocking');
      return who + ' blocking: ' + (blocking ? blocking.text : (r.fail_reason || 'failed'));
    }
    const nits = this.findingCount(r, 'nit');
    return who + ' passed' + (nits ? ', ' + this.plural(nits, 'nit') : '');
  },
  activeLines(step: number): string[] {
    return this.previewRuns.filter((r: RunRow) => this.runStep(r) === step && this.runActive(r)).map((r: RunRow) => {
      const detail = [r.runner, r.focus].filter(Boolean).join(', ');
      const what = r.stage === 'branch_review' ? (r.phase === 'reviewing' ? 'the review' + (r.runner ? ' (' + r.runner + ')' : '') : 'setup and the checks on ' + (r.branch || 'the branch')) : r.phase === 'checks' ? 'the checks' : r.phase === 'fixing' ? 'a fix round for the failed checks' + (r.runner ? ' (' + r.runner + ')' : '') : r.stage + (detail ? ' (' + detail + ')' : '');
      // A worker-side failure sent it back to the queue (it retries on its own).
      if (r.status === 'queued' && r.retries) return 'Retrying ' + what + ' (attempt ' + (r.retries + 1) + ' of 3) after a worker problem: ' + (r.fail_reason || '').replace(/^attempt \d+ of \d+ failed on /, '');
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
  runMore(r: RunRow, who: string): MoreRow[] {
    const out: MoreRow[] = (r.findings || []).map((f, k) => ({ key: r.id + '|f' + k, label: f.severity + ' · ' + who + (f.file ? ' · ' + f.file : ''), text: f.text }));
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
    const inputs = [this.previewItem, this.previewRuns, this.previewApprovedRev, this.previewPipe, this.previewWorkers];
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
      case 'in_review': cur = 6; curState = 'you'; break;
      case 'blocked': case 'failed': cur = checksBlocked || (reviewOnly && !!last && last.stage === 'branch_review') ? 4 : last ? Math.max(1, this.stageOf(last.stage)) : 1; curState = 'bad'; break;
      case 'done': cur = 7; break;
      case 'discarded': cur = 7; break;
    }
    const impl = this.trackImpl();
    // The implementation the Implement and Checks steps describe: the newest
    // successful one, the one its checks blocked, or the one being checked.
    const inChecks = st === 'implementing' && cur === 4;
    const implRun = checksBlocked || inChecks ? last : impl;
    const reviews = this.trackReviews();
    const planRuns = main.filter((r: RunRow) => r.stage === 'plan' && r.status === 'succeeded');
    const rev = this.previewApprovedRev;
    const configured = this.reviewConfigured();
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
      { name: 'Your sign-off', who: 'you' },
    ];
    // Evidence for a finished step.
    const doneLines = (i: number): { lines: string[]; when: string; links?: LinkRow[]; more?: MoreRow[]; warn?: string } => {
      if (i === 1 && planRuns.length) {
        const newest = planRuns[0];
        const round = newest.group_id ? planRuns.filter((r: RunRow) => r.group_id === newest.group_id) : [newest];
        const names = round.map((r: RunRow) => r.runner || r.worker_id || 'The agent').filter((v: string, k: number, a: string[]) => a.indexOf(v) === k);
        let text = round.length > 1 ? names.join(', ') + ' wrote ' + round.length + ' candidates' : names[0] + ' wrote the plan';
        if (main.some((r: RunRow) => r.stage === 'compare' && r.status === 'succeeded')) text += ', compared';
        if (main.some((r: RunRow) => r.stage === 'synthesize' && r.status === 'succeeded')) text += ', plus a synthesis';
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
      if (i === 5 && reviews.length) {
        const ok = reviews.filter((r: RunRow) => r.status === 'succeeded' && !(r.stage === 'branch_review' && this.findingCount(r, 'blocking') > 0)).length;
        const lines = reviews.map((r: RunRow) => this.reviewLine(r));
        if (impl && impl.head_sha) lines.push(ok + ' of ' + reviews.length + ' passed, on ' + this.shortSha(impl.head_sha));
        return { lines, when: this.timeAgo(reviews[0].updated_at), more: reviews.flatMap((r: RunRow) => this.runMore(r, this.reviewerName(r))) };
      }
      if (i === 6 && st === 'done') {
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
        lines = i === 5 && reviews.length ? reviews.map((r: RunRow) => this.reviewLine(r)) : [(failed && failed.fail_reason) || 'Blocked: open the run history for the reason'];
        if (i === 5) more = reviews.flatMap((r: RunRow) => this.runMore(r, this.reviewerName(r)));
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
          ? 'No review stage is configured for this workspace, so no agent reviews the commit. Set one under Workspaces → Pipeline.'
          : 'No agent reviewed this commit'];
      }
      // Review only: nobody plans or implements the branch.
      if (reviewOnly && i >= 1 && i <= 3) {
        state = 'skip';
        lines = i === 1 ? ['Review only: the agent checks and reviews ' + (item.branch ? item.branch : 'the existing branch') + '; nothing is planned or implemented. Request changes at sign-off to have it continued.'] : [];
        when = ''; links = []; more = []; warn = '';
      }
      // A discarded item: the steps it never finished are greyed out.
      if (st === 'discarded' && state === 'done' && !lines.length && i > 0) state = 'skip';
      const round = i >= 3 && i <= 6 && rounds > 1 && state !== 'up' ? 'round ' + rounds : '';
      // The key leaves out `when` (relative time): it changes every minute,
      // and a re-keyed row is re-created, closing an open fold-out. A kept
      // row still updates its time through the bindings.
      const moreLabel = i === 3 ? 'Setup output' : i === 4 ? 'Check output' : 'Findings and summaries';
      return { key: i + '|' + state + '|' + round + '|' + lines.join('/') + '|' + warn + '|' + more.map((m: MoreRow) => m.key).join(','), name: d.name, who: d.who, state, when, round, lines, links, more, moreLabel, warn };
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
    const words: Record<string, string> = { done: 'finished', run: 'in progress', you: 'waiting for you', bad: 'blocked', up: 'not reached yet', skip: 'skipped' };
    return s.name + ': ' + (words[s.state] || s.state);
  },
  stageTrackHead(): StageRow[] { const m = this.stageModel(); return m.current >= 7 ? m.rows : m.rows.slice(0, m.current + 1); },
  stageTrackTail(): StageRow[] { const m = this.stageModel(); return m.current >= 7 || m.current < 0 ? [] : m.rows.slice(m.current + 1); },
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
      else if (row.state === 'bad') { text = 'Blocked at ' + row.name.toLowerCase(); tone = 'tone-danger'; }
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
      const ok = reviews.filter((r: RunRow) => r.status === 'succeeded').length;
      const nits = reviews.reduce((n: number, r: RunRow) => n + this.findingCount(r, 'nit'), 0);
      out.push({ key: 'g-head|' + head, ic: 'y', text: 'Commit ' + this.shortSha(head) + ' is the branch head, and the reviewers checked it' });
      out.push({ key: 'g-rev|' + ok + '|' + reviews.length, ic: ok === reviews.length ? 'y' : 'w', text: 'Agent review: ' + ok + ' of ' + reviews.length + ' passed' + (nits ? ', ' + this.plural(nits, 'nit') : '') + (ok === reviews.length ? ', no blocking findings' : '') });
    } else {
      out.push({ key: 'g-head|' + head, ic: 'w', text: 'Commit ' + this.shortSha(head) + ' is the branch head. No agent reviewed it, so check it yourself' + (this.unpushed(head) ? ' on the worker.' : ' or ask for a second opinion.') });
    }
    return out;
  },
  // Second opinions already asked for, newest first: runner, commit, verdict
  // (a blocking finding = the reviewer would block). An opinion on an older
  // commit is marked, since it does not cover what sign-off approves.
  secondOpinionResults(): TextRow[] {
    const head = this.reviewHead();
    return this.previewRuns.filter((r: RunRow) => r.stage === 'second_opinion').map((r: RunRow) => {
      const on = (r.runner || 'agent') + ' on ' + this.shortSha(r.head_sha || '');
      let ic = 'y';
      let verdict = '';
      if (this.runActive(r)) { ic = 'n'; verdict = r.status === 'queued' ? 'waiting for a worker' : 'running'; }
      else if (r.status === 'failed') { ic = 'w'; verdict = 'did not finish: ' + (r.fail_reason || 'no reason given'); }
      else {
        const blocking = (r.findings || []).find(f => f.severity === 'blocking');
        const nits = this.findingCount(r, 'nit');
        if (blocking) { ic = 'w'; verdict = 'would block: ' + blocking.text; }
        else verdict = 'pass' + (nits ? ', ' + this.plural(nits, 'nit') : '');
      }
      const stale = !!head && !!r.head_sha && r.head_sha !== head ? ' (older commit)' : '';
      return { key: r.id + '|' + r.status + '|' + r.updated_at, ic, text: on + ': ' + verdict + stale, more: this.runMore(r, r.runner || 'agent') };
    });
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
  async reloadPreview() {
    if (!this.showInboxPreview || !this.previewItem) return;
    const id = this.previewItem.id;
    const res = await this.authFetch('/api/inbox/' + encodeURIComponent(id));
    if (!this.showInboxPreview) return;
    if (!res.ok) { this.closeInboxPreview(); return; }
    this.previewItem = (await res.json()) as InboxDetail;
    const html = this.previewItem.content_html;
    this.$nextTick(() => {
      document.querySelectorAll('[data-md-preview]').forEach((el) => { el.innerHTML = html; });
    });
    this.syncPreviewSections();
    this.fetchPreviewRuns(id);
    this.fetchPreviewRevision();
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
    if (this.workflowShowApprove()) return 'Notes (required for Request changes)';
    return '';
  },
  workflowNotePlaceholder(): string {
    if (this.workflowShowRetry()) return 'answer to the blocker (required)';
    if (this.workflowShowMarkDone()) return 'what to change (required for Request changes)';
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
    return !!this.workflowNote && this.workflowNote.trim().length > 0;
  },
  async postWorkflowAction(id: string, action: string, body: Record<string, string> | null, okMessage: string) {
    if (this.workflowBusy) return;
    this.workflowBusy = true;
    const opts: RequestInit = { method: 'POST', headers: { 'X-Skopos-Via': 'dashboard' } };
    if (body) opts.body = JSON.stringify(body);
    const res = await this.authFetch('/api/inbox/' + encodeURIComponent(id) + '/' + action, opts);
    this.workflowBusy = false;
    if (!await this.handleBad(res, 'Action failed')) {
      // A refusal often means the preview is stale (a newer revision, a
      // status that moved on): show the current state.
      await this.reloadPreview();
      return;
    }
    this.notify(okMessage, 'success');
    this.workflowNote = '';
    await this.fetchInbox();
    await this.reloadPreview();
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
  requestChangesInboxItem(id: string) { if (this.workflowNote.trim()) this.postWorkflowAction(id, 'request-changes', { notes: this.workflowNote }, 'Change requested'); },
  retryInboxItem(id: string) { if (this.workflowNote.trim()) this.postWorkflowAction(id, 'retry', { answer: this.workflowNote }, 'Requeued'); },
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
    const [res, wres] = await Promise.all([this.authFetch(base + encodeURIComponent(id) + '/pipeline'), this.authFetch('/api/workers')]);
    if (!await this.handleBad(res, 'Could not load the pipeline')) return;
    const v = (await res.json()) as PipeView;
    const runners: string[] = [];
    if (wres.ok) for (const w of (await wres.json()) as WorkerSeen[]) for (const r of w.runners || []) if (!runners.includes(r)) runners.push(r);
    this.pipe.kind = kind; this.pipe.id = v.owner_id; this.pipe.label = label;
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
    this.pipe.rows = ['plan', 'compare', 'synthesize', 'implement', 'review', 'qa'].map((stage: string) => {
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
    if (kind === 'workspace' && (v.groups || []).length) notes.push({ key: 'g', tone: 'tone-neutral', text: 'Inherits from group' + ((v.groups || []).length > 1 ? 's ' : ' ') + (v.groups || []).map((g: { name: string }) => g.name).join(', ') + ' where a stage is not set here.' });
    this.pipe.notes = notes;
    this.pipe.rev++;
    this.pipe.open = true;
  },
  closePipeline() { this.pipe.open = false; },
  pipeRow(stage: string): PipeRow | undefined { return this.pipe.rows.find((r: PipeRow) => r.stage === stage); },
  pipeBump() { this.pipe.rows = this.pipe.rows.map((r: PipeRow) => ({ ...r })); this.pipe.rev++; },
  pipeOverride(stage: string) {
    const r = this.pipeRow(stage);
    if (!r || !r.inherited) return;
    r.mode = 'own';
    r.members = r.inherited.members.map((m: PipeMember) => ({ runner: m.runner, focus: m.focus || '' }));
    r.policy = r.inherited.policy || ''; r.earlyExit = !!r.inherited.early_exit;
    r.timeoutMin = r.inherited.timeout_ms ? String(Math.round(r.inherited.timeout_ms / 60000)) : '';
    this.pipeBump();
  },
  pipeSetHere(stage: string) { const r = this.pipeRow(stage); if (r) { r.mode = 'own'; r.members = []; this.pipeBump(); } },
  // Back to the group's value (or to not set when no group sets it).
  pipeClearStage(stage: string) {
    const r = this.pipeRow(stage);
    if (!r) return;
    r.mode = r.inherited ? 'inherited' : 'unset'; r.members = []; r.policy = ''; r.earlyExit = false; r.timeoutMin = '';
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
    if (r.mode === 'unset') return r.stage === 'review' || r.stage === 'qa' ? 'not set — skipped' : (r.stage === 'compare' || r.stage === 'synthesize' ? 'not set — off' : 'not set — any runner');
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
      if (r.members.length === 0) { this.notify(r.label + ': add a runner, or clear the stage', 'error'); return; }
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

  // ---- workers (derived from run history) ----
  // There is no worker registry yet: what the server knows is what runs
  // reveal — who claimed what, when, and how it went. Offered runners and
  // versions need a registration step (future work); this view is the
  // honest projection of run history per worker.
  async fetchWorkers() {
    this.workersLoading = true;
    const [runsRes, workersRes] = await Promise.all([
      this.authFetch('/api/runs'),
      this.authFetch('/api/workers'),
    ]);
    this.workersLoading = false;
    if (!runsRes.ok) { this.notify('Could not load runs', 'error'); return; }
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
      byWorker.set(entry[0], { id: entry[0], workspaces: [...(reg.workspaces || [])], stages: [], runners: [...(reg.runners || [])], active: 0, succeeded: 0, failed: 0, lastActive: '', currentLabel: '', state: reg.state || '', version: reg.version || '', lastSeen: reg.last_seen || '', unhealthy: Object.entries(reg.unhealthy || {}).map(([ws, why]) => ws + ': ' + why), indexable: [...(reg.indexable || [])] });
    }
    for (const run of runs) {
      if (!run.worker_id) continue; // queued, never claimed
      let w = byWorker.get(run.worker_id);
      if (!w) { w = { id: run.worker_id, workspaces: [], stages: [], runners: [], active: 0, succeeded: 0, failed: 0, lastActive: run.updated_at, currentLabel: '', state: '', version: '', lastSeen: '', unhealthy: [], indexable: [] }; byWorker.set(run.worker_id, w); }
      const reg = registered.get(run.worker_id);
      if (reg) { for (const ws of reg.workspaces || []) if (!w.workspaces.includes(ws)) w.workspaces.push(ws); }
      if (run.workspace_id && !w.workspaces.includes(run.workspace_id)) w.workspaces.push(run.workspace_id);
      if (run.stage && !w.stages.includes(run.stage)) w.stages.push(run.stage);
      if (run.status === 'claimed' || run.status === 'running' || run.status === 'paused') { w.active++; w.currentLabel = run.stage + ' \u00b7 ' + run.status + ' \u00b7 ' + this.timeAgo(run.updated_at); }
      if (run.status === 'succeeded') w.succeeded++;
      if (run.status === 'failed') w.failed++;
      if (run.updated_at > w.lastActive) w.lastActive = run.updated_at;
    }
    // Online first (busy, idle), then offline; newest activity within each.
    const rank: Record<string, number> = { busy: 0, idle: 1, offline: 2 };
    this.workers = Array.from(byWorker.values()).sort((a: WorkerRow, b: WorkerRow) => {
      const ra = rank[this.workerStatusLabel(a)] ?? 3, rb = rank[this.workerStatusLabel(b)] ?? 3;
      if (ra !== rb) return ra - rb;
      const la = a.lastSeen || a.lastActive, lb = b.lastSeen || b.lastActive;
      return la < lb ? 1 : -1;
    });
  },
  // Presence comes from the server (busy / idle / offline); a worker known
  // only from run history (pre-presence) falls back to its runs.
  workerStatusLabel(w: WorkerRow): string { return w.state || (w.active > 0 ? 'busy' : 'offline'); },
  workerStatusClass(w: WorkerRow): string {
    const s = this.workerStatusLabel(w);
    if (w.unhealthy.length && s !== 'offline') return 'tone-danger';
    return s === 'busy' ? 'tone-success' : s === 'idle' ? 'tone-info' : 'tone-muted';
  },
  // The badge: an online worker whose preflight fails somewhere says so.
  workerBadge(w: WorkerRow): string {
    const s = this.workerStatusLabel(w);
    return w.unhealthy.length && s !== 'offline' ? s + ' · unhealthy' : s;
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
  // keyCapsLabel names a capped key's limits ("" = full access, no chip).
  keyCapsLabel(k: ApiKey): string {
    const caps = k.capabilities;
    if (!caps) return '';
    if (keyIsAgentPreset(caps)) return 'agent · MCP only';
    return caps.length ? caps.join(', ') : 'no access';
  },
  openNewKeyModal() {
    this.keyModalMode = 'create';
    this.keyForm = { id: '', name: '', all: false, workspaces: [], groups: [], access: 'full', custom: '' };
    this.keyErrors = {};
    this.formNonce++;
    this.showApiKeyModal = true;
  },
  openEditKeyModal(k: ApiKey) {
    this.keyModalMode = 'edit';
    const caps = k.capabilities;
    const access = !caps ? 'full' : (keyIsAgentPreset(caps) ? 'agent' : 'keep');
    this.keyForm = { id: k.id, name: k.name, all: k.all_workspaces, workspaces: [...(k.workspaces || [])], groups: [...(k.groups || [])], access, custom: access === 'keep' ? (caps || []).join(', ') : '' };
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
      if (f.access === 'agent') { body.preset = 'agent'; if (!creating) body.capabilities = []; }
      else if (f.access === 'full' && !creating) body.capabilities = [];
      const res = creating
        ? await this.authFetch('/api/keys', { method: 'POST', body: JSON.stringify(body) })
        : await this.authFetch('/api/keys/' + encodeURIComponent(f.id), { method: 'PATCH', body: JSON.stringify(body) });
      if (!await this.handleBad(res, creating ? 'Key not created' : 'Key not updated')) return;
      this.showApiKeyModal = false;
      if (creating) {
        const data = await res.json();
        this.newSecret = data.key_secret;
        this.newSecretName = data.key.name;
        this.showSecretModal = true;
      } else {
        this.notify('Key updated', 'success');
      }
      await this.fetchKeys();
    } finally {
      this.keySaving = false;
    }
  },
  closeSecretModal() { this.newSecret = ''; this.newSecretName = ''; this.secretCopied = false; this.showSecretModal = false; },
  async copySecret() {
    try {
      await navigator.clipboard.writeText(this.newSecret);
      this.secretCopied = true;
      this.notify('API key copied to clipboard', 'success');
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
