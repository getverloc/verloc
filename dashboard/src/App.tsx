import { lazy, Suspense, useEffect, useMemo, useState } from 'react';
import {
  Activity,
  ArrowUpCircle,
  Bell,
  BookOpen,
  CalendarRange,
  ChevronRight,
  Clock3,
  CircleAlert,
  KeyRound,
  LayoutDashboard,
  ListOrdered,
  LogOut,
  Menu,
  RotateCw,
  ScrollText,
  Server,
  Settings as SettingsIcon,
  TriangleAlert,
  X,
  type LucideIcon,
} from 'lucide-react';
import { getState, getSeriesRange, getAlerts, getKeyEvents, connectLive, logout, fmtMs, fmtMoney, fmtInt, fmtTokens, fmtVersion, type State, type SeriesPoint, type RangePreset, type AlertEvent, type KeyEvent, type TabProps } from './api';
import { rollupSeries, fmtSpan } from './window';
import { readRange, writeRange } from './time-range';
import { DateRangePicker } from './components/DateRangePicker';
import Overview from './tabs/Overview';
import Requests from './tabs/Requests';
import Backends from './tabs/Backends';
import Keys from './tabs/Keys';
import Alerts from './tabs/Alerts';
import ClientKeys from './tabs/ClientKeys';
import Settings from './tabs/Settings';
// Heavy tabs load on demand: Admin (1.7k lines), Analytics (Recharts) and
// Docs are never needed for first paint or the login screen.
const Admin = lazy(() => import('./tabs/Admin'));
const Analytics = lazy(() => import('./tabs/Analytics'));
const Docs = lazy(() => import('./tabs/Docs'));
import Auth from './components/Auth';
import { MicroSlats } from './components/MicroSlats';
import { Button } from './components/ui/button';
import { Badge } from './components/ui/badge';
import { Card, CardContent } from './components/ui/card';
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from './components/ui/empty';
import { Separator } from './components/ui/separator';
import { Sheet, SheetContent, SheetTitle } from './components/ui/sheet';
import { Skeleton } from './components/ui/skeleton';
import { Tooltip, TooltipContent, TooltipTrigger } from './components/ui/tooltip';

const TABS = [
  { key: 'overview', label: 'Overview', description: 'Request volume, known spend, latency, and gateway health.' },
  { key: 'requests', label: 'Requests', description: 'Search request metadata and inspect upstream attempts.' },
  { key: 'providers', label: 'Providers', description: 'Manage upstream connections, credentials, and provider health.' },
  { key: 'models', label: 'Models', description: 'Configure public model IDs, pricing, and fallback routes.' },
  { key: 'api-keys', label: 'API Keys', description: 'Manage application access, model scopes, and usage limits.' },
  { key: 'analytics', label: 'Analytics', description: 'Usage, known cost, and response times by client, model, and provider.' },
  { key: 'alerts', label: 'Alerts', description: 'Review configured thresholds and recent operational events.' },
  { key: 'settings', label: 'Settings', description: 'Manage owner access, gateway limits, software updates, alert rules, and backups.' },
  { key: 'docs', label: 'Docs', description: 'Gateway API reference: endpoints, schemas, errors, routing, and configuration.' },
] as const;
export type TabKey = (typeof TABS)[number]['key'];
type Tab = (typeof TABS)[number];

const TAB_ICONS: Record<TabKey, LucideIcon> = {
  overview: LayoutDashboard,
  requests: ScrollText,
  providers: Server,
  models: ListOrdered,
  'api-keys': KeyRound,
  analytics: Activity,
  alerts: Bell,
  settings: SettingsIcon,
  docs: BookOpen,
};

const NAV_GROUPS: { label: string; keys: TabKey[] }[] = [
  { label: 'Monitor', keys: ['overview', 'requests', 'analytics', 'alerts'] },
  { label: 'Manage', keys: ['providers', 'models', 'api-keys'] },
  { label: 'System', keys: ['settings'] },
  { label: 'Reference', keys: ['docs'] },
];

function locationTab(): TabKey { const value = new URLSearchParams(location.search).get('tab'); return TABS.find((t) => t.key === value)?.key || 'overview'; }
function locationRange(): RangePreset { return readRange(location.search); }
const EMPTY_SERIES: SeriesPoint[] = [];

