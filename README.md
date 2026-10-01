# 黄金零售系统 · 入门骨架项目

配套《03-开发教程/从零开始开发教程-Mac版》使用。这是一个**能直接跑起来**的最小系统：
Go 后端（零第三方依赖）+ Vue3 前端，实现了登录、总库库存查询、入库单
（单号 RKyyyymmddNN、条码留空自动生成/填写则校验、整单全有或全无）。

## 快速启动（两个终端窗口）

终端 1 —— 启动后端：

    cd server
    go run main.go
    # 看到 "后端已启动: http://localhost:8080/api/health"

终端 2 —— 启动前端：

    cd client
    npm install        # 第一次需要，之后不用
    npm run dev
    # 浏览器打开 http://localhost:5173

登录账号：admin / 123456

## 目录结构

    server/
      main.go                        后端全部代码（刻意单文件，方便通读）
      migrations/001_init_postgres.sql  接入 PostgreSQL 时的第一版建表脚本（已在 PG16 实测）
    client/
      src/App.vue           界面（登录/入库单/库存列表）
      src/api.ts            API 调用统一封装
      vite.config.ts        /api 代理到 8080 后端

## 这个骨架教了哪些真实系统的关键概念

- 单号服务端发号：RK + YYYYMMDD + 2位序号，日上限99（main.go: genDocNo）
- 条码双模式：留空自动生成 / 填写则校验字符集(A-Z0-9)+全局唯一
- 全有或全无：任何一行校验失败，整单不入库（main.go 中标注的段落）
- 登录令牌：前端每个请求带 Authorization 头，后端中间件统一校验
- 并发保护：内存版用互斥锁演示，PostgreSQL 版换成数据库事务+条件更新

## 下一步（按教程章节走）

1. 把内存数据换成 PostgreSQL（migrations/001_init_postgres.sql，驱动用 pgx）
2. 拆分"保存草稿/确认/反确认"三个动作
3. 加权限点框架
4. Electron 打包成桌面应用
