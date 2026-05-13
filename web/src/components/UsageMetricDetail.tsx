import type { CSSProperties, ReactNode } from 'react';
import type { UsageRecordSurfaceProps } from '@doudou-start/airgate-theme/plugin';

interface UsageAttribute {
  key?: string;
  label?: string;
  kind?: string;
  value?: string;
  metadata?: Record<string, string>;
}

interface UsageMetric {
  key?: string;
  label?: string;
  kind?: string;
  unit?: string;
  value?: number;
  account_cost?: number;
  currency?: string;
  metadata?: Record<string, string>;
}

interface UsageRecordLike {
  model?: string;
  input_tokens?: number;
  output_tokens?: number;
  cached_input_tokens?: number;
  reasoning_output_tokens?: number;
  reasoning_effort?: string;
  image_size?: string;
  service_tier?: string;
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

const chipWrapStyle: CSSProperties = {
  display: 'flex',
  flexWrap: 'wrap',
  gap: '0.25rem',
  padding: '0.5rem 0.5rem 0.125rem',
};

const chipStyle: CSSProperties = {
  display: 'inline-flex',
  maxWidth: '100%',
  alignItems: 'center',
  border: '1px solid color-mix(in srgb, var(--ag-primary) 28%, transparent)',
  borderRadius: '0.25rem',
  background: 'color-mix(in srgb, var(--ag-primary) 12%, transparent)',
  padding: '0.125rem 0.375rem',
  color: 'var(--ag-primary)',
  fontFamily: 'var(--ag-font-mono)',
  fontSize: '0.6875rem',
  fontWeight: 600,
  lineHeight: 1,
};

function contextArray<T>(context: UsageRecordSurfaceProps['context'], camel: string, snake: string): T[] {
  const value = context?.[camel] ?? context?.[snake];
  return Array.isArray(value) ? value as T[] : [];
}

function recordFromContext(context: UsageRecordSurfaceProps['context']): UsageRecordLike {
  const record = context?.record;
  return record && typeof record === 'object' ? record as UsageRecordLike : {};
}

function norm(value?: string) {
  return (value || '').trim().toLowerCase().replace(/[\s-]+/g, '_');
}

function numberValue(value: unknown) {
  return typeof value === 'number' && Number.isFinite(value) ? value : 0;
}

function formatNumber(value: number) {
  return Number.isInteger(value)
    ? value.toLocaleString()
    : value.toLocaleString(undefined, { maximumFractionDigits: 4 });
}

function metricValue(metrics: UsageMetric[], keys: string[]) {
  const metric = metrics.find((item) => keys.includes(norm(item.key || item.kind || item.label)));
  return metric ? numberValue(metric.value) : 0;
}

function Row({ label, tone, value }: { label: ReactNode; tone?: string; value: ReactNode }) {
  return (
    <div style={rowStyle}>
      <span style={labelStyle}>{label}</span>
      <span style={{ ...valueStyle, color: tone }}>{value}</span>
    </div>
  );
}

export function UsageMetricDetail({ context }: UsageRecordSurfaceProps) {
  const record = recordFromContext(context);
  const attributes = contextArray<UsageAttribute>(context, 'usageAttributes', 'usage_attributes');
  const metrics = contextArray<UsageMetric>(context, 'usageMetrics', 'usage_metrics');
  const attrValue = (keys: string[]) => attributes.find((item) => keys.includes(norm(item.key || item.kind || item.label)))?.value || '';

  const imageSize = attrValue(['image_size', 'resolution', 'size']) || record.image_size || '';
  const serviceTier = attrValue(['service_tier', 'tier']) || record.service_tier || '';
  const reasoningEffort = attrValue(['reasoning_effort', 'reasoning']) || record.reasoning_effort || '';
  const inputTokens = metricValue(metrics, ['input_tokens', 'input_token', 'prompt_tokens', 'prompt_token']) || record.input_tokens || 0;
  const outputTokens = metricValue(metrics, ['output_tokens', 'output_token', 'completion_tokens', 'completion_token']) || record.output_tokens || 0;
  const cachedInputTokens = metricValue(metrics, ['cached_input_tokens', 'cached_input_token', 'cache_read_tokens', 'cache_read_token']) || record.cached_input_tokens || 0;
  const reasoningTokens = metricValue(metrics, ['reasoning_output_tokens', 'reasoning_tokens', 'reasoning_token']) || record.reasoning_output_tokens || 0;
  const images = metricValue(metrics, ['images', 'image', 'image_generation']);
  const totalTokens = metricValue(metrics, ['total_tokens', 'total_token']) || inputTokens + outputTokens + cachedInputTokens;

  return (
    <div style={panelStyle}>
      <div style={headerStyle}>
        <div style={titleStyle}>OpenAI 计量明细</div>
        {record.model ? <div style={subtitleStyle}>{record.model}</div> : null}
      </div>
      {(imageSize || serviceTier || reasoningEffort) ? (
        <div style={chipWrapStyle}>
          {imageSize ? <span style={chipStyle}>{imageSize}</span> : null}
          {serviceTier ? <span style={chipStyle}>{serviceTier}</span> : null}
          {reasoningEffort ? <span style={chipStyle}>推理 {reasoningEffort}</span> : null}
        </div>
      ) : null}
      <div style={bodyStyle}>
        <Row label="输入 Token" value={formatNumber(inputTokens)} tone="var(--ag-info)" />
        <Row label="输出 Token" value={formatNumber(outputTokens)} tone="var(--ag-primary)" />
        {cachedInputTokens > 0 ? <Row label="缓存读取 Token" value={formatNumber(cachedInputTokens)} tone="var(--ag-success)" /> : null}
        {reasoningTokens > 0 ? <Row label="推理 Token" value={formatNumber(reasoningTokens)} tone="var(--ag-warning)" /> : null}
        {images > 0 ? <Row label="图片数量" value={formatNumber(images)} tone="var(--ag-success)" /> : null}
        <Row label="总 Token" value={formatNumber(totalTokens)} tone="var(--ag-text)" />
      </div>
    </div>
  );
}
