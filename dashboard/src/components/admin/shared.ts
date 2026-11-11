import type { AdminModel, ImagePricing, ModelHint } from '../../api';

export type WizMeta = {
  dn: string; description: string; owned_by: string; tags: string[];
  pi: string; po: string; pc: string; piImage: string; ctx: string; mx: string; src: string;
  kind: string; ops: string[]; imagePrices?: ImagePricing;
  modalities?: AdminModel['modalities']; attachment?: boolean; reasoning?: boolean; temperature?: boolean;
};

export const emptyMeta = (upstream: string): WizMeta => ({
  dn: upstream, description: '', owned_by: '', tags: [],
  pi: '', po: '', pc: '', piImage: '', ctx: '', mx: '', src: '', kind: '', ops: [],
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
  const normalized = upstream.trim().toLowerCase();
  const matches = models.filter((model) =>
    model.backend === backend && model.upstream.trim().toLowerCase() === normalized);
  if (matches.length === 0) return null;
  const first = matches[0];
  // Aliases with conflicting prices, kinds, or operation scopes cannot
  // establish reliable metadata.
  return matches.every((model) =>
    model.pricing_known === first.pricing_known &&
    model.pricing_input === first.pricing_input && model.pricing_output === first.pricing_output &&
    model.pricing_cache === first.pricing_cache &&
    (model.pricing_image ?? null) === (first.pricing_image ?? null) &&
    model.kind === first.kind &&
    JSON.stringify(model.image_prices) === JSON.stringify(first.image_prices) &&
    JSON.stringify(model.operations || []) === JSON.stringify(first.operations || []) &&
    model.context_window === first.context_window && model.max_tokens === first.max_tokens) ? first : null;
}

export function metaFromMatch(upstream: string, match: AdminModel | null): WizMeta {
  if (!match) return emptyMeta(upstream);
  return {
    dn: match.name || upstream,
    description: match.description || '',
    owned_by: match.owned_by || '',
    tags: match.tags || [],
    pi: hasPriceField(match, 'input') ? String(match.pricing_input) : '',
    po: hasPriceField(match, 'output') ? String(match.pricing_output) : '',
    pc: hasPriceField(match, 'cache_read') ? String(match.pricing_cache) : '',
    piImage: match.pricing_image != null ? String(match.pricing_image) : '',
    imagePrices: match.image_prices,
    kind: match.kind || (match.operations?.length === 1 ? match.operations[0] : ''),
    ops: match.operations || [],
    modalities: match.modalities, attachment: match.attachment, reasoning: match.reasoning, temperature: match.temperature,
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
    piImage: hint.pricing_image !== undefined ? String(hint.pricing_image) : '',
    imagePrices: hint.image_prices,
    kind: hint.type || (hint.operations?.length === 1 ? hint.operations[0] : ''),
    ops: hint.operations || [],
    modalities: hint.modalities, attachment: hint.attachment, reasoning: hint.reasoning, temperature: hint.temperature,
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
    piImage: current.piImage || hint.piImage,
    imagePrices: current.imagePrices || hint.imagePrices,
    kind: current.kind || hint.kind,
    ops: current.ops.length ? current.ops : hint.ops,
    modalities: { ...hint.modalities, ...current.modalities },
    attachment: current.attachment ?? hint.attachment,
    reasoning: current.reasoning ?? hint.reasoning,
    temperature: current.temperature ?? hint.temperature,
    ctx: current.ctx || hint.ctx, mx: current.mx || hint.mx,
    src: current.src === hint.src ? current.src : [current.src, hint.src].filter(Boolean).join(' + '),
  };
}

export function hasPriceField(model: AdminModel, field: string): boolean {
  return model.pricing_fields ? model.pricing_fields.includes(field) : model.pricing_known;
}

// Lookup fills gaps in the current draft, including edits made while it was loading.
export function fillModelDraftGaps<T extends {
  name?: string; description?: string; owned_by?: string; tags_text: string;
  context_window?: string; max_tokens?: string; pricing_input?: string; pricing_output?: string;
  pricing_cache?: string; pricing_image?: string; image_prices?: ImagePricing; operations?: string[];
  modalities?: AdminModel['modalities']; attachment?: boolean; reasoning?: boolean; temperature?: boolean;
}>(current: T, meta: WizMeta): T {
  return {
    ...current, name: current.name || meta.dn, description: current.description || meta.description,
    owned_by: current.owned_by || meta.owned_by, tags_text: current.tags_text || meta.tags.join(', '),
    context_window: current.context_window || meta.ctx, max_tokens: current.max_tokens || meta.mx,
    pricing_input: current.pricing_input || meta.pi, pricing_output: current.pricing_output || meta.po,
    pricing_cache: current.pricing_cache || meta.pc, pricing_image: current.pricing_image || meta.piImage,
    image_prices: current.image_prices ?? meta.imagePrices,
    operations: current.operations ?? (meta.ops.length ? meta.ops : undefined),
    modalities: { ...meta.modalities, ...current.modalities },
    attachment: current.attachment ?? meta.attachment, reasoning: current.reasoning ?? meta.reasoning,
    temperature: current.temperature ?? meta.temperature,
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
