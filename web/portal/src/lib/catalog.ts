import type { CatalogAddon } from './types';

// Compares dotted numeric version strings (e.g. "3.13.1" vs "3.12.2"). Non-numeric
// segments compare as 0 so odd formats degrade gracefully instead of throwing.
export function compareVersions(a: string, b: string): number {
  const pa = a.split('.').map((n) => parseInt(n, 10) || 0);
  const pb = b.split('.').map((n) => parseInt(n, 10) || 0);
  const len = Math.max(pa.length, pb.length);
  for (let i = 0; i < len; i++) {
    const diff = (pa[i] ?? 0) - (pb[i] ?? 0);
    if (diff !== 0) return diff;
  }
  return 0;
}

const STATUS_RANK: Record<string, number> = { supported: 2, deprecated: 1, eol: 0 };

// The catalog lists one entry per available add-on *version* (bundles pin specific ones
// via `supersedes` chains), so the same add-on name can appear multiple times. Pickers
// that just toggle "is this add-on installed" want one row per name - collapse to the
// most-supported, newest version.
export function dedupeAddons(addons: CatalogAddon[]): CatalogAddon[] {
  const byName = new Map<string, CatalogAddon>();
  for (const a of addons) {
    const cur = byName.get(a.name);
    if (!cur) {
      byName.set(a.name, a);
      continue;
    }
    const statusDiff = (STATUS_RANK[a.status] ?? 0) - (STATUS_RANK[cur.status] ?? 0);
    if (statusDiff > 0 || (statusDiff === 0 && compareVersions(a.version, cur.version) > 0)) {
      byName.set(a.name, a);
    }
  }
  return [...byName.values()];
}
