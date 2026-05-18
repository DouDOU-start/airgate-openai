import { useEffect, useState, type CSSProperties } from 'react';
import type { AccountSurfaceProps } from '@doudou-start/airgate-theme/plugin';

interface UsageWindowItem {
  key?: string;
  label: string;
  used_percent: number;
  reset_seconds?: number;
  reset_after_seconds?: number;
  reset_at?: string;
}

type WindowSlot = '5h' | '7d' | 'other';

interface WindowGroup {
  id: string;
  five?: UsageWindowItem;
  seven?: UsageWindowItem;
  others: UsageWindowItem[];
}

function isUsageWindowItem(item: unknown): item is UsageWindowItem {
  if (item === null || typeof item !== 'object') return false;
  const record = item as Record<string, unknown>;
  return typeof record.label === 'string' && typeof record.used_percent === 'number';
}

function getUsageWindows(context: AccountSurfaceProps['context']): UsageWindowItem[] {
  const windows = context?.windows;
  if (!Array.isArray(windows)) return [];
  return windows.filter(isUsageWindowItem);
}

function useResetTick(enabled: boolean) {
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    if (!enabled) return undefined;
    const timer = window.setInterval(() => setNow(Date.now()), 30_000);
    return () => window.clearInterval(timer);
  }, [enabled]);

  return now;
}

function resolveResetSeconds(w: UsageWindowItem, now: number) {
  if (w.reset_at) {
    const delta = Date.parse(w.reset_at) - now;
    if (Number.isFinite(delta)) return Math.max(0, Math.floor(delta / 1000));
  }
  if (typeof w.reset_seconds === 'number') return w.reset_seconds;
  if (typeof w.reset_after_seconds === 'number') return w.reset_after_seconds;
  return 0;
}

function formatReset(seconds: number) {
  if (!seconds || seconds <= 0) return '-';
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  if (d > 0) return h > 0 ? `${d}d${h}h` : `${d}d`;
  if (h > 0) return m > 0 ? `${h}h${m}m` : `${h}h`;
  return `${m}m`;
}

function usageColor(pct: number) {
  if (pct < 50) return 'var(--ag-success)';
  if (pct < 80) return 'var(--ag-warning)';
  return 'var(--ag-danger)';
}

function getWindowSlot(w: UsageWindowItem, index: number): { group: string; slot: WindowSlot } {
  const key = w.key?.trim() ?? '';
  const label = w.label.trim();
  const modelKey = /^model:(5h|7d):(.+)$/i.exec(key);
  if (modelKey?.[1] && modelKey[2]) {
    return { group: `model:${modelKey[2]}`, slot: modelKey[1].toLowerCase() as '5h' | '7d' };
  }
  if (key === '5h' || key === '7d') {
    return { group: 'base', slot: key };
  }

  const labelKey = /^(5h|7d)(?:\s+(.+))?$/i.exec(label);
  if (labelKey?.[1]) {
    const slot = labelKey[1].toLowerCase() as '5h' | '7d';
    return { group: labelKey[2] ? `model:${labelKey[2]}` : 'base', slot };
  }

  return { group: key || label || `window:${index}`, slot: 'other' };
}

function buildGroups(items: UsageWindowItem[]) {
  const groups: WindowGroup[] = [];
  const groupMap = new Map<string, WindowGroup>();

  items.forEach((item, index) => {
    const { group, slot } = getWindowSlot(item, index);
    let bucket = groupMap.get(group);
    if (!bucket) {
      bucket = { id: group, others: [] };
      groupMap.set(group, bucket);
      groups.push(bucket);
    }
    if (slot === '5h') bucket.five = item;
    else if (slot === '7d') bucket.seven = item;
    else bucket.others.push(item);
  });

  return groups;
}

function shortLabel(label: string) {
  const parts = label.trim().split(/\s+/);
  const timePart = parts[0] ?? label;
  if (parts.length <= 1) return timePart;
  const modelPart = parts.slice(1).join(' ');
  const segments = modelPart.split('-').filter(Boolean);
  return `${timePart} ${segments[segments.length - 1] ?? modelPart}`;
}

