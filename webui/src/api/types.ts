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
