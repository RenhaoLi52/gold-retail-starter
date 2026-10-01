-- 003：PBKDF2 哈希串(约112字符)超过原列宽，放宽密码列
ALTER TABLE app_user ALTER COLUMN password TYPE VARCHAR(200);
