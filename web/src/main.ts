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
type ApiKey = { id: string; name: string; key_prefix: string; all_workspaces: boolean; workspaces: string[]; created_at: string; last_used_at?: string; revoked_at?: string };
type Whoami = { root: boolean; key?: { id: string; name: string; all_workspaces: boolean; workspaces: string[] }; workspaces: { id: string; name: string }[] };
type KeyForm = { name: string; all: boolean; workspaces: string[] };
type EntryForm = { scope: 'project' | 'branch' | 'session'; entry_type: 'finding' | 'decision' | 'bug' | 'debt' | 'warning' | 'context'; title: string; content: string; code_ref: string; branch_name: string; session_id: string };
type PlanForm = { name: string; description: string; branch_name: string };
type ItemForm = { title: string; description: string; phase: string; depends_on: string };
type InboxPlanSummary = { id: string; name: string; status: string };
type InboxItem = { id: string; workspace_id?: string; title: string; tags: string[]; status: string; priority?: number; claimed_by_agent_id?: string; author_agent_id: string; plan_id?: string; plan?: InboxPlanSummary; created_at: string; updated_at: string; excerpt?: string };
type InboxDetail = InboxItem & { content: string; content_html: string };
type InboxForm = { id: string; title: string; tags: string; workspace: string };

const UI_AUTHOR = 'ui';
type View = 'sessions' | 'blackboard' | 'plans' | 'inbox' | 'index' | 'keys';

