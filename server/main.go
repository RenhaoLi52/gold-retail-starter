// 黄金零售系统 - 后端 v0.2（PostgreSQL 版）
//
// 相比内存版的变化：
//   1. 数据存进 PostgreSQL，重启不丢；
//   2. "全有或全无"由数据库事务保证（BEGIN → 全部成功 COMMIT / 任何失败 ROLLBACK）；
//   3. 单号/条码由数据库发号器（doc_counter 表 + ON CONFLICT）生成，多终端并发不重号；
//   4. 每件货入库同时写 stock_flow 库存流水（审计对账的生命线）。
//
// 运行前提：PostgreSQL 已启动且已执行 migrations/001_init_postgres.sql 建表。
// 连接地址可用环境变量 DB_DSN 覆盖，默认连本机 Docker 容器。
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq" // PostgreSQL 驱动（只需引入，database/sql 会用它）
)

// ===== 数据结构（与前端交换 JSON 用） =====

type Item struct {
	ID      int64   `json:"id"`
	Barcode string  `json:"barcode"`
	Name    string  `json:"name"`
	Purity  string  `json:"purity"`
	WeightG float64 `json:"weightG"`
	Status  string  `json:"status"`
}

type InboundDoc struct {
	ID       int64  `json:"id"`
	DocNo    string `json:"docNo"`
	Category string `json:"category"`
	Status   string `json:"status"`
	Items    []Item `json:"items"`
	MadeAt   string `json:"madeAt"`
}

// ===== 全局数据库连接池 =====

var db *sql.DB

// ===== 发号器：在"已开启的事务"里取下一个序号 =====
// 一条 SQL 完成"不存在则插 1，存在则 +1，并返回新值"，并发安全。
func nextSeq(tx *sql.Tx, docType, bizDate string) (int, error) {
	var seq int
	err := tx.QueryRow(`
		INSERT INTO doc_counter (doc_type, biz_date, seq) VALUES ($1, $2, 1)
		ON CONFLICT (doc_type, biz_date) DO UPDATE SET seq = doc_counter.seq + 1
		RETURNING seq`, docType, bizDate).Scan(&seq)
	return seq, err
}

// ===== 小工具 =====

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func validBarcode(bc string) bool {
	if bc == "" || len(bc) > 32 {
		return false
	}
	for _, ch := range bc {
		if !((ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')) {
			return false
		}
	}
	return true
}

// ===== 中间件（与内存版相同） =====

func withCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

func withAuth(next http.HandlerFunc) http.HandlerFunc {
	return withCORS(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token != "demo-token-123" {
			writeErr(w, 401, "未登录或登录已过期")
			return
		}
		next(w, r)
	})
}

// ===== 接口 =====

func handleHealth(w http.ResponseWriter, r *http.Request) {
	// 顺带检查数据库连通性，让健康检查更有意义
	if err := db.Ping(); err != nil {
		writeErr(w, 500, "数据库连接异常: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok", "db": "ok", "time": time.Now().Format(time.RFC3339)})
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数格式错误")
		return
	}
	// 教学版：仍是写死的账号；接权限框架时换成查 app_user 表 + bcrypt 校验 + JWT
	if req.Username == "admin" && req.Password == "123456" {
		writeJSON(w, 200, map[string]string{"token": "demo-token-123", "name": "超级管理员"})
		return
	}
	writeErr(w, 401, "账号或密码错误")
}

// GET /api/items —— 从数据库读库存
func handleItems(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`
		SELECT id, barcode, name, purity, weight_g, status
		FROM item ORDER BY id DESC LIMIT 500`)
	if err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	defer rows.Close()

	list := []Item{}
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.Barcode, &it.Name, &it.Purity, &it.WeightG, &it.Status); err != nil {
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		list = append(list, it)
	}
	writeJSON(w, 200, map[string]any{"list": list, "total": len(list)})
}

