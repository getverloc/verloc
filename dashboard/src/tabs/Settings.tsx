import { useEffect, useState, type FormEvent } from 'react';
import { toast } from 'sonner';
import { Database, History, Plus, RefreshCw } from 'lucide-react';
import {
  changePassword, createBackup, fmtDateTime, fmtInt, getAudit, getSettings, saveSettings,
  type AlertRule, type AuditEvent, type GatewayLimits, type ResponseCacheConfig, type Settings as GatewaySettings,
  type TabProps,
} from '../api';
import { Badge } from '../components/ui/badge';
import { Button } from '../components/ui/button';
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from '../components/ui/card';
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from '../components/ui/empty';
import { Field, FieldDescription, FieldLabel } from '../components/ui/field';
import { Input } from '../components/ui/input';
import { Label } from '../components/ui/label';
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from '../components/ui/select';
import { Skeleton } from '../components/ui/skeleton';
import { Switch } from '../components/ui/switch';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '../components/ui/table';
import { Textarea } from '../components/ui/textarea';
import { Tooltip, TooltipContent, TooltipTrigger } from '../components/ui/tooltip';

const limitLabels: Record<keyof GatewayLimits, string> = {
  max_body_bytes: 'Maximum request body (bytes)',
  max_attempts: 'Maximum upstream attempts',
  retry_budget_seconds: 'Retry budget (seconds)',
  queue_timeout_seconds: 'Queue wait timeout (seconds)',
  max_in_flight: 'Global concurrent requests',
  max_queued: 'Global queued requests',
  stream_idle_timeout_seconds: 'Stream idle timeout (seconds)',
};

interface LimitGroup {
  title: string;
  description: string;
  keys: Array<keyof GatewayLimits>;
}

const limitGroups: LimitGroup[] = [
  { title: 'Traffic admission', description: 'Bound parallel work and queue pressure.', keys: ['max_in_flight', 'max_queued', 'queue_timeout_seconds'] },
  { title: 'Execution & retries', description: 'Control upstream attempts and request lifetimes.', keys: ['max_attempts', 'retry_budget_seconds', 'stream_idle_timeout_seconds'] },
  { title: 'Request payload', description: 'Bound the size of incoming request bodies.', keys: ['max_body_bytes'] },
];

const METRICS: Record<string, { help: string; threshold: number }> = {
  error_rate: { help: 'share of requests that failed (%), over window', threshold: 5 },
  failover_rate: { help: 'share of requests that needed a retry (%)', threshold: 10 },
  cost_per_hour: { help: 'estimated spend rate ($/h) — notification only, not a billing budget', threshold: 1 },
  rps: { help: 'requests per second', threshold: 100 },
  tokens_per_hour: { help: 'token throughput (tok/h)', threshold: 1000000 },
  latency_p95: { help: 'p95 total latency (ms) — tracks output length as much as upstream health', threshold: 30000 },
  ttft_avg: { help: 'average time-to-first-token (ms)', threshold: 2000 },
  ttft_p95: { help: 'p95 time-to-first-token of recorded streams (ms) — measures the upstream, not output length', threshold: 15000 },
  keys_in_cooldown: { help: 'count of keys currently in cooldown', threshold: 1 },
  telemetry_errors: { help: 'cumulative write errors and dropped telemetry records this process', threshold: 1 },
  telemetry_queued: { help: 'telemetry records waiting to be written', threshold: 1536 },
};

const SEVERITIES = ['info', 'warning', 'critical'];

const MIB = 1024 * 1024;

interface CacheDraft {
  enabled: boolean;
  ttl_seconds: string;
  max_entries: string;
  max_mib: string;
  models_text: string;
}

function cacheDraftFrom(cfg?: ResponseCacheConfig): CacheDraft {
  return {
    enabled: !!cfg?.enabled,
    ttl_seconds: String(cfg?.ttl_seconds || 300),
    max_entries: String(cfg?.max_entries || 1024),
    max_mib: String(Math.max(1, Math.round((cfg?.max_bytes || 64 * MIB) / MIB))),
    models_text: (cfg?.models || []).join(', '),
  };
}

