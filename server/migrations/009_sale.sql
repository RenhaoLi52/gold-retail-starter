-- 009：销售单前置——单据头增加 合计金额 与 金价快照。
-- gold_price_snapshot 在销售确认那一刻写入：当时用到的各成色金价 + 每行成交参数。
-- 此后金价怎么变，这张单的账永远可复算（审计口径的根）。
ALTER TABLE doc ADD COLUMN IF NOT EXISTS total_amount NUMERIC(14,2) NOT NULL DEFAULT 0;
ALTER TABLE doc ADD COLUMN IF NOT EXISTS gold_price_snapshot JSONB;
