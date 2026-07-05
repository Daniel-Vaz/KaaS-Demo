// Small presentation helpers.

import dayjs from 'dayjs';
import relativeTime from 'dayjs/plugin/relativeTime';

dayjs.extend(relativeTime);

export function relative(ts: string | Date): string {
  return dayjs(ts).fromNow();
}

export function timeOfDay(ts: string | Date): string {
  return dayjs(ts).format('HH:mm:ss');
}

export function gib(mb: number): string {
  return (mb / 1024).toFixed(mb % 1024 === 0 ? 0 : 1);
}

export function pct(used: number, total: number): number {
  if (total <= 0) return 0;
  return Math.min(100, Math.round((used / total) * 100));
}

// cores renders CPU millicores as cores: 250 → "0.25", 2000 → "2".
export function cores(milli: number): string {
  const c = milli / 1000;
  return c.toFixed(Number.isInteger(c) ? 0 : 2);
}

// gibBytes renders a byte count as GiB: 2147483648 → "2", 3.2e9 → "3.0".
export function gibBytes(bytes: number): string {
  const g = bytes / (1024 * 1024 * 1024);
  return g.toFixed(g >= 10 || Number.isInteger(g) ? 0 : 1);
}

// duration renders a span of seconds compactly: "45s", "2m 10s", "1h 5m".
export function duration(seconds: number): string {
  const s = Math.max(0, Math.round(seconds));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return s % 60 ? `${m}m ${s % 60}s` : `${m}m`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

// secondsBetween returns the non-negative gap in seconds between two timestamps (b - a).
export function secondsBetween(a: string | Date, b: string | Date): number {
  return Math.max(0, (dayjs(b).valueOf() - dayjs(a).valueOf()) / 1000);
}

// secondsSince returns how many seconds have elapsed since ts (>= 0).
export function secondsSince(ts: string | Date): number {
  return Math.max(0, (Date.now() - dayjs(ts).valueOf()) / 1000);
}
