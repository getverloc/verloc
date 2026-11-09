import { useEffect, useRef } from 'react';
import { lookupModelHints, type AdminModel, type AdminProvider } from '../../api';
import { toast } from 'sonner';
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
import { fillMetaGaps, findCatalogMatch, metaFromHint, metaFromMatch, revealForm, type EditorActions } from './shared';
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

export function ModelEditor({ editModel, setEditModel, originalModelID, saveModel, providers, models, busy, setBusy, setErr, deleteModel }: ModelEditorProps) {
  const modelFormRef = useRef<HTMLDivElement | null>(null);
  const editingExistingModel = !!originalModelID;
  useEffect(() => { revealForm(modelFormRef.current); }, [originalModelID]);
  if (!editModel) return null;
  return (
            <Card ref={modelFormRef} className="order-first min-w-0 gap-0 border-edge-600 bg-edge-900 py-0 xl:order-last">
              <CardHeader className="gap-2 border-b border-edge-700 bg-edge-950 p-5">
                <p className="text-[10px] uppercase tracking-[0.16em] text-chart-3">Model editor</p>
                <CardTitle className="break-all text-base">{editingExistingModel ? editModel.id : 'New model route'}</CardTitle>
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
                        const result = await lookupModelHints([upstream]);
                        const local = metaFromMatch(upstream, findCatalogMatch(upstream, models, editModel.backend || ''));
                        const hint = result.hints[upstream];
                        if (!hint && !local.src) { setErr(`No metadata found for ${upstream}`); return; }
                        const meta = hint ? fillMetaGaps(upstream, metaFromHint(upstream, hint), local) : local;
                        setEditModel((current) => !current || current.upstream?.trim() !== upstream ? current : {
                          ...current, name: meta.dn, description: meta.description, owned_by: meta.owned_by,
                          tags: meta.tags, tags_text: meta.tags.join(', '), context_window: meta.ctx, max_tokens: meta.mx,
                          pricing_input: meta.pi, pricing_output: meta.po, pricing_cache: meta.pc,
                        });
                        toast.success(`Metadata loaded from ${meta.src}`);
                      } catch (error) {
                        setErr(error instanceof Error ? error.message : 'Metadata lookup failed');
                      } finally {
                        setBusy(false);
                      }
                    }}
                  >
                    <Search className="size-3.5" />Lookup catalog
                  </Button>
                  <span className="min-w-0 basis-40 flex-1 text-[11px] leading-relaxed text-muted-foreground">Match the upstream ID to prefill name, limits, and known pricing.</span>
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

                <details className="border border-edge-700 bg-edge-950 p-4" open={(editModel.routes || []).length > 0}>
                  <summary className="cursor-pointer text-[11px] font-medium text-chart-3">02 / Ordered fallback routes · {(editModel.routes || []).length}</summary>
                  <div className="mt-3 space-y-4 border-t border-edge-700 pt-4">
                    <p className="text-[11px] leading-relaxed text-muted-foreground">The primary route is tried first. Additional routes are bounded by gateway retry limits and never used after streaming begins.</p>
                    {(editModel.routes || []).map((route, index) => (
                      <div key={index} className="grid grid-cols-1 gap-3 border-l-2 border-chart-3/40 pl-3 sm:grid-cols-2">
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
                        <div className="flex items-end sm:col-span-2">
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
                    <FieldLabel htmlFor="em-context">Context window</FieldLabel>
                    <Input id="em-context" value={editModel.context_window ?? ''} onChange={(e) => setEditModel({ ...editModel, context_window: e.target.value })} inputMode="numeric" className="font-mono" />
                  </Field>
                  <Field>
                    <FieldLabel htmlFor="em-max-tokens">Max output tokens</FieldLabel>
                    <Input id="em-max-tokens" value={editModel.max_tokens ?? ''} onChange={(e) => setEditModel({ ...editModel, max_tokens: e.target.value })} inputMode="numeric" className="font-mono" />
                  </Field>
                </div>
                <div className="space-y-2 border-b border-edge-700 pb-3">
                  <p className="text-[10px] uppercase tracking-[0.14em] text-chart-4">04 / Pricing per million tokens</p>
                  <p className="text-[11px] leading-relaxed text-muted-foreground">Leave prices blank when unknown. Enter zero only when the provider explicitly offers free usage.</p>
                </div>
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
                  <Field>
                    <FieldLabel htmlFor="em-price-input">Price in $/1M</FieldLabel>
                    <Input id="em-price-input" value={editModel.pricing_input ?? ''} onChange={(e) => setEditModel({ ...editModel, pricing_input: e.target.value })} inputMode="decimal" className="font-mono" />
                  </Field>
                  <Field>
                    <FieldLabel htmlFor="em-price-output">Out $/1M</FieldLabel>
                    <Input id="em-price-output" value={editModel.pricing_output ?? ''} onChange={(e) => setEditModel({ ...editModel, pricing_output: e.target.value })} inputMode="decimal" className="font-mono" />
                  </Field>
                  <Field>
                    <FieldLabel htmlFor="em-price-cache">Cache $/1M</FieldLabel>
                    <Input id="em-price-cache" value={editModel.pricing_cache ?? ''} onChange={(e) => setEditModel({ ...editModel, pricing_cache: e.target.value })} inputMode="decimal" className="font-mono" />
                  </Field>
                </div>
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