// POST /api/doc/inbound/create —— 在一个数据库事务里完成整张入库单
func handleInboundCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Category string `json:"category"`
		Lines    []struct {
			Barcode string  `json:"barcode"`
			Name    string  `json:"name"`
			Purity  string  `json:"purity"`
			WeightG float64 `json:"weightG"`
		} `json:"lines"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Lines) == 0 {
		writeErr(w, 400, "参数错误：至少要有一行明细")
		return
	}
	if req.Category == "" {
		req.Category = "黄金"
	}

	// ====== 开启事务：从这里到 Commit 之间要么全成功、要么全不发生 ======
	tx, err := db.Begin()
	if err != nil {
		writeErr(w, 500, "开启事务失败: "+err.Error())
		return
	}
	defer tx.Rollback() // 中途任何 return，事务自动回滚；Commit 成功后此句无副作用

	today := time.Now().Format("20060102")

	// 1) 逐行校验 + 准备条码（全部通过才会走到写入）
	type line struct {
		barcode string
		name    string
		purity  string
		weightG float64
	}
	lines := make([]line, 0, len(req.Lines))
	seen := map[string]bool{}
	for i, l := range req.Lines {
		bc := strings.ToUpper(strings.TrimSpace(l.Barcode))
		if bc == "" {
			// 条码留空 → 数据库发号（BC 计数器），规则：SA + 年月日(6位) + 4位序号
			seq, err := nextSeq(tx, "BC", today)
			if err != nil {
				writeErr(w, 500, "条码发号失败: "+err.Error())
				return
			}
			bc = fmt.Sprintf("SA%s%04d", time.Now().Format("060102"), seq)
		}
		if !validBarcode(bc) {
			writeErr(w, 400, fmt.Sprintf("第%d行条码 %s 含非法字符（仅允许大写字母和数字），整单未保存", i+1, bc))
			return
		}
		if seen[bc] {
			writeErr(w, 400, fmt.Sprintf("第%d行条码 %s 在本单内重复，整单未保存", i+1, bc))
			return
		}
		// 查库里是否已存在该条码
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM item WHERE barcode=$1)`, bc).Scan(&exists); err != nil {
			writeErr(w, 500, "条码查询失败: "+err.Error())
			return
		}
		if exists {
			writeErr(w, 400, fmt.Sprintf("第%d行条码 %s 已存在，整单未保存", i+1, bc))
			return
		}
		seen[bc] = true
		lines = append(lines, line{bc, l.Name, l.Purity, l.WeightG})
	}

	// 2) 取入库单号：RK + YYYYMMDD + 2位序号，当日上限99
	seq, err := nextSeq(tx, "RK", today)
	if err != nil {
		writeErr(w, 500, "单号发号失败: "+err.Error())
		return
	}
	if seq > 99 {
		writeErr(w, 400, "当日入库单号已满99张")
		return
	}
	docNo := fmt.Sprintf("RK%s%02d", today, seq)

	// 3) 写单据头（教学版 maker_id 暂用 1；接权限框架后换成登录用户ID）
	var docID int64
	err = tx.QueryRow(`
		INSERT INTO doc (doc_no, doc_type, status, category, maker_id, confirmed_at)
		VALUES ($1, 'inbound', '已确认', $2, 1, now())
		RETURNING id`, docNo, req.Category).Scan(&docID)
	if err != nil {
		writeErr(w, 500, "写入单据失败: "+err.Error())
		return
	}

	// 4) 逐行写 货品件 + 单据明细 + 库存流水
	doc := InboundDoc{ID: docID, DocNo: docNo, Category: req.Category, Status: "已确认",
		MadeAt: time.Now().Format("2006-01-02 15:04:05")}
	for i, l := range lines {
		var itemID int64
		err = tx.QueryRow(`
			INSERT INTO item (barcode, name, category, purity, weight_g, status)
			VALUES ($1, $2, $3, $4, $5, '在库')
			RETURNING id`, l.barcode, l.name, req.Category, l.purity, l.weightG).Scan(&itemID)
		if err != nil {
			writeErr(w, 500, fmt.Sprintf("第%d行写入失败: %s", i+1, err.Error()))
			return
		}
		if _, err = tx.Exec(`INSERT INTO doc_line (doc_id, item_id, line_no) VALUES ($1,$2,$3)`,
			docID, itemID, i+1); err != nil {
			writeErr(w, 500, "写入明细失败: "+err.Error())
			return
		}
		if _, err = tx.Exec(`
			INSERT INTO stock_flow (item_id, doc_id, from_status, to_status, to_loc, operator_id)
			VALUES ($1, $2, '(入库)', '在库', '总库', 1)`, itemID, docID); err != nil {
			writeErr(w, 500, "写入流水失败: "+err.Error())
			return
		}
		doc.Items = append(doc.Items, Item{ID: itemID, Barcode: l.barcode, Name: l.name,
			Purity: l.purity, WeightG: l.weightG, Status: "在库"})
	}

	// 5) 提交事务——到这一刻，整张单才真正"发生"
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, doc)
}

