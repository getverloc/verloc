import { useEffect, useRef, useState, type FormEvent } from 'react';
import { toast } from 'sonner';
import {
  ArrowUpCircle,
  CheckCircle2,
  Copy,
  Download,
  ExternalLink,
  RefreshCw,
  ShieldCheck,
  Terminal,
  TriangleAlert,
} from 'lucide-react';
import {
  applyUpdate,
  checkForUpdates,
  fmtDateTime,
  fmtVersion,
  getState,
  getUpdateStatus,
  saveSettings,
  type BuildInfo,
  type UpdateConfig,
  type UpdateStatus,
} from '../api';
import { Badge } from './ui/badge';
import { Button } from './ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from './ui/card';
import { Field, FieldDescription, FieldLabel } from './ui/field';
import { Label } from './ui/label';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from './ui/select';
import { Skeleton } from './ui/skeleton';
import { Switch } from './ui/switch';

interface UpdateSectionProps {
  onAuditRefresh?: () => void;
}

const ACTIVE_STATES = new Set(['downloading', 'verifying', 'installing', 'restarting']);

const INTERVAL_OPTIONS: { value: string; label: string }[] = [
  { value: '1', label: 'Every hour' },
  { value: '6', label: 'Every 6 hours' },
  { value: '12', label: 'Every 12 hours' },
  { value: '24', label: 'Daily (24 hours)' },
  { value: '168', label: 'Weekly (7 days)' },
];

function fmtBuildTime(raw?: string): string {
  if (!raw || raw === 'unknown') return '—';
  const d = new Date(raw);
  if (Number.isNaN(d.getTime())) return raw;
  return d.toLocaleString();
}

