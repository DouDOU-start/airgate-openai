import type { CSSProperties, ReactNode } from 'react';
import type { UsageRecordSurfaceProps } from '@devilgenius/airgate-theme/plugin';

interface UsageRecordLike {
  model?: string;
  input_tokens?: number;
  output_tokens?: number;
  cached_input_tokens?: number;
  reasoning_output_tokens?: number;
  reasoning_effort?: string;
  service_tier?: string;
  usage_metadata?: Record<string, string>;
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

const inlineValueStyle: CSSProperties = {
  display: 'inline-flex',
  minWidth: 0,
  maxWidth: '100%',
  alignItems: 'baseline',
  justifyContent: 'flex-end',
  gap: '0.25rem',
};

const inlineValueMetaStyle: CSSProperties = {
  minWidth: 0,
  overflow: 'hidden',
  color: 'var(--ag-text-tertiary)',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

const inlineValueNumberStyle: CSSProperties = {
  flexShrink: 0,
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

function recordFromContext(context: UsageRecordSurfaceProps['context']): UsageRecordLike {
  const record = context?.record;
  return record && typeof record === 'object' ? record as UsageRecordLike : {};
}

function metadataFromContext(context: UsageRecordSurfaceProps['context'], record: UsageRecordLike): Record<string, string> {
  const fromContext = context?.usage_metadata;
  if (fromContext && typeof fromContext === 'object' && !Array.isArray(fromContext)) {
    return fromContext as Record<string, string>;
  }
  return record.usage_metadata ?? {};
}

function norm(value?: string) {
  return (value || '').trim().toLowerCase().replace(/[\s-]+/g, '_');
}

function formatNumber(value: number) {
  return Number.isInteger(value)
    ? value.toLocaleString()
    : value.toLocaleString(undefined, { maximumFractionDigits: 4 });
}

function metadataText(metadata: Record<string, string>, keys: string[]) {
  for (const [key, value] of Object.entries(metadata)) {
    if (!keys.includes(norm(key))) continue;
    const text = value.trim();
    if (text) return text;
  }
  return '';
}

function metadataNumber(metadata: Record<string, string>, keys: string[]) {
  const value = metadataText(metadata, keys);
  if (!value) return 0;
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : 0;
}

function Row({ label, tone, value }: { label: ReactNode; tone?: string; value: ReactNode }) {
  return (
    <div style={rowStyle}>
      <span style={labelStyle}>{label}</span>
      <span style={{ ...valueStyle, color: tone }}>{value}</span>
    </div>
  );
}

function outputTokenValue(reasoningTokens: number, outputTokens: number) {
  return (
    <span style={inlineValueStyle}>
      {reasoningTokens > 0 ? (
        <span style={inlineValueMetaStyle}>(推理 {formatNumber(reasoningTokens)})</span>
      ) : null}
      <span style={inlineValueNumberStyle}>{formatNumber(outputTokens)}</span>
    </span>
  );
}

function inputTokenValue(textInputTokens: number, imageInputTokens: number, inputTokens: number) {
  if (imageInputTokens <= 0) return formatNumber(inputTokens);
  return (
    <span style={inlineValueStyle}>
      <span style={inlineValueMetaStyle}>(文本 {formatNumber(textInputTokens)})</span>
      <span style={inlineValueNumberStyle}>{formatNumber(inputTokens)}</span>
    </span>
  );
}

export function UsageMetricDetail({ context }: UsageRecordSurfaceProps) {
  const record = recordFromContext(context);
  const metadata = metadataFromContext(context, record);

  const imageSize = metadataText(metadata, ['openai.image.size']);
  const serviceTier = record.service_tier || '';
  const reasoningEffort = record.reasoning_effort || '';
  const inputTokens = record.input_tokens || 0;
  const outputTokens = record.output_tokens || 0;
  const cachedInputTokens = record.cached_input_tokens || 0;
  const reasoningTokens = record.reasoning_output_tokens || 0;
  const imageInputTokens = metadataNumber(metadata, ['openai.image.input_image_tokens']);
  const rawTextInputTokens = metadataNumber(metadata, ['openai.image.input_text_tokens']);
  const textInputTokens = rawTextInputTokens || (imageInputTokens > 0 && inputTokens >= imageInputTokens ? inputTokens - imageInputTokens : 0);
  const images = metadataNumber(metadata, ['openai.image.count']);
  const totalTokens = inputTokens + outputTokens + cachedInputTokens;

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
        {images > 0 ? <Row label="图片数量" value={formatNumber(images)} tone="var(--ag-success)" /> : null}
        <Row label="输入 Token" value={inputTokenValue(textInputTokens, imageInputTokens, inputTokens)} tone="var(--ag-info)" />
        <Row label="输出 Token" value={outputTokenValue(reasoningTokens, outputTokens)} tone="var(--ag-primary)" />
        {cachedInputTokens > 0 ? <Row label="缓存读取 Token" value={formatNumber(cachedInputTokens)} tone="var(--ag-success)" /> : null}
        <Row label="总 Token" value={formatNumber(totalTokens)} tone="var(--ag-text)" />
      </div>
    </div>
  );
}
