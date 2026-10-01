-- 006：撤销"大类绑定销售模式"的设计——同一大类下既有标签价也有变金价的货，
-- 结算方式改为销售开单时逐件选择（标签价读售价；变金价=克重×当日金价/银价）。
ALTER TABLE dict_item DROP COLUMN IF EXISTS price_mode;
