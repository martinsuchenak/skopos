import Alpine from 'alpinejs';

type SessionSummary = {
  id: string;
  title: string;
  workspace: string;
  status: string;
  agent_count: number;
};

type SessionDetail = SessionSummary & {
  agents?: AgentState[];
  events?: Event[];
};

type AgentState = {
  agent_id: string;
  agent_type: string;
  status: string;
  progress?: number;
  message: string;
  snippet: string;
  original_status?: string;
  stuck_at?: string;
};

type Event = AgentState & {
  id: string;
  created_at: string;
};

type Entry = {
  id: string;
  scope: string;
  branch_name?: string;
  session_id?: string;
  entry_type: string;
  title: string;
  content: string;
  code_ref?: string;
  author_agent_id: string;
  created_at: string;
  updated_at: string;
};

type Bundle = {
  entries: Entry[];
  markdown_bundle: string;
};

type PlanItem = {
  id: string;
  plan_id: string;
  title: string;
  description?: string;
  phase?: string;
  status: string;
  position: number;
  claimed_by_agent_id?: string;
  depends_on?: string[];
};

type Plan = {
  id: string;
  name: string;
  branch_name?: string;
  description?: string;
  status: string;
  author_agent_id: string;
  items?: PlanItem[];
  depends_on?: string[];
  created_at: string;
};

type Toast = { id: number; message: string; type: 'success' | 'error' | 'info' };

type EntryForm = {
  scope: 'project' | 'branch' | 'session';
  entry_type: 'finding' | 'decision' | 'bug' | 'debt' | 'warning' | 'context';
  title: string;
  content: string;
  code_ref: string;
  branch_name: string;
  session_id: string;
};

type PlanForm = { name: string; description: string; branch_name: string };
type ItemForm = { title: string; description: string; phase: string; depends_on: string };

const UI_AUTHOR = 'ui';

declare global {
  interface Window {
    Alpine: typeof Alpine;
    app: () => object;
  }
}

