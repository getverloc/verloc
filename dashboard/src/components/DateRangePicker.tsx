import { useMemo, useState } from 'react';
import { CalendarRange, ChevronLeft, ChevronRight, Radio } from 'lucide-react';
import { Button } from './ui/button';
import { Input } from './ui/input';
import { Label } from './ui/label';
import { Popover, PopoverContent, PopoverTrigger } from './ui/popover';
import { Switch } from './ui/switch';
import { endOfDay, makeTimeRange, startOfDay, type TimeRange } from '../time-range';

const WEEKDAYS = ['Mo', 'Tu', 'We', 'Th', 'Fr', 'Sa', 'Su'] as const;

function toLocalDateInput(ts: number): string {
  const d = new Date(ts);
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, '0');
  const day = String(d.getDate()).padStart(2, '0');
  return `${y}-${m}-${day}`;
}

function toLocalTimeInput(ts: number): string {
  const d = new Date(ts);
  const hh = String(d.getHours()).padStart(2, '0');
  const mm = String(d.getMinutes()).padStart(2, '0');
  return `${hh}:${mm}`;
}

function combineDateAndTime(dateStr: string, timeStr: string, fallbackEnd: boolean): number | null {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(dateStr)) return null;
  const [y, m, d] = dateStr.split('-').map(Number);
  let hh = fallbackEnd ? 23 : 0;
  let mm = fallbackEnd ? 59 : 0;
  let ss = fallbackEnd ? 59 : 0;
  let ms = fallbackEnd ? 999 : 0;
  if (/^\d{2}:\d{2}$/.test(timeStr)) {
    const [th, tm] = timeStr.split(':').map(Number);
    hh = th;
    mm = tm;
    ss = fallbackEnd ? 59 : 0;
    ms = fallbackEnd ? 999 : 0;
  }
  const dt = new Date(y, m - 1, d, hh, mm, ss, ms);
  return Number.isFinite(dt.getTime()) ? dt.getTime() : null;
}

function monthMatrix(year: number, month: number): { ts: number; day: number; inMonth: boolean }[] {
  const first = new Date(year, month, 1);
  // Monday = 0 .. Sunday = 6
  const lead = (first.getDay() + 6) % 7;
  const start = new Date(year, month, 1 - lead);
  const cells: { ts: number; day: number; inMonth: boolean }[] = [];
  for (let i = 0; i < 42; i++) {
    const cur = new Date(start.getFullYear(), start.getMonth(), start.getDate() + i);
    cells.push({
      ts: cur.getTime(),
      day: cur.getDate(),
      inMonth: cur.getMonth() === month,
    });
  }
  return cells;
}

