// 黄金零售系统 - 后端 v0.3（单据状态机版）
//
// 相比 v0.2 的变化：入库从"一步到位"拆成真正的单据生命周期——
//
//	保存草稿 ──确认──▶ 已确认 ──反确认──▶ 回到草稿（条码保留）
//	   │
//	   └─删除（仅草稿可删；单号不回收）
//
// 关键机制（对应设计文档 4.2 / 6.x 节）：
//   - 草稿明细存在 doc.draft_lines(JSONB) 里，确认那一刻才生成货品件(item)；
//   - 确认/反确认都是"条件更新"（WHERE status='草稿'）：两台电脑同时点，只有一台成功；
//   - 反确认有下游校验：本单所有件必须仍"在库"，否则拒绝并指明条码；
//   - 反确认把件删除、明细(含已发的条码)写回草稿——再次确认时条码原样复用，不再发新号；
//   - 库存流水 stock_flow 正反都留痕。
//
// 运行前提：已执行 migrations/001 和 002；连接地址可用环境变量 DB_DSN 覆盖。
package main

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	_ "github.com/lib/pq"
)

// ===== 数据结构 =====

type Item struct {
	ID      int64   `json:"id"`
	Barcode string  `json:"barcode"`
	Name    string  `json:"name"`
	Purity  string  `json:"purity"`
	WeightG float64 `json:"weightG"`
	Status  string  `json:"status"`
}

// DraftLine 草稿明细行（存进 doc.draft_lines 的 JSON 结构，字段名与前端一致）
type DraftLine struct {
	Barcode string  `json:"barcode"`
	Name    string  `json:"name"`
	Purity  string  `json:"purity"`
	WeightG float64 `json:"weightG"`
}

type InboundDoc struct {
	ID       int64  `json:"id"`
	DocNo    string `json:"docNo"`
	Category string `json:"category"`
	Status   string `json:"status"` // 草稿 / 已确认
	Items    []Item `json:"items"`
	MadeAt   string `json:"madeAt"`
}

var db *sql.DB

// ===== 账号与令牌（v0.4） =====
//
// 密码存储：PBKDF2-HMAC-SHA256 加盐哈希（Go 标准库），格式 pbkdf2:迭代次数:盐:哈希。
// 数据库里永远只有哈希——即使整库泄露，也无法还原出密码原文。
// 登录令牌：JWT(HS256)。服务端签发带过期时间的令牌，之后每个请求凭令牌证明身份，
// 服务端无需保存会话状态（无状态认证，天然适配多终端）。

var jwtSecret = func() []byte {
	if s := os.Getenv("JWT_SECRET"); s != "" {
		return []byte(s)
	}
	return []byte("dev-secret-change-me-in-production") // 上线前必须用环境变量换掉
}()

const pbkdf2Iter = 120000

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2:%d:%s:%s", pbkdf2Iter, hex.EncodeToString(salt), hex.EncodeToString(key)), nil
}

func verifyPassword(pw, stored string) bool {
	parts := strings.Split(stored, ":")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1 // 恒定时间比较，防时序侧信道
}

