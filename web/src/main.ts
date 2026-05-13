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

declare global {
  interface Window {
    Alpine: typeof Alpine;
    app: () => object;
  }
}

window.app = () => ({
  // sessions tab
  sessions: [] as SessionSummary[],
  selectedSession: null as SessionDetail | null,
  selectedSessionId: '',
  loading: false,

  // blackboard tab
  activeTab: 'sessions' as 'sessions' | 'blackboard',
  entries: [] as Entry[],
  blackboardBranch: '',
  blackboardLoading: false,

  init() {
    this.refresh();
    window.setInterval(() => this.refresh(), 5000);
  },

  async refresh() {
    this.loading = true;
    try {
      const response = await fetch('/api/sessions');
      this.sessions = await response.json();
      if (!this.selectedSessionId && this.sessions.length > 0) {
        this.selectedSessionId = this.sessions[0].id;
      }
      if (this.selectedSessionId) {
        await this.selectSession(this.selectedSessionId);
      }
    } finally {
      this.loading = false;
    }
    if (this.activeTab === 'blackboard') {
      await this.fetchBundle();
    }
  },

  async selectSession(id: string) {
    this.selectedSessionId = id;
    const response = await fetch(`/api/sessions/${encodeURIComponent(id)}`);
    this.selectedSession = await response.json();
  },

  async switchTab(tab: 'sessions' | 'blackboard') {
    this.activeTab = tab;
    if (tab === 'blackboard') {
      await this.fetchBundle();
    }
  },

  async fetchBundle() {
    this.blackboardLoading = true;
    try {
      const params = new URLSearchParams();
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
      bug: '🐛 Bugs',
      debt: '⚠️ Tech Debt',
      warning: '⚠️ Warnings',
      finding: '🔍 Findings',
      decision: '✅ Decisions',
      context: '📋 Context',
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
    if (!value) {
      return '';
    }
    return new Intl.DateTimeFormat(undefined, {
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
    }).format(new Date(value));
  },
});

window.Alpine = Alpine;
Alpine.start();
