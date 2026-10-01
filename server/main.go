// 黄金零售系统 - 入门骨架后端（零依赖版，只用 Go 标准库）
//
// 这是教学用的最小可运行版本：
//   1. 数据先放在内存里（重启就没了），目的是先跑通「前端→API→业务逻辑」这条链路；
//   2. 没有任何第三方依赖，装好 Go 后 `go run main.go` 直接启动；
//   3. 第二步再把内存换成 PostgreSQL（见 migrations/001_init_postgres.sql 和教程第 6 章），
//      届时驱动用 pgx，也可以顺手把路由换成 Gin 框架。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ===== 数据结构 =====

// Item 货品件：一物一码，每件首饰一条记录
type Item struct {
	ID      int     `json:"id"`
	Barcode string  `json:"barcode"` // 条码号，仅大写字母+数字
	Name    string  `json:"name"`    // 首饰名称
	Purity  string  `json:"purity"`  // 成色
	WeightG float64 `json:"weightG"` // 总件重(克)
	Status  string  `json:"status"`  // 在库/已售...
}

// InboundDoc 入库单（简化版：表头几个字段+明细）
type InboundDoc struct {
	ID       int    `json:"id"`
	DocNo    string `json:"docNo"`    // 单号，如 RK2026080503
	Category string `json:"category"` // 首饰大类
	Status   string `json:"status"`   // 草稿/已确认
	Items    []Item `json:"items"`
	MadeAt   string `json:"madeAt"`
}

// ===== 内存"数据库"（第二步换成 PostgreSQL） =====

var (
	mu    sync.Mutex // 互斥锁：多个请求同时进来时保证数据不打架
	items = []Item{
		{ID: 1, Barcode: "SA6071302735", Name: "足金999.9吊坠", Purity: "足金999.9", WeightG: 0.39, Status: "在库"},
		{ID: 2, Barcode: "SA6071302736", Name: "足金999.9吊坠", Purity: "足金999.9", WeightG: 0.38, Status: "在库"},
	}
	docs       []InboundDoc
	nextItem   = 3
	nextDoc    = 1
	docSeq     = map[string]int{} // 单号发号器: key="RK20260805" -> 当日已用序号
	barcodeSeq = 0                // 条码发号器（教学版：全局递增，保证不撞号）
)

// genDocNo 单号生成：前缀 + YYYYMMDD + 2位序号（当日最多99张）
// 真实系统里这段逻辑必须在服务端+数据库事务里做（PostgreSQL 用
// INSERT ... ON CONFLICT ... RETURNING seq 一条语句取号），客户端永远不拼单号。
func genDocNo(prefix string) (string, error) {
	key := prefix + time.Now().Format("20060102")
	docSeq[key]++
	if docSeq[key] > 99 {
		return "", fmt.Errorf("当日%s单号已满99张", prefix)
	}
	return fmt.Sprintf("%s%02d", key, docSeq[key]), nil
}

// ===== 小工具：统一的 JSON 返回 =====

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// ===== 中间件 =====

// withCORS：允许浏览器里的前端(5173端口)调用本服务(8080端口)
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

// withAuth：校验登录令牌（教学版：写死的 token；真实版换 JWT）
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

// ===== 接口处理函数 =====

// GET /api/health 健康检查：浏览器打开能看到 ok 就说明服务活着
func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok", "time": time.Now().Format(time.RFC3339)})
}

// POST /api/login 登录（教学版：账号 admin 密码 123456）
func handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数格式错误")
		return
	}
	if req.Username == "admin" && req.Password == "123456" {
		writeJSON(w, 200, map[string]string{"token": "demo-token-123", "name": "超级管理员"})
		return
	}
	writeErr(w, 401, "账号或密码错误")
}

// GET /api/items 库存列表
func handleItems(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	writeJSON(w, 200, map[string]any{"list": items, "total": len(items)})
}

// POST /api/doc/inbound/create 新建并确认一张入库单
// （教学版把"保存草稿"和"确认"合并成一步；真实版要拆开：保存→确认→审核，见设计文档4.2）
func handleInboundCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Category string `json:"category"`
		Lines    []struct {
			Barcode string  `json:"barcode"` // 留空则自动生成
			Name    string  `json:"name"`
			Purity  string  `json:"purity"`
			WeightG float64 `json:"weightG"`
		} `json:"lines"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Lines) == 0 {
		writeErr(w, 400, "参数错误：至少要有一行明细")
		return
	}

	mu.Lock()
	defer mu.Unlock()

	// ====== 关键原则：全有或全无 ======
	// 先把整批明细全部校验完、在临时切片里构造好，任何一行出错都直接返回，
	// 库存一件不动；全部通过后才一次性提交。真实系统里这就是"数据库事务"。
	newItems := make([]Item, 0, len(req.Lines))
	seen := map[string]bool{} // 本批内部去重
	for i, l := range req.Lines {
		bc := strings.ToUpper(strings.TrimSpace(l.Barcode))
		if bc == "" {
			// 条码留空 → 服务端发号（教学版：SA+日期+4位全局序号，保证不撞号）
			barcodeSeq++
			bc = fmt.Sprintf("SA%s%04d", time.Now().Format("060102"), barcodeSeq)
		}
		// 条码字符集校验：仅 A-Z 0-9
		for _, ch := range bc {
			if !((ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')) {
				writeErr(w, 400, fmt.Sprintf("第%d行条码 %s 含非法字符（仅允许大写字母和数字），整单未导入", i+1, bc))
				return
			}
		}
		// 唯一性校验（对已有库存 + 本批内部）
		if seen[bc] {
			writeErr(w, 400, fmt.Sprintf("第%d行条码 %s 在本单内重复，整单未导入", i+1, bc))
			return
		}
		for _, it := range items {
			if it.Barcode == bc {
				writeErr(w, 400, fmt.Sprintf("第%d行条码 %s 已存在，整单未导入", i+1, bc))
				return
			}
		}
		seen[bc] = true
		newItems = append(newItems, Item{Barcode: bc, Name: l.Name, Purity: l.Purity,
			WeightG: l.WeightG, Status: "在库"})
	}

	// 全部校验通过，才取单号、分配ID、一次性入账
	docNo, err := genDocNo("RK")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	doc := InboundDoc{ID: nextDoc, DocNo: docNo, Category: req.Category, Status: "已确认",
		MadeAt: time.Now().Format("2006-01-02 15:04:05")}
	nextDoc++
	for _, it := range newItems {
		it.ID = nextItem
		nextItem++
		items = append(items, it)
		doc.Items = append(doc.Items, it)
	}
	docs = append(docs, doc)
	writeJSON(w, 200, doc)
}

// GET /api/doc/inbound 入库单列表
func handleInboundList(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	writeJSON(w, 200, map[string]any{"list": docs, "total": len(docs)})
}

// ===== 主程序 =====

func main() {
	mux := http.NewServeMux()

	// 公开接口
	mux.HandleFunc("GET /api/health", withCORS(handleHealth))
	mux.HandleFunc("POST /api/login", withCORS(handleLogin))
	mux.HandleFunc("OPTIONS /api/", withCORS(func(w http.ResponseWriter, r *http.Request) {}))

	// 需要登录的接口
	mux.HandleFunc("GET /api/items", withAuth(handleItems))
	mux.HandleFunc("POST /api/doc/inbound/create", withAuth(handleInboundCreate))
	mux.HandleFunc("GET /api/doc/inbound", withAuth(handleInboundList))

	fmt.Println("后端已启动: http://localhost:8080/api/health  (Ctrl+C 停止)")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		panic(err)
	}
}
