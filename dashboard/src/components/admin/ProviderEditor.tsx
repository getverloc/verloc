import { useEffect, useRef } from 'react';
import { deleteAdminProviderKey, listAdminProviders } from '../../api';
import { Button } from '../ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '../ui/card';
import { Checkbox } from '../ui/checkbox';
import { Field, FieldDescription, FieldLabel } from '../ui/field';
import { Input } from '../ui/input';
import { Label } from '../ui/label';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '../ui/select';
import { Textarea } from '../ui/textarea';
import { Tooltip, TooltipContent, TooltipTrigger } from '../ui/tooltip';
import { Check } from 'lucide-react';
import { revealForm, type EditorActions } from './shared';
import type { ProviderEditorState } from './useProviderEditor';

type ProviderEditorProps = ProviderEditorState & Pick<EditorActions, 'run' | 'setConfirm'> & { busy: boolean };

export function ProviderEditor({ editProvider, setEditProvider, originalProviderName, saveProvider, busy, run, setConfirm }: ProviderEditorProps) {
  const providerFormRef = useRef<HTMLDivElement | null>(null);
  const editingExistingProvider = !!originalProviderName;
  useEffect(() => { revealForm(providerFormRef.current); }, [originalProviderName]);
  if (!editProvider) return null;
  return (
            <Card ref={providerFormRef} className="order-first min-w-0 gap-0 border-edge-600 bg-edge-900 py-0 xl:order-last">
              <CardHeader className="gap-2 border-b border-edge-700 bg-edge-950 p-5">
                <p className="text-[10px] uppercase tracking-[0.16em] text-chart-2">Connection editor</p>
                <CardTitle className="break-all text-base">{editingExistingProvider ? editProvider.name : 'New provider'}</CardTitle>
                <CardDescription className="text-[11px] leading-relaxed">{editingExistingProvider ? 'Connection settings apply live on save. Stored credentials are never exposed.' : 'Staged until saved. No connection or credential is stored before you apply.'}</CardDescription>
              </CardHeader>
              <CardContent className="space-y-5 p-5">
                <p className="border-b border-edge-700 pb-2 text-[10px] uppercase tracking-[0.14em] text-primary">01 / Identity + endpoint</p>
                <Field>
                  <FieldLabel htmlFor="ep-name">Name</FieldLabel>
                  <Input id="ep-name" value={editProvider.name || ''} onChange={(e) => setEditProvider({ ...editProvider, name: e.target.value })} disabled={editingExistingProvider} className="font-mono" />
                </Field>
                <Field>
                  <FieldLabel htmlFor="ep-base">Base URL</FieldLabel>
                  <Input id="ep-base" value={editProvider.base_url || ''} onChange={(e) => setEditProvider({ ...editProvider, base_url: e.target.value })} placeholder="https://api.example.com/v1" className="font-mono" />
                </Field>
                <Field>
                  <FieldLabel htmlFor="ep-api-format">API format</FieldLabel>
                  <Select value={editProvider.api_format || 'openai'} onValueChange={(value) => setEditProvider({ ...editProvider, api_format: value === 'systemone' ? 'systemone' : 'openai' })}>
                    <SelectTrigger id="ep-api-format" aria-label="Provider API format">
                      <SelectValue>{editProvider.api_format === 'systemone' ? 'System One (typed questions)' : 'OpenAI-compatible'}</SelectValue>
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="openai">OpenAI-compatible</SelectItem>
                      <SelectItem value="systemone">System One (typed questions)</SelectItem>
                    </SelectContent>
                  </Select>
                </Field>
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                  <Field>
                    <FieldLabel htmlFor="ep-auth-header">Auth header</FieldLabel>
                    <Input id="ep-auth-header" value={editProvider.auth_header || ''} onChange={(e) => setEditProvider({ ...editProvider, auth_header: e.target.value })} placeholder="Authorization (default)" />
                  </Field>
                  <Field>
                    <FieldLabel htmlFor="ep-auth-format">Auth format (%s = key)</FieldLabel>
                    <Input id="ep-auth-format" value={editProvider.auth_header_format || ''} onChange={(e) => setEditProvider({ ...editProvider, auth_header_format: e.target.value })} placeholder="Bearer %s (default)" />
                  </Field>
                </div>
                <p className="border-b border-edge-700 pb-2 text-[10px] uppercase tracking-[0.14em] text-chart-3">02 / Limits + protocol</p>
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
                  <Field>
                    <FieldLabel htmlFor="ep-cooldown">Cooldown sec</FieldLabel>
                    <Input id="ep-cooldown" value={editProvider.cooldown_seconds ?? ''} onChange={(e) => setEditProvider({ ...editProvider, cooldown_seconds: e.target.value })} inputMode="numeric" className="font-mono" />
                  </Field>
                  <Field>
                    <FieldLabel htmlFor="ep-timeout">Timeout sec</FieldLabel>
                    <Input id="ep-timeout" value={editProvider.request_timeout_seconds ?? ''} onChange={(e) => setEditProvider({ ...editProvider, request_timeout_seconds: e.target.value })} inputMode="numeric" className="font-mono" />
                  </Field>
                  <Field>
                    <FieldLabel htmlFor="ep-max-tokens">Max tokens cap</FieldLabel>
                    <Input id="ep-max-tokens" value={editProvider.max_tokens ?? ''} onChange={(e) => setEditProvider({ ...editProvider, max_tokens: e.target.value })} inputMode="numeric" className="font-mono" />
                  </Field>
                </div>
                <div className="grid gap-3 border border-edge-700 bg-edge-950 p-4 text-[11px]">
                  <Label className="flex cursor-pointer items-center gap-1.5 font-normal">
                    <Checkbox checked={!!editProvider.disabled} onCheckedChange={(v) => setEditProvider({ ...editProvider, disabled: v === true })} />
                    Disabled (preserve configuration)
                  </Label>
                  <Label className="flex cursor-pointer items-center gap-1.5 font-normal">
                    <Checkbox checked={!!editProvider.allow_private_upstreams} onCheckedChange={(v) => setEditProvider({ ...editProvider, allow_private_upstreams: v === true })} />
                    Allow private / LAN upstream
                  </Label>
                  <p className="leading-relaxed text-muted-foreground">Enable only for an intentionally local endpoint. This permission is saved with the provider.</p>
                  <Label className="flex cursor-pointer items-center gap-1.5 font-normal">
                    <Checkbox checked={!!editProvider.no_auth} onCheckedChange={(v) => setEditProvider({ ...editProvider, no_auth: v === true })} />
                    <Tooltip>
                      <TooltipTrigger><span>no_auth</span></TooltipTrigger>
                      <TooltipContent>Keyless endpoint — no credential headers are sent.</TooltipContent>
                    </Tooltip>
                  </Label>
                  <Label className="flex cursor-pointer items-center gap-1.5 font-normal">
                    <Checkbox checked={!!editProvider.synthesize_cli_headers} onCheckedChange={(v) => setEditProvider({ ...editProvider, synthesize_cli_headers: v === true })} />
                    synthesize_cli_headers
                  </Label>
                  <Label className="flex cursor-pointer items-center gap-1.5 font-normal">
                    <Checkbox
                      checked={editProvider.strip_reasoning_content !== false}
                      onCheckedChange={(v) => setEditProvider({ ...editProvider, strip_reasoning_content: v === true })}
                    />
                    strip_reasoning_content
                  </Label>
                </div>

                <p className="border-b border-edge-700 pb-2 text-[10px] uppercase tracking-[0.14em] text-chart-2">03 / Credential vault</p>
                {!editingExistingProvider ? (
                  <Field>
                    <FieldLabel htmlFor="ep-keys">API keys</FieldLabel>
                    <Textarea
                      id="ep-keys"
                      value={editProvider.keys_text || ''}
                      onChange={(e) => setEditProvider({ ...editProvider, keys_text: e.target.value })}
                      placeholder="sk-…"
                      rows={4}
                      className="font-mono"
                    />
                    <FieldDescription>One per line; or account_id:api_key. Stored encrypted on save.</FieldDescription>
                  </Field>
                ) : (
                  <div className="space-y-2">
                    <FieldLabel>Existing keys</FieldLabel>
                    {(editProvider.keys || []).map((k) => (
                      <div key={k.id} className="flex min-w-0 flex-wrap items-center justify-between gap-2 border border-edge-700 bg-edge-950 px-3 py-3 text-[11px]">
                        <span className="min-w-0 basis-32 flex-1 break-all text-muted-foreground">{k.masked}{k.account_id ? ` · ${k.account_id}` : ''}</span>
                        <Button
                          variant="destructive"
                          size="xs"
                          className="shrink-0"
                          disabled={busy}
                          onClick={() => {
                            setConfirm({
                              title: `Remove credential ${k.masked}?`,
                              body: 'In-flight requests may still finish. The remaining keys keep serving.',
                              confirmLabel: 'Remove credential',
                              run: () => {
                                void run(async () => {
                                  await deleteAdminProviderKey(editProvider.name, k.id);
                                  const p = (await listAdminProviders()).providers.find((x) => x.name === editProvider.name);
                                  if (p) setEditProvider({ ...editProvider, keys: p.keys, append_key: '' });
                                }, 'Credential removed');
                              },
                            });
                          }}
                        >
                          remove
                        </Button>
                      </div>
                    ))}
                    <Field>
                      <FieldLabel htmlFor="ep-append-key">Append upstream credential (stored encrypted)</FieldLabel>
                      <Input
                        id="ep-append-key"
                        value={editProvider.append_key || ''}
                        onChange={(e) => setEditProvider({ ...editProvider, append_key: e.target.value })}
                        placeholder="sk-… or account_id:api_key"
                        className="font-mono"
                      />
                    </Field>
                  </div>
                )}

                <div className="flex flex-wrap gap-2 border-t border-edge-700 pt-5">
                  <Button disabled={busy} onClick={saveProvider}><Check className="size-3.5" />Save · live apply</Button>
                  <Button variant="outline" onClick={() => setEditProvider(null)}>Cancel</Button>
                </div>
              </CardContent>
            </Card>
  );
}