window.app = () => ({
  sessions: [] as SessionSummary[],
  selectedSession: null as SessionDetail | null,
  selectedSessionId: '',
  loading: false,

  activeTab: (localStorage.getItem('skopos:tab') || 'sessions') as 'sessions' | 'blackboard' | 'plans',
  entries: [] as Entry[],
  blackboardBranch: '',
  blackboardLoading: false,

  plans: [] as Plan[],
  plansLoading: false,
  plansBranch: '',
  expandedPlan: null as Plan | null,

  activeWorkspace: '' as string,
  workspaces: [] as string[],

  // auth + feedback
  apiKey: (localStorage.getItem('skopos:apiKey') || '') as string,
  showKeyPanel: false,
  keyDraft: '',
  toasts: [] as Toast[],

  // authoring forms
  showEntryForm: false,
  entryForm: emptyEntryForm(),
  showPlanForm: false,
  planForm: emptyPlanForm(),
  showItemForm: false,
  itemForm: emptyItemForm(),

  init() {
    const saved = localStorage.getItem('skopos:session') || '';
    if (saved) this.selectedSessionId = saved;
    this.refresh();
    window.setInterval(() => this.refresh(), 5000);
  },

  // ---- networking helpers ----
  authHeaders(extra: Record<string, string> = {}): Headers {
    const h = new Headers(extra);
    if (this.apiKey) h.set('X-API-Key', this.apiKey);
    return h;
  },

  async authFetch(url: string, opts: RequestInit = {}): Promise<Response> {
    const headers = this.authHeaders();
    if (opts.body) headers.set('Content-Type', 'application/json');
    if (opts.headers) new Headers(opts.headers).forEach((v, k) => headers.set(k, v));
    return fetch(url, { ...opts, headers });
  },

  async extractError(res: Response): Promise<string> {
    try {
      const j = (await res.json()) as { error?: string };
      return j.error || res.statusText || 'request failed';
    } catch {
      return res.statusText || 'request failed';
    }
  },

  // ---- toasts ----
  notify(message: string, type: Toast['type'] = 'info') {
    const id = Date.now() + Math.random();
    this.toasts.push({ id, message, type });
    window.setTimeout(() => this.dismissToast(id), 4500);
  },
  dismissToast(id: number) {
    this.toasts = this.toasts.filter((t: Toast) => t.id !== id);
  },
  toastClass(type: Toast['type']): string {
    switch (type) {
      case 'success': return 'border-emerald-600 bg-emerald-950/80 text-emerald-200';
      case 'error': return 'border-rose-600 bg-rose-950/80 text-rose-200';
      default: return 'border-zinc-600 bg-zinc-900/90 text-zinc-200';
    }
  },

  // ---- api key ----
  openKeyPanel() {
    this.keyDraft = this.apiKey;
    this.showKeyPanel = true;
  },
  saveKey() {
    this.apiKey = this.keyDraft.trim();
    if (this.apiKey) localStorage.setItem('skopos:apiKey', this.apiKey);
    else localStorage.removeItem('skopos:apiKey');
    this.showKeyPanel = false;
    this.notify(this.apiKey ? 'API key saved' : 'API key cleared', 'success');
  },
  clearKey() {
    this.apiKey = '';
    this.keyDraft = '';
    localStorage.removeItem('skopos:apiKey');
    this.showKeyPanel = false;
  },

  workspaceParams(): string {
    const params = new URLSearchParams();
    if (this.activeWorkspace) params.set('workspace', this.activeWorkspace);
    return params.size > 0 ? '?' + params.toString() : '';
  },

  setWorkspace(ws: string) {
    this.activeWorkspace = ws;
    this.refresh();
  },

  async refresh() {
    this.loading = true;
    try {
      const response = await this.authFetch('/api/sessions' + this.workspaceParams());
      if (response.ok) this.sessions = await response.json();
    } finally {
      this.loading = false;
    }
    this.workspaces = [...new Set(this.sessions.map((s: SessionSummary) => s.workspace).filter(Boolean))];
    if (!this.selectedSessionId && this.sessions.length > 0) {
      this.selectedSessionId = this.sessions[0].id;
    }
    if (this.selectedSessionId) {
      const match = this.sessions.find((s: SessionSummary) => s.id === this.selectedSessionId);
      if (!match) {
        this.selectedSessionId = '';
        this.selectedSession = null;
        localStorage.removeItem('skopos:session');
      } else {
        await this.selectSession(this.selectedSessionId);
      }
    }
    if (this.activeTab === 'blackboard') await this.fetchBundle();
    if (this.activeTab === 'plans') await this.fetchPlans();
  },

  async selectSession(id: string) {
    this.selectedSessionId = id;
    localStorage.setItem('skopos:session', id);
    const response = await this.authFetch(`/api/sessions/${encodeURIComponent(id)}`);
    if (response.ok) this.selectedSession = await response.json();
  },

  async switchTab(tab: 'sessions' | 'blackboard' | 'plans') {
    this.activeTab = tab;
    localStorage.setItem('skopos:tab', tab);
    if (tab === 'blackboard') await this.fetchBundle();
    if (tab === 'plans') await this.fetchPlans();
  },

  // ---- deletes ----
  async doDelete(url: string, onOk: () => Promise<void> | void, label = 'Item') {
    if (!confirm(`Delete this ${label.toLowerCase()}? This cannot be undone.`)) return;
    const res = await this.authFetch(url, { method: 'DELETE' });
    if (!res.ok) {
      const msg = await this.extractError(res);
      this.notify(`${label} not deleted: ${msg}`, 'error');
      if (res.status === 401) this.openKeyPanel();
      return;
    }
    this.notify(`${label} deleted`, 'success');
    await onOk();
  },

  deleteSession(id: string) {
    return this.doDelete(`/api/sessions/${encodeURIComponent(id)}`, () => {
      if (this.selectedSessionId === id) {
        this.selectedSessionId = '';
        this.selectedSession = null;
        localStorage.removeItem('skopos:session');
      }
      return this.refresh();
    }, 'Session');
  },

  deleteEntry(id: string) {
    return this.doDelete(`/api/blackboard/entries/${encodeURIComponent(id)}`, () => this.fetchBundle(), 'Entry');
  },

  deletePlan(id: string) {
    return this.doDelete(`/api/plans/${encodeURIComponent(id)}`, () => {
      if (this.expandedPlan?.id === id) this.expandedPlan = null;
      return this.fetchPlans();
    }, 'Plan');
  },

  deletePlanItem(planId: string, itemId: string) {
    return this.doDelete(
      `/api/plans/${encodeURIComponent(planId)}/items/${encodeURIComponent(itemId)}`,
      () => this.togglePlan({ id: planId } as Plan),
      'Item',
    );
  },

  // ---- blackboard authoring ----
  async fetchBundle() {
    this.blackboardLoading = true;
    try {
      const params = new URLSearchParams();
      if (this.activeWorkspace) params.set('workspace', this.activeWorkspace);
      if (this.blackboardBranch.trim()) params.set('branch', this.blackboardBranch.trim());
      const qs = params.size > 0 ? '?' + params.toString() : '';
      const response = await this.authFetch('/api/blackboard/entries' + qs);
      if (!response.ok) {
        this.entries = [];
        return;
      }
      const bundle: Bundle = await response.json();
      this.entries = bundle.entries ?? [];
    } catch {
      this.entries = [];
    } finally {
      this.blackboardLoading = false;
    }
  },

  toggleEntryForm() {
    this.showEntryForm = !this.showEntryForm;
    if (this.showEntryForm) this.entryForm = emptyEntryForm(this.blackboardBranch);
  },

  async writeEntry() {
    const f = this.entryForm;
    if (!f.title.trim()) {
      this.notify('Title is required', 'error');
      return;
    }
    const body: Record<string, string> = {
      scope: f.scope,
      entry_type: f.entry_type,
      title: f.title.trim(),
      content: f.content.trim(),
      author_agent_id: UI_AUTHOR,
    };
    if (this.activeWorkspace) body.workspace_id = this.activeWorkspace;
    if (f.scope === 'branch') body.branch_name = f.branch_name.trim();
    if (f.scope === 'session') body.session_id = f.session_id.trim();
    if (f.code_ref.trim()) body.code_ref = f.code_ref.trim();

    const res = await this.authFetch('/api/blackboard/entries', {
      method: 'POST',
      body: JSON.stringify(body),
    });
    if (!res.ok) {
      const msg = await this.extractError(res);
      this.notify(`Entry not written: ${msg}`, 'error');
      if (res.status === 401) this.openKeyPanel();
      return;
    }
    this.notify('Entry written', 'success');
    this.showEntryForm = false;
    await this.fetchBundle();
  },

  async promoteEntry(id: string) {
    const res = await this.authFetch(`/api/blackboard/entries/${encodeURIComponent(id)}/promote`, {
      method: 'PATCH',
    });
    if (!res.ok) {
      const msg = await this.extractError(res);
      this.notify(`Promote failed: ${msg}`, 'error');
      if (res.status === 401) this.openKeyPanel();
      return;
    }
    this.notify('Entry promoted', 'success');
    await this.fetchBundle();
  },

  entriesByType(): { type: string; label: string; colorClass: string; items: Entry[] }[] {
    const order = ['bug', 'debt', 'warning', 'finding', 'decision', 'context'];
    const labels: Record<string, string> = {
      bug: 'Bugs', debt: 'Tech Debt', warning: 'Warnings',
      finding: 'Findings', decision: 'Decisions', context: 'Context',
    };
    const colors: Record<string, string> = {
      bug: 'text-rose-400', debt: 'text-amber-400', warning: 'text-amber-400',
      finding: 'text-cyan-400', decision: 'text-emerald-400', context: 'text-zinc-400',
    };
    return order
      .map(type => ({
        type, label: labels[type] ?? type,
        colorClass: colors[type] ?? 'text-zinc-400',
        items: (this.entries ?? []).filter((e: Entry) => e.entry_type === type),
      }))
      .filter(g => g.items.length > 0);
  },

  entryTypeClass(type: string): string {
    const map: Record<string, string> = {
      bug: 'bg-rose-500/15 text-rose-300', debt: 'bg-amber-500/15 text-amber-300',
      warning: 'bg-amber-500/15 text-amber-300', finding: 'bg-cyan-500/15 text-cyan-300',
      decision: 'bg-emerald-500/15 text-emerald-300', context: 'bg-zinc-700 text-zinc-300',
    };
    return map[type] ?? 'bg-zinc-700 text-zinc-200';
  },

  scopeClass(scope: string): string {
    const map: Record<string, string> = {
      session: 'bg-zinc-700 text-zinc-300',
      branch: 'bg-violet-500/15 text-violet-300',
      project: 'bg-indigo-500/15 text-indigo-300',
    };
    return map[scope] ?? 'bg-zinc-700 text-zinc-200';
  },

  // ---- plans authoring ----
  async fetchPlans() {
    this.plansLoading = true;
    try {
      const params = new URLSearchParams();
      if (this.activeWorkspace) params.set('workspace', this.activeWorkspace);
      if (this.plansBranch.trim()) params.set('branch', this.plansBranch.trim());
      const qs = params.size > 0 ? '?' + params.toString() : '';
      const response = await this.authFetch('/api/plans' + qs);
      if (!response.ok) {
        this.plans = [];
        return;
      }
      this.plans = await response.json();
    } catch {
      this.plans = [];
    } finally {
      this.plansLoading = false;
    }
  },

  togglePlanForm() {
    this.showPlanForm = !this.showPlanForm;
    if (this.showPlanForm) this.planForm = emptyPlanForm(this.plansBranch);
  },

  async createPlan() {
    const f = this.planForm;
    if (!f.name.trim()) {
      this.notify('Plan name is required', 'error');
      return;
    }
    const body: Record<string, string> = {
      name: f.name.trim(),
      description: f.description.trim(),
      author_agent_id: UI_AUTHOR,
    };
    if (f.branch_name.trim()) body.branch_name = f.branch_name.trim();
    if (this.activeWorkspace) body.workspace_id = this.activeWorkspace;

    const res = await this.authFetch('/api/plans', { method: 'POST', body: JSON.stringify(body) });
    if (!res.ok) {
      const msg = await this.extractError(res);
      this.notify(`Plan not created: ${msg}`, 'error');
      if (res.status === 401) this.openKeyPanel();
      return;
    }
    this.notify('Plan created', 'success');
    this.showPlanForm = false;
    await this.fetchPlans();
  },

  async updatePlanStatus(planId: string, status: string) {
    if (!status) return;
    const res = await this.authFetch(`/api/plans/${encodeURIComponent(planId)}`, {
      method: 'PATCH', body: JSON.stringify({ status }),
    });
    if (!res.ok) {
      const msg = await this.extractError(res);
      this.notify(`Status not updated: ${msg}`, 'error');
      if (res.status === 401) this.openKeyPanel();
      return;
    }
    this.notify('Plan status updated', 'success');
    await this.fetchPlans();
    if (this.expandedPlan?.id === planId) await this.togglePlan({ id: planId } as Plan);
  },

  toggleItemForm() {
    this.showItemForm = !this.showItemForm;
    if (this.showItemForm) this.itemForm = emptyItemForm();
  },

  async addPlanItem() {
    const planId = this.expandedPlan?.id;
    if (!planId) return;
    const f = this.itemForm;
    if (!f.title.trim()) {
      this.notify('Item title is required', 'error');
      return;
    }
    const body: Record<string, unknown> = { title: f.title.trim(), description: f.description.trim() };
    if (f.phase.trim()) body.phase = f.phase.trim();
    if (f.depends_on) body.depends_on = [f.depends_on];

    const res = await this.authFetch(`/api/plans/${encodeURIComponent(planId)}/items`, {
      method: 'POST', body: JSON.stringify(body),
    });
    if (!res.ok) {
      const msg = await this.extractError(res);
      this.notify(`Item not added: ${msg}`, 'error');
      if (res.status === 401) this.openKeyPanel();
      return;
    }
    this.notify('Item added', 'success');
    this.showItemForm = false;
    await this.togglePlan({ id: planId } as Plan);
  },

  async updateItemStatus(planId: string, itemId: string, status: string) {
    if (!status) return;
    const res = await this.authFetch(`/api/plans/${encodeURIComponent(planId)}/items/${encodeURIComponent(itemId)}`, {
      method: 'PATCH', body: JSON.stringify({ status }),
    });
    if (!res.ok) {
      const msg = await this.extractError(res);
      this.notify(`Status not updated: ${msg}`, 'error');
      if (res.status === 401) this.openKeyPanel();
      return;
    }
    this.notify('Item status updated', 'success');
    await this.togglePlan({ id: planId } as Plan);
  },

  async addPlanDependency(planId: string, itemId: string, dependsOnId: string) {
    if (!dependsOnId) return;
    const res = await this.authFetch(
      `/api/plans/${encodeURIComponent(planId)}/items/${encodeURIComponent(itemId)}/dependencies`,
      { method: 'POST', body: JSON.stringify({ depends_on_item_id: dependsOnId }) },
    );
    if (!res.ok) {
      const msg = await this.extractError(res);
      this.notify(`Dependency not added: ${msg}`, 'error');
      if (res.status === 401) this.openKeyPanel();
      return;
    }
    this.notify('Dependency added', 'success');
    await this.togglePlan({ id: planId } as Plan);
  },

  async togglePlan(plan: Plan) {
    if (this.expandedPlan?.id === plan.id) {
      this.expandedPlan = null;
      this.showItemForm = false;
      return;
    }
    const response = await this.authFetch(`/api/plans/${encodeURIComponent(plan.id)}`);
    if (response.ok) {
      this.expandedPlan = await response.json();
      this.showItemForm = false;
    }
  },

  otherItems(item: PlanItem): PlanItem[] {
    const deps = new Set(item.depends_on ?? []);
    return (this.expandedPlan?.items ?? []).filter(i => i.id !== item.id && !deps.has(i.id));
  },

  planStatusClass(status: string): string {
    const map: Record<string, string> = {
      active: 'bg-cyan-500/15 text-cyan-300',
      completed: 'bg-emerald-500/15 text-emerald-300',
      archived: 'bg-zinc-700 text-zinc-400',
      blocked: 'bg-rose-500/15 text-rose-300',
    };
    return map[status] ?? 'bg-zinc-700 text-zinc-200';
  },

  itemStatusClass(status: string): string {
    const map: Record<string, string> = {
      done: 'bg-emerald-500/15 text-emerald-300',
      in_progress: 'bg-cyan-500/15 text-cyan-300',
      blocked: 'bg-rose-500/15 text-rose-300',
      pending: 'bg-zinc-700 text-zinc-300',
    };
    return map[status] ?? 'bg-zinc-700 text-zinc-200';
  },

  depLabel(item: PlanItem): string {
    if (!item.depends_on || item.depends_on.length === 0) return '';
    const items = this.expandedPlan?.items ?? [];
    return item.depends_on.map(depId => {
      const dep = items.find(i => i.id === depId);
      return dep ? `#${dep.position}` : '...';
    }).join(', ');
  },

  itemsGroupedByPhase(items: PlanItem[]): { phase: string; label: string; items: PlanItem[] }[] {
    if (!items || items.length === 0) return [];
    const groups = new Map<string, PlanItem[]>();
    const order: string[] = [];
    for (const item of items) {
      const phase = item.phase || '';
      if (!groups.has(phase)) {
        groups.set(phase, []);
        order.push(phase);
      }
      groups.get(phase)!.push(item);
    }
    return order.map(phase => ({
      phase, label: phase || 'Items', items: groups.get(phase)!,
    }));
  },

  planDepNames(plan: Plan): string {
    if (!plan.depends_on || plan.depends_on.length === 0) return '';
    return plan.depends_on.map(depId => {
      const dep = this.plans.find(p => p.id === depId);
      return dep ? dep.name : depId.slice(0, 8) + '...';
    }).join(', ');
  },

  statusClass(status?: string) {
    switch (status) {
      case 'succeeded': return 'bg-emerald-500/15 text-emerald-300';
      case 'failed':
      case 'blocked': return 'bg-rose-500/15 text-rose-300';
      case 'orphaned': return 'bg-rose-500/15 text-rose-200';
      case 'testing':
      case 'running':
      case 'editing': return 'bg-cyan-500/15 text-cyan-300';
      case 'waiting':
      case 'paused':
      case 'stuck': return 'bg-amber-500/15 text-amber-300';
      default: return 'bg-zinc-700 text-zinc-200';
    }
  },

  formatTime(value: string) {
    if (!value) return '';
    return new Intl.DateTimeFormat(undefined, {
      hour: '2-digit', minute: '2-digit', second: '2-digit',
    }).format(new Date(value));
  },
});

function emptyEntryForm(branch = ''): EntryForm {
  return { scope: 'branch', entry_type: 'finding', title: '', content: '', code_ref: '', branch_name: branch, session_id: '' };
}
function emptyPlanForm(branch = ''): PlanForm {
  return { name: '', description: '', branch_name: branch };
}
function emptyItemForm(): ItemForm {
  return { title: '', description: '', phase: '', depends_on: '' };
}

window.Alpine = Alpine;
Alpine.start();
