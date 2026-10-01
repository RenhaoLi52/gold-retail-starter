-- 002：单据支持草稿——草稿阶段的明细行以 JSONB 存在单据头上，
-- 确认时才生成真正的货品件(item)；反确认时把件删除、明细退回草稿(保留条码)。
ALTER TABLE doc ADD COLUMN IF NOT EXISTS draft_lines JSONB;