export default function App() { return <Auth><Gateway /></Auth>; }
function Gateway() {
  const [tab, setTab] = useState<TabKey>(locationTab);
  const [range, setRange] = useState<RangePreset>(locationRange);
  const [rangeAnchor, setRangeAnchor] = useState(Date.now);
  const [navOpen, setNavOpen] = useState(false);
  const [state, setState] = useState<State | null>(null);
  const [series, setSeries] = useState<SeriesPoint[]>([]);
  const [seriesKey, setSeriesKey] = useState('');
  const [error, setError] = useState('');
  const [seriesError, setSeriesError] = useState('');
  const [stateNonce, setStateNonce] = useState(0);
  const [seriesNonce, setSeriesNonce] = useState(0);
  const [liveAlerts, setLiveAlerts] = useState<AlertEvent[]>([]);
  const [liveKeyEvents, setLiveKeyEvents] = useState<KeyEvent[]>([]);
  const [liveOn, setLiveOn] = useState(false);
  const [signingOut, setSigningOut] = useState(false);
  const [dismissedUpdate, setDismissedUpdate] = useState('');
  useEffect(() => {
    const pop = () => { setTab(locationTab()); setRange(locationRange()); setRangeAnchor(Date.now()); };
    window.addEventListener('popstate', pop);
    return () => window.removeEventListener('popstate', pop);
  }, []);
  const navigate = (next: TabKey, hash?: string) => {
    const url = new URL(location.href);
    url.searchParams.set('tab', next);
    url.hash = hash || '';
    history.pushState(null, '', url);
    setTab(next);
    setNavOpen(false);
    if (hash) {
      setTimeout(() => document.getElementById(hash.replace(/^#/, ''))?.scrollIntoView({ behavior: 'smooth', block: 'start' }), 60);
    }
  };
  const changeRange = (next: RangePreset, anchor = Date.now()) => { const url = new URL(location.href); writeRange(url, next); history.pushState(null, '', url); setRange(next); setRangeAnchor(anchor); };
  useEffect(() => {
    let stopped = false;
    let inFlight = false;
    const tick = async () => {
      if (inFlight) return; inFlight = true;
      try { const response = await getState(); if (!stopped) { setState(response); setError(''); } }
      catch (e) { if (!stopped) setError(e instanceof Error ? e.message : 'Gateway unavailable'); }
      finally { inFlight = false; }
    };
    void tick(); const timer = setInterval(tick, 3000);
    return () => { stopped = true; clearInterval(timer); };
  }, [stateNonce]);
  useEffect(() => {
    const controller = new AbortController(); let stopped = false; let inFlight = false;
    const tick = async () => {
      if (inFlight || stopped) return; inFlight = true;
      try { const response = await getSeriesRange(range, 800, controller.signal); if (!stopped) { setSeries(response || []); setSeriesKey(range.key); setSeriesError(''); } }
      catch (e) { if (!stopped) setSeriesError(e instanceof Error ? e.message : 'Historical charts unavailable'); }
      finally { inFlight = false; }
    };
    void tick(); const timer = range.to === null ? setInterval(tick, 10000) : null;
    return () => { stopped = true; controller.abort(); if (timer) clearInterval(timer); };
  }, [range, seriesNonce]);
  useEffect(() => {
    let stopped = false;
    getAlerts(100).then((events) => { if (!stopped) setLiveAlerts((previous) => [...previous, ...events.filter((e) => !previous.some((p) => p.ts === e.ts && p.rule_id === e.rule_id))].sort((a, b) => b.ts - a.ts).slice(0, 200)); }).catch(() => { /* Live events still arrive over SSE. */ });
    getKeyEvents(100).then((events) => { if (!stopped) setLiveKeyEvents((previous) => [...previous, ...events].sort((a, b) => b.ts - a.ts).slice(0, 100)); }).catch(() => { /* Key health remains in state. */ });
    const close = connectLive({
      onAlert: (alert) => setLiveAlerts((previous) => [alert, ...previous].slice(0, 200)),
      onKeyEvent: (event) => setLiveKeyEvents((previous) => [event, ...previous].slice(0, 100)),
      onInit: () => setLiveOn(true), onClose: () => setLiveOn(false),
    });
    return () => { stopped = true; close(); };
  }, []);
  const viewSeries = seriesKey === range.key ? series : EMPTY_SERIES;
  const roll = useMemo(() => rollupSeries(viewSeries), [viewSeries]);
  const props: TabProps = { state, series: viewSeries, range, rangeAnchor, liveAlerts, liveKeyEvents };
  const selectedTab = TABS.find((item) => item.key === tab)!;
  const selectedGroup = NAV_GROUPS.find((group) => group.keys.includes(tab))!;
  const tabNumber = String(TABS.findIndex((item) => item.key === tab) + 1).padStart(2, '0');
  const telemetry = state?.telemetry;
  const telemetryWarning = !!telemetry && (telemetry.dropped_requests > 0 || telemetry.dropped_key_events > 0 || telemetry.write_errors > 0 || telemetry.queued > 1000);
  const cooldownKeys = state ? Object.values(state.keys).flat().filter((k) => !k.healthy).length : 0;
  const build = state?.build;
  const versionLabel = fmtVersion(build?.version || state?.version);
  const updateSummary = state?.update;
  const updateActive = !!updateSummary && ['downloading', 'verifying', 'installing', 'restarting'].includes(updateSummary.state);
  const showUpdateBanner = !!updateSummary?.update_available && !!updateSummary.latest_version && dismissedUpdate !== updateSummary.latest_version && !updateActive;
  const buildTooltip = build
    ? `${versionLabel} (${build.channel}) · commit ${build.commit}${build.build_time && build.build_time !== 'unknown' ? ` · built ${new Date(build.build_time).toLocaleString()}` : ''} · ${build.os}/${build.arch}`
    : versionLabel;
  const signOut = async () => {
    setSigningOut(true);
    try { await logout(); window.dispatchEvent(new Event('verloc:logout')); }
    catch (e) { setError(e instanceof Error ? e.message : 'Sign out failed'); }
    finally { setSigningOut(false); }
  };
  const renderNavLink = (item: Tab) => {
    const Icon = TAB_ICONS[item.key];
    const active = tab === item.key;
    return (
      <a
        key={item.key}
        href={`?${new URLSearchParams({ ...Object.fromEntries(new URLSearchParams(location.search)), tab: item.key })}`}
        aria-current={active ? 'page' : undefined}
        onClick={(e) => { if (e.button || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return; e.preventDefault(); navigate(item.key); }}
        className="nav-link"
      >
        <Icon className="size-4 shrink-0" strokeWidth={1.5} aria-hidden="true" />
        <span>{item.label}</span>
        {item.key === 'alerts' && liveAlerts.length > 0
          ? <span className="ml-auto text-[10px] text-amber-300">{liveAlerts.length}</span>
          : item.key === 'settings' && updateSummary?.update_available
            ? <span className="ml-auto flex items-center gap-1 text-[9px] uppercase tracking-wider text-primary"><span className="size-1.5 rounded-full bg-primary" aria-hidden="true" />new</span>
            : <span className="nav-index">{String(TABS.findIndex((t) => t.key === item.key) + 1).padStart(2, '0')}</span>}
      </a>
    );
  };
  const navBody = (label: string) => (
    <div className="console-sidebar flex h-full flex-col">
      <div className="flex h-[76px] shrink-0 items-center gap-3 border-b border-edge-700 px-5">
        <div className="brand-mark" aria-hidden="true"><img src="/dash/logo.png" alt="" /></div>
        <div>
          <div className="text-xl font-medium tracking-[-.07em] text-edge-100">verloc<span className="text-primary">_</span></div>
          <div className="mt-0.5 text-[8px] uppercase tracking-[.2em] text-edge-500">LLM gateway</div>
        </div>
      </div>
      <div className="mx-4 mt-5 flex items-center gap-2.5 border border-edge-700 bg-edge-950/70 px-3 py-3">
        <Server className="size-4 text-edge-400" strokeWidth={1.5} aria-hidden="true" />
        <div className="min-w-0">
          <div className="text-[11px] text-edge-200">Gateway workspace</div>
          <div className="mt-1 text-[9px] text-edge-500">Single-owner console</div>
        </div>
      </div>
      <nav aria-label={label} className="flex-1 space-y-6 overflow-y-auto px-4 py-6">
        {NAV_GROUPS.map((group) => (
          <div key={group.label}>
            <div className="console-kicker mb-2 px-3 text-edge-500">{group.label}</div>
            <div className="space-y-1">{group.keys.map((key) => renderNavLink(TABS.find((item) => item.key === key)!))}</div>
          </div>
        ))}
      </nav>
      <div className="shrink-0 border-t border-edge-700 p-4">
        <div className="mb-4 border border-edge-700 bg-edge-950/60 p-3">
          <div className="flex items-center justify-between text-[9px] uppercase tracking-widest">
            <span className="text-edge-400">Event stream</span>
            <span className={liveOn ? 'text-primary' : 'text-amber-300'}>{liveOn ? 'Connected' : 'Connecting'}</span>
          </div>
          <Separator className="my-2.5" />
          <div className="flex items-center justify-between text-[10px] text-edge-500">
            <span>Keys in cooldown</span><span className={cooldownKeys ? 'text-amber-300' : 'text-edge-300'}>{cooldownKeys}</span>
          </div>
        </div>
        <button
          type="button"
          onClick={() => navigate('settings', '#updates')}
          className="mb-2 flex w-full items-center justify-between gap-2 border border-edge-700 bg-edge-950/60 px-3 py-2 text-left text-[10px] transition-colors hover:border-edge-500 hover:bg-edge-900"
          title={buildTooltip}
        >
          <span className="truncate font-mono text-edge-300">{versionLabel}</span>
          {updateActive ? (
            <span className="shrink-0 text-[9px] uppercase tracking-wider text-primary">Updating…</span>
          ) : updateSummary?.update_available && updateSummary.latest_version ? (
            <span className="inline-flex shrink-0 items-center gap-1 font-mono text-[9px] text-primary">
              <ArrowUpCircle className="size-3" aria-hidden="true" />{fmtVersion(updateSummary.latest_version)}
            </span>
          ) : (
            <span className="shrink-0 text-[9px] uppercase tracking-wider text-edge-500">{build?.channel || 'build'}</span>
          )}
        </button>
        <Button variant="ghost" size="sm" disabled={signingOut} className="w-full justify-start gap-3 text-edge-400" onClick={() => void signOut()}>
          <LogOut className="size-3.5" aria-hidden="true" />{signingOut ? 'Signing out…' : 'Sign out'}
        </Button>
      </div>
    </div>
  );
  const showWindow = ['overview', 'requests', 'analytics', 'alerts'].includes(tab);
  return <div className="flex h-dvh bg-background text-edge-200">
    <a href="#main-content" className="sr-only z-[60] bg-primary px-4 py-3 text-primary-foreground focus:not-sr-only focus:absolute">Skip to content</a>
    <aside className="hidden w-[244px] shrink-0 border-r border-edge-700 md:block">{navBody('Main navigation')}</aside>
    <MicroSlats className="flex min-w-0 flex-1 flex-col">
      <header className="flex h-[60px] shrink-0 items-center justify-between gap-3 border-b border-edge-700 bg-edge-950/80 px-4 md:px-[30px]">
        <div className="flex min-w-0 items-center gap-2.5 text-[11px]">
          <Button variant="ghost" size="icon-sm" className="-ml-1 md:hidden" aria-label="Open navigation" onClick={() => setNavOpen(true)}><Menu aria-hidden="true" /></Button>
          <span className="hidden text-edge-500 sm:inline">Workspace</span>
          <ChevronRight className="hidden size-3 text-edge-600 sm:block" aria-hidden="true" />
          <span className="text-edge-400">{selectedGroup.label}</span>
          <ChevronRight className="size-3 text-edge-600" aria-hidden="true" />
          <span className="truncate text-edge-200">{selectedTab.label}</span>
        </div>
        <div className="flex shrink-0 items-center gap-4">
          {state && (
            <Tooltip>
              <TooltipTrigger render={<button type="button" onClick={() => navigate('settings', '#updates')} className="hidden items-center gap-1.5 border border-edge-700 bg-edge-900/70 px-2 py-0.5 font-mono text-[10px] text-edge-300 transition-colors hover:border-edge-500 hover:text-primary sm:inline-flex" />}>
                <span>{versionLabel}</span>
                {updateSummary?.update_available && <span className="size-1.5 rounded-full bg-primary" aria-label="Update available" />}
              </TooltipTrigger>
              <TooltipContent>{buildTooltip}</TooltipContent>
            </Tooltip>
          )}
          {state && <span className="hidden items-center gap-1.5 text-[10px] text-edge-500 lg:inline-flex"><Clock3 className="size-3" aria-hidden="true" />{fmtMs(state.uptime_sec * 1000)} uptime</span>}
          <Tooltip>
            <TooltipTrigger render={<span className={`flex cursor-default items-center gap-2 text-[9px] uppercase tracking-widest ${liveOn ? 'text-primary' : 'text-edge-400'}`} />}>
              <span className="workspace-status" aria-hidden="true" /><span aria-live="polite">{liveOn ? 'Live' : 'Connecting'}</span>
            </TooltipTrigger>
            <TooltipContent>{liveOn ? 'Receiving live gateway events' : 'Connecting to the live event stream…'}</TooltipContent>
          </Tooltip>
        </div>
      </header>
      <main id="main-content" tabIndex={-1} className="min-w-0 flex-1 overflow-y-auto outline-none">
        <div className="console-content">
          <div className="console-page-heading">
            <div className="console-section-marker console-kicker mb-3 text-primary">{selectedGroup.label}<span className="text-edge-600">/</span><span className="text-edge-500">{tabNumber}</span></div>
            <div className="flex flex-wrap items-start justify-between gap-4">
              <div><h1>{selectedTab.label}</h1><p className="mt-2">{selectedTab.description}</p></div>
              {tab === 'overview' && state && <Badge variant="outline" className={state.ready ? 'border-primary/25 bg-primary/5 text-primary' : 'border-amber-500/30 text-amber-300'}><span className={`size-1.5 ${state.ready ? 'bg-primary' : 'bg-amber-300'}`} aria-hidden="true" />{state.ready ? 'Gateway ready' : 'Gateway not ready'}</Badge>}
            </div>
          </div>
          {showWindow && (
            <div className="window-bar mb-6 border-y border-edge-700 py-3">
              <div className="min-w-0">
                <div className="flex items-center gap-2 text-[11px] text-edge-300"><CalendarRange className="size-3.5 text-edge-500" aria-hidden="true" />Reporting window <span className="text-edge-600">/</span><span className="text-edge-100">{range.label}</span></div>
                <div role="status" className="mt-1.5 text-[9px] leading-relaxed text-edge-500">{seriesKey !== range.key ? 'Loading historical snapshots…' : !viewSeries.length ? 'No historical snapshots in this window' : `${fmtMoney(roll.cost)} known spend · ${fmtInt(roll.requests)} requests · ${fmtTokens(roll.tokIn + roll.tokOut)} tokens · ${fmtSpan(roll.from, roll.to)}`}</div>
              </div>
              <DateRangePicker range={range} onChange={(next) => changeRange(next, Date.now())} />
            </div>
          )}
          <div className="space-y-5">
          {error && (
            <Card role="alert" className="border-red-500/30 bg-red-500/5">
              <CardContent className="flex flex-wrap items-center justify-between gap-2 text-[11px] text-red-400">
                <span>Gateway connection: {error}. Your open forms are preserved; reconnecting automatically.</span>
                <Button variant="outline" size="sm" onClick={() => setStateNonce((n) => n + 1)}>
                  <RotateCw aria-hidden="true" />Retry now
                </Button>
              </CardContent>
            </Card>
          )}
          {showWindow && seriesError && (
            <Card role="alert" className="border-red-500/30 bg-red-500/5">
              <CardContent className="flex flex-wrap items-center justify-between gap-2 text-[11px] text-red-400">
                <span className="inline-flex items-center gap-1.5"><CircleAlert className="size-3.5 shrink-0" aria-hidden="true" />Historical charts: {seriesError}. Retrying automatically.</span>
                <Button variant="outline" size="sm" onClick={() => setSeriesNonce((n) => n + 1)}>
                  <RotateCw aria-hidden="true" />Retry now
                </Button>
              </CardContent>
            </Card>
          )}
          {updateActive && updateSummary && (
            <Card role="status" className="border-primary/40 bg-primary/5">
              <CardContent className="flex flex-wrap items-center justify-between gap-2 text-[11px] text-primary">
                <span className="inline-flex items-center gap-2">
                  <RotateCw className="size-3.5 animate-spin shrink-0" aria-hidden="true" />
                  Updating verloc ({updateSummary.state}{updateSummary.progress_pct ? ` · ${updateSummary.progress_pct}%` : ''})…
                </span>
                {tab !== 'settings' && (
                  <Button variant="outline" size="sm" onClick={() => navigate('settings', '#updates')}>View progress</Button>
                )}
              </CardContent>
            </Card>
          )}
          {showUpdateBanner && updateSummary?.latest_version && (
            <Card role="status" className="border-primary/35 bg-primary/5">
              <CardContent className="flex flex-wrap items-center justify-between gap-3 text-[11px]">
                <span className="inline-flex items-center gap-2 text-edge-100">
                  <ArrowUpCircle className="size-4 shrink-0 text-primary" aria-hidden="true" />
                  <span>
                    <strong className="font-mono text-primary">{fmtVersion(updateSummary.latest_version)}</strong> is available (running <span className="font-mono">{versionLabel}</span>). Smooth in-place update preserves all user data.
                  </span>
                </span>
                <div className="flex items-center gap-2">
                  <Button size="sm" onClick={() => navigate('settings', '#updates')}>Review &amp; update</Button>
                  <Button variant="ghost" size="icon-sm" aria-label="Dismiss update notification" onClick={() => setDismissedUpdate(updateSummary.latest_version || '')}>
                    <X className="size-3.5" aria-hidden="true" />
                  </Button>
                </div>
              </CardContent>
            </Card>
          )}
          {telemetryWarning && (
            <Card role="alert" className="border-amber-500/40 bg-amber-500/5">
              <CardContent className="flex gap-2.5 text-[11px] leading-relaxed text-amber-400">
                <TriangleAlert className="mt-0.5 size-4 shrink-0" aria-hidden="true" />
                <p><strong>Analytics telemetry needs attention.</strong> {telemetry?.queued} queued · {telemetry?.dropped_requests} dropped requests · {telemetry?.dropped_key_events} dropped credential events · {telemetry?.write_errors} write errors. Totals may be incomplete. Last successful write: {telemetry?.last_write_ts ? new Date(telemetry.last_write_ts).toLocaleString() : 'not recorded'}.</p>
              </CardContent>
            </Card>
          )}
          {tab === 'overview' && (state && seriesKey === range.key ? <Overview {...props} /> : (
            <div className="space-y-3">
              <div className="grid gap-3 sm:grid-cols-3">
                <Skeleton className="h-20" />
                <Skeleton className="h-20" />
                <Skeleton className="h-20" />
              </div>
              <Empty role="status">
                <EmptyHeader>
                  <EmptyTitle>Loading gateway health</EmptyTitle>
                  <EmptyDescription>Loading gateway health and historical traffic…</EmptyDescription>
                </EmptyHeader>
              </Empty>
            </div>
          ))}
          {tab === 'requests' && <Requests {...props} />}
          {(tab === 'providers' || tab === 'models') && <Suspense fallback={<Skeleton className="h-64 w-full" />}><Admin key={tab} {...props} section={tab} /></Suspense>}
          {tab === 'providers' && <details className="group border border-edge-700 bg-card"><summary className="flex cursor-pointer list-none items-center justify-between gap-3 px-5 py-4 text-[11px] text-edge-300"><span className="flex items-center gap-2.5"><Activity className="size-4 text-cyber-blue" aria-hidden="true" />Provider & credential diagnostics</span><ChevronRight className="size-4 transition-transform group-open:rotate-90" aria-hidden="true" /></summary><div className="space-y-5 border-t border-edge-700 p-4 md:p-5"><p className="text-[11px] leading-relaxed text-edge-400">Process-session health counters, separate from client API-key usage. Durable client totals are available in Analytics.</p><Backends {...props} /><Keys {...props} /></div></details>}
          {tab === 'api-keys' && <ClientKeys />}
          {tab === 'analytics' && <Suspense fallback={<Skeleton className="h-64 w-full" />}><Analytics {...props} /></Suspense>}
          {tab === 'alerts' && <Alerts {...props} />}
          {tab === 'settings' && <Settings {...props} />}
          {tab === 'docs' && <Suspense fallback={<Skeleton className="h-64 w-full" />}><Docs /></Suspense>}
          </div>
          <footer className="console-footer"><span>verloc <span className="font-mono text-edge-300">{versionLabel}</span> / gateway control plane</span><span>Request metadata only. No stored prompts or completions.</span></footer>
        </div>
      </main>
    </MicroSlats>
    <Sheet open={navOpen} onOpenChange={(open) => setNavOpen(open)}>
      <SheetContent side="left" className="w-[280px] gap-0 p-0 sm:max-w-[280px]">
        <SheetTitle className="sr-only">Navigation</SheetTitle>
        {navBody('Mobile navigation')}
      </SheetContent>
    </Sheet>
  </div>;
}
