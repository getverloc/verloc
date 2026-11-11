import { useEffect, useMemo, useState } from 'react';
import { discoverUpstreamModels, lookupModelHints, onboard, testUpstreamModel, type AdminModel, type AdminProvider, type DiscoveredModel, type ModelHint, type ProbeConn, type TestResult } from '../../api';
import { toast } from 'sonner';
import { Badge } from '../ui/badge';
import { Button } from '../ui/button';
import { Checkbox } from '../ui/checkbox';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '../ui/dialog';
import { Field, FieldDescription, FieldLabel } from '../ui/field';
import { Input } from '../ui/input';
import { Label } from '../ui/label';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '../ui/select';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '../ui/table';
import { Textarea } from '../ui/textarea';
import { Tooltip, TooltipContent, TooltipTrigger } from '../ui/tooltip';
import { ArrowRight, Check, FlaskConical, Search } from 'lucide-react';
import { emptyMeta, fillMetaGaps, findCatalogMatch, firstProbeKey, metaFromHint, metaFromMatch, parseKeyLine, suggestVerlocId, uniquePrefix, type PendingConfirm, type WizMeta } from './shared';

type OnboardingWizardProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  providers: AdminProvider[];
  models: AdminModel[];
  busy: boolean;
  refresh: () => Promise<void>;
  setConfirm: (confirmation: PendingConfirm) => void;
};

