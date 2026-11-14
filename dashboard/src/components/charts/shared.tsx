// Shared analytics chrome. Every visualization uses real gateway payloads.
import type { ReactNode } from 'react';
import { Button } from '../ui/button';
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from '../ui/card';
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from '../ui/empty';
import { Skeleton } from '../ui/skeleton';

export function KpiCard({ label, value, sub, accent }: {
  label: string; value: ReactNode; sub?: ReactNode; accent?: string;
}) {
  return (
    <Card size="sm" className="min-w-0 gap-3 rounded-none border-edge-700 bg-edge-900/95 py-5 shadow-none">
      <CardHeader className="gap-3 px-5">
        <CardDescription className="text-[10px] uppercase tracking-[.16em] text-edge-400">{label}</CardDescription>
        <CardTitle className={`break-words text-2xl font-medium leading-tight tracking-tight tabular-nums ${accent || 'text-edge-100'}`}>{value}</CardTitle>
      </CardHeader>
      {sub !== undefined && <CardContent className="px-5 text-[10px] leading-relaxed text-edge-400">{sub}</CardContent>}
    </Card>
  );
}

export function PanelCard({ title, description, action, children, className }: {
  title: ReactNode; description?: ReactNode; action?: ReactNode; children: ReactNode; className?: string;
}) {
  return (
    <Card className={`min-w-0 gap-0 rounded-none border-edge-700 bg-edge-900/95 py-0 shadow-none ${className || ''}`}>
      <CardHeader className="gap-1.5 border-b border-edge-700 px-5 py-4">
        <CardTitle className="text-xs font-medium tracking-wide text-edge-100">{title}</CardTitle>
        {description !== undefined && <CardDescription className="max-w-3xl text-[11px] leading-relaxed text-edge-400">{description}</CardDescription>}
        {action !== undefined && <CardAction className="max-sm:col-start-1 max-sm:row-start-auto max-sm:mt-2 max-sm:justify-self-start">{action}</CardAction>}
      </CardHeader>
      <CardContent className="min-w-0 p-5">{children}</CardContent>
    </Card>
  );
}

export function ChartSkeleton({ height = 240 }: { height?: number }) {
  return <div className="space-y-3" role="status" aria-label="Loading chart"><Skeleton className="w-full rounded-none" style={{ height }} /><div className="flex gap-3"><Skeleton className="h-2 flex-1 rounded-none" /><Skeleton className="h-2 flex-1 rounded-none" /><Skeleton className="h-2 flex-1 rounded-none" /></div></div>;
}

export function ChartEmptyState({ title, description }: { title: string; description?: string }) {
  return <Empty className="min-h-56 rounded-none border-edge-700 bg-edge-950/50"><EmptyHeader><EmptyTitle className="text-sm">{title}</EmptyTitle>{description !== undefined && <EmptyDescription className="text-xs">{description}</EmptyDescription>}</EmptyHeader></Empty>;
}

export function InlineError({ message, onRetry }: { message: string; onRetry?: () => void }) {
  return <div role="alert" className="flex flex-wrap items-center justify-between gap-3 border border-destructive/40 bg-destructive/10 px-4 py-3 text-xs leading-relaxed text-destructive"><span>{message}</span>{onRetry && <Button type="button" size="sm" variant="outline" onClick={onRetry}>Retry</Button>}</div>;
}
