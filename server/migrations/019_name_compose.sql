-- 019：首饰名称自动拼接（v0.28，用户口径）
-- 名称不再手填，入库时由 成色+主石名称+首饰类别 三段拼接而成；
-- 三个字段既可下拉选字典项、也可直接填写——填了新值自动补进字典（下次就在下拉里）。
ALTER TABLE item ADD COLUMN IF NOT EXISTS jewel_type VARCHAR(50) NOT NULL DEFAULT ''; -- 首饰类别(戒指/手镯…)
ALTER TABLE item ADD COLUMN IF NOT EXISTS stone_name VARCHAR(50) NOT NULL DEFAULT ''; -- 主石名称(可空=素金)

-- 主石名称字典（第五本字典）
INSERT INTO dict_item (dict_type, name, sort) VALUES
  ('stone_name', '和田玉', 1),
  ('stone_name', '翡翠',   2),
  ('stone_name', '南红',   3),
  ('stone_name', '珍珠',   4),
  ('stone_name', '钻石',   5)
ON CONFLICT (dict_type, name) DO NOTHING;
