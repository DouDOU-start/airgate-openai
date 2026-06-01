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
  image_size?: string;
  endpoint?: string;
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

function normalizedKey(item: UsageCostDetailItem): string {
  return `${item.key || ''} ${item.label || ''}`.toLowerCase().replace(/[\s-]+/g, '_');
}

function isImageCostDetail(item: UsageCostDetailItem): boolean {
  const key = normalizedKey(item);
  if (key.includes('input') || key.includes('输入')) return false;
  return key.includes('image') || key.includes('图片');
}

function isImageInputDetail(item: UsageCostDetailItem): boolean {
  const key = normalizedKey(item);
  return (key.includes('image') || key.includes('图片')) && (key.includes('input') || key.includes('输入'));
}

function isImageRelatedDetail(item: UsageCostDetailItem): boolean {
  const key = normalizedKey(item);
  return key.includes('image') || key.includes('图片');
}

function isInputDetail(item: UsageCostDetailItem): boolean {
  const key = normalizedKey(item);
  return !isImageRelatedDetail(item)
    && !key.includes('cached')
    && !key.includes('cache')
    && !key.includes('缓存')
    && (key.includes('input') || key.includes('输入'));
}

function isOutputDetail(item: UsageCostDetailItem): boolean {
  const key = normalizedKey(item);
  return !isImageRelatedDetail(item) && (key.includes('output') || key.includes('输出'));
}

function isImageOnlyModel(model: string | undefined): boolean {
  const normalized = (model || '').trim().toLowerCase();
  return normalized.startsWith('gpt-image')
    || normalized.startsWith('dall-e')
    || normalized.startsWith('dalle');
}

function hasFixedImagePricing(rows: UsageCostDetailItem[]): boolean {
  return rows.some((item) => item.metadata?.billing_mode === 'fixed_image_price');
}

function hasImageContext(record: UsageRecordLike, rows: UsageCostDetailItem[]): boolean {
  const endpoint = (record.endpoint || '').toLowerCase();
  return !!record.image_size
    || endpoint.includes('/images/')
    || rows.some((item) => isImageRelatedDetail(item));
}

function visibleUserCost(item: UsageCostDetailItem): number {
  return item.user_cost ?? item.account_cost ?? 0;
}

function mergeCostDetail(base: UsageCostDetailItem, extra: UsageCostDetailItem): UsageCostDetailItem {
  const accountCost = (base.account_cost ?? 0) + (extra.account_cost ?? 0);
  const userCost = (base.user_cost ?? base.account_cost ?? 0) + (extra.user_cost ?? extra.account_cost ?? 0);
  return {
    ...base,
    account_cost: accountCost,
    user_cost: userCost,
    billing_multiplier: accountCost > 0 ? userCost / accountCost : base.billing_multiplier,
  };
}

function normalizeTokenDetail(item: UsageCostDetailItem, key: string, label: string): UsageCostDetailItem {
  const metadata: Record<string, string> = {};
  for (const metadataKey of ['unit', 'unit_price', 'billing_model']) {
    const value = item.metadata?.[metadataKey];
    if (value) metadata[metadataKey] = value;
  }
  return {
    ...item,
    key,
    label,
    metadata: Object.keys(metadata).length > 0 ? metadata : undefined,
  };
}

function mergeTokenBillingRows(rows: UsageCostDetailItem[]): UsageCostDetailItem[] {
  const merged: UsageCostDetailItem[] = [];
  let inputIndex = -1;
  let outputIndex = -1;

  for (const item of rows) {
    if (isImageInputDetail(item)) {
      const detail = normalizeTokenDetail(item, 'input_tokens', '输入 Token');
      if (inputIndex >= 0) {
        merged[inputIndex] = mergeCostDetail(merged[inputIndex], detail);
      } else {
        inputIndex = merged.length;
        merged.push(detail);
      }
      continue;
    }
    if (isImageCostDetail(item)) {
      const detail = normalizeTokenDetail(item, 'output_tokens', '输出 Token');
      if (outputIndex >= 0) {
        merged[outputIndex] = mergeCostDetail(merged[outputIndex], detail);
      } else {
        outputIndex = merged.length;
        merged.push(detail);
      }
      continue;
    }
    if (isInputDetail(item)) {
      if (inputIndex >= 0) {
        merged[inputIndex] = mergeCostDetail(merged[inputIndex], item);
      } else {
        inputIndex = merged.length;
        merged.push(item);
      }
      continue;
    }
    if (isOutputDetail(item)) {
      if (outputIndex >= 0) {
        merged[outputIndex] = mergeCostDetail(merged[outputIndex], item);
      } else {
        outputIndex = merged.length;
        merged.push(item);
      }
      continue;
    }
    merged.push(item);
  }

  return merged;
}

