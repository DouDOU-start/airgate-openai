import type { UsageRecordSurfaceProps } from '@devilgenius/airgate-theme/plugin';
import type { CSSProperties } from 'react';

type UsageContext = {
  reasoning_effort?: string;
  service_tier?: string;
  usage_metadata?: Record<string, string>;
};

const EFFORT_LOW_COLOR = 'rgb(34,197,94)';
const EFFORT_MEDIUM_COLOR = 'rgb(59,130,246)';
const EFFORT_HIGH_COLOR = 'rgb(249,115,22)';
const EFFORT_XHIGH_COLOR = 'rgb(239,68,68)';

const EFFORT_COLORS: Record<string, string> = {
  low: EFFORT_LOW_COLOR,
  medium: EFFORT_MEDIUM_COLOR,
  high: EFFORT_HIGH_COLOR,
  xhigh: EFFORT_XHIGH_COLOR,
};
const IMAGE_SIZE_COLOR = 'rgb(148,163,184)';
const FAST_SERVICE_TIER_COLOR = 'rgb(168, 85, 247)';
const FAST_INDICATOR_STYLE: CSSProperties = {
  position: 'absolute',
  left: '0.375rem',
  top: 0,
  bottom: 0,
  display: 'inline-flex',
  alignItems: 'center',
  justifyContent: 'center',
  width: 'var(--ag-usage-image-dot-size, 0.375rem)',
  color: 'rgb(234, 179, 8)',
  fontSize: '0.75rem',
  height: '100%',
  lineHeight: 1,
  pointerEvents: 'none',
};

function imageSizeDotColor(imageSize: string): string {
  const normalized = imageSize.trim().toLowerCase();
  if (/\b4k\b/.test(normalized)) return EFFORT_HIGH_COLOR;
  if (/\b2k\b/.test(normalized)) return EFFORT_MEDIUM_COLOR;
  if (/\b1k\b/.test(normalized)) return EFFORT_LOW_COLOR;

  const dimensions = normalized.match(/\d+(?:\.\d+)?/g)?.map(Number).filter(Number.isFinite) ?? [];
  const maxDimension = Math.max(0, ...dimensions);
  if (maxDimension > 2048) return EFFORT_HIGH_COLOR;
  if (maxDimension > 1536) return EFFORT_MEDIUM_COLOR;
  return EFFORT_LOW_COLOR;
}

function chipStyle(color: string): CSSProperties {
  return {
    background: `color-mix(in srgb, ${color} 18%, transparent)`,
    boxShadow: `inset 0 0 0 1px color-mix(in srgb, ${color} 34%, transparent)`,
    color,
  };
}

function isUsageServiceTierFast(context: UsageRecordSurfaceProps['context']): boolean {
  const serviceTier = String((context as UsageContext | undefined)?.service_tier ?? '').trim().toLowerCase();
  return serviceTier === 'fast' || serviceTier === 'priority' || serviceTier === 'scale';
}

function usageMetadata(context: UsageRecordSurfaceProps['context']): Record<string, string> {
  const ctx = (context ?? {}) as UsageContext;
  const metadata = ctx.usage_metadata;
  return metadata && typeof metadata === 'object' && !Array.isArray(metadata) ? metadata : {};
}

export function UsageModelMeta(props: UsageRecordSurfaceProps) {
  const ctx = (props.context ?? {}) as UsageContext;
  const imageSize = usageMetadata(props.context)['openai.image.size']?.trim() ?? '';
  const chips: Array<{ label: string; color: string; dotColor?: string; fastMark?: boolean }> = [];

  if (imageSize) {
    chips.push({
      label: imageSize,
      color: IMAGE_SIZE_COLOR,
      dotColor: imageSizeDotColor(imageSize),
    });
  }
  const hasReasoningEffort = Boolean(ctx.reasoning_effort?.trim());
  const showFastMark = !imageSize && isUsageServiceTierFast(ctx);
  if (showFastMark && !hasReasoningEffort) {
    chips.push({ label: 'fast', color: FAST_SERVICE_TIER_COLOR, fastMark: true });
  }
  if (ctx.reasoning_effort) {
    chips.push({
      label: ctx.reasoning_effort,
      color: EFFORT_COLORS[ctx.reasoning_effort] ?? 'rgb(148,163,184)',
      fastMark: showFastMark,
    });
  }

  if (!chips.length) return null;

  return (
    <div className="flex shrink-0 gap-1">
      {chips.map((chip) => (
        <span
          key={chip.label}
          className={`inline-flex shrink-0 items-center rounded px-1.5 font-semibold leading-4 whitespace-nowrap ${chip.dotColor ? 'ag-usage-image-size-chip justify-start gap-1 text-[11px]' : 'text-[12px]'}`}
          style={{
            ...chipStyle(chip.color),
            position: chip.fastMark ? 'relative' : undefined,
          }}
        >
          {chip.dotColor ? (
            <span
              className="ag-usage-image-size-dot"
              aria-hidden="true"
              style={{ backgroundColor: chip.dotColor }}
            />
          ) : null}
          {chip.fastMark ? <span aria-hidden="true" style={FAST_INDICATOR_STYLE}>⚡️</span> : null}
          {chip.label}
        </span>
      ))}
    </div>
  );
}