// ensureAdmin 首次启动时创建默认管理员（admin/123456），并提醒改密码
func ensureAdmin() {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM app_user`).Scan(&n); err != nil {
		panic("检查用户表失败: " + err.Error())
	}
	if n > 0 {
		return
	}
	h, err := hashPassword("123456")
	if err != nil {
		panic("生成密码哈希失败: " + err.Error())
	}
	if _, err := db.Exec(`INSERT INTO app_user (username, password, name, status, is_admin) VALUES ('admin', $1, '超级管理员', 1, true)`, h); err != nil {
		panic("创建默认管理员失败: " + err.Error())
	}
	fmt.Println("已创建默认管理员 admin / 123456 —— 请尽快登录后修改密码！")
}

func issueToken(uid int64, username, name string) (string, error) {
	claims := jwt.MapClaims{
		"uid":  uid,
		"usr":  username,
		"name": name,
		"exp":  time.Now().Add(12 * time.Hour).Unix(), // 12小时过期，过期需重新登录
		"iat":  time.Now().Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(jwtSecret)
}

type ctxKey string

const (
	ctxUID ctxKey = "uid"
	ctxAdm ctxKey = "adm"
)

// currentUID 从请求上下文取当前登录用户id（withAuth 已验证并放入）
func currentUID(r *http.Request) int64 {
	if v, ok := r.Context().Value(ctxUID).(int64); ok {
		return v
	}
	return 0
}

func currentIsAdmin(r *http.Request) bool {
	v, _ := r.Context().Value(ctxAdm).(bool)
	return v
}

// ===== 发号器（事务内取号，并发安全） =====

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

// normalizeLines 整理并校验草稿明细（保存和确认共用）：
// 条码转大写去空白；填写了的条码校验字符集与本单内不重复；weightG>0；name 非空。
func normalizeLines(lines []DraftLine) ([]DraftLine, error) {
	if len(lines) == 0 {
		return nil, fmt.Errorf("至少要有一行明细")
	}
	seen := map[string]bool{}
	out := make([]DraftLine, 0, len(lines))
	for i, l := range lines {
		l.Barcode = strings.ToUpper(strings.TrimSpace(l.Barcode))
		if l.Barcode != "" {
			if !validBarcode(l.Barcode) {
				return nil, fmt.Errorf("第%d行条码 %s 含非法字符（仅允许大写字母和数字）", i+1, l.Barcode)
			}
			if seen[l.Barcode] {
				return nil, fmt.Errorf("第%d行条码 %s 在本单内重复", i+1, l.Barcode)
			}
			seen[l.Barcode] = true
		}
		if strings.TrimSpace(l.Name) == "" {
			return nil, fmt.Errorf("第%d行缺少首饰名称", i+1)
		}
		if l.WeightG <= 0 {
			return nil, fmt.Errorf("第%d行总件重必须大于0", i+1)
		}
		out = append(out, l)
	}
	return out, nil
}

// ===== 中间件 =====

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
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if raw == "" {
			writeErr(w, 401, "未登录")
			return
		}
		tok, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
			if t.Method != jwt.SigningMethodHS256 { // 只接受我们签发时用的算法
				return nil, fmt.Errorf("算法不符")
			}
			return jwtSecret, nil
		})
		if err != nil || !tok.Valid {
			writeErr(w, 401, "登录已过期，请重新登录")
			return
		}
		claims, _ := tok.Claims.(jwt.MapClaims)
		uid, _ := claims["uid"].(float64) // JSON 数字解析为 float64
		if uid <= 0 {
			writeErr(w, 401, "令牌无效")
			return
		}
		// 实时校验账号状态：被禁用的账号即使令牌未过期也立即失效
		var status int
		var isAdmin bool
		err = db.QueryRow(`SELECT status, is_admin FROM app_user WHERE id=$1`, int64(uid)).Scan(&status, &isAdmin)
		if err != nil || status != 1 {
			writeErr(w, 401, "账号已被禁用或不存在")
			return
		}
		ctx := context.WithValue(r.Context(), ctxUID, int64(uid))
		ctx = context.WithValue(ctx, ctxAdm, isAdmin)
		next(w, r.WithContext(ctx))
	})
}

// ===== 基础接口 =====

func handleHealth(w http.ResponseWriter, r *http.Request) {
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
	var uid int64
	var stored, name string
	var isAdmin bool
	err := db.QueryRow(`SELECT id, password, name, is_admin FROM app_user WHERE username=$1 AND status=1`,
		strings.TrimSpace(req.Username)).Scan(&uid, &stored, &name, &isAdmin)
	if err == sql.ErrNoRows || (err == nil && !verifyPassword(req.Password, stored)) {
		// 账号不存在与密码错误返回同一句话——不给攻击者"账号是否存在"的线索
		writeErr(w, 401, "账号或密码错误")
		return
	}
	if err != nil {
		writeErr(w, 500, "登录失败: "+err.Error())
		return
	}
	token, err := issueToken(uid, req.Username, name)
	if err != nil {
		writeErr(w, 500, "签发令牌失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"token": token, "name": name, "isAdmin": isAdmin})
}

// POST /api/me/password —— 修改自己的密码（需先验证旧密码）
func handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数格式错误")
		return
	}
	if len(req.NewPassword) < 6 {
		writeErr(w, 400, "新密码至少6位")
		return
	}
	uid := currentUID(r)
	var stored string
	if err := db.QueryRow(`SELECT password FROM app_user WHERE id=$1`, uid).Scan(&stored); err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	if !verifyPassword(req.OldPassword, stored) {
		writeErr(w, 400, "旧密码不正确")
		return
	}
	h, err := hashPassword(req.NewPassword)
	if err != nil {
		writeErr(w, 500, "生成哈希失败: "+err.Error())
		return
	}
	if _, err := db.Exec(`UPDATE app_user SET password=$1 WHERE id=$2`, h, uid); err != nil {
		writeErr(w, 500, "更新失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "密码已修改"})
}

// ===== 用户管理（仅管理员，v0.5） =====

func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !currentIsAdmin(r) {
		writeErr(w, 403, "需要管理员权限")
		return false
	}
	return true
}

// GET /api/users —— 用户列表
func handleUserList(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	rows, err := db.Query(`SELECT id, username, name, status, is_admin,
		to_char(created_at,'YYYY-MM-DD') FROM app_user ORDER BY id`)
	if err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	defer rows.Close()
	type U struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		Name     string `json:"name"`
		Status   int    `json:"status"`
		IsAdmin  bool   `json:"isAdmin"`
		Created  string `json:"created"`
	}
	list := []U{}
	for rows.Next() {
		var u U
		if err := rows.Scan(&u.ID, &u.Username, &u.Name, &u.Status, &u.IsAdmin, &u.Created); err != nil {
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		list = append(list, u)
	}
	writeJSON(w, 200, map[string]any{"list": list})
}

// POST /api/users —— 新建用户
func handleUserCreate(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req struct {
		Username string `json:"username"`
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数格式错误")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	req.Name = strings.TrimSpace(req.Name)
	if len(req.Username) < 3 || len(req.Username) > 20 {
		writeErr(w, 400, "用户名需3-20个字符")
		return
	}
	for _, ch := range req.Username {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_') {
			writeErr(w, 400, "用户名只能含字母、数字、下划线")
			return
		}
	}
	if req.Name == "" {
		writeErr(w, 400, "姓名不能为空")
		return
	}
	if len(req.Password) < 6 {
		writeErr(w, 400, "初始密码至少6位")
		return
	}
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM app_user WHERE username=$1)`, req.Username).Scan(&exists); err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	if exists {
		writeErr(w, 400, "用户名已存在")
		return
	}
	h, err := hashPassword(req.Password)
	if err != nil {
		writeErr(w, 500, "生成哈希失败: "+err.Error())
		return
	}
	var id int64
	if err := db.QueryRow(`INSERT INTO app_user (username, password, name, status, is_admin)
		VALUES ($1,$2,$3,1,false) RETURNING id`, req.Username, h, req.Name).Scan(&id); err != nil {
		writeErr(w, 500, "创建失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "username": req.Username})
}

// POST /api/users/status —— 启用/禁用（不能操作自己）
func handleUserStatus(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req struct {
		ID     int64 `json:"id"`
		Status int   `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == 0 || (req.Status != 0 && req.Status != 1) {
		writeErr(w, 400, "参数错误")
		return
	}
	if req.ID == currentUID(r) {
		writeErr(w, 400, "不能启用/禁用自己的账号")
		return
	}
	res, err := db.Exec(`UPDATE app_user SET status=$1 WHERE id=$2`, req.Status, req.ID)
	if err != nil {
		writeErr(w, 500, "更新失败: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 400, "用户不存在")
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "status": req.Status})
}

// POST /api/users/password —— 管理员重置任意用户密码
func handleUserResetPwd(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req struct {
		ID          int64  `json:"id"`
		NewPassword string `json:"newPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == 0 {
		writeErr(w, 400, "参数错误")
		return
	}
	if len(req.NewPassword) < 6 {
		writeErr(w, 400, "新密码至少6位")
		return
	}
	h, err := hashPassword(req.NewPassword)
	if err != nil {
		writeErr(w, 500, "生成哈希失败: "+err.Error())
		return
	}
	res, err := db.Exec(`UPDATE app_user SET password=$1 WHERE id=$2`, h, req.ID)
	if err != nil {
		writeErr(w, 500, "更新失败: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 400, "用户不存在")
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "密码已重置"})
}

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

// ===== 入库单：保存草稿 =====
// POST /api/doc/inbound/save  请求: {id?, category, lines[]}
// id 为空 → 新建草稿并取单号（草稿即占号，删除不回收）；id 非空 → 更新已有草稿。
func handleInboundSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       int64       `json:"id"`
		Category string      `json:"category"`
		Lines    []DraftLine `json:"lines"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数格式错误")
		return
	}
	if req.Category == "" {
		req.Category = "黄金"
	}
	lines, err := normalizeLines(req.Lines)
	if err != nil {
		writeErr(w, 400, err.Error()+"，未保存")
		return
	}
	linesJSON, _ := json.Marshal(lines)

	if req.ID == 0 {
		// 新建草稿：取号 + 插入，同一事务
		tx, err := db.Begin()
		if err != nil {
			writeErr(w, 500, "开启事务失败: "+err.Error())
			return
		}
		defer tx.Rollback()
		today := time.Now().Format("20060102")
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
		var docID int64
		err = tx.QueryRow(`
			INSERT INTO doc (doc_no, doc_type, status, category, maker_id, draft_lines)
			VALUES ($1, 'inbound', '草稿', $2, $3, $4) RETURNING id`,
			docNo, req.Category, currentUID(r), linesJSON).Scan(&docID)
		if err != nil {
			writeErr(w, 500, "保存失败: "+err.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			writeErr(w, 500, "提交失败: "+err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"id": docID, "docNo": docNo, "status": "草稿"})
		return
	}

	// 更新已有草稿：条件更新——只有仍是草稿才允许改（并发/状态保护）
	res, err := db.Exec(`
		UPDATE doc SET category=$1, draft_lines=$2
		WHERE id=$3 AND doc_type='inbound' AND status='草稿'`,
		req.Category, linesJSON, req.ID)
	if err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 400, "该单据不是草稿状态（可能已被确认或删除），请刷新")
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "status": "草稿"})
}

// ===== 入库单：确认 =====
// POST /api/doc/inbound/confirm  请求: {id}
// 确认 = 业务生效点：此刻才生成货品件、写库存流水；全程一个事务。
func handleInboundConfirm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == 0 {
		writeErr(w, 400, "参数错误：缺少单据id")
		return
	}
	tx, err := db.Begin()
	if err != nil {
		writeErr(w, 500, "开启事务失败: "+err.Error())
		return
	}
	defer tx.Rollback()

	// 条件更新：只有"草稿"能变"已确认"。两台电脑同时点确认，只有一台影响行数=1。
	res, err := tx.Exec(`
		UPDATE doc SET status='已确认', confirmed_at=now()
		WHERE id=$1 AND doc_type='inbound' AND status='草稿'`, req.ID)
	if err != nil {
		writeErr(w, 500, "确认失败: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 400, "确认失败：单据不存在或已被他人确认，请刷新")
		return
	}

	// 读出草稿明细
	var docNo, category string
	var linesJSON []byte
	err = tx.QueryRow(`SELECT doc_no, COALESCE(category,''), draft_lines
		FROM doc WHERE id=$1`, req.ID).Scan(&docNo, &category, &linesJSON)
	if err != nil {
		writeErr(w, 500, "读取单据失败: "+err.Error())
		return
	}
	var lines []DraftLine
	if err := json.Unmarshal(linesJSON, &lines); err != nil || len(lines) == 0 {
		writeErr(w, 400, "草稿明细为空，无法确认")
		return
	}

	// 逐行生成货品件（条码留空→发号；已有条码→查重后复用）
	today := time.Now().Format("20060102")
	doc := InboundDoc{ID: req.ID, DocNo: docNo, Category: category, Status: "已确认",
		MadeAt: time.Now().Format("2006-01-02 15:04:05")}
	finalLines := make([]DraftLine, 0, len(lines)) // 回写：把发出的条码存回草稿字段，反确认后可复用
	for i, l := range lines {
		bc := l.Barcode
		if bc == "" {
			seq, err := nextSeq(tx, "BC", today)
			if err != nil {
				writeErr(w, 500, "条码发号失败: "+err.Error())
				return
			}
			bc = fmt.Sprintf("SA%s%04d", time.Now().Format("060102"), seq)
		}
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM item WHERE barcode=$1)`, bc).Scan(&exists); err != nil {
			writeErr(w, 500, "条码查询失败: "+err.Error())
			return
		}
		if exists {
			writeErr(w, 400, fmt.Sprintf("第%d行条码 %s 已存在，整单未确认", i+1, bc))
			return
		}
		var itemID int64
		err = tx.QueryRow(`
			INSERT INTO item (barcode, name, category, purity, weight_g, status)
			VALUES ($1,$2,$3,$4,$5,'在库') RETURNING id`,
			bc, l.Name, category, l.Purity, l.WeightG).Scan(&itemID)
		if err != nil {
			writeErr(w, 500, fmt.Sprintf("第%d行写入失败: %s", i+1, err.Error()))
			return
		}
		if _, err = tx.Exec(`INSERT INTO doc_line (doc_id, item_id, line_no) VALUES ($1,$2,$3)`,
			req.ID, itemID, i+1); err != nil {
			writeErr(w, 500, "写入明细失败: "+err.Error())
			return
		}
		if _, err = tx.Exec(`
			INSERT INTO stock_flow (item_id, doc_id, from_status, to_status, to_loc, operator_id)
			VALUES ($1,$2,'(入库确认)','在库','总库',$3)`, itemID, req.ID, currentUID(r)); err != nil {
			writeErr(w, 500, "写入流水失败: "+err.Error())
			return
		}
		doc.Items = append(doc.Items, Item{ID: itemID, Barcode: bc, Name: l.Name,
			Purity: l.Purity, WeightG: l.WeightG, Status: "在库"})
		l.Barcode = bc
		finalLines = append(finalLines, l)
	}

	// 把最终条码回写草稿字段（反确认后再确认时条码不变）
	fl, _ := json.Marshal(finalLines)
	if _, err = tx.Exec(`UPDATE doc SET draft_lines=$1 WHERE id=$2`, fl, req.ID); err != nil {
		writeErr(w, 500, "回写明细失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, doc)
}

// ===== 入库单：反确认 =====
// POST /api/doc/inbound/unconfirm  请求: {id}
// 撤销生效：校验本单所有件仍"在库"→ 写反向流水 → 删件 → 单据退回草稿（条码保留在草稿里）。
func handleInboundUnconfirm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == 0 {
		writeErr(w, 400, "参数错误：缺少单据id")
		return
	}
	tx, err := db.Begin()
	if err != nil {
		writeErr(w, 500, "开启事务失败: "+err.Error())
		return
	}
	defer tx.Rollback()

	// 条件更新：只有"已确认"能反确认（并发保护同前）
	res, err := tx.Exec(`
		UPDATE doc SET status='草稿', confirmed_at=NULL
		WHERE id=$1 AND doc_type='inbound' AND status='已确认'`, req.ID)
	if err != nil {
		writeErr(w, 500, "反确认失败: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 400, "反确认失败：单据不存在或不是已确认状态，请刷新")
		return
	}

	// 下游依赖校验：本单所有件必须仍"在库"且在总库。
	// （将来有了销售/分销单，被引用的件状态会变，这里就会拦住并指明哪件被占用。）
	rows, err := tx.Query(`
		SELECT it.id, it.barcode, it.status, it.name, it.purity, it.weight_g
		FROM doc_line dl JOIN item it ON it.id = dl.item_id
		WHERE dl.doc_id=$1 ORDER BY dl.line_no`, req.ID)
	if err != nil {
		writeErr(w, 500, "明细查询失败: "+err.Error())
		return
	}
	type ref struct {
		id      int64
		barcode string
	}
	var itemRefs []ref
	var rebuilt []DraftLine // 从真实货品件重建草稿明细（兼容老单据草稿字段为空的情况）
	for rows.Next() {
		var id int64
		var bc, st, name, purity string
		var wg float64
		if err := rows.Scan(&id, &bc, &st, &name, &purity, &wg); err != nil {
			rows.Close()
			writeErr(w, 500, "明细读取失败: "+err.Error())
			return
		}
		if st != "在库" {
			rows.Close()
			writeErr(w, 400, fmt.Sprintf("反确认被拒绝：条码 %s 当前状态为「%s」，已被下游业务占用", bc, st))
			return
		}
		itemRefs = append(itemRefs, ref{id, bc})
		rebuilt = append(rebuilt, DraftLine{Barcode: bc, Name: name, Purity: purity, WeightG: wg})
	}
	rows.Close()
	if len(rebuilt) > 0 {
		// 把明细(含条码)写回草稿字段——保证反确认后的草稿永远有内容可编辑/再确认
		rj, _ := json.Marshal(rebuilt)
		if _, err = tx.Exec(`UPDATE doc SET draft_lines=$1 WHERE id=$2`, rj, req.ID); err != nil {
			writeErr(w, 500, "回写草稿明细失败: "+err.Error())
			return
		}
	}

	// 写反向流水 → 删明细 → 删件（流水是日志，保留下来就是审计轨迹）
	for _, it := range itemRefs {
		if _, err = tx.Exec(`
			INSERT INTO stock_flow (item_id, doc_id, from_status, to_status, from_loc, operator_id)
			VALUES ($1,$2,'在库','(反确认撤销)','总库',$3)`, it.id, req.ID, currentUID(r)); err != nil {
			writeErr(w, 500, "写入流水失败: "+err.Error())
			return
		}
	}
	if _, err = tx.Exec(`DELETE FROM doc_line WHERE doc_id=$1`, req.ID); err != nil {
		writeErr(w, 500, "删除明细失败: "+err.Error())
		return
	}
	for _, it := range itemRefs {
		if _, err = tx.Exec(`DELETE FROM item WHERE id=$1`, it.id); err != nil {
			writeErr(w, 500, "删除货品失败: "+err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "status": "草稿"})
}

// ===== 入库单：删除草稿 =====
// POST /api/doc/inbound/delete  请求: {id}（仅草稿可删；单号留空洞，不回收）
func handleInboundDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == 0 {
		writeErr(w, 400, "参数错误：缺少单据id")
		return
	}
	res, err := db.Exec(`DELETE FROM doc WHERE id=$1 AND doc_type='inbound' AND status='草稿'`, req.ID)
	if err != nil {
		writeErr(w, 500, "删除失败: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 400, "删除失败：只有草稿可以删除（已确认的请先反确认）")
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": req.ID})
}

// ===== 入库单：列表（草稿读draft_lines，已确认读真实明细） =====
func handleInboundList(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`
		SELECT id, doc_no, COALESCE(category,''), status, draft_lines,
		       to_char(created_at,'YYYY-MM-DD HH24:MI:SS')
		FROM doc WHERE doc_type='inbound' ORDER BY id DESC LIMIT 20`)
	if err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	defer rows.Close()

	docs := []InboundDoc{}
	drafts := map[int64][]byte{}
	for rows.Next() {
		var d InboundDoc
		var dl []byte
		if err := rows.Scan(&d.ID, &d.DocNo, &d.Category, &d.Status, &dl, &d.MadeAt); err != nil {
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		drafts[d.ID] = dl
		docs = append(docs, d)
	}
	for i := range docs {
		docs[i].Items = []Item{} // 永远返回数组而不是 null，前端渲染才安全
		if docs[i].Status == "草稿" {
			var lines []DraftLine
			_ = json.Unmarshal(drafts[docs[i].ID], &lines)
			for _, l := range lines {
				docs[i].Items = append(docs[i].Items, Item{Barcode: l.Barcode, Name: l.Name,
					Purity: l.Purity, WeightG: l.WeightG, Status: "草稿"})
			}
			continue
		}
		irows, err := db.Query(`
			SELECT it.id, it.barcode, it.name, it.purity, it.weight_g, it.status
			FROM doc_line dl JOIN item it ON it.id = dl.item_id
			WHERE dl.doc_id=$1 ORDER BY dl.line_no`, docs[i].ID)
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
	ensureAdmin()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", withCORS(handleHealth))
	mux.HandleFunc("POST /api/login", withCORS(handleLogin))
	mux.HandleFunc("OPTIONS /api/", withCORS(func(w http.ResponseWriter, r *http.Request) {}))
	mux.HandleFunc("POST /api/me/password", withAuth(handleChangePassword))
	mux.HandleFunc("GET /api/users", withAuth(handleUserList))
	mux.HandleFunc("POST /api/users", withAuth(handleUserCreate))
	mux.HandleFunc("POST /api/users/status", withAuth(handleUserStatus))
	mux.HandleFunc("POST /api/users/password", withAuth(handleUserResetPwd))
	mux.HandleFunc("GET /api/items", withAuth(handleItems))
	mux.HandleFunc("POST /api/doc/inbound/save", withAuth(handleInboundSave))
	mux.HandleFunc("POST /api/doc/inbound/confirm", withAuth(handleInboundConfirm))
	mux.HandleFunc("POST /api/doc/inbound/unconfirm", withAuth(handleInboundUnconfirm))
	mux.HandleFunc("POST /api/doc/inbound/delete", withAuth(handleInboundDelete))
	mux.HandleFunc("GET /api/doc/inbound", withAuth(handleInboundList))

	fmt.Println("后端已启动: http://localhost:8080/api/health  (Ctrl+C 停止)")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		panic(err)
	}
}
