-- 018：计价模型升级（v0.27）
-- 依据 JMP 真实数据验证的公式：按克货 实售价 = 克重 ×（金价 + 每克销售工费），
-- 工费也可按件（+固定额）。样例 BQ06552：2.46×(1169+78)=3067.62→3068（四舍五入到元）。
-- 销售工费复用 001 迁移埋好的 labor_fee / labor_fee_mode 两列（当年的伏笔今天兑现）；
-- 本迁移补进货成本侧三列（成本核算与毛利报表的根）。
-- 存量货品：成本与工费=0，变金价建议价退化为纯 克重×金价，入库补录或将来"修改单"再补。
ALTER TABLE item ADD COLUMN IF NOT EXISTS cost_gold_price NUMERIC(12,2) NOT NULL DEFAULT 0; -- 进货金价(元/克)
ALTER TABLE item ADD COLUMN IF NOT EXISTS cost_fee_mode VARCHAR(10) NOT NULL DEFAULT '按克'; -- 进货工费方式
ALTER TABLE item ADD COLUMN IF NOT EXISTS cost_fee NUMERIC(12,2) NOT NULL DEFAULT 0;         -- 进货工费
