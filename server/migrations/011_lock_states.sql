-- 011：分单据类型的占用状态（v0.16，用户提议）
-- 设计定稿：货品进入任何单据草稿即被占用，占用状态按单据类型命名——
--   销售草稿占用 = '销售中'，退库草稿占用 = '退库中'（以后盘点、维修同理扩展）。
-- 比统一叫"锁定"多一层信息：一眼看出货在哪类单里。
-- 本迁移只做存量数据转换（状态集合活在代码里，无表结构变化）：
--   1) v0.15 用过"锁定"的货品改叫"销售中"；
--   2) 历史流水里的"锁定"同步改名，保持审计链语义一致。
UPDATE item SET status='销售中' WHERE status='锁定';
UPDATE stock_flow SET from_status='销售中' WHERE from_status='锁定';
UPDATE stock_flow SET to_status='销售中' WHERE to_status='锁定';
