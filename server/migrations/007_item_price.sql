-- 007：货品增加售价（标签价）——计价设计修订的落地第一步：
-- 入库时录入克重和售价；销售开单时逐件选结算方式（标签价读售价，变金价按克重×金价）。
-- 售价允许为0（纯按克的货可以不定标签价），负数由应用层拒绝。
ALTER TABLE item ADD COLUMN IF NOT EXISTS price NUMERIC(12,2) NOT NULL DEFAULT 0;
