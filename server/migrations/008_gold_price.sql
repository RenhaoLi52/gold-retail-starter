-- 008：金价发布——按成色发布 零售价/回收价。
-- 核心设计：价格记录**只追加、永不修改**（append-only）：
--   当前价 = 每个成色最新的一条；历史永远可查；
--   将来销售单确认时把当时的价格快照进单据，历史单据金额永远可复算。
CREATE TABLE IF NOT EXISTS gold_price (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  purity        VARCHAR(50) NOT NULL,            -- 成色名（引用 purity 字典）
  retail_price  NUMERIC(10,2) NOT NULL,          -- 零售金价(元/克)
  recycle_price NUMERIC(10,2) NOT NULL DEFAULT 0,-- 回收金价(元/克)，0=未定
  published_by  BIGINT NOT NULL,                 -- 发布人
  published_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_gold_price_purity ON gold_price (purity, id DESC);
