import type { CSSProperties, ReactNode } from 'react';
import type { UsageRecordSurfaceProps } from '@doudou-start/airgate-theme/plugin';

interface UsageCostDetailItem {
  key?: string;
  label?: string;
  account_cost?: number;
  user_cost?: number;
  billing_multiplier?: number;
  currency?: string;
  metadata?: Record<string, string>;
}

interface UsageRecordLike {
  model?: string;
  input_cost?: number;
  output_cost?: number;
  cached_input_cost?: number;
  cache_creation_cost?: number;
  total_cost?: number;
  actual_cost?: number;
  billed_cost?: number;
  account_cost?: number;
  rate_multiplier?: number;
  sell_rate?: number;
  account_rate_multiplier?: number;
  service_tier?: string;
  input_price?: number;
  output_price?: number;
}

const panelStyle: CSSProperties = {
  overflow: 'hidden',
  borderRadius: 'var(--radius)',
};

const headerStyle: CSSProperties = {
  borderBottom: '1px solid var(--ag-border)',
  background: 'var(--ag-default-bg)',
  padding: '0.375rem 0.625rem',
};

const titleStyle: CSSProperties = {
  color: 'var(--ag-text)',
  fontSize: '0.875rem',
  fontWeight: 600,
  lineHeight: 1,
};

const subtitleStyle: CSSProperties = {
  marginTop: '0.25rem',
  overflow: 'hidden',
  color: 'var(--ag-text-tertiary)',
  fontSize: '0.75rem',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

const bodyStyle: CSSProperties = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.125rem',
  padding: '0.5rem',
};

const rowStyle: CSSProperties = {
  display: 'grid',
  gridTemplateColumns: 'minmax(0,1fr) minmax(7rem,max-content)',
  alignItems: 'center',
  gap: '0.75rem',
  borderRadius: 'var(--radius)',
  background: 'var(--ag-surface)',
  padding: '0.25rem 0.5rem',
  fontSize: '0.75rem',
};

const labelStyle: CSSProperties = {
  minWidth: 0,
  overflow: 'hidden',
  color: 'var(--ag-text-tertiary)',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

const valueStyle: CSSProperties = {
  minWidth: 0,
  maxWidth: '12rem',
  justifySelf: 'end',
  overflow: 'hidden',
  color: 'var(--ag-text-secondary)',
  fontFamily: 'var(--ag-font-mono)',
  fontWeight: 500,
  textAlign: 'right',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

const dividerStyle: CSSProperties = {
  margin: '0.125rem 0',
  borderTop: '1px solid var(--ag-border)',
};

function contextArray<T>(context: UsageRecordSurfaceProps['context'], camel: string, snake: string): T[] {
  const value = context?.[camel] ?? context?.[snake];
  return Array.isArray(value) ? value as T[] : [];
}

function recordFromContext(context: UsageRecordSurfaceProps['context']): UsageRecordLike {
  const record = context?.record;
  return record && typeof record === 'object' ? record as UsageRecordLike : {};
}

function money(value: unknown) {
  const amount = typeof value === 'number' && Number.isFinite(value) ? value : 0;
  return `$${amount.toFixed(6)}`;
}

function Row({ label, tone, value }: { label: ReactNode; tone?: string; value: ReactNode }) {
  return (
    <div style={rowStyle}>
      <span style={labelStyle}>{label}</span>
      <span style={{ ...valueStyle, color: tone }}>{value}</span>
    </div>
  );
}

function stripTokenSuffix(s: string): string {
  return s.replace(/\s*Token\s*$/i, '').replace(/\s*成本\s*$/, '').trim();
}

function toCostLabel(raw: string): string {
  const s = raw.trim();
  if (s.includes('成本') || s.includes('费用') || s.toLowerCase().includes('cost')) return s;
  return stripTokenSuffix(s) + '成本';
}

function toUnitLabel(raw: string): string {
  const s = raw.trim();
  if (s.includes('单价') || s.toLowerCase().includes('price')) return s;
  return stripTokenSuffix(s) + '单价';
}

function fallbackDetails(record: UsageRecordLike): UsageCostDetailItem[] {
  return [
    { key: 'input_tokens', label: '输入', account_cost: record.input_cost },
    { key: 'cached_input_tokens', label: '缓存输入', account_cost: record.cached_input_cost },
    { key: 'cache_creation_tokens', label: '缓存写入', account_cost: record.cache_creation_cost },
    { key: 'output_tokens', label: '输出', account_cost: record.output_cost },
  ].filter((item) => (item.account_cost ?? 0) > 0);
}

export function UsageCostDetail({ context }: UsageRecordSurfaceProps) {
  const record = recordFromContext(context);
  const details = contextArray<UsageCostDetailItem>(context, 'usageCostDetails', 'usage_cost_details');
  const rows = details.length > 0 ? details : fallbackDetails(record);

  const unitPrices: { label: string; value: string }[] = [];
  for (const item of rows) {
    if (item.metadata?.unit_price && item.metadata?.unit) {
      unitPrices.push({
        label: toUnitLabel(item.label || item.key || ''),
        value: `$${Number(item.metadata.unit_price).toFixed(4)} / ${item.metadata.unit.replace(/^USD\//, '')}`,
      });
    }
  }
  if (unitPrices.length === 0) {
    if (record.input_price && record.input_price > 0)
      unitPrices.push({ label: '输入单价', value: `$${record.input_price.toFixed(4)} / 1M Token` });
    if (record.output_price && record.output_price > 0)
      unitPrices.push({ label: '输出单价', value: `$${record.output_price.toFixed(4)} / 1M Token` });
  }

  const hasRateInfo = !!record.service_tier
    || (record.rate_multiplier !== undefined && record.rate_multiplier > 0)
    || (record.account_rate_multiplier !== undefined && record.account_rate_multiplier > 0 && record.account_rate_multiplier !== 1);

  return (
    <div style={panelStyle}>
      <div style={headerStyle}>
        <div style={titleStyle}>OpenAI 费用明细</div>
        {record.model ? <div style={subtitleStyle}>{record.model}</div> : null}
      </div>
      <div style={bodyStyle}>
        {rows.map((item, index) => (
          <Row
            key={item.key || `${item.label}:${index}`}
            label={toCostLabel(item.label || item.key || '费用')}
            value={money(item.user_cost ?? item.account_cost)}
          />
        ))}
        {unitPrices.map((up, i) => (
          <Row key={`up-${i}`} label={up.label} value={up.value} />
        ))}
        <div style={dividerStyle} />
        {record.service_tier ? (
          <Row label="服务档位" value={<span style={{ textTransform: 'capitalize' }}>{record.service_tier}</span>} />
        ) : null}
        {record.rate_multiplier !== undefined && record.rate_multiplier > 0 ? (
          <Row label="倍率" value={`${record.rate_multiplier.toFixed(2)}x`} />
        ) : null}
        {record.account_rate_multiplier !== undefined && record.account_rate_multiplier > 0 ? (
          <Row label="账号倍率" value={`${record.account_rate_multiplier.toFixed(2)}x`} />
        ) : null}
        {record.sell_rate && record.sell_rate > 0 ? (
          <Row label="销售倍率" value={`${record.sell_rate.toFixed(2)}x`} />
        ) : null}
        {hasRateInfo ? <div style={dividerStyle} /> : null}
        <Row label="原始" value={money(record.total_cost)} tone="var(--ag-text)" />
        {record.account_cost !== undefined ? (
          <Row label="账号计费" value={money(record.account_cost)} tone="var(--ag-success)" />
        ) : null}
        <Row label="用户扣费" value={money(record.actual_cost)} tone="var(--ag-warning)" />
        {record.sell_rate && record.sell_rate > 0 && record.billed_cost !== record.actual_cost ? (
          <>
            <Row label="客户账面" value={money(record.billed_cost)} tone="var(--ag-primary)" />
            <Row label="利润" value={money((record.billed_cost ?? 0) - (record.actual_cost ?? 0))} tone="var(--ag-success)" />
          </>
        ) : null}
      </div>
    </div>
  );
}
