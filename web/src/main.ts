import Alpine from 'alpinejs';
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
type EntryForm = { scope: 'project' | 'branch' | 'session'; entry_type: 'finding' | 'decision' | 'bug' | 'debt' | 'warning' | 'context'; title: string; content: string; code_ref: string; branch_name: string; session_id: string };
type PlanForm = { name: string; description: string; branch_name: string };
type ItemForm = { title: string; description: string; phase: string; depends_on: string };

const UI_AUTHOR = 'ui';
type View = 'sessions' | 'blackboard' | 'plans';

declare global { interface Window { Alpine: typeof Alpine; app: () => object } }

window.app = () => ({
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

  // header
  activeWorkspace: '',
  workspaces: [] as string[],
  registeredWorkspaces: [] as { id: string; name: string }[],

  // api key
  apiKey: (localStorage.getItem('skopos:apiKey') || '') as string,
  showKeyModal: false,
  keyDraft: '',

  // toasts
  toasts: [] as Toast[],

  // live updates
  es: null as EventSource | null,
  streamConnected: false,
  streamRetryDelay: 2000,
  streamRetryTimer: null as ReturnType<typeof setTimeout> | null,
  pollTimer: null as ReturnType<typeof setInterval> | null,

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

  // SSE with polling fallback. On error the EventSource is closed (not left to
  // auto-reconnect every 3s), polling takes over, and SSE retries with backoff.
  openEventStream() {
    if (typeof EventSource === 'undefined') { this.startPolling(); return; }
    this.connectSSE();
  },
  connectSSE() {
    const es = new EventSource('/api/events/stream');
    this.es = es;
    es.onopen = () => {
      this.streamConnected = true;
      this.streamRetryDelay = 2000; // reset backoff
      this.stopPolling();
      this.refresh();
    };
    es.onerror = () => {
      this.streamConnected = false;
      es.close(); // prevent the browser's 3s auto-reconnect spam
      this.es = null;
      this.startPolling(); // keep data fresh while SSE is down
      this.streamRetryDelay = Math.min((this.streamRetryDelay ?? 2000) * 2, 60000);
      clearTimeout(this.streamRetryTimer ?? undefined);
      this.streamRetryTimer = setTimeout(() => this.connectSSE(), this.streamRetryDelay);
    };
    es.addEventListener('sessions', () => this.refresh());
    es.addEventListener('blackboard', () => { if (this.activeView === 'blackboard') this.fetchBundle(); });
    es.addEventListener('plans', () => { if (this.activeView === 'plans') this.fetchPlans(); });
    es.addEventListener('workspaces', () => this.fetchWorkspaces());
    es.addEventListener('change', () => this.refresh());
  },
  startPolling() {
    if (this.pollTimer) return;
    this.pollTimer = setInterval(() => this.refresh(), 5000);
  },
  stopPolling() {
    if (this.pollTimer) { clearInterval(this.pollTimer); this.pollTimer = null; }
  },

  anyModalOpen() {
    return this.showKeyModal || this.showEntryModal || this.showPlanModal || this.showItemModal || this.showWorkspaceModal || this.confirm.open;
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
  async authFetch(url: string, opts: RequestInit = {}): Promise<Response> {
    const headers = new Headers(opts.headers || {});
    if (opts.body) headers.set('Content-Type', 'application/json');
    if (this.apiKey) headers.set('Authorization', 'Bearer ' + this.apiKey);
    return fetch(url, { ...opts, headers });
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
  openKeyModal() { this.keyDraft = this.apiKey; this.showKeyModal = true; },
  closeKeyModal() { this.showKeyModal = false; },
  saveKey() {
    this.apiKey = this.keyDraft.trim();
    if (this.apiKey) localStorage.setItem('skopos:apiKey', this.apiKey); else localStorage.removeItem('skopos:apiKey');
    this.showKeyModal = false;
    this.notify(this.apiKey ? 'API key saved' : 'API key cleared', 'success');
  },
  clearKey() { this.apiKey = ''; this.keyDraft = ''; localStorage.removeItem('skopos:apiKey'); this.showKeyModal = false; this.notify('API key cleared', 'success'); },

  // ---- view / workspace ----
  switchView(v: View) {
    this.activeView = v; this.sidebarOpen = false; localStorage.setItem('skopos:view', v);
    if (v === 'blackboard') this.fetchBundle();
    if (v === 'plans') this.fetchPlans();
  },
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
    for (const w of this.registeredWorkspaces) map.set(w.id, w.name || w.id);
    for (const ws of this.workspaces) if (!map.has(ws)) map.set(ws, ws);
    return [...map.entries()].map(([id, label]) => ({ id, label }));
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
      if (this.activeWorkspace) body.workspace_id = this.activeWorkspace;
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
      if (this.activeWorkspace) body.workspace_id = this.activeWorkspace;
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
window.Alpine = Alpine;
Alpine.start();
