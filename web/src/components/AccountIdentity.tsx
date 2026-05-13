import type { CSSProperties } from 'react';
import type { AccountSurfaceProps } from '@doudou-start/airgate-theme/plugin';

type AccountLike = {
  type?: string;
  credentials?: Record<string, string>;
};

function readAccount(context: AccountSurfaceProps['context']): AccountLike {
  const account = context?.account;
  if (account && typeof account === 'object') return account as AccountLike;
  return {};
}

function typeLabel(type?: string) {
  if (type === 'oauth') return 'OAuth';
  if (type === 'apikey') return 'API Key';
  if (type === 'session_key') return 'Session Key';
  return type || '';
}

function titleCase(value: string) {
  return value ? value.charAt(0).toUpperCase() + value.slice(1) : value;
}

const rowStyle: CSSProperties = {
  display: 'flex',
  maxWidth: '100%',
  alignItems: 'center',
  justifyContent: 'center',
  gap: '0.25rem',
};

const typeBadgeStyle: CSSProperties = {
  maxWidth: '100%',
  overflow: 'hidden',
  border: '1px solid var(--ag-glass-border)',
  borderRadius: '0.25rem',
  background: 'var(--ag-bg-surface)',
  padding: '0 0.25rem',
  color: 'var(--ag-text-secondary)',
  fontSize: '0.625rem',
  lineHeight: 1,
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

const planBadgeStyle: CSSProperties = {
  maxWidth: '100%',
  overflow: 'hidden',
  borderRadius: '0.25rem',
  background: 'var(--ag-primary)',
  padding: '0 0.25rem',
  color: 'var(--ag-text-inverse)',
  fontSize: '0.625rem',
  fontWeight: 500,
  lineHeight: 1,
  opacity: 0.85,
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

export function AccountIdentity({ accountType, context }: AccountSurfaceProps) {
  const account = readAccount(context);
  const credentials = (context?.credentials as Record<string, string> | undefined) ?? account.credentials ?? {};
  const type = account.type || accountType;
  const planType = credentials.plan_type;
  const subscriptionUntil = credentials.subscription_active_until;
  const subscriptionExpired = subscriptionUntil ? new Date(subscriptionUntil) < new Date() : false;
  const hasQuotaMetadata = type === 'oauth' && (
    planType !== undefined || credentials.email !== undefined || subscriptionUntil !== undefined
  );
  const rawDisplayPlan = planType || (hasQuotaMetadata ? 'free' : '');
  const displayPlan = rawDisplayPlan && subscriptionExpired && rawDisplayPlan.toLowerCase() !== 'free'
    ? 'free'
    : rawDisplayPlan;
  const isPaid = displayPlan && displayPlan.toLowerCase() !== 'free';
  const planTitle = isPaid && subscriptionUntil && !subscriptionExpired
    ? `过期时间：${new Date(subscriptionUntil).toLocaleDateString()}`
    : undefined;

  return (
    <div style={rowStyle}>
      {type && <span style={typeBadgeStyle}>{typeLabel(type)}</span>}
      {displayPlan && (
        <span style={planBadgeStyle} title={planTitle}>
          {titleCase(displayPlan)}
        </span>
      )}
    </div>
  );
}