function shouldShowCostRow(item: UsageCostDetailItem, fixedImagePricing: boolean, responseLikeRequest: boolean): boolean {
  if (!fixedImagePricing) {
    return visibleUserCost(item) > 0;
  }
  if (isImageCostDetail(item)) return true;
  if (isImageRelatedDetail(item)) return false;
  if (!responseLikeRequest) return false;
  return visibleUserCost(item) > 0;
}

function displayCostLabel(item: UsageCostDetailItem, fixedImagePricing: boolean): string {
  if (fixedImagePricing && isImageCostDetail(item)) {
    return '图片生成成本';
  }
  const label = toCostLabel(item.label || item.key || '费用');
  return label;
}

export function UsageCostDetail({ context }: UsageRecordSurfaceProps) {
  const record = recordFromContext(context);
  const isAdmin = context?.adminView !== false;

  const details = contextArray<UsageCostDetailItem>(context, 'usageCostDetails', 'usage_cost_details');
  const rows = details.length > 0 ? details : fallbackDetails(record);
  const fixedImagePricing = hasFixedImagePricing(rows);
  const imageContext = fixedImagePricing || hasImageContext(record, rows);
  const responseLikeRequest = !isImageOnlyModel(record.model);
  const imageOnlyFixedPricing = fixedImagePricing && !responseLikeRequest;
  const displayRows = imageContext && !fixedImagePricing ? mergeTokenBillingRows(rows) : rows;
  const visibleRows = displayRows.filter((item) => shouldShowCostRow(item, fixedImagePricing, responseLikeRequest));
  const unitPrices: { label: string; value: string }[] = [];
  if (!imageOnlyFixedPricing) {
    for (const item of visibleRows) {
      if (fixedImagePricing && isImageCostDetail(item)) continue;
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
  }

  const showRateInfo = !imageOnlyFixedPricing;
  const showAccountInfo = isAdmin && !fixedImagePricing;
  const hasRateInfo = showRateInfo && (!!record.service_tier
    || (record.rate_multiplier !== undefined && record.rate_multiplier > 0)
    || (showAccountInfo && record.account_rate_multiplier !== undefined && record.account_rate_multiplier > 0 && record.account_rate_multiplier !== 1));
  const conversationBaseCost = Math.max(
    0,
    visibleRows
      .filter((item) => !isImageRelatedDetail(item))
      .reduce((sum, item) => sum + (item.account_cost ?? 0), 0),
  );

  return (
    <div style={panelStyle}>
      <div style={headerStyle}>
        <div style={titleStyle}>OpenAI 费用明细</div>
        {record.model ? <div style={subtitleStyle}>{record.model}</div> : null}
      </div>
      <div style={bodyStyle}>
        {visibleRows.map((item, index) => (
          <Row
            key={item.key || `${item.label}:${index}`}
            label={displayCostLabel(item, fixedImagePricing)}
            value={money(item.user_cost ?? item.account_cost)}
          />
        ))}
        {unitPrices.map((up, i) => (
          <Row key={`up-${i}`} label={up.label} value={up.value} />
        ))}
        <div style={dividerStyle} />
        {showRateInfo && record.service_tier ? (
          <Row label="服务档位" value={<span style={{ textTransform: 'capitalize' }}>{record.service_tier}</span>} />
        ) : null}
        {showRateInfo && record.rate_multiplier !== undefined && record.rate_multiplier > 0 ? (
          <Row label="倍率" value={`${record.rate_multiplier.toFixed(2)}x`} />
        ) : null}
        {showRateInfo && showAccountInfo && record.account_rate_multiplier !== undefined && record.account_rate_multiplier > 0 ? (
          <Row label="账号倍率" value={`${record.account_rate_multiplier.toFixed(2)}x`} />
        ) : null}
        {showRateInfo && isAdmin && record.sell_rate && record.sell_rate > 0 ? (
          <Row label="销售倍率" value={`${record.sell_rate.toFixed(2)}x`} />
        ) : null}
        {hasRateInfo ? <div style={dividerStyle} /> : null}
        {!fixedImagePricing ? (
          <Row label={imageContext ? '倍率前成本' : '原始'} value={money(record.total_cost)} tone="var(--ag-text)" />
        ) : null}
        {fixedImagePricing && responseLikeRequest ? (
          <Row label="对话原始" value={money(conversationBaseCost)} tone="var(--ag-text)" />
        ) : null}
        {showAccountInfo && record.account_cost !== undefined ? (
          <Row label="账号计费" value={money(record.account_cost)} tone="var(--ag-success)" />
        ) : null}
        <Row label="本次消费" value={money(record.actual_cost)} tone="var(--ag-warning)" />
        {isAdmin && !imageOnlyFixedPricing && record.sell_rate && record.sell_rate > 0 && record.billed_cost !== record.actual_cost ? (
          <>
            <Row label="客户账面" value={money(record.billed_cost)} tone="var(--ag-primary)" />
            <Row label="利润" value={money((record.billed_cost ?? 0) - (record.actual_cost ?? 0))} tone="var(--ag-success)" />
          </>
        ) : null}
      </div>
    </div>
  );
}
