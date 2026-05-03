import { useState, useCallback, useEffect } from 'react';
import { cssVar } from '@airgate/theme';
import type {
  AccountFormProps,
  PluginBatchAccountInput,
  PluginOAuthBatchExchangeResult,
} from '@airgate/theme/plugin';

type BatchExchangeResult = PluginOAuthBatchExchangeResult;
type BatchAccountInput = PluginBatchAccountInput;

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
  borderWidth: '1px',
  borderStyle: 'solid',
  borderColor: cssVar('glassBorder'),
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
  borderWidth: '1px',
  borderStyle: 'solid',
  borderColor: cssVar('glassBorder'),
  borderRadius: cssVar('radiusLg'),
  padding: '1rem',
  cursor: 'pointer',
  transition: 'border-color 0.2s, background-color 0.2s',
};

const cardActiveStyle: React.CSSProperties = {
  borderWidth: '1px',
  borderStyle: 'solid',
  borderColor: cssVar('primary'),
  borderRadius: cssVar('radiusLg'),
  padding: '1rem',
  cursor: 'pointer',
  transition: 'border-color 0.2s, background-color 0.2s',
  backgroundColor: cssVar('primarySubtle'),
};

const descStyle: React.CSSProperties = {
  fontSize: '0.75rem',
  color: cssVar('textTertiary'),
  marginTop: '0.25rem',
};

const pillStyle: React.CSSProperties = {
  display: 'inline-block',
  padding: '0.25rem 0.75rem',
  borderRadius: '9999px',
  fontSize: '0.75rem',
  cursor: 'pointer',
  transition: 'all 0.15s',
  borderWidth: '1px',
  borderStyle: 'solid',
  borderColor: cssVar('glassBorder'),
  color: cssVar('textSecondary'),
  backgroundColor: 'transparent',
};

const pillActiveStyle: React.CSSProperties = {
  ...pillStyle,
  borderColor: cssVar('primary'),
  color: cssVar('primary'),
  backgroundColor: cssVar('primarySubtle'),
};

type AccountType = 'apikey' | 'oauth';

/** OAuth 导入模式：浏览器授权 / 单个 Refresh Token / 批量 Refresh Token */
type OAuthMode = 'browser' | 'refresh_single' | 'refresh_batch';
type RefreshTokenImportType = 'codex' | 'mobile';

const mobileRefreshTokenClientID = 'app_LlGpXReQgckcGGUo2JrYvtJK';

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

function parseRefreshTokenLines(text: string): string[] {
  return text
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line.length > 0 && !line.startsWith('#'));
}

function StatusMessage({ status }: { status: { type: 'info' | 'success' | 'error'; text: string } | null }) {
  if (!status) return null;
  return (
    <div
      style={{
        fontSize: '0.75rem',
        color:
          status.type === 'error'
            ? cssVar('danger')
            : status.type === 'success'
              ? cssVar('success')
              : cssVar('textSecondary'),
      }}
    >
      {status.text}
    </div>
  );
}

