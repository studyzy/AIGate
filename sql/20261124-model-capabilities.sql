-- ---------------------------------------------------------------------------
-- 模型能力字段：上下文窗口、输出上限、图片支持、effort 等级
--
-- 升级脚本：仅当数据库是 20261124 之前建的实例时才需要执行一次；
-- 全新安装直接导入 sql/init.sql 即可，不要跑本文件（会因列已存在而报错）。
-- ---------------------------------------------------------------------------

ALTER TABLE `ai_models`
    ADD COLUMN `context_window`    BIGINT       NOT NULL DEFAULT 0  AFTER `use_upstream_cost`,
    ADD COLUMN `max_output_tokens` BIGINT       NOT NULL DEFAULT 0  AFTER `context_window`,
    ADD COLUMN `supports_image`    TINYINT(1)   NOT NULL DEFAULT 0  AFTER `max_output_tokens`,
    ADD COLUMN `effort_levels`     VARCHAR(128) NOT NULL DEFAULT '' AFTER `supports_image`;