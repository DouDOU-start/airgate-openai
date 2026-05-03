import { AccountForm } from './components/AccountForm';
import type { PluginFrontendModule } from '@airgate/theme/plugin';
import { OpenAIIcon } from './components/OpenAIIcon';

const plugin: PluginFrontendModule = {
  accountForm: AccountForm,
  platformIcon: OpenAIIcon,
};

export default plugin;
