-- 014：提成模块（v0.20）
-- 口径（与用户确认）：规则按 大类×结算方式 配置，三种算法三选一；
-- 一张单1~3个售货员整单平分；店长从本店店员的提成里抽成（店员到手=份额×(1-抽成%)），
-- 店长自己卖货按普通规则全额拿；销退时自动按原单冲减。

-- 1) 售货员档案补齐：门店归属 / 角色 / 店长抽成比例
ALTER TABLE salesperson ADD COLUMN IF NOT EXISTS distributor_id BIGINT REFERENCES distributor(id); -- NULL=总部
ALTER TABLE salesperson ADD COLUMN IF NOT EXISTS role VARCHAR(10) NOT NULL DEFAULT '店员';          -- 店员/店长
ALTER TABLE salesperson ADD COLUMN IF NOT EXISTS manager_rate NUMERIC(5,2) NOT NULL DEFAULT 0;      -- 店长抽成%

-- 2) 提成规则：每个 大类×结算方式 一条
CREATE TABLE IF NOT EXISTS commission_rule (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  category   VARCHAR(50) NOT NULL,
  mode       VARCHAR(10) NOT NULL,              -- 标签价 / 变金价
  calc_type  VARCHAR(20) NOT NULL,              -- 销售额百分比 / 每克固定 / 每件固定
  value      NUMERIC(12,4) NOT NULL,
  enabled    BOOLEAN NOT NULL DEFAULT true,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (category, mode)
);

-- 3) 提成流水：只追加的台账（报表从这里汇总）
--   kind: 销售 / 店长抽成 / 销退冲减（冲减行金额为负）
CREATE TABLE IF NOT EXISTS commission_flow (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  doc_id         BIGINT NOT NULL REFERENCES doc(id),
  barcode        VARCHAR(32) NOT NULL,
  salesperson_id BIGINT NOT NULL REFERENCES salesperson(id),
  kind           VARCHAR(10) NOT NULL,
  amount         NUMERIC(12,2) NOT NULL,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_cf_sp ON commission_flow (salesperson_id, created_at);
CREATE INDEX IF NOT EXISTS idx_cf_doc ON commission_flow (doc_id);

-- 4) 销售单多售货员：JSONB数组；存量单据从旧的单售货员列回填
ALTER TABLE doc ADD COLUMN IF NOT EXISTS salesperson_ids JSONB;
UPDATE doc SET salesperson_ids = jsonb_build_array(salesperson_id)
  WHERE salesperson_id IS NOT NULL AND salesperson_ids IS NULL;
