import { useState, useCallback } from 'react';

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

const inputStyle: React.CSSProperties = {
  display: 'block',
  width: '100%',
  borderRadius: 'var(--ag-radius-md, 0.5rem)',
  border: '1px solid var(--ag-glass-border, #2a3050)',
  backgroundColor: 'var(--ag-bg-surface, #1c2237)',
  padding: '0.5rem 0.75rem',
  fontSize: '0.875rem',
  color: 'var(--ag-text, #e8ecf4)',
  outline: 'none',
  transition: 'border-color 0.2s, box-shadow 0.2s',
};

const labelStyle: React.CSSProperties = {
  display: 'block',
  fontSize: '0.75rem',
  fontWeight: 500,
  color: 'var(--ag-text-secondary, #8892a8)',
  textTransform: 'uppercase',
  letterSpacing: '0.05em',
  marginBottom: '0.375rem',
};

const cardStyle: React.CSSProperties = {
  border: '1px solid var(--ag-glass-border, #2a3050)',
  borderRadius: 'var(--ag-radius-lg, 0.75rem)',
  padding: '1rem',
  cursor: 'pointer',
  transition: 'border-color 0.2s, background-color 0.2s',
};

const cardActiveStyle: React.CSSProperties = {
  ...cardStyle,
  borderColor: 'var(--ag-primary, #3b82f6)',
  backgroundColor: 'var(--ag-primary-subtle, rgba(59,130,246,0.08))',
};

const descStyle: React.CSSProperties = {
  fontSize: '0.75rem',
  color: 'var(--ag-text-tertiary, #5a637a)',
  marginTop: '0.25rem',
};

type AccountType = 'apikey' | 'sub2api' | 'oauth';

function detectType(credentials: Record<string, string>): AccountType | '' {
  if (credentials.provider === 'sub2api') return 'sub2api';
  if (credentials.api_key) return 'apikey';
  if (credentials.access_token) return 'oauth';
  return '';
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
      } else if (type === 'sub2api') {
        onChange({ api_key: '', base_url: credentials.base_url || 'https://sub2api.xxxx.com', provider: 'sub2api' });
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
      <div>
        <span style={labelStyle}>账号类型 *</span>
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: '0.75rem' }}>
          <div style={accountType === 'apikey' ? cardActiveStyle : cardStyle} onClick={() => handleTypeChange('apikey')}>
            <div style={{ fontSize: '0.875rem', fontWeight: 500, color: 'var(--ag-text, #e8ecf4)' }}>API Key</div>
            <div style={descStyle}>使用 OpenAI API Key 直连</div>
          </div>
          <div style={accountType === 'sub2api' ? cardActiveStyle : cardStyle} onClick={() => handleTypeChange('sub2api')}>
            <div style={{ fontSize: '0.875rem', fontWeight: 500, color: 'var(--ag-text, #e8ecf4)' }}>Sub2API</div>
            <div style={descStyle}>使用 sub2api 转发（默认 Responses）</div>
          </div>
          <div style={accountType === 'oauth' ? cardActiveStyle : cardStyle} onClick={() => handleTypeChange('oauth')}>
            <div style={{ fontSize: '0.875rem', fontWeight: 500, color: 'var(--ag-text, #e8ecf4)' }}>OAuth 登录</div>
            <div style={descStyle}>通过浏览器授权登录</div>
          </div>
        </div>
      </div>

      {(accountType === 'apikey' || accountType === 'sub2api') && (
        <>
          <div>
            <label style={labelStyle}>
              API Key <span style={{ color: 'var(--ag-danger, #ef4444)' }}>*</span>
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
              placeholder={accountType === 'sub2api' ? 'https://sub2api.xxxxx.com' : 'https://api.openai.com'}
              value={credentials.base_url ?? ''}
              onChange={(e) => updateField('base_url', e.target.value)}
            />
            <div style={{ ...descStyle, marginTop: '0.375rem' }}>
              {accountType === 'sub2api' ? 'Sub2API 账号将自动按 sub2api 协议转发' : '留空使用默认地址，支持自定义反向代理'}
            </div>
          </div>
        </>
      )}

      {accountType === 'oauth' && (
        <>
          {oauth && (
            <div style={{ border: '1px solid var(--ag-glass-border, #2a3050)', borderRadius: 'var(--ag-radius-lg, 0.75rem)', padding: '1rem', backgroundColor: 'var(--ag-bg-surface, #1c2237)' }}>
              <div style={{ fontSize: '0.875rem', fontWeight: 600, color: 'var(--ag-text, #e8ecf4)', marginBottom: '0.25rem' }}>
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
                    backgroundColor: 'var(--ag-primary, #3b82f6)',
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
                    color: 'var(--ag-text, #e8ecf4)',
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
                  placeholder="点击“生成授权链接”后，这里会显示完整授权地址"
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
                    color: 'var(--ag-primary, #3b82f6)',
                    border: '1px solid var(--ag-primary, #3b82f6)',
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
                          ? 'var(--ag-danger, #ef4444)'
                          : oauthStatus.type === 'success'
                            ? '#4ade80'
                            : 'var(--ag-text-secondary, #8892a8)',
                    }}
                  >
                    {oauthStatus.text}
                  </div>
                )}
              </div>
            </div>
          )}

          <div>
            <label style={labelStyle}>
              Access Token {!oauth && <span style={{ color: 'var(--ag-danger, #ef4444)' }}>*</span>}
            </label>
            <input
              type="password"
              style={inputStyle}
              placeholder={oauth ? '授权后自动填充，或手动输入' : 'eyJhbG...'}
              value={credentials.access_token ?? ''}
              onChange={(e) => updateField('access_token', e.target.value)}
            />
          </div>
          <div>
            <label style={labelStyle}>Refresh Token</label>
            <input
              type="password"
              style={inputStyle}
              placeholder="授权后自动填充"
              value={credentials.refresh_token ?? ''}
              onChange={(e) => updateField('refresh_token', e.target.value)}
            />
          </div>
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
        </>
      )}
    </div>
  );
}
