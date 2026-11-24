-- Tencent Cloud AIGate 增量 DDL：模型路由（含 auto 伪模型路由）。
-- 仅用于【升级已存在的数据库】，全新安装请只导入 sql/init.sql（它已包含全部列）。
--
-- 执行方式：mysql -u... -p dbname < sql/20261124-auto-routing.sql
-- 幂等性：MySQL 8 不支持 ADD COLUMN IF NOT EXISTS，本文件**不可重复执行**，
--         重复执行会以 "Duplicate column name" 失败——这是刻意的，避免静默跳过漏建。
--
-- 已在 MySQL 8.0 上实测：全新安装（init.sql）与升级（本文件）两条路径均通过，
-- 第 5 段的数据迁移可单独重跑而不会产生重复行。
--
-- 本文件包含五部分：
--   1) llm_usage_logs 新增 count_toward_quota：区分「计入配额的用量」与「网关内部开销」
--      （如路由的决策调用）。默认值为 1，存量行自动按「计入配额」处理，语义正确。
--   2) settings 新增 auto_router_* 系列列（历史形态，见下）。
--      默认关闭（auto_router_enabled = 0），升级后行为与升级前完全一致。
--      注意：数据面**不再读取**这些列——路由配置改由 model_routers 承载（见第 3 部分）。
--      它们只作为第 5 段数据迁移的来源保留一个版本，下个版本可删除。
--   3) 新建 model_routers 表；4) llm_usage_logs 新增 router_id；5) 旧配置迁移成一条路由。
--
-- 注意：count_toward_quota 参与配额查询的过滤条件（SumUserTokensInWindow 等），
--       这是一次会触达全部流量读路径的 schema 变更。建议在低峰执行。
--       TINYINT(1) 上无需为该列单独建索引：它始终与 created_at / user_id 等
--       高选择性条件一起使用，现有索引已足够。

-- ---------------------------------------------------------------------------
-- 1) 用量明细：区分是否计入配额
-- ---------------------------------------------------------------------------
ALTER TABLE `llm_usage_logs`
    ADD COLUMN `count_toward_quota` TINYINT(1) NOT NULL DEFAULT 1
        COMMENT '1=计入用户/密钥/全局配额；0=网关内部开销（记成本、不占配额）';

-- ---------------------------------------------------------------------------
-- 2) 全局设置：auto 路由开关与阈值
-- ---------------------------------------------------------------------------
ALTER TABLE `settings`
    ADD COLUMN `auto_router_enabled`            TINYINT(1)      NOT NULL DEFAULT 0,
    ADD COLUMN `auto_router_model_id`           BIGINT UNSIGNED NOT NULL DEFAULT 0,
    ADD COLUMN `auto_router_max_candidates`     INT             NOT NULL DEFAULT 8,
    ADD COLUMN `auto_router_max_short_tokens`   INT             NOT NULL DEFAULT 200,
    ADD COLUMN `auto_router_max_context_tokens` INT             NOT NULL DEFAULT 8000,
    ADD COLUMN `auto_router_max_chars`          INT             NOT NULL DEFAULT 8000,
    ADD COLUMN `auto_router_timeout_ms`         INT             NOT NULL DEFAULT 2000,
    ADD COLUMN `auto_router_cooldown_seconds`   INT             NOT NULL DEFAULT 30;
-- ---------------------------------------------------------------------------
-- 3) 模型路由表（取代第 2 部分的 settings 列；列保留一个版本以便回滚）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `model_routers` (
    `id`          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `name`        VARCHAR(128)    NOT NULL DEFAULT '',
    `description` VARCHAR(512)    NOT NULL DEFAULT '',
    `strategy`    VARCHAR(32)     NOT NULL DEFAULT '',
    `config`      TEXT            NOT NULL,
    `scope`       VARCHAR(16)     NOT NULL DEFAULT 'global',
    `scope_id`    BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `priority`    INT             NOT NULL DEFAULT 0,
    `enabled`     TINYINT(1)      NOT NULL DEFAULT 1,
    `created_at`  DATETIME(3)     NOT NULL,
    `updated_at`  DATETIME(3)     NOT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_model_routers_scope_id` (`scope_id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

-- 4) 用量明细记录命中的路由规则（0 = 未经路由）
ALTER TABLE `llm_usage_logs`
    ADD COLUMN `router_id` BIGINT UNSIGNED NOT NULL DEFAULT 0,
    ADD KEY `idx_llm_usage_logs_router_id` (`router_id`);

-- 5) 数据迁移：把旧的 settings 单例配置转成一条全局 decision 路由。
--    仅在旧配置确为启用时生成，且只在尚未存在同名路由时生成（可安全重跑本段）。
INSERT INTO `model_routers` (`name`, `description`, `strategy`, `config`, `scope`, `scope_id`, `priority`, `enabled`, `created_at`, `updated_at`)
SELECT
    'auto', 'Migrated from settings.auto_router_*',
    'decision',
    JSON_OBJECT(
        'decision_model_id',     `auto_router_model_id`,
        'max_candidates',        `auto_router_max_candidates`,
        'max_short_tokens',      `auto_router_max_short_tokens`,
        'max_context_tokens',    `auto_router_max_context_tokens`,
        'max_chars',             `auto_router_max_chars`,
        'timeout_ms',            `auto_router_timeout_ms`,
        'cooldown_seconds',      `auto_router_cooldown_seconds`,
        'short_circuit_enabled', true
    ),
    'global', 0, 0, `auto_router_enabled`, NOW(3), NOW(3)
FROM `settings`
WHERE `id` = 1
  AND `auto_router_enabled` = 1
  AND `auto_router_model_id` > 0
  AND NOT EXISTS (SELECT 1 FROM `model_routers` WHERE `name` = 'auto');
