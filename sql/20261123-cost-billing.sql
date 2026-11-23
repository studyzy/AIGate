-- Tencent Cloud AIGate 增量 DDL：按金额计费（cost billing）。
-- 仅用于【升级已存在的数据库】，全新安装请只导入 sql/init.sql（它已包含全部列与新表）。
--
-- 执行方式：mysql -u... -p dbname < sql/20261123-cost-billing.sql
-- 幂等性：MySQL 8 不支持 ADD COLUMN IF NOT EXISTS，本文件**不可重复执行**，
--         重复执行会以 "Duplicate column name" 失败——这是刻意的，避免静默跳过漏建。
-- 本文件包含两部分：
--   1) 三张新表的 CREATE TABLE IF NOT EXISTS（与 init.sql 逐字一致，可安全重复执行）；
--   2) 既有表的 ADD COLUMN（不可重复执行）。
-- 全新安装只导 init.sql 即可，无需执行本文件。
--
-- ---------------------------------------------------------------------------
-- 1) 新增表：按金额计费的三个快照表
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `ai_model_prices` (
    `id`                      BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `ai_model_id`             BIGINT UNSIGNED NOT NULL DEFAULT 0,
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

CREATE TABLE IF NOT EXISTS `billing_calendar` (
    `date`       DATE        NOT NULL,
    `is_workday` TINYINT(1)  NOT NULL DEFAULT 1,
    `note`       VARCHAR(128) NOT NULL DEFAULT '',
    `updated_at` DATETIME(3) NOT NULL,
    PRIMARY KEY (`date`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

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
-- 2) 既有表加列（本段不可重复执行）
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