export function AccountForm({
  credentials,
  onChange,
  mode,
  accountType: propType,
  onAccountTypeChange,
  onSuggestedName,
  onBatchModeChange,
  onBatchImport,
  oauth,
}: AccountFormProps) {
  const [localType, setLocalType] = useState<AccountType | ''>(
    (propType as AccountType) || (mode === 'edit' ? detectType(credentials) : ''),
  );
  const [oauthMode, setOauthMode] = useState<OAuthMode>('browser');
  const [authorizeURL, setAuthorizeURL] = useState('');
  const [callbackURL, setCallbackURL] = useState('');
  const [refreshTokenInput, setRefreshTokenInput] = useState('');
  const [refreshTokenImportType, setRefreshTokenImportType] = useState<RefreshTokenImportType>('codex');
  const [batchText, setBatchText] = useState('');
  const [batchPhase, setBatchPhase] = useState<'input' | 'running' | 'result'>('input');
  const [batchResults, setBatchResults] = useState<BatchExchangeResult[]>([]);
  const [batchImportedCount, setBatchImportedCount] = useState(0);
  const [oauthLoading, setOAuthLoading] = useState(false);
  const [oauthStatus, setOAuthStatus] = useState<{ type: 'info' | 'success' | 'error'; text: string } | null>(null);
  const accountType = (propType as AccountType | undefined) ?? localType;

  // 进入/退出批量模式时通知外层隐藏"下一步/创建"按钮
  const isBatchActive = mode === 'create' && accountType === 'oauth' && oauthMode === 'refresh_batch';
  useEffect(() => {
    onBatchModeChange?.(isBatchActive);
  }, [isBatchActive, onBatchModeChange]);

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
      setOauthMode('browser');
      setRefreshTokenInput('');
      setRefreshTokenImportType('codex');
      setBatchText('');
      setBatchPhase('input');
      setBatchResults([]);
      setBatchImportedCount(0);
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

  const submitRefreshTokenImport = useCallback(async () => {
    if (!oauth?.importRefresh || !refreshTokenInput.trim()) return;
    const clientId = refreshTokenImportType === 'mobile' ? mobileRefreshTokenClientID : undefined;
    setOAuthLoading(true);
    setOAuthStatus({ type: 'info', text: '正在使用 Refresh Token 换取凭证...' });
    try {
      const result = await oauth.importRefresh(refreshTokenInput.trim(), clientId);
      onAccountTypeChange?.(result.accountType || 'oauth');
      onChange({ ...credentials, ...result.credentials });
      if (result.accountName) {
        onSuggestedName?.(result.accountName);
      }
      setOAuthStatus({ type: 'success', text: '导入成功，凭证已自动填充。' });
      setRefreshTokenInput('');
    } catch (error) {
      setOAuthStatus({
        type: 'error',
        text: error instanceof Error ? error.message : 'Refresh Token 导入失败',
      });
    } finally {
      setOAuthLoading(false);
    }
  }, [oauth, refreshTokenInput, refreshTokenImportType, onAccountTypeChange, onChange, credentials, onSuggestedName]);

  const submitBatchRefreshImport = useCallback(async () => {
    if (!oauth?.batchImportRefresh || !onBatchImport) {
      setOAuthStatus({ type: 'error', text: '当前环境不支持批量导入' });
      return;
    }
    const tokens = parseRefreshTokenLines(batchText);
    if (tokens.length === 0) {
      setOAuthStatus({ type: 'error', text: '请至少粘贴一个 Refresh Token（每行一个）' });
      return;
    }
    const clientId = refreshTokenImportType === 'mobile' ? mobileRefreshTokenClientID : undefined;
    setBatchPhase('running');
    setOAuthStatus({ type: 'info', text: `正在批量换取 ${tokens.length} 个 Token...` });
    try {
      const results = await oauth.batchImportRefresh(tokens, clientId);
      setBatchResults(results);
      const successItems = results.filter((r) => r.status === 'ok' && r.credentials);
      if (successItems.length > 0) {
        const accounts: BatchAccountInput[] = successItems.map((r) => ({
          name: r.accountName || r.credentials.email || 'OpenAI OAuth',
          type: r.accountType || 'oauth',
          credentials: r.credentials,
        }));
        const importResp = await onBatchImport(accounts);
        setBatchImportedCount(importResp.imported);
      }
      setBatchPhase('result');
      setOAuthStatus(null);
    } catch (err) {
      setBatchPhase('input');
      setOAuthStatus({ type: 'error', text: err instanceof Error ? err.message : '批量导入失败' });
    }
  }, [batchText, refreshTokenImportType, oauth, onBatchImport]);

  const resetBatch = useCallback(() => {
    setBatchText('');
    setRefreshTokenImportType('codex');
    setBatchPhase('input');
    setBatchResults([]);
    setBatchImportedCount(0);
    setOAuthStatus(null);
  }, []);

  const copyAuthorizeURL = useCallback(async () => {
    if (!authorizeURL) return;

    // 尝试 1: Clipboard API（HTTPS 下可用）
    if (navigator.clipboard?.writeText) {
      try {
        await navigator.clipboard.writeText(authorizeURL);
        setOAuthStatus({ type: 'success', text: '授权链接已复制到剪贴板。' });
        return;
      } catch { /* 继续回退 */ }
    }

    // 尝试 2: execCommand（兼容旧浏览器和部分 HTTP 场景）
    try {
      const textarea = document.createElement('textarea');
      textarea.value = authorizeURL;
      textarea.setAttribute('readonly', '');
      textarea.style.position = 'fixed';
      textarea.style.left = '-9999px';
      document.body.appendChild(textarea);
      textarea.focus();
      textarea.select();
      const ok = document.execCommand('copy');
      document.body.removeChild(textarea);
      if (ok) {
        setOAuthStatus({ type: 'success', text: '授权链接已复制到剪贴板。' });
        return;
      }
    } catch { /* 继续回退 */ }

    // 尝试 3: 选中授权链接文本，提示用户手动 Ctrl+C
    const el = document.querySelector<HTMLTextAreaElement>('textarea[readonly]');
    if (el) {
      el.focus();
      el.select();
    }
    setOAuthStatus({ type: 'error', text: '自动复制不可用，请手动选中上方链接并按 Ctrl+C 复制。' });
  }, [authorizeURL]);

  const primaryBtnStyle = (disabled: boolean): React.CSSProperties => ({
    ...inputStyle,
    cursor: disabled ? 'not-allowed' : 'pointer',
    backgroundColor: cssVar('primary'),
    color: 'white',
    borderColor: 'transparent',
    fontWeight: 500,
    width: 'auto',
    opacity: disabled ? 0.6 : 1,
  });

  const ghostBtnStyle = (disabled: boolean): React.CSSProperties => ({
    ...inputStyle,
    cursor: disabled ? 'not-allowed' : 'pointer',
    backgroundColor: 'transparent',
    color: cssVar('text'),
    width: 'auto',
    opacity: disabled ? 0.6 : 1,
  });

  const outlineBtnStyle = (disabled: boolean): React.CSSProperties => ({
    ...inputStyle,
    cursor: disabled ? 'not-allowed' : 'pointer',
    backgroundColor: 'transparent',
    color: cssVar('primary'),
    borderColor: cssVar('primary'),
    width: 'auto',
    opacity: disabled ? 0.6 : 1,
  });

  const refreshTokenTypeField = (
    <div style={{ marginBottom: '0.75rem' }}>
      <label style={labelStyle}>Refresh Token 类型</label>
      <select
        style={inputStyle}
        value={refreshTokenImportType}
        onChange={(e) => setRefreshTokenImportType(e.target.value as RefreshTokenImportType)}
      >
        <option value="codex">普通 RT</option>
        <option value="mobile">Mobile RT</option>
      </select>
      <div style={descStyle}>普通 RT 使用后端默认 client_id；Mobile RT 使用 Sora Mobile client_id。</div>
    </div>
  );

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
            <div style={descStyle}>支持浏览器授权 / Refresh Token 导入</div>
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
              name="api_key"
              type="password"
              autoComplete="off"
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
              borderWidth: '1px',
              borderStyle: 'solid',
              borderColor: cssVar('glassBorder'),
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

          {oauth && mode === 'create' && (
            <div style={{ borderWidth: '1px', borderStyle: 'solid', borderColor: cssVar('glassBorder'), borderRadius: cssVar('radiusLg'), padding: '1rem', backgroundColor: cssVar('bgSurface') }}>
              <div style={{ fontSize: '0.875rem', fontWeight: 600, color: cssVar('text'), marginBottom: '0.5rem' }}>
                授权方式
              </div>
              <div style={{ display: 'flex', gap: '0.5rem', flexWrap: 'wrap', marginBottom: '0.875rem' }}>
                <span
                  style={oauthMode === 'browser' ? pillActiveStyle : pillStyle}
                  onClick={() => setOauthMode('browser')}
                >
                  浏览器授权
                </span>
                {oauth.importRefresh && (
                  <span
                    style={oauthMode === 'refresh_single' ? pillActiveStyle : pillStyle}
                    onClick={() => setOauthMode('refresh_single')}
                  >
                    Refresh Token 导入
                  </span>
                )}
                {oauth.batchImportRefresh && onBatchImport && (
                  <span
                    style={oauthMode === 'refresh_batch' ? pillActiveStyle : pillStyle}
                    onClick={() => setOauthMode('refresh_batch')}
                  >
                    批量导入
                  </span>
                )}
              </div>

              {oauthMode === 'browser' && (
                <>
                  <div style={{ ...descStyle, marginTop: 0, marginBottom: '0.75rem' }}>
                    先生成授权链接，在浏览器完成授权后，把完整回调 URL 粘贴回来完成交换。
                  </div>
                  <div style={{ display: 'flex', gap: '0.75rem', marginBottom: '0.75rem', flexWrap: 'wrap' }}>
                    <button type="button" onClick={startOAuth} disabled={oauthLoading} style={primaryBtnStyle(oauthLoading)}>
                      生成授权链接
                    </button>
                    <button type="button" onClick={copyAuthorizeURL} disabled={!authorizeURL || oauthLoading} style={ghostBtnStyle(!authorizeURL || oauthLoading)}>
                      复制授权链接
                    </button>
                  </div>
                  <div style={{ marginBottom: '0.75rem' }}>
                    <label style={labelStyle}>授权链接</label>
                    <textarea
                      style={{ ...inputStyle, minHeight: '120px', resize: 'vertical' }}
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
                      style={outlineBtnStyle(!callbackURL.trim() || oauthLoading)}
                    >
                      完成授权交换
                    </button>
                    <StatusMessage status={oauthStatus} />
                  </div>
                </>
              )}

              {oauthMode === 'refresh_single' && (
                <>
                  <div style={{ ...descStyle, marginTop: 0, marginBottom: '0.75rem' }}>
                    粘贴已有的 Refresh Token，后台会自动刷新拿回 access_token 并解析用户信息。
                  </div>
                  {refreshTokenTypeField}
                  <div style={{ marginBottom: '0.75rem' }}>
                    <label style={labelStyle}>Refresh Token</label>
                    <textarea
                      style={{ ...inputStyle, minHeight: '90px', resize: 'vertical' }}
                      placeholder="粘贴单个 Refresh Token"
                      value={refreshTokenInput}
                      onChange={(e) => setRefreshTokenInput(e.target.value)}
                      autoComplete="off"
                    />
                  </div>
                  <div style={{ display: 'flex', gap: '0.75rem', alignItems: 'center', flexWrap: 'wrap' }}>
                    <button
                      type="button"
                      onClick={submitRefreshTokenImport}
                      disabled={!refreshTokenInput.trim() || oauthLoading}
                      style={primaryBtnStyle(!refreshTokenInput.trim() || oauthLoading)}
                    >
                      导入
                    </button>
                    <StatusMessage status={oauthStatus} />
                  </div>
                </>
              )}

              {oauthMode === 'refresh_batch' && (
                <>
                  {batchPhase === 'input' && (
                    <>
                      <div style={{ ...descStyle, marginTop: 0, marginBottom: '0.75rem' }}>
                        每行一个 Refresh Token，批量换取凭证并一键创建账号（# 开头的行视为注释）。
                      </div>
                      {refreshTokenTypeField}
                      <div style={{ marginBottom: '0.75rem' }}>
                        <label style={labelStyle}>Refresh Tokens</label>
                        <textarea
                          style={{ ...inputStyle, minHeight: '180px', resize: 'vertical', fontFamily: 'monospace' }}
                          placeholder={'每行一个 Refresh Token\n# 以 # 开头的行会被忽略'}
                          value={batchText}
                          onChange={(e) => setBatchText(e.target.value)}
                          autoComplete="off"
                        />
                      </div>
                      <div style={{ display: 'flex', gap: '0.75rem', alignItems: 'center', flexWrap: 'wrap' }}>
                        <button
                          type="button"
                          onClick={submitBatchRefreshImport}
                          disabled={parseRefreshTokenLines(batchText).length === 0}
                          style={primaryBtnStyle(parseRefreshTokenLines(batchText).length === 0)}
                        >
                          批量导入 ({parseRefreshTokenLines(batchText).length})
                        </button>
                        <StatusMessage status={oauthStatus} />
                      </div>
                    </>
                  )}

                  {batchPhase === 'running' && (
                    <div style={{ fontSize: '0.875rem', color: cssVar('textSecondary') }}>
                      正在换取并导入，请稍候...
                    </div>
                  )}

                  {batchPhase === 'result' && (
                    <>
                      <div style={{ fontSize: '0.875rem', color: cssVar('text'), marginBottom: '0.75rem' }}>
                        共 {batchResults.length} 个，成功 {batchImportedCount} 个，失败 {batchResults.filter((r) => r.status === 'failed').length} 个
                      </div>
                      <div style={{
                        maxHeight: '280px',
                        overflowY: 'auto',
                        borderWidth: '1px',
                        borderStyle: 'solid',
                        borderColor: cssVar('glassBorder'),
                        borderRadius: cssVar('radiusMd'),
                        padding: '0.5rem 0.75rem',
                        marginBottom: '0.75rem',
                      }}>
                        {batchResults.map((r, idx) => (
                          <div key={idx} style={{
                            fontSize: '0.75rem',
                            padding: '0.375rem 0',
                            borderBottom: idx < batchResults.length - 1 ? `1px solid ${cssVar('glassBorder')}` : 'none',
                            color: r.status === 'ok' ? cssVar('success') : cssVar('danger'),
                          }}>
                            <span style={{ fontWeight: 500 }}>
                              [{r.status === 'ok' ? '成功' : '失败'}]
                            </span>{' '}
                            {r.status === 'ok'
                              ? (r.accountName || r.credentials.email || '未知账号')
                              : (r.error || '未知错误')}
                          </div>
                        ))}
                      </div>
                      <button type="button" onClick={resetBatch} style={ghostBtnStyle(false)}>
                        再导入一批
                      </button>
                    </>
                  )}
                </>
              )}
            </div>
          )}

          {/* 非批量模式下展示凭证字段 */}
          {!isBatchActive && mode === 'create' && (
            <>
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
              <div>
                <label style={labelStyle}>Client ID</label>
                <input
                  type="text"
                  autoComplete="off"
                  style={inputStyle}
                  placeholder="留空使用默认 Codex Client ID"
                  value={credentials.client_id ?? ''}
                  onChange={(e) => updateField('client_id', e.target.value)}
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
        </>
      )}
    </div>
  );
}
