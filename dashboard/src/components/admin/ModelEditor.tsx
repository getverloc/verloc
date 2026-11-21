import { useEffect, useRef } from 'react';
import { lookupModelHints, type AdminModel, type AdminProvider } from '../../api';
import { toast } from 'sonner';
import { Badge } from '../ui/badge';
import { Button } from '../ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '../ui/card';
import { Checkbox } from '../ui/checkbox';
import { Field, FieldDescription, FieldLabel } from '../ui/field';
import { Input } from '../ui/input';
import { Label } from '../ui/label';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '../ui/select';
import { Separator } from '../ui/separator';
import { Textarea } from '../ui/textarea';
import { Check, Search } from 'lucide-react';
import { fillModelDraftGaps, fillMetaGaps, findCatalogMatch, metaFromHint, metaFromMatch, revealForm, type EditorActions } from './shared';
import { ModelCapabilities } from './ModelCapabilities';
import type { ModelEditorState } from './useModelEditor';

type ModelEditorProps = ModelEditorState & {
  providers: AdminProvider[];
  models: AdminModel[];
  busy: boolean;
  setBusy: (busy: boolean) => void;
  setErr: EditorActions['setErr'];
  deleteModel: (id: string) => void;
};

const STRATEGY_LABELS: Record<'ordered' | 'latency' | 'weighted', string> = {
  ordered: 'Ordered (primary first)',
  latency: 'Latency (Peak EWMA)',
  weighted: 'Weighted (smooth WRR)',
};

