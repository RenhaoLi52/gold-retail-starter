-- 013：账号绑门店（v0.19）
-- 设计定稿（与用户讨论）：不做"代理商密码"那层两段式登录——共享密码约等于没有密码。
-- 门店在建账号时绑死在账号上（NULL=总部），登录后服务端从账号推导"你是谁、哪个店、
-- 能卖哪里的货"，客户端一个字不传，伪造无门。
-- 将来上云多代理商时，再加"代理商代码"字段（只是门牌号，不是秘密），每个代理商独立数据库。
ALTER TABLE app_user ADD COLUMN IF NOT EXISTS distributor_id BIGINT REFERENCES distributor(id);
