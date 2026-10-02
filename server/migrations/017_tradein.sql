-- 017：以旧换新 / 旧料回收（v0.24）
-- 口径（与用户确认）：
--   1. 换新价随金价每日发布（零售价/回收价之外的第三个价），确认时快照；
--   2. 金换金、银换银：换新额度按大类分开算，上限=本单该大类新品克重；
--      旧料超出额度的部分按回收价；抵扣超过货款则倒付顾客；
--   3. 提成规则加"业务类型"维度：正常销售 / 以旧换新 / 旧料回收。
--      以旧换新允许负值——它是"折扣"不是"倒扣"：正常卖金每克提成12、换新部分配-6，
--      净得每克6。业务上折扣绝对值不超过正常规则，单据不会翻负；账上允许负数，不垫底。
ALTER TABLE gold_price ADD COLUMN IF NOT EXISTS trade_price NUMERIC(12,2) NOT NULL DEFAULT 0;
ALTER TABLE doc ADD COLUMN IF NOT EXISTS old_lines JSONB;
ALTER TABLE commission_rule ADD COLUMN IF NOT EXISTS biz_type VARCHAR(10) NOT NULL DEFAULT '正常销售';
CREATE INDEX IF NOT EXISTS idx_cr_biz ON commission_rule (category, biz_type, mode, valid_from DESC);
