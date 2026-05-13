import { AccountForm } from './components/AccountForm';
import type { PluginFrontendModule } from '@doudou-start/airgate-theme/plugin';
import { OpenAIIcon } from './components/OpenAIIcon';
import { AccountIdentity } from './components/AccountIdentity';
import { UsageCostDetail } from './components/UsageCostDetail';
import { UsageMetricDetail } from './components/UsageMetricDetail';
import { UsageModelMeta } from './components/UsageModelMeta';
import { UsageWindow } from './components/UsageWindow';

const plugin: PluginFrontendModule = {
  accountCreate: AccountForm,
  accountEdit: AccountForm,
  accountIdentity: AccountIdentity,
  accountUsageWindow: UsageWindow,
  usageModelMeta: UsageModelMeta,
  usageMetricDetail: UsageMetricDetail,
  usageCostDetail: UsageCostDetail,
  platformIcon: OpenAIIcon,
};

export default plugin;
