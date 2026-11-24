// 手写类型，对齐 docs/openapi.yaml 与 internal/admin 的请求/响应结构。

export type ID = number;

export interface AIGateModel {
  id: ID;
  provider: string;
  model_id: string;
  model_name: string;
  url: string;
  credential_id: ID;
  model_type: string;
  custom_http_headers: Record<string, string> | null;
  quota_day: number;
  enabled: boolean;
  user_key_required: boolean;
  /** 模型计价币种（ISO 4217 三位大写）；空串表示未配置，不参与金额记账。 */
  currency: string;
  /** 是否启用峰谷定价；时段随价目快照保存在 peak_hours。 */
  enable_peak_pricing: boolean;
  /** 是否优先采信上游自报的 usage.cost。 */
  use_upstream_cost: boolean;
  /** 上下文窗口 token 数；0 表示未配置（仅展示）。 */
  context_window: number;
  /** 模型级输出上限；0 表示未配置，>0 时与密钥级上限取较小者。 */
  max_output_tokens: number;
  /** 是否支持图片输入（能力声明，当前不拦截）。 */
  supports_image: boolean;
  /** 支持的 reasoning_effort 等级（升序）；未配置时为空数组。 */
  effort_levels: string[];
  /** 当前生效的价目快照；无价目时为 null。 */
  current_price: AIModelPrice | null;
  created_at: string;
  updated_at: string;
}

export interface ModelInput {
  provider: string;
  model_id: string;
  model_name: string;
  url: string;
  api_key?: string;
  credential_id?: ID;
  model_type?: string;
  custom_http_headers?: Record<string, string>;
  quota_day?: number;
  enabled?: boolean;
  user_key_required?: boolean;
  currency?: string;
  enable_peak_pricing?: boolean;
  use_upstream_cost?: boolean;
  context_window?: number;
  max_output_tokens?: number;
  supports_image?: boolean;
  effort_levels?: string[];
}

/** PeakWindow 是一个高峰时段窗口；days 用 Go time.Weekday 编号（0=周日 … 6=周六）。 */
export interface PeakWindow {
  /** 指窗口开始的那一天。 */
  days: number[];
  /** "HH:MM"；end <= start 表示跨午夜，"24:00" 合法。 */
  start: string;
  end: string;
}

/** PeakSchedule 是模型级的峰谷时段配置，时区为 IANA 名称。 */
export interface PeakSchedule {
  timezone: string;
  windows: PeakWindow[];
}

/**
 * AIModelPrice 是一份价目快照。micros 字段为「每百万 token 的微元数」，
 * 展示时要除以 1e6 换成币种单位（整数微元只在这里和录入表单之间换算）。
 */
export interface AIModelPrice {
  id: ID;
  ai_model_id: ID;
  currency: string;
  /** RFC3339 时间戳。 */
  effective_at: string;
  /** 服务端原样回显的 JSON 文本，便于直接核对厂商价目页；无时段时为 "{}"。 */
  peak_hours: string;
  peak_input_hit_micros: number;
  peak_input_miss_micros: number;
  peak_output_micros: number;
  offpeak_input_hit_micros: number;
  offpeak_input_miss_micros: number;
  offpeak_output_micros: number;
  created_at: string;
}

export interface PriceInput {
  /** 缺省取模型当前币种。 */
  currency?: string;
  effective_at: string;
  peak_hours: string;
  peak_input_hit_micros: number;
  peak_input_miss_micros: number;
  peak_output_micros: number;
  offpeak_input_hit_micros: number;
  offpeak_input_miss_micros: number;
  offpeak_output_micros: number;
}

/** BillingCalendarDay 是计费日历的一天：is_workday 为 false 强制判为非高峰。 */
export interface BillingCalendarDay {
  /** YYYY-MM-DD（业务时区日期）。 */
  date: string;
  is_workday: boolean;
  note: string;
}

export interface BillingCalendarInput {
  date: string;
  is_workday: boolean;
  note?: string;
}

/** CurrencyRate：1 单位 from_currency = rate_micros/1e6 单位 to_currency。 */
export interface CurrencyRate {
  id: ID;
  from_currency: string;
  to_currency: string;
  /** RFC3339 时间戳。 */
  effective_at: string;
  rate_micros: number;
  created_at: string;
}

export interface CurrencyRateInput {
  from_currency: string;
  to_currency: string;
  effective_at: string;
  rate_micros: number;
}

export interface SecretItem {
  key: string;
  value: string;
  location: string;
}