// GET /api/doc/inbound —— 入库单列表（最近20张，含明细）
func handleInboundList(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`
		SELECT id, doc_no, COALESCE(category,''), status,
		       to_char(created_at, 'YYYY-MM-DD HH24:MI:SS')
		FROM doc WHERE doc_type='inbound' ORDER BY id DESC LIMIT 20`)
	if err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	defer rows.Close()

	docs := []InboundDoc{}
	for rows.Next() {
		var d InboundDoc
		if err := rows.Scan(&d.ID, &d.DocNo, &d.Category, &d.Status, &d.MadeAt); err != nil {
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		docs = append(docs, d)
	}
	// 补每张单的明细（20张以内，逐单查询足够；量大时再优化成一次联查）
	for i := range docs {
		irows, err := db.Query(`
			SELECT it.id, it.barcode, it.name, it.purity, it.weight_g, it.status
			FROM doc_line dl JOIN item it ON it.id = dl.item_id
			WHERE dl.doc_id = $1 ORDER BY dl.line_no`, docs[i].ID)
		if err != nil {
			writeErr(w, 500, "明细查询失败: "+err.Error())
			return
		}
		for irows.Next() {
			var it Item
			if err := irows.Scan(&it.ID, &it.Barcode, &it.Name, &it.Purity, &it.WeightG, &it.Status); err != nil {
				irows.Close()
				writeErr(w, 500, "明细读取失败: "+err.Error())
				return
			}
			docs[i].Items = append(docs[i].Items, it)
		}
		irows.Close()
	}
	writeJSON(w, 200, map[string]any{"list": docs, "total": len(docs)})
}

// ===== 主程序 =====

func main() {
	dsn := os.Getenv("DB_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:dev123456@localhost:5432/gold?sslmode=disable"
	}
	var err error
	db, err = sql.Open("postgres", dsn)
	if err != nil {
		panic("数据库配置错误: " + err.Error())
	}
	if err = db.Ping(); err != nil {
		panic("连不上数据库（golddb 容器启动了吗？）: " + err.Error())
	}
	db.SetMaxOpenConns(10)
	fmt.Println("数据库已连接:", dsn)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", withCORS(handleHealth))
	mux.HandleFunc("POST /api/login", withCORS(handleLogin))
	mux.HandleFunc("OPTIONS /api/", withCORS(func(w http.ResponseWriter, r *http.Request) {}))
	mux.HandleFunc("GET /api/items", withAuth(handleItems))
	mux.HandleFunc("POST /api/doc/inbound/create", withAuth(handleInboundCreate))
	mux.HandleFunc("GET /api/doc/inbound", withAuth(handleInboundList))

	fmt.Println("后端已启动: http://localhost:8080/api/health  (Ctrl+C 停止)")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		panic(err)
	}
}