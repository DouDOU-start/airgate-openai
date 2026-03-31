import { useState, useCallback } from 'react';
import { cssVar } from '@airgate/theme';

/** 账号表单 Props（由核心 AccountsPage 注入） */
export interface AccountFormProps {
  credentials: Record<string, string>;
  onChange: (credentials: Record<string, string>) => void;
  mode: 'create' | 'edit';
  accountType?: string;
  onAccountTypeChange?: (type: string) => void;
  onSuggestedName?: (name: string) => void;
  oauth?: {
    start: () => Promise<{ authorizeURL: string; state: string }>;
    exchange: (callbackURL: string) => Promise<{
      accountType: string;
      accountName: string;
      credentials: Record<string, string>;
    }>;
  };
}

/** 订阅计划显示名称和颜色映射 */
const planDisplayMap: Record<string, { label: string; color: string; bg: string }> = {
  free: { label: 'Free', color: '#6b7280', bg: '#f3f4f6' },
  plus: { label: 'Plus', color: '#059669', bg: '#d1fae5' },
  pro: { label: 'Pro', color: '#7c3aed', bg: '#ede9fe' },
  team: { label: 'Team', color: '#2563eb', bg: '#dbeafe' },
};

const inputStyle: React.CSSProperties = {
  display: 'block',
  width: '100%',
  borderRadius: cssVar('radiusMd'),
  border: `1px solid ${cssVar('glassBorder')}`,
  backgroundColor: cssVar('bgSurface'),
  padding: '0.5rem 0.75rem',
  fontSize: '0.875rem',
  color: cssVar('text'),
  outline: 'none',
  transition: 'border-color 0.2s, box-shadow 0.2s',
};

/** 密码字段样式：用 CSS 遮蔽代替 type="password"，避免浏览器自动填充 */
const passwordInputStyle = {
  ...inputStyle,
  WebkitTextSecurity: 'disc',
  textSecurity: 'disc',
} as React.CSSProperties;

const labelStyle: React.CSSProperties = {
  display: 'block',
  fontSize: '0.75rem',
  fontWeight: 500,
  color: cssVar('textSecondary'),
  textTransform: 'uppercase',
  letterSpacing: '0.05em',
  marginBottom: '0.375rem',
};

const cardStyle: React.CSSProperties = {
  border: `1px solid ${cssVar('glassBorder')}`,
  borderRadius: cssVar('radiusLg'),
  padding: '1rem',
  cursor: 'pointer',
  transition: 'border-color 0.2s, background-color 0.2s',
};

const cardActiveStyle: React.CSSProperties = {
  ...cardStyle,
  borderColor: cssVar('primary'),
  backgroundColor: cssVar('primarySubtle'),
};

const descStyle: React.CSSProperties = {
  fontSize: '0.75rem',
  color: cssVar('textTertiary'),
  marginTop: '0.25rem',
};

type AccountType = 'apikey' | 'oauth';

function detectType(credentials: Record<string, string>): AccountType | '' {
  if (credentials.api_key) return 'apikey';
  if (credentials.access_token) return 'oauth';
  return '';
}

/** 从 JWT access_token 中解析订阅信息（不验签） */
function parseJWTSubscription(token: string): { planType: string; subscriptionUntil: string } {
  try {
    const parts = token.split('.');
    if (parts.length !== 3) return { planType: '', subscriptionUntil: '' };
    const payload = JSON.parse(atob(parts[1].replace(/-/g, '+').replace(/_/g, '/')));
    const auth = payload['https://api.openai.com/auth'] || {};
    return {
      planType: auth.chatgpt_plan_type || '',
      subscriptionUntil: auth.chatgpt_subscription_active_until
        ? String(auth.chatgpt_subscription_active_until)
        : '',
    };
  } catch {
    return { planType: '', subscriptionUntil: '' };
  }
}

