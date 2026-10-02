-- 015：提成规则版本化（v0.22，用户提议）
-- 每条规则带生效区间 [valid_from, valid_to]（valid_to 空=一直有效）。
-- "调整规则"=新增版本（旧版本自动在新版本生效前一天关闭），历史版本不可修改——
-- 与金价、提成台账同一哲学：只追加，不涂改。
-- 说明：历史报表本就安全（提成是确认时刻的快照），版本化带来的是"排期"与"留痕"。
ALTER TABLE commission_rule ADD COLUMN IF NOT EXISTS valid_from DATE NOT NULL DEFAULT CURRENT_DATE;
ALTER TABLE commission_rule ADD COLUMN IF NOT EXISTS valid_to DATE;

-- 存量数据：生效日回填为创建日；此前"停用"的规则视为已结束
UPDATE commission_rule SET valid_from = created_at::date;
UPDATE commission_rule SET valid_to = CURRENT_DATE - 1 WHERE enabled = false;

-- 同一维度允许多版本并存（按区间区分），原唯一约束退役
ALTER TABLE commission_rule DROP CONSTRAINT IF EXISTS commission_rule_category_mode_key;
CREATE INDEX IF NOT EXISTS idx_cr_lookup ON commission_rule (category, mode, valid_from DESC);
