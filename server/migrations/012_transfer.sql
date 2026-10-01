-- 012：分销商模块——统一调拨单（v0.18）
-- 设计要点：
--   1. 货品"位置"= location_type('总库'/'分销商') + distributor_id——001迁移早已备好，
--      位置与状态(在库/已售)是两个正交维度：在库的货可能在总库，也可能在某分销商处；
--   2. 一种单据三种用法：调拨单(from→to)覆盖 分货(总库→分销商)、退货(分销商→总库)、
--      互调(分销商A→B)。from/to 为 NULL 表示总库，否则指向分销商（按id引用，改名安全）；
--   3. 调拨草稿占用状态 = '调拨中'（延续第16章命名约定）。
ALTER TABLE doc ADD COLUMN IF NOT EXISTS from_distributor_id BIGINT REFERENCES distributor(id);
ALTER TABLE doc ADD COLUMN IF NOT EXISTS to_distributor_id BIGINT REFERENCES distributor(id);