export function DateRangePicker({
  range,
  onChange,
}: {
  range: TimeRange;
  onChange: (next: TimeRange) => void;
}) {
  const [open, setOpen] = useState(false);
  const [openedAt, setOpenedAt] = useState(Date.now);
  const [cursorMonth, setCursorMonth] = useState(() => {
    const d = new Date(range.to ?? Date.now());
    return { year: d.getFullYear(), month: d.getMonth() };
  });
  const [fromDate, setFromDate] = useState(() => toLocalDateInput(range.from));
  const [fromTime, setFromTime] = useState(() => toLocalTimeInput(range.from));
  const [liveEnd, setLiveEnd] = useState(() => range.to === null);
  const [toDate, setToDate] = useState(() => toLocalDateInput(range.to ?? Date.now()));
  const [toTime, setToTime] = useState(() => (range.to === null ? '23:59' : toLocalTimeInput(range.to)));
  const [pickStage, setPickStage] = useState<'start' | 'end'>('start');
  const [validationError, setValidationError] = useState('');

  const handleOpenChange = (nextOpen: boolean) => {
    if (nextOpen) {
      const now = Date.now();
      setOpenedAt(now);
      const endRef = range.to ?? now;
      const d = new Date(endRef);
      setCursorMonth({ year: d.getFullYear(), month: d.getMonth() });
      setFromDate(toLocalDateInput(range.from));
      setFromTime(toLocalTimeInput(range.from));
      setLiveEnd(range.to === null);
      setToDate(toLocalDateInput(endRef));
      setToTime(range.to === null ? '23:59' : toLocalTimeInput(range.to));
      setPickStage('start');
      setValidationError('');
    }
    setOpen(nextOpen);
  };

  const draftFromTs = useMemo(() => combineDateAndTime(fromDate, fromTime, false), [fromDate, fromTime]);
  const draftToTs = useMemo(
    () => (liveEnd ? null : combineDateAndTime(toDate, toTime, true)),
    [liveEnd, toDate, toTime],
  );

  const prevMonth = useMemo(() => {
    const d = new Date(cursorMonth.year, cursorMonth.month - 1, 1);
    return { year: d.getFullYear(), month: d.getMonth() };
  }, [cursorMonth]);

  const selectCalendarDay = (dayTs: number) => {
    const dayStart = startOfDay(dayTs);
    const dayStr = toLocalDateInput(dayStart);
    setValidationError('');
    if (pickStage === 'start') {
      setFromDate(dayStr);
      setFromTime('00:00');
      const currentEnd = draftToTs ?? endOfDay(openedAt);
      if (dayStart > currentEnd) {
        setToDate(dayStr);
        setToTime('23:59');
      }
      setPickStage('end');
    } else {
      const currentStart = draftFromTs !== null ? startOfDay(draftFromTs) : dayStart;
      if (dayStart < currentStart) {
        setFromDate(dayStr);
        setFromTime('00:00');
        setToDate(toLocalDateInput(currentStart));
        setToTime('23:59');
      } else {
        setToDate(dayStr);
        setToTime('23:59');
      }
      setLiveEnd(false);
      setPickStage('start');
    }
  };

  const applyRange = () => {
    const now = Date.now();
    if (draftFromTs === null) {
      setValidationError('Enter a valid start date and time.');
      return;
    }
    if (draftFromTs > now) {
      setValidationError('Start date cannot be in the future.');
      return;
    }
    if (!liveEnd) {
      if (draftToTs === null) {
        setValidationError('Enter a valid end date and time.');
        return;
      }
      if (draftToTs <= draftFromTs) {
        setValidationError('End date must be after the start date.');
        return;
      }
      onChange(makeTimeRange(draftFromTs, draftToTs, now));
    } else {
      onChange(makeTimeRange(draftFromTs, null, now));
    }
    setOpen(false);
  };

  const renderMonth = (ym: { year: number; month: number }) => {
    const label = new Date(ym.year, ym.month, 1).toLocaleDateString(undefined, {
      month: 'long',
      year: 'numeric',
    });
    const cells = monthMatrix(ym.year, ym.month);
    const startDay = draftFromTs !== null ? startOfDay(draftFromTs) : null;
    const endDay = liveEnd ? startOfDay(openedAt) : draftToTs !== null ? startOfDay(draftToTs) : null;
    const todayDay = startOfDay(openedAt);

    return (
      <div key={`${ym.year}-${ym.month}`} className="min-w-0 flex-1">
        <div className="mb-2 text-center text-[11px] font-medium text-edge-100">{label}</div>
        <div className="grid grid-cols-7 gap-0.5 text-center text-[9px] uppercase tracking-wider text-edge-400">
          {WEEKDAYS.map((w) => (
            <div key={w} className="py-1">{w}</div>
          ))}
        </div>
        <div className="grid grid-cols-7 gap-0.5 text-[11px]">
          {cells.map((cell) => {
            const cellDay = startOfDay(cell.ts);
            const isStart = startDay !== null && cellDay === startDay;
            const isEnd = endDay !== null && cellDay === endDay;
            const inSpan = startDay !== null && endDay !== null && cellDay > startDay && cellDay < endDay;
            const isFuture = cellDay > todayDay;
            return (
              <button
                key={cell.ts}
                type="button"
                disabled={isFuture}
                onClick={() => selectCalendarDay(cell.ts)}
                className={[
                  'h-7 w-full text-center tabular-nums transition-colors',
                  !cell.inMonth ? 'text-edge-600' : 'text-edge-200',
                  isFuture ? 'cursor-not-allowed opacity-35' : 'hover:bg-edge-800 hover:text-edge-100',
                  inSpan ? 'bg-primary/10 text-edge-100' : '',
                  isStart || isEnd ? 'border border-primary bg-primary/25 font-medium text-primary' : '',
                ].filter(Boolean).join(' ')}
              >
                {cell.day}
              </button>
            );
          })}
        </div>
      </div>
    );
  };

  return (
    <Popover open={open} onOpenChange={handleOpenChange}>
      <PopoverTrigger
        render={
          <Button
            variant="outline"
            size="sm"
            aria-label={`Date range: ${range.label}`}
            className="gap-2.5 border-edge-600 bg-edge-950 px-3 text-[11px] text-edge-100 hover:border-primary/50"
          />
        }
      >
        <CalendarRange className="size-3.5 text-primary" aria-hidden="true" />
        <span className="tabular-nums">{range.label}</span>
        {range.to === null && (
          <span className="inline-flex items-center gap-1 border border-primary/30 bg-primary/10 px-1.5 py-0.5 text-[9px] uppercase tracking-wider text-primary">
            <Radio className="size-2.5" aria-hidden="true" />Live
          </span>
        )}
      </PopoverTrigger>
      <PopoverContent align="end" className="w-[min(560px,calc(100vw-1.5rem))] p-4">
        <div className="space-y-4">
          <div className="flex items-center justify-between border-b border-edge-700 pb-3">
            <div>
              <div className="text-[10px] uppercase tracking-[.16em] text-primary">Custom date range</div>
              <p className="mt-0.5 text-[10px] text-edge-400">
                {pickStage === 'start' ? 'Select start date on calendar or enter exact timestamps below.' : 'Select end date on calendar.'}
              </p>
            </div>
            <div className="flex items-center gap-1">
              <Button
                type="button"
                variant="outline"
                size="icon-sm"
                aria-label="Previous month"
                onClick={() =>
                  setCursorMonth((m) => {
                    const d = new Date(m.year, m.month - 1, 1);
                    return { year: d.getFullYear(), month: d.getMonth() };
                  })
                }
              >
                <ChevronLeft className="size-3.5" />
              </Button>
              <Button
                type="button"
                variant="outline"
                size="icon-sm"
                aria-label="Next month"
                onClick={() =>
                  setCursorMonth((m) => {
                    const d = new Date(m.year, m.month + 1, 1);
                    return { year: d.getFullYear(), month: d.getMonth() };
                  })
                }
              >
                <ChevronRight className="size-3.5" />
              </Button>
            </div>
          </div>

          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            {renderMonth(prevMonth)}
            {renderMonth(cursorMonth)}
          </div>

          <div className="grid grid-cols-1 gap-3 border-t border-edge-700 pt-3 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="drp-from-date" className="text-[10px] uppercase tracking-[.12em] text-edge-400">Start (local)</Label>
              <div className="grid grid-cols-[1fr_96px] gap-1.5">
                <Input
                  id="drp-from-date"
                  type="date"
                  value={fromDate}
                  onChange={(e) => { setFromDate(e.target.value); setValidationError(''); }}
                />
                <Input
                  id="drp-from-time"
                  aria-label="Start time"
                  type="time"
                  value={fromTime}
                  onChange={(e) => { setFromTime(e.target.value); setValidationError(''); }}
                />
              </div>
            </div>

            <div className="space-y-1.5">
              <div className="flex items-center justify-between">
                <Label htmlFor="drp-to-date" className="text-[10px] uppercase tracking-[.12em] text-edge-400">End (local)</Label>
                <label className="inline-flex cursor-pointer items-center gap-1.5 text-[10px] text-edge-300">
                  <Switch checked={liveEnd} onCheckedChange={(checked) => { setLiveEnd(checked); setValidationError(''); }} />
                  <span>Through now (live)</span>
                </label>
              </div>
              <div className="grid grid-cols-[1fr_96px] gap-1.5">
                <Input
                  id="drp-to-date"
                  type="date"
                  disabled={liveEnd}
                  value={toDate}
                  onChange={(e) => { setToDate(e.target.value); setValidationError(''); }}
                />
                <Input
                  id="drp-to-time"
                  aria-label="End time"
                  type="time"
                  disabled={liveEnd}
                  value={toTime}
                  onChange={(e) => { setToTime(e.target.value); setValidationError(''); }}
                />
              </div>
            </div>
          </div>

          {validationError && (
            <p role="alert" className="border border-destructive/40 bg-destructive/10 px-3 py-2 text-[11px] text-destructive">
              {validationError}
            </p>
          )}

          <div className="flex items-center justify-between gap-2 border-t border-edge-700 pt-3">
            <span className="text-[10px] text-edge-400">
              {draftFromTs !== null
                ? makeTimeRange(draftFromTs, liveEnd ? null : draftToTs, openedAt).label
                : 'Select start and end dates'}
            </span>
            <div className="flex items-center gap-2">
              <Button type="button" variant="ghost" size="sm" onClick={() => setOpen(false)}>
                Cancel
              </Button>
              <Button type="button" size="sm" onClick={applyRange}>
                Apply range
              </Button>
            </div>
          </div>
        </div>
      </PopoverContent>
    </Popover>
  );
}