export function ModelEditor({ editModel, setEditModel, originalModelID, saveModel, providers, models, busy, setBusy, setErr, deleteModel }: ModelEditorProps) {
  const modelFormRef = useRef<HTMLDivElement | null>(null);
  const editingExistingModel = !!originalModelID;
  useEffect(() => { revealForm(modelFormRef.current); }, [originalModelID]);
  if (!editModel) return null;
  const strategy = editModel.routing_strategy || 'ordered';
  const hasCustomRouting = strategy !== 'ordered' || Number(editModel.routing_hedge_delay_ms) > 0;
  return (
            <Card ref={modelFormRef} className="order-first min-w-0 gap-0 border-edge-600 bg-edge-900 py-0 xl:order-last">
              <CardHeader className="gap-2 border-b border-edge-700 bg-edge-950 p-5">
                <p className="text-[10px] uppercase tracking-[0.16em] text-chart-3">Model editor</p>
                <CardTitle className="break-all text-base">{editingExistingModel ? editModel.id : 'New model route'}</CardTitle>
                <div className="flex flex-wrap items-center gap-2">
                  <Badge variant="outline" className="border-chart-2/40 font-normal text-chart-2">
                    {(editModel.operations || []).length === 1
                      ? editModel.operations![0] === 'images' ? 'image' : editModel.operations![0] === 'embeddings' ? 'embedding' : 'chat'
                      : 'kind unknown / unrestricted'}
                  </Badge>
                  {(editModel.operations || []).length > 0 && <span className="text-[10px] text-muted-foreground">endpoints / {editModel.operations!.join(', ')}</span>}
                </div>
                <CardDescription className="text-[11px] leading-relaxed">Stage routing and metadata here, then save to apply live. Guided add can discover and prefill models.</CardDescription>
              </CardHeader>
              <CardContent className="space-y-5 p-5">
                <p className="border-b border-edge-700 pb-2 text-[10px] uppercase tracking-[0.14em] text-primary">01 / Identity + primary route</p>
                <Field>
                  <FieldLabel htmlFor="em-id">Verloc model id</FieldLabel>
                  <Input
                    id="em-id"
                    value={editModel.id}
                    onChange={(e) => setEditModel({ ...editModel, id: e.target.value })}
                    disabled={editingExistingModel}
                    placeholder="xx/org/model"
                    className="font-mono"
                  />
                  <FieldDescription>Short unique prefix, then /upstream-id.</FieldDescription>
                </Field>
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                  <Field>
                    <FieldLabel htmlFor="em-backend">Backend</FieldLabel>
                    <Select value={editModel.backend || ''} onValueChange={(v) => setEditModel({ ...editModel, backend: v ?? '' })}>
                      <SelectTrigger id="em-backend" aria-label="Primary model provider">
                        <SelectValue placeholder="Choose a provider…" />
                      </SelectTrigger>
                      <SelectContent>
                        {providers.map((p) => <SelectItem key={p.name} value={p.name}>{p.name}</SelectItem>)}
                      </SelectContent>
                    </Select>
                  </Field>
                  <Field>
                    <FieldLabel htmlFor="em-upstream">Upstream model id</FieldLabel>
                    <Input id="em-upstream" value={editModel.upstream || ''} onChange={(e) => setEditModel({ ...editModel, upstream: e.target.value })} className="font-mono" />
                  </Field>
                </div>
                <div className="flex flex-wrap items-center gap-3 border border-edge-700 bg-edge-950 p-3">
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={busy || !editModel.upstream?.trim()}
                    onClick={async () => {
                      const upstream = editModel.upstream?.trim() || '';
                      setBusy(true);
                      setErr(null);
                      try {
                        const result = await lookupModelHints([upstream], { provider: editModel.backend || undefined });
                        const local = metaFromMatch(upstream, findCatalogMatch(upstream, models, editModel.backend || ''));
                        const hint = result.hints[upstream];
                        if (!hint && !local.src) { setErr(`No metadata found for ${upstream}`); return; }
                        const meta = hint ? fillMetaGaps(upstream, local, metaFromHint(upstream, hint)) : local;
                        if (meta.kind && !['chat', 'images', 'embeddings', 'systemone'].includes(meta.kind)) {
                          setErr(`Unsupported model kind: ${meta.kind}. Metadata was not applied.`); return;
                        }
                        setEditModel((current) => !current || current.upstream?.trim() !== upstream || current.backend !== editModel.backend ? current : fillModelDraftGaps(current, meta));
                        toast.success(`Empty fields filled from ${meta.src}; existing values preserved`);
                      } catch (error) {
                        setErr(error instanceof Error ? error.message : 'Metadata lookup failed');
                      } finally {
                        setBusy(false);
                      }
                    }}
                  >
                    <Search className="size-3.5" />Lookup catalog
                  </Button>
                  <span className="min-w-0 basis-40 flex-1 text-[11px] leading-relaxed text-muted-foreground">Fill empty fields only. Existing values, including zero prices and explicit capability overrides, are preserved.</span>
                </div>
                <Field>
                  <FieldLabel htmlFor="em-name">Display name</FieldLabel>
                  <Input id="em-name" value={editModel.name || ''} onChange={(e) => setEditModel({ ...editModel, name: e.target.value })} />
                </Field>
                <Field>
                  <FieldLabel htmlFor="em-description">Description</FieldLabel>
                  <Textarea id="em-description" value={editModel.description || ''} onChange={(e) => setEditModel({ ...editModel, description: e.target.value })} rows={3} />
                </Field>
                <Field>
                  <FieldLabel htmlFor="em-owner">Owned by</FieldLabel>
                  <Input id="em-owner" value={editModel.owned_by || ''} onChange={(e) => setEditModel({ ...editModel, owned_by: e.target.value })} />
                </Field>
                <Field>
                  <FieldLabel htmlFor="em-tags">Tags</FieldLabel>
                  <Input id="em-tags" value={editModel.tags_text} onChange={(e) => setEditModel({ ...editModel, tags_text: e.target.value })} aria-describedby="em-tags-help" />
                  <FieldDescription id="em-tags-help">Separate tags with commas. Capabilities are edited separately below.</FieldDescription>
                </Field>
                <ModelCapabilities model={editModel} onChange={(patch) => setEditModel({ ...editModel, ...patch })} />
                <Label className="flex cursor-pointer items-center gap-2 text-xs font-normal">
                  <Checkbox checked={!!editModel.disabled} onCheckedChange={(v) => setEditModel({ ...editModel, disabled: v === true })} />
                  Disabled (preserve route)
                </Label>

                <details className="border border-edge-700 bg-edge-950 p-4" open={(editModel.routes || []).length > 0 || hasCustomRouting}>
                  <summary className="cursor-pointer text-[11px] font-medium text-chart-3">
                    02 / Routing & fallback routes · {(editModel.routes || []).length}
                    {strategy !== 'ordered' ? ` · ${strategy}` : ''}
                    {Number(editModel.routing_hedge_delay_ms) > 0 ? ` · hedge ${editModel.routing_hedge_delay_ms}ms` : ''}
                  </summary>
                  <div className="mt-3 space-y-4 border-t border-edge-700 pt-4">
                    <p className="text-[11px] leading-relaxed text-muted-foreground">
                      Choose how primary and fallback targets are ordered. Failover is bounded by gateway retry limits and never triggers after response bytes reach the client.
                    </p>
                    <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                      <Field>
                        <FieldLabel htmlFor="em-routing-strategy">Routing strategy</FieldLabel>
                        <Select
                          value={strategy}
                          onValueChange={(v) => setEditModel({
                            ...editModel,
                            routing_strategy: v === 'latency' || v === 'weighted' ? v : 'ordered',
                          })}
                        >
                          <SelectTrigger id="em-routing-strategy" aria-label="Model routing strategy">
                            <SelectValue>{STRATEGY_LABELS[strategy]}</SelectValue>
                          </SelectTrigger>
                          <SelectContent>
                            <SelectItem value="ordered">Ordered (primary first)</SelectItem>
                            <SelectItem value="latency">Latency (Peak EWMA)</SelectItem>
                            <SelectItem value="weighted">Weighted (smooth WRR)</SelectItem>
                          </SelectContent>
                        </Select>
                      </Field>
                      {strategy === 'weighted' && (
                        <Field>
                          <FieldLabel htmlFor="em-primary-weight">Primary weight</FieldLabel>
                          <Input
                            id="em-primary-weight"
                            value={editModel.routing_primary_weight ?? ''}
                            onChange={(e) => setEditModel({ ...editModel, routing_primary_weight: e.target.value })}
                            placeholder="1 (default)"
                            inputMode="numeric"
                            className="font-mono"
                          />
                          <FieldDescription>Relative share for the primary target (1–1000).</FieldDescription>
                        </Field>
                      )}
                    </div>
                    <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                      <Field>
                        <FieldLabel htmlFor="em-hedge-delay">Hedge delay (ms)</FieldLabel>
                        <Input
                          id="em-hedge-delay"
                          value={editModel.routing_hedge_delay_ms ?? ''}
                          onChange={(e) => setEditModel({ ...editModel, routing_hedge_delay_ms: e.target.value })}
                          placeholder="0 (off)"
                          inputMode="numeric"
                          className="font-mono"
                        />
                        <FieldDescription>Speculatively race the next route after this delay (non-streaming).</FieldDescription>
                      </Field>
                      <Field>
                        <FieldLabel htmlFor="em-max-hedges">Max hedges</FieldLabel>
                        <Select
                          value={editModel.routing_max_hedges || '1'}
                          onValueChange={(v) => setEditModel({ ...editModel, routing_max_hedges: v ?? '1' })}
                          disabled={!editModel.routing_hedge_delay_ms || Number(editModel.routing_hedge_delay_ms) <= 0}
                        >
                          <SelectTrigger id="em-max-hedges" aria-label="Maximum speculative hedges">
                            <SelectValue>{editModel.routing_max_hedges === '2' ? '2 backup routes' : '1 backup route (default)'}</SelectValue>
                          </SelectTrigger>
                          <SelectContent>
                            <SelectItem value="1">1 backup route (default)</SelectItem>
                            <SelectItem value="2">2 backup routes</SelectItem>
                          </SelectContent>
                        </Select>
                      </Field>
                    </div>
                    {(editModel.routes || []).map((route, index) => (
                      <div key={index} className={`grid grid-cols-1 gap-3 border-l-2 border-chart-3/40 pl-3 ${strategy === 'weighted' ? 'sm:grid-cols-[1fr_1fr_96px]' : 'sm:grid-cols-2'}`}>
                        <Field>
                          <FieldLabel htmlFor={`em-fallback-${index}-provider`}>{`Route ${index + 2} / provider`}</FieldLabel>
                          <Select
                            value={route.backend}
                            onValueChange={(v) => setEditModel({ ...editModel, routes: (editModel.routes || []).map((r, i) => i === index ? { ...r, backend: v ?? '' } : r) })}
                          >
                            <SelectTrigger id={`em-fallback-${index}-provider`} aria-label={`Fallback provider ${index + 1}`}>
                              <SelectValue placeholder="provider…" />
                            </SelectTrigger>
                            <SelectContent>
                              {providers.map((p) => <SelectItem key={p.name} value={p.name}>{p.name}</SelectItem>)}
                            </SelectContent>
                          </Select>
                        </Field>
                        <Field>
                          <FieldLabel htmlFor={`em-fallback-${index}-upstream`}>Upstream model ID</FieldLabel>
                          <Input id={`em-fallback-${index}-upstream`} value={route.upstream} onChange={(e) => setEditModel({ ...editModel, routes: (editModel.routes || []).map((r, i) => i === index ? { ...r, upstream: e.target.value } : r) })} className="font-mono" />
                        </Field>
                        {strategy === 'weighted' && (
                          <Field>
                            <FieldLabel htmlFor={`em-fallback-${index}-weight`}>Weight</FieldLabel>
                            <Input
                              id={`em-fallback-${index}-weight`}
                              value={route.weight ? String(route.weight) : ''}
                              onChange={(e) => {
                                const raw = e.target.value.trim();
                                const parsed = raw === '' ? undefined : Number(raw);
                                setEditModel({
                                  ...editModel,
                                  routes: (editModel.routes || []).map((r, i) => i === index ? { ...r, weight: Number.isFinite(parsed) ? parsed : r.weight } : r),
                                });
                              }}
                              placeholder="1"
                              inputMode="numeric"
                              className="font-mono"
                            />
                          </Field>
                        )}
                        <div className={`flex items-end ${strategy === 'weighted' ? 'sm:col-span-3' : 'sm:col-span-2'}`}>
                          <Button variant="outline" size="sm" onClick={() => setEditModel({ ...editModel, routes: (editModel.routes || []).filter((_, i) => i !== index) })}>
                            Remove fallback
                          </Button>
                        </div>
                      </div>
                    ))}
                    <Button variant="outline" size="sm" onClick={() => setEditModel({ ...editModel, routes: [...(editModel.routes || []), { backend: providers[0]?.name || '', upstream: '' }] })}>
                      Add fallback route
                    </Button>
                  </div>
                </details>

                <p className="border-b border-edge-700 pb-2 text-[10px] uppercase tracking-[0.14em] text-chart-2">03 / Context + output limits</p>
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                  <Field>
                    <FieldLabel htmlFor="em-context">{editModel.operations?.[0] === 'embeddings' ? 'Max input tokens' : 'Context window (tokens)'}</FieldLabel>
                    <Input id="em-context" value={editModel.context_window ?? ''} onChange={(e) => setEditModel({ ...editModel, context_window: e.target.value })} inputMode="numeric" className="font-mono" />
                  </Field>
                  {!(editModel.operations?.length === 1 && editModel.operations[0] === 'embeddings') && <Field>
                    <FieldLabel htmlFor="em-max-tokens">Max output tokens</FieldLabel>
                    <Input id="em-max-tokens" value={editModel.max_tokens ?? ''} onChange={(e) => setEditModel({ ...editModel, max_tokens: e.target.value })} inputMode="numeric" className="font-mono" />
                  </Field>}
                </div>
                <div className="space-y-2 border-b border-edge-700 pb-3">
                  <p className="text-[10px] uppercase tracking-[0.14em] text-chart-4">04 / Pricing per million tokens</p>
                  <p className="text-[11px] leading-relaxed text-muted-foreground">Leave prices blank when unknown. Enter zero only when the provider explicitly offers free usage. Embeddings need only an input rate; image models can use a flat image rate, exact variants, or token rates.</p>
                </div>
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                  <Field>
                    <FieldLabel htmlFor="em-price-input">Price in $/1M</FieldLabel>
                    <Input id="em-price-input" value={editModel.pricing_input ?? ''} onChange={(e) => setEditModel({ ...editModel, pricing_input: e.target.value })} inputMode="decimal" className="font-mono" />
                  </Field>
                  {!(editModel.operations?.length === 1 && editModel.operations[0] === 'embeddings') && <Field>
                    <FieldLabel htmlFor="em-price-output">Out $/1M</FieldLabel>
                    <Input id="em-price-output" value={editModel.pricing_output ?? ''} onChange={(e) => setEditModel({ ...editModel, pricing_output: e.target.value })} inputMode="decimal" className="font-mono" />
                  </Field>}
                  <Field>
                    <FieldLabel htmlFor="em-price-cache">Cache $/1M</FieldLabel>
                    <Input id="em-price-cache" value={editModel.pricing_cache ?? ''} onChange={(e) => setEditModel({ ...editModel, pricing_cache: e.target.value })} inputMode="decimal" className="font-mono" />
                  </Field>
                  <Field>
                    <FieldLabel htmlFor="em-price-image">Image $/image</FieldLabel>
                    <Input id="em-price-image" value={editModel.pricing_image ?? ''} onChange={(e) => setEditModel({ ...editModel, pricing_image: e.target.value })} inputMode="decimal" className="font-mono" />
                    <FieldDescription>Flat rate per returned image. Blank removes this rate; zero is explicitly free.</FieldDescription>
                  </Field>
                </div>
                <div className="space-y-2 border-b border-edge-700 pb-3">
                  <p className="text-[10px] uppercase tracking-[0.14em] text-chart-5">05 / Endpoints</p>
                  <p className="text-[11px] leading-relaxed text-muted-foreground">No selection allows every endpoint. Restrict when a provider exposes only some operations.</p>
                </div>
                <div className="flex flex-wrap gap-5">
                  {(['chat', 'embeddings', 'images'] as const).map((op) => (
                    <label key={op} className="flex items-center gap-2 text-xs text-edge-100">
                      <Checkbox
                        checked={(editModel.operations || []).includes(op)}
                        onCheckedChange={(checked) => {
                          const next = new Set(editModel.operations || []);
                          if (checked) next.add(op); else next.delete(op);
                          setEditModel({ ...editModel, operations: Array.from(next) });
                        }}
                      />
                      {op}
                    </label>
                  ))}
                </div>
                <details className="border border-edge-700 bg-edge-950 p-4">
                  <summary className="cursor-pointer text-[11px] font-medium text-chart-4">Image pricing · variants and token breakdown</summary>
                  <div className="mt-3 space-y-4 border-t border-edge-700 pt-4">
                    {editModel.image_prices?.unsupported_conditions && <div role="alert" className="space-y-2 text-xs text-chart-4">
                      <p>Catalog pricing has unsupported or conflicting conditions. Calculated image cost stays unknown until you review and replace those rates.</p>
                      <Button variant="outline" size="sm" onClick={() => setEditModel({ ...editModel, image_prices: { ...editModel.image_prices, unsupported_conditions: false } })}>Use reviewed manual rates</Button>
                    </div>}
                    <p className="text-[11px] leading-relaxed text-muted-foreground">Pricing precedence: opted-in provider USD cost, most-specific size/quality variant, flat image rate, then token rates. Methods are alternatives, not added together. Missing quantities or unmatched/ambiguous variants stay unknown. A blank condition is unconstrained; a stated condition must match the request exactly.</p>
                    {(editModel.image_prices?.variants || []).map((variant, index) => (
                      <div key={index} className="grid min-w-0 grid-cols-1 gap-3 border-l-2 border-chart-4/40 pl-3 sm:grid-cols-3">
                        {(['size', 'quality', 'price'] as const).map((field) => (
                          <Field key={field}>
                            <FieldLabel htmlFor={`em-variant-${index}-${field}`}>{field === 'price' ? 'USD / image' : field}</FieldLabel>
                            <Input id={`em-variant-${index}-${field}`} value={variant[field]} onChange={(e) => setEditModel({ ...editModel, image_prices: { ...editModel.image_prices, variants: (editModel.image_prices?.variants || []).map((v, i) => i === index ? { ...v, [field]: e.target.value } : v) } })} />
                          </Field>
                        ))}
                        <Button variant="outline" size="sm" onClick={() => setEditModel({ ...editModel, image_prices: { ...editModel.image_prices, variants: (editModel.image_prices?.variants || []).filter((_, i) => i !== index) } })}>Remove variant</Button>
                      </div>
                    ))}
                    <Button variant="outline" size="sm" onClick={() => setEditModel({ ...editModel, image_prices: { ...editModel.image_prices, variants: [...(editModel.image_prices?.variants || []), { size: '', quality: '', price: '' }] } })}>Add image price variant</Button>
                    <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                      {(['text_input', 'image_input', 'text_output', 'image_output'] as const).map((field) => (
                        <Field key={field}>
                          <FieldLabel htmlFor={`em-image-${field}`}>{field.replace('_', ' ')} $/1M tokens</FieldLabel>
                          <Input id={`em-image-${field}`} inputMode="decimal" value={editModel.image_prices?.[field] ?? ''} onChange={(e) => setEditModel({ ...editModel, image_prices: { ...editModel.image_prices, [field]: e.target.value.trim() || undefined } })} />
                        </Field>
                      ))}
                    </div>
                    <p className="text-[11px] text-muted-foreground">Breakdown rates require matching upstream text/image token details. Cached breakdowns are not estimated.</p>
                    <Label className="flex cursor-pointer items-start gap-2 text-xs font-normal">
                      <Checkbox checked={!!editModel.image_prices?.provider_cost} onCheckedChange={(v) => setEditModel({ ...editModel, image_prices: { ...editModel.image_prices, provider_cost: v === true } })} />
                      Trust upstream usage.cost as USD (only when documented by this provider)
                    </Label>
                  </div>
                </details>
                <Separator />
                <div className="flex flex-wrap items-center justify-between gap-3 pt-1">
                  <div className="flex flex-wrap gap-2">
                    <Button disabled={busy} onClick={saveModel}><Check className="size-3.5" />Save · live apply</Button>
                    <Button variant="outline" onClick={() => setEditModel(null)}>Cancel</Button>
                  </div>
                  {editingExistingModel && (
                    <Button variant="destructive" disabled={busy} onClick={() => deleteModel(editModel.id)}>
                      delete model
                    </Button>
                  )}
                </div>
              </CardContent>
            </Card>
  );
}
