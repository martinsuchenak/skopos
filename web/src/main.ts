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

  init() {
    const saved = localStorage.getItem('skopos:session') || '';
    if (saved) this.selectedSessionId = saved;
    this.refresh();
    window.setInterval(() => this.refresh(), 5000);
  },

  workspaceParams(): string {
    const params = new URLSearchParams();
    if (this.activeWorkspace) {
      params.set('workspace', this.activeWorkspace);
    }
    return params.size > 0 ? '?' + params.toString() : '';
  },

  setWorkspace(ws: string) {
    this.activeWorkspace = ws;
    this.refresh();
  },

  async refresh() {
    this.loading = true;
    try {
      const response = await fetch('/api/sessions' + this.workspaceParams());
      this.sessions = await response.json();
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
    } finally {
      this.loading = false;
    }
    if (this.activeTab === 'blackboard') {
      await this.fetchBundle();
    }
    if (this.activeTab === 'plans') {
      await this.fetchPlans();
    }
  },

  async selectSession(id: string) {
    this.selectedSessionId = id;
    localStorage.setItem('skopos:session', id);
    const response = await fetch(`/api/sessions/${encodeURIComponent(id)}`);
    this.selectedSession = await response.json();
  },

  async switchTab(tab: 'sessions' | 'blackboard' | 'plans') {
    this.activeTab = tab;
    localStorage.setItem('skopos:tab', tab);
    if (tab === 'blackboard') {
      await this.fetchBundle();
    }
    if (tab === 'plans') {
      await this.fetchPlans();
    }
  },

  async confirmDelete(url: string, onSuccess: () => Promise<void> | void) {
    if (!confirm('Delete this item? This cannot be undone.')) return;
    const res = await fetch(url, { method: 'DELETE' });
    if (res.ok) {
      await onSuccess();
    }
  },

  async deleteSession(id: string) {
    await this.confirmDelete(`/api/sessions/${encodeURIComponent(id)}`, () => {
      if (this.selectedSessionId === id) {
        this.selectedSessionId = '';
        this.selectedSession = null;
        localStorage.removeItem('skopos:session');
      }
      return this.refresh();
    });
  },

  async deleteEntry(id: string) {
    await this.confirmDelete(`/api/blackboard/entries/${encodeURIComponent(id)}`, () => this.fetchBundle());
  },

  async deletePlan(id: string) {
    await this.confirmDelete(`/api/plans/${encodeURIComponent(id)}`, () => {
      if (this.expandedPlan?.id === id) this.expandedPlan = null;
      return this.fetchPlans();
    });
  },

  async deletePlanItem(planId: string, itemId: string) {
    await this.confirmDelete(`/api/plans/${encodeURIComponent(planId)}/items/${encodeURIComponent(itemId)}`, () => {
      return this.togglePlan({ id: planId } as Plan);
    });
  },

  async fetchBundle() {
    this.blackboardLoading = true;
    try {
      const params = new URLSearchParams();
      if (this.activeWorkspace) {
        params.set('workspace', this.activeWorkspace);
      }
      if (this.blackboardBranch.trim()) {
        params.set('branch', this.blackboardBranch.trim());
      }
      const qs = params.size > 0 ? '?' + params.toString() : '';
      const response = await fetch('/api/blackboard/entries' + qs);
      const bundle: Bundle = await response.json();
      this.entries = bundle.entries ?? [];
    } catch {
      this.entries = [];
    } finally {
      this.blackboardLoading = false;
    }
  },

  entriesByType(): { type: string; label: string; colorClass: string; items: Entry[] }[] {
    const order = ['bug', 'debt', 'warning', 'finding', 'decision', 'context'];
    const labels: Record<string, string> = {
      bug: 'Bugs',
      debt: 'Tech Debt',
      warning: 'Warnings',
      finding: 'Findings',
      decision: 'Decisions',
      context: 'Context',
    };
    const colors: Record<string, string> = {
      bug: 'text-rose-400',
      debt: 'text-amber-400',
      warning: 'text-amber-400',
      finding: 'text-cyan-400',
      decision: 'text-emerald-400',
      context: 'text-zinc-400',
    };
    return order
      .map(type => ({
        type,
        label: labels[type] ?? type,
        colorClass: colors[type] ?? 'text-zinc-400',
        items: (this.entries ?? []).filter((e: Entry) => e.entry_type === type),
      }))
      .filter(g => g.items.length > 0);
  },

  entryTypeClass(type: string): string {
    const map: Record<string, string> = {
      bug: 'bg-rose-500/15 text-rose-300',
      debt: 'bg-amber-500/15 text-amber-300',
      warning: 'bg-amber-500/15 text-amber-300',
      finding: 'bg-cyan-500/15 text-cyan-300',
      decision: 'bg-emerald-500/15 text-emerald-300',
      context: 'bg-zinc-700 text-zinc-300',
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

  async fetchPlans() {
    this.plansLoading = true;
    try {
      const params = new URLSearchParams();
      if (this.activeWorkspace) {
        params.set('workspace', this.activeWorkspace);
      }
      if (this.plansBranch.trim()) {
        params.set('branch', this.plansBranch.trim());
      }
      const qs = params.size > 0 ? '?' + params.toString() : '';
      const response = await fetch('/api/plans' + qs);
      this.plans = await response.json();
    } catch {
      this.plans = [];
    } finally {
      this.plansLoading = false;
    }
  },

  async togglePlan(plan: Plan) {
    if (this.expandedPlan?.id === plan.id) {
      this.expandedPlan = null;
      return;
    }
    const response = await fetch(`/api/plans/${encodeURIComponent(plan.id)}`);
    this.expandedPlan = await response.json();
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
    const labels = item.depends_on.map(depId => {
      const dep = items.find(i => i.id === depId);
      return dep ? `#${dep.position}` : '...';
    });
    return labels.join(', ');
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
      phase,
      label: phase || 'Items',
      items: groups.get(phase)!,
    }));
  },

  planDepNames(plan: Plan): string {
    if (!plan.depends_on || plan.depends_on.length === 0) return '';
    const names = plan.depends_on.map(depId => {
      const dep = this.plans.find(p => p.id === depId);
      return dep ? dep.name : depId.slice(0, 8) + '...';
    });
    return names.join(', ');
  },

  statusClass(status?: string) {
    switch (status) {
      case 'succeeded':
        return 'bg-emerald-500/15 text-emerald-300';
      case 'failed':
      case 'blocked':
        return 'bg-rose-500/15 text-rose-300';
      case 'orphaned':
        return 'bg-rose-500/15 text-rose-200';
      case 'testing':
      case 'running':
      case 'editing':
        return 'bg-cyan-500/15 text-cyan-300';
      case 'waiting':
      case 'paused':
      case 'stuck':
        return 'bg-amber-500/15 text-amber-300';
      default:
        return 'bg-zinc-700 text-zinc-200';
    }
  },

  formatTime(value: string) {
    if (!value) return '';
    return new Intl.DateTimeFormat(undefined, {
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
    }).format(new Date(value));
  },
});

window.Alpine = Alpine;
Alpine.start();