export function OnboardingWizard({ open: wizOpen, onOpenChange: setWizOpen, providers, models, busy, refresh, setConfirm }: OnboardingWizardProps) {
  const [wizStep, setWizStep] = useState<1 | 2 | 3>(1);
  const [wUseExisting, setWUseExisting] = useState('');
  const [wName, setWName] = useState('');
  const [wBase, setWBase] = useState('');
  const [wAPIFormat, setWAPIFormat] = useState<'openai' | 'systemone'>('openai');
  const [wKeysText, setWKeysText] = useState('');
  const [wNoAuth, setWNoAuth] = useState(false);
  const [wAllowPrivate, setWAllowPrivate] = useState(false);
  const [wSynth, setWSynth] = useState(false);
  const [wStrip, setWStrip] = useState(true);
  const [wCooldown, setWCooldown] = useState('30');
  const [wCap, setWCap] = useState('');
  const [wTimeout, setWTimeout] = useState('');
  const [wAuthH, setWAuthH] = useState('');
  const [wAuthF, setWAuthF] = useState('');
  const [wDiscovered, setWDiscovered] = useState<DiscoveredModel[]>([]);
  const [wFetchBusy, setWFetchBusy] = useState(false);
  const [wFetchErr, setWFetchErr] = useState<string | null>(null);
  const [wFilter, setWFilter] = useState('');
  const [wManual, setWManual] = useState('');
  const [wSel, setWSel] = useState<Record<string, boolean>>({});
  const [wIds, setWIds] = useState<Record<string, string>>({});
  const [wMeta, setWMeta] = useState<Record<string, WizMeta>>({});
  const [wTests, setWTests] = useState<Record<string, TestResult>>({});
  const [wTesting, setWTesting] = useState<Record<string, boolean>>({});
  const [wSaving, setWSaving] = useState(false);

  useEffect(() => { if (wizOpen) setWFetchErr(null); }, [wizOpen]);

  // ---- Wizard helpers (staged inputs; nothing is saved until review) ----
  const wizProviderName = wUseExisting || wName.trim();
  const wizProbeConn = (): ProbeConn => {
    if (wUseExisting) return { provider: wUseExisting, allow_private_upstreams: wAllowPrivate };
    const fk = firstProbeKey(wKeysText);
    return {
      base_url: wBase.trim(),
      api_format: wAPIFormat,
      api_key: fk.api_key,
      account_id: fk.account_id,
      auth_header: wAuthH || undefined,
      auth_header_format: wAuthF || undefined,
      no_auth: wNoAuth,
      synthesize_cli_headers: wSynth,
      allow_private_upstreams: wAllowPrivate,
    };
  };

  const wizEnsureRow = (upstream: string, providerName: string, hint?: ModelHint) => {
    const id = suggestVerlocId(providerName, upstream, providers, models);
    const local = findCatalogMatch(upstream, models, providerName);
    const base = metaFromMatch(upstream, local);
    const external = hint ? metaFromHint(upstream, hint) : null;
    const meta = external ? fillMetaGaps(upstream, base, external) : base;
    setWIds((prev) => (prev[upstream] ? prev : { ...prev, [upstream]: id }));
    setWMeta((prev) => ({
      ...prev, [upstream]: prev[upstream] ? fillMetaGaps(upstream, prev[upstream], meta) : meta,
    }));
  };

  const wizLookupRow = async (upstream: string) => {
    setWFetchBusy(true);
    try {
      const result = await lookupModelHints([upstream]);
      if (result.hints[upstream]) wizEnsureRow(upstream, wizProviderName, result.hints[upstream]);
    } catch (error) {
      setWFetchErr(error instanceof Error ? error.message : 'Metadata lookup failed');
    } finally {
      setWFetchBusy(false);
    }
  };

  const wizFetch = async () => {
    setWFetchErr(null);
    const name = wizProviderName;
    if (!wUseExisting && (!wName.trim() || !wBase.trim())) {
      setWFetchErr('provider name and base_url required');
      return;
    }
    if (!wUseExisting && !wNoAuth && !firstProbeKey(wKeysText).api_key) {
      setWFetchErr('paste at least one API key (or tick no_auth)');
      return;
    }
    setWFetchBusy(true);
    try {
      const res = await discoverUpstreamModels(wizProbeConn());
      setWDiscovered(res.models || []);
      const sel: Record<string, boolean> = {};
      (res.models || []).forEach((m) => {
        sel[m.id] = wSel[m.id] ?? (!m.hint?.type || ['chat', 'images', 'embeddings', 'systemone'].includes(m.hint.type));
        wizEnsureRow(m.id, name || 'xx', m.hint);
      });
      setWSel((prev) => ({ ...sel, ...prev }));
      setWizStep(2);
    } catch (e) {
      setWFetchErr((e as Error).message);
    } finally {
      setWFetchBusy(false);
    }
  };

  const wizSelectedUpstreams = useMemo(
    () => Object.keys(wSel).filter((u) => wSel[u]),
    [wSel],
  );

  const wizFiltered = useMemo(() => {
    const q = wFilter.trim().toLowerCase();
    if (!q) return wDiscovered;
    return wDiscovered.filter((m) => `${m.id} ${m.owned_by || ''} ${m.hint?.type || ''} ${(m.hint?.tags || []).join(' ')}`.toLowerCase().includes(q));
  }, [wDiscovered, wFilter]);

  const wizAllFilteredSelected = wizFiltered.length > 0 && wizFiltered.every((m) => !!wSel[m.id]);

  const wizTestRow = (upstream: string) => {
    const meta = wMeta[upstream] || emptyMeta(upstream);
    if (!['chat', 'images', 'embeddings', 'systemone'].includes(meta.kind)) {
      setWTests((p) => ({
        ...p,
        [upstream]: { ok: false, status: 0, latency_ms: 0, error: meta.kind ? `Detected kind “${meta.kind}” is not served by verloc` : 'Choose a model kind before testing' },
      }));
      return;
    }
    const operation = meta.kind === 'images' || meta.kind === 'embeddings' ? meta.kind : 'chat';
    const run = () => {
      const conn = wizProbeConn();
      setWTesting((p) => ({ ...p, [upstream]: true }));
      testUpstreamModel({ ...conn, upstream, operation, confirm_image_spend: operation === 'images' })
        .then((res) => setWTests((p) => ({ ...p, [upstream]: res })))
        .catch((e) => setWTests((p) => ({
          ...p, [upstream]: { ok: false, status: 0, latency_ms: 0, error: (e as Error).message },
        })))
        .finally(() => setWTesting((p) => ({ ...p, [upstream]: false })));
    };
    if (operation === 'images') {
      setConfirm({
        title: `Generate one test image with ${upstream}?`,
        body: 'This sends a real image request to the provider and may consume credits. One image is generated, validated, and discarded — never stored or returned.',
        confirmLabel: 'Generate test image',
        run,
      });
      return;
    }
    run();
  };

  const doWizBulkSave = async () => {
    const backend = wizProviderName;
    const rows = wizSelectedUpstreams;
    setWSaving(true);
    setWFetchErr(null);
    try {
      const stagedProvider = wUseExisting ? undefined : {
        name: wName.trim(), base_url: wBase.trim(), auth_header: wAuthH,
        api_format: wAPIFormat,
        auth_header_format: wAuthF, no_auth: wNoAuth, synthesize_cli_headers: wSynth,
        allow_private_upstreams: wAllowPrivate,
        strip_reasoning_content: wStrip, cooldown_seconds: Number(wCooldown) || 0,
        request_timeout_seconds: Number(wTimeout) || 0, max_tokens: Number(wCap) || 0,
        keys: wKeysText.split('\n').map((s) => s.trim()).filter(Boolean).map((line) => {
          const parsed = parseKeyLine(line);
          return parsed.account_id ? { account_id: parsed.account_id, api_key: parsed.api_key } : parsed.api_key;
        }),
      };
      const collisions = rows.filter((u) => models.some((m) => m.id === wIds[u]?.trim()));
      const providerCollision = !wUseExisting && providers.some((p) => p.name === wName.trim());
      await onboard({
        provider: stagedProvider,
        overwrite: collisions.length > 0 || providerCollision,
        models: rows.map((upstream) => {
          const meta = wMeta[upstream] || emptyMeta(upstream);
          const id = wIds[upstream].trim();
          return {
            id, backend, upstream, name: meta.dn.trim() || id,
            description: meta.description, owned_by: meta.owned_by, tags: meta.tags,
            context_window: Number(meta.ctx) || 0,
            max_tokens: Number(meta.mx) || 0, ...(meta.pi.trim() ? { pricing_input: Number(meta.pi) } : {}),
            ...(meta.po.trim() ? { pricing_output: Number(meta.po) } : {}), ...(meta.pc.trim() ? { pricing_cache: Number(meta.pc) } : {}),
            ...(meta.piImage.trim() ? { pricing_image: Number(meta.piImage) } : {}),
            ...(meta.imagePrices ? { image_prices: meta.imagePrices } : {}),
            ...(meta.ops.length ? { operations: meta.ops } : {}),
            kind: meta.kind,
            modalities: meta.modalities, attachment: meta.attachment, reasoning: meta.reasoning, temperature: meta.temperature,
          };
        }),
      });
      setWKeysText('');
      setWName(''); setWBase(''); setWUseExisting(''); setWDiscovered([]);
      setWAPIFormat('openai');
      setWAllowPrivate(false);
      setWSel({}); setWIds({}); setWMeta({}); setWTests({});
      toast.success(`${rows.length} model${rows.length > 1 ? 's' : ''} saved · live apply`);
      setWizOpen(false);
      setWizStep(1);
    } catch (e) {
      setWFetchErr((e as Error).message);
    } finally {
      setWSaving(false);
    }
    void refresh();
  };

  const wizBulkSave = () => {
    const backend = wizProviderName;
    if (!backend) {
      setWFetchErr('Choose or enter a provider first');
      setWizStep(1);
      return;
    }
    if (!wUseExisting && (!wBase.trim() || (!wNoAuth && !firstProbeKey(wKeysText).api_key))) {
      setWFetchErr('Enter a provider endpoint and at least one credential, or explicitly enable keyless access.');
      setWizStep(1);
      return;
    }
    const rows = wizSelectedUpstreams;
    if (rows.length === 0) {
      setWFetchErr('select at least one model');
      setWizStep(2);
      return;
    }
    const unsupported = rows.filter((u) => {
      const kind = wMeta[u]?.kind;
      return !kind || !['chat', 'images', 'embeddings', 'systemone'].includes(kind);
    });
    if (unsupported.length > 0) {
      setWFetchErr(`Choose a supported model kind, or remove these rows before saving: ${unsupported.join(', ')}`);
      setWizStep(3);
      return;
    }
    for (const u of rows) {
      const id = (wIds[u] || '').trim();
      if (!id) {
        setWFetchErr(`verloc id missing for ${u}`);
        return;
      }
    }
    const collisions = rows.filter((u) => models.some((m) => m.id === wIds[u]?.trim()));
    const providerCollision = !wUseExisting && providers.some((p) => p.name === wName.trim());
    if (collisions.length > 0 || providerCollision) {
      setConfirm({
        title: `Overwrite ${collisions.length} model route${collisions.length === 1 ? '' : 's'}${providerCollision ? ' and the existing provider' : ''}?`,
        body: 'This changes live application routing. Provider and models apply together in one transaction.',
        confirmLabel: 'Save · live apply',
        run: () => { void doWizBulkSave(); },
      });
      return;
    }
    void doWizBulkSave();
  };

  return (
      <Dialog open={wizOpen} onOpenChange={setWizOpen}>
        <DialogContent className="max-h-[90dvh] overflow-y-auto border-edge-600 bg-edge-900 p-4 sm:max-w-5xl sm:p-6">
          <DialogHeader className="border-b border-edge-700 pb-5">
            <p className="text-[10px] uppercase tracking-[0.18em] text-primary">Transactional onboarding</p>
            <DialogTitle className="text-lg">Connect an upstream. Publish your models.</DialogTitle>
            <DialogDescription className="max-w-2xl text-xs leading-relaxed">
              Discovery and testing use staged inputs without saving. Provider and model changes apply
              together in one transaction only after the final review.
            </DialogDescription>
          </DialogHeader>

          <div className="grid grid-cols-3 gap-px border border-edge-700 bg-edge-700">
            {([
              { title: 'Connection', description: 'Provider + credentials' },
              { title: 'Select models', description: `${wizSelectedUpstreams.length} selected` },
              { title: 'Review + apply', description: 'Metadata + live tests' },
            ] as const).map(({ title, description }, i) => {
              const step = (i + 1) as 1 | 2 | 3;
              const active = wizStep === step;
              return (
                <button
                  key={title}
                  type="button"
                  aria-current={active ? 'step' : undefined}
                  onClick={() => setWizStep(step)}
                  className={`min-w-0 border-t-2 p-3 text-left transition-colors focus-visible:outline-2 focus-visible:outline-primary sm:p-4 ${active ? 'border-primary bg-primary/10 text-primary' : 'border-transparent bg-edge-950 text-muted-foreground hover:bg-edge-800 hover:text-foreground'}`}
                >
                  <span className="mb-2 block text-[10px] tabular-nums">{String(step).padStart(2, '0')}</span>
                  <span className="block text-xs">{title}</span>
                  <span className="mt-1 hidden text-[10px] text-muted-foreground sm:block">{description}</span>
                </button>
              );
            })}
          </div>

          {wFetchErr && (
            <div role="alert" className="border border-destructive/40 bg-destructive/10 px-3 py-3 text-xs break-words text-destructive">
              {wFetchErr}
            </div>
          )}

          {wizStep === 1 && (
            <div className="space-y-5">
              <Field>
                <FieldLabel htmlFor="wiz-provider">Provider</FieldLabel>
                <Select value={wUseExisting} onValueChange={(v) => { setWUseExisting(v === '__new' ? '' : (v ?? '')); setWFetchErr(null); }}>
                  <SelectTrigger id="wiz-provider" aria-label="Existing provider or new provider">
                    <SelectValue placeholder="+ new provider…" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="__new">+ new provider…</SelectItem>
                    {providers.map((p) => <SelectItem key={p.name} value={p.name}>{p.name} · {p.base_url}</SelectItem>)}
                  </SelectContent>
                </Select>
                {wUseExisting
                  ? <FieldDescription>Using saved provider keys server-side — no need to paste keys again. Fetch lists its live upstream models.</FieldDescription>
                  : <FieldDescription>Connect an OpenAI-compatible or System One endpoint, then fetch its live model list.</FieldDescription>}
              </Field>

              {!wUseExisting && (
                <>
                  <div className="flex items-center gap-3 border-b border-edge-700 pb-2 text-[10px] uppercase tracking-[0.14em]">
                    <span className="text-primary">01 / Endpoint</span><span className="text-muted-foreground">Provider connection</span>
                  </div>

                  <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                    <Field>
                      <FieldLabel htmlFor="wiz-name">Name</FieldLabel>
                      <Input id="wiz-name" value={wName} onChange={(e) => setWName(e.target.value)} placeholder="acme" className="font-mono" />
                      <FieldDescription>Lowercase, no / : space.</FieldDescription>
                    </Field>
                    <Field>
                      <FieldLabel htmlFor="wiz-base">Base URL</FieldLabel>
                      <Input id="wiz-base" value={wBase} onChange={(e) => setWBase(e.target.value)} placeholder="https://api.example.com/v1" className="font-mono" />
                      <FieldDescription>Usually ends in /v1.</FieldDescription>
                    </Field>
                  </div>
                  <Field>
                    <FieldLabel htmlFor="wiz-api-format">API format</FieldLabel>
                    <Select value={wAPIFormat} onValueChange={(value) => setWAPIFormat(value === 'systemone' ? 'systemone' : 'openai')}>
                      <SelectTrigger id="wiz-api-format" aria-label="New provider API format">
                        <SelectValue>{wAPIFormat === 'systemone' ? 'System One (typed questions)' : 'OpenAI-compatible'}</SelectValue>
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="openai">OpenAI-compatible</SelectItem>
                        <SelectItem value="systemone">System One (typed questions)</SelectItem>
                      </SelectContent>
                    </Select>
                    <FieldDescription>Jev uses System One. Cloudflare stays OpenAI-compatible; Clef is routed natively per model.</FieldDescription>
                  </Field>
                  <p className="border-l-2 border-chart-4 bg-chart-4/5 px-3 py-2 text-[11px] leading-relaxed text-chart-4">Use an http(s):// base URL, usually ending in /v1. Review your endpoint before saving.</p>

                  <div className="flex items-center gap-3 border-b border-edge-700 pb-2 text-[10px] uppercase tracking-[0.14em]">
                    <span className="text-chart-2">02 / Credentials</span><span className="text-muted-foreground">Staged until final save</span>
                  </div>
                  <Field>
                    <FieldLabel htmlFor="wiz-keys">API keys</FieldLabel>
                    <Textarea
                      id="wiz-keys"
                      value={wKeysText}
                      onChange={(e) => setWKeysText(e.target.value)}
                      placeholder="sk-…"
                      rows={3}
                      className="font-mono"
                    />
                    <FieldDescription>One per line; or account_id:api_key for Cloudflare-style credentials. Staged only — stored encrypted on save.</FieldDescription>
                  </Field>

                  <details className="border border-edge-700 bg-edge-950 px-4 py-3">
                    <summary className="cursor-pointer text-[11px] font-medium text-chart-3">Advanced / authentication, limits, and protocol options</summary>
                    <div className="space-y-4 border-t border-edge-700 pt-4 mt-3">
                      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                        <Field>
                          <FieldLabel htmlFor="wiz-auth-header">Auth header</FieldLabel>
                          <Input id="wiz-auth-header" value={wAuthH} onChange={(e) => setWAuthH(e.target.value)} placeholder="Authorization (default)" />
                        </Field>
                        <Field>
                          <FieldLabel htmlFor="wiz-auth-format">Auth format (%s = key)</FieldLabel>
                          <Input id="wiz-auth-format" value={wAuthF} onChange={(e) => setWAuthF(e.target.value)} placeholder="Bearer %s (default)" />
                        </Field>
                      </div>
                      <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
                        <Field>
                          <FieldLabel htmlFor="wiz-cooldown">Cooldown sec</FieldLabel>
                          <Input id="wiz-cooldown" value={wCooldown} onChange={(e) => setWCooldown(e.target.value)} inputMode="numeric" className="font-mono" />
                        </Field>
                        <Field>
                          <FieldLabel htmlFor="wiz-max-tokens">Max tokens cap (0 = none)</FieldLabel>
                          <Input id="wiz-max-tokens" value={wCap} onChange={(e) => setWCap(e.target.value)} inputMode="numeric" className="font-mono" />
                        </Field>
                        <Field>
                          <FieldLabel htmlFor="wiz-timeout">Timeout sec (0 = none)</FieldLabel>
                          <Input id="wiz-timeout" value={wTimeout} onChange={(e) => setWTimeout(e.target.value)} inputMode="numeric" className="font-mono" />
                        </Field>
                      </div>
                      <div className="flex flex-wrap gap-4 text-xs">
                        <Label className="flex cursor-pointer items-center gap-1.5 font-normal">
                          <Checkbox checked={wNoAuth} onCheckedChange={(v) => setWNoAuth(v === true)} />
                          <Tooltip>
                            <TooltipTrigger><span>no_auth (keyless)</span></TooltipTrigger>
                            <TooltipContent>Skip credential headers entirely — for public or keyless endpoints like OpenCode.</TooltipContent>
                          </Tooltip>
                        </Label>
                        <Label className="flex cursor-pointer items-center gap-1.5 font-normal">
                          <Checkbox checked={wSynth} onCheckedChange={(v) => setWSynth(v === true)} />
                          synthesize_cli_headers
                        </Label>
                        <Label className="flex cursor-pointer items-center gap-1.5 font-normal">
                          <Checkbox checked={wStrip} onCheckedChange={(v) => setWStrip(v === true)} />
                          strip_reasoning_content
                        </Label>
                      </div>
                    </div>
                  </details>
                </>
              )}
              <div className="space-y-2 border border-edge-700 bg-edge-950 p-4 text-xs">
                <Label className="flex cursor-pointer items-center gap-2 font-normal">
                  <Checkbox checked={wAllowPrivate} onCheckedChange={(v) => setWAllowPrivate(v === true)} />
                  Allow private / LAN upstream
                </Label>
                <p className="text-[11px] leading-relaxed text-muted-foreground">Enable only for an intentionally local endpoint. Applies to probes and new providers; existing providers also honor their saved permission.</p>
              </div>

              <DialogFooter className="border-t border-edge-700 pt-4">
                <Button
                  variant="outline"
                  disabled={wFetchBusy}
                  onClick={() => {
                    if (!wizProviderName || (!wUseExisting && !wBase.trim())) {
                      setWFetchErr('Enter a provider name and endpoint first.');
                      return;
                    }
                    setWizStep(2);
                  }}
                >
                  Enter models manually
                </Button>
                <Button disabled={wFetchBusy || busy} onClick={() => { void wizFetch(); }}>
                  {wFetchBusy ? 'fetching…' : 'fetch models →'}
                </Button>
              </DialogFooter>
            </div>
          )}

          {wizStep === 2 && (
            <div className="space-y-5">
              <div className="flex flex-wrap items-center justify-between gap-3 border-b border-edge-700 pb-4">
                <div className="relative min-w-0 flex-1 sm:max-w-sm">
                  <Search className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground" />
                  <Input aria-label="Filter upstream models" value={wFilter} onChange={(e) => setWFilter(e.target.value)} placeholder="Filter upstream models…" className="w-full pl-8" />
                </div>
                <span className="text-[10px] uppercase tracking-wide text-muted-foreground"><span className="text-primary">{wizSelectedUpstreams.length} selected</span> / {wDiscovered.length} discovered</span>
              </div>

              {wDiscovered.length === 0 && (
                <p className="border border-dashed border-edge-600 bg-edge-950 p-5 text-xs leading-relaxed text-muted-foreground">No discovery results yet. Fetch models from the connection step, or enter upstream IDs below when discovery is unavailable.</p>
              )}

              <div className="overflow-x-auto border border-edge-700 bg-edge-950">
                <Table className="min-w-[680px]">
                  <TableHeader>
                    <TableRow>
                      <TableHead className="w-10">
                        <Checkbox
                          aria-label="Select or unselect all filtered models"
                          checked={wizAllFilteredSelected}
                          onCheckedChange={(v) => {
                            const on = v === true;
                            if (on) wizFiltered.forEach((m) => wizEnsureRow(m.id, wizProviderName || 'xx', m.hint));
                            setWSel((prev) => {
                              const next = { ...prev };
                              wizFiltered.forEach((m) => { next[m.id] = on; });
                              return next;
                            });
                          }}
                        />
                      </TableHead>
                      <TableHead>Upstream model</TableHead>
                      <TableHead>Verloc id</TableHead>
                      <TableHead>Display name</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {wizFiltered.map((m) => {
                      const checked = !!wSel[m.id];
                      const idVal = wIds[m.id] || '';
                      const dup = models.some((x) => x.id === idVal.trim() && idVal.trim() !== '');
                      return (
                        <TableRow key={m.id}>
                          <TableCell>
                            <Checkbox
                              aria-label={`Select upstream model ${m.id}`}
                              checked={checked}
                              onCheckedChange={(v) => {
                                const on = v === true;
                                setWSel((p) => ({ ...p, [m.id]: on }));
                                if (on) wizEnsureRow(m.id, wizProviderName || 'xx', m.hint);
                              }}
                            />
                          </TableCell>
                          <TableCell>
                            <div className="max-w-64 break-all text-[11px] text-chart-2">{m.id}</div>
                            {m.owned_by && <div className="text-[10px] text-muted-foreground">{m.owned_by}</div>}
                            <div className="mt-1 flex flex-wrap items-center gap-1.5">
                              <Badge variant="outline" className="border-chart-2/40 font-normal normal-case text-chart-2">
                                {m.hint?.type === 'images' ? 'image' : m.hint?.type === 'embeddings' ? 'embedding' : m.hint?.type === 'chat' ? 'chat' : m.hint?.type ? `unsupported / ${m.hint.type}` : 'kind unknown'}
                              </Badge>
                              {m.hint?.pricing_image !== undefined && <span className="text-[10px] text-chart-4">${m.hint.pricing_image}/img</span>}
                              {m.hint?.pricing_input !== undefined && <span className="text-[10px] text-muted-foreground">${m.hint.pricing_input}/1M in</span>}
                            </div>
                          </TableCell>
                          <TableCell>
                            {checked ? (
                              <div className="flex min-w-44 items-center gap-2">
                                <Input
                                  value={idVal}
                                  onChange={(e) => setWIds((p) => ({ ...p, [m.id]: e.target.value }))}
                                  placeholder={`${uniquePrefix(wizProviderName || 'xx', providers, models)}/${m.id}`}
                                  aria-label={`verloc id for ${m.id}`}
                                  className="font-mono text-[11px]"
                                />
                                {dup && <Badge variant="outline" className="shrink-0 border-chart-4/40 text-chart-4">exists · overwrites</Badge>}
                              </div>
                            ) : (
                              <span className="text-[11px] text-muted-foreground">—</span>
                            )}
                          </TableCell>
                          <TableCell>
                            {checked ? (
                              <Input
                                value={(wMeta[m.id] || emptyMeta(m.id)).dn}
                                onChange={(e) => setWMeta((p) => ({
                                  ...p,
                                  [m.id]: { ...(p[m.id] || emptyMeta(m.id)), dn: e.target.value },
                                }))}
                                placeholder="display name"
                                aria-label={`display name for ${m.id}`}
                                className="min-w-32 text-[11px]"
                              />
                            ) : (
                              <span className="text-[11px] text-muted-foreground">—</span>
                            )}
                          </TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              </div>

              <div className="flex flex-wrap items-end gap-3 border border-edge-700 bg-edge-950 p-4">
                <Field className="min-w-0 basis-56 flex-1">
                  <FieldLabel htmlFor="wiz-manual-upstream">Manual upstream ID</FieldLabel>
                  <Input id="wiz-manual-upstream" value={wManual} onChange={(e) => setWManual(e.target.value)} placeholder="org/model-id" className="font-mono" />
                </Field>
                <Button
                  variant="outline"
                  disabled={wFetchBusy}
                  onClick={() => {
                    const u = wManual.trim();
                    if (!u) return;
                    setWDiscovered((p) => (p.some((x) => x.id === u) ? p : [...p, { id: u }]));
                    setWSel((p) => ({ ...p, [u]: true }));
                    wizEnsureRow(u, wizProviderName || 'xx');
                    void wizLookupRow(u);
                    setWManual('');
                  }}
                >
                  + add row
                </Button>
              </div>
              <p className="text-[11px] leading-relaxed text-muted-foreground">Model IDs receive an editable collision-free prefix. Catalog matches and external hints prefill available fields; blank pricing remains unknown, while explicit zero means free. Existing IDs require confirmation before overwrite.</p>

              <DialogFooter className="border-t border-edge-700 pt-4">
                <Button variant="outline" onClick={() => setWizStep(1)}>← provider</Button>
                <Button disabled={wFetchBusy || wizSelectedUpstreams.length === 0} onClick={() => setWizStep(3)}>
                  review {wizSelectedUpstreams.length} →
                </Button>
              </DialogFooter>
            </div>
          )}

          {wizStep === 3 && (
            <div className="space-y-5">
              {wizSelectedUpstreams.length === 0 && (
                <p className="text-xs text-muted-foreground">Nothing selected — go back and tick at least one model.</p>
              )}
              {wizSelectedUpstreams.map((u) => {
                const meta = wMeta[u] || emptyMeta(u);
                const setMeta = (patch: Partial<WizMeta>) => {
                  setWFetchErr(null);
                  setWMeta((p) => ({ ...p, [u]: { ...meta, ...patch } }));
                };
                const t = wTests[u];
                const testing = !!wTesting[u];
                return (
                  <div key={u} className="space-y-4 border border-edge-700 bg-edge-950 p-4">
                    <div className="flex flex-wrap items-start justify-between gap-3 border-b border-edge-700 pb-3">
                      <div className="min-w-0 flex-1 basis-56">
                        <div className="break-all text-xs text-primary">{wIds[u] || u}</div>
                        <div className="mt-1 flex items-start gap-2 text-[11px] text-muted-foreground"><ArrowRight className="mt-0.5 size-3 shrink-0" /><span className="break-all">{wizProviderName}:{u}</span></div>
                        <div className="mt-2 flex flex-wrap items-center gap-2">
                          <Badge variant="outline" className="border-chart-2/40 font-normal text-chart-2">
                            {meta.kind === 'images' ? 'image generation' : meta.kind === 'embeddings' ? 'embeddings' : meta.kind === 'chat' ? 'chat' : meta.kind === 'systemone' ? 'System One' : meta.kind ? `unsupported / ${meta.kind}` : 'kind not detected'}
                          </Badge>
                          {meta.ops.length > 0 && <span className="text-[10px] text-muted-foreground">endpoints / {meta.ops.join(', ')}</span>}
                          {meta.src !== '' && <span className="text-[10px] text-chart-3">source / {meta.src}</span>}
                        </div>
                      </div>
                      <div className="flex flex-wrap items-center gap-2">
                        {t && (t.ok
                          ? <Badge variant="outline" className="border-primary/40 text-primary"><Check className="size-3" />{t.kind === 'images' ? `${t.images_known ? t.images : '?'} image` : t.kind === 'embeddings' ? `${t.dimensions || '?'} dims` : 'reply'} · {t.latency_ms}ms</Badge>
                          : <Badge variant="outline" className="border-destructive/40 text-destructive">fail · {t.status || 'err'}</Badge>)}
                        <Button variant="outline" size="xs" disabled={testing} onClick={() => wizTestRow(u)}>
                          <FlaskConical className="size-3" />{testing ? 'Testing…' : 'Test upstream'}
                        </Button>
                        <Button variant="ghost" size="xs" onClick={() => { setWFetchErr(null); setWSel((p) => ({ ...p, [u]: false })); }}>remove</Button>
                      </div>
                    </div>
                    {meta.kind && !['chat', 'images', 'embeddings', 'systemone'].includes(meta.kind) && (
                      <div className="border border-destructive/40 bg-destructive/10 px-3 py-2 text-xs break-words text-destructive">
                        Detected kind “{meta.kind}” is not served by verloc. Remove this row before saving.
                      </div>
                    )}
                    {(!meta.kind || ['chat', 'images', 'embeddings'].includes(meta.kind)) && <Field>
                      <FieldLabel htmlFor={`wiz-${u}-kind`}>Model kind / test endpoint</FieldLabel>
                      <Select value={meta.kind} onValueChange={(value) => {
                        if (value) { setMeta({ kind: value, ops: [value] }); setWTests((p) => { const next = { ...p }; delete next[u]; return next; }); }
                      }}>
                        <SelectTrigger id={`wiz-${u}-kind`}><SelectValue placeholder="Choose kind before saving or testing">{meta.kind === 'images' ? 'Image generation / editing' : meta.kind === 'embeddings' ? 'Embeddings' : meta.kind === 'chat' ? 'Chat' : undefined}</SelectValue></SelectTrigger>
                        <SelectContent>
                          <SelectItem value="chat">Chat</SelectItem><SelectItem value="embeddings">Embeddings</SelectItem><SelectItem value="images">Image generation / editing</SelectItem>
                        </SelectContent>
                      </Select>
                    </Field>}
                    {meta.modalities && <p className="break-words text-[11px] text-muted-foreground">Modalities / input: {meta.modalities.input?.join(', ') || 'unknown'} · output: {meta.modalities.output?.join(', ') || 'unknown'}</p>}
                    {meta.imagePrices?.unsupported_conditions && <p className="text-xs text-chart-4">Unsupported or conflicting catalog price conditions. Image cost remains unknown until reviewed in the model editor.</p>}
                    {t && !t.ok && (
                      <div className="border border-destructive/40 bg-destructive/10 px-3 py-2 text-xs break-words text-destructive">
                        {t.error || 'failed'}{t.hint ? ` — ${t.hint}` : ''}
                      </div>
                    )}
                    {t?.ok && t.reply && (
                      <div className="text-[10px] text-muted-foreground">reply: <span className="font-mono">{t.reply}</span></div>
                    )}
                    <Field>
                      <FieldLabel htmlFor={`wiz-${u}-name`}>Display name</FieldLabel>
                      <Input id={`wiz-${u}-name`} value={meta.dn} onChange={(e) => setMeta({ dn: e.target.value })} placeholder={u} />
                    </Field>
                    <Field>
                      <FieldLabel htmlFor={`wiz-${u}-description`}>Description</FieldLabel>
                      <Textarea id={`wiz-${u}-description`} value={meta.description} onChange={(e) => setMeta({ description: e.target.value })} rows={2} />
                    </Field>
                    <Field>
                      <FieldLabel htmlFor={`wiz-${u}-tags`}>Capabilities / tags</FieldLabel>
                      <Input id={`wiz-${u}-tags`} value={meta.tags.join(', ')} onChange={(e) => setMeta({ tags: e.target.value.split(',').map((tag) => tag.trim()).filter(Boolean) })} />
                    </Field>
                    <div className="grid grid-cols-2 gap-2 sm:grid-cols-6">
                      <Field>
                        <FieldLabel htmlFor={`wiz-${u}-pi`}>In $/1M</FieldLabel>
                        <Input id={`wiz-${u}-pi`} value={meta.pi} onChange={(e) => setMeta({ pi: e.target.value })} inputMode="decimal" className="font-mono" />
                      </Field>
                      {meta.kind !== 'embeddings' && <Field>
                        <FieldLabel htmlFor={`wiz-${u}-po`}>Out $/1M</FieldLabel>
                        <Input id={`wiz-${u}-po`} value={meta.po} onChange={(e) => setMeta({ po: e.target.value })} inputMode="decimal" className="font-mono" />
                      </Field>}
                      <Field>
                        <FieldLabel htmlFor={`wiz-${u}-pc`}>Cache $/1M</FieldLabel>
                        <Input id={`wiz-${u}-pc`} value={meta.pc} onChange={(e) => setMeta({ pc: e.target.value })} inputMode="decimal" className="font-mono" />
                      </Field>
                      {(meta.kind === 'images' || !meta.kind) && <Field>
                        <FieldLabel htmlFor={`wiz-${u}-pimg`}>Image $/img</FieldLabel>
                        <Input id={`wiz-${u}-pimg`} value={meta.piImage} onChange={(e) => setMeta({ piImage: e.target.value })} inputMode="decimal" className="font-mono" />
                      </Field>}
                      <Field>
                        <FieldLabel htmlFor={`wiz-${u}-ctx`}>{meta.kind === 'embeddings' ? 'Max input tokens' : 'Context tokens'}</FieldLabel>
                        <Input id={`wiz-${u}-ctx`} value={meta.ctx} onChange={(e) => setMeta({ ctx: e.target.value })} inputMode="numeric" className="font-mono" />
                      </Field>
                      {meta.kind !== 'embeddings' && <Field>
                        <FieldLabel htmlFor={`wiz-${u}-mx`}>Max out</FieldLabel>
                        <Input id={`wiz-${u}-mx`} value={meta.mx} onChange={(e) => setMeta({ mx: e.target.value })} inputMode="numeric" className="font-mono" />
                      </Field>}
                    </div>
                    {meta.imagePrices?.variants?.length ? (
                      <p className="text-[10px] leading-relaxed text-muted-foreground">
                        {meta.imagePrices.variants.length} size/quality price variant{meta.imagePrices.variants.length === 1 ? '' : 's'} detected; review them in the model editor after saving.
                      </p>
                    ) : null}
                  </div>
                );
              })}
              {wizSelectedUpstreams.filter((u) => models.some((m) => m.id === (wIds[u] || '').trim() && (wIds[u] || '').trim() !== '')).length > 0 && (
                <p className="border-l-2 border-chart-4 bg-chart-4/5 px-3 py-2 text-xs text-chart-4">Existing model IDs will be explicitly confirmed before transactional overwrite.</p>
              )}
              {wizSelectedUpstreams.filter((u) => { const mt = wMeta[u]; return !mt || ((Number(mt.pi) || 0) === 0 && (Number(mt.po) || 0) === 0 && (Number(mt.piImage) || 0) === 0); }).length > 0 && (
                <p className="text-xs text-muted-foreground">Some prices are zero or unset. Review against provider pricing; unknown cost is not free usage.</p>
              )}
              <DialogFooter className="border-t border-edge-700 pt-4">
                <Button variant="outline" onClick={() => setWizStep(2)}>← pick</Button>
                <Button disabled={wSaving || wFetchBusy || wizSelectedUpstreams.length === 0} onClick={() => wizBulkSave()}>
                  {wSaving ? 'saving…' : `save ${wizSelectedUpstreams.length} model${wizSelectedUpstreams.length === 1 ? '' : 's'} · live apply`}
                </Button>
              </DialogFooter>
            </div>
          )}
        </DialogContent>
      </Dialog>
  );
}
