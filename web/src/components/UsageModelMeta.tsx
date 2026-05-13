import type { UsageRecordSurfaceProps } from '@doudou-start/airgate-theme/plugin';
import type { CSSProperties } from 'react';

type UsageContext = {
  image_size?: string;
  reasoning_effort?: string;
  service_tier?: string;
};

const EFFORT_COLORS: Record<string, string> = {
  low: 'rgb(34,197,94)',
  medium: 'rgb(59,130,246)',
  high: 'rgb(249,115,22)',
  xhigh: 'rgb(239,68,68)',
};

function chipStyle(color: string): CSSProperties {
  return {
    background: `color-mix(in srgb, ${color} 18%, transparent)`,
    boxShadow: `inset 0 0 0 1px color-mix(in srgb, ${color} 34%, transparent)`,
    color,
  };
}

export function UsageModelMeta(props: UsageRecordSurfaceProps) {
  const ctx = (props.context ?? {}) as UsageContext;
  const chips: Array<{ label: string; color: string }> = [];

  if (ctx.reasoning_effort) {
    chips.push({
      label: ctx.reasoning_effort,
      color: EFFORT_COLORS[ctx.reasoning_effort] ?? 'rgb(148,163,184)',
    });
  }
  if (ctx.service_tier && ctx.service_tier !== 'auto' && ctx.service_tier !== 'default') {
    chips.push({ label: ctx.service_tier === 'priority' ? 'fast' : ctx.service_tier, color: 'rgb(192,132,252)' });
  }
  if (ctx.image_size) {
    chips.push({ label: ctx.image_size, color: 'rgb(74,222,128)' });
  }

  if (!chips.length) return null;

  return (
    <div className="flex shrink-0 gap-1">
      {chips.map((chip) => (
        <span
          key={chip.label}
          className="inline-flex shrink-0 items-center rounded px-1.5 text-[11px] font-semibold leading-4 whitespace-nowrap"
          style={chipStyle(chip.color)}
        >
          {chip.label}
        </span>
      ))}
    </div>
  );
}
