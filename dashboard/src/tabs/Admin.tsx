import { useCallback, useEffect, useMemo, useState } from 'react';
import {
  listAdminProviders, listAdminModels, upsertAdminProvider,
  deleteAdminProvider, deleteAdminModel, bulkModels,
  type AdminProvider, type AdminModel, type TabProps, type RouteTarget,
} from '../api';
import { ProviderEditor } from '../components/admin/ProviderEditor';
import { useProviderEditor } from '../components/admin/useProviderEditor';
import { ModelEditor } from '../components/admin/ModelEditor';
import { useModelEditor } from '../components/admin/useModelEditor';
import { OnboardingWizard } from '../components/admin/OnboardingWizard';
import type { PendingConfirm } from '../components/admin/shared';
import { toast } from 'sonner';
import {
  AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent,
  AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle,
} from '../components/ui/alert-dialog';
import { Badge } from '../components/ui/badge';
import { Button } from '../components/ui/button';
import { Card, CardContent, CardDescription, CardTitle } from '../components/ui/card';
import { Checkbox } from '../components/ui/checkbox';
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from '../components/ui/empty';
import { Input } from '../components/ui/input';
import { Skeleton } from '../components/ui/skeleton';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '../components/ui/table';
import { Tooltip, TooltipContent, TooltipTrigger } from '../components/ui/tooltip';
import { Database, KeyRound, Layers3, Pencil, Plus, RefreshCw, Search, Server, ShieldCheck, Trash2 } from 'lucide-react';

type Section = 'providers' | 'models';


