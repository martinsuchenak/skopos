// Times in the dashboard: a recent one relative ("12m ago", "3h ago"), an
// older one as a day and time or a date ("yesterday 14:05", "Mon 14:05",
// "12 Nov 14:05", "12 Nov 2025"), and the exact time with its time zone on
// hover (whenFull). The browser's locale and time zone decide the format.

const HOUR_MIN: Intl.DateTimeFormatOptions = { hour: '2-digit', minute: '2-digit' };

function startOfDay(d: Date, offsetDays = 0): number {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate() + offsetDays).getTime();
}

function fmt(d: Date, opts: Intl.DateTimeFormatOptions): string {
  return new Intl.DateTimeFormat(undefined, opts).format(d);
}

// whenText: when something happened (or, for a time ahead, will happen),
// as short as it can be while still saying when.
export function whenText(iso: string, now: Date = new Date()): string {
  if (!iso) return '';
  const d = new Date(iso);
  const t = d.getTime();
  if (Number.isNaN(t)) return '';
  const hm = fmt(d, HOUR_MIN);
  if (t > now.getTime() + 60_000) {
    // Ahead (a schedule's next run): the day it falls on.
    if (t < startOfDay(now, 1)) return 'today ' + hm;
    if (t < startOfDay(now, 2)) return 'tomorrow ' + hm;
    if (t < startOfDay(now, 7)) return fmt(d, { weekday: 'short' }) + ' ' + hm;
    return dateText(d, now, hm);
  }
  const s = Math.max(0, (now.getTime() - t) / 1000);
  if (s < 60) return 'just now';
  if (s < 3600) return Math.floor(s / 60) + 'm ago';
  if (t >= startOfDay(now)) return Math.floor(s / 3600) + 'h ago';
  if (t >= startOfDay(now, -1)) return 'yesterday ' + hm;
  if (t >= startOfDay(now, -6)) return fmt(d, { weekday: 'short' }) + ' ' + hm;
  return dateText(d, now, hm);
}

// dateText: "12 Nov 14:05" this year, "12 Nov 2025" before it.
function dateText(d: Date, now: Date, hm: string): string {
  if (d.getFullYear() === now.getFullYear()) return fmt(d, { day: 'numeric', month: 'short' }) + ' ' + hm;
  return fmt(d, { day: 'numeric', month: 'short', year: 'numeric' });
}

// whenFull: the exact time, for a title: "Thu, 20 Nov 2026, 14:05:32 CET".
export function whenFull(iso: string): string {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return fmt(d, { weekday: 'short', day: 'numeric', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit', timeZoneName: 'short' });
}

// waitedText: how long since iso, as a span: "under a minute", "6m",
// "3h 12m", "2d 4h".
export function waitedText(iso: string, now: Date = new Date()): string {
  if (!iso) return '';
  const ms = Math.max(0, now.getTime() - new Date(iso).getTime());
  if (Number.isNaN(ms)) return '';
  if (ms < 60_000) return 'under a minute';
  return spanText(ms);
}

// spanText: a length of time in ms as "45s", "6m", "3h 12m", "2d 4h".
export function spanText(ms: number): string {
  let s = Math.max(0, ms / 1000);
  if (Number.isNaN(s)) return '';
  if (s < 60) return Math.round(s) + 's';
  const d = Math.floor(s / 86400); s -= d * 86400;
  const h = Math.floor(s / 3600); s -= h * 3600;
  const m = Math.floor(s / 60);
  if (d) return d + 'd' + (h ? ' ' + h + 'h' : '');
  if (h) return h + 'h' + (m ? ' ' + m + 'm' : '');
  return m + 'm';
}
