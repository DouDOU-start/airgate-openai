import { useState, useCallback } from 'react';

/** 账号表单 Props（由核心 AccountsPage 注入） */
export interface AccountFormProps {
  credentials: Record<string, string>;
  onChange: (credentials: Record<string, string>) => void;
  mode: 'create' | 'edit';
  accountType?: string;
  onAccountTypeChange?: (type: string) => void;
  /** OAuth 授权回调：核心 shell 调用此函数发起 OAuth 流程 */
  onOAuthStart?: () => void;
}

// 复用核心 CSS 变量的样式
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

/** OpenAI 账号类型 */
type AccountType = 'apikey' | 'oauth';

/** 检测当前凭证对应的账号类型 */
function detectType(credentials: Record<string, string>): AccountType | '' {
  if (credentials.api_key) return 'apikey';
  if (credentials.access_token) return 'oauth';
  return '';
}

/** OpenAI 插件自定义账号表单 */
export function AccountForm({ credentials, onChange, mode, accountType: propType, onAccountTypeChange, onOAuthStart }: AccountFormProps) {
  const [localType, setLocalType] = useState<AccountType | ''>(
    (propType as AccountType) || (mode === 'edit' ? detectType(credentials) : ''),
  );
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
      // 切换类型时清空凭证，保留 base_url
      const baseUrl = credentials.base_url || '';
      if (type === 'apikey') {
        onChange({ api_key: '', base_url: baseUrl });
      } else {
        onChange({ access_token: '', refresh_token: '', chatgpt_account_id: '', base_url: baseUrl });
      }
    },
    [credentials.base_url, onChange, onAccountTypeChange],
  );

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
      {/* 账号类型选择 */}
      <div>
        <span style={labelStyle}>账号类型 *</span>
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '0.75rem' }}>
          <div
            style={accountType === 'apikey' ? cardActiveStyle : cardStyle}
            onClick={() => handleTypeChange('apikey')}
          >
            <div style={{ fontSize: '0.875rem', fontWeight: 500, color: 'var(--ag-text, #e8ecf4)' }}>
              API Key
            </div>
            <div style={descStyle}>使用 OpenAI API Key 直连</div>
          </div>
          <div
            style={accountType === 'oauth' ? cardActiveStyle : cardStyle}
            onClick={() => handleTypeChange('oauth')}
          >
            <div style={{ fontSize: '0.875rem', fontWeight: 500, color: 'var(--ag-text, #e8ecf4)' }}>
              OAuth 登录
            </div>
            <div style={descStyle}>通过浏览器授权登录</div>
          </div>
        </div>
      </div>

      {/* API Key 模式字段 */}
      {accountType === 'apikey' && (
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

      {/* OAuth 模式字段 */}
      {accountType === 'oauth' && (
        <>
          {/* 授权登录按钮 */}
          {mode === 'create' && onOAuthStart && (
            <div style={{ textAlign: 'center', padding: '0.5rem 0' }}>
              <button
                type="button"
                onClick={onOAuthStart}
                style={{
                  ...inputStyle,
                  cursor: 'pointer',
                  backgroundColor: 'var(--ag-primary, #3b82f6)',
                  color: 'white',
                  border: 'none',
                  fontWeight: 500,
                  fontSize: '0.9rem',
                  padding: '0.6rem 1.5rem',
                  width: 'auto',
                  display: 'inline-block',
                }}
              >
                浏览器授权登录
              </button>
              <div style={{ ...descStyle, marginTop: '0.5rem' }}>
                点击后将打开 OpenAI 授权页面，授权完成后自动填充凭证
              </div>
            </div>
          )}

          {/* 手动填写凭证 */}
          <div>
            <label style={labelStyle}>
              Access Token {!onOAuthStart && <span style={{ color: 'var(--ag-danger, #ef4444)' }}>*</span>}
            </label>
            <input
              type="password"
              style={inputStyle}
              placeholder={onOAuthStart ? '授权后自动填充，或手动输入' : 'eyJhbG...'}
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
