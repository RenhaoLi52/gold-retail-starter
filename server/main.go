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
	ID       int64   `json:"id"`
	Barcode  string  `json:"barcode"`
	Name     string  `json:"name"`
	Category string  `json:"category,omitempty"`
	Purity   string  `json:"purity"`
	WeightG  float64 `json:"weightG"`
	Price    float64 `json:"price"`
	Status   string  `json:"status"`
	Location string  `json:"location,omitempty"` // v0.18：总库 或 分销商名
}

// DraftLine 草稿明细行（存进 doc.draft_lines 的 JSON 结构，字段名与前端一致）
type DraftLine struct {
	Barcode string  `json:"barcode"`
	Name    string  `json:"name"`
	Purity  string  `json:"purity"`
	WeightG float64 `json:"weightG"`
	Price   float64 `json:"price"` // 售价(标签价)，0=未定价
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
		if l.Price < 0 {
			return nil, fmt.Errorf("第%d行售价不能为负数", i+1)
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

// GET /api/me —— 恢复登录态用：客户端启动时拿本地保存的令牌来问"我是谁"（v0.13）。
// 能走到这里说明 withAuth 已验过：令牌有效 + 账号未禁用。
func handleMe(w http.ResponseWriter, r *http.Request) {
	uid := currentUID(r)
	var username, name string
	if err := db.QueryRow(`SELECT username, name FROM app_user WHERE id=$1`, uid).Scan(&username, &name); err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"username": username, "name": name, "isAdmin": currentIsAdmin(r)})
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


// ===== 基础资料字典（v0.6） =====
// 三个字典（大类/成色/类别）共用一张表、一套接口——与单据引擎同源的复用思想。
// 读取：所有登录用户；新增/修改：仅管理员。只停用不删除。

var validDictTypes = map[string]bool{"category": true, "purity": true, "jewel_type": true, "pay_method": true}

// GET /api/dict?type=category —— 读取某字典（含停用项，前端下拉自行过滤 enabled）
func handleDictList(w http.ResponseWriter, r *http.Request) {
	dt := r.URL.Query().Get("type")
	if !validDictTypes[dt] {
		writeErr(w, 400, "未知字典类型")
		return
	}
	rows, err := db.Query(`SELECT id, name, sort, enabled
		FROM dict_item WHERE dict_type=$1 ORDER BY sort, id`, dt)
	if err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	defer rows.Close()
	type D struct {
		ID      int64  `json:"id"`
		Name    string `json:"name"`
		Sort    int    `json:"sort"`
		Enabled bool   `json:"enabled"`
	}
	list := []D{}
	for rows.Next() {
		var d D
		if err := rows.Scan(&d.ID, &d.Name, &d.Sort, &d.Enabled); err != nil {
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		list = append(list, d)
	}
	writeJSON(w, 200, map[string]any{"list": list})
}

// POST /api/dict —— 新增字典项（管理员）
func handleDictCreate(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req struct {
		DictType string `json:"dictType"`
		Name     string `json:"name"`
		Sort     int    `json:"sort"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数格式错误")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if !validDictTypes[req.DictType] {
		writeErr(w, 400, "未知字典类型")
		return
	}
	if req.Name == "" || len([]rune(req.Name)) > 20 {
		writeErr(w, 400, "名称不能为空且不超过20字")
		return
	}
	var id int64
	err := db.QueryRow(`INSERT INTO dict_item (dict_type, name, sort)
		VALUES ($1,$2,$3) RETURNING id`, req.DictType, req.Name, req.Sort).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			writeErr(w, 400, "该名称已存在")
			return
		}
		writeErr(w, 500, "创建失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id})
}

// POST /api/dict/update —— 修改排序/停用启用/销售模式（管理员；不支持改名，历史数据引用着名字）
func handleDictUpdate(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req struct {
		ID      int64 `json:"id"`
		Sort    *int  `json:"sort"`
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == 0 {
		writeErr(w, 400, "参数错误")
		return
	}
	if req.Sort != nil {
		if _, err := db.Exec(`UPDATE dict_item SET sort=$1 WHERE id=$2`, *req.Sort, req.ID); err != nil {
			writeErr(w, 500, "更新失败: "+err.Error())
			return
		}
	}
	if req.Enabled != nil {
		if _, err := db.Exec(`UPDATE dict_item SET enabled=$1 WHERE id=$2`, *req.Enabled, req.ID); err != nil {
			writeErr(w, 500, "更新失败: "+err.Error())
			return
		}
	}
	writeJSON(w, 200, map[string]any{"id": req.ID})
}

// ===== 售货员档案（v0.14） =====
// 独立表而非字典：以后要挂提成比例/工号/门店等字段。
// 单据按 id 引用（非名字）——所以改名安全，这点与字典相反。

// GET /api/salespersons —— 所有登录用户可读（开单下拉要用；前端自行过滤 enabled）
func handleSalespersonList(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`SELECT id, name, sort, enabled FROM salesperson ORDER BY sort, id`)
	if err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	defer rows.Close()
	type S struct {
		ID      int64  `json:"id"`
		Name    string `json:"name"`
		Sort    int    `json:"sort"`
		Enabled bool   `json:"enabled"`
	}
	list := []S{}
	for rows.Next() {
		var s S
		if err := rows.Scan(&s.ID, &s.Name, &s.Sort, &s.Enabled); err != nil {
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		list = append(list, s)
	}
	writeJSON(w, 200, map[string]any{"list": list})
}

// POST /api/salespersons —— 新增（管理员）
func handleSalespersonCreate(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req struct {
		Name string `json:"name"`
		Sort int    `json:"sort"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数格式错误")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len([]rune(req.Name)) > 20 {
		writeErr(w, 400, "姓名不能为空且不超过20字")
		return
	}
	var id int64
	err := db.QueryRow(`INSERT INTO salesperson (name, sort) VALUES ($1,$2) RETURNING id`,
		req.Name, req.Sort).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			writeErr(w, 400, "该姓名已存在")
			return
		}
		writeErr(w, 500, "创建失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id})
}

// POST /api/salespersons/update —— 改名/排序/停用启用（管理员）
func handleSalespersonUpdate(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req struct {
		ID      int64   `json:"id"`
		Name    *string `json:"name"`
		Sort    *int    `json:"sort"`
		Enabled *bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == 0 {
		writeErr(w, 400, "参数错误")
		return
	}
	if req.Name != nil {
		n := strings.TrimSpace(*req.Name)
		if n == "" || len([]rune(n)) > 20 {
			writeErr(w, 400, "姓名不能为空且不超过20字")
			return
		}
		if _, err := db.Exec(`UPDATE salesperson SET name=$1 WHERE id=$2`, n, req.ID); err != nil {
			if strings.Contains(err.Error(), "duplicate key") {
				writeErr(w, 400, "该姓名已存在")
				return
			}
			writeErr(w, 500, "更新失败: "+err.Error())
			return
		}
	}
	if req.Sort != nil {
		if _, err := db.Exec(`UPDATE salesperson SET sort=$1 WHERE id=$2`, *req.Sort, req.ID); err != nil {
			writeErr(w, 500, "更新失败: "+err.Error())
			return
		}
	}
	if req.Enabled != nil {
		if _, err := db.Exec(`UPDATE salesperson SET enabled=$1 WHERE id=$2`, *req.Enabled, req.ID); err != nil {
			writeErr(w, 500, "更新失败: "+err.Error())
			return
		}
	}
	writeJSON(w, 200, map[string]any{"id": req.ID})
}

// ===== 首饰退库单（v0.7）—— 按"新模块checklist"添加的第一个单据 =====
// 业务：把总库在库的件退回供应商。件状态：在库 →(确认)→ 已退库 →(反确认)→ 在库。
// 并发裁决复用同一模式：确认时对每件做条件更新 WHERE status='在库'，
// 若某件已被其他单据占用，整单回滚并指明条码。

// POST /api/doc/outbound/save  {id?, supplier, barcodes[]}
func handleOutboundSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       int64    `json:"id"`
		Supplier string   `json:"supplier"`
		Barcodes []string `json:"barcodes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Barcodes) == 0 {
		writeErr(w, 400, "参数错误：至少要有一个条码")
		return
	}
	// 规范化+去重+逐个校验（必须存在且在库），同时取快照用于草稿展示
	seen := map[string]bool{}
	lines := []DraftLine{}
	for i, raw := range req.Barcodes {
		bc := strings.ToUpper(strings.TrimSpace(raw))
		if !validBarcode(bc) {
			writeErr(w, 400, fmt.Sprintf("第%d个条码 %s 格式非法", i+1, bc))
			return
		}
		if seen[bc] {
			writeErr(w, 400, fmt.Sprintf("条码 %s 重复", bc))
			return
		}
		seen[bc] = true
		lines = append(lines, DraftLine{Barcode: bc})
	}

	// v0.16 草稿即占用（与销售单同模式）：退库草稿里的件 在库→退库中。
	// 解锁+上锁+存单一个事务。
	tx, err := db.Begin()
	if err != nil {
		writeErr(w, 500, "开启事务失败: "+err.Error())
		return
	}
	defer tx.Rollback()

	if req.ID != 0 {
		var old []byte
		err := tx.QueryRow(`SELECT draft_lines FROM doc
			WHERE id=$1 AND doc_type='outbound' AND status='草稿' FOR UPDATE`, req.ID).Scan(&old)
		if err == sql.ErrNoRows {
			writeErr(w, 400, "该单据不是草稿状态（可能已被确认或删除），请刷新")
			return
		}
		if err != nil {
			writeErr(w, 500, "读取单据失败: "+err.Error())
			return
		}
		var oldLines []DraftLine
		_ = json.Unmarshal(old, &oldLines)
		for _, ol := range oldLines {
			if _, err := tx.Exec(`UPDATE item SET status='在库', version=version+1
				WHERE barcode=$1 AND status='退库中'`, ol.Barcode); err != nil {
				writeErr(w, 500, "解锁货品失败: "+err.Error())
				return
			}
		}
	}

	for i := range lines {
		l := &lines[i]
		err := tx.QueryRow(`SELECT name, purity, weight_g, price FROM item WHERE barcode=$1`, l.Barcode).
			Scan(&l.Name, &l.Purity, &l.WeightG, &l.Price)
		if err == sql.ErrNoRows {
			writeErr(w, 400, fmt.Sprintf("条码 %s 不存在", l.Barcode))
			return
		}
		if err != nil {
			writeErr(w, 500, "查询失败: "+err.Error())
			return
		}
		// v0.18：退供应商只能退总库的货（分销商处的先调拨回总库）
		res, err := tx.Exec(`UPDATE item SET status='退库中', version=version+1
			WHERE barcode=$1 AND status='在库' AND location_type='总库'`, l.Barcode)
		if err != nil {
			writeErr(w, 500, "锁定货品失败: "+err.Error())
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var cur string
			_ = tx.QueryRow(`SELECT status FROM item WHERE barcode=$1`, l.Barcode).Scan(&cur)
			if cur == "在库" {
				writeErr(w, 400, fmt.Sprintf("条码 %s 在「%s」处——退供应商只能退总库的货，请先调拨回总库", l.Barcode, itemLocName(tx, l.Barcode)))
			} else {
				writeErr(w, 400, fmt.Sprintf("条码 %s 当前状态「%s」（可能已被其他单据占用），整单未保存", l.Barcode, cur))
			}
			return
		}
	}
	linesJSON, _ := json.Marshal(lines)

	if req.ID == 0 {
		today := time.Now().Format("20060102")
		seq, err := nextSeq(tx, "TK", today)
		if err != nil {
			writeErr(w, 500, "单号发号失败: "+err.Error())
			return
		}
		if seq > 99 {
			writeErr(w, 400, "当日退库单号已满99张")
			return
		}
		docNo := fmt.Sprintf("TK%s%02d", today, seq)
		var docID int64
		err = tx.QueryRow(`INSERT INTO doc (doc_no, doc_type, status, supplier, maker_id, draft_lines)
			VALUES ($1, 'outbound', '草稿', $2, $3, $4) RETURNING id`,
			docNo, strings.TrimSpace(req.Supplier), currentUID(r), linesJSON).Scan(&docID)
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
	if _, err := tx.Exec(`UPDATE doc SET supplier=$1, draft_lines=$2 WHERE id=$3`,
		strings.TrimSpace(req.Supplier), linesJSON, req.ID); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "status": "草稿"})
}

// POST /api/doc/outbound/confirm {id}
func handleOutboundConfirm(w http.ResponseWriter, r *http.Request) {
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

	res, err := tx.Exec(`UPDATE doc SET status='已确认', confirmed_at=now()
		WHERE id=$1 AND doc_type='outbound' AND status='草稿'`, req.ID)
	if err != nil {
		writeErr(w, 500, "确认失败: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 400, "确认失败：单据不存在或已被他人确认，请刷新")
		return
	}
	var linesJSON []byte
	if err := tx.QueryRow(`SELECT draft_lines FROM doc WHERE id=$1`, req.ID).Scan(&linesJSON); err != nil {
		writeErr(w, 500, "读取单据失败: "+err.Error())
		return
	}
	var lines []DraftLine
	if err := json.Unmarshal(linesJSON, &lines); err != nil || len(lines) == 0 {
		writeErr(w, 400, "草稿明细为空，无法确认")
		return
	}
	for i, l := range lines {
		// v0.16：正常流程里件已在挂草稿时变"退库中"，这里 退库中→已退库；
		// 兼容老草稿（件还是在库）。FOR UPDATE 锁行读状态，真实来源状态记进流水。
		var itemID int64
		var fromSt string
		err := tx.QueryRow(`SELECT id, status FROM item WHERE barcode=$1 FOR UPDATE`, l.Barcode).
			Scan(&itemID, &fromSt)
		if err == sql.ErrNoRows {
			writeErr(w, 400, fmt.Sprintf("第%d行条码 %s 不存在，整单未确认", i+1, l.Barcode))
			return
		}
		if err != nil {
			writeErr(w, 500, "查询货品失败: "+err.Error())
			return
		}
		if fromSt != "退库中" && fromSt != "在库" {
			writeErr(w, 400, fmt.Sprintf("第%d行条码 %s 当前状态「%s」，整单未确认", i+1, l.Barcode, fromSt))
			return
		}
		if _, err := tx.Exec(`UPDATE item SET status='已退库', version=version+1 WHERE id=$1`, itemID); err != nil {
			writeErr(w, 500, "更新货品失败: "+err.Error())
			return
		}
		if _, err = tx.Exec(`INSERT INTO doc_line (doc_id, item_id, line_no) VALUES ($1,$2,$3)`,
			req.ID, itemID, i+1); err != nil {
			writeErr(w, 500, "写入明细失败: "+err.Error())
			return
		}
		if _, err = tx.Exec(`INSERT INTO stock_flow (item_id, doc_id, from_status, to_status, from_loc, operator_id)
			VALUES ($1,$2,$3,'已退库','总库',$4)`, itemID, req.ID, fromSt, currentUID(r)); err != nil {
			writeErr(w, 500, "写入流水失败: "+err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "status": "已确认"})
}

// POST /api/doc/outbound/unconfirm {id} —— 件从已退库回到在库
func handleOutboundUnconfirm(w http.ResponseWriter, r *http.Request) {
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
	res, err := tx.Exec(`UPDATE doc SET status='草稿', confirmed_at=NULL
		WHERE id=$1 AND doc_type='outbound' AND status='已确认'`, req.ID)
	if err != nil {
		writeErr(w, 500, "反确认失败: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 400, "反确认失败：单据不存在或不是已确认状态，请刷新")
		return
	}
	rows, err := tx.Query(`SELECT it.id, it.barcode FROM doc_line dl
		JOIN item it ON it.id = dl.item_id WHERE dl.doc_id=$1 ORDER BY dl.line_no`, req.ID)
	if err != nil {
		writeErr(w, 500, "明细查询失败: "+err.Error())
		return
	}
	type ref struct {
		id      int64
		barcode string
	}
	var refs []ref
	for rows.Next() {
		var rf ref
		if err := rows.Scan(&rf.id, &rf.barcode); err != nil {
			rows.Close()
			writeErr(w, 500, "明细读取失败: "+err.Error())
			return
		}
		refs = append(refs, rf)
	}
	rows.Close()
	for _, rf := range refs {
		// v0.16（用户发现并提议）：反确认后单据回到草稿、草稿仍占着这些件——
		// 所以是 已退库→退库中（不是在库！否则别的单能把草稿里的货抢走）。
		res, err := tx.Exec(`UPDATE item SET status='退库中', version=version+1
			WHERE id=$1 AND status='已退库'`, rf.id)
		if err != nil {
			writeErr(w, 500, "恢复货品失败: "+err.Error())
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeErr(w, 400, fmt.Sprintf("反确认被拒绝：条码 %s 状态异常", rf.barcode))
			return
		}
		if _, err = tx.Exec(`INSERT INTO stock_flow (item_id, doc_id, from_status, to_status, to_loc, operator_id)
			VALUES ($1,$2,'已退库','退库中','总库',$3)`, rf.id, req.ID, currentUID(r)); err != nil {
			writeErr(w, 500, "写入流水失败: "+err.Error())
			return
		}
	}
	if _, err = tx.Exec(`DELETE FROM doc_line WHERE doc_id=$1`, req.ID); err != nil {
		writeErr(w, 500, "删除明细失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "status": "草稿"})
}

// POST /api/doc/outbound/delete {id} —— v0.16：删草稿同时解锁它占用的货品
func handleOutboundDelete(w http.ResponseWriter, r *http.Request) {
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
	var old []byte
	err = tx.QueryRow(`SELECT draft_lines FROM doc
		WHERE id=$1 AND doc_type='outbound' AND status='草稿' FOR UPDATE`, req.ID).Scan(&old)
	if err == sql.ErrNoRows {
		writeErr(w, 400, "删除失败：只有草稿可以删除（已确认的请先反确认）")
		return
	}
	if err != nil {
		writeErr(w, 500, "读取单据失败: "+err.Error())
		return
	}
	var oldLines []DraftLine
	_ = json.Unmarshal(old, &oldLines)
	for _, ol := range oldLines {
		if _, err := tx.Exec(`UPDATE item SET status='在库', version=version+1
			WHERE barcode=$1 AND status='退库中'`, ol.Barcode); err != nil {
			writeErr(w, 500, "解锁货品失败: "+err.Error())
			return
		}
	}
	if _, err := tx.Exec(`DELETE FROM doc WHERE id=$1`, req.ID); err != nil {
		writeErr(w, 500, "删除失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": req.ID})
}

// GET /api/doc/outbound —— 退库单列表
func handleOutboundList(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`SELECT id, doc_no, COALESCE(supplier,''), status, draft_lines,
		to_char(created_at,'YYYY-MM-DD HH24:MI:SS')
		FROM doc WHERE doc_type='outbound' ORDER BY id DESC LIMIT 20`)
	if err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	defer rows.Close()
	type ODoc struct {
		ID       int64  `json:"id"`
		DocNo    string `json:"docNo"`
		Supplier string `json:"supplier"`
		Status   string `json:"status"`
		Items    []Item `json:"items"`
		MadeAt   string `json:"madeAt"`
	}
	docs := []ODoc{}
	drafts := map[int64][]byte{}
	for rows.Next() {
		var d ODoc
		var dl []byte
		if err := rows.Scan(&d.ID, &d.DocNo, &d.Supplier, &d.Status, &dl, &d.MadeAt); err != nil {
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		d.Items = []Item{}
		drafts[d.ID] = dl
		docs = append(docs, d)
	}
	for i := range docs {
		var lines []DraftLine
		_ = json.Unmarshal(drafts[docs[i].ID], &lines)
		for _, l := range lines {
			docs[i].Items = append(docs[i].Items, Item{Barcode: l.Barcode, Name: l.Name,
				Purity: l.Purity, WeightG: l.WeightG, Price: l.Price, Status: docs[i].Status})
		}
	}
	writeJSON(w, 200, map[string]any{"list": docs, "total": len(docs)})
}

// ===== 金价发布（v0.10） =====
// 只追加不修改：当前价=每成色最新一条；历史可查；销售时将快照进单据。

// GET /api/gold-price/current —— 每个成色的最新价（所有登录用户，开单要用）
func handleGoldPriceCurrent(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`
		SELECT DISTINCT ON (gp.purity) gp.purity, gp.retail_price, gp.recycle_price,
		       u.name, to_char(gp.published_at,'MM-DD HH24:MI')
		FROM gold_price gp JOIN app_user u ON u.id = gp.published_by
		ORDER BY gp.purity, gp.id DESC`)
	if err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	defer rows.Close()
	type P struct {
		Purity       string  `json:"purity"`
		RetailPrice  float64 `json:"retailPrice"`
		RecyclePrice float64 `json:"recyclePrice"`
		By           string  `json:"by"`
		At           string  `json:"at"`
	}
	list := []P{}
	for rows.Next() {
		var x P
		if err := rows.Scan(&x.Purity, &x.RetailPrice, &x.RecyclePrice, &x.By, &x.At); err != nil {
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		list = append(list, x)
	}
	writeJSON(w, 200, map[string]any{"list": list})
}

// GET /api/gold-price/history —— 最近50条发布记录
func handleGoldPriceHistory(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`
		SELECT gp.purity, gp.retail_price, gp.recycle_price, u.name,
		       to_char(gp.published_at,'YYYY-MM-DD HH24:MI:SS')
		FROM gold_price gp JOIN app_user u ON u.id = gp.published_by
		ORDER BY gp.id DESC LIMIT 50`)
	if err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	defer rows.Close()
	type P struct {
		Purity       string  `json:"purity"`
		RetailPrice  float64 `json:"retailPrice"`
		RecyclePrice float64 `json:"recyclePrice"`
		By           string  `json:"by"`
		At           string  `json:"at"`
	}
	list := []P{}
	for rows.Next() {
		var x P
		if err := rows.Scan(&x.Purity, &x.RetailPrice, &x.RecyclePrice, &x.By, &x.At); err != nil {
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		list = append(list, x)
	}
	writeJSON(w, 200, map[string]any{"list": list})
}

// POST /api/gold-price —— 发布新价（管理员）：{purity, retailPrice, recyclePrice}
func handleGoldPricePublish(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req struct {
		Purity       string  `json:"purity"`
		RetailPrice  float64 `json:"retailPrice"`
		RecyclePrice float64 `json:"recyclePrice"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数格式错误")
		return
	}
	req.Purity = strings.TrimSpace(req.Purity)
	var ok bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM dict_item
		WHERE dict_type='purity' AND name=$1 AND enabled=true)`, req.Purity).Scan(&ok); err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	if !ok {
		writeErr(w, 400, "成色「"+req.Purity+"」不存在或已停用")
		return
	}
	if req.RetailPrice <= 0 {
		writeErr(w, 400, "零售金价必须大于0")
		return
	}
	if req.RecyclePrice < 0 {
		writeErr(w, 400, "回收金价不能为负")
		return
	}
	if req.RecyclePrice > req.RetailPrice {
		writeErr(w, 400, "回收金价不应高于零售金价，请核对")
		return
	}
	var id int64
	if err := db.QueryRow(`INSERT INTO gold_price (purity, retail_price, recycle_price, published_by)
		VALUES ($1,$2,$3,$4) RETURNING id`,
		req.Purity, req.RetailPrice, req.RecyclePrice, currentUID(r)).Scan(&id); err != nil {
		writeErr(w, 500, "发布失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id})
}

// ===== 首饰销售单（v0.11 第一轮） =====
// 核心：扫码加货(仅在库) → 逐件选结算方式：
//   标签价 = 读货品售价(须>0)；变金价 = 克重 × 该成色当日零售金价(须已发布)。
// 实售价可在建议价基础上修改(议价)。确认时：条件更新 在库→已售、
// 把当时各成色金价和每行成交参数快照进单据、算合计。反确认仅限当日。

// SaleLine 销售明细（存 draft_lines）
type SaleLine struct {
	Barcode   string  `json:"barcode"`
	Name      string  `json:"name"`
	Purity    string  `json:"purity"`
	WeightG   float64 `json:"weightG"`
	Price     float64 `json:"price"`     // 标签价快照(加入时)
	Mode      string  `json:"mode"`      // 标签价 / 变金价
	SoldPrice float64 `json:"soldPrice"` // 实售价(可议价)
}

// currentGoldPrice 查某成色当前零售金价；无发布记录返回 ok=false
func currentGoldPrice(purity string) (float64, bool, error) {
	var rate float64
	err := db.QueryRow(`SELECT retail_price FROM gold_price WHERE purity=$1
		ORDER BY id DESC LIMIT 1`, purity).Scan(&rate)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	return rate, err == nil, err
}

// PayLine 一笔收款：方式+金额。组合收款 = 多笔，确认时校验合计=应收。
type PayLine struct {
	Method string  `json:"method"`
	Amount float64 `json:"amount"`
}

// 收款明细的通用校验（草稿与确认共用）：方式不重复、金额>0
func validatePayments(pays []PayLine) error {
	seen := map[string]bool{}
	for i, p := range pays {
		if strings.TrimSpace(p.Method) == "" {
			return fmt.Errorf("第%d笔收款未选方式", i+1)
		}
		if seen[p.Method] {
			return fmt.Errorf("收款方式「%s」重复——同一方式请合并成一笔", p.Method)
		}
		seen[p.Method] = true
		if p.Amount <= 0 {
			return fmt.Errorf("收款「%s」金额必须大于0", p.Method)
		}
	}
	return nil
}

// POST /api/doc/sale/save  {id?, salespersonId?, payments?, lines:[{barcode,mode,soldPrice}]}
func handleSaleSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID            int64     `json:"id"`
		SalespersonID int64     `json:"salespersonId"`
		Payments      []PayLine `json:"payments"`
		Lines         []struct {
			Barcode   string  `json:"barcode"`
			Mode      string  `json:"mode"`
			SoldPrice float64 `json:"soldPrice"`
		} `json:"lines"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Lines) == 0 {
		writeErr(w, 400, "参数错误：至少要有一行")
		return
	}
	// 草稿阶段的宽松校验：售货员/收款可以先不填（挂单常在收款前），
	// 但填了就不能是明显错的。收款合计=应收 的硬校验放在确认时。
	if err := validatePayments(req.Payments); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	seen := map[string]bool{}
	lines := []SaleLine{}
	for i, l := range req.Lines {
		bc := strings.ToUpper(strings.TrimSpace(l.Barcode))
		if !validBarcode(bc) {
			writeErr(w, 400, fmt.Sprintf("第%d行条码 %s 格式非法", i+1, bc))
			return
		}
		if seen[bc] {
			writeErr(w, 400, fmt.Sprintf("条码 %s 重复", bc))
			return
		}
		seen[bc] = true
		if l.Mode != "标签价" && l.Mode != "变金价" {
			writeErr(w, 400, fmt.Sprintf("第%d行结算方式必须是 标签价 或 变金价", i+1))
			return
		}
		if l.SoldPrice <= 0 {
			writeErr(w, 400, fmt.Sprintf("条码 %s 实售价必须大于0", bc))
			return
		}
		lines = append(lines, SaleLine{Barcode: bc, Mode: l.Mode, SoldPrice: l.SoldPrice})
	}
	if req.Payments == nil {
		req.Payments = []PayLine{}
	}
	paysJSON, _ := json.Marshal(req.Payments)
	// salesperson_id 可空：0 表示未选，存 NULL（外键列不能存0——没有id为0的售货员）
	var spID any
	if req.SalespersonID > 0 {
		spID = req.SalespersonID
	}

	// v0.15 挂单即锁定：保存草稿这一刻就把货品 在库→销售中，并发裁决从"确认"提前到"挂单"。
	// 解锁+锁定+存单在同一个事务里——要么全成，要么全不动。
	tx, err := db.Begin()
	if err != nil {
		writeErr(w, 500, "开启事务失败: "+err.Error())
		return
	}
	defer tx.Rollback()

	// 编辑已有草稿：先锁住单据行并取旧明细，把旧明细占用的货品全部解锁。
	// （从草稿里移出的件由此回到在库；留在草稿里的件马上会被重新锁定——净效果不变）
	if req.ID != 0 {
		var old []byte
		err := tx.QueryRow(`SELECT draft_lines FROM doc
			WHERE id=$1 AND doc_type='sale' AND status='草稿' FOR UPDATE`, req.ID).Scan(&old)
		if err == sql.ErrNoRows {
			writeErr(w, 400, "该单据不是草稿状态（可能已被确认或删除），请刷新")
			return
		}
		if err != nil {
			writeErr(w, 500, "读取单据失败: "+err.Error())
			return
		}
		var oldLines []SaleLine
		_ = json.Unmarshal(old, &oldLines)
		for _, ol := range oldLines {
			// WHERE status='销售中'：v0.14 之前的老草稿里货还是"在库"，解锁自然空转，无需特判
			if _, err := tx.Exec(`UPDATE item SET status='在库', version=version+1
				WHERE barcode=$1 AND status='销售中'`, ol.Barcode); err != nil {
				writeErr(w, 500, "解锁货品失败: "+err.Error())
				return
			}
		}
	}

	// 逐行：读取货品资料 → 计价规则校验 → 条件更新锁定（抢不到就整单失败）
	for i := range lines {
		sl := &lines[i]
		err := tx.QueryRow(`SELECT name, purity, weight_g, price FROM item WHERE barcode=$1`, sl.Barcode).
			Scan(&sl.Name, &sl.Purity, &sl.WeightG, &sl.Price)
		if err == sql.ErrNoRows {
			writeErr(w, 400, fmt.Sprintf("条码 %s 不存在", sl.Barcode))
			return
		}
		if err != nil {
			writeErr(w, 500, "查询失败: "+err.Error())
			return
		}
		switch sl.Mode {
		case "标签价":
			if sl.Price <= 0 {
				writeErr(w, 400, fmt.Sprintf("条码 %s 未定标签价，不能按标签价销售", sl.Barcode))
				return
			}
		case "变金价":
			rate, ok, e := currentGoldPriceTx(tx, sl.Purity)
			if e != nil {
				writeErr(w, 500, "金价查询失败: "+e.Error())
				return
			}
			if !ok || rate <= 0 {
				writeErr(w, 400, fmt.Sprintf("成色「%s」今日未发布金价，不能按变金价销售", sl.Purity))
				return
			}
		}
		// v0.18：销售开单只能卖总库的货（门店销售待"账号绑门店"后放开）
		res, err := tx.Exec(`UPDATE item SET status='销售中', version=version+1
			WHERE barcode=$1 AND status='在库' AND location_type='总库'`, sl.Barcode)
		if err != nil {
			writeErr(w, 500, "锁定货品失败: "+err.Error())
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var cur string
			_ = tx.QueryRow(`SELECT status FROM item WHERE barcode=$1`, sl.Barcode).Scan(&cur)
			if cur == "在库" {
				writeErr(w, 400, fmt.Sprintf("条码 %s 在「%s」处——销售开单目前只能卖总库的货，请先调拨回总库", sl.Barcode, itemLocName(tx, sl.Barcode)))
			} else {
				writeErr(w, 400, fmt.Sprintf("条码 %s 当前状态「%s」（可能已被其他柜台挂单或售出），整单未保存", sl.Barcode, cur))
			}
			return
		}
	}
	linesJSON, _ := json.Marshal(lines)

	if req.ID == 0 {
		today := time.Now().Format("20060102")
		seq, err := nextSeq(tx, "XS", today)
		if err != nil {
			writeErr(w, 500, "单号发号失败: "+err.Error())
			return
		}
		if seq > 99 {
			writeErr(w, 400, "当日销售单号已满99张")
			return
		}
		docNo := fmt.Sprintf("XS%s%02d", today, seq)
		var docID int64
		err = tx.QueryRow(`INSERT INTO doc (doc_no, doc_type, status, maker_id, draft_lines, salesperson_id, payments)
			VALUES ($1,'sale','草稿',$2,$3,$4,$5) RETURNING id`, docNo, currentUID(r), linesJSON, spID, paysJSON).Scan(&docID)
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
	if _, err := tx.Exec(`UPDATE doc SET draft_lines=$1, salesperson_id=$2, payments=$3
		WHERE id=$4`, linesJSON, spID, paysJSON, req.ID); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "status": "草稿"})
}

