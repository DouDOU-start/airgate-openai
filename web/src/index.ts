import { AccountForm } from './components/AccountForm';
import type { AccountFormProps } from './components/AccountForm';
import { OpenAIIcon } from './components/OpenAIIcon';

/** 插件前端模块导出 */
export interface PluginFrontendModule {
  accountForm?: React.ComponentType<AccountFormProps>;
  platformIcon?: React.ComponentType<{ className?: string; style?: React.CSSProperties }>;
}

const plugin: PluginFrontendModule = {
  accountForm: AccountForm,
  platformIcon: OpenAIIcon,
};

export default plugin;
