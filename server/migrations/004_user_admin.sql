-- 004：用户管理——加管理员标记（角色权限框架的第一块砖）
ALTER TABLE app_user ADD COLUMN IF NOT EXISTS is_admin BOOLEAN NOT NULL DEFAULT false;
UPDATE app_user SET is_admin = true WHERE username = 'admin';