export function AccountForm({
  credentials,
  onChange,
  mode,
  accountType: propType,
  onAccountTypeChange,
  onSuggestedName,
  oauth,
}: AccountFormProps) {
  const [localType, setLocalType] = useState<AccountType | ''>(
    (propType as AccountType) || (mode === 'edit' ? detectType(credentials) : ''),
  );
  const [authorizeURL, setAuthorizeURL] = useState('');
  const [callbackURL, setCallbackURL] = useState('');
  const [oauthLoading, setOAuthLoading] = useState(false);
  const [oauthStatus, setOAuthStatus] = useState<{ type: 'info' | 'success' | 'error'; text: string } | null>(null);
  const accountType = (propType as AccountType | undefined) ?? localType;

  // 从 credentials 中读取订阅信息，没有则从 access_token JWT 解析
  const jwtInfo = (!credentials.plan_type && credentials.access_token)
    ? parseJWTSubscription(credentials.access_token)
    : null;
  const planType = credentials.plan_type || jwtInfo?.planType || '';
  const subscriptionUntil = credentials.subscription_active_until || jwtInfo?.subscriptionUntil || '';

  const updateField = useCallback(
    (key: string, value: string) => {
      onChange({ ...credentials, [key]: value });
    },
    [credentials, onChange],
  );

  const handleTypeChange = useCallback(
    (type: AccountType) => {
      setLocalType(type);
      onAccountTypeChange?.(type);
      setAuthorizeURL('');
      setCallbackURL('');
      setOAuthStatus(null);
      const baseUrl = credentials.base_url || '';
      if (type === 'apikey') {
        onChange({ api_key: '', base_url: baseUrl, provider: '' });
      } else {
        onChange({ access_token: '', refresh_token: '', chatgpt_account_id: '', base_url: baseUrl, provider: '' });
      }
    },
    [credentials.base_url, onChange, onAccountTypeChange],
  );

  const startOAuth = useCallback(async () => {
    if (!oauth) return;
    setOAuthLoading(true);
    setOAuthStatus({ type: 'info', text: '正在生成授权链接...' });
    try {
      const result = await oauth.start();
      setAuthorizeURL(result.authorizeURL);
      setCallbackURL('');
      setOAuthStatus({ type: 'success', text: '授权链接已生成，请复制到浏览器完成授权。' });
    } catch (error) {
      setOAuthStatus({
        type: 'error',
        text: error instanceof Error ? error.message : '生成授权链接失败',
      });
    } finally {
      setOAuthLoading(false);
    }
  }, [oauth]);

  const submitOAuthCallback = useCallback(async () => {
    if (!oauth || !callbackURL.trim()) return;
    setOAuthLoading(true);
    setOAuthStatus({ type: 'info', text: '正在完成授权交换...' });
    try {
      const result = await oauth.exchange(callbackURL.trim());
      onAccountTypeChange?.(result.accountType || 'oauth');
      onChange({ ...credentials, ...result.credentials });
      if (result.accountName) {
        onSuggestedName?.(result.accountName);
      }
      setOAuthStatus({ type: 'success', text: '授权成功，凭证已自动填充。' });
    } catch (error) {
      setOAuthStatus({
        type: 'error',
        text: error instanceof Error ? error.message : '授权交换失败',
      });
    } finally {
      setOAuthLoading(false);
    }
  }, [oauth, callbackURL, onAccountTypeChange, onChange, credentials, onSuggestedName]);

  const copyAuthorizeURL = useCallback(async () => {
    if (!authorizeURL) return;
    try {
      await navigator.clipboard.writeText(authorizeURL);
      setOAuthStatus({ type: 'success', text: '授权链接已复制到剪贴板。' });
    } catch {
      setOAuthStatus({ type: 'error', text: '复制失败，请手动复制授权链接。' });
    }
  }, [authorizeURL]);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
      {/* 账号类型选择（编辑模式下只读） */}
      <div>
        <span style={labelStyle}>账号类型 {mode === 'create' ? '*' : ''}</span>
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '0.75rem' }}>
          <div
            style={{
              ...(accountType === 'apikey' ? cardActiveStyle : cardStyle),
              ...(mode === 'edit' && accountType !== 'apikey' ? { opacity: 0.4, cursor: 'not-allowed' } : {}),
            }}
            onClick={mode === 'create' ? () => handleTypeChange('apikey') : undefined}
          >
            <div style={{ fontSize: '0.875rem', fontWeight: 500, color: cssVar('text') }}>API Key</div>
            <div style={descStyle}>支持所有 Responses 标准接口</div>
          </div>
          <div
            style={{
              ...(accountType === 'oauth' ? cardActiveStyle : cardStyle),
              ...(mode === 'edit' && accountType !== 'oauth' ? { opacity: 0.4, cursor: 'not-allowed' } : {}),
            }}
            onClick={mode === 'create' ? () => handleTypeChange('oauth') : undefined}
          >
            <div style={{ fontSize: '0.875rem', fontWeight: 500, color: cssVar('text') }}>OAuth 登录</div>
            <div style={descStyle}>通过浏览器授权登录</div>
          </div>
        </div>
      </div>

      {accountType === 'apikey' && (
        <>
          <div>
            <label style={labelStyle}>
              API Key <span style={{ color: cssVar('danger') }}>*</span>
            </label>
            <input
              type="password"
              style={inputStyle}
              placeholder="sk-..."
              value={credentials.api_key ?? ''}
              onChange={(e) => updateField('api_key', e.target.value)}
            />
          </div>
          <div>
            <label style={labelStyle}>API 地址</label>
            <input
              type="text"
              style={inputStyle}
              placeholder="https://api.openai.com"
              value={credentials.base_url ?? ''}
              onChange={(e) => updateField('base_url', e.target.value)}
            />
            <div style={{ ...descStyle, marginTop: '0.375rem' }}>
              留空使用默认地址，支持自定义反向代理
            </div>
          </div>
        </>
      )}

      {accountType === 'oauth' && (
        <>
          {/* 订阅信息展示 */}
          {(planType || subscriptionUntil) && (
            <div style={{
              border: `1px solid ${cssVar('glassBorder')}`,
              borderRadius: cssVar('radiusLg'),
              padding: '0.875rem 1rem',
              backgroundColor: cssVar('bgSurface'),
              display: 'flex',
              alignItems: 'center',
              gap: '0.75rem',
              flexWrap: 'wrap',
            }}>
              <div style={{ fontSize: '0.75rem', fontWeight: 500, color: cssVar('textSecondary'), textTransform: 'uppercase', letterSpacing: '0.05em' }}>
                订阅
              </div>
              {planType && (() => {
                const plan = planDisplayMap[planType] || { label: planType, color: cssVar('text'), bg: cssVar('bgSurface') };
                return (
                  <span style={{
                    display: 'inline-block',
                    padding: '0.125rem 0.5rem',
                    borderRadius: '9999px',
                    fontSize: '0.75rem',
                    fontWeight: 600,
                    color: plan.color,
                    backgroundColor: plan.bg,
                  }}>
                    {plan.label}
                  </span>
                );
              })()}
              {subscriptionUntil && (
                <span style={{ fontSize: '0.75rem', color: cssVar('textTertiary') }}>
                  有效期至 {subscriptionUntil}
                </span>
              )}
            </div>
          )}

          {oauth && (
            <div style={{ border: `1px solid ${cssVar('glassBorder')}`, borderRadius: cssVar('radiusLg'), padding: '1rem', backgroundColor: cssVar('bgSurface') }}>
              <div style={{ fontSize: '0.875rem', fontWeight: 600, color: cssVar('text'), marginBottom: '0.25rem' }}>
                OAuth 授权辅助
              </div>
              <div style={{ ...descStyle, marginTop: 0, marginBottom: '0.75rem' }}>
                先生成授权链接，在浏览器完成授权后，把完整回调 URL 粘贴回来完成交换。
              </div>
              <div style={{ display: 'flex', gap: '0.75rem', marginBottom: '0.75rem', flexWrap: 'wrap' }}>
                <button
                  type="button"
                  onClick={startOAuth}
                  disabled={oauthLoading}
                  style={{
                    ...inputStyle,
                    cursor: oauthLoading ? 'not-allowed' : 'pointer',
                    backgroundColor: cssVar('primary'),
                    color: 'white',
                    border: 'none',
                    fontWeight: 500,
                    width: 'auto',
                    opacity: oauthLoading ? 0.6 : 1,
                  }}
                >
                  生成授权链接
                </button>
                <button
                  type="button"
                  onClick={copyAuthorizeURL}
                  disabled={!authorizeURL || oauthLoading}
                  style={{
                    ...inputStyle,
                    cursor: !authorizeURL || oauthLoading ? 'not-allowed' : 'pointer',
                    backgroundColor: 'transparent',
                    color: cssVar('text'),
                    width: 'auto',
                    opacity: !authorizeURL || oauthLoading ? 0.6 : 1,
                  }}
                >
                  复制授权链接
                </button>
              </div>
              <div style={{ marginBottom: '0.75rem' }}>
                <label style={labelStyle}>授权链接</label>
                <textarea
                  style={{ ...inputStyle, minHeight: '76px', resize: 'vertical' }}
                  readOnly
                  placeholder='点击"生成授权链接"后，这里会显示完整授权地址'
                  value={authorizeURL}
                />
              </div>
              <div style={{ marginBottom: '0.75rem' }}>
                <label style={labelStyle}>回调 URL</label>
                <textarea
                  style={{ ...inputStyle, minHeight: '76px', resize: 'vertical' }}
                  placeholder="粘贴完整回调 URL，例如 http://localhost:1455/auth/callback?code=...&state=..."
                  value={callbackURL}
                  onChange={(e) => setCallbackURL(e.target.value)}
                />
              </div>
              <div style={{ display: 'flex', gap: '0.75rem', alignItems: 'center', flexWrap: 'wrap' }}>
                <button
                  type="button"
                  onClick={submitOAuthCallback}
                  disabled={!callbackURL.trim() || oauthLoading}
                  style={{
                    ...inputStyle,
                    cursor: !callbackURL.trim() || oauthLoading ? 'not-allowed' : 'pointer',
                    backgroundColor: 'transparent',
                    color: cssVar('primary'),
                    border: `1px solid ${cssVar('primary')}`,
                    width: 'auto',
                    opacity: !callbackURL.trim() || oauthLoading ? 0.6 : 1,
                  }}
                >
                  完成授权交换
                </button>
                {oauthStatus && (
                  <div
                    style={{
                      fontSize: '0.75rem',
                      color:
                        oauthStatus.type === 'error'
                          ? cssVar('danger')
                          : oauthStatus.type === 'success'
                            ? cssVar('success')
                            : cssVar('textSecondary'),
                    }}
                  >
                    {oauthStatus.text}
                  </div>
                )}
              </div>
            </div>
          )}

          {mode === 'create' && (
            <div>
              <label style={labelStyle}>
                Access Token {!oauth && <span style={{ color: cssVar('danger') }}>*</span>}
              </label>
              <input
                type="text"
                autoComplete="off"
                style={passwordInputStyle}
                placeholder={oauth ? '授权后自动填充，或手动输入' : 'eyJhbG...'}
                value={credentials.access_token ?? ''}
                onChange={(e) => updateField('access_token', e.target.value)}
              />
            </div>
          )}
          {mode === 'create' && (
            <div>
              <label style={labelStyle}>Refresh Token</label>
              <input
                type="text"
                autoComplete="off"
                style={passwordInputStyle}
                placeholder="授权后自动填充"
                value={credentials.refresh_token ?? ''}
                onChange={(e) => updateField('refresh_token', e.target.value)}
              />
            </div>
          )}
          {mode === 'create' && (
            <div>
              <label style={labelStyle}>ChatGPT Account ID</label>
              <input
                type="text"
                style={inputStyle}
                placeholder="授权后自动填充"
                value={credentials.chatgpt_account_id ?? ''}
                onChange={(e) => updateField('chatgpt_account_id', e.target.value)}
              />
            </div>
          )}
        </>
      )}
    </div>
  );
}
