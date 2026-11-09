import type { AdminModel, ModelHint } from '../../api';

export type WizMeta = {
  dn: string; description: string; owned_by: string; tags: string[];
  pi: string; po: string; pc: string; ctx: string; mx: string; src: string;
};

export const emptyMeta = (upstream: string): WizMeta => ({
  dn: upstream, description: '', owned_by: '', tags: [],
  pi: '', po: '', pc: '', ctx: '', mx: '', src: '',
});

function existingPrefixes(providers: { name: string }[], models: { id: string }[]): Set<string> {
  const s = new Set<string>();
  for (const p of providers) if (p.name) s.add(p.name);
  for (const m of models) {
    const i = m.id.indexOf('/');
    if (i > 0) s.add(m.id.slice(0, i));
  }
  return s;
}

export function uniquePrefix(providerName: string, providers: { name: string }[], models: { id: string }[]): string {
  const letters = (providerName || '').toLowerCase().replace(/[^a-z0-9]/g, '') || 'xx';
  const taken = existingPrefixes(providers, models);
  const cands = [letters.slice(0, 2) || 'xx', letters.slice(0, 3), letters.slice(0, 4)];
  for (const c of cands) if (c && !taken.has(c)) return c;
  const base = letters.slice(0, 2) || 'xx';
  let suffix = 1;
  while (taken.has(`${base}${suffix}`)) suffix++;
  return `${base}${suffix}`;
}

export function suggestVerlocId(providerName: string, upstream: string, providers: { name: string }[] = [], models: { id: string }[] = []): string {
  const up = (upstream || '').replace(/^\/+/, '');
  return `${uniquePrefix(providerName, providers, models)}/${up}`;
}

export function findCatalogMatch(upstream: string, models: AdminModel[], backend: string): AdminModel | null {
  const normalized = upstream.trim().toLowerCase().replace(/:free$/, '-free');
  const matches = models.filter((model) =>
    model.backend === backend && model.upstream.trim().toLowerCase().replace(/:free$/, '-free') === normalized);
  if (matches.length === 0) return null;
  const first = matches[0];
  // Aliases with conflicting prices cannot establish reliable metadata.
  return matches.every((model) =>
    model.pricing_known === first.pricing_known &&
    model.pricing_input === first.pricing_input && model.pricing_output === first.pricing_output &&
    model.pricing_cache === first.pricing_cache &&
    model.context_window === first.context_window && model.max_tokens === first.max_tokens) ? first : null;
}

export function metaFromMatch(upstream: string, match: AdminModel | null): WizMeta {
  if (!match) return emptyMeta(upstream);
  return {
    dn: match.name || upstream,
    description: match.description || '',
    owned_by: match.owned_by || '',
    tags: match.tags || [],
    pi: match.pricing_known ? String(match.pricing_input) : '',
    po: match.pricing_known ? String(match.pricing_output) : '',
    pc: match.pricing_known ? String(match.pricing_cache) : '',
    ctx: match.context_window > 0 ? String(match.context_window) : '',
    mx: match.max_tokens > 0 ? String(match.max_tokens) : '',
    src: 'catalog',
  };
}

export function metaFromHint(upstream: string, hint: ModelHint): WizMeta {
  return {
    dn: hint.name || upstream,
    description: hint.description || '',
    owned_by: hint.owned_by || '',
    tags: hint.tags || [],
    pi: hint.pricing_input !== undefined ? String(hint.pricing_input) : '',
    po: hint.pricing_output !== undefined ? String(hint.pricing_output) : '',
    pc: hint.pricing_cache !== undefined ? String(hint.pricing_cache) : '',
    ctx: (hint.context_window || 0) > 0 ? String(hint.context_window) : '',
    mx: (hint.max_tokens || 0) > 0 ? String(hint.max_tokens) : '',
    src: hint.source || 'catalog',
  };
}

export function fillMetaGaps(upstream: string, current: WizMeta, hint: WizMeta): WizMeta {
  return {
    dn: current.dn !== upstream ? current.dn : hint.dn,
    description: current.description || hint.description,
    owned_by: current.owned_by || hint.owned_by,
    tags: current.tags.length ? current.tags : hint.tags,
    pi: current.pi || hint.pi, po: current.po || hint.po, pc: current.pc || hint.pc,
    ctx: current.ctx || hint.ctx, mx: current.mx || hint.mx,
    src: current.src === hint.src ? current.src : [current.src, hint.src].filter(Boolean).join(' + '),
  };
}

export function firstProbeKey(keysText: string): { api_key: string; account_id?: string } {
  const line = (keysText || '').split('\n').map((s) => s.trim()).find(Boolean) || '';
  if (!line) return { api_key: '' };
  return parseKeyLine(line);
}

export function parseKeyLine(line: string): { api_key: string; account_id?: string } {
  const i = line.indexOf(':');
  if (i > 0 && !line.startsWith('sk-') && !line.startsWith('Bearer')) {
    const left = line.slice(0, i);
    const right = line.slice(i + 1);
    if (left.length <= 64 && !left.includes(' ')) {
      return { account_id: left, api_key: right };
    }
  }
  return { api_key: line };
}

// Scroll the main pane only, and only when the form is actually off screen.
export function revealForm(el: HTMLElement | null) {
  if (!el) return;
  const scroller = el.closest('main');
  const top = scroller ? scroller.getBoundingClientRect().top : 0;
  const bottom = scroller ? scroller.getBoundingClientRect().bottom : window.innerHeight;
  const r = el.getBoundingClientRect();
  if (r.top >= top && r.top <= bottom - 48) return;
  el.scrollIntoView({ behavior: 'auto', block: 'start' });
}

export type PendingConfirm = { title: string; body: string; confirmLabel: string; run: () => void };

export type MutationRunner = (fn: () => Promise<void>, success: string) => Promise<void>;
export type EditorActions = {
  run: MutationRunner;
  setErr: (message: string | null) => void;
  setConfirm: (confirmation: PendingConfirm) => void;
};