function fmtBytes(n: number): string {
  if (!n || n <= 0) return '0 B';
  if (n >= MIB) return `${(n / MIB).toFixed(1)} MiB`;
  if (n >= 1024) return `${(n / 1024).toFixed(1)} KiB`;
  return `${n} B`;
}

export default function Settings({ state }: TabProps) {
  const [settings, setSettings] = useState<GatewaySettings | null>(null);
  const [cacheDraft, setCacheDraft] = useState<CacheDraft>(() => cacheDraftFrom());
  const [cacheError, setCacheError] = useState('');
  const [events, setEvents] = useState<AuditEvent[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState('');
  const [busy, setBusy] = useState(false);
  const [limitsError, setLimitsError] = useState('');
  const [passwordError, setPasswordError] = useState('');
  const [backup, setBackup] = useState('');
  const [backupError, setBackupError] = useState('');
  const [rules, setRules] = useState<string | null>(null);
  const [rulesError, setRulesError] = useState('');
  const [showHelp, setShowHelp] = useState(false);
  const [newId, setNewId] = useState('');
  const [newMetric, setNewMetric] = useState('error_rate');
  const [newThreshold, setNewThreshold] = useState('5');
  const [newSeverity, setNewSeverity] = useState('warning');
  const [currentPassword, setCurrentPassword] = useState('');
  const [newPassword, setNewPassword] = useState('');
  const [confirmPassword, setConfirmPassword] = useState('');
  const [auditBusy, setAuditBusy] = useState(false);

  useEffect(() => {
    let active = true;
    Promise.all([getSettings(), getAudit()])
      .then(([s, a]) => {
        if (active) {
          setSettings(s);
          setCacheDraft(cacheDraftFrom(s.cache));
          setEvents(a.events || []);
          setLoadError('');
        }
      })
      .catch((e) => { if (active) setLoadError(e instanceof Error ? e.message : 'Could not load settings'); })
      .finally(() => { if (active) setLoading(false); });
    return () => { active = false; };
  }, []);

  const refreshAudit = async () => {
    setAuditBusy(true);
    try {
      setEvents((await getAudit()).events || []);
    } catch (e) {
      setLoadError(e instanceof Error ? e.message : 'Audit unavailable');
    } finally {
      setAuditBusy(false);
    }
  };

  const reload = () => {
    setLoading(true);
    setLoadError('');
    Promise.all([getSettings(), getAudit()])
      .then(([s, a]) => {
        setSettings(s);
        setCacheDraft(cacheDraftFrom(s.cache));
        setEvents(a.events || []);
      })
      .catch((e) => setLoadError(e instanceof Error ? e.message : 'Could not load settings'))
      .finally(() => setLoading(false));
  };

  const submitPassword = async (event: FormEvent) => {
    event.preventDefault();
    if (newPassword.length < 12 || new TextEncoder().encode(newPassword).length > 1024 || newPassword !== confirmPassword) {
      setPasswordError('New password must be 12+ characters, at most 1024 bytes, and match the confirmation.');
      return;
    }
    setBusy(true);
    setPasswordError('');
    try {
      // changePassword renews this session server-side; other owner sessions are revoked.
      await changePassword(currentPassword, newPassword);
      setCurrentPassword('');
      setNewPassword('');
      setConfirmPassword('');
      toast.success('Password changed. All other owner sessions were revoked.');
      void refreshAudit();
    } catch (e) {
      setPasswordError(e instanceof Error ? e.message : 'Password change failed');
    } finally {
      setBusy(false);
    }
  };

  const submitLimits = async (event: FormEvent) => {
    event.preventDefault();
    if (!settings) return;
    setBusy(true);
    setLimitsError('');
    try {
      if (Object.values(settings.limits).some((v) => !Number.isInteger(v) || v < 0)) {
        throw new Error('Limits must be non-negative integers.');
      }
      await saveSettings({ limits: settings.limits });
      toast.success('Limits saved.');
      void refreshAudit();
    } catch (e) {
      setLimitsError(e instanceof Error ? e.message : 'Save failed');
    } finally {
      setBusy(false);
    }
  };

  const submitCache = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setCacheError('');
    try {
      const ttl = Number(cacheDraft.ttl_seconds);
      const maxEntries = Number(cacheDraft.max_entries);
      const maxMib = Number(cacheDraft.max_mib);
      if (!Number.isInteger(ttl) || ttl < 1 || ttl > 86400) {
        throw new Error('TTL must be an integer between 1 and 86400 seconds.');
      }
      if (!Number.isInteger(maxEntries) || maxEntries < 1 || maxEntries > 100000) {
        throw new Error('Max entries must be an integer between 1 and 100000.');
      }
      if (!Number.isInteger(maxMib) || maxMib < 1 || maxMib > 1024) {
        throw new Error('Max memory must be an integer between 1 and 1024 MiB.');
      }
      const models = cacheDraft.models_text
        .split(/[\n,]/)
        .map((m) => m.trim())
        .filter(Boolean);
      const cache: ResponseCacheConfig = {
        enabled: cacheDraft.enabled,
        ttl_seconds: ttl,
        max_entries: maxEntries,
        max_bytes: maxMib * MIB,
        models,
      };
      await saveSettings({ cache });
      const fresh = await getSettings();
      setSettings(fresh);
      setCacheDraft(cacheDraftFrom(fresh.cache));
      toast.success(cache.enabled ? 'Response cache enabled · live apply.' : 'Response cache disabled · live apply.');
      void refreshAudit();
    } catch (e) {
      setCacheError(e instanceof Error ? e.message : 'Could not save cache settings');
    } finally {
      setBusy(false);
    }
  };

  const rulesText = rules ?? JSON.stringify(state?.alert_rules || [], null, 2);

  const addRule = () => {
    const id = newId.trim();
    if (!id) {
      setRulesError('Give the rule an id before adding it.');
      return;
    }
    const threshold = Number(newThreshold);
    if (!Number.isFinite(threshold)) {
      setRulesError('Threshold must be a number.');
      return;
    }
    let current: AlertRule[];
    try {
      const parsed: unknown = JSON.parse(rulesText);
      if (!Array.isArray(parsed)) throw new Error('not an array');
      current = parsed as AlertRule[];
    } catch {
      setRulesError('Fix the JSON below before adding a rule.');
      return;
    }
    if (current.some((r) => r && typeof r === 'object' && r.id === id)) {
      setRulesError(`A rule named "${id}" already exists.`);
      return;
    }
    current.push({ id, metric: newMetric, scope: 'global', threshold, window_sec: 300, cooldown_sec: 3600, severity: newSeverity });
    setRules(JSON.stringify(current, null, 2));
    setRulesError('');
    setNewId('');
  };

  const submitRules = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setRulesError('');
    try {
      const parsed: unknown = JSON.parse(rulesText);
      if (!Array.isArray(parsed)) throw new Error('Rules must be a JSON array.');
      for (const rule of parsed) {
        if (!rule || typeof rule !== 'object' || typeof (rule as AlertRule).id !== 'string' || typeof (rule as AlertRule).metric !== 'string' || typeof (rule as AlertRule).threshold !== 'number' || !Number.isFinite((rule as AlertRule).threshold)) {
          throw new Error('Each rule requires id, metric and a numeric threshold.');
        }
      }
      await saveSettings({ alerts: parsed as AlertRule[] });
      toast.success('Alert rules saved.');
      void refreshAudit();
    } catch (e) {
      setRulesError(e instanceof Error ? e.message : 'Could not save alert rules');
    } finally {
      setBusy(false);
    }
  };

  const runBackup = async () => {
    setBusy(true);
    setBackupError('');
    try {
      // Server-side snapshot: the response carries only the host path, never archive bytes.
      const response = await createBackup();
      setBackup(response.backup);
      toast.success('Private backup created.');
      void refreshAudit();
    } catch (e) {
      setBackupError(e instanceof Error ? e.message : 'Backup failed');
    } finally {
      setBusy(false);
    }
  };

  const cacheStats = settings?.cache_stats;
  const cacheTotal = (cacheStats?.hits || 0) + (cacheStats?.misses || 0);
  const cacheHitRate = cacheTotal > 0 ? `${(((cacheStats?.hits || 0) / cacheTotal) * 100).toFixed(1)}%` : '—';

  return (
    <div className="space-y-5">
      <nav aria-label="Settings sections" className="flex flex-wrap border border-edge-700 bg-edge-900">
        {[
          ['01', 'Owner access', 'owner-access'],
          ['02', 'Gateway limits', 'gateway-limits'],
          ['03', 'Response cache', 'response-cache'],
          ['04', 'Alert rules', 'alert-rules'],
          ['05', 'Backups', 'backups'],
          ['06', 'Connection', 'connection'],
          ['07', 'Audit', 'audit'],
        ].map(([index, label, id]) => (
          <a key={id} href={`#${id}`} className="flex items-center gap-2 border-r border-edge-700 px-4 py-3 text-[10px] transition-colors hover:bg-edge-800 hover:text-primary">
            <span className="text-primary">{index}</span><span className="text-muted-foreground">{label}</span>
          </a>
        ))}
      </nav>
      {loadError && (
        <div className="flex flex-wrap items-center gap-3 border border-destructive/40 bg-destructive/10 px-4 py-3 text-sm">
          <p role="alert" className="flex-1 text-destructive">{loadError}</p>
          <Button variant="outline" size="sm" onClick={reload}>Retry</Button>
        </div>
      )}

      <Card id="owner-access" className="scroll-mt-6 gap-0 overflow-hidden border-edge-700 bg-edge-900 py-0 lg:grid lg:grid-cols-[240px_minmax(0,1fr)]">
        <CardHeader className="flex flex-col items-start gap-3 border-b border-edge-700 bg-edge-950/40 p-6 lg:border-r lg:border-b-0">
          <span className="text-[10px] tracking-[0.16em] text-primary uppercase">01 / Identity</span>
          <CardTitle>Owner access</CardTitle>
          <CardDescription>Changing the password revokes every prior session and renews this one. Application API keys are unaffected.</CardDescription>
        </CardHeader>
        <CardContent className="p-6">
          <form className="space-y-5" onSubmit={(e) => void submitPassword(e)}>
            {passwordError && <p role="alert" className="text-sm text-destructive">{passwordError}</p>}
            <div className="grid grid-cols-1 gap-4 xl:grid-cols-3">
              <Field>
                <FieldLabel htmlFor="pw-current">Current password</FieldLabel>
                <Input id="pw-current" type="password" value={currentPassword} onChange={(e) => setCurrentPassword(e.target.value)} />
              </Field>
              <Field>
                <FieldLabel htmlFor="pw-new">New password (12+ characters)</FieldLabel>
                <Input id="pw-new" type="password" value={newPassword} onChange={(e) => setNewPassword(e.target.value)} />
              </Field>
              <Field>
                <FieldLabel htmlFor="pw-confirm">Confirm new password</FieldLabel>
                <Input id="pw-confirm" type="password" value={confirmPassword} onChange={(e) => setConfirmPassword(e.target.value)} />
              </Field>
            </div>
            <Button type="submit" disabled={busy || !currentPassword || !newPassword}>Change password</Button>
          </form>
        </CardContent>
      </Card>

      {loading && !settings && (
        <Card>
          <CardHeader>
            <CardTitle>Settings</CardTitle>
          </CardHeader>
          <CardContent>
            <div className="space-y-2">
              <Skeleton className="h-10 w-full" />
              <Skeleton className="h-10 w-full" />
              <Skeleton className="h-10 w-2/3" />
            </div>
          </CardContent>
        </Card>
      )}

      {settings && (
        <>
          <Card id="gateway-limits" className="scroll-mt-6 gap-0 overflow-hidden border-edge-700 bg-edge-900 py-0 lg:grid lg:grid-cols-[240px_minmax(0,1fr)]">
            <CardHeader className="flex flex-col items-start gap-3 border-b border-edge-700 bg-edge-950/40 p-6 lg:border-r lg:border-b-0">
              <span className="text-[10px] tracking-[0.16em] text-primary uppercase">02 / Runtime policy</span>
              <CardTitle>Gateway limits</CardTitle>
              <CardDescription>
                Gateway-wide admission and retry bounds apply to all applications. Analytics history
                is retained indefinitely, with no automatic expiry.
              </CardDescription>
            </CardHeader>
            <CardContent className="p-6">
              <form className="space-y-6" onSubmit={(e) => void submitLimits(e)}>
                {limitsError && <p role="alert" className="text-sm text-destructive">{limitsError}</p>}
                <div className="space-y-6">
                  {limitGroups.map((group) => (
                    <fieldset key={group.title} className="border border-edge-700 bg-edge-950/40 p-4">
                      <legend className="px-2 text-xs text-foreground">{group.title}</legend>
                      <p className="mb-4 text-[11px] text-muted-foreground">{group.description}</p>
                      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-3">
                        {group.keys.map((key) => (
                          <Field key={key}>
                            <FieldLabel htmlFor={`limit-${key}`}>{limitLabels[key]}</FieldLabel>
                            <Input
                              id={`limit-${key}`}
                              type="number"
                              min={0}
                              step={1}
                              value={String(settings.limits[key])}
                              onChange={(e) => setSettings({ ...settings, limits: { ...settings.limits, [key]: Number(e.target.value) } })}
                            />
                          </Field>
                        ))}
                      </div>
                    </fieldset>
                  ))}
                </div>
                <Button type="submit" disabled={busy}>{busy ? 'Saving…' : 'Save settings'}</Button>
              </form>
            </CardContent>
          </Card>

          <Card id="response-cache" className="scroll-mt-6 gap-0 overflow-hidden border-edge-700 bg-edge-900 py-0 lg:grid lg:grid-cols-[240px_minmax(0,1fr)]">
            <CardHeader className="flex flex-col items-start gap-3 border-b border-edge-700 bg-edge-950/40 p-6 lg:border-r lg:border-b-0">
              <span className="text-[10px] tracking-[0.16em] text-chart-3 uppercase">03 / Deterministic cache</span>
              <CardTitle>Response cache</CardTitle>
              <CardDescription>
                Opt-in in-memory LRU cache, strictly isolated per client API key. Caches non-streaming
                chat requests with temperature 0, embeddings, and System One decisions; honors
                Cache-Control: no-store / no-cache.
              </CardDescription>
              <div className="flex flex-wrap gap-1.5">
                <Badge variant="outline" className={settings.cache?.enabled ? 'border-primary/40 text-primary' : ''}>
                  {settings.cache?.enabled ? 'Enabled' : 'Disabled'}
                </Badge>
                {cacheStats && (
                  <Badge variant="outline">
                    {fmtInt(cacheStats.entries)} entries · {fmtBytes(cacheStats.bytes)}
                  </Badge>
                )}
              </div>
            </CardHeader>
            <CardContent className="p-6">
              <form className="space-y-5" onSubmit={(e) => void submitCache(e)}>
                {cacheError && <p role="alert" className="text-sm text-destructive">{cacheError}</p>}
                <div className="flex items-center justify-between gap-3 border border-edge-700 bg-edge-950/40 p-4">
                  <div>
                    <Label htmlFor="cache-enabled">Enable deterministic response cache</Label>
                    <FieldDescription>Responses carry X-Verloc-Cache: HIT or MISS. Saving rebuilds the in-memory cache.</FieldDescription>
                  </div>
                  <Switch
                    id="cache-enabled"
                    checked={cacheDraft.enabled}
                    onCheckedChange={(v) => setCacheDraft({ ...cacheDraft, enabled: v === true })}
                  />
                </div>
                {cacheStats && (
                  <div className="grid grid-cols-2 gap-px border border-edge-700 bg-edge-700 sm:grid-cols-4">
                    {[
                      { label: 'Hits', value: fmtInt(cacheStats.hits) },
                      { label: 'Misses', value: fmtInt(cacheStats.misses) },
                      { label: 'Hit rate', value: cacheHitRate },
                      { label: 'Memory', value: fmtBytes(cacheStats.bytes) },
                    ].map(({ label, value }) => (
                      <div key={label} className="bg-edge-950 p-3">
                        <div className="text-[10px] uppercase tracking-[0.12em] text-muted-foreground">{label}</div>
                        <div className="mt-1 font-mono text-sm tabular-nums text-foreground">{value}</div>
                      </div>
                    ))}
                  </div>
                )}
                <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
                  <Field>
                    <FieldLabel htmlFor="cache-ttl">TTL (seconds)</FieldLabel>
                    <Input
                      id="cache-ttl"
                      type="number"
                      min={1}
                      max={86400}
                      step={1}
                      value={cacheDraft.ttl_seconds}
                      onChange={(e) => setCacheDraft({ ...cacheDraft, ttl_seconds: e.target.value })}
                      className="font-mono"
                    />
                    <FieldDescription>Entry lifetime (1–86400s). Default 300s.</FieldDescription>
                  </Field>
                  <Field>
                    <FieldLabel htmlFor="cache-max-entries">Max entries</FieldLabel>
                    <Input
                      id="cache-max-entries"
                      type="number"
                      min={1}
                      max={100000}
                      step={1}
                      value={cacheDraft.max_entries}
                      onChange={(e) => setCacheDraft({ ...cacheDraft, max_entries: e.target.value })}
                      className="font-mono"
                    />
                    <FieldDescription>LRU entry ceiling (1–100000). Default 1024.</FieldDescription>
                  </Field>
                  <Field>
                    <FieldLabel htmlFor="cache-max-mib">Max memory (MiB)</FieldLabel>
                    <Input
                      id="cache-max-mib"
                      type="number"
                      min={1}
                      max={1024}
                      step={1}
                      value={cacheDraft.max_mib}
                      onChange={(e) => setCacheDraft({ ...cacheDraft, max_mib: e.target.value })}
                      className="font-mono"
                    />
                    <FieldDescription>Total byte budget (1–1024 MiB). Default 64 MiB.</FieldDescription>
                  </Field>
                </div>
                <Field>
                  <FieldLabel htmlFor="cache-models">Allowed models (optional)</FieldLabel>
                  <Input
                    id="cache-models"
                    value={cacheDraft.models_text}
                    onChange={(e) => setCacheDraft({ ...cacheDraft, models_text: e.target.value })}
                    placeholder="Leave blank for all models, or comma-separated model IDs"
                    className="font-mono"
                  />
                  <FieldDescription>Restrict caching to specific public Verloc model IDs. Empty allows every eligible model.</FieldDescription>
                </Field>
                <div className="flex flex-wrap items-center gap-2">
                  <Button type="submit" disabled={busy}>{busy ? 'Saving…' : 'Save cache policy'}</Button>
                  <Button type="button" variant="outline" size="sm" disabled={busy || loading} onClick={reload}>
                    <RefreshCw className="size-3.5" />Refresh stats
                  </Button>
                </div>
              </form>
            </CardContent>
          </Card>

          <Card id="alert-rules" className="scroll-mt-6 gap-0 overflow-hidden border-edge-700 bg-edge-900 py-0 lg:grid lg:grid-cols-[240px_minmax(0,1fr)]">
            <CardHeader className="flex flex-col items-start gap-3 border-b border-edge-700 bg-edge-950/40 p-6 lg:border-r lg:border-b-0">
              <span className="text-[10px] tracking-[0.16em] text-chart-2 uppercase">04 / Notifications</span>
              <CardTitle>Alert policy</CardTitle>
              <CardDescription>
                Rules persist with gateway settings. Spend thresholds notify but do not enforce a
                billing budget. Each rule accepts id, metric, scope, threshold, window_sec,
                cooldown_sec and severity.
              </CardDescription>
              <Badge variant="outline">{(state?.alert_rules || []).length} saved rules</Badge>
            </CardHeader>
            <CardContent className="p-6">
              <form className="space-y-4" onSubmit={(e) => void submitRules(e)}>
                <div className="grid grid-cols-1 gap-4 border border-edge-700 bg-edge-950/40 p-4 sm:grid-cols-2 2xl:grid-cols-4">
                  <Field>
                    <FieldLabel htmlFor="rule-id">Rule id</FieldLabel>
                    <Input id="rule-id" value={newId} onChange={(e) => setNewId(e.target.value)} placeholder="high-error-rate" />
                  </Field>
                  <Field>
                    <FieldLabel htmlFor="rule-metric">Metric</FieldLabel>
                    <Select
                      value={newMetric}
                      onValueChange={(v) => {
                        setNewMetric(v ?? 'error_rate');
                        setNewThreshold(String(METRICS[v ?? 'error_rate']?.threshold ?? 0));
                      }}
                    >
                      <SelectTrigger id="rule-metric" className="w-full">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {Object.keys(METRICS).map((m) => (
                          <SelectItem key={m} value={m}>{m}</SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </Field>
                  <Field>
                    <FieldLabel htmlFor="rule-threshold">Threshold</FieldLabel>
                    <Input id="rule-threshold" type="number" value={newThreshold} onChange={(e) => setNewThreshold(e.target.value)} />
                  </Field>
                  <Field>
                    <FieldLabel htmlFor="rule-severity">Severity</FieldLabel>
                    <Select value={newSeverity} onValueChange={(v) => setNewSeverity(v ?? 'warning')}>
                      <SelectTrigger id="rule-severity" className="w-full">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {SEVERITIES.map((s) => (
                          <SelectItem key={s} value={s}>{s}</SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </Field>
                  <div className="flex items-end sm:col-span-2 2xl:col-span-4">
                    <Button type="button" variant="outline" onClick={addRule}><Plus />Add rule</Button>
                  </div>
                </div>
                <div className="flex items-center justify-between gap-3 border-y border-edge-700 py-4">
                  <div>
                    <Label htmlFor="metric-help">Metric reference</Label>
                    <FieldDescription>What each metric measures.</FieldDescription>
                  </div>
                  <Switch id="metric-help" checked={showHelp} onCheckedChange={(v) => setShowHelp(v === true)} />
                </div>
                {showHelp && (
                  <ul className="space-y-3 border border-edge-700 bg-edge-950 p-4 text-xs">
                    {Object.entries(METRICS).map(([metric, meta]) => (
                      <li key={metric} className="flex flex-col gap-1 sm:flex-row sm:gap-3">
                        <code className="shrink-0 font-mono text-foreground">{metric}</code>
                        <span className="text-muted-foreground">{meta.help}</span>
                      </li>
                    ))}
                  </ul>
                )}
                {rulesError && <p role="alert" className="text-sm text-destructive">{rulesError}</p>}
                <Field>
                  <FieldLabel htmlFor="rules-json">Alert rules (JSON)</FieldLabel>
                  <Textarea id="rules-json" value={rulesText} onChange={(e) => setRules(e.target.value)} rows={12} spellCheck={false} className="bg-edge-950 text-xs leading-relaxed" />
                </Field>
                <Button type="submit" disabled={busy}>{busy ? 'Saving…' : 'Save alert rules'}</Button>
              </form>
            </CardContent>
          </Card>

          <div className="grid grid-cols-1 items-start gap-5 xl:grid-cols-2">
            <Card id="backups" className="scroll-mt-6 gap-0 overflow-hidden border-edge-700 bg-edge-900">
              <CardHeader className="border-b border-edge-700 pb-5">
                <span className="text-[10px] tracking-[0.16em] text-chart-3 uppercase">05 / Recovery</span>
                <CardTitle className="flex items-center gap-2"><Database className="size-4 text-chart-3" />Private backups</CardTitle>
                <CardDescription>
                  Consistent server-side snapshot of management and analytics data. Backups contain
                  encrypted upstream credentials — protect them like the original database. Restore
                  using the CLI while the gateway is stopped; no secrets are downloaded by this page.
                </CardDescription>
              </CardHeader>
              <CardContent className="space-y-4 pt-5">
                <Button disabled={busy} onClick={() => void runBackup()}>
                  <Database />{busy ? 'Creating backup…' : 'Create backup'}
                </Button>
                {backupError && <p role="alert" className="text-sm text-destructive">{backupError}</p>}
                {backup && (
                  <div role="status" className="space-y-2 border border-primary/30 bg-primary/5 p-4">
                    <p className="text-xs text-muted-foreground">Backup stored on the gateway host at:</p>
                    <code className="block font-mono text-xs break-all select-all">{backup}</code>
                    <p className="text-xs text-muted-foreground">Copy it off the host yourself; the master-key file must be backed up separately.</p>
                  </div>
                )}
              </CardContent>
            </Card>

            <Card id="connection" className="scroll-mt-6 gap-0 overflow-hidden border-edge-700 bg-edge-900">
              <CardHeader className="border-b border-edge-700 pb-5">
                <span className="text-[10px] tracking-[0.16em] text-chart-2 uppercase">06 / Infrastructure</span>
                <CardTitle>Connection & storage</CardTitle>
                <CardDescription>How applications reach this gateway and where its data lives.</CardDescription>
              </CardHeader>
              <CardContent className="space-y-4 pt-5 text-xs leading-relaxed">
                <div className="border border-edge-700 bg-edge-950 p-4"><p className="mb-2 text-[10px] tracking-widest text-muted-foreground uppercase">OpenAI-compatible base URL</p><code className="break-all text-primary select-all">{location.origin}/v1</code></div>
                <p>Use <code className="font-mono text-foreground">Authorization: Bearer YOUR_CLIENT_API_KEY</code>. Create keys on API Keys and select configured Verloc model IDs. Owner passwords and provider credentials are not inference credentials.</p>
                <p>
                  Session cookie transport:{' '}
                  <Tooltip>
                    <TooltipTrigger className="font-medium text-foreground underline decoration-dotted underline-offset-4">
                      {settings.secure_cookies || location.protocol === 'https:' ? 'Secure / HTTPS' : 'HTTP — use HTTPS before exposing this gateway'}
                    </TooltipTrigger>
                    <TooltipContent>Owner sessions ride an HttpOnly cookie. Without HTTPS the cookie — and gateway control — travel in cleartext.</TooltipContent>
                  </Tooltip>
                </p>
                <dl className="grid gap-3 border-t border-edge-700 pt-4 text-[11px]">
                  <div><dt className="mb-1 text-muted-foreground">Management database</dt><dd className="break-all">{settings.management_db}</dd></div>
                  <div><dt className="mb-1 text-muted-foreground">Analytics database</dt><dd className="break-all">{settings.analytics_db}</dd></div>
                </dl>
                <div className="flex gap-2"><Badge variant="outline">{settings.provider_count} providers</Badge><Badge variant="outline">{settings.model_count} models</Badge></div>
              </CardContent>
            </Card>
          </div>
        </>
      )}

      <Card id="audit" className="scroll-mt-6 gap-0 overflow-hidden border-edge-700 bg-edge-900 py-0 lg:grid lg:grid-cols-[240px_minmax(0,1fr)]">
        <CardHeader className="flex flex-col items-start gap-3 border-b border-edge-700 bg-edge-950/40 p-6 lg:border-r lg:border-b-0">
          <span className="text-[10px] tracking-[0.16em] text-primary uppercase">07 / Accountability</span>
          <CardTitle className="flex items-center gap-2"><History className="size-4" />Audit history</CardTitle>
          <CardDescription>Recorded management actions and their subjects.</CardDescription>
          <CardAction className="mt-2 self-start">
            <Button variant="outline" size="sm" disabled={auditBusy} onClick={() => void refreshAudit()}>
              <RefreshCw />{auditBusy ? 'Refreshing…' : 'Refresh audit'}
            </Button>
          </CardAction>
        </CardHeader>
        <CardContent className="min-w-0 p-0">
          {(loading || auditBusy) && events.length === 0 ? (
            <div className="space-y-3 p-6">
              <Skeleton className="h-8 w-full" />
              <Skeleton className="h-8 w-full" />
              <Skeleton className="h-8 w-2/3" />
            </div>
          ) : events.length === 0 ? (
            <Empty>
              <EmptyHeader>
                <EmptyTitle>No audit events</EmptyTitle>
                <EmptyDescription>Management actions will appear here.</EmptyDescription>
              </EmptyHeader>
            </Empty>
          ) : (
            <Table className="min-w-[520px]">
              <TableHeader>
                <TableRow>
                  <TableHead>Time</TableHead>
                  <TableHead>Action</TableHead>
                  <TableHead>Subject</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {events.map((event) => (
                  <TableRow key={event.id}>
                    <TableCell className="py-4 pl-6 text-[11px] whitespace-nowrap text-muted-foreground">{fmtDateTime(event.ts)}</TableCell>
                    <TableCell className="text-xs text-chart-2">{event.action}</TableCell>
                    <TableCell className="pr-6 text-xs break-all">{event.subject}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
