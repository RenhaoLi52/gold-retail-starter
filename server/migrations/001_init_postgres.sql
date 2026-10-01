-- 第一版建表脚本（PostgreSQL 版，替换原 MySQL 版 001_init.sql）
-- 教学参考：先够 M1（入库/退库）使用
-- 导入方式见《MySQL改PostgreSQL说明》

-- 用户（注意：user 在 PostgreSQL 里是保留字，表名改为 app_user）
CREATE TABLE IF NOT EXISTS app_user (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  username    VARCHAR(50)  NOT NULL UNIQUE,
  password    VARCHAR(100) NOT NULL,           -- bcrypt哈希，绝不存明文
  name        VARCHAR(50)  NOT NULL,
  status      SMALLINT     NOT NULL DEFAULT 1, -- 1启用 0停用
  created_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- 分销商
CREATE TABLE IF NOT EXISTS distributor (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name        VARCHAR(100) NOT NULL,
  parent_id   BIGINT NULL,                     -- 树形分组
  status      SMALLINT    NOT NULL DEFAULT 1,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 货品件（一物一码）
CREATE TABLE IF NOT EXISTS item (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  barcode        VARCHAR(32)  NOT NULL UNIQUE,          -- 仅A-Z0-9
  name           VARCHAR(100) NOT NULL,
  category       VARCHAR(50)  NOT NULL,                 -- 首饰大类
  purity         VARCHAR(50)  NOT NULL,                 -- 成色
  weight_g       NUMERIC(10,4) NOT NULL,                -- 总件重(克)
  labor_fee      NUMERIC(12,2) NOT NULL DEFAULT 0,      -- 销售工费
  labor_fee_mode VARCHAR(10)  NOT NULL DEFAULT '按件',
  location_type  VARCHAR(10)  NOT NULL DEFAULT '总库',  -- 总库/分销
  distributor_id BIGINT NULL,
  status         VARCHAR(20)  NOT NULL DEFAULT '在库',  -- 在库/锁定/分销在库/已售/已退库
  attrs          JSONB NULL,                            -- 动态列数据（JSONB可建索引，比JSON更好）
  version        INT          NOT NULL DEFAULT 0,       -- 乐观锁
  created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
  -- 条码字符集在数据库层再兜底一次（应用层校验之外的最后防线）
  CONSTRAINT chk_barcode CHECK (barcode ~ '^[A-Z0-9]+$')
);
CREATE INDEX IF NOT EXISTS idx_item_status ON item (status);
CREATE INDEX IF NOT EXISTS idx_item_loc    ON item (location_type, distributor_id);

-- 单据头
CREATE TABLE IF NOT EXISTS doc (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  doc_no         VARCHAR(20) NOT NULL UNIQUE,           -- 如RK2026080103
  doc_type       VARCHAR(20) NOT NULL,                  -- inbound/outbound/...
  status         VARCHAR(10) NOT NULL DEFAULT '草稿',   -- 草稿/已确认/已审核
  category       VARCHAR(50) NULL,
  supplier       VARCHAR(100) NULL,
  distributor_id BIGINT NULL,
  remark         VARCHAR(500) NULL,
  maker_id       BIGINT NOT NULL,
  confirmer_id   BIGINT NULL,
  auditor_id     BIGINT NULL,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  confirmed_at   TIMESTAMPTZ NULL
);

-- 单据明细
CREATE TABLE IF NOT EXISTS doc_line (
  id        BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  doc_id    BIGINT NOT NULL,
  item_id   BIGINT NOT NULL,
  line_no   INT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_line_doc  ON doc_line (doc_id);
CREATE INDEX IF NOT EXISTS idx_line_item ON doc_line (item_id);

-- 单号发号器
-- 取号（事务内执行，并发安全，一条语句拿到新序号）：
--   INSERT INTO doc_counter (doc_type, biz_date, seq) VALUES ($1, $2, 1)
--   ON CONFLICT (doc_type, biz_date) DO UPDATE SET seq = doc_counter.seq + 1
--   RETURNING seq;
CREATE TABLE IF NOT EXISTS doc_counter (
  doc_type  VARCHAR(20) NOT NULL,
  biz_date  CHAR(8) NOT NULL,                  -- YYYYMMDD
  seq       INT NOT NULL DEFAULT 0,
  PRIMARY KEY (doc_type, biz_date)
);

-- 库存流水：每一次状态/位置变动一条，审计与对账的生命线
CREATE TABLE IF NOT EXISTS stock_flow (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  item_id     BIGINT NOT NULL,
  doc_id      BIGINT NOT NULL,
  from_status VARCHAR(20) NOT NULL,
  to_status   VARCHAR(20) NOT NULL,
  from_loc    VARCHAR(50) NULL,
  to_loc      VARCHAR(50) NULL,
  operator_id BIGINT NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_flow_item ON stock_flow (item_id);