const rootStyle: CSSProperties = {
  display: 'flex',
  minWidth: 0,
  flexDirection: 'column',
  justifyContent: 'center',
  gap: '0.25rem',
  fontFamily: 'var(--ag-font-mono)',
};

const rowStyle: CSSProperties = {
  display: 'grid',
  height: '1.25rem',
  minWidth: 0,
  gridTemplateColumns: '3.25rem minmax(3.25rem, 1fr) 1.375rem 2.5rem',
  alignItems: 'center',
  gap: '0.125rem',
};

const pairedGroupStyle: CSSProperties = {
  display: 'flex',
  minHeight: '2.75rem',
  minWidth: 0,
  flexDirection: 'column',
  justifyContent: 'center',
  gap: '0.25rem',
};

const badgeStyle: CSSProperties = {
  display: 'inline-flex',
  minWidth: 0,
  alignItems: 'center',
  justifyContent: 'flex-start',
  overflow: 'hidden',
  borderRadius: '0.25rem',
  border: '1px solid var(--ag-glass-border)',
  background: 'var(--ag-bg-surface)',
  padding: '0 0.25rem',
  color: 'var(--ag-text-secondary)',
  fontSize: '0.6875rem',
  fontWeight: 600,
  lineHeight: 1,
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

const barStyle: CSSProperties = {
  height: '0.375rem',
  minWidth: 0,
  overflow: 'hidden',
  borderRadius: '999px',
  background: 'var(--ag-glass-border)',
};

const valueStyle: CSSProperties = {
  width: '100%',
  minWidth: 0,
  overflow: 'hidden',
  textAlign: 'right',
  fontSize: '0.625rem',
  fontWeight: 600,
  lineHeight: 1,
  fontVariantNumeric: 'tabular-nums',
  whiteSpace: 'nowrap',
};

const resetStyle: CSSProperties = {
  ...valueStyle,
  display: 'inline-flex',
  height: '100%',
  alignItems: 'center',
  justifyContent: 'flex-end',
  color: 'var(--ag-text-secondary)',
};

function renderWindowRow(w: UsageWindowItem, index: number, now: number) {
  const percent = Math.round(w.used_percent);
  const barPercent = Math.max(0, Math.min(100, percent));
  const color = usageColor(w.used_percent);
  const resetText = formatReset(resolveResetSeconds(w, now));

  return (
    <div key={w.key || `${w.label}:${index}`} style={rowStyle}>
      <span style={badgeStyle} title={w.label}>
        {shortLabel(w.label)}
      </span>
      <div style={barStyle}>
        <div
          style={{
            width: `${barPercent}%`,
            height: '100%',
            borderRadius: '999px',
            background: color,
          }}
        />
      </div>
      <span style={{ ...valueStyle, color }}>{percent}%</span>
      <span style={resetStyle} title={resetText}>{resetText}</span>
    </div>
  );
}

export function UsageWindow({ context }: AccountSurfaceProps) {
  const groups = buildGroups(getUsageWindows(context));
  const resetNow = useResetTick(groups.length > 0);
  if (groups.length === 0) return null;

  return (
    <div style={rootStyle}>
      {groups.map((group, groupIndex) => {
        const pairedRows = [group.five, group.seven].filter(Boolean) as UsageWindowItem[];
        return (
          <div key={group.id || `group:${groupIndex}`} style={{ display: 'flex', minWidth: 0, flexDirection: 'column', gap: '0.25rem' }}>
            {pairedRows.length > 0 ? (
              <div style={pairedGroupStyle}>
                {pairedRows.map((w, rowIndex) => renderWindowRow(w, rowIndex, resetNow))}
              </div>
            ) : null}
            {group.others.map((w, rowIndex) => renderWindowRow(w, rowIndex, resetNow))}
          </div>
        );
      })}
    </div>
  );
}
