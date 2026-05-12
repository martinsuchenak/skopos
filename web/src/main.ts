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

declare global {
  interface Window {
    Alpine: typeof Alpine;
    app: () => {
      sessions: SessionSummary[];
      selectedSession: SessionDetail | null;
      selectedSessionId: string;
      loading: boolean;
      init: () => void;
      refresh: () => Promise<void>;
      selectSession: (id: string) => Promise<void>;
      statusClass: (status?: string) => string;
      formatTime: (value: string) => string;
    };
  }
}

window.app = () => ({
  sessions: [],
  selectedSession: null,
  selectedSessionId: '',
  loading: false,
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
  },
  async selectSession(id: string) {
    this.selectedSessionId = id;
    const response = await fetch(`/api/sessions/${encodeURIComponent(id)}`);
    this.selectedSession = await response.json();
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
