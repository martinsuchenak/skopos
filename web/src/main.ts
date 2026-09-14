// The CSP build of Alpine evaluates expressions without eval/new Function,
// letting the dashboard ship a strict Content-Security-Policy (no
// 'unsafe-eval'). It supports property access, operators, ternaries, and
// method calls — but not template literals or optional chaining, hence the
// expression style in base.html.
import Alpine from '@alpinejs/csp';
import focus from '@alpinejs/focus';

type SessionSummary = { id: string; title: string; workspace: string; status: string; agent_count: number };
type SessionDetail = SessionSummary & { agents?: AgentState[]; events?: Event[] };
type AgentState = { agent_id: string; agent_type: string; status: string; progress?: number; message: string; snippet: string };
type Event = AgentState & { id: string; created_at: string };
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

const UI_AUTHOR = 'ui';
type View = 'sessions' | 'blackboard' | 'plans' | 'index' | 'keys';

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

  // code index
  indexBranches: [] as { branch: string; head_sha?: string; built_at: string; source?: string; file_count: number; symbol_count: number }[],

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
  // modal: delete confirm
  confirm: { open: false, title: '', message: '', busy: false, pending: null as null | { kind: string; id: string } },
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
    return this.showNewKeyModal || this.showSecretModal || this.showEditKeyModal || this.showEntryModal || this.showPlanModal || this.showItemModal || this.showWorkspaceModal || this.confirm.open;
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
  async togglePlan(plan: Plan) {
    if (this.expandedPlan?.id === plan.id) { this.expandedPlan = null; return; }
    const res = await this.authFetch(`/api/plans/${encodeURIComponent(plan.id)}`);
    if (res.ok) this.expandedPlan = await res.json();
  },
  // Re-fetch the open plan and keep it expanded (used after item/dependency changes).
  async reloadPlan(planId: string) {
    if (this.expandedPlan?.id !== planId) return;
    const res = await this.authFetch(`/api/plans/${encodeURIComponent(planId)}`);
    if (res.ok) this.expandedPlan = await res.json();
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
  async updateItemStatus(planId: string, itemId: string, status: string) {
    if (!status) return;
    const res = await this.authFetch(`/api/plans/${encodeURIComponent(planId)}/items/${encodeURIComponent(itemId)}`, { method: 'PATCH', body: JSON.stringify({ status }) });
    if (!await this.handleBad(res, 'Status not updated')) return;
    this.notify('Item status updated', 'success'); await this.reloadPlan(planId);
  },
  async addPlanDependency(planId: string, itemId: string, dependsOnId: string) {
    if (!dependsOnId) return;
    const res = await this.authFetch(`/api/plans/${encodeURIComponent(planId)}/items/${encodeURIComponent(itemId)}/dependencies`, { method: 'POST', body: JSON.stringify({ depends_on_item_id: dependsOnId }) });
    if (!await this.handleBad(res, 'Dependency not added')) return;
    this.notify('Dependency added', 'success'); await this.reloadPlan(planId);
  },
  openPlanModal() {
    this.planForm = emptyPlanForm(this.plansBranch); this.planErrors = {};
    this.showPlanModal = true;
  },
  closePlanModal() { this.showPlanModal = false; },
  async submitPlan() {
    const e: Record<string, string> = {};
    if (!this.planForm.name.trim()) e.name = 'Name is required.';
    this.planErrors = e;
    if (Object.keys(e).length) return;
    this.planSaving = true;
    try {
      const body: Record<string, string> = { name: this.planForm.name.trim(), description: this.planForm.description.trim(), author_agent_id: UI_AUTHOR };
      if (this.planForm.branch_name.trim()) body.branch_name = this.planForm.branch_name.trim();
      body.workspace_id = this.writeWorkspace();
      if (!body.workspace_id) { this.notify('Writes are workspace-scoped — select a workspace first', 'error'); return; }
      const res = await this.authFetch('/api/plans', { method: 'POST', body: JSON.stringify(body) });
      if (!await this.handleBad(res, 'Plan not created')) return;
      this.notify('Plan created', 'success'); this.showPlanModal = false; await this.fetchPlans();
    } finally { this.planSaving = false; }
  },
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
    return k.all_workspaces ? '* (all workspaces)' : k.workspaces.join(', ');
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
    this.editKeyForm = { id: k.id, name: k.name, all: k.all_workspaces, workspaces: [...k.workspaces] };
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
    // No workspace selected: fall back to the first registered one — the
    // literal id "default" matches nothing and would show an empty index.
    const ws = this.activeWorkspace || this.registeredWorkspaces[0]?.id || this.workspaces[0] || 'default';
    try {
      const res = await this.authFetch(`/api/codeindex/${encodeURIComponent(ws)}/status`);
      if (!res.ok) { this.indexBranches = []; return; }
      this.indexBranches = (await res.json()) ?? [];
    } catch { this.indexBranches = []; }
  },

  // ---- delete (modal-driven) ----
  requestDelete(kind: string, id: string, name: string) {
    this.confirm = { open: true, title: `Delete ${kind}`, message: `Delete “${name}”? This cannot be undone.`, busy: false, pending: { kind, id } };
  },
  cancelDelete() { if (!this.confirm.busy) this.confirm = { open: false, title: '', message: '', busy: false, pending: null }; },
  async confirmDelete() {
    const p = this.confirm.pending; if (!p) return;
    this.confirm.busy = true;
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

Alpine.plugin(focus);
// Register as a named component: the CSP Alpine build cannot resolve globals
// from expressions, so x-data="app()" (window.app) would evaluate empty —
// x-data="app" resolves through this registry instead.
Alpine.data('app', appState);
window.Alpine = Alpine;
window.app = appState; // exposed for tests/debugging
Alpine.start();