function fmtSizeMB(bytes?: number): string {
  if (!bytes || bytes <= 0) return '';
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

function channelBadgeClass(channel?: string): string {
  if (channel === 'stable') return 'border-primary/40 bg-primary/5 text-primary';
  if (channel === 'prerelease') return 'border-amber-500/40 bg-amber-500/5 text-amber-300';
  return 'border-edge-600 text-muted-foreground';
}

export function UpdateSection({ onAuditRefresh }: UpdateSectionProps) {
  const [status, setStatus] = useState<UpdateStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const [checking, setChecking] = useState(false);
  const [saving, setSaving] = useState(false);
  const [reconnecting, setReconnecting] = useState(false);
  const [targetVersion, setTargetVersion] = useState('');
  const [error, setError] = useState('');

  // Editable policy draft
  const [autoCheck, setAutoCheck] = useState(true);
  const [autoUpdate, setAutoUpdate] = useState(false);
  const [intervalHours, setIntervalHours] = useState('6');
  const [includePrereleases, setIncludePrereleases] = useState(false);
  const policyInitRef = useRef(false);

  const syncPolicyDraft = (policy: UpdateConfig) => {
    setAutoCheck(policy.auto_check !== false);
    setAutoUpdate(!!policy.auto_update);
    const hrs = policy.check_interval_hours || 6;
    setIntervalHours(String(INTERVAL_OPTIONS.some((o) => Number(o.value) === hrs) ? hrs : 6));
    setIncludePrereleases(!!policy.include_prereleases);
  };

  useEffect(() => {
    let active = true;
    getUpdateStatus()
      .then((res) => {
        if (!active) return;
        setStatus(res);
        if (!policyInitRef.current) {
          policyInitRef.current = true;
          syncPolicyDraft(res.policy);
        }
        setError('');
      })
      .catch((e) => {
        if (active) setError(e instanceof Error ? e.message : 'Could not load update status');
      })
      .finally(() => {
        if (active) setLoading(false);
      });
    return () => { active = false; };
  }, []);

  const isUpdating = !!status && ACTIVE_STATES.has(status.state);

  // Poll live progress while downloading / verifying / installing / restarting.
  useEffect(() => {
    if (!isUpdating || reconnecting) return;
    let cancelled = false;
    const timer = setInterval(async () => {
      try {
        const fresh = await getUpdateStatus();
        if (cancelled) return;
        setStatus(fresh);
        if (fresh.state === 'restarting') {
          setReconnecting(true);
        }
      } catch {
        // Connection drop right after "installing"/"restarting" means the new
        // process is executing its graceful swap & exec restart.
        if (!cancelled) {
          setReconnecting(true);
        }
      }
    }, 800);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [isUpdating, reconnecting]);

  // Smooth post-restart reconnection: wait for the updated gateway to come back
  // online, then reload the dashboard so the new embedded SPA assets take effect.
  useEffect(() => {
    if (!reconnecting) return;
    let cancelled = false;
    const prevVersion = status?.current.version || '';
    const startedAt = Date.now();
    const poll = async () => {
      while (!cancelled && Date.now() - startedAt < 60_000) {
        await new Promise((r) => setTimeout(r, 1200));
        if (cancelled) return;
        try {
          const freshState = await getState();
          if (cancelled) return;
          const nextVer = freshState.build?.version || freshState.version;
          const changed = !!nextVer && nextVer !== prevVersion;
          const freshUp = await getUpdateStatus().catch(() => null);
          if (changed || (freshUp && freshUp.state !== 'restarting' && !ACTIVE_STATES.has(freshUp.state))) {
            toast.success(`Updated to ${fmtVersion(nextVer || targetVersion)} — reloading dashboard…`);
            setTimeout(() => window.location.reload(), 900);
            return;
          }
        } catch {
          // Gateway is still draining/restarting; keep polling.
        }
      }
      if (!cancelled) {
        setReconnecting(false);
        setError('Gateway is taking longer than expected to reconnect. Refresh this page in a moment.');
      }
    };
    void poll();
    return () => { cancelled = true; };
  }, [reconnecting, status?.current.version, targetVersion]);

  const handleCheck = async () => {
    setChecking(true);
    setError('');
    try {
      const fresh = await checkForUpdates();
      setStatus(fresh);
      if (fresh.last_error) {
        setError(fresh.last_error);
      } else if (fresh.update_available && fresh.latest) {
        toast.info(`New release available: ${fmtVersion(fresh.latest.version)}`);
      } else {
        toast.success(`You are up to date (${fmtVersion(fresh.current.version)}).`);
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Update check failed');
    } finally {
      setChecking(false);
    }
  };

  const handleApply = async () => {
    if (!status?.latest) return;
    setError('');
    setTargetVersion(status.latest.version);
    try {
      const started = await applyUpdate();
      setStatus(started);
      toast.info(`Updating verloc to ${fmtVersion(status.latest.version)}…`);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Could not start update');
    }
  };

  const handleSavePolicy = async (event: FormEvent) => {
    event.preventDefault();
    setSaving(true);
    setError('');
    try {
      const update: UpdateConfig = {
        auto_check: autoCheck,
        auto_update: autoUpdate,
        check_interval_hours: Number(intervalHours) || 6,
        include_prereleases: includePrereleases,
        repo: status?.policy.repo,
      };
      await saveSettings({ update });
      const fresh = await getUpdateStatus();
      setStatus(fresh);
      syncPolicyDraft(fresh.policy);
      toast.success(autoUpdate ? 'Auto-update policy enabled.' : 'Update policy saved.');
      onAuditRefresh?.();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Could not save update policy');
    } finally {
      setSaving(false);
    }
  };

  const copyCommand = async (cmd: string) => {
    try {
      await navigator.clipboard.writeText(cmd);
      toast.success('Update command copied.');
    } catch {
      toast.error('Could not copy command to clipboard.');
    }
  };

  const build: BuildInfo | undefined = status?.current;
  const latest = status?.latest;
  const progress = reconnecting ? 100 : (status?.progress_pct || 0);

  return (
    <Card id="updates" className="scroll-mt-6 gap-0 overflow-hidden border-edge-700 bg-edge-900 py-0 lg:grid lg:grid-cols-[240px_minmax(0,1fr)]">
      <CardHeader className="flex flex-col items-start gap-3 border-b border-edge-700 bg-edge-950/40 p-6 lg:border-r lg:border-b-0">
        <span className="text-[10px] tracking-[0.16em] text-primary uppercase">05 / Software updates</span>
        <CardTitle className="flex items-center gap-2">
          <ArrowUpCircle className="size-4 text-primary" />
          Version &amp; updates
        </CardTitle>
        <CardDescription>
          Check GitHub Releases, verify SHA-256 checksums, and smoothly update the verloc binary in
          place. Databases, encrypted credentials, API keys, and configuration are never deleted.
        </CardDescription>
        {build && (
          <div className="flex flex-wrap gap-1.5">
            <Badge variant="outline" className="font-mono text-foreground">
              {fmtVersion(build.version)}
            </Badge>
            <Badge variant="outline" className={channelBadgeClass(build.channel)}>
              {build.channel}
            </Badge>
            {status?.policy.auto_update && status.can_self_update && (
              <Badge variant="outline" className="border-primary/40 text-primary">
                Auto-update on
              </Badge>
            )}
          </div>
        )}
      </CardHeader>

      <CardContent className="space-y-6 p-6">
        {loading && !status ? (
          <div className="space-y-3">
            <Skeleton className="h-16 w-full" />
            <Skeleton className="h-24 w-full" />
          </div>
        ) : (
          <>
            {build && (
              <div className="grid grid-cols-2 gap-px border border-edge-700 bg-edge-700 sm:grid-cols-4">
                {[
                  { label: 'Version', value: fmtVersion(build.version) },
                  { label: 'Commit', value: build.commit || '—' },
                  { label: 'Built', value: fmtBuildTime(build.build_time) },
                  { label: 'Runtime', value: `${build.os}/${build.arch} · ${build.go_version}` },
                ].map(({ label, value }) => (
                  <div key={label} className="bg-edge-950 p-3">
                    <div className="text-[10px] uppercase tracking-[0.12em] text-muted-foreground">{label}</div>
                    <div className="mt-1 truncate font-mono text-xs text-foreground" title={value}>{value}</div>
                  </div>
                ))}
              </div>
            )}

            {(error || status?.last_error) && (
              <div role="alert" className="flex items-start gap-2.5 border border-destructive/40 bg-destructive/10 px-4 py-3 text-xs text-destructive">
                <TriangleAlert className="mt-0.5 size-4 shrink-0" />
                <span>{error || status?.last_error}</span>
              </div>
            )}

            {/* Active update / restart progress */}
            {(isUpdating || reconnecting) && (
              <div role="status" className="space-y-3 border border-primary/40 bg-primary/5 p-4">
                <div className="flex items-center justify-between gap-3 text-xs">
                  <span className="flex items-center gap-2 font-medium text-primary">
                    <RefreshCw className="size-3.5 animate-spin" />
                    {reconnecting
                      ? `Restarting gateway and reconnecting to ${fmtVersion(targetVersion || latest?.version)}…`
                      : (status?.message || `Updating (${status?.state})…`)}
                  </span>
                  <span className="font-mono text-[11px] tabular-nums text-primary">{progress}%</span>
                </div>
                <div className="h-1.5 w-full overflow-hidden bg-edge-800">
                  <div
                    className="h-full bg-primary transition-all duration-300"
                    style={{ width: `${Math.max(8, Math.min(100, progress))}%` }}
                  />
                </div>
                <p className="text-[11px] text-muted-foreground">
                  Active requests drain cleanly before the new binary takes over. Your SQLite databases and keys are untouched.
                </p>
              </div>
            )}

            {/* Update available panel */}
            {!isUpdating && !reconnecting && status?.update_available && latest && (
              <div className="space-y-4 border border-primary/35 bg-primary/5 p-4">
                <div className="flex flex-wrap items-start justify-between gap-3">
                  <div className="space-y-1">
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="font-mono text-sm font-medium text-foreground">
                        {fmtVersion(latest.version)} available
                      </span>
                      {latest.prerelease && (
                        <Badge variant="outline" className="border-amber-500/40 text-amber-300">Pre-release</Badge>
                      )}
                      {latest.asset_size ? (
                        <span className="text-[11px] text-muted-foreground">{fmtSizeMB(latest.asset_size)}</span>
                      ) : null}
                    </div>
                    <p className="text-[11px] text-muted-foreground">
                      {latest.published_at ? `Published ${fmtBuildTime(latest.published_at)}` : 'New release published on GitHub'}
                      {status.last_checked_at ? ` · Checked ${fmtDateTime(status.last_checked_at)}` : ''}
                    </p>
                  </div>
                  {latest.url && (
                    <a
                      href={latest.url}
                      target="_blank"
                      rel="noreferrer"
                      className="inline-flex items-center gap-1.5 text-xs text-primary hover:underline"
                    >
                      Release notes <ExternalLink className="size-3" />
                    </a>
                  )}
                </div>

                {latest.notes && (
                  <details className="border border-edge-700 bg-edge-950/80">
                    <summary className="cursor-pointer px-3 py-2 text-[11px] text-edge-300">
                      View changelog
                    </summary>
                    <pre className="max-h-56 overflow-auto border-t border-edge-700 p-3 font-mono text-[11px] leading-relaxed whitespace-pre-wrap text-muted-foreground">
                      {latest.notes}
                    </pre>
                  </details>
                )}

                {status.can_self_update && latest.asset_ready ? (
                  <div className="space-y-2">
                    <div className="flex flex-wrap items-center gap-2">
                      <Button disabled={checking || saving} onClick={() => void handleApply()}>
                        <Download className="size-3.5" />
                        Update to {fmtVersion(latest.version)}
                      </Button>
                      <Button variant="outline" size="sm" disabled={checking} onClick={() => void handleCheck()}>
                        <RefreshCw className={checking ? 'size-3.5 animate-spin' : 'size-3.5'} />
                        {checking ? 'Checking…' : 'Recheck'}
                      </Button>
                    </div>
                    <p className="flex items-center gap-1.5 text-[11px] text-muted-foreground">
                      <ShieldCheck className="size-3.5 shrink-0 text-primary" />
                      Verified via SHA-256 checksum, backs up previous binary, and preserves all user data.
                    </p>
                  </div>
                ) : (
                  <div className="space-y-2.5 border border-edge-700 bg-edge-950/70 p-3 text-xs">
                    <p className="text-muted-foreground">
                      {!status.can_self_update
                        ? status.cannot_update_reason
                        : `Release ${fmtVersion(latest.version)} does not yet include a verified ${build?.os}/${build?.arch} archive.`}
                    </p>
                    {status.update_command && (
                      <div className="flex items-center justify-between gap-2 border border-edge-700 bg-edge-950 px-3 py-2">
                        <code className="flex items-center gap-2 overflow-x-auto font-mono text-[11px] text-primary">
                          <Terminal className="size-3.5 shrink-0 text-muted-foreground" />
                          {status.update_command}
                        </code>
                        <Button
                          type="button"
                          variant="ghost"
                          size="icon-sm"
                          aria-label="Copy update command"
                          onClick={() => void copyCommand(status.update_command!)}
                        >
                          <Copy className="size-3.5" />
                        </Button>
                      </div>
                    )}
                  </div>
                )}
              </div>
            )}

            {/* Up-to-date panel */}
            {!isUpdating && !reconnecting && status && !status.update_available && (
              <div className="flex flex-wrap items-center justify-between gap-3 border border-edge-700 bg-edge-950/50 p-4">
                <div className="flex items-start gap-2.5">
                  <CheckCircle2 className="mt-0.5 size-4 shrink-0 text-primary" />
                  <div className="space-y-0.5">
                    <div className="text-xs font-medium text-foreground">
                      {latest
                        ? `Up to date — running ${fmtVersion(status.current.version)}`
                        : (status.message || `Running ${fmtVersion(status.current.version)}`)}
                    </div>
                    <div className="text-[11px] text-muted-foreground">
                      {status.last_checked_at
                        ? `Last checked ${fmtDateTime(status.last_checked_at)}`
                        : 'Not checked yet in this session'}
                      {latest ? ` · Latest release: ${fmtVersion(latest.version)}` : ''}
                    </div>
                  </div>
                </div>
                <Button variant="outline" size="sm" disabled={checking} onClick={() => void handleCheck()}>
                  <RefreshCw className={checking ? 'size-3.5 animate-spin' : 'size-3.5'} />
                  {checking ? 'Checking…' : 'Check for updates'}
                </Button>
              </div>
            )}

            {/* Automatic update policy */}
            <form className="space-y-4 border-t border-edge-700 pt-5" onSubmit={(e) => void handleSavePolicy(e)}>
              <div className="flex items-center justify-between gap-3 border border-edge-700 bg-edge-950/40 p-4">
                <div>
                  <Label htmlFor="update-auto-check">Check for new releases automatically</Label>
                  <FieldDescription>
                    Periodically polls GitHub Releases using ETag caching and surfaces new versions in the dashboard.
                  </FieldDescription>
                </div>
                <Switch
                  id="update-auto-check"
                  checked={autoCheck}
                  onCheckedChange={(v) => {
                    const next = v === true;
                    setAutoCheck(next);
                    if (!next) setAutoUpdate(false);
                  }}
                />
              </div>

              <div className="flex items-center justify-between gap-3 border border-edge-700 bg-edge-950/40 p-4">
                <div>
                  <Label htmlFor="update-auto-apply">Automatically install new releases</Label>
                  <FieldDescription>
                    {status && !status.can_self_update
                      ? status.cannot_update_reason
                      : 'Downloads SHA-256 verified releases, backs up the previous executable, drains requests, and restarts automatically with zero data loss.'}
                  </FieldDescription>
                </div>
                <Switch
                  id="update-auto-apply"
                  checked={autoUpdate}
                  disabled={!autoCheck || (!!status && !status.can_self_update)}
                  onCheckedChange={(v) => setAutoUpdate(v === true)}
                />
              </div>

              <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                <Field>
                  <FieldLabel htmlFor="update-interval">Check interval</FieldLabel>
                  <Select value={intervalHours} onValueChange={(v) => setIntervalHours(v ?? '6')}>
                    <SelectTrigger id="update-interval" className="w-full">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {INTERVAL_OPTIONS.map((opt) => (
                        <SelectItem key={opt.value} value={opt.value}>{opt.label}</SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <FieldDescription>How often verloc checks for published releases.</FieldDescription>
                </Field>

                <div className="flex items-center justify-between gap-3 border border-edge-700 bg-edge-950/40 px-4 py-3">
                  <div>
                    <Label htmlFor="update-prerelease">Include pre-releases</Label>
                    <FieldDescription>Opt into beta / release candidate tags (e.g. v1.2.0-rc.1).</FieldDescription>
                  </div>
                  <Switch
                    id="update-prerelease"
                    checked={includePrereleases}
                    onCheckedChange={(v) => setIncludePrereleases(v === true)}
                  />
                </div>
              </div>

              <div className="flex flex-wrap items-center gap-2">
                <Button type="submit" disabled={saving || isUpdating}>
                  {saving ? 'Saving…' : 'Save update policy'}
                </Button>
                {status?.executable_path && (
                  <span className="truncate font-mono text-[10px] text-muted-foreground" title={status.executable_path}>
                    Binary: {status.executable_path}
                  </span>
                )}
              </div>
            </form>
          </>
        )}
      </CardContent>
    </Card>
  );
}
