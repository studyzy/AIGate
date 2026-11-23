-- LLM Gateway schema (full table creation for the MySQL production environment)
-- Convention: one-to-one with the GORM models; keep the two in sync (see internal/model/*.go).
-- Migration strategy: this file is the single source of truth for a fresh install; apply schema changes by editing it directly.
-- If an already-deployed instance needs an in-place upgrade, add an incremental sql/YYYYMMDD-<slug>.sql (year-first date prefix, e.g. sql/20261002-add-foo.sql).
-- Note: the SQLite local-development path creates tables via GORM AutoMigrate and does not use this file.

CREATE TABLE IF NOT EXISTS `users` (
    `id`           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `username`         VARCHAR(128)    NOT NULL DEFAULT '',
    `role`             VARCHAR(16)     NOT NULL DEFAULT 'user',
    `group_id`         BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `api_token_hash`   VARCHAR(64)     NOT NULL DEFAULT '',
    `api_token_prefix` VARCHAR(32)     NOT NULL DEFAULT '',
    `enabled`          TINYINT(1)      NOT NULL DEFAULT 1,
    `created_at`       DATETIME(3)     NOT NULL,
    `updated_at`       DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_users_api_token_hash` (`api_token_hash`),
    KEY `idx_users_group_id` (`group_id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `ai_models` (
    `id`                  BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `provider`            VARCHAR(64)     NOT NULL DEFAULT '',
    `model_id`            VARCHAR(191)    NOT NULL DEFAULT '',
    `model_name`          VARCHAR(191)    NOT NULL DEFAULT '',
    `url`                 VARCHAR(512)    NOT NULL DEFAULT '',
    `api_key`             VARCHAR(1024)   NOT NULL DEFAULT '',
    `credential_id`       BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `model_type`          VARCHAR(32)     NOT NULL DEFAULT 'openai-completions',
    `billing_mode`        VARCHAR(32)     NOT NULL DEFAULT 'per_token',
    `custom_http_headers` TEXT            NOT NULL,
    `quota_day`           BIGINT          NOT NULL DEFAULT -1,
    `enabled`             TINYINT(1)      NOT NULL DEFAULT 1,
    -- user_key_required: BYOK mode; no shared key, the per-binding key is used (immutable after creation)
    `user_key_required`   TINYINT(1)      NOT NULL DEFAULT 0,
    -- currency: model pricing currency (ISO 4217); '' = not configured, no monetary accounting
    `currency`            VARCHAR(3)      NOT NULL DEFAULT '',
    -- enable_peak_pricing: switch peak/off-peak pricing on (peak_hours lives in ai_model_prices)
    `enable_peak_pricing` TINYINT(1)      NOT NULL DEFAULT 0,
    -- use_upstream_cost: trust usage.cost reported by the upstream when > 0
    `use_upstream_cost`   TINYINT(1)      NOT NULL DEFAULT 1,
    `created_at`          DATETIME(3)     NOT NULL,
    `updated_at`          DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_ai_models_credential_id` (`credential_id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `api_keys` (
    `id`           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `name`         VARCHAR(128)    NOT NULL DEFAULT '',
    `user_id`     BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `group_id`     BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `proxy_token_hash`   VARCHAR(64)     NOT NULL DEFAULT '',
    `proxy_token_prefix` VARCHAR(32)     NOT NULL DEFAULT '',
    `max_tokens`   BIGINT          NOT NULL DEFAULT 0,
    -- token_quota_rules: per-key period quota JSON ('' = unlimited); checked as the 4th quota level
    `token_quota_rules` TEXT       NOT NULL,
    `enabled`      TINYINT(1)      NOT NULL DEFAULT 1,
    `expires_at`   DATETIME(3)     NULL DEFAULT NULL,
    `expired_at`   DATETIME(3)     NULL DEFAULT NULL,
    `created_at`   DATETIME(3)     NOT NULL,
    `updated_at`   DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_api_keys_proxy_token_hash` (`proxy_token_hash`),
    KEY `idx_api_keys_user_id` (`user_id`),
    KEY `idx_api_keys_group_id` (`group_id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `api_key_models` (
    `id`          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `api_key_id`  BIGINT UNSIGNED NOT NULL,
    `ai_model_id` BIGINT UNSIGNED NOT NULL,
    `role`        VARCHAR(16)     NOT NULL DEFAULT 'fallback',
    `sort_order`  BIGINT          NOT NULL DEFAULT 0,
    -- api_key: per-binding upstream key (BYOK models only); sealed (enc:v1:) when a master key is set
    `api_key`     VARCHAR(1024)   NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_api_key_model` (`api_key_id`, `ai_model_id`),
    KEY `idx_api_key_models_ai_model_id` (`ai_model_id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `llm_usage_logs` (
    `id`                        BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `api_key_id`                BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `user_id`                   BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `group_id`                  BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `ai_model_id`               BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `model`                     VARCHAR(191)    NOT NULL DEFAULT '',
    `provider`                  VARCHAR(64)     NOT NULL DEFAULT '',
    `prompt_tokens`             BIGINT          NOT NULL DEFAULT 0,
    `completion_tokens`         BIGINT          NOT NULL DEFAULT 0,
    `total_tokens`              BIGINT          NOT NULL DEFAULT 0,
    `prompt_cache_read_tokens`  BIGINT          NOT NULL DEFAULT 0,
    `prompt_cache_write_tokens` BIGINT          NOT NULL DEFAULT 0,
    `status_code`               BIGINT          NOT NULL DEFAULT 0,
    `latency`                   BIGINT          NOT NULL DEFAULT 0,
    -- Two monetary columns only (integer micros, 1 unit = 1e6 micros); no per-tier breakdown.
    -- cost_vendor_micros: amount in the model currency; the currency is ai_models.currency.
    -- cost_platform_micros: amount in settings.platform_currency, fixed at accounting time.
    `cost_vendor_micros`        BIGINT          NOT NULL DEFAULT 0,
    `cost_platform_micros`      BIGINT          NOT NULL DEFAULT 0,
    `created_at`                DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_llm_usage_logs_api_key_id` (`api_key_id`),
    KEY `idx_llm_usage_logs_user_id` (`user_id`),
    KEY `idx_llm_usage_logs_group_id` (`group_id`),
    KEY `idx_llm_usage_logs_created_at` (`created_at`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `daily_usage_summaries` (
    `id`                        BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `date`                      DATETIME(3)     NOT NULL,
    `user_id`                   BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `api_key_id`                BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `ai_model_id`               BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `group_id`                  BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `prompt_tokens`             BIGINT          NOT NULL DEFAULT 0,
    `completion_tokens`         BIGINT          NOT NULL DEFAULT 0,
    `total_tokens`              BIGINT          NOT NULL DEFAULT 0,
    `prompt_cache_read_tokens`  BIGINT          NOT NULL DEFAULT 0,
    `prompt_cache_write_tokens` BIGINT          NOT NULL DEFAULT 0,
    `request_count`             BIGINT          NOT NULL DEFAULT 0,
    `cost_vendor_micros`        BIGINT          NOT NULL DEFAULT 0,
    `cost_platform_micros`      BIGINT          NOT NULL DEFAULT 0,
    -- currency: pricing currency of ai_model_id; set on first insert, never overwritten
    `currency`                  VARCHAR(3)      NOT NULL DEFAULT '',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_daily_usage` (`date`, `user_id`, `api_key_id`, `ai_model_id`),
    KEY `idx_daily_usage_summaries_group_id` (`group_id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `user_groups` (
    `id`                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `name`              VARCHAR(128)    NOT NULL DEFAULT '',
    `token_quota_rules` TEXT            NOT NULL,
    -- global_token_quota_rules: group-level global quota override ('' = fall back to site rules)
    `global_token_quota_rules` TEXT     NOT NULL,
    `created_at`        DATETIME(3)     NOT NULL,
    `updated_at`        DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `settings` (
    `id`                        BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `default_token_quota_rules` TEXT            NOT NULL,
    `global_token_quota_rules`  TEXT            NOT NULL,
    `global_quota_scope`        VARCHAR(16)     NOT NULL DEFAULT 'site',
    `user_key_limit`            INT             NOT NULL DEFAULT 5,
    -- platform_currency: single settlement currency for monetary quotas and reports
    `platform_currency`         VARCHAR(3)      NOT NULL DEFAULT 'CNY',
    `updated_at`                DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

-- Default settings row. Kept to the columns that exist in EVERY version of the schema, so this
-- statement also succeeds when re-applied over an existing database (upgrade path); the new
-- columns simply fall back to their column defaults.
INSERT IGNORE INTO `settings` (`id`, `default_token_quota_rules`, `global_token_quota_rules`, `global_quota_scope`, `user_key_limit`, `updated_at`)
VALUES (1, '[]', '[]', 'site', 5, NOW(3));

-- Upstream credentials: a set of secret items, referenced by ai_models.credential_id and resolved at forward time.
-- secrets_json is encrypted per the master-key policy (AES-256-GCM, ciphertext prefix enc:v1:); plaintext JSON when no master key is configured.
CREATE TABLE IF NOT EXISTS `credentials` (
    `id`           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `name`         VARCHAR(128)    NOT NULL DEFAULT '',
    `description`  VARCHAR(1024)   NOT NULL DEFAULT '',
    `secrets_json` TEXT            NOT NULL,
    `enabled`      TINYINT(1)      NOT NULL DEFAULT 1,
    `created_at`   DATETIME(3)     NOT NULL,
    `updated_at`   DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

-- Webhook event subscriptions: event_types is a JSON string array ("[]" means subscribe to all); secret is write-only and never read back.
CREATE TABLE IF NOT EXISTS `webhooks` (
    `id`          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `name`        VARCHAR(128)    NOT NULL DEFAULT '',
    `url`         VARCHAR(512)    NOT NULL DEFAULT '',
    `secret`      VARCHAR(512)    NOT NULL DEFAULT '',
    `event_types` TEXT            NOT NULL,
    `enabled`     TINYINT(1)      NOT NULL DEFAULT 1,
    `created_at`  DATETIME(3)     NOT NULL,
    `updated_at`  DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

-- Webhook delivery records: each attempt updates attempts/last_status/last_error; terminal states are success/failed.
CREATE TABLE IF NOT EXISTS `webhook_deliveries` (
    `id`          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `webhook_id`  BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `event_type`  VARCHAR(64)     NOT NULL DEFAULT '',
    `payload`     TEXT            NOT NULL,
    `status`      VARCHAR(16)     NOT NULL DEFAULT 'pending',
    `attempts`    INT             NOT NULL DEFAULT 0,
    `last_status` INT             NOT NULL DEFAULT 0,
    `last_error`  VARCHAR(512)    NOT NULL DEFAULT '',
    `created_at`  DATETIME(3)     NOT NULL,
    `updated_at`  DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_webhook_deliveries_webhook_id` (`webhook_id`),
    KEY `idx_webhook_deliveries_created_at` (`created_at`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

-- Responses API session state: items is the accumulated conversation snapshot (complete input + output items),
-- response_body is the full Responses JSON returned by GET /v1/responses/{id}. Rows are pruned by created_at.
CREATE TABLE IF NOT EXISTS `response_states` (
    `id`                   VARCHAR(64)     NOT NULL,
    `conversation_id`      VARCHAR(64)     NOT NULL DEFAULT '',
    `owner_user_id`        BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `owner_api_key_id`     BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `request_model`        VARCHAR(191)    NOT NULL DEFAULT '',
    `previous_response_id` VARCHAR(64)     NOT NULL DEFAULT '',
    `status`               VARCHAR(16)     NOT NULL DEFAULT 'queued',
    `items`                LONGTEXT        NOT NULL,
    `response_body`        LONGTEXT        NOT NULL,
    `input_tokens`         INT             NOT NULL DEFAULT 0,
    `output_tokens`        INT             NOT NULL DEFAULT 0,
    `total_tokens`         INT             NOT NULL DEFAULT 0,
    `reasoning_tokens`     INT             NOT NULL DEFAULT 0,
    `error`                TEXT            NOT NULL,
    `created_at`           DATETIME(3)     NOT NULL,
    `updated_at`           DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_response_states_created_at` (`created_at`),
    KEY `idx_response_states_owner` (`owner_user_id`, `owner_api_key_id`),
    KEY `idx_response_states_conversation` (`conversation_id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

-- Model price snapshots (cost billing). Each row is one immutable version: six raw prices
-- (peak/off-peak x input-cache-hit / input-cache-miss / output) in integer micros per million
-- tokens, plus the peak-hour schedule. A new price is appended with a new effective_at; rows
-- are never updated in place, so already-recorded usage keeps its historical cost.
-- peak_hours is JSON: {"timezone":"Asia/Shanghai","windows":[{"days":[1,2,3,4,5],"start":"09:00","end":"12:00"}]}
-- (days use Go time.Weekday, 0=Sunday; "{}" means no schedule).
CREATE TABLE IF NOT EXISTS `ai_model_prices` (
    `id`                      BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `ai_model_id`             BIGINT UNSIGNED NOT NULL DEFAULT 0,
    -- currency: snapshot of the model currency at insert time, so later model edits do not rewrite history
    `currency`                VARCHAR(3)      NOT NULL DEFAULT '',
    `effective_at`            DATETIME(3)     NOT NULL,
    `peak_hours`              TEXT            NOT NULL,
    `peak_input_hit_micros`   BIGINT          NOT NULL DEFAULT 0,
    `peak_input_miss_micros`  BIGINT          NOT NULL DEFAULT 0,
    `peak_output_micros`      BIGINT          NOT NULL DEFAULT 0,
    `offpeak_input_hit_micros`  BIGINT        NOT NULL DEFAULT 0,
    `offpeak_input_miss_micros` BIGINT        NOT NULL DEFAULT 0,
    `offpeak_output_micros`   BIGINT          NOT NULL DEFAULT 0,
    `created_at`              DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_ai_model_prices_effective` (`ai_model_id`, `effective_at`),
    KEY `idx_ai_model_prices_ai_model_id` (`ai_model_id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

-- Billing calendar: workday overrides that correct the weekday matching of peak_hours
-- (public holidays = 0, make-up working days = 1). Unlisted dates follow the weekday rule.
-- date is a business-timezone date label, the same convention as daily_usage_summaries.date.
CREATE TABLE IF NOT EXISTS `billing_calendar` (
    `date`       DATE        NOT NULL,
    `is_workday` TINYINT(1)  NOT NULL DEFAULT 1,
    `note`       VARCHAR(128) NOT NULL DEFAULT '',
    `updated_at` DATETIME(3) NOT NULL,
    PRIMARY KEY (`date`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

-- Currency exchange rates: 1 unit of from_currency = rate_micros/1e6 units of to_currency.
-- Append-only like prices; the rate effective at accounting time is used, so history never moves.
CREATE TABLE IF NOT EXISTS `currency_rates` (
    `id`            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `from_currency` VARCHAR(3)      NOT NULL,
    `to_currency`   VARCHAR(3)      NOT NULL,
    `effective_at`  DATETIME(3)     NOT NULL,
    `rate_micros`   BIGINT          NOT NULL,
    `created_at`    DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_currency_rates_effective` (`from_currency`, `to_currency`, `effective_at`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- Upgrade only: RUN ONCE when upgrading an EXISTING database (do not re-run; MySQL 8 has no
-- ADD COLUMN IF NOT EXISTS, so a second run fails with "Duplicate column name").
-- A fresh install must NOT execute this block: the CREATE TABLE statements above already carry
-- every column, so these ALTERs would fail there with the same duplicate-column error.
-- The three CREATE TABLE IF NOT EXISTS statements above run cleanly on both paths.
-- ---------------------------------------------------------------------------
ALTER TABLE `ai_models`
    ADD COLUMN `currency`             VARCHAR(3) NOT NULL DEFAULT '' AFTER `user_key_required`,
    ADD COLUMN `enable_peak_pricing`  TINYINT(1) NOT NULL DEFAULT 0   AFTER `currency`,
    ADD COLUMN `use_upstream_cost`    TINYINT(1) NOT NULL DEFAULT 1   AFTER `enable_peak_pricing`;

ALTER TABLE `llm_usage_logs`
    ADD COLUMN `cost_vendor_micros`   BIGINT NOT NULL DEFAULT 0 AFTER `latency`,
    ADD COLUMN `cost_platform_micros` BIGINT NOT NULL DEFAULT 0 AFTER `cost_vendor_micros`;

ALTER TABLE `daily_usage_summaries`
    ADD COLUMN `cost_vendor_micros`   BIGINT     NOT NULL DEFAULT 0 AFTER `request_count`,
    ADD COLUMN `cost_platform_micros` BIGINT     NOT NULL DEFAULT 0 AFTER `cost_vendor_micros`,
    ADD COLUMN `currency`             VARCHAR(3) NOT NULL DEFAULT '' AFTER `cost_platform_micros`;

ALTER TABLE `settings`
    ADD COLUMN `platform_currency` VARCHAR(3) NOT NULL DEFAULT 'CNY' AFTER `user_key_limit`;

-- Existing settings row was created before the column existed: backfill the settlement currency.
UPDATE `settings` SET `platform_currency` = 'CNY' WHERE `platform_currency` = '';