// POST /api/doc/sale/confirm {id} —— 收款确认：售出+快照+合计，一个事务
func handleSaleConfirm(w http.ResponseWriter, r *http.Request) {
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

	res, err := tx.Exec(`UPDATE doc SET status='已确认', confirmed_at=now()
		WHERE id=$1 AND doc_type='sale' AND status='草稿'`, req.ID)
	if err != nil {
		writeErr(w, 500, "确认失败: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 400, "确认失败：单据不存在或已被他人确认，请刷新")
		return
	}
	var linesJSON, paysJSON []byte
	var spID sql.NullInt64
	if err := tx.QueryRow(`SELECT draft_lines, salesperson_id, payments FROM doc WHERE id=$1`,
		req.ID).Scan(&linesJSON, &spID, &paysJSON); err != nil {
		writeErr(w, 500, "读取单据失败: "+err.Error())
		return
	}
	var lines []SaleLine
	if err := json.Unmarshal(linesJSON, &lines); err != nil || len(lines) == 0 {
		writeErr(w, 400, "草稿明细为空，无法确认")
		return
	}

	// 确认时硬校验一：售货员必填且在职启用（v0.14）
	if !spID.Valid {
		writeErr(w, 400, "请先选择售货员再确认收款")
		return
	}
	var spEnabled bool
	if err := tx.QueryRow(`SELECT enabled FROM salesperson WHERE id=$1`, spID.Int64).Scan(&spEnabled); err != nil {
		writeErr(w, 400, "售货员不存在，请重新选择")
		return
	}
	if !spEnabled {
		writeErr(w, 400, "该售货员已停用，请重新选择")
		return
	}
	// 确认时硬校验二：收款明细合法，且每种方式都是启用的字典项
	var pays []PayLine
	_ = json.Unmarshal(paysJSON, &pays)
	if len(pays) == 0 {
		writeErr(w, 400, "请先录入收款方式再确认收款")
		return
	}
	if err := validatePayments(pays); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	for _, p := range pays {
		var en bool
		err := tx.QueryRow(`SELECT enabled FROM dict_item WHERE dict_type='pay_method' AND name=$1`,
			p.Method).Scan(&en)
		if err != nil || !en {
			writeErr(w, 400, fmt.Sprintf("收款方式「%s」不存在或已停用", p.Method))
			return
		}
	}

	rates := map[string]float64{} // 本单用到的各成色确认时金价
	type snapLine struct {
		Barcode   string  `json:"barcode"`
		Mode      string  `json:"mode"`
		WeightG   float64 `json:"weightG"`
		LabelPrice float64 `json:"labelPrice"`
		GoldRate  float64 `json:"goldRate"`  // 变金价行：确认时金价
		RefPrice  float64 `json:"refPrice"`  // 系统参考价
		SoldPrice float64 `json:"soldPrice"` // 实际成交价
	}
	snaps := []snapLine{}
	var total float64
	for i, l := range lines {
		// v0.15：正常流程里件已在挂单时锁定，这里 销售中→已售；
		// 兼容 v0.14 之前保存的老草稿（件还是在库），所以两种来源状态都接受。
		// FOR UPDATE 锁行后读状态再改——既是并发裁决，又能把真实的来源状态记进流水。
		var itemID int64
		var fromSt string
		err := tx.QueryRow(`SELECT id, status FROM item WHERE barcode=$1 FOR UPDATE`, l.Barcode).
			Scan(&itemID, &fromSt)
		if err == sql.ErrNoRows {
			writeErr(w, 400, fmt.Sprintf("第%d行条码 %s 不存在，整单未确认", i+1, l.Barcode))
			return
		}
		if err != nil {
			writeErr(w, 500, "查询货品失败: "+err.Error())
			return
		}
		if fromSt != "销售中" && fromSt != "在库" {
			writeErr(w, 400, fmt.Sprintf("第%d行条码 %s 当前状态「%s」（可能刚被其他柜台售出），整单未确认", i+1, l.Barcode, fromSt))
			return
		}
		if _, err := tx.Exec(`UPDATE item SET status='已售', version=version+1 WHERE id=$1`, itemID); err != nil {
			writeErr(w, 500, "更新货品失败: "+err.Error())
			return
		}
		sp := snapLine{Barcode: l.Barcode, Mode: l.Mode, WeightG: l.WeightG,
			LabelPrice: l.Price, SoldPrice: l.SoldPrice}
		if l.Mode == "变金价" {
			rate, ok := rates[l.Purity]
			if !ok {
				var e error
				rate, ok, e = currentGoldPriceTx(tx, l.Purity)
				if e != nil {
					writeErr(w, 500, "金价查询失败: "+e.Error())
					return
				}
				if !ok || rate <= 0 {
					writeErr(w, 400, fmt.Sprintf("成色「%s」今日未发布金价，整单未确认", l.Purity))
					return
				}
				rates[l.Purity] = rate
			}
			sp.GoldRate = rate
			sp.RefPrice = round2(l.WeightG * rate)
		} else {
			sp.RefPrice = l.Price
		}
		snaps = append(snaps, sp)
		total += l.SoldPrice
		if _, err = tx.Exec(`INSERT INTO doc_line (doc_id, item_id, line_no) VALUES ($1,$2,$3)`,
			req.ID, itemID, i+1); err != nil {
			writeErr(w, 500, "写入明细失败: "+err.Error())
			return
		}
		if _, err = tx.Exec(`INSERT INTO stock_flow (item_id, doc_id, from_status, to_status, from_loc, operator_id)
			VALUES ($1,$2,$3,'已售','总库',$4)`, itemID, req.ID, fromSt, currentUID(r)); err != nil {
			writeErr(w, 500, "写入流水失败: "+err.Error())
			return
		}
	}
	// 确认时硬校验三：收款合计必须分毫不差等于应收合计
	var paySum float64
	for _, p := range pays {
		paySum += p.Amount
	}
	if round2(paySum) != round2(total) {
		writeErr(w, 400, fmt.Sprintf("收款合计 ¥%.2f 与应收合计 ¥%.2f 不一致，整单未确认", paySum, total))
		return
	}

	snapshot, _ := json.Marshal(map[string]any{"rates": rates, "lines": snaps})
	if _, err = tx.Exec(`UPDATE doc SET total_amount=$1, gold_price_snapshot=$2 WHERE id=$3`,
		round2(total), snapshot, req.ID); err != nil {
		writeErr(w, 500, "写入合计失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "status": "已确认", "totalAmount": round2(total)})
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

func currentGoldPriceTx(tx *sql.Tx, purity string) (float64, bool, error) {
	var rate float64
	err := tx.QueryRow(`SELECT retail_price FROM gold_price WHERE purity=$1
		ORDER BY id DESC LIMIT 1`, purity).Scan(&rate)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	return rate, err == nil, err
}

// POST /api/doc/sale/unconfirm {id} —— 仅限当日（隔日请走销退流程）
func handleSaleUnconfirm(w http.ResponseWriter, r *http.Request) {
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
	res, err := tx.Exec(`UPDATE doc SET status='草稿', confirmed_at=NULL,
		total_amount=0, gold_price_snapshot=NULL
		WHERE id=$1 AND doc_type='sale' AND status='已确认'
		AND confirmed_at::date = CURRENT_DATE`, req.ID)
	if err != nil {
		writeErr(w, 500, "反确认失败: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 400, "反确认失败：单据不存在、不是已确认状态，或已跨日（跨日请走销退流程）")
		return
	}
	rows, err := tx.Query(`SELECT it.id, it.barcode FROM doc_line dl
		JOIN item it ON it.id = dl.item_id WHERE dl.doc_id=$1 ORDER BY dl.line_no`, req.ID)
	if err != nil {
		writeErr(w, 500, "明细查询失败: "+err.Error())
		return
	}
	type ref struct {
		id      int64
		barcode string
	}
	var refs []ref
	for rows.Next() {
		var rf ref
		if err := rows.Scan(&rf.id, &rf.barcode); err != nil {
			rows.Close()
			writeErr(w, 500, "明细读取失败: "+err.Error())
			return
		}
		refs = append(refs, rf)
	}
	rows.Close()
	for _, rf := range refs {
		// v0.15：反确认后单据回到草稿、草稿仍占着这些件——所以是 已售→销售中（不是在库）。
		// 想让件回到在库，要么删掉草稿，要么把它从明细里移出再保存。
		res, err := tx.Exec(`UPDATE item SET status='销售中', version=version+1
			WHERE id=$1 AND status='已售'`, rf.id)
		if err != nil {
			writeErr(w, 500, "恢复货品失败: "+err.Error())
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeErr(w, 400, fmt.Sprintf("反确认被拒绝：条码 %s 状态异常", rf.barcode))
			return
		}
		if _, err = tx.Exec(`INSERT INTO stock_flow (item_id, doc_id, from_status, to_status, to_loc, operator_id)
			VALUES ($1,$2,'已售','销售中','总库',$3)`, rf.id, req.ID, currentUID(r)); err != nil {
			writeErr(w, 500, "写入流水失败: "+err.Error())
			return
		}
	}
	if _, err = tx.Exec(`DELETE FROM doc_line WHERE doc_id=$1`, req.ID); err != nil {
		writeErr(w, 500, "删除明细失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "status": "草稿"})
}

// POST /api/doc/sale/delete {id} —— v0.15：删草稿同时解锁它占用的货品
func handleSaleDelete(w http.ResponseWriter, r *http.Request) {
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
	var old []byte
	err = tx.QueryRow(`SELECT draft_lines FROM doc
		WHERE id=$1 AND doc_type='sale' AND status='草稿' FOR UPDATE`, req.ID).Scan(&old)
	if err == sql.ErrNoRows {
		writeErr(w, 400, "删除失败：只有草稿可以删除")
		return
	}
	if err != nil {
		writeErr(w, 500, "读取单据失败: "+err.Error())
		return
	}
	var oldLines []SaleLine
	_ = json.Unmarshal(old, &oldLines)
	for _, ol := range oldLines {
		if _, err := tx.Exec(`UPDATE item SET status='在库', version=version+1
			WHERE barcode=$1 AND status='销售中'`, ol.Barcode); err != nil {
			writeErr(w, 500, "解锁货品失败: "+err.Error())
			return
		}
	}
	if _, err := tx.Exec(`DELETE FROM doc WHERE id=$1`, req.ID); err != nil {
		writeErr(w, 500, "删除失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": req.ID})
}

// GET /api/doc/sale —— 销售单列表
func handleSaleList(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`SELECT d.id, d.doc_no, d.status, d.draft_lines, d.total_amount,
		d.salesperson_id, COALESCE(sp.name,''), d.payments,
		to_char(d.created_at,'YYYY-MM-DD HH24:MI:SS')
		FROM doc d LEFT JOIN salesperson sp ON sp.id = d.salesperson_id
		WHERE d.doc_type='sale' ORDER BY d.id DESC LIMIT 20`)
	if err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	defer rows.Close()
	type SDoc struct {
		ID              int64      `json:"id"`
		DocNo           string     `json:"docNo"`
		Status          string     `json:"status"`
		TotalAmount     float64    `json:"totalAmount"`
		SalespersonID   int64      `json:"salespersonId"`
		SalespersonName string     `json:"salespersonName"`
		Payments        []PayLine  `json:"payments"`
		Lines           []SaleLine `json:"lines"`
		MadeAt          string     `json:"madeAt"`
	}
	docs := []SDoc{}
	for rows.Next() {
		var d SDoc
		var dl, pj []byte
		var spid sql.NullInt64
		if err := rows.Scan(&d.ID, &d.DocNo, &d.Status, &dl, &d.TotalAmount,
			&spid, &d.SalespersonName, &pj, &d.MadeAt); err != nil {
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		d.SalespersonID = spid.Int64
		d.Lines = []SaleLine{}
		_ = json.Unmarshal(dl, &d.Lines)
		d.Payments = []PayLine{}
		_ = json.Unmarshal(pj, &d.Payments)
		docs = append(docs, d)
	}
	writeJSON(w, 200, map[string]any{"list": docs, "total": len(docs)})
}

// ===== 销退单（v0.17）—— 跨日退货的正规出口 =====
// 业务：只收"已售"的件；自动带出原销售单与原成交价；退款默认原价、可下调（扣损耗）
// 但不得超过原成交价；退款走组合退款方式（与收款同一套字典与校验）。
// 件状态：已售 →(挂草稿)→ 退货中 →(确认)→ 在库；反确认 在库→退货中，限当日。

// SRLine 销退明细：原单信息在挂草稿时查定、存进草稿——确认时不再追溯
type SRLine struct {
	Barcode     string  `json:"barcode"`
	Name        string  `json:"name"`
	Purity      string  `json:"purity"`
	WeightG     float64 `json:"weightG"`
	OrigDocID   int64   `json:"origDocId"`
	OrigDocNo   string  `json:"origDocNo"`
	SoldPrice   float64 `json:"soldPrice"`   // 原实际成交价（从原单快照取）
	RefundPrice float64 `json:"refundPrice"` // 本次退款金额
}

// rowQuerier 让同一个查询函数既能用 db 也能用 tx
type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// findOrigSale 找到卖出该条码的最近一张已确认销售单，并从金价快照里取出当时的实售价
func findOrigSale(q rowQuerier, barcode string) (docID int64, docNo string, soldPrice float64, err error) {
	var snap []byte
	err = q.QueryRow(`SELECT d.id, d.doc_no, d.gold_price_snapshot
		FROM doc d
		JOIN doc_line dl ON dl.doc_id = d.id
		JOIN item it ON it.id = dl.item_id
		WHERE it.barcode=$1 AND d.doc_type='sale' AND d.status='已确认'
		ORDER BY d.id DESC LIMIT 1`, barcode).Scan(&docID, &docNo, &snap)
	if err != nil {
		return 0, "", 0, err
	}
	var s struct {
		Lines []struct {
			Barcode   string  `json:"barcode"`
			SoldPrice float64 `json:"soldPrice"`
		} `json:"lines"`
	}
	_ = json.Unmarshal(snap, &s)
	for _, l := range s.Lines {
		if l.Barcode == barcode {
			return docID, docNo, l.SoldPrice, nil
		}
	}
	return docID, docNo, 0, nil
}

// GET /api/sale-return/lookup?barcode=XXX —— 开销退单扫码时带出原单信息
func handleSaleReturnLookup(w http.ResponseWriter, r *http.Request) {
	bc := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("barcode")))
	if !validBarcode(bc) {
		writeErr(w, 400, "条码格式非法")
		return
	}
	var sl SRLine
	var status string
	err := db.QueryRow(`SELECT name, purity, weight_g, status FROM item WHERE barcode=$1`, bc).
		Scan(&sl.Name, &sl.Purity, &sl.WeightG, &status)
	if err == sql.ErrNoRows {
		writeErr(w, 400, fmt.Sprintf("条码 %s 不存在", bc))
		return
	}
	if err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	if status != "已售" {
		writeErr(w, 400, fmt.Sprintf("条码 %s 当前状态「%s」——只有已售的件才能销退", bc, status))
		return
	}
	docID, docNo, soldPrice, err := findOrigSale(db, bc)
	if err == sql.ErrNoRows {
		writeErr(w, 400, fmt.Sprintf("条码 %s 找不到对应的已确认销售单", bc))
		return
	}
	if err != nil {
		writeErr(w, 500, "原单查询失败: "+err.Error())
		return
	}
	sl.Barcode, sl.OrigDocID, sl.OrigDocNo, sl.SoldPrice, sl.RefundPrice = bc, docID, docNo, soldPrice, soldPrice
	writeJSON(w, 200, sl)
}

// POST /api/doc/sale-return/save  {id?, payments?, lines:[{barcode,refundPrice}]}
func handleSaleReturnSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       int64     `json:"id"`
		Payments []PayLine `json:"payments"`
		Lines    []struct {
			Barcode     string  `json:"barcode"`
			RefundPrice float64 `json:"refundPrice"`
		} `json:"lines"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Lines) == 0 {
		writeErr(w, 400, "参数错误：至少要有一行")
		return
	}
	if err := validatePayments(req.Payments); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	seen := map[string]bool{}
	lines := []SRLine{}
	for i, l := range req.Lines {
		bc := strings.ToUpper(strings.TrimSpace(l.Barcode))
		if !validBarcode(bc) {
			writeErr(w, 400, fmt.Sprintf("第%d行条码 %s 格式非法", i+1, bc))
			return
		}
		if seen[bc] {
			writeErr(w, 400, fmt.Sprintf("条码 %s 重复", bc))
			return
		}
		seen[bc] = true
		if l.RefundPrice <= 0 {
			writeErr(w, 400, fmt.Sprintf("条码 %s 退款金额必须大于0", bc))
			return
		}
		lines = append(lines, SRLine{Barcode: bc, RefundPrice: l.RefundPrice})
	}
	if req.Payments == nil {
		req.Payments = []PayLine{}
	}
	paysJSON, _ := json.Marshal(req.Payments)

	tx, err := db.Begin()
	if err != nil {
		writeErr(w, 500, "开启事务失败: "+err.Error())
		return
	}
	defer tx.Rollback()

	if req.ID != 0 {
		var old []byte
		err := tx.QueryRow(`SELECT draft_lines FROM doc
			WHERE id=$1 AND doc_type='sale_return' AND status='草稿' FOR UPDATE`, req.ID).Scan(&old)
		if err == sql.ErrNoRows {
			writeErr(w, 400, "该单据不是草稿状态（可能已被确认或删除），请刷新")
			return
		}
		if err != nil {
			writeErr(w, 500, "读取单据失败: "+err.Error())
			return
		}
		var oldLines []SRLine
		_ = json.Unmarshal(old, &oldLines)
		for _, ol := range oldLines {
			if _, err := tx.Exec(`UPDATE item SET status='已售', version=version+1
				WHERE barcode=$1 AND status='退货中'`, ol.Barcode); err != nil {
				writeErr(w, 500, "解锁货品失败: "+err.Error())
				return
			}
		}
	}

	for i := range lines {
		sl := &lines[i]
		err := tx.QueryRow(`SELECT name, purity, weight_g FROM item WHERE barcode=$1`, sl.Barcode).
			Scan(&sl.Name, &sl.Purity, &sl.WeightG)
		if err == sql.ErrNoRows {
			writeErr(w, 400, fmt.Sprintf("条码 %s 不存在", sl.Barcode))
			return
		}
		if err != nil {
			writeErr(w, 500, "查询失败: "+err.Error())
			return
		}
		docID, docNo, soldPrice, err := findOrigSale(tx, sl.Barcode)
		if err == sql.ErrNoRows {
			writeErr(w, 400, fmt.Sprintf("条码 %s 找不到对应的已确认销售单", sl.Barcode))
			return
		}
		if err != nil {
			writeErr(w, 500, "原单查询失败: "+err.Error())
			return
		}
		if soldPrice > 0 && sl.RefundPrice > soldPrice {
			writeErr(w, 400, fmt.Sprintf("条码 %s 退款 ¥%.2f 超过原成交价 ¥%.2f", sl.Barcode, sl.RefundPrice, soldPrice))
			return
		}
		sl.OrigDocID, sl.OrigDocNo, sl.SoldPrice = docID, docNo, soldPrice
		res, err := tx.Exec(`UPDATE item SET status='退货中', version=version+1
			WHERE barcode=$1 AND status='已售'`, sl.Barcode)
		if err != nil {
			writeErr(w, 500, "锁定货品失败: "+err.Error())
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var cur string
			_ = tx.QueryRow(`SELECT status FROM item WHERE barcode=$1`, sl.Barcode).Scan(&cur)
			writeErr(w, 400, fmt.Sprintf("条码 %s 当前状态「%s」——只有已售的件才能销退，整单未保存", sl.Barcode, cur))
			return
		}
	}
	linesJSON, _ := json.Marshal(lines)

	if req.ID == 0 {
		today := time.Now().Format("20060102")
		seq, err := nextSeq(tx, "XT", today)
		if err != nil {
			writeErr(w, 500, "单号发号失败: "+err.Error())
			return
		}
		if seq > 99 {
			writeErr(w, 400, "当日销退单号已满99张")
			return
		}
		docNo := fmt.Sprintf("XT%s%02d", today, seq)
		var docID int64
		err = tx.QueryRow(`INSERT INTO doc (doc_no, doc_type, status, maker_id, draft_lines, payments)
			VALUES ($1,'sale_return','草稿',$2,$3,$4) RETURNING id`,
			docNo, currentUID(r), linesJSON, paysJSON).Scan(&docID)
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
	if _, err := tx.Exec(`UPDATE doc SET draft_lines=$1, payments=$2 WHERE id=$3`,
		linesJSON, paysJSON, req.ID); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "status": "草稿"})
}

// POST /api/doc/sale-return/confirm {id} —— 退款确认：货回在库 + 退款校验，一个事务
func handleSaleReturnConfirm(w http.ResponseWriter, r *http.Request) {
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
	res, err := tx.Exec(`UPDATE doc SET status='已确认', confirmed_at=now()
		WHERE id=$1 AND doc_type='sale_return' AND status='草稿'`, req.ID)
	if err != nil {
		writeErr(w, 500, "确认失败: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 400, "确认失败：单据不存在或已被他人确认，请刷新")
		return
	}
	var linesJSON, paysJSON []byte
	if err := tx.QueryRow(`SELECT draft_lines, payments FROM doc WHERE id=$1`, req.ID).
		Scan(&linesJSON, &paysJSON); err != nil {
		writeErr(w, 500, "读取单据失败: "+err.Error())
		return
	}
	var lines []SRLine
	if err := json.Unmarshal(linesJSON, &lines); err != nil || len(lines) == 0 {
		writeErr(w, 400, "草稿明细为空，无法确认")
		return
	}
	var pays []PayLine
	_ = json.Unmarshal(paysJSON, &pays)
	if len(pays) == 0 {
		writeErr(w, 400, "请先录入退款方式再确认")
		return
	}
	if err := validatePayments(pays); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	for _, p := range pays {
		var en bool
		err := tx.QueryRow(`SELECT enabled FROM dict_item WHERE dict_type='pay_method' AND name=$1`,
			p.Method).Scan(&en)
		if err != nil || !en {
			writeErr(w, 400, fmt.Sprintf("退款方式「%s」不存在或已停用", p.Method))
			return
		}
	}
	var total float64
	for i, l := range lines {
		var itemID int64
		err := tx.QueryRow(`UPDATE item SET status='在库', version=version+1
			WHERE barcode=$1 AND status='退货中' RETURNING id`, l.Barcode).Scan(&itemID)
		if err == sql.ErrNoRows {
			var cur string
			_ = tx.QueryRow(`SELECT status FROM item WHERE barcode=$1`, l.Barcode).Scan(&cur)
			writeErr(w, 400, fmt.Sprintf("第%d行条码 %s 当前状态「%s」，整单未确认", i+1, l.Barcode, cur))
			return
		}
		if err != nil {
			writeErr(w, 500, "更新货品失败: "+err.Error())
			return
		}
		total += l.RefundPrice
		if _, err = tx.Exec(`INSERT INTO doc_line (doc_id, item_id, line_no) VALUES ($1,$2,$3)`,
			req.ID, itemID, i+1); err != nil {
			writeErr(w, 500, "写入明细失败: "+err.Error())
			return
		}
		if _, err = tx.Exec(`INSERT INTO stock_flow (item_id, doc_id, from_status, to_status, to_loc, operator_id)
			VALUES ($1,$2,'退货中','在库','总库',$3)`, itemID, req.ID, currentUID(r)); err != nil {
			writeErr(w, 500, "写入流水失败: "+err.Error())
			return
		}
	}
	var paySum float64
	for _, p := range pays {
		paySum += p.Amount
	}
	if round2(paySum) != round2(total) {
		writeErr(w, 400, fmt.Sprintf("退款合计 ¥%.2f 与应退合计 ¥%.2f 不一致，整单未确认", paySum, total))
		return
	}
	if _, err = tx.Exec(`UPDATE doc SET total_amount=$1 WHERE id=$2`, round2(total), req.ID); err != nil {
		writeErr(w, 500, "写入合计失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "status": "已确认", "totalAmount": round2(total)})
}

// POST /api/doc/sale-return/unconfirm {id} —— 仅限当日；货 在库→退货中（草稿仍占着）
func handleSaleReturnUnconfirm(w http.ResponseWriter, r *http.Request) {
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
	res, err := tx.Exec(`UPDATE doc SET status='草稿', confirmed_at=NULL, total_amount=0
		WHERE id=$1 AND doc_type='sale_return' AND status='已确认'
		AND confirmed_at::date = CURRENT_DATE`, req.ID)
	if err != nil {
		writeErr(w, 500, "反确认失败: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 400, "反确认失败：单据不存在、不是已确认状态，或已跨日")
		return
	}
	rows, err := tx.Query(`SELECT it.id, it.barcode FROM doc_line dl
		JOIN item it ON it.id = dl.item_id WHERE dl.doc_id=$1 ORDER BY dl.line_no`, req.ID)
	if err != nil {
		writeErr(w, 500, "明细查询失败: "+err.Error())
		return
	}
	type ref struct {
		id      int64
		barcode string
	}
	var refs []ref
	for rows.Next() {
		var rf ref
		if err := rows.Scan(&rf.id, &rf.barcode); err != nil {
			rows.Close()
			writeErr(w, 500, "明细读取失败: "+err.Error())
			return
		}
		refs = append(refs, rf)
	}
	rows.Close()
	for _, rf := range refs {
		// 件回库后可能已被再次销售/退库占用——条件更新抢不回就整单拒绝
		res, err := tx.Exec(`UPDATE item SET status='退货中', version=version+1
			WHERE id=$1 AND status='在库'`, rf.id)
		if err != nil {
			writeErr(w, 500, "恢复货品失败: "+err.Error())
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var cur string
			_ = tx.QueryRow(`SELECT status FROM item WHERE id=$1`, rf.id).Scan(&cur)
			writeErr(w, 400, fmt.Sprintf("反确认被拒绝：条码 %s 当前状态「%s」（退回的货可能已被再次占用）", rf.barcode, cur))
			return
		}
		if _, err = tx.Exec(`INSERT INTO stock_flow (item_id, doc_id, from_status, to_status, from_loc, operator_id)
			VALUES ($1,$2,'在库','退货中','总库',$3)`, rf.id, req.ID, currentUID(r)); err != nil {
			writeErr(w, 500, "写入流水失败: "+err.Error())
			return
		}
	}
	if _, err = tx.Exec(`DELETE FROM doc_line WHERE doc_id=$1`, req.ID); err != nil {
		writeErr(w, 500, "删除明细失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "status": "草稿"})
}

// POST /api/doc/sale-return/delete {id} —— 删草稿同时把件放回"已售"
func handleSaleReturnDelete(w http.ResponseWriter, r *http.Request) {
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
	var old []byte
	err = tx.QueryRow(`SELECT draft_lines FROM doc
		WHERE id=$1 AND doc_type='sale_return' AND status='草稿' FOR UPDATE`, req.ID).Scan(&old)
	if err == sql.ErrNoRows {
		writeErr(w, 400, "删除失败：只有草稿可以删除")
		return
	}
	if err != nil {
		writeErr(w, 500, "读取单据失败: "+err.Error())
		return
	}
	var oldLines []SRLine
	_ = json.Unmarshal(old, &oldLines)
	for _, ol := range oldLines {
		if _, err := tx.Exec(`UPDATE item SET status='已售', version=version+1
			WHERE barcode=$1 AND status='退货中'`, ol.Barcode); err != nil {
			writeErr(w, 500, "解锁货品失败: "+err.Error())
			return
		}
	}
	if _, err := tx.Exec(`DELETE FROM doc WHERE id=$1`, req.ID); err != nil {
		writeErr(w, 500, "删除失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": req.ID})
}

// GET /api/doc/sale-return —— 销退单列表
func handleSaleReturnList(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`SELECT id, doc_no, status, draft_lines, total_amount, payments,
		to_char(created_at,'YYYY-MM-DD HH24:MI:SS')
		FROM doc WHERE doc_type='sale_return' ORDER BY id DESC LIMIT 20`)
	if err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	defer rows.Close()
	type RDoc struct {
		ID          int64     `json:"id"`
		DocNo       string    `json:"docNo"`
		Status      string    `json:"status"`
		TotalAmount float64   `json:"totalAmount"`
		Payments    []PayLine `json:"payments"`
		Lines       []SRLine  `json:"lines"`
		MadeAt      string    `json:"madeAt"`
	}
	docs := []RDoc{}
	for rows.Next() {
		var d RDoc
		var dl, pj []byte
		if err := rows.Scan(&d.ID, &d.DocNo, &d.Status, &dl, &d.TotalAmount, &pj, &d.MadeAt); err != nil {
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		d.Lines = []SRLine{}
		_ = json.Unmarshal(dl, &d.Lines)
		d.Payments = []PayLine{}
		_ = json.Unmarshal(pj, &d.Payments)
		docs = append(docs, d)
	}
	writeJSON(w, 200, map[string]any{"list": docs, "total": len(docs)})
}

// ===== 分销商与调拨单（v0.18） =====
// 位置 = location_type('总库'/'分销商') + distributor_id，与状态(在库/已售)正交。
// 一种单据三种用法：调拨单(from→to) = 分货 / 退回总库 / 分销商互调。
// 调拨草稿占用状态 = '调拨中'；确认后位置变更、状态回'在库'。

// locName 位置显示名：0=总库，否则查分销商名
func locName(q rowQuerier, distID int64) string {
	if distID == 0 {
		return "总库"
	}
	var n string
	if err := q.QueryRow(`SELECT name FROM distributor WHERE id=$1`, distID).Scan(&n); err != nil {
		return "未知分销商"
	}
	return n
}

// itemLocName 某件货当前位置的显示名
func itemLocName(q rowQuerier, barcode string) string {
	var lt string
	var did sql.NullInt64
	if err := q.QueryRow(`SELECT location_type, distributor_id FROM item WHERE barcode=$1`, barcode).
		Scan(&lt, &did); err != nil {
		return "未知"
	}
	if lt == "总库" {
		return "总库"
	}
	return locName(q, did.Int64)
}

// GET /api/distributors —— 所有登录用户可读（调拨下拉要用）
func handleDistributorList(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`SELECT id, name, status FROM distributor ORDER BY id`)
	if err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	defer rows.Close()
	type D struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Status int    `json:"status"`
	}
	list := []D{}
	for rows.Next() {
		var d D
		if err := rows.Scan(&d.ID, &d.Name, &d.Status); err != nil {
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		list = append(list, d)
	}
	writeJSON(w, 200, map[string]any{"list": list})
}

// POST /api/distributors —— 新增（管理员）
func handleDistributorCreate(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数格式错误")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len([]rune(req.Name)) > 30 {
		writeErr(w, 400, "名称不能为空且不超过30字")
		return
	}
	var exists bool
	_ = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM distributor WHERE name=$1)`, req.Name).Scan(&exists)
	if exists {
		writeErr(w, 400, "该名称已存在")
		return
	}
	var id int64
	if err := db.QueryRow(`INSERT INTO distributor (name) VALUES ($1) RETURNING id`, req.Name).Scan(&id); err != nil {
		writeErr(w, 500, "创建失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id})
}

// POST /api/distributors/update —— 改名/停用启用（管理员；按id引用，改名安全）
func handleDistributorUpdate(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req struct {
		ID     int64   `json:"id"`
		Name   *string `json:"name"`
		Status *int    `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == 0 {
		writeErr(w, 400, "参数错误")
		return
	}
	if req.Name != nil {
		n := strings.TrimSpace(*req.Name)
		if n == "" || len([]rune(n)) > 30 {
			writeErr(w, 400, "名称不能为空且不超过30字")
			return
		}
		var exists bool
		_ = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM distributor WHERE name=$1 AND id<>$2)`, n, req.ID).Scan(&exists)
		if exists {
			writeErr(w, 400, "该名称已存在")
			return
		}
		if _, err := db.Exec(`UPDATE distributor SET name=$1 WHERE id=$2`, n, req.ID); err != nil {
			writeErr(w, 500, "更新失败: "+err.Error())
			return
		}
	}
	if req.Status != nil {
		if _, err := db.Exec(`UPDATE distributor SET status=$1 WHERE id=$2`, *req.Status, req.ID); err != nil {
			writeErr(w, 500, "更新失败: "+err.Error())
			return
		}
	}
	writeJSON(w, 200, map[string]any{"id": req.ID})
}

// checkLoc 校验调拨端点：0=总库恒合法；>0 须是启用的分销商
func checkLoc(q rowQuerier, distID int64, side string) error {
	if distID == 0 {
		return nil
	}
	var status int
	err := q.QueryRow(`SELECT status FROM distributor WHERE id=$1`, distID).Scan(&status)
	if err != nil {
		return fmt.Errorf("%s分销商不存在", side)
	}
	if status != 1 {
		return fmt.Errorf("%s分销商已停用", side)
	}
	return nil
}

// POST /api/doc/transfer/save  {id?, fromDistributorId, toDistributorId, barcodes[]}
func handleTransferSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID                int64    `json:"id"`
		FromDistributorID int64    `json:"fromDistributorId"` // 0=总库
		ToDistributorID   int64    `json:"toDistributorId"`   // 0=总库
		Barcodes          []string `json:"barcodes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Barcodes) == 0 {
		writeErr(w, 400, "参数错误：至少要有一个条码")
		return
	}
	if req.FromDistributorID == req.ToDistributorID {
		writeErr(w, 400, "调出方与调入方不能相同")
		return
	}
	seen := map[string]bool{}
	lines := []DraftLine{}
	for i, raw := range req.Barcodes {
		bc := strings.ToUpper(strings.TrimSpace(raw))
		if !validBarcode(bc) {
			writeErr(w, 400, fmt.Sprintf("第%d个条码 %s 格式非法", i+1, bc))
			return
		}
		if seen[bc] {
			writeErr(w, 400, fmt.Sprintf("条码 %s 重复", bc))
			return
		}
		seen[bc] = true
		lines = append(lines, DraftLine{Barcode: bc})
	}

	tx, err := db.Begin()
	if err != nil {
		writeErr(w, 500, "开启事务失败: "+err.Error())
		return
	}
	defer tx.Rollback()
	if err := checkLoc(tx, req.FromDistributorID, "调出方"); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := checkLoc(tx, req.ToDistributorID, "调入方"); err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	if req.ID != 0 {
		var old []byte
		err := tx.QueryRow(`SELECT draft_lines FROM doc
			WHERE id=$1 AND doc_type='transfer' AND status='草稿' FOR UPDATE`, req.ID).Scan(&old)
		if err == sql.ErrNoRows {
			writeErr(w, 400, "该单据不是草稿状态（可能已被确认或删除），请刷新")
			return
		}
		if err != nil {
			writeErr(w, 500, "读取单据失败: "+err.Error())
			return
		}
		var oldLines []DraftLine
		_ = json.Unmarshal(old, &oldLines)
		for _, ol := range oldLines {
			if _, err := tx.Exec(`UPDATE item SET status='在库', version=version+1
				WHERE barcode=$1 AND status='调拨中'`, ol.Barcode); err != nil {
				writeErr(w, 500, "解锁货品失败: "+err.Error())
				return
			}
		}
	}

	fromName := locName(tx, req.FromDistributorID)
	for i := range lines {
		l := &lines[i]
		err := tx.QueryRow(`SELECT name, purity, weight_g, price FROM item WHERE barcode=$1`, l.Barcode).
			Scan(&l.Name, &l.Purity, &l.WeightG, &l.Price)
		if err == sql.ErrNoRows {
			writeErr(w, 400, fmt.Sprintf("条码 %s 不存在", l.Barcode))
			return
		}
		if err != nil {
			writeErr(w, 500, "查询失败: "+err.Error())
			return
		}
		// 锁定：必须 在库 且在调出方位置
		var res sql.Result
		if req.FromDistributorID == 0 {
			res, err = tx.Exec(`UPDATE item SET status='调拨中', version=version+1
				WHERE barcode=$1 AND status='在库' AND location_type='总库'`, l.Barcode)
		} else {
			res, err = tx.Exec(`UPDATE item SET status='调拨中', version=version+1
				WHERE barcode=$1 AND status='在库' AND location_type='分销商' AND distributor_id=$2`,
				l.Barcode, req.FromDistributorID)
		}
		if err != nil {
			writeErr(w, 500, "锁定货品失败: "+err.Error())
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var cur string
			_ = tx.QueryRow(`SELECT status FROM item WHERE barcode=$1`, l.Barcode).Scan(&cur)
			if cur == "在库" {
				writeErr(w, 400, fmt.Sprintf("条码 %s 在「%s」处，不在调出方「%s」，整单未保存",
					l.Barcode, itemLocName(tx, l.Barcode), fromName))
			} else {
				writeErr(w, 400, fmt.Sprintf("条码 %s 当前状态「%s」，不能调拨，整单未保存", l.Barcode, cur))
			}
			return
		}
	}
	linesJSON, _ := json.Marshal(lines)
	var fromID, toID any
	if req.FromDistributorID > 0 {
		fromID = req.FromDistributorID
	}
	if req.ToDistributorID > 0 {
		toID = req.ToDistributorID
	}

	if req.ID == 0 {
		today := time.Now().Format("20060102")
		seq, err := nextSeq(tx, "DB", today)
		if err != nil {
			writeErr(w, 500, "单号发号失败: "+err.Error())
			return
		}
		if seq > 99 {
			writeErr(w, 400, "当日调拨单号已满99张")
			return
		}
		docNo := fmt.Sprintf("DB%s%02d", today, seq)
		var docID int64
		err = tx.QueryRow(`INSERT INTO doc (doc_no, doc_type, status, maker_id, draft_lines,
			from_distributor_id, to_distributor_id)
			VALUES ($1,'transfer','草稿',$2,$3,$4,$5) RETURNING id`,
			docNo, currentUID(r), linesJSON, fromID, toID).Scan(&docID)
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
	if _, err := tx.Exec(`UPDATE doc SET draft_lines=$1, from_distributor_id=$2, to_distributor_id=$3
		WHERE id=$4`, linesJSON, fromID, toID, req.ID); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "status": "草稿"})
}

// POST /api/doc/transfer/confirm {id} —— 位置变更生效：调拨中→在库@调入方
func handleTransferConfirm(w http.ResponseWriter, r *http.Request) {
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
	res, err := tx.Exec(`UPDATE doc SET status='已确认', confirmed_at=now()
		WHERE id=$1 AND doc_type='transfer' AND status='草稿'`, req.ID)
	if err != nil {
		writeErr(w, 500, "确认失败: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 400, "确认失败：单据不存在或已被他人确认，请刷新")
		return
	}
	var linesJSON []byte
	var fromID, toID sql.NullInt64
	if err := tx.QueryRow(`SELECT draft_lines, from_distributor_id, to_distributor_id
		FROM doc WHERE id=$1`, req.ID).Scan(&linesJSON, &fromID, &toID); err != nil {
		writeErr(w, 500, "读取单据失败: "+err.Error())
		return
	}
	var lines []DraftLine
	if err := json.Unmarshal(linesJSON, &lines); err != nil || len(lines) == 0 {
		writeErr(w, 400, "草稿明细为空，无法确认")
		return
	}
	if err := checkLoc(tx, toID.Int64, "调入方"); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	fromName, toName := locName(tx, fromID.Int64), locName(tx, toID.Int64)
	for i, l := range lines {
		var itemID int64
		var e error
		if toID.Int64 == 0 {
			e = tx.QueryRow(`UPDATE item SET status='在库', location_type='总库', distributor_id=NULL,
				version=version+1 WHERE barcode=$1 AND status='调拨中' RETURNING id`, l.Barcode).Scan(&itemID)
		} else {
			e = tx.QueryRow(`UPDATE item SET status='在库', location_type='分销商', distributor_id=$2,
				version=version+1 WHERE barcode=$1 AND status='调拨中' RETURNING id`, l.Barcode, toID.Int64).Scan(&itemID)
		}
		if e == sql.ErrNoRows {
			var cur string
			_ = tx.QueryRow(`SELECT status FROM item WHERE barcode=$1`, l.Barcode).Scan(&cur)
			writeErr(w, 400, fmt.Sprintf("第%d行条码 %s 当前状态「%s」，整单未确认", i+1, l.Barcode, cur))
			return
		}
		if e != nil {
			writeErr(w, 500, "更新货品失败: "+e.Error())
			return
		}
		if _, err = tx.Exec(`INSERT INTO doc_line (doc_id, item_id, line_no) VALUES ($1,$2,$3)`,
			req.ID, itemID, i+1); err != nil {
			writeErr(w, 500, "写入明细失败: "+err.Error())
			return
		}
		if _, err = tx.Exec(`INSERT INTO stock_flow (item_id, doc_id, from_status, to_status, from_loc, to_loc, operator_id)
			VALUES ($1,$2,'调拨中','在库',$3,$4,$5)`, itemID, req.ID, fromName, toName, currentUID(r)); err != nil {
			writeErr(w, 500, "写入流水失败: "+err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "status": "已确认"})
}

// POST /api/doc/transfer/unconfirm {id} —— 件从调入方拉回：在库@调入方→调拨中@调出方
func handleTransferUnconfirm(w http.ResponseWriter, r *http.Request) {
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
	res, err := tx.Exec(`UPDATE doc SET status='草稿', confirmed_at=NULL
		WHERE id=$1 AND doc_type='transfer' AND status='已确认'`, req.ID)
	if err != nil {
		writeErr(w, 500, "反确认失败: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 400, "反确认失败：单据不存在或不是已确认状态，请刷新")
		return
	}
	var fromID, toID sql.NullInt64
	if err := tx.QueryRow(`SELECT from_distributor_id, to_distributor_id FROM doc WHERE id=$1`,
		req.ID).Scan(&fromID, &toID); err != nil {
		writeErr(w, 500, "读取单据失败: "+err.Error())
		return
	}
	fromName, toName := locName(tx, fromID.Int64), locName(tx, toID.Int64)
	rows, err := tx.Query(`SELECT it.id, it.barcode FROM doc_line dl
		JOIN item it ON it.id = dl.item_id WHERE dl.doc_id=$1 ORDER BY dl.line_no`, req.ID)
	if err != nil {
		writeErr(w, 500, "明细查询失败: "+err.Error())
		return
	}
	type ref struct {
		id      int64
		barcode string
	}
	var refs []ref
	for rows.Next() {
		var rf ref
		if err := rows.Scan(&rf.id, &rf.barcode); err != nil {
			rows.Close()
			writeErr(w, 500, "明细读取失败: "+err.Error())
			return
		}
		refs = append(refs, rf)
	}
	rows.Close()
	for _, rf := range refs {
		// 件必须还"在库"且仍在调入方，才能拉回——已被再次调走/挂单/售出则拒绝
		var res sql.Result
		var e error
		if toID.Int64 == 0 {
			res, e = tx.Exec(`UPDATE item SET status='调拨中', location_type=$2, distributor_id=$3,
				version=version+1 WHERE id=$1 AND status='在库' AND location_type='总库'`,
				rf.id, locTypeOf(fromID.Int64), nullableID(fromID.Int64))
		} else {
			res, e = tx.Exec(`UPDATE item SET status='调拨中', location_type=$2, distributor_id=$3,
				version=version+1 WHERE id=$1 AND status='在库' AND location_type='分销商' AND distributor_id=$4`,
				rf.id, locTypeOf(fromID.Int64), nullableID(fromID.Int64), toID.Int64)
		}
		if e != nil {
			writeErr(w, 500, "恢复货品失败: "+e.Error())
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var cur string
			_ = tx.QueryRow(`SELECT status FROM item WHERE id=$1`, rf.id).Scan(&cur)
			writeErr(w, 400, fmt.Sprintf("反确认被拒绝：条码 %s 当前状态「%s」或已不在「%s」（可能已被再次占用）",
				rf.barcode, cur, toName))
			return
		}
		if _, err = tx.Exec(`INSERT INTO stock_flow (item_id, doc_id, from_status, to_status, from_loc, to_loc, operator_id)
			VALUES ($1,$2,'在库','调拨中',$3,$4,$5)`, rf.id, req.ID, toName, fromName, currentUID(r)); err != nil {
			writeErr(w, 500, "写入流水失败: "+err.Error())
			return
		}
	}
	if _, err = tx.Exec(`DELETE FROM doc_line WHERE doc_id=$1`, req.ID); err != nil {
		writeErr(w, 500, "删除明细失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "status": "草稿"})
}

func locTypeOf(distID int64) string {
	if distID == 0 {
		return "总库"
	}
	return "分销商"
}

func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// POST /api/doc/transfer/delete {id} —— 删草稿解锁（件留在调出方）
func handleTransferDelete(w http.ResponseWriter, r *http.Request) {
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
	var old []byte
	err = tx.QueryRow(`SELECT draft_lines FROM doc
		WHERE id=$1 AND doc_type='transfer' AND status='草稿' FOR UPDATE`, req.ID).Scan(&old)
	if err == sql.ErrNoRows {
		writeErr(w, 400, "删除失败：只有草稿可以删除（已确认的请先反确认）")
		return
	}
	if err != nil {
		writeErr(w, 500, "读取单据失败: "+err.Error())
		return
	}
	var oldLines []DraftLine
	_ = json.Unmarshal(old, &oldLines)
	for _, ol := range oldLines {
		if _, err := tx.Exec(`UPDATE item SET status='在库', version=version+1
			WHERE barcode=$1 AND status='调拨中'`, ol.Barcode); err != nil {
			writeErr(w, 500, "解锁货品失败: "+err.Error())
			return
		}
	}
	if _, err := tx.Exec(`DELETE FROM doc WHERE id=$1`, req.ID); err != nil {
		writeErr(w, 500, "删除失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": req.ID})
}

// GET /api/doc/transfer —— 调拨单列表（带两端名称）
func handleTransferList(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`SELECT d.id, d.doc_no, d.status, d.draft_lines,
		d.from_distributor_id, COALESCE(f.name,'总库'), d.to_distributor_id, COALESCE(t.name,'总库'),
		to_char(d.created_at,'YYYY-MM-DD HH24:MI:SS')
		FROM doc d
		LEFT JOIN distributor f ON f.id = d.from_distributor_id
		LEFT JOIN distributor t ON t.id = d.to_distributor_id
		WHERE d.doc_type='transfer' ORDER BY d.id DESC LIMIT 20`)
	if err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	defer rows.Close()
	type TDoc struct {
		ID       int64       `json:"id"`
		DocNo    string      `json:"docNo"`
		Status   string      `json:"status"`
		FromID   int64       `json:"fromDistributorId"`
		FromName string      `json:"fromName"`
		ToID     int64       `json:"toDistributorId"`
		ToName   string      `json:"toName"`
		Lines    []DraftLine `json:"lines"`
		MadeAt   string      `json:"madeAt"`
	}
	docs := []TDoc{}
	for rows.Next() {
		var d TDoc
		var dl []byte
		var fid, tid sql.NullInt64
		if err := rows.Scan(&d.ID, &d.DocNo, &d.Status, &dl, &fid, &d.FromName, &tid, &d.ToName, &d.MadeAt); err != nil {
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		d.FromID, d.ToID = fid.Int64, tid.Int64
		d.Lines = []DraftLine{}
		_ = json.Unmarshal(dl, &d.Lines)
		docs = append(docs, d)
	}
	writeJSON(w, 200, map[string]any{"list": docs, "total": len(docs)})
}

func handleItems(w http.ResponseWriter, r *http.Request) {
	// 查询参数：status(状态) category(大类) q(条码前缀或名称模糊) —— 都可选
	q := r.URL.Query()
	where := []string{"1=1"}
	args := []any{}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if v := strings.TrimSpace(q.Get("status")); v != "" {
		add("it.status=$%d", v)
	}
	if v := strings.TrimSpace(q.Get("category")); v != "" {
		add("it.category=$%d", v)
	}
	// 位置筛选（v0.18）：loc 参数 "0"=总库，其他数字=分销商id，空=全部
	if v := strings.TrimSpace(q.Get("loc")); v != "" {
		if v == "0" {
			where = append(where, "it.location_type='总库'")
		} else {
			add("it.distributor_id=$%d", v)
		}
	}
	if v := strings.TrimSpace(q.Get("q")); v != "" {
		kw := strings.ToUpper(v)
		args = append(args, kw+"%", "%"+v+"%")
		where = append(where, fmt.Sprintf("(it.barcode LIKE $%d OR it.name ILIKE $%d)", len(args)-1, len(args)))
	}
	cond := strings.Join(where, " AND ")

	// 汇总与明细用同一组条件——保证"合计"永远和看到的列表一致
	var total int
	var sumW float64
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(it.weight_g),0) FROM item it WHERE `+cond, args...).
		Scan(&total, &sumW); err != nil {
		writeErr(w, 500, "汇总失败: "+err.Error())
		return
	}
	rows, err := db.Query(`SELECT it.id, it.barcode, it.name, it.category, it.purity, it.weight_g, it.price, it.status,
		CASE WHEN it.location_type='总库' THEN '总库' ELSE COALESCE(dst.name,'未知分销商') END
		FROM item it LEFT JOIN distributor dst ON dst.id = it.distributor_id
		WHERE `+cond+` ORDER BY it.id DESC LIMIT 500`, args...)
	if err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	defer rows.Close()
	list := []Item{}
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.Barcode, &it.Name, &it.Category, &it.Purity, &it.WeightG, &it.Price, &it.Status, &it.Location); err != nil {
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		list = append(list, it)
	}
	writeJSON(w, 200, map[string]any{"list": list, "total": total, "sumWeightG": sumW})
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
	var catOK bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM dict_item
		WHERE dict_type='category' AND name=$1 AND enabled=true)`, req.Category).Scan(&catOK); err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	if !catOK {
		writeErr(w, 400, "首饰大类「"+req.Category+"」不存在或已停用")
		return
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
			INSERT INTO item (barcode, name, category, purity, weight_g, price, status)
			VALUES ($1,$2,$3,$4,$5,$6,'在库') RETURNING id`,
			bc, l.Name, category, l.Purity, l.WeightG, l.Price).Scan(&itemID)
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
			Purity: l.Purity, WeightG: l.WeightG, Price: l.Price, Status: "在库"})
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
		SELECT it.id, it.barcode, it.status, it.name, it.purity, it.weight_g, it.price
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
		var wg, pr float64
		if err := rows.Scan(&id, &bc, &st, &name, &purity, &wg, &pr); err != nil {
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
		rebuilt = append(rebuilt, DraftLine{Barcode: bc, Name: name, Purity: purity, WeightG: wg, Price: pr})
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
					Purity: l.Purity, WeightG: l.WeightG, Price: l.Price, Status: "草稿"})
			}
			continue
		}
		irows, err := db.Query(`
			SELECT it.id, it.barcode, it.name, it.purity, it.weight_g, it.price, it.status
			FROM doc_line dl JOIN item it ON it.id = dl.item_id
			WHERE dl.doc_id=$1 ORDER BY dl.line_no`, docs[i].ID)
		if err != nil {
			writeErr(w, 500, "明细查询失败: "+err.Error())
			return
		}
		for irows.Next() {
			var it Item
			if err := irows.Scan(&it.ID, &it.Barcode, &it.Name, &it.Purity, &it.WeightG, &it.Price, &it.Status); err != nil {
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
	mux.HandleFunc("GET /api/me", withAuth(handleMe))
	mux.HandleFunc("POST /api/me/password", withAuth(handleChangePassword))
	mux.HandleFunc("GET /api/dict", withAuth(handleDictList))
	mux.HandleFunc("POST /api/dict", withAuth(handleDictCreate))
	mux.HandleFunc("POST /api/dict/update", withAuth(handleDictUpdate))
	mux.HandleFunc("GET /api/salespersons", withAuth(handleSalespersonList))
	mux.HandleFunc("POST /api/salespersons", withAuth(handleSalespersonCreate))
	mux.HandleFunc("POST /api/salespersons/update", withAuth(handleSalespersonUpdate))
	mux.HandleFunc("GET /api/users", withAuth(handleUserList))
	mux.HandleFunc("POST /api/users", withAuth(handleUserCreate))
	mux.HandleFunc("POST /api/users/status", withAuth(handleUserStatus))
	mux.HandleFunc("POST /api/users/password", withAuth(handleUserResetPwd))
	mux.HandleFunc("GET /api/gold-price/current", withAuth(handleGoldPriceCurrent))
	mux.HandleFunc("GET /api/gold-price/history", withAuth(handleGoldPriceHistory))
	mux.HandleFunc("POST /api/gold-price", withAuth(handleGoldPricePublish))
	mux.HandleFunc("GET /api/items", withAuth(handleItems))
	mux.HandleFunc("POST /api/doc/inbound/save", withAuth(handleInboundSave))
	mux.HandleFunc("POST /api/doc/inbound/confirm", withAuth(handleInboundConfirm))
	mux.HandleFunc("POST /api/doc/inbound/unconfirm", withAuth(handleInboundUnconfirm))
	mux.HandleFunc("POST /api/doc/inbound/delete", withAuth(handleInboundDelete))
	mux.HandleFunc("GET /api/doc/inbound", withAuth(handleInboundList))
	mux.HandleFunc("POST /api/doc/outbound/save", withAuth(handleOutboundSave))
	mux.HandleFunc("POST /api/doc/outbound/confirm", withAuth(handleOutboundConfirm))
	mux.HandleFunc("POST /api/doc/outbound/unconfirm", withAuth(handleOutboundUnconfirm))
	mux.HandleFunc("POST /api/doc/outbound/delete", withAuth(handleOutboundDelete))
	mux.HandleFunc("GET /api/doc/outbound", withAuth(handleOutboundList))
	mux.HandleFunc("POST /api/doc/sale/save", withAuth(handleSaleSave))
	mux.HandleFunc("POST /api/doc/sale/confirm", withAuth(handleSaleConfirm))
	mux.HandleFunc("POST /api/doc/sale/unconfirm", withAuth(handleSaleUnconfirm))
	mux.HandleFunc("POST /api/doc/sale/delete", withAuth(handleSaleDelete))
	mux.HandleFunc("GET /api/doc/sale", withAuth(handleSaleList))
	mux.HandleFunc("GET /api/sale-return/lookup", withAuth(handleSaleReturnLookup))
	mux.HandleFunc("POST /api/doc/sale-return/save", withAuth(handleSaleReturnSave))
	mux.HandleFunc("POST /api/doc/sale-return/confirm", withAuth(handleSaleReturnConfirm))
	mux.HandleFunc("POST /api/doc/sale-return/unconfirm", withAuth(handleSaleReturnUnconfirm))
	mux.HandleFunc("POST /api/doc/sale-return/delete", withAuth(handleSaleReturnDelete))
	mux.HandleFunc("GET /api/doc/sale-return", withAuth(handleSaleReturnList))
	mux.HandleFunc("GET /api/distributors", withAuth(handleDistributorList))
	mux.HandleFunc("POST /api/distributors", withAuth(handleDistributorCreate))
	mux.HandleFunc("POST /api/distributors/update", withAuth(handleDistributorUpdate))
	mux.HandleFunc("POST /api/doc/transfer/save", withAuth(handleTransferSave))
	mux.HandleFunc("POST /api/doc/transfer/confirm", withAuth(handleTransferConfirm))
	mux.HandleFunc("POST /api/doc/transfer/unconfirm", withAuth(handleTransferUnconfirm))
	mux.HandleFunc("POST /api/doc/transfer/delete", withAuth(handleTransferDelete))
	mux.HandleFunc("GET /api/doc/transfer", withAuth(handleTransferList))

	fmt.Println("后端已启动: http://localhost:8080/api/health  (Ctrl+C 停止)")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		panic(err)
	}
}
