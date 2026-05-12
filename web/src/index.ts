import { AccountForm } from './components/AccountForm';
import type { PluginFrontendModule } from '@doudou-start/airgate-theme/plugin';
import { OpenAIIcon } from './components/OpenAIIcon';

const plugin: PluginFrontendModule = {
  accountCreate: AccountForm,
  accountEdit: AccountForm,
  platformIcon: OpenAIIcon,
};

export default plugin;