// The inbox markdown editor lives OUTSIDE Alpine's reactive state: deep-
// proxying a live CodeMirror view breaks it. One instance per open modal.
let inboxEditor: EditorView | null = null;

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
  inboxStatus: 'open' as '' | 'open' | 'in_progress' | 'converted' | 'done' | 'discarded',
  inboxTagFilter: '',
  expandedInboxId: '',
  expandedInbox: null as InboxDetail | null,
  // id of the card being dragged (HTML5 DnD; same-window only, so state is
  // the transport — dataTransfer.setData exists for Firefox's sake)
  inboxDragId: '',

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
  showNewKeyModal: false, keySaving: false,
  keyForm: { name: '', all: false, workspaces: [] as string[] } as KeyForm,
  keyErrors: {} as Record<string, string>,
  showSecretModal: false, newSecret: '', newSecretName: '', secretCopied: false,
  showEditKeyModal: false, editKeySaving: false,
  editKeyForm: { id: '', name: '', all: false, workspaces: [] as string[] },
  editKeyErrors: {} as Record<string, string>,

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
  // modal: create workspace
  showWorkspaceModal: false, workspaceSaving: false,
  workspaceForm: { id: '', name: '' }, workspaceErrors: {} as Record<string, string>,

  init() {
    this.theme = (localStorage.getItem('skopos:theme') as 'dark' | 'light' | 'system') || 'system';
    this.applyTheme();
    if (window.matchMedia) {
      window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
        if (this.theme === 'system') this.applyTheme();
      });
    }
    this.activeView = (localStorage.getItem('skopos:view') || 'sessions') as View;
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
    else if (type === 'inbox') { if (this.activeView === 'inbox') this.fetchInbox(); }
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
    return this.showNewKeyModal || this.showSecretModal || this.showEditKeyModal || this.showEntryModal || this.showPlanModal || this.showItemModal || this.showInboxItemModal || this.showWorkspaceModal || this.confirm.open;
  },

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
  notify(message: string, type: Toast['type'] = 'info') {
    const id = Date.now() + Math.random();
    this.toasts.push({ id, message, type });
    window.setTimeout(() => this.dismissToast(id), type === 'error' ? 7000 : 5000);
  },
  dismissToast(id: number) { this.toasts = this.toasts.filter((t: Toast) => t.id !== id); },
  toastClass(type: Toast['type']): string {
    switch (type) {
      case 'success': return 'border-emerald-700 bg-emerald-950/80 text-emerald-200';
      case 'error': return 'border-rose-700 bg-rose-950/80 text-rose-200';
      default: return 'border-zinc-700 bg-zinc-900/90 text-zinc-200';
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
  },
  whoamiIsRoot(): boolean { return !!this.whoami && this.whoami.root; },
  setWorkspace(ws: string) { this.activeWorkspace = ws; this.refresh(); },

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
      for (const w of this.whoami.workspaces) map.set(w.id, w.name || w.id);
      return [...map.entries()].map(([id, label]) => ({ id, label }));
    }
    for (const w of this.registeredWorkspaces) map.set(w.id, w.name || w.id);
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
  workspaceLabel(id?: string): string {
    if (!id) return '';
    const w = this.registeredWorkspaces.find((r: { id: string; name: string }) => r.id === id);
    return w?.name || id;
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
  openWorkspaceModal() {
    this.workspaceForm = { id: '', name: '' }; this.workspaceErrors = {};
    this.showWorkspaceModal = true;
  },
  closeWorkspaceModal() { this.showWorkspaceModal = false; },
  async submitWorkspace() {
    const e: Record<string, string> = {};
    if (!this.workspaceForm.id.trim()) e.id = 'ID is required.';
    this.workspaceErrors = e;
    if (Object.keys(e).length) return;
    this.workspaceSaving = true;
    try {
      const id = this.workspaceForm.id.trim();
      const res = await this.authFetch('/api/workspaces', { method: 'POST', body: JSON.stringify({ id, name: this.workspaceForm.name.trim() }) });
      if (!await this.handleBad(res, 'Workspace not created')) return;
      this.notify('Workspace created', 'success');
      this.showWorkspaceModal = false;
      await this.fetchWorkspaces();
      this.activeWorkspace = id;
      await this.refresh();
    } finally { this.workspaceSaving = false; }
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
    await this.fetchWorkspaces();
    await this.autoRegisterWorkspaces();
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
  entryTypeClass(t: string) { return { bug: 'bg-rose-500/15 text-rose-300', debt: 'bg-amber-500/15 text-amber-300', warning: 'bg-amber-500/15 text-amber-300', finding: 'bg-cyan-500/15 text-cyan-300', decision: 'bg-emerald-500/15 text-emerald-300', context: 'bg-zinc-700 text-zinc-300' }[t] ?? 'bg-zinc-700 text-zinc-200'; },
  scopeClass(s: string) { return { session: 'bg-zinc-700 text-zinc-300', branch: 'bg-violet-500/15 text-violet-300', project: 'bg-indigo-500/15 text-indigo-300' }[s] ?? 'bg-zinc-700 text-zinc-200'; },

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
      const qs = p.size ? '?' + p.toString() : '';
      const res = await this.authFetch('/api/inbox' + qs);
      if (!res.ok) { this.inboxItems = []; return; }
      this.inboxItems = (await res.json()) ?? [];
    } catch { this.inboxItems = []; } finally { this.inboxLoading = false; }
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
  inboxStatusChips(): { key: string; label: string }[] {
    return [
      { key: 'open', label: 'Open' },
      { key: 'in_progress', label: 'In progress' },
      { key: 'converted', label: 'Converted' },
      { key: 'done', label: 'Done' },
      { key: 'discarded', label: 'Discarded' },
      { key: '', label: 'All' },
    ];
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
  inboxStatusClass(s: string) { return { open: 'bg-amber-500/15 text-amber-300', in_progress: 'bg-cyan-500/15 text-cyan-300', converted: 'bg-violet-500/15 text-violet-300', done: 'bg-emerald-500/15 text-emerald-300', discarded: 'bg-zinc-700 text-zinc-400' }[s] ?? 'bg-zinc-700 text-zinc-200'; },
  inboxItemEditable(item: InboxItem): boolean { return item.status === 'open' || item.status === 'in_progress'; },

  // ---- inbox board (swimlanes) ----
  inboxLanes(): { key: string; label: string }[] {
    return [
      { key: 'open', label: 'Open' },
      { key: 'in_progress', label: 'In progress' },
      { key: 'converted', label: 'Converted' },
      { key: 'done', label: 'Done' },
      { key: 'discarded', label: 'Discarded' },
    ];
  },
  laneItems(key: string): InboxItem[] { return this.inboxItems.filter((i: InboxItem) => i.status === key); },
  // Draggable: only actionable items plus converted (which can be dragged to
  // Discarded). Done/Discarded are terminal.
  inboxDraggable(item: InboxItem): boolean {
    // Discarded cards drag back to Open (restore); done is terminal.
    return item.status !== 'done';
  },
  laneAcceptsDrop(key: string): boolean {
    const from = this.inboxItems.find((i: InboxItem) => i.id === this.inboxDragId);
    if (!from) return false;
    if (key === from.status) return true; // reorder within the lane
    if (key === 'in_progress' && from.status === 'open') return true;
    if (key === 'open' && (from.status === 'in_progress' || from.status === 'discarded')) return true;
    if (key === 'discarded' && from.status !== 'done' && from.status !== 'discarded') return true;
    return false; // converted needs a plan; done is automatic
  },
  startInboxDrag(item: InboxItem, ev: DragEvent) {
    if (item.status === 'done') { ev.preventDefault(); return; } // terminal
    this.inboxDragId = item.id;
    if (ev.dataTransfer) {
      ev.dataTransfer.setData('text/plain', item.id); // Firefox requires data
      ev.dataTransfer.effectAllowed = 'move';
    }
  },
  endInboxDrag() { this.inboxDragId = ''; },
  laneDragOver(key: string, ev: DragEvent) {
    if (!this.laneAcceptsDrop(key)) return; // no preventDefault -> drop not allowed
    if (ev.dataTransfer) ev.dataTransfer.dropEffect = 'move';
    ev.preventDefault();
  },
  // Drop on a lane (its empty area or below all cards): a cross-lane drop is
  // the status transition; a same-lane drop at the end unprioritizes.
  async dropOnLane(key: string) {
    const dragId = this.inboxDragId;
    this.inboxDragId = '';
    if (!dragId) return;
    const from = this.inboxItems.find((i: InboxItem) => i.id === dragId);
    if (!from) return;
    if (key !== from.status) { await this.inboxTransition(dragId, from.status, key); return; }
    if (key !== 'open' && key !== 'in_progress') return;
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
    if (!dragId || dragId === target.id) return;
    const from = this.inboxItems.find((i: InboxItem) => i.id === dragId);
    if (!from) return;
    if (key !== from.status) { await this.inboxTransition(dragId, from.status, key); return; }
    if (key !== 'open' && key !== 'in_progress') return;
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
    else if (to === 'discarded') { await this.discardInboxItem(id); }
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
  // silently un-sync hidden ranked items (duplicate #1s).
  async inboxOrderedIds(excludeId: string): Promise<string[] | null> {
    const p = new URLSearchParams();
    if (this.activeWorkspace) p.set('workspace', this.activeWorkspace);
    const qs = p.size ? '?' + p.toString() : '';
    const res = await this.authFetch('/api/inbox' + qs);
    if (!res.ok) return null;
    const all = (await res.json()) as InboxItem[];
    return all.filter((i: InboxItem) => i.priority != null && i.id !== excludeId).map((i: InboxItem) => i.id);
  },
  async inboxSetPriority(id: string, priority: number) {
    const res = await this.authFetch('/api/inbox/' + encodeURIComponent(id), { method: 'PATCH', body: JSON.stringify({ priority }) });
    if (!await this.handleBad(res, 'Priority not updated')) return;
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
  // The editor host div lives inside the modal; it only exists in the DOM
  // once the modal renders, hence the $nextTick mount.
  mountInboxEditor(initial: string) {
    this.$nextTick(() => {
      const host = this.$refs.inboxEditorHost as HTMLElement | undefined;
      if (!host) return;
      inboxEditor = destroyEditor(inboxEditor);
      inboxEditor = mountMarkdownEditor(host, initial, {
        // Cmd/Ctrl+Enter saves straight from the editor.
        onSubmit: () => { this.submitInboxItem(); },
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
    this.inboxBaseline = { title: '', tags: '', content: '', workspace: this.activeWorkspace };
    this.showInboxItemModal = true;
    this.mountInboxEditor('');
  },
  async openInboxEditModal(item: InboxItem) {
    // Edit needs the full content: fetch the detail even if already expanded.
    const res = await this.authFetch('/api/inbox/' + encodeURIComponent(item.id));
    if (!await this.handleBad(res, 'Could not load item')) return;
    const detail = (await res.json()) as InboxDetail;
    this.inboxForm = { id: detail.id, title: detail.title, tags: (detail.tags || []).join(', '), workspace: detail.workspace_id || '' };
    this.inboxBaseline = { title: detail.title, tags: this.inboxForm.tags, content: detail.content, workspace: detail.workspace_id || '' };
    this.inboxErrors = {};
    this.showInboxItemModal = true;
    this.mountInboxEditor(detail.content);
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
      || this.inboxForm.workspace !== this.inboxBaseline.workspace;
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
        // 'unfiled' (root-only option) captures without a workspace; the
        // server rejects it for scoped keys with an actionable error.
        if (f.workspace !== 'unfiled') body.workspace_id = f.workspace;
        const res = await this.authFetch('/api/inbox', { method: 'POST', body: JSON.stringify(body) });
        if (!await this.handleBad(res, 'Item not captured')) return;
        this.notify(f.workspace === 'unfiled' ? 'Item captured (unfiled)' : 'Item captured', 'success');
      } else {
        const body: Record<string, unknown> = { title: f.title.trim(), content, tags };
        // Filing: a changed, non-empty workspace re-files the item.
        if (f.workspace && f.workspace !== this.inboxBaseline.workspace) body.workspace_id = f.workspace;
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
  async fileInboxItem(item: InboxItem, el: HTMLSelectElement) {
    const ws = el.value;
    el.value = '';
    if (!ws || !item.id) return;
    const res = await this.authFetch('/api/inbox/' + encodeURIComponent(item.id), { method: 'PATCH', body: JSON.stringify({ workspace_id: ws }) });
    if (!await this.handleBad(res, 'Could not file item')) return;
    this.notify('Filed into ' + ws, 'success');
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
    return k.all_workspaces ? '* (all workspaces)' : (k.workspaces || []).join(', ');
  },
  openNewKeyModal() {
    this.keyForm = { name: '', all: false, workspaces: [] };
    this.keyErrors = {};
    this.showNewKeyModal = true;
  },
  closeNewKeyModal() { this.showNewKeyModal = false; },
  toggleKeyWorkspace(id: string) {
    const i = this.keyForm.workspaces.indexOf(id);
    if (i >= 0) this.keyForm.workspaces.splice(i, 1);
    else this.keyForm.workspaces.push(id);
  },
  async submitKey() {
    const f = this.keyForm;
    const errs: Record<string, string> = {};
    if (!f.name.trim()) errs.name = 'Name is required.';
    if (!f.all && f.workspaces.length === 0) errs.workspaces = 'Select at least one workspace, or all workspaces.';
    this.keyErrors = errs;
    if (Object.keys(errs).length) return;
    this.keySaving = true;
    try {
      const body: Record<string, unknown> = { name: f.name.trim(), workspaces: f.all ? ['*'] : f.workspaces };
      const res = await this.authFetch('/api/keys', { method: 'POST', body: JSON.stringify(body) });
      if (!await this.handleBad(res, 'Key not created')) return;
      const data = await res.json();
      this.newSecret = data.key_secret;
      this.newSecretName = data.key.name;
      this.showNewKeyModal = false;
      this.showSecretModal = true;
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

  // ---- edit key ----
  openEditKeyModal(k: ApiKey) {
    this.editKeyForm = { id: k.id, name: k.name, all: k.all_workspaces, workspaces: [...(k.workspaces || [])] };
    this.editKeyErrors = {};
    this.showEditKeyModal = true;
  },
  closeEditKeyModal() { this.showEditKeyModal = false; },
  toggleEditKeyWorkspace(id: string) {
    const i = this.editKeyForm.workspaces.indexOf(id);
    if (i >= 0) this.editKeyForm.workspaces.splice(i, 1);
    else this.editKeyForm.workspaces.push(id);
  },
  async submitEditKey() {
    const f = this.editKeyForm;
    const errs: Record<string, string> = {};
    if (!f.name.trim()) errs.name = 'Name is required.';
    if (!f.all && f.workspaces.length === 0) errs.workspaces = 'Select at least one workspace, or all workspaces.';
    this.editKeyErrors = errs;
    if (Object.keys(errs).length) return;
    this.editKeySaving = true;
    try {
      const body: Record<string, unknown> = { name: f.name.trim(), workspaces: f.all ? ['*'] : f.workspaces };
      const res = await this.authFetch(`/api/keys/${encodeURIComponent(f.id)}`, { method: 'PATCH', body: JSON.stringify(body) });
      if (!await this.handleBad(res, 'Key not updated')) return;
      this.notify('Key updated', 'success');
      this.showEditKeyModal = false;
      await this.fetchKeys();
    } finally {
      this.editKeySaving = false;
    }
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
  cancelDelete() { if (!this.confirm.busy) this.confirm = { open: false, title: '', message: '', label: 'Delete', busy: false, pending: null }; },
  async confirmDelete() {
    const p = this.confirm.pending; if (!p) return;
    this.confirm.busy = true;
    // Discarding unsaved inbox edits closes the editor; nothing to delete.
    if (p.kind === 'inboxdiscard') {
      this.confirm.busy = false;
      this.confirm.open = false; this.confirm.pending = null;
      this.closeInboxItemModal();
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
  planStatusClass(s: string) { return { active: 'bg-cyan-500/15 text-cyan-300', completed: 'bg-emerald-500/15 text-emerald-300', archived: 'bg-zinc-700 text-zinc-400', blocked: 'bg-rose-500/15 text-rose-300' }[s] ?? 'bg-zinc-700 text-zinc-200'; },
  itemStatusClass(s: string) { return { done: 'bg-emerald-500/15 text-emerald-300', in_progress: 'bg-cyan-500/15 text-cyan-300', blocked: 'bg-rose-500/15 text-rose-300', pending: 'bg-zinc-700 text-zinc-300' }[s] ?? 'bg-zinc-700 text-zinc-200'; },
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
  statusClass(status?: string) {
    switch (status) {
      case 'succeeded': return 'bg-emerald-500/15 text-emerald-300';
      case 'failed': case 'blocked': return 'bg-rose-500/15 text-rose-300';
      case 'orphaned': return 'bg-rose-500/15 text-rose-200';
      case 'testing': case 'running': case 'editing': return 'bg-cyan-500/15 text-cyan-300';
      case 'waiting': case 'paused': case 'stuck': return 'bg-amber-500/15 text-amber-300';
      default: return 'bg-zinc-700 text-zinc-200';
    }
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
        statusCls: this.statusClass(e.status),
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
function emptyInboxForm(): InboxForm { return { id: '', title: '', tags: '', workspace: '' }; }
function emptyInboxBaseline() { return { title: '', tags: '', content: '', workspace: '' }; }

Alpine.plugin(focus);
// Register as a named component: the CSP Alpine build cannot resolve globals
// from expressions, so x-data="app()" (window.app) would evaluate empty —
// x-data="app" resolves through this registry instead.
Alpine.data('app', appState);
window.Alpine = Alpine;
window.app = appState; // exposed for tests/debugging
Alpine.start();