export interface Credential {
  id: ID;
  name: string;
  description: string;
  secrets: SecretItem[];
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

export interface CredentialInput {
  name: string;
  description: string;
  secrets: SecretItem[];
  enabled?: boolean;
}

export interface Binding {
  ai_model_id: ID;
  model_id: string;
  model_name: string;
  provider: string;
  role: string;
  sort_order: number;
  has_key: boolean;
}

export interface ApiKey {
  id: ID;
  name: string;
  user_id: ID;
  group_id: ID;
  proxy_token_prefix: string;
  max_tokens: number;
  token_quota_rules: string;
  enabled: boolean;
  expires_at: string | null;
  expired_at: string | null;
  created_at: string;
  updated_at: string;
  bindings: Binding[];
  token?: string;
}

export interface BindingInput {
  ai_model_id: ID;
  role: string;
  sort_order: number;
  api_key?: string;
}

export interface KeyInput {
  name: string;
  user_id: ID;
  group_id: ID;
  max_tokens: number;
  token_quota_rules?: string;
  expires_at?: string | null;
  bindings: BindingInput[];
}

export interface User {
  id: ID;
  username: string;
  role: string;
  group_id: ID;
  api_token_prefix: string;
  enabled: boolean;
  created_at: string;
  updated_at: string;
  token?: string;
}

export interface UserInput {
  username: string;
  role: string;
  group_id: ID;
  enabled?: boolean;
}

export interface UserGroup {
  id: ID;
  name: string;
  token_quota_rules: string;
  global_token_quota_rules: string;
  created_at: string;
  updated_at: string;
}

export interface GroupInput {
  name: string;
  token_quota_rules?: string;
  global_token_quota_rules?: string;
}

export interface Settings {
  default_token_quota_rules: string;
  global_token_quota_rules: string;
  global_quota_scope: string;
  user_key_limit: number;
  /** 平台结算币种（ISO 4217 三位大写，全局唯一）。 */
  platform_currency: string;
}

export interface Webhook {
  id: ID;
  name: string;
  url: string;
  secret: string;
  event_types: string[];
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

export interface WebhookInput {
  name: string;
  url: string;
  secret: string;
  event_types: string[];
  enabled?: boolean;
}

export interface WebhookDelivery {
  id: ID;
  webhook_id: ID;
  event_type: string;
  status: string;
  attempts: number;
  last_status: number;
  last_error: string;
  created_at: string;
  updated_at: string;
}

export interface UsageSummaryRow {
  date?: string;
  user_id?: ID;
  api_key_id?: ID;
  ai_model_id?: ID;
  group_id?: ID;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  prompt_cache_read_tokens: number;
  prompt_cache_write_tokens: number;
  request_count: number;
  /** 行金额的计价币种；只有 group_by=model 时唯一有意义，空串表示未配置。 */
  currency?: string;
  /** 模型币种金额（整数微元）；只有 group_by=model 时可直接相加，其余维度恒为 0。 */
  cost_vendor_micros: number;
  /** 平台结算币种金额（整数微元）；记账时已统一到结算币种，任何维度都可相加。 */
  cost_platform_micros: number;
  /** 该聚合行跨越了多个计价币种：模型币种金额不可跨模型求和。 */
  mixed_currency?: boolean;
}

/**
 * UsageSummary 是 /admin/usage/summary 的响应信封：顶层合计即当前筛选行的
 * cost_platform_micros 之和，币种取平台的全局结算币种。
 */
export interface UsageSummary {
  group_by: GroupBy;
  rows: UsageSummaryRow[];
  total_cost_platform_micros: number;
  platform_currency: string;
}

export interface UsageLogRow {
  id: ID;
  api_key_id: ID;
  user_id: ID;
  group_id: ID;
  ai_model_id: ID;
  model: string;
  provider: string;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  prompt_cache_read_tokens: number;
  prompt_cache_write_tokens: number;
  status_code: number;
  latency: number;
  /** 结算币种，与 cost_platform_micros 同币种；不适用于模型币种的 cost_vendor_micros。 */
  currency: string;
  /** 记账时的模型币种金额（整数微元），币种见模型的 currency。 */
  cost_vendor_micros: number;
  /** 记账时的平台结算币种金额（整数微元），币种见 currency。 */
  cost_platform_micros: number;
  created_at: string;
}

export type GroupBy = "date" | "model" | "key" | "user" | "group";

export interface UsageFilter {
  start?: string;
  end?: string;
  user_id?: number;
  group_id?: number;
  api_key_id?: number;
  ai_model_id?: number;
}

export interface ConnectivityResult {
  ok: boolean;
  latency_ms?: number;
  /** 上游真实返回的助手文本（探测要求模型回一个 hello）。 */
  reply?: string;
  /** 上游自报的请求 id 与模型名，便于和上游日志对齐。 */
  upstream_id?: string;
  upstream_model?: string;
  error_kind?: string;
  message?: string;
}

export interface ImportDetail {
  kind: string;
  ref: string;
  action: string;
  message?: string;
}

export interface ImportResult {
  dry_run: boolean;
  summary: { created: number; skipped: number; errors: number };
  details: ImportDetail[];
}

export interface QuotaRule {
  mode: "day" | "month" | "year" | "custom";
  limit: number;
  start?: number;
  end?: number;
  refresh?: "daily" | "monthly" | "yearly" | "none";
}

/** 网关构建信息，对齐 GET /version（公开端点，无需鉴权）。 */
export interface BuildInfo {
  version: string;
  commit: string;
  build_time: string;
  dirty: boolean;
  go_version: string;
  started_at: string;
}

/** 路由作用域：越具体越优先（key > user > group > global）。 */
export type RouterScope = "global" | "group" | "user" | "key";

/** 一条模型路由规则。config 是策略私有的 JSON 字符串，形状由策略决定。 */
export interface ModelRouter {
  id: ID;
  name: string;
  description: string;
  strategy: string;
  config: string;
  scope: RouterScope;
  scope_id: number;
  priority: number;
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

export interface ModelRouterInput {
  name: string;
  description?: string;
  strategy: string;
  config: string;
  scope: RouterScope;
  scope_id: number;
  priority: number;
  enabled: boolean;
}

/** 策略配置字段；type 决定控件类型（string/int/bool/enum/model）。 */
export interface RouterStrategyField {
  key: string;
  type: "string" | "int" | "bool" | "enum" | "model";
  title: string;
  description?: string;
  default?: unknown;
  enum?: string[];
  min?: number;
  max?: number;
  required?: boolean;
}

export interface RouterStrategy {
  name: string;
  title: string;
  description?: string;
  fields: RouterStrategyField[];
}

export interface RouterPreview {
  router_id: ID;
  strategy: string;
  ai_model_id: ID;
  model_id: string;
  model_name?: string;
  cause: string;
  detail?: Record<string, unknown>;
  /** true 表示策略未能决定，预览的是「会回退到 primary」这一结果。 */
  fallback: boolean;
}
