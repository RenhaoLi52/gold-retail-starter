-- 005：基础资料字典——三个字典（首饰大类/成色/首饰类别）共用一张表
-- 设计要点：
--   1. dict_type 区分字典种类，同一套接口和界面服务所有字典（复用思想）；
--   2. price_mode 仅大类使用（变金价/标签价，对应设计文档3.2的双销售模式）；
--   3. 字典只停用不删除——历史单据和货品引用着这些名字，删除会让历史数据失去解释。
CREATE TABLE IF NOT EXISTS dict_item (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  dict_type  VARCHAR(20) NOT NULL,          -- category(大类) / purity(成色) / jewel_type(类别)
  name       VARCHAR(50) NOT NULL,
  price_mode VARCHAR(10) NULL,              -- 仅大类: 变金价 / 标签价
  sort       INT NOT NULL DEFAULT 0,
  enabled    BOOLEAN NOT NULL DEFAULT true,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (dict_type, name)
);

-- 种子数据（已存在则跳过）
INSERT INTO dict_item (dict_type, name, price_mode, sort) VALUES
  ('category', '黄金',      '变金价', 1),
  ('category', '万足银(克)', '变金价', 2),
  ('category', '玉器类',    '标签价', 3),
  ('category', '钻石类',    '标签价', 4),
  ('category', '3D金',      '标签价', 5)
ON CONFLICT (dict_type, name) DO NOTHING;

INSERT INTO dict_item (dict_type, name, sort) VALUES
  ('purity', '足金999.9', 1),
  ('purity', '足金999',   2),
  ('purity', 'Au750',     3),
  ('purity', 'Pt950',     4),
  ('purity', 'S925银',    5),
  ('purity', 'A货',       6)
ON CONFLICT (dict_type, name) DO NOTHING;

INSERT INTO dict_item (dict_type, name, sort) VALUES
  ('jewel_type', '手镯', 1),
  ('jewel_type', '吊坠', 2),
  ('jewel_type', '戒指', 3),
  ('jewel_type', '项链', 4),
  ('jewel_type', '耳饰', 5),
  ('jewel_type', '挂坠', 6)
ON CONFLICT (dict_type, name) DO NOTHING;
