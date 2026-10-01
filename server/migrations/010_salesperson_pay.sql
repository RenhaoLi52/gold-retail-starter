-- 010：售货员档案 + 收款方式字典 + 销售单记售货员与组合收款（v0.14）
-- 设计要点：
--   1. 售货员是独立档案表，不塞进 dict_item——以后要挂提成比例、工号、所属门店等字段，
--      字典那张"名称+排序"的窄表装不下；
--   2. 单据按 id 引用售货员（不是按名字）——人改名、同名新人入职都不会污染历史单据，
--      这点与字典"按名字引用所以禁止改名"刚好相反，所以售货员允许改名；
--   3. 收款方式是纯名称列表，走字典正合适（以后加"美团""抖音"自己在基础资料里添）；
--   4. payments 存 JSONB：[{"method":"现金","amount":5000},...]，确认时校验合计=应收。
CREATE TABLE IF NOT EXISTS salesperson (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name       VARCHAR(50) NOT NULL UNIQUE,
  sort       INT NOT NULL DEFAULT 0,
  enabled    BOOLEAN NOT NULL DEFAULT true,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE doc ADD COLUMN IF NOT EXISTS salesperson_id BIGINT REFERENCES salesperson(id);
ALTER TABLE doc ADD COLUMN IF NOT EXISTS payments JSONB;

INSERT INTO dict_item (dict_type, name, sort) VALUES
  ('pay_method', '现金',   1),
  ('pay_method', '微信',   2),
  ('pay_method', '支付宝', 3),
  ('pay_method', '银行卡', 4)
ON CONFLICT (dict_type, name) DO NOTHING;