export default function Admin({ state, section }: TabProps & { section: Section }) {
  const [providers, setProviders] = useState<AdminProvider[]>([]);
  const [models, setModels] = useState<AdminModel[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [loading, setLoading] = useState(true);
  const [selectedModels, setSelectedModels] = useState<string[]>([]);
  const [pq, setPq] = useState('');
  const [mq, setMq] = useState('');
  const [confirm, setConfirm] = useState<PendingConfirm | null>(null);

  const [wizOpen, setWizOpen] = useState(false);

  const refresh = useCallback(async (opts?: { keepErr?: boolean }) => {
    if (!opts?.keepErr) setErr(null);
    try {
      const [p, m] = await Promise.all([listAdminProviders(), listAdminModels()]);
      setProviders(p.providers || []);
      setModels(m.models || []);
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { void refresh(); }, [refresh]);


  const run = async (fn: () => Promise<void>, success: string) => {
    setBusy(true);
    setErr(null);
    let saved = false;
    try {
      await fn();
      saved = true;
      toast.success(success);
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
    // Reload after the button is free. A slow list must not leave the save
    // button stuck once the write itself has finished.
    void refresh(saved ? undefined : { keepErr: true });
  };

  const providerEditor = useProviderEditor({ providers, run, setErr, setConfirm });
  const modelEditor = useModelEditor({ providers, models, run, setErr, setConfirm });
  const { editProvider } = providerEditor;
  const { editModel, setEditModel } = modelEditor;

  const filteredProviders = useMemo(() => {
    const q = pq.trim().toLowerCase();
    if (!q) return providers;
    return providers.filter((p) =>
      `${p.name} ${p.base_url}`.toLowerCase().includes(q));
  }, [providers, pq]);

  const filteredModels = useMemo(() => {
    const q = mq.trim().toLowerCase();
    if (!q) return models;
    return models.filter((m) =>
      `${m.id} ${m.name} ${m.backend} ${m.upstream}`.toLowerCase().includes(q));
  }, [models, mq]);


  const deleteProvider = (p: AdminProvider) => {
    setConfirm({
      title: `Delete provider ${p.name}?`,
      body: `Its ${p.keys.length} stored credential${p.keys.length === 1 ? '' : 's'} and connection settings are removed. Model routes pointing at ${p.name} stop working.`,
      confirmLabel: 'Delete provider',
      run: () => { void run(async () => { await deleteAdminProvider(p.name); }, 'Provider deleted'); },
    });
  };

  const deleteModel = (id: string) => {
    setConfirm({
      title: `Delete model ${id}?`,
      body: 'Applications using this ID will fail until it is re-added. This cannot be undone.',
      confirmLabel: 'Delete model',
      run: () => {
        void run(async () => {
          await deleteAdminModel(id);
          if (editModel?.id === id) setEditModel(null);
        }, 'Model deleted');
      },
    });
  };

  const bulkAction = (action: 'enable' | 'disable' | 'delete') => {
    if (selectedModels.length === 0) return;
    if (action === 'delete') {
      setConfirm({
        title: `Delete ${selectedModels.length} model route${selectedModels.length === 1 ? '' : 's'}?`,
        body: 'Permanently removes the selected routes. This cannot be undone.',
        confirmLabel: 'Delete selected',
        run: () => {
          void run(async () => { await bulkModels(selectedModels, action); setSelectedModels([]); }, `${selectedModels.length} model(s) deleted`);
        },
      });
      return;
    }
    void run(async () => { await bulkModels(selectedModels, action); setSelectedModels([]); }, `${selectedModels.length} model(s): ${action}`);
  };

  const allFilteredSelected = filteredModels.length > 0 && filteredModels.every((m) => selectedModels.includes(m.id));

  return (
    <div className="min-w-0 space-y-5">
      <div className="grid grid-cols-2 gap-px border border-edge-700 bg-edge-700 lg:grid-cols-4">
        {(section === 'providers' ? [
          { label: 'Connections', value: providers.length, detail: 'Configured upstream providers', icon: Server, color: 'text-primary' },
          { label: 'Enabled', value: providers.filter((p) => !p.disabled).length, detail: 'Connections allowed to serve', icon: ShieldCheck, color: 'text-chart-2' },
          { label: 'Stored credentials', value: providers.reduce((sum, p) => sum + p.keys.length, 0), detail: 'Encrypted upstream keys', icon: KeyRound, color: 'text-chart-3' },
          { label: 'Keyless endpoints', value: providers.filter((p) => p.no_auth).length, detail: 'No credential header required', icon: Layers3, color: 'text-chart-2' },
        ] : [
          { label: 'Catalog routes', value: models.length, detail: 'Application-facing model IDs', icon: Database, color: 'text-primary' },
          { label: 'Enabled', value: models.filter((m) => !m.disabled).length, detail: 'Model routes allowed to serve', icon: ShieldCheck, color: 'text-chart-2' },
          { label: 'With fallbacks', value: models.filter((m) => (m.routes || []).length > 0).length, detail: 'Routes with additional targets', icon: Layers3, color: 'text-chart-3' },
          { label: 'Unknown pricing', value: models.filter((m) => !m.pricing_known).length, detail: 'Not counted as free usage', icon: Database, color: 'text-chart-4' },
        ]).map(({ label, value, detail, icon: Icon, color }) => (
          <div key={label} className="min-w-0 bg-edge-900 p-4 sm:p-5">
            <div className="flex items-center justify-between gap-2 text-[10px] uppercase tracking-[0.14em] text-muted-foreground">
              <span>{label}</span><Icon className={`size-3.5 shrink-0 ${color}`} />
            </div>
            <div className={`mt-4 text-3xl tabular-nums tracking-tight ${color}`}>{loading ? '—' : value}</div>
            <p className="mt-2 text-[10px] leading-relaxed text-muted-foreground">{detail}</p>
          </div>
        ))}
      </div>

      <Card className="gap-0 border-edge-700 bg-edge-900 py-0">
        <div className="flex flex-col gap-4 p-4 sm:flex-row sm:items-center sm:justify-between sm:px-5">
          <div className="flex min-w-0 items-start gap-3">
            <ShieldCheck className="mt-0.5 size-4 shrink-0 text-primary" />
            <div>
              <p className="text-[10px] uppercase tracking-[0.14em] text-primary">Management workspace</p>
              <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
                Encrypted credentials. Changes apply live. Guided onboarding stages discovery, tests, and catalog hints before one transactional save.
              </p>
            </div>
          </div>
          <div className="flex shrink-0 flex-wrap items-center gap-2">
            <Button variant="outline" size="sm" disabled={busy || loading} onClick={() => { void refresh(); }}>
              <RefreshCw className={`size-3.5 ${loading ? 'animate-spin' : ''}`} />Refresh
            </Button>
            <Button size="sm" onClick={() => setWizOpen(true)}>
              <Plus className="size-3.5" />Guided add
            </Button>
          </div>
        </div>
        {err && (
          <CardContent className="border-t border-edge-700 p-4">
            <div role="alert" className="flex flex-wrap items-center gap-2 border border-destructive/40 bg-destructive/10 px-3 py-3 text-xs">
              <span className="min-w-0 flex-1 break-words text-destructive">{err}</span>
              <Button variant="outline" size="xs" onClick={() => { void refresh(); }}>retry</Button>
            </div>
          </CardContent>
        )}
      </Card>

      <OnboardingWizard open={wizOpen} onOpenChange={setWizOpen} providers={providers} models={models} busy={busy} refresh={refresh} setConfirm={setConfirm} />

      {section === 'providers' && (
        <div className={`grid min-w-0 grid-cols-1 items-start gap-5 ${editProvider ? 'xl:grid-cols-[minmax(0,1fr)_minmax(360px,0.8fr)]' : ''}`}>
          <Card className="min-w-0 gap-0 border-edge-700 bg-edge-900 py-0">
            <div className="flex flex-wrap items-center justify-between gap-4 border-b border-edge-700 p-4 sm:p-5">
              <div className="min-w-0">
                <p className="mb-2 text-[10px] uppercase tracking-[0.16em] text-primary">01 / Connection directory</p>
                <CardTitle className="text-sm">Upstream providers <span className="ml-2 text-muted-foreground">{filteredProviders.length}{pq ? ` / ${providers.length}` : ''}</span></CardTitle>
                <CardDescription className="mt-2 text-[11px]">Connection settings, encrypted credentials, and live pool health.</CardDescription>
              </div>
              <div className="flex min-w-0 flex-1 basis-full flex-wrap items-center gap-2 sm:basis-auto sm:justify-end">
                  <div className="relative min-w-0 flex-1 sm:max-w-60">
                    <Search className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground" />
                    <Input aria-label="Filter providers" value={pq} onChange={(e) => setPq(e.target.value)} placeholder="Search connections…" className="w-full pl-8" />
                  </div>
                  <Button
                    size="sm"
                    className="shrink-0"
                    onClick={providerEditor.startNew}
                  >
                    <Plus className="size-3.5" />New provider
                  </Button>
                </div>
            </div>
            <CardContent className="px-0 pb-0">
              <div className="overflow-x-auto">
                <Table className="min-w-[700px]">
                  <TableHeader className="bg-edge-950">
                    <TableRow>
                      <TableHead className="w-[40%] pl-5">Connection / endpoint</TableHead>
                      <TableHead>Credentials</TableHead>
                      <TableHead>Status</TableHead>
                      <TableHead className="text-right">Actions</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {loading && (
                      Array.from({ length: 4 }).map((_, i) => (
                        <TableRow key={i}>
                          <TableCell colSpan={4}><Skeleton className="h-16 w-full rounded-none" /></TableCell>
                        </TableRow>
                      ))
                    )}
                    {!loading && filteredProviders.length === 0 && (
                      <TableRow>
                        <TableCell colSpan={4}>
                          <Empty className="py-12">
                            <EmptyHeader>
                              <EmptyTitle>{pq ? 'No providers match' : 'No providers'}</EmptyTitle>
                              <EmptyDescription>{pq ? 'Adjust the filter.' : 'Add a provider manually or via guided add.'}</EmptyDescription>
                            </EmptyHeader>
                          </Empty>
                        </TableCell>
                      </TableRow>
                    )}
                    {filteredProviders.map((p) => {
                      const pool = state?.keys?.[p.name] || [];
                      const up = pool.filter((k) => k.healthy).length;
                      return (
                        <TableRow key={p.name} className={editProvider?.name === p.name ? 'bg-primary/5' : ''}>
                          <TableCell className="py-5 pl-5 align-top">
                            <div className="min-w-0 max-w-md whitespace-normal">
                              <div className="flex items-center gap-2 text-xs font-medium text-foreground"><Server className="size-3.5 shrink-0 text-chart-2" /><span className="break-all">{p.name}</span></div>
                              <div className="mt-2 break-all text-[11px] leading-relaxed text-muted-foreground">{p.base_url}</div>
                              <div className="mt-3 flex flex-wrap gap-1.5">
                                <Badge variant="outline">{p.model_count} models</Badge>
                                {p.api_format === 'systemone' && <Badge variant="outline">System One</Badge>}
                                {p.no_auth && <Badge variant="outline" className="border-chart-2/40 text-chart-2">keyless</Badge>}
                                {p.max_tokens > 0 && <Badge variant="outline">cap {p.max_tokens}</Badge>}
                              </div>
                            </div>
                          </TableCell>
                          <TableCell className="py-5 align-top">
                            <Tooltip>
                              <TooltipTrigger>
                                <span className="block text-left text-[11px]">
                                  <span className="block">{p.keys.length} stored key{p.keys.length === 1 ? '' : 's'}</span><span className={`mt-1 block ${up < p.keys.length ? 'text-chart-4' : 'text-primary'}`}>{up} healthy in live pool</span>
                                </span>
                              </TooltipTrigger>
                              <TooltipContent>Live pool health from the Keys readout{pool.length !== p.keys.length ? ' (pool view may lag staged edits)' : ''}.</TooltipContent>
                            </Tooltip>
                          </TableCell>
                          <TableCell className="py-5 align-top">
                            <Badge variant="outline" className={p.disabled ? 'border-chart-4/40 text-chart-4' : 'border-primary/30 text-primary'}>
                              {p.disabled ? 'Disabled' : 'Enabled'}
                            </Badge>
                          </TableCell>
                          <TableCell className="py-5 pr-5 align-top">
                            <div className="flex min-w-44 flex-wrap justify-end gap-1.5">
                              <Button
                                variant="outline"
                                size="xs"
                                disabled={busy}
                                onClick={() => {
                                  void run(async () => {
                                    await upsertAdminProvider({ ...p, keys: undefined, model_count: undefined, disabled: !p.disabled });
                                  }, p.disabled ? 'Provider enabled' : 'Provider disabled');
                                }}
                              >
                                {p.disabled ? 'Enable' : 'Disable'}
                              </Button>
                              <Button
                                variant="outline"
                                size="xs"
                                onClick={() => providerEditor.edit(p)}
                              >
                                <Pencil className="size-3" />edit
                              </Button>
                              <Button variant="destructive" size="xs" disabled={busy} onClick={() => deleteProvider(p)}>
                                <Trash2 className="size-3" />Delete
                              </Button>
                            </div>
                          </TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              </div>
            </CardContent>
            <div className="flex flex-wrap justify-between gap-2 border-t border-edge-700 bg-edge-950 px-5 py-3 text-[10px] text-muted-foreground">
              <span>{loading ? 'Loading connections…' : `${filteredProviders.length} of ${providers.length} connections`}</span>
              <span>Credentials are separate from application keys</span>
            </div>
          </Card>

          {editProvider && <ProviderEditor {...providerEditor} busy={busy} run={run} setConfirm={setConfirm} />}
        </div>
      )}

      {section === 'models' && (
        <div className={`grid min-w-0 grid-cols-1 items-start gap-5 ${editModel ? 'xl:grid-cols-[minmax(0,1fr)_minmax(360px,0.8fr)]' : ''}`}>
          <Card className="min-w-0 gap-0 border-edge-700 bg-edge-900 py-0">
            <div className="flex flex-wrap items-center justify-between gap-4 border-b border-edge-700 p-4 sm:p-5">
              <div className="min-w-0">
                <p className="mb-2 text-[10px] uppercase tracking-[0.16em] text-primary">01 / Routing directory</p>
                <CardTitle className="text-sm">Model catalog <span className="ml-2 text-muted-foreground">{filteredModels.length}{mq ? ` / ${models.length}` : ''}</span></CardTitle>
                <CardDescription className="mt-2 text-[11px]">Application IDs, primary targets, and ordered fallback routes.</CardDescription>
              </div>
              <div className="flex min-w-0 flex-1 basis-full flex-wrap items-center gap-2 sm:basis-auto sm:justify-end">
                  <div className="relative min-w-0 flex-1 sm:max-w-60">
                    <Search className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground" />
                    <Input aria-label="Filter models" value={mq} onChange={(e) => setMq(e.target.value)} placeholder="Search IDs, providers, targets…" className="w-full pl-8" />
                  </div>
                  <Button
                    size="sm"
                    className="shrink-0"
                    onClick={modelEditor.startNew}
                  >
                    <Plus className="size-3.5" />New model
                  </Button>
                </div>
            </div>
            {(selectedModels.length > 0 || filteredModels.length > 0) && (
              <CardContent className="border-b border-edge-700 bg-edge-950 px-4 py-3 sm:px-5">
                <div className="flex flex-wrap items-center gap-2 text-[11px]">
                  <Button variant="outline" size="xs" disabled={busy || !filteredModels.length} onClick={() => setSelectedModels(filteredModels.map((m) => m.id))}>
                    Select filtered
                  </Button>
                  <span className={selectedModels.length > 0 ? 'text-primary' : 'text-muted-foreground'}>{selectedModels.length} selected</span>
                  <div className="ml-auto flex flex-wrap gap-1.5">
                    <Button variant="outline" size="xs" disabled={busy || !selectedModels.length} onClick={() => bulkAction('enable')}>enable</Button>
                    <Button variant="outline" size="xs" disabled={busy || !selectedModels.length} onClick={() => bulkAction('disable')}>disable</Button>
                    <Button variant="destructive" size="xs" disabled={busy || !selectedModels.length} onClick={() => bulkAction('delete')}>delete</Button>
                  </div>
                </div>
              </CardContent>
            )}
            <CardContent className="px-0 pb-0">
              <div className="overflow-x-auto">
                <Table className="min-w-[800px]">
                  <TableHeader className="bg-edge-950">
                    <TableRow>
                      <TableHead className="w-12 pl-5">
                        <Checkbox
                          aria-label="Select all filtered models"
                          checked={allFilteredSelected}
                          onCheckedChange={(v) => {
                            if (v === true) setSelectedModels(filteredModels.map((m) => m.id));
                            else setSelectedModels([]);
                          }}
                        />
                      </TableHead>
                      <TableHead className="w-[34%]">Application model / pricing</TableHead>
                      <TableHead>Primary target / fallbacks</TableHead>
                      <TableHead>Status</TableHead>
                      <TableHead className="text-right">Actions</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {loading && (
                      Array.from({ length: 4 }).map((_, i) => (
                        <TableRow key={i}>
                          <TableCell colSpan={5}><Skeleton className="h-16 w-full rounded-none" /></TableCell>
                        </TableRow>
                      ))
                    )}
                    {!loading && filteredModels.length === 0 && (
                      <TableRow>
                        <TableCell colSpan={5}>
                          <Empty className="py-12">
                            <EmptyHeader>
                              <EmptyTitle>{mq ? 'No models match' : 'No models'}</EmptyTitle>
                              <EmptyDescription>{mq ? 'Adjust the filter.' : 'Add a model manually or via guided add.'}</EmptyDescription>
                            </EmptyHeader>
                          </Empty>
                        </TableCell>
                      </TableRow>
                    )}
                    {filteredModels.map((m) => {
                      const routes: RouteTarget[] = [{ backend: m.backend, upstream: m.upstream }, ...(m.routes || [])];
                      const fallbacks = routes.slice(1);
                      return (
                        <TableRow key={m.id} className={selectedModels.includes(m.id) || editModel?.id === m.id ? 'bg-primary/5' : ''}>
                          <TableCell className="py-5 pl-5 align-top">
                            <Checkbox
                              aria-label={`Select ${m.id}`}
                              checked={selectedModels.includes(m.id)}
                              onCheckedChange={(v) => setSelectedModels(v === true ? [...selectedModels, m.id] : selectedModels.filter((id) => id !== m.id))}
                            />
                          </TableCell>
                          <TableCell className="py-5 align-top">
                            <div className="min-w-0 max-w-md whitespace-normal">
                              <div className="break-all text-xs font-medium text-foreground">{m.id}</div>
                              <div className="mt-1 break-words text-[11px] text-muted-foreground">{m.name || m.upstream}</div>
                              <div className="mt-3 flex flex-wrap gap-1.5">
                                <Badge variant="outline" className={m.pricing_known ? 'border-chart-4/30 text-chart-4 font-normal normal-case' : 'border-edge-600 text-muted-foreground font-normal normal-case'}>
                                  {m.pricing_known ? `${m.pricing_input} in / ${m.pricing_output} out · $/1M` : 'Pricing unknown'}
                                </Badge>
                                {m.has_meta && <Badge variant="outline" className="border-chart-2/40 text-chart-2">metadata</Badge>}
                              </div>
                            </div>
                          </TableCell>
                          <TableCell className="py-5 align-top">
                            <div className="min-w-0 max-w-xs whitespace-normal text-[11px]">
                              <div className="text-chart-2">{m.backend}</div>
                              <div className="mt-1 break-all leading-relaxed text-muted-foreground">{m.upstream}</div>
                              {fallbacks.length > 0 && (
                                <Tooltip>
                                  <TooltipTrigger>
                                    <span className="mt-2 inline-block cursor-help text-chart-3 underline decoration-dotted underline-offset-4">
                                      +{fallbacks.length} fallback{fallbacks.length === 1 ? '' : 's'}
                                    </span>
                                  </TooltipTrigger>
                                  <TooltipContent>
                                    <div className="max-w-sm space-y-2 break-all text-[11px]">
                                      {fallbacks.map((r, i) => <div key={i}>{String(i + 2).padStart(2, '0')} / {r.backend}:{r.upstream}</div>)}
                                    </div>
                                  </TooltipContent>
                                </Tooltip>
                              )}
                            </div>
                          </TableCell>
                          <TableCell className="py-5 align-top">
                            <Badge variant="outline" className={m.disabled ? 'border-chart-4/40 text-chart-4' : 'border-primary/30 text-primary'}>
                              {m.disabled ? 'Disabled' : 'Enabled'}
                            </Badge>
                          </TableCell>
                          <TableCell className="py-5 pr-5 align-top">
                            <div className="flex min-w-44 flex-wrap justify-end gap-1.5">
                              <Button variant="outline" size="xs" disabled={busy} onClick={() => { void run(async () => { await bulkModels([m.id], m.disabled ? 'enable' : 'disable'); }, m.disabled ? 'Model enabled' : 'Model disabled'); }}>
                                {m.disabled ? 'Enable' : 'Disable'}
                              </Button>
                              <Button
                                variant="outline"
                                size="xs"
                                onClick={() => modelEditor.edit(m)}
                              >
                                <Pencil className="size-3" />edit
                              </Button>
                              <Button variant="destructive" size="xs" disabled={busy} onClick={() => deleteModel(m.id)}>
                                <Trash2 className="size-3" />Delete
                              </Button>
                            </div>
                          </TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              </div>
            </CardContent>
            <div className="flex flex-wrap justify-between gap-2 border-t border-edge-700 bg-edge-950 px-5 py-3 text-[10px] text-muted-foreground">
              <span>{loading ? 'Loading catalog…' : `${filteredModels.length} of ${models.length} model routes`}</span>
              <span>Unknown pricing is not free usage</span>
            </div>
          </Card>

          {editModel && <ModelEditor {...modelEditor} providers={providers} models={models} busy={busy} setBusy={setBusy} setErr={setErr} deleteModel={deleteModel} />}
        </div>
      )}

      {/* ---- Explicit overwrite / destructive confirm ---- */}
      <AlertDialog open={!!confirm} onOpenChange={(open) => { if (!open) setConfirm(null); }}>
        <AlertDialogContent className="border-edge-600 bg-edge-900">
          <AlertDialogHeader>
            <AlertDialogTitle className="break-words text-base">{confirm?.title}</AlertDialogTitle>
            <AlertDialogDescription className="break-words text-xs leading-relaxed">{confirm?.body}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                confirm?.run();
                setConfirm(null);
              }}
            >
              {confirm?.confirmLabel || 'Confirm'}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
