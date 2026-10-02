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
	"archive/zip"
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
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
	Price     float64 `json:"price"`
	Status    string  `json:"status"`
	Location  string  `json:"location,omitempty"` // v0.18：总库 或 分销商名
	JewelType string  `json:"jewelType,omitempty"` // v0.28 名称三要素
	StoneName string  `json:"stoneName,omitempty"`
	// v0.27 计价模型
	SaleFeeMode   string  `json:"saleFeeMode,omitempty"`
	SaleFee       float64 `json:"saleFee,omitempty"`
	CostGoldPrice float64 `json:"costGoldPrice,omitempty"` // 成本字段仅管理员可见
	CostFeeMode   string  `json:"costFeeMode,omitempty"`
	CostFee       float64 `json:"costFee,omitempty"`
}

// DraftLine 草稿明细行（存进 doc.draft_lines 的 JSON 结构，字段名与前端一致）
type DraftLine struct {
	Barcode string  `json:"barcode"`
	Name    string  `json:"name"` // v0.28起由服务端拼接：成色+主石名称+首饰类别
	Purity  string  `json:"purity"`
	// v0.28：名称三要素中的另外两段（既可选字典也可直接填，新值自动进字典）
	StoneName string  `json:"stoneName"`
	JewelType string  `json:"jewelType"`
	WeightG   float64 `json:"weightG"`
	Price     float64 `json:"price"` // 售价(标签价)，0=未定价
	// v0.27 计价模型：销售工费（变金价=克重×金价+工费）与进货成本
	SaleFeeMode   string  `json:"saleFeeMode,omitempty"` // 按克/按件
	SaleFee       float64 `json:"saleFee,omitempty"`
	CostGoldPrice float64 `json:"costGoldPrice,omitempty"` // 进货金价(元/克)
	CostFeeMode   string  `json:"costFeeMode,omitempty"`
	CostFee       float64 `json:"costFee,omitempty"`
}

// ensureDictValues 把入库时直接填写的新值补进字典（已存在则跳过）——
// "可以直接填写，也可以下拉"的另一半：填过一次，下次就在下拉里。
func ensureDictValues(tx *sql.Tx, dictType string, values map[string]bool) error {
	for v := range values {
		if v == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO dict_item (dict_type, name, sort)
			VALUES ($1,$2,99) ON CONFLICT (dict_type, name) DO NOTHING`, dictType, v); err != nil {
			return err
		}
	}
	return nil
}

// feeAmount 工费金额：按克=克重×单价；按件=固定额
func feeAmount(mode string, fee, weightG float64) float64 {
	if mode == "按件" {
		return fee
	}
	return round2(weightG * fee)
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
	ctxUID   ctxKey = "uid"
	ctxAdm   ctxKey = "adm"
	ctxStore ctxKey = "store" // v0.19：账号所属门店id，0=总部
)

// currentUID 从请求上下文取当前登录用户id（withAuth 已验证并放入）
func currentUID(r *http.Request) int64 {
	if v, ok := r.Context().Value(ctxUID).(int64); ok {
		return v
	}
	return 0
}

// currentStore 当前账号所属门店id，0=总部（v0.19）
func currentStore(r *http.Request) int64 {
	if v, ok := r.Context().Value(ctxStore).(int64); ok {
		return v
	}
	return 0
}

// requireHQ 入库/退库/调拨等是总部职能，门店账号拒绝
func requireHQ(w http.ResponseWriter, r *http.Request) bool {
	if currentStore(r) != 0 {
		writeErr(w, 403, "门店账号无权进行此操作（总部职能）")
		return false
	}
	return true
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
		// v0.28：名称=成色+主石名称+首饰类别 拼接而成，不再手填。
		// 类别必填（名称的骨架）；主石可空（素金）。老草稿（无类别但有名称）原样放行。
		l.Purity = strings.TrimSpace(l.Purity)
		l.StoneName = strings.TrimSpace(l.StoneName)
		l.JewelType = strings.TrimSpace(l.JewelType)
		if l.Purity == "" {
			return nil, fmt.Errorf("第%d行缺少成色", i+1)
		}
		if l.JewelType == "" {
			if strings.TrimSpace(l.Name) == "" {
				return nil, fmt.Errorf("第%d行缺少首饰类别", i+1)
			}
		} else {
			l.Name = l.Purity + l.StoneName + l.JewelType
		}
		if l.WeightG <= 0 {
			return nil, fmt.Errorf("第%d行总件重必须大于0", i+1)
		}
		if l.Price < 0 {
			return nil, fmt.Errorf("第%d行售价不能为负数", i+1)
		}
		// v0.27：工费与成本校验（方式默认按克；金额不得为负）
		if l.SaleFeeMode == "" {
			l.SaleFeeMode = "按克"
		}
		if l.CostFeeMode == "" {
			l.CostFeeMode = "按克"
		}
		if l.SaleFeeMode != "按克" && l.SaleFeeMode != "按件" {
			return nil, fmt.Errorf("第%d行销售工费方式必须是 按克 或 按件", i+1)
		}
		if l.CostFeeMode != "按克" && l.CostFeeMode != "按件" {
			return nil, fmt.Errorf("第%d行进货工费方式必须是 按克 或 按件", i+1)
		}
		if l.SaleFee < 0 || l.CostFee < 0 || l.CostGoldPrice < 0 {
			return nil, fmt.Errorf("第%d行工费/进货金价不能为负数", i+1)
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
		// v0.19：门店归属也每次实时读取——改绑门店立即生效
		var status int
		var isAdmin bool
		var storeID sql.NullInt64
		err = db.QueryRow(`SELECT status, is_admin, distributor_id FROM app_user WHERE id=$1`,
			int64(uid)).Scan(&status, &isAdmin, &storeID)
		if err != nil || status != 1 {
			writeErr(w, 401, "账号已被禁用或不存在")
			return
		}
		ctx := context.WithValue(r.Context(), ctxUID, int64(uid))
		ctx = context.WithValue(ctx, ctxAdm, isAdmin)
		ctx = context.WithValue(ctx, ctxStore, storeID.Int64)
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
	var stored, name, storeName string
	var isAdmin bool
	var storeID sql.NullInt64
	err := db.QueryRow(`SELECT u.id, u.password, u.name, u.is_admin, u.distributor_id, COALESCE(d.name,'')
		FROM app_user u LEFT JOIN distributor d ON d.id = u.distributor_id
		WHERE u.username=$1 AND u.status=1`,
		strings.TrimSpace(req.Username)).Scan(&uid, &stored, &name, &isAdmin, &storeID, &storeName)
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
	writeJSON(w, 200, map[string]any{"token": token, "name": name, "isAdmin": isAdmin,
		"storeId": storeID.Int64, "storeName": storeName})
}

// GET /api/me —— 恢复登录态用：客户端启动时拿本地保存的令牌来问"我是谁"（v0.13）。
// 能走到这里说明 withAuth 已验过：令牌有效 + 账号未禁用。
func handleMe(w http.ResponseWriter, r *http.Request) {
	uid := currentUID(r)
	var username, name, storeName string
	var storeID sql.NullInt64
	if err := db.QueryRow(`SELECT u.username, u.name, u.distributor_id, COALESCE(d.name,'')
		FROM app_user u LEFT JOIN distributor d ON d.id = u.distributor_id
		WHERE u.id=$1`, uid).Scan(&username, &name, &storeID, &storeName); err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"username": username, "name": name, "isAdmin": currentIsAdmin(r),
		"storeId": storeID.Int64, "storeName": storeName})
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
	rows, err := db.Query(`SELECT u.id, u.username, u.name, u.status, u.is_admin,
		COALESCE(d.name,'总部'), to_char(u.created_at,'YYYY-MM-DD')
		FROM app_user u LEFT JOIN distributor d ON d.id = u.distributor_id ORDER BY u.id`)
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
		Store    string `json:"store"`
		Created  string `json:"created"`
	}
	list := []U{}
	for rows.Next() {
		var u U
		if err := rows.Scan(&u.ID, &u.Username, &u.Name, &u.Status, &u.IsAdmin, &u.Store, &u.Created); err != nil {
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
		Username      string `json:"username"`
		Name          string `json:"name"`
		Password      string `json:"password"`
		DistributorID int64  `json:"distributorId"` // v0.19：0=总部，否则绑定该门店
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数格式错误")
		return
	}
	if req.DistributorID != 0 {
		var status int
		if err := db.QueryRow(`SELECT status FROM distributor WHERE id=$1`, req.DistributorID).Scan(&status); err != nil {
			writeErr(w, 400, "所选门店不存在")
			return
		}
		if status != 1 {
			writeErr(w, 400, "所选门店已停用")
			return
		}
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
	if err := db.QueryRow(`INSERT INTO app_user (username, password, name, status, is_admin, distributor_id)
		VALUES ($1,$2,$3,1,false,$4) RETURNING id`, req.Username, h, req.Name,
		nullableID(req.DistributorID)).Scan(&id); err != nil {
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

var validDictTypes = map[string]bool{"category": true, "purity": true, "jewel_type": true, "pay_method": true, "stone_name": true}

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
	rows, err := db.Query(`SELECT s.id, s.name, s.sort, s.enabled,
		s.distributor_id, COALESCE(d.name,'总部'), s.role, s.manager_rate
		FROM salesperson s LEFT JOIN distributor d ON d.id = s.distributor_id
		ORDER BY s.sort, s.id`)
	if err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	defer rows.Close()
	type S struct {
		ID            int64   `json:"id"`
		Name          string  `json:"name"`
		Sort          int     `json:"sort"`
		Enabled       bool    `json:"enabled"`
		DistributorID int64   `json:"distributorId"`
		StoreName     string  `json:"storeName"`
		Role          string  `json:"role"`
		ManagerRate   float64 `json:"managerRate"`
	}
	list := []S{}
	for rows.Next() {
		var s S
		var did sql.NullInt64
		if err := rows.Scan(&s.ID, &s.Name, &s.Sort, &s.Enabled, &did, &s.StoreName, &s.Role, &s.ManagerRate); err != nil {
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		s.DistributorID = did.Int64
		list = append(list, s)
	}
	writeJSON(w, 200, map[string]any{"list": list})
}

// checkOneManager 每个门店（含总部）最多一个启用的店长——店长抽成的归属才不含糊
func checkOneManager(distID int64, role string, excludeID int64) error {
	if role != "店长" {
		return nil
	}
	var exists bool
	var err error
	if distID == 0 {
		err = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM salesperson
			WHERE role='店长' AND enabled=true AND distributor_id IS NULL AND id<>$1)`, excludeID).Scan(&exists)
	} else {
		err = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM salesperson
			WHERE role='店长' AND enabled=true AND distributor_id=$1 AND id<>$2)`, distID, excludeID).Scan(&exists)
	}
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("该门店已有一位启用的店长——先把原店长改为店员或停用")
	}
	return nil
}

// POST /api/salespersons —— 新增（管理员）
func handleSalespersonCreate(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req struct {
		Name          string  `json:"name"`
		Sort          int     `json:"sort"`
		DistributorID int64   `json:"distributorId"` // 0=总部
		Role          string  `json:"role"`
		ManagerRate   float64 `json:"managerRate"`
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
	if req.Role == "" {
		req.Role = "店员"
	}
	if req.Role != "店员" && req.Role != "店长" {
		writeErr(w, 400, "角色必须是 店员 或 店长")
		return
	}
	if req.ManagerRate < 0 || req.ManagerRate > 100 {
		writeErr(w, 400, "抽成比例须在0~100之间")
		return
	}
	if err := checkOneManager(req.DistributorID, req.Role, 0); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var id int64
	err := db.QueryRow(`INSERT INTO salesperson (name, sort, distributor_id, role, manager_rate)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		req.Name, req.Sort, nullableID(req.DistributorID), req.Role, req.ManagerRate).Scan(&id)
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
		ID            int64    `json:"id"`
		Name          *string  `json:"name"`
		Sort          *int     `json:"sort"`
		Enabled       *bool    `json:"enabled"`
		DistributorID *int64   `json:"distributorId"`
		Role          *string  `json:"role"`
		ManagerRate   *float64 `json:"managerRate"`
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
	// v0.20：门店/角色/抽成比例——先算出更新后的最终值再做"一店一店长"校验
	if req.DistributorID != nil || req.Role != nil || req.ManagerRate != nil {
		var curDist sql.NullInt64
		var curRole string
		if err := db.QueryRow(`SELECT distributor_id, role FROM salesperson WHERE id=$1`, req.ID).
			Scan(&curDist, &curRole); err != nil {
			writeErr(w, 400, "售货员不存在")
			return
		}
		newDist, newRole := curDist.Int64, curRole
		if req.DistributorID != nil {
			newDist = *req.DistributorID
		}
		if req.Role != nil {
			if *req.Role != "店员" && *req.Role != "店长" {
				writeErr(w, 400, "角色必须是 店员 或 店长")
				return
			}
			newRole = *req.Role
		}
		if req.ManagerRate != nil && (*req.ManagerRate < 0 || *req.ManagerRate > 100) {
			writeErr(w, 400, "抽成比例须在0~100之间")
			return
		}
		if err := checkOneManager(newDist, newRole, req.ID); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if _, err := db.Exec(`UPDATE salesperson SET distributor_id=$1, role=$2 WHERE id=$3`,
			nullableID(newDist), newRole, req.ID); err != nil {
			writeErr(w, 500, "更新失败: "+err.Error())
			return
		}
		if req.ManagerRate != nil {
			if _, err := db.Exec(`UPDATE salesperson SET manager_rate=$1 WHERE id=$2`, *req.ManagerRate, req.ID); err != nil {
				writeErr(w, 500, "更新失败: "+err.Error())
				return
			}
		}
	}
	if req.Sort != nil {
		if _, err := db.Exec(`UPDATE salesperson SET sort=$1 WHERE id=$2`, *req.Sort, req.ID); err != nil {
			writeErr(w, 500, "更新失败: "+err.Error())
			return
		}
	}
	if req.Enabled != nil {
		if *req.Enabled { // 重新启用一个店长前也要过"一店一店长"校验
			var dist sql.NullInt64
			var role string
			if err := db.QueryRow(`SELECT distributor_id, role FROM salesperson WHERE id=$1`, req.ID).
				Scan(&dist, &role); err == nil {
				if err := checkOneManager(dist.Int64, role, req.ID); err != nil {
					writeErr(w, 400, err.Error())
					return
				}
			}
		}
		if _, err := db.Exec(`UPDATE salesperson SET enabled=$1 WHERE id=$2`, *req.Enabled, req.ID); err != nil {
			writeErr(w, 500, "更新失败: "+err.Error())
			return
		}
	}
	writeJSON(w, 200, map[string]any{"id": req.ID})
}

// ===== 提成规则与提成台账（v0.20） =====
// 规则：大类×结算方式 唯一；三种算法三选一。
// 台账 commission_flow 只追加：销售确认写入、销售反确认删除（当日重开）、销退确认写冲减负行。

// GET /api/commission-rules —— 管理员可读；返回全部版本，带派生状态（生效中/已失效/未生效）
func handleCommissionRuleList(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	rows, err := db.Query(`SELECT id, category, biz_type, mode, calc_type, value,
		to_char(valid_from,'YYYY-MM-DD'), COALESCE(to_char(valid_to,'YYYY-MM-DD'),''),
		CASE WHEN valid_from > CURRENT_DATE THEN '未生效'
		     WHEN valid_to IS NOT NULL AND valid_to < CURRENT_DATE THEN '已失效'
		     ELSE '生效中' END
		FROM commission_rule ORDER BY category, biz_type, mode, valid_from DESC`)
	if err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	defer rows.Close()
	type R struct {
		ID        int64   `json:"id"`
		Category  string  `json:"category"`
		BizType   string  `json:"bizType"`
		Mode      string  `json:"mode"`
		CalcType  string  `json:"calcType"`
		Value     float64 `json:"value"`
		ValidFrom string  `json:"validFrom"`
		ValidTo   string  `json:"validTo"`
		Status    string  `json:"status"`
	}
	list := []R{}
	for rows.Next() {
		var x R
		if err := rows.Scan(&x.ID, &x.Category, &x.BizType, &x.Mode, &x.CalcType, &x.Value,
			&x.ValidFrom, &x.ValidTo, &x.Status); err != nil {
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		list = append(list, x)
	}
	writeJSON(w, 200, map[string]any{"list": list})
}

var validCalcTypes = map[string]bool{"销售额百分比": true, "每克固定": true, "每件固定": true}

// POST /api/commission-rules —— 新增版本（管理员，v0.22）。
// "调整规则"=对同一维度再建一个版本：旧的开放版本自动在新版本生效前一天关闭。
// 历史版本不可修改——已入账的提成本就是快照，规则留痕让"当时按什么算的"永远可查。
func handleCommissionRuleCreate(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req struct {
		Category  string  `json:"category"`
		BizType   string  `json:"bizType"` // 正常销售/以旧换新/旧料回收，空=正常销售
		Mode      string  `json:"mode"`
		CalcType  string  `json:"calcType"`
		Value     float64 `json:"value"`
		ValidFrom string  `json:"validFrom"` // YYYY-MM-DD，空=今天
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数格式错误")
		return
	}
	var catOK bool
	_ = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM dict_item WHERE dict_type='category' AND name=$1)`,
		req.Category).Scan(&catOK)
	if !catOK {
		writeErr(w, 400, "首饰大类不存在")
		return
	}
	if req.BizType == "" {
		req.BizType = "正常销售"
	}
	switch req.BizType {
	case "正常销售":
		if req.Mode != "标签价" && req.Mode != "变金价" {
			writeErr(w, 400, "结算方式必须是 标签价 或 变金价")
			return
		}
		if req.Value <= 0 {
			writeErr(w, 400, "数值必须大于0")
			return
		}
	case "以旧换新":
		// 换新是"折扣"：通常配负值（抵减正常销售提成），也允许正值；不允许0
		req.Mode = ""
		if req.Value == 0 {
			writeErr(w, 400, "数值不能为0（换新折扣通常为负值，如 -6 元/克）")
			return
		}
	case "旧料回收":
		req.Mode = ""
		if req.Value <= 0 {
			writeErr(w, 400, "数值必须大于0")
			return
		}
	default:
		writeErr(w, 400, "业务类型必须是 正常销售/以旧换新/旧料回收")
		return
	}
	if !validCalcTypes[req.CalcType] {
		writeErr(w, 400, "计算方式必须是 销售额百分比/每克固定/每件固定")
		return
	}
	if req.CalcType == "销售额百分比" && (req.Value > 100 || req.Value < -100) {
		writeErr(w, 400, "百分比不能超过±100")
		return
	}
	if req.ValidFrom == "" {
		req.ValidFrom = time.Now().Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", req.ValidFrom); err != nil {
		writeErr(w, 400, "生效日期格式应为 YYYY-MM-DD")
		return
	}

	tx, err := db.Begin()
	if err != nil {
		writeErr(w, 500, "开启事务失败: "+err.Error())
		return
	}
	defer tx.Rollback()
	// 新版本必须晚于该维度现有最新版本的生效日（版本按生效日严格递增，区间才不重叠）
	var latestID int64
	var latestFrom string
	var latestTo sql.NullString
	err = tx.QueryRow(`SELECT id, to_char(valid_from,'YYYY-MM-DD'), to_char(valid_to,'YYYY-MM-DD')
		FROM commission_rule WHERE category=$1 AND mode=$2 AND biz_type=$3
		ORDER BY valid_from DESC, id DESC LIMIT 1 FOR UPDATE`, req.Category, req.Mode, req.BizType).
		Scan(&latestID, &latestFrom, &latestTo)
	if err != nil && err != sql.ErrNoRows {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	if err == nil {
		if latestFrom > req.ValidFrom {
			writeErr(w, 400, fmt.Sprintf("生效日期不能早于现有最新版本的生效日（%s）", latestFrom))
			return
		}
		// 旧开放版本（或失效日晚于新版本生效日的）自动关闭在新版本前一天。
		// 同日调整（latestFrom == validFrom）：旧版本区间变空=当即作废，只留痕——
		// 已确认单据的提成是快照，不受影响。
		if !latestTo.Valid || latestTo.String >= req.ValidFrom {
			if _, err := tx.Exec(`UPDATE commission_rule SET valid_to = $1::date - 1 WHERE id=$2`,
				req.ValidFrom, latestID); err != nil {
				writeErr(w, 500, "关闭旧版本失败: "+err.Error())
				return
			}
		}
	}
	var id int64
	if err := tx.QueryRow(`INSERT INTO commission_rule (category, mode, biz_type, calc_type, value, valid_from)
		VALUES ($1,$2,$3,$4,$5,$6::date) RETURNING id`,
		req.Category, req.Mode, req.BizType, req.CalcType, req.Value, req.ValidFrom).Scan(&id); err != nil {
		writeErr(w, 500, "创建失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id})
}

// POST /api/commission-rules/update —— 仅两种动作（管理员，v0.22）：
//   {id, validTo}：结束一个开放版本（此后该维度没有提成，直到建新版本）；
//   {id, cancel:true}：取消一个还没生效的排期版本（删除）。
// 数值/算法不可改——调整请新建版本。
func handleCommissionRuleUpdate(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req struct {
		ID      int64  `json:"id"`
		ValidTo string `json:"validTo"`
		Cancel  bool   `json:"cancel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == 0 {
		writeErr(w, 400, "参数错误")
		return
	}
	if req.Cancel {
		// 取消排期要把"被它顶掉"的上一版本重新打开——否则排期日起是无规则真空期，静默没提成
		tx, err := db.Begin()
		if err != nil {
			writeErr(w, 500, "开启事务失败: "+err.Error())
			return
		}
		defer tx.Rollback()
		var cat, mode, biz, from string
		err = tx.QueryRow(`DELETE FROM commission_rule WHERE id=$1 AND valid_from > CURRENT_DATE
			RETURNING category, mode, biz_type, to_char(valid_from,'YYYY-MM-DD')`, req.ID).Scan(&cat, &mode, &biz, &from)
		if err == sql.ErrNoRows {
			writeErr(w, 400, "只能取消还没生效的排期版本——已生效的版本请用\"结束\"关闭")
			return
		}
		if err != nil {
			writeErr(w, 500, "取消失败: "+err.Error())
			return
		}
		// 上一版本若恰好结束在排期前一天（自动关闭留下的痕迹），重新打开
		if _, err := tx.Exec(`UPDATE commission_rule SET valid_to = NULL
			WHERE id = (SELECT id FROM commission_rule WHERE category=$1 AND mode=$2 AND biz_type=$3
				AND valid_to = $4::date - 1
				ORDER BY valid_from DESC, id DESC LIMIT 1)`, cat, mode, biz, from); err != nil {
			writeErr(w, 500, "恢复上一版本失败: "+err.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			writeErr(w, 500, "提交失败: "+err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"deleted": req.ID})
		return
	}
	if req.ValidTo == "" {
		writeErr(w, 400, "请指定失效日期")
		return
	}
	if _, err := time.Parse("2006-01-02", req.ValidTo); err != nil {
		writeErr(w, 400, "失效日期格式应为 YYYY-MM-DD")
		return
	}
	res, err := db.Exec(`UPDATE commission_rule SET valid_to=$1::date
		WHERE id=$2 AND valid_to IS NULL AND valid_from <= $1::date`, req.ValidTo, req.ID)
	if err != nil {
		writeErr(w, 500, "更新失败: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 400, "结束失败：该版本不存在、已有失效日，或失效日早于生效日")
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID})
}

// GET /api/commission-report?from=YYYY-MM-DD&to=YYYY-MM-DD —— 提成报表（管理员）
// 报表=对台账做SUM，没有任何新的计算逻辑；台账是确认时刻的快照，报表永远可复算。
func handleCommissionReport(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	from := strings.TrimSpace(r.URL.Query().Get("from"))
	to := strings.TrimSpace(r.URL.Query().Get("to"))
	if from == "" || to == "" {
		writeErr(w, 400, "请指定起止日期")
		return
	}
	rows, err := db.Query(`SELECT sp.id, sp.name, COALESCE(d.name,'总部'), sp.role,
		COALESCE(SUM(cf.amount) FILTER (WHERE cf.kind='销售'), 0),
		COALESCE(SUM(cf.amount) FILTER (WHERE cf.kind IN ('以旧换新','旧料回收')), 0),
		COALESCE(SUM(cf.amount) FILTER (WHERE cf.kind='店长抽成'), 0),
		COALESCE(SUM(cf.amount) FILTER (WHERE cf.kind='销退冲减'), 0),
		COALESCE(SUM(cf.amount), 0),
		COUNT(DISTINCT cf.doc_id) FILTER (WHERE cf.kind='销售')
		FROM commission_flow cf
		JOIN salesperson sp ON sp.id = cf.salesperson_id
		LEFT JOIN distributor d ON d.id = sp.distributor_id
		WHERE cf.created_at >= $1::date AND cf.created_at < ($2::date + 1)
		GROUP BY sp.id, sp.name, d.name, sp.role
		ORDER BY 8 DESC`, from, to)
	if err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	defer rows.Close()
	type Row struct {
		SalespersonID int64   `json:"salespersonId"`
		Name          string  `json:"name"`
		StoreName     string  `json:"storeName"`
		Role          string  `json:"role"`
		SaleComm      float64 `json:"saleComm"`
		TradeComm     float64 `json:"tradeComm"` // 以旧换新(负)+旧料回收(正)
		ManagerComm   float64 `json:"managerComm"`
		ReturnOffset  float64 `json:"returnOffset"`
		Net           float64 `json:"net"`
		DocCount      int     `json:"docCount"`
	}
	list := []Row{}
	var tSale, tTrade, tMgr, tRet, tNet float64
	for rows.Next() {
		var x Row
		if err := rows.Scan(&x.SalespersonID, &x.Name, &x.StoreName, &x.Role,
			&x.SaleComm, &x.TradeComm, &x.ManagerComm, &x.ReturnOffset, &x.Net, &x.DocCount); err != nil {
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		tSale += x.SaleComm
		tTrade += x.TradeComm
		tMgr += x.ManagerComm
		tRet += x.ReturnOffset
		tNet += x.Net
		list = append(list, x)
	}
	writeJSON(w, 200, map[string]any{"list": list,
		"totalSale": round2(tSale), "totalTrade": round2(tTrade), "totalManager": round2(tMgr),
		"totalReturn": round2(tRet), "totalNet": round2(tNet)})
}

// oldCommission 旧料业务（以旧换新/旧料回收）的提成：按克重或按抵扣金额（v0.24）。
// 以旧换新的规则通常是负值（折扣），引擎只管按规则算、照实入账。
func oldCommission(tx *sql.Tx, category, bizType string, grams, amountBase float64) (float64, error) {
	var calcType string
	var value float64
	err := tx.QueryRow(`SELECT calc_type, value FROM commission_rule
		WHERE category=$1 AND biz_type=$2
		AND valid_from <= CURRENT_DATE
		AND (valid_to IS NULL OR valid_to >= CURRENT_DATE)
		ORDER BY valid_from DESC, id DESC LIMIT 1`, category, bizType).Scan(&calcType, &value)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	switch calcType {
	case "销售额百分比":
		return round2(amountBase * value / 100), nil
	case "每克固定":
		return round2(grams * value), nil
	case "每件固定":
		return round2(value), nil
	}
	return 0, nil
}

// postCommission 把一笔提成总额按"多人平分+店长抽成"写进台账（正负通吃，v0.24）
func postCommission(tx *sql.Tx, docID int64, barcode, kind string, totalC float64, sps []spInfo, mgr *spInfo) error {
	if totalC == 0 || len(sps) == 0 {
		return nil
	}
	share := round2(totalC / float64(len(sps)))
	for _, s := range sps {
		amt := share
		if s.Role == "店员" && mgr != nil && mgr.ManagerRate > 0 {
			cut := round2(share * mgr.ManagerRate / 100)
			amt = round2(share - cut)
			if cut != 0 {
				if _, err := tx.Exec(`INSERT INTO commission_flow (doc_id, barcode, salesperson_id, kind, amount)
					VALUES ($1,$2,$3,'店长抽成',$4)`, docID, barcode, mgr.ID, cut); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(`INSERT INTO commission_flow (doc_id, barcode, salesperson_id, kind, amount)
			VALUES ($1,$2,$3,$4,$5)`, docID, barcode, s.ID, kind, amt); err != nil {
			return err
		}
	}
	return nil
}

// spInfo 确认时用到的售货员信息
type spInfo struct {
	ID          int64
	Name        string
	StoreID     int64 // 0=总部
	Role        string
	ManagerRate float64
	Enabled     bool
}

func loadSalesperson(q rowQuerier, id int64) (spInfo, error) {
	var s spInfo
	var did sql.NullInt64
	err := q.QueryRow(`SELECT id, name, distributor_id, role, manager_rate, enabled
		FROM salesperson WHERE id=$1`, id).Scan(&s.ID, &s.Name, &did, &s.Role, &s.ManagerRate, &s.Enabled)
	s.StoreID = did.Int64
	return s, err
}

// lineCommission 按"今天生效的版本"算一件货的提成总额；没有生效版本 = 0（不报错）。
// v0.22：规则带生效区间，取 valid_from<=今天<=valid_to(或开放) 中最新的一版。
func lineCommission(tx *sql.Tx, category, mode string, soldPrice, weightG float64) (float64, error) {
	var calcType string
	var value float64
	err := tx.QueryRow(`SELECT calc_type, value FROM commission_rule
		WHERE category=$1 AND mode=$2 AND biz_type='正常销售'
		AND valid_from <= CURRENT_DATE
		AND (valid_to IS NULL OR valid_to >= CURRENT_DATE)
		ORDER BY valid_from DESC, id DESC LIMIT 1`, category, mode).Scan(&calcType, &value)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	switch calcType {
	case "销售额百分比":
		return round2(soldPrice * value / 100), nil
	case "每克固定":
		return round2(weightG * value), nil
	case "每件固定":
		return round2(value), nil
	}
	return 0, nil
}

// ===== 首饰退库单（v0.7）—— 按"新模块checklist"添加的第一个单据 =====
// 业务：把总库在库的件退回供应商。件状态：在库 →(确认)→ 已退库 →(反确认)→ 在库。
// 并发裁决复用同一模式：确认时对每件做条件更新 WHERE status='在库'，
// 若某件已被其他单据占用，整单回滚并指明条码。

// POST /api/doc/outbound/save  {id?, supplier, barcodes[]}
func handleOutboundSave(w http.ResponseWriter, r *http.Request) {
	if !requireHQ(w, r) { // v0.19：入库/退库/调拨是总部职能
		return
	}
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
	if !requireHQ(w, r) { // v0.19：入库/退库/调拨是总部职能
		return
	}
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
	if !requireHQ(w, r) { // v0.19：入库/退库/调拨是总部职能
		return
	}
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
	if !requireHQ(w, r) { // v0.19：入库/退库/调拨是总部职能
		return
	}
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
	if currentStore(r) != 0 { // v0.19：门店账号无此视图
		writeJSON(w, 200, map[string]any{"list": []any{}, "total": 0})
		return
	}
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
		SELECT DISTINCT ON (gp.purity) gp.purity, gp.retail_price, gp.recycle_price, gp.trade_price,
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
		TradePrice   float64 `json:"tradePrice"`
		By           string  `json:"by"`
		At           string  `json:"at"`
	}
	list := []P{}
	for rows.Next() {
		var x P
		if err := rows.Scan(&x.Purity, &x.RetailPrice, &x.RecyclePrice, &x.TradePrice, &x.By, &x.At); err != nil {
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
		SELECT gp.purity, gp.retail_price, gp.recycle_price, gp.trade_price, u.name,
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
		TradePrice   float64 `json:"tradePrice"`
		By           string  `json:"by"`
		At           string  `json:"at"`
	}
	list := []P{}
	for rows.Next() {
		var x P
		if err := rows.Scan(&x.Purity, &x.RetailPrice, &x.RecyclePrice, &x.TradePrice, &x.By, &x.At); err != nil {
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
		TradePrice   float64 `json:"tradePrice"` // v0.24：以旧换新价（给顾客旧料的折价）
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
	// v0.24：换新价可不填（0=该成色不支持换新）；填了必须落在 回收价~零售价 之间——
	// 换新本质是"回收价上稍微加一点"，高过零售价或低于回收价都是手滑
	if req.TradePrice != 0 {
		if req.TradePrice < req.RecyclePrice {
			writeErr(w, 400, "换新价不应低于回收价，请核对")
			return
		}
		if req.TradePrice > req.RetailPrice {
			writeErr(w, 400, "换新价不应高于零售金价，请核对")
			return
		}
	}
	var id int64
	if err := db.QueryRow(`INSERT INTO gold_price (purity, retail_price, recycle_price, trade_price, published_by)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		req.Purity, req.RetailPrice, req.RecyclePrice, req.TradePrice, currentUID(r)).Scan(&id); err != nil {
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
	// v0.27：销售工费快照（变金价参考价 = 克重×金价 + 工费，四舍五入到元）
	SaleFeeMode string  `json:"saleFeeMode,omitempty"`
	SaleFee     float64 `json:"saleFee,omitempty"`
}

// OldLine 旧料行（v0.24 以旧换新/旧料回收）：旧料不是系统货品，没有条码，
// 只记 大类（换新额度按大类匹配：金换金银换银）、成色（定价按成色）、克重。
// TradeG/RecycleG/各价格在确认时拆分并快照——此前都是预览。
type OldLine struct {
	Category     string  `json:"category"`
	Purity       string  `json:"purity"`
	WeightG      float64 `json:"weightG"`
	TradeG       float64 `json:"tradeG,omitempty"`
	RecycleG     float64 `json:"recycleG,omitempty"`
	TradePrice   float64 `json:"tradePrice,omitempty"`
	RecyclePrice float64 `json:"recyclePrice,omitempty"`
	Credit       float64 `json:"credit,omitempty"` // 本行抵扣金额
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
		ID             int64     `json:"id"`
		SalespersonIDs []int64   `json:"salespersonIds"` // v0.20：1~3人整单平分
		Payments       []PayLine `json:"payments"`
		OldLines       []OldLine `json:"oldLines"` // v0.24：旧料行（以旧换新/回收）
		Lines          []struct {
			Barcode   string  `json:"barcode"`
			Mode      string  `json:"mode"`
			SoldPrice float64 `json:"soldPrice"`
		} `json:"lines"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Lines) == 0 {
		writeErr(w, 400, "参数错误：至少要有一行新品（纯旧料回收单据后续另做）")
		return
	}
	// v0.24 旧料行基础校验：大类/成色是启用字典项、克重>0（拆分与定价在确认时做）
	for i, ol := range req.OldLines {
		var catOK, puOK bool
		_ = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM dict_item WHERE dict_type='category' AND name=$1 AND enabled=true)`, ol.Category).Scan(&catOK)
		_ = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM dict_item WHERE dict_type='purity' AND name=$1 AND enabled=true)`, ol.Purity).Scan(&puOK)
		if !catOK {
			writeErr(w, 400, fmt.Sprintf("旧料第%d行大类「%s」不存在或已停用", i+1, ol.Category))
			return
		}
		if !puOK {
			writeErr(w, 400, fmt.Sprintf("旧料第%d行成色「%s」不存在或已停用", i+1, ol.Purity))
			return
		}
		if ol.WeightG <= 0 {
			writeErr(w, 400, fmt.Sprintf("旧料第%d行克重必须大于0", i+1))
			return
		}
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
	// v0.20：售货员1~3人，挂单可先不选（确认时强制）；重复剔除
	spSeen := map[int64]bool{}
	spIDs := []int64{}
	for _, id := range req.SalespersonIDs {
		if id > 0 && !spSeen[id] {
			spSeen[id] = true
			spIDs = append(spIDs, id)
		}
	}
	if len(spIDs) > 3 {
		writeErr(w, 400, "售货员最多3人")
		return
	}
	spIDsJSON, _ := json.Marshal(spIDs)
	if req.OldLines == nil {
		req.OldLines = []OldLine{}
	}
	oldJSON, _ := json.Marshal(req.OldLines)

	// v0.15 挂单即锁定：保存草稿这一刻就把货品 在库→销售中，并发裁决从"确认"提前到"挂单"。
	// 解锁+锁定+存单在同一个事务里——要么全成，要么全不动。
	store := currentStore(r) // v0.19：0=总部卖总库，否则卖本店
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
		var docStore sql.NullInt64
		err := tx.QueryRow(`SELECT draft_lines, distributor_id FROM doc
			WHERE id=$1 AND doc_type='sale' AND status='草稿' FOR UPDATE`, req.ID).Scan(&old, &docStore)
		if err == sql.ErrNoRows {
			writeErr(w, 400, "该单据不是草稿状态（可能已被确认或删除），请刷新")
			return
		}
		if err != nil {
			writeErr(w, 500, "读取单据失败: "+err.Error())
			return
		}
		if store != 0 && docStore.Int64 != store {
			writeErr(w, 403, "不能操作其他门店的单据")
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
		err := tx.QueryRow(`SELECT name, purity, weight_g, price, labor_fee_mode, labor_fee
			FROM item WHERE barcode=$1`, sl.Barcode).
			Scan(&sl.Name, &sl.Purity, &sl.WeightG, &sl.Price, &sl.SaleFeeMode, &sl.SaleFee)
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
		// v0.19：销售只能卖"本账号所在位置"的货——总部账号卖总库，门店账号卖本店
		var res sql.Result
		if store == 0 {
			res, err = tx.Exec(`UPDATE item SET status='销售中', version=version+1
				WHERE barcode=$1 AND status='在库' AND location_type='总库'`, sl.Barcode)
		} else {
			res, err = tx.Exec(`UPDATE item SET status='销售中', version=version+1
				WHERE barcode=$1 AND status='在库' AND location_type='分销商' AND distributor_id=$2`,
				sl.Barcode, store)
		}
		if err != nil {
			writeErr(w, 500, "锁定货品失败: "+err.Error())
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var cur string
			_ = tx.QueryRow(`SELECT status FROM item WHERE barcode=$1`, sl.Barcode).Scan(&cur)
			if cur == "在库" {
				writeErr(w, 400, fmt.Sprintf("条码 %s 在「%s」处——只能销售「%s」的货，请先调拨", sl.Barcode, itemLocName(tx, sl.Barcode), locName(tx, store)))
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
		err = tx.QueryRow(`INSERT INTO doc (doc_no, doc_type, status, maker_id, draft_lines, salesperson_ids, payments, old_lines, distributor_id)
			VALUES ($1,'sale','草稿',$2,$3,$4,$5,$6,$7) RETURNING id`, docNo, currentUID(r), linesJSON, spIDsJSON, paysJSON,
			oldJSON, nullableID(store)).Scan(&docID)
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
	if _, err := tx.Exec(`UPDATE doc SET draft_lines=$1, salesperson_ids=$2, payments=$3, old_lines=$4
		WHERE id=$5`, linesJSON, spIDsJSON, paysJSON, oldJSON, req.ID); err != nil {
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

	// v0.19：门店账号只能确认本店的销售单
	if store := currentStore(r); store != 0 {
		var ds sql.NullInt64
		if err := tx.QueryRow(`SELECT distributor_id FROM doc WHERE id=$1 AND doc_type='sale'`,
			req.ID).Scan(&ds); err != nil || ds.Int64 != store {
			writeErr(w, 403, "不能操作其他门店的单据")
			return
		}
	}
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
	var linesJSON, paysJSON, spIDsJSON, oldJSON []byte
	var legacySpID, docStore sql.NullInt64
	if err := tx.QueryRow(`SELECT draft_lines, salesperson_ids, salesperson_id, payments, old_lines, distributor_id
		FROM doc WHERE id=$1`, req.ID).Scan(&linesJSON, &spIDsJSON, &legacySpID, &paysJSON, &oldJSON, &docStore); err != nil {
		writeErr(w, 500, "读取单据失败: "+err.Error())
		return
	}
	var lines []SaleLine
	if err := json.Unmarshal(linesJSON, &lines); err != nil || len(lines) == 0 {
		writeErr(w, 400, "草稿明细为空，无法确认")
		return
	}

	// 确认时硬校验一（v0.20多人版）：售货员1~3人、在职启用、且归属与单据门店一致
	var spIDs []int64
	_ = json.Unmarshal(spIDsJSON, &spIDs)
	if len(spIDs) == 0 && legacySpID.Valid { // 兼容多人版之前的老草稿
		spIDs = []int64{legacySpID.Int64}
	}
	if len(spIDs) == 0 {
		writeErr(w, 400, "请先选择售货员再确认收款")
		return
	}
	if len(spIDs) > 3 {
		writeErr(w, 400, "售货员最多3人")
		return
	}
	sps := []spInfo{}
	for _, id := range spIDs {
		s, err := loadSalesperson(tx, id)
		if err != nil {
			writeErr(w, 400, "售货员不存在，请重新选择")
			return
		}
		if !s.Enabled {
			writeErr(w, 400, fmt.Sprintf("售货员「%s」已停用，请重新选择", s.Name))
			return
		}
		if s.StoreID != docStore.Int64 {
			writeErr(w, 400, fmt.Sprintf("售货员「%s」属于「%s」，不能在「%s」的销售单上记提成",
				s.Name, locName(tx, s.StoreID), locName(tx, docStore.Int64)))
			return
		}
		sps = append(sps, s)
	}
	// 本单涉及门店的店长（店员份额被抽成的去向）；店长自己卖货不被抽
	var mgr *spInfo
	{
		var m spInfo
		var did sql.NullInt64
		var err error
		if docStore.Int64 == 0 {
			err = tx.QueryRow(`SELECT id, name, distributor_id, role, manager_rate, enabled FROM salesperson
				WHERE role='店长' AND enabled=true AND distributor_id IS NULL`).
				Scan(&m.ID, &m.Name, &did, &m.Role, &m.ManagerRate, &m.Enabled)
		} else {
			err = tx.QueryRow(`SELECT id, name, distributor_id, role, manager_rate, enabled FROM salesperson
				WHERE role='店长' AND enabled=true AND distributor_id=$1`, docStore.Int64).
				Scan(&m.ID, &m.Name, &did, &m.Role, &m.ManagerRate, &m.Enabled)
		}
		if err == nil {
			m.StoreID = did.Int64
			mgr = &m
		}
	}
	// 确认时硬校验二：收款明细合法，且每种方式都是启用的字典项
	// （v0.24：是否必须录收款，要等算出"净应收"才知道——两讫校验挪到后面）
	var pays []PayLine
	_ = json.Unmarshal(paysJSON, &pays)
	var oldLines []OldLine
	_ = json.Unmarshal(oldJSON, &oldLines)
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
		Barcode    string  `json:"barcode"`
		Mode       string  `json:"mode"`
		WeightG    float64 `json:"weightG"`
		LabelPrice float64 `json:"labelPrice"`
		GoldRate   float64 `json:"goldRate"`  // 变金价行：确认时金价
		FeeMode    string  `json:"feeMode"`   // v0.27：销售工费方式
		Fee        float64 `json:"fee"`       // 工费单价（按克=元/克；按件=元/件）
		FeeAmt     float64 `json:"feeAmt"`    // 工费金额
		RefPrice   float64 `json:"refPrice"`  // 系统参考价 = 克重×金价 + 工费（四舍五入到元）
		SoldPrice  float64 `json:"soldPrice"` // 实际成交价
	}
	snaps := []snapLine{}
	var total float64
	newWeightByCat := map[string]float64{} // v0.24：换新额度=本单各大类新品克重
	for i, l := range lines {
		// v0.15：正常流程里件已在挂单时锁定，这里 销售中→已售；
		// 兼容 v0.14 之前保存的老草稿（件还是在库），所以两种来源状态都接受。
		// FOR UPDATE 锁行后读状态再改——既是并发裁决，又能把真实的来源状态记进流水。
		var itemID int64
		var fromSt, itemCat string
		err := tx.QueryRow(`SELECT id, status, category FROM item WHERE barcode=$1 FOR UPDATE`, l.Barcode).
			Scan(&itemID, &fromSt, &itemCat)
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
			LabelPrice: l.Price, SoldPrice: l.SoldPrice,
			FeeMode: l.SaleFeeMode, Fee: l.SaleFee}
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
			sp.FeeAmt = feeAmount(l.SaleFeeMode, l.SaleFee, l.WeightG)
			// JMP同款口径：参考价四舍五入到元（2.46×(1169+78)=3067.62→3068）
			sp.RefPrice = math.Round(l.WeightG*rate + sp.FeeAmt)
		} else {
			sp.RefPrice = l.Price
		}
		snaps = append(snaps, sp)
		total += l.SoldPrice
		newWeightByCat[itemCat] += l.WeightG
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
		// v0.20 提成入账：按规则算这件货的提成总额 → 几个售货员平分 →
		// 店员份额被本店店长抽 manager_rate%（店长自己卖货拿全额）。没配规则=没提成。
		cTotal, err := lineCommission(tx, itemCat, l.Mode, l.SoldPrice, l.WeightG)
		if err != nil {
			writeErr(w, 500, "提成规则查询失败: "+err.Error())
			return
		}
		if cTotal > 0 {
			share := round2(cTotal / float64(len(sps)))
			for _, s := range sps {
				amt := share
				if s.Role == "店员" && mgr != nil && mgr.ManagerRate > 0 {
					cut := round2(share * mgr.ManagerRate / 100)
					amt = round2(share - cut)
					if cut > 0 {
						if _, err := tx.Exec(`INSERT INTO commission_flow (doc_id, barcode, salesperson_id, kind, amount)
							VALUES ($1,$2,$3,'店长抽成',$4)`, req.ID, l.Barcode, mgr.ID, cut); err != nil {
							writeErr(w, 500, "提成入账失败: "+err.Error())
							return
						}
					}
				}
				if _, err := tx.Exec(`INSERT INTO commission_flow (doc_id, barcode, salesperson_id, kind, amount)
					VALUES ($1,$2,$3,'销售',$4)`, req.ID, l.Barcode, s.ID, amt); err != nil {
					writeErr(w, 500, "提成入账失败: "+err.Error())
					return
				}
			}
		}
	}
	// ===== v0.24 旧料拆分与抵扣：金换金银换银，额度=本单该大类新品克重 =====
	usedByCat := map[string]float64{}
	var credit float64
	for i := range oldLines {
		ol := &oldLines[i]
		avail := newWeightByCat[ol.Category] - usedByCat[ol.Category]
		if avail < 0 {
			avail = 0
		}
		ol.TradeG = ol.WeightG
		if ol.TradeG > avail {
			ol.TradeG = avail
		}
		ol.RecycleG = round2(ol.WeightG - ol.TradeG)
		ol.TradeG = round2(ol.TradeG)
		usedByCat[ol.Category] += ol.TradeG
		// 价格快照：按成色取当日换新价/回收价
		var tp, rp float64
		err := tx.QueryRow(`SELECT trade_price, recycle_price FROM gold_price
			WHERE purity=$1 ORDER BY id DESC LIMIT 1`, ol.Purity).Scan(&tp, &rp)
		if err == sql.ErrNoRows {
			writeErr(w, 400, fmt.Sprintf("旧料成色「%s」今日未发布金价，整单未确认", ol.Purity))
			return
		}
		if err != nil {
			writeErr(w, 500, "金价查询失败: "+err.Error())
			return
		}
		if ol.TradeG > 0 && tp <= 0 {
			writeErr(w, 400, fmt.Sprintf("成色「%s」未发布换新价，不能以旧换新，整单未确认", ol.Purity))
			return
		}
		if ol.RecycleG > 0 && rp <= 0 {
			writeErr(w, 400, fmt.Sprintf("成色「%s」未发布回收价，不能回收旧料，整单未确认", ol.Purity))
			return
		}
		ol.TradePrice, ol.RecyclePrice = tp, rp
		ol.Credit = round2(ol.TradeG*tp + ol.RecycleG*rp)
		credit += ol.Credit
		// 旧料提成：换新部分（规则通常为负=折扣）+ 回收部分（正提成）
		if ol.TradeG > 0 {
			c, e := oldCommission(tx, ol.Category, "以旧换新", ol.TradeG, round2(ol.TradeG*tp))
			if e != nil {
				writeErr(w, 500, "提成规则查询失败: "+e.Error())
				return
			}
			if e := postCommission(tx, req.ID, "旧料", "以旧换新", c, sps, mgr); e != nil {
				writeErr(w, 500, "提成入账失败: "+e.Error())
				return
			}
		}
		if ol.RecycleG > 0 {
			c, e := oldCommission(tx, ol.Category, "旧料回收", ol.RecycleG, round2(ol.RecycleG*rp))
			if e != nil {
				writeErr(w, 500, "提成规则查询失败: "+e.Error())
				return
			}
			if e := postCommission(tx, req.ID, "旧料", "旧料回收", c, sps, mgr); e != nil {
				writeErr(w, 500, "提成入账失败: "+e.Error())
				return
			}
		}
	}

	// 确认时硬校验三：两讫——净应收>0 收款合计须相等；净应收<0（倒付顾客）退款合计须相等；
	// 净应收恰好为0（旧料抵扣=货款）允许不录收款。
	net := round2(total - credit)
	var paySum float64
	for _, p := range pays {
		paySum += p.Amount
	}
	paySum = round2(paySum)
	switch {
	case net > 0:
		if len(pays) == 0 {
			writeErr(w, 400, "请先录入收款方式再确认收款")
			return
		}
		if paySum != net {
			writeErr(w, 400, fmt.Sprintf("收款合计 ¥%.2f 与净应收 ¥%.2f 不一致（货款%.2f−旧料抵扣%.2f），整单未确认",
				paySum, net, total, credit))
			return
		}
	case net < 0:
		if len(pays) == 0 {
			writeErr(w, 400, fmt.Sprintf("旧料抵扣超过货款，应退顾客 ¥%.2f——请录入退款方式再确认", -net))
			return
		}
		if paySum != -net {
			writeErr(w, 400, fmt.Sprintf("退款合计 ¥%.2f 与应退顾客 ¥%.2f 不一致，整单未确认", paySum, -net))
			return
		}
	default:
		if paySum != 0 {
			writeErr(w, 400, "旧料抵扣恰好等于货款（两讫），不应再录收款金额")
			return
		}
	}

	snapshot, _ := json.Marshal(map[string]any{"rates": rates, "lines": snaps,
		"oldLines": oldLines, "credit": round2(credit), "itemTotal": round2(total)})
	processedOld, _ := json.Marshal(oldLines)
	if _, err = tx.Exec(`UPDATE doc SET total_amount=$1, gold_price_snapshot=$2, old_lines=$3 WHERE id=$4`,
		net, snapshot, processedOld, req.ID); err != nil {
		writeErr(w, 500, "写入合计失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "status": "已确认",
		"totalAmount": net, "itemTotal": round2(total), "credit": round2(credit)})
}

// round2 四舍五入到分。踩坑10：旧实现 int64(v*100+0.5) 对负数向零截断，
// round2(-30) 会得到 -29.99——销退冲减出现负数合计后才暴露。math.Round 对正负都正确。
func round2(v float64) float64 {
	return math.Round(v*100) / 100
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
	if store := currentStore(r); store != 0 {
		var ds sql.NullInt64
		if err := tx.QueryRow(`SELECT distributor_id FROM doc WHERE id=$1 AND doc_type='sale'`,
			req.ID).Scan(&ds); err != nil || ds.Int64 != store {
			writeErr(w, 403, "不能操作其他门店的单据")
			return
		}
	}
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
	// v0.20：当日反确认=单据重开，确认时入账的提成一并撤销
	if _, err := tx.Exec(`DELETE FROM commission_flow WHERE doc_id=$1`, req.ID); err != nil {
		writeErr(w, 500, "撤销提成失败: "+err.Error())
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
	var docStore sql.NullInt64
	err = tx.QueryRow(`SELECT draft_lines, distributor_id FROM doc
		WHERE id=$1 AND doc_type='sale' AND status='草稿' FOR UPDATE`, req.ID).Scan(&old, &docStore)
	if err == sql.ErrNoRows {
		writeErr(w, 400, "删除失败：只有草稿可以删除")
		return
	}
	if err != nil {
		writeErr(w, 500, "读取单据失败: "+err.Error())
		return
	}
	if store := currentStore(r); store != 0 && docStore.Int64 != store {
		writeErr(w, 403, "不能操作其他门店的单据")
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
	// v0.19：门店账号只看本店的销售单
	cond, args := "", []any{}
	if store := currentStore(r); store != 0 {
		cond, args = " AND d.distributor_id=$1", []any{store}
	}
	// 售货员名字查一次装进map，列表循环里只查内存
	spNames := map[int64]string{}
	if nrows, err := db.Query(`SELECT id, name FROM salesperson`); err == nil {
		for nrows.Next() {
			var id int64
			var n string
			_ = nrows.Scan(&id, &n)
			spNames[id] = n
		}
		nrows.Close()
	}
	rows, err := db.Query(`SELECT d.id, d.doc_no, d.status, d.draft_lines, d.total_amount,
		d.salesperson_ids, d.payments, d.old_lines,
		to_char(d.created_at,'YYYY-MM-DD HH24:MI:SS')
		FROM doc d
		WHERE d.doc_type='sale'`+cond+` ORDER BY d.id DESC LIMIT 20`, args...)
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
		SalespersonIDs  []int64    `json:"salespersonIds"`
		SalespersonName string     `json:"salespersonName"` // 多人用"、"连接
		Payments        []PayLine  `json:"payments"`
		OldLines        []OldLine  `json:"oldLines"`
		Lines           []SaleLine `json:"lines"`
		MadeAt          string     `json:"madeAt"`
	}
	docs := []SDoc{}
	for rows.Next() {
		var d SDoc
		var dl, pj, spj, oj []byte
		if err := rows.Scan(&d.ID, &d.DocNo, &d.Status, &dl, &d.TotalAmount,
			&spj, &pj, &oj, &d.MadeAt); err != nil {
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		d.SalespersonIDs = []int64{}
		_ = json.Unmarshal(spj, &d.SalespersonIDs)
		names := []string{}
		for _, id := range d.SalespersonIDs {
			if n, ok := spNames[id]; ok {
				names = append(names, n)
			}
		}
		d.SalespersonName = strings.Join(names, "、")
		d.Lines = []SaleLine{}
		_ = json.Unmarshal(dl, &d.Lines)
		d.Payments = []PayLine{}
		_ = json.Unmarshal(pj, &d.Payments)
		d.OldLines = []OldLine{}
		_ = json.Unmarshal(oj, &d.OldLines)
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
	var status, lt string
	var did sql.NullInt64
	err := db.QueryRow(`SELECT name, purity, weight_g, status, location_type, distributor_id
		FROM item WHERE barcode=$1`, bc).
		Scan(&sl.Name, &sl.Purity, &sl.WeightG, &status, &lt, &did)
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
	// v0.19：只能销退"本账号所在位置"卖出的件
	store := currentStore(r)
	itemStore := int64(0)
	if lt != "总库" {
		itemStore = did.Int64
	}
	if itemStore != store {
		writeErr(w, 400, fmt.Sprintf("条码 %s 是「%s」卖出的件，请在该处办理销退", bc, locName(db, itemStore)))
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
		var docStore sql.NullInt64
		err := tx.QueryRow(`SELECT draft_lines, distributor_id FROM doc
			WHERE id=$1 AND doc_type='sale_return' AND status='草稿' FOR UPDATE`, req.ID).Scan(&old, &docStore)
		if err == sql.ErrNoRows {
			writeErr(w, 400, "该单据不是草稿状态（可能已被确认或删除），请刷新")
			return
		}
		if err != nil {
			writeErr(w, 500, "读取单据失败: "+err.Error())
			return
		}
		if store := currentStore(r); store != 0 && docStore.Int64 != store {
			writeErr(w, 403, "不能操作其他门店的单据")
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
		// v0.19：锁定时同样限定位置——只能收本账号所在位置卖出的件
		var res sql.Result
		if store := currentStore(r); store == 0 {
			res, err = tx.Exec(`UPDATE item SET status='退货中', version=version+1
				WHERE barcode=$1 AND status='已售' AND location_type='总库'`, sl.Barcode)
		} else {
			res, err = tx.Exec(`UPDATE item SET status='退货中', version=version+1
				WHERE barcode=$1 AND status='已售' AND location_type='分销商' AND distributor_id=$2`,
				sl.Barcode, store)
		}
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
		err = tx.QueryRow(`INSERT INTO doc (doc_no, doc_type, status, maker_id, draft_lines, payments, distributor_id)
			VALUES ($1,'sale_return','草稿',$2,$3,$4,$5) RETURNING id`,
			docNo, currentUID(r), linesJSON, paysJSON, nullableID(currentStore(r))).Scan(&docID)
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
	if store := currentStore(r); store != 0 {
		var ds sql.NullInt64
		if err := tx.QueryRow(`SELECT distributor_id FROM doc WHERE id=$1 AND doc_type='sale_return'`,
			req.ID).Scan(&ds); err != nil || ds.Int64 != store {
			writeErr(w, 403, "不能操作其他门店的单据")
			return
		}
	}
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
		// v0.20：货退了，提成也要退——按原销售单这件货的入账记录逐条写负行（店长抽成同步冲）
		type cf struct {
			spID int64
			amt  float64
		}
		var cuts []cf
		crows, err := tx.Query(`SELECT salesperson_id, amount FROM commission_flow
			WHERE doc_id=$1 AND barcode=$2 AND kind IN ('销售','店长抽成')`, l.OrigDocID, l.Barcode)
		if err != nil {
			writeErr(w, 500, "提成查询失败: "+err.Error())
			return
		}
		for crows.Next() {
			var c cf
			if err := crows.Scan(&c.spID, &c.amt); err != nil {
				crows.Close()
				writeErr(w, 500, "提成读取失败: "+err.Error())
				return
			}
			cuts = append(cuts, c)
		}
		crows.Close()
		for _, c := range cuts {
			if _, err := tx.Exec(`INSERT INTO commission_flow (doc_id, barcode, salesperson_id, kind, amount)
				VALUES ($1,$2,$3,'销退冲减',$4)`, req.ID, l.Barcode, c.spID, -c.amt); err != nil {
				writeErr(w, 500, "提成冲减失败: "+err.Error())
				return
			}
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
	if store := currentStore(r); store != 0 {
		var ds sql.NullInt64
		if err := tx.QueryRow(`SELECT distributor_id FROM doc WHERE id=$1 AND doc_type='sale_return'`,
			req.ID).Scan(&ds); err != nil || ds.Int64 != store {
			writeErr(w, 403, "不能操作其他门店的单据")
			return
		}
	}
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
	// v0.20：销退反确认=退货撤销，冲减行一并删除
	if _, err := tx.Exec(`DELETE FROM commission_flow WHERE doc_id=$1`, req.ID); err != nil {
		writeErr(w, 500, "撤销冲减失败: "+err.Error())
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
	var docStore sql.NullInt64
	err = tx.QueryRow(`SELECT draft_lines, distributor_id FROM doc
		WHERE id=$1 AND doc_type='sale_return' AND status='草稿' FOR UPDATE`, req.ID).Scan(&old, &docStore)
	if err == sql.ErrNoRows {
		writeErr(w, 400, "删除失败：只有草稿可以删除")
		return
	}
	if err != nil {
		writeErr(w, 500, "读取单据失败: "+err.Error())
		return
	}
	if store := currentStore(r); store != 0 && docStore.Int64 != store {
		writeErr(w, 403, "不能操作其他门店的单据")
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
	cond, args := "", []any{}
	if store := currentStore(r); store != 0 {
		cond, args = " AND distributor_id=$1", []any{store}
	}
	rows, err := db.Query(`SELECT id, doc_no, status, draft_lines, total_amount, payments,
		to_char(created_at,'YYYY-MM-DD HH24:MI:SS')
		FROM doc WHERE doc_type='sale_return'`+cond+` ORDER BY id DESC LIMIT 20`, args...)
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
	if !requireHQ(w, r) { // v0.19：入库/退库/调拨是总部职能
		return
	}
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
	if !requireHQ(w, r) { // v0.19：入库/退库/调拨是总部职能
		return
	}
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
	if !requireHQ(w, r) { // v0.19：入库/退库/调拨是总部职能
		return
	}
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
	if !requireHQ(w, r) { // v0.19：入库/退库/调拨是总部职能
		return
	}
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
	if currentStore(r) != 0 { // v0.19：门店账号无此视图
		writeJSON(w, 200, map[string]any{"list": []any{}, "total": 0})
		return
	}
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

// ===== 盘点单（v0.23） =====
// 按位置盘点；草稿只存扫到的条码（不锁定任何货品——盘点是"看"不是"占"）；
// 确认那一刻对账生成结果快照：正常/盘亏/盘盈（盘盈带当前状态位置+最近关联单据）。
// 确认不改库存：盘亏的货可能后续找到，盘盈揭示的问题走各自业务单据处理。

type stScan struct {
	Barcode string `json:"barcode"`
	Name    string `json:"name"`
}

// POST /api/doc/stocktake/save {id?, distributorId, barcodes[]}
func handleStocktakeSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID            int64    `json:"id"`
		DistributorID int64    `json:"distributorId"` // 0=总库
		Barcodes      []string `json:"barcodes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Barcodes) == 0 {
		writeErr(w, 400, "参数错误：至少要有一个条码")
		return
	}
	// 门店账号只能盘本店（无视客户端传什么）
	if store := currentStore(r); store != 0 {
		req.DistributorID = store
	}
	seen := map[string]bool{}
	scans := []stScan{}
	for i, raw := range req.Barcodes {
		bc := strings.ToUpper(strings.TrimSpace(raw))
		if !validBarcode(bc) {
			writeErr(w, 400, fmt.Sprintf("第%d个条码 %s 格式非法", i+1, bc))
			return
		}
		if seen[bc] {
			continue // 重复扫到不算错，静默去重
		}
		seen[bc] = true
		scans = append(scans, stScan{Barcode: bc})
	}

	tx, err := db.Begin()
	if err != nil {
		writeErr(w, 500, "开启事务失败: "+err.Error())
		return
	}
	defer tx.Rollback()
	if err := checkLoc(tx, req.DistributorID, "盘点位置"); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// 只收系统存在过的条码——不存在的条码当场报错（可能扫错/标签不是本系统的）
	for i := range scans {
		err := tx.QueryRow(`SELECT name FROM item WHERE barcode=$1`, scans[i].Barcode).Scan(&scans[i].Name)
		if err == sql.ErrNoRows {
			writeErr(w, 400, fmt.Sprintf("条码 %s 系统中不存在——盘点只收系统存在过的条码", scans[i].Barcode))
			return
		}
		if err != nil {
			writeErr(w, 500, "查询失败: "+err.Error())
			return
		}
	}
	linesJSON, _ := json.Marshal(scans)

	if req.ID == 0 {
		today := time.Now().Format("20060102")
		seq, err := nextSeq(tx, "PD", today)
		if err != nil {
			writeErr(w, 500, "单号发号失败: "+err.Error())
			return
		}
		if seq > 99 {
			writeErr(w, 400, "当日盘点单号已满99张")
			return
		}
		docNo := fmt.Sprintf("PD%s%02d", today, seq)
		var docID int64
		err = tx.QueryRow(`INSERT INTO doc (doc_no, doc_type, status, maker_id, draft_lines, distributor_id)
			VALUES ($1,'stocktake','草稿',$2,$3,$4) RETURNING id`,
			docNo, currentUID(r), linesJSON, nullableID(req.DistributorID)).Scan(&docID)
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
	res, err := tx.Exec(`UPDATE doc SET draft_lines=$1, distributor_id=$2
		WHERE id=$3 AND doc_type='stocktake' AND status='草稿'`,
		linesJSON, nullableID(req.DistributorID), req.ID)
	if err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 400, "该单据不是草稿状态（可能已被确认或删除），请刷新")
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "status": "草稿"})
}

// POST /api/doc/stocktake/confirm {id} —— 对账时刻：生成正常/盘亏/盘盈快照
func handleStocktakeConfirm(w http.ResponseWriter, r *http.Request) {
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
	if store := currentStore(r); store != 0 {
		var ds sql.NullInt64
		if err := tx.QueryRow(`SELECT distributor_id FROM doc WHERE id=$1 AND doc_type='stocktake'`,
			req.ID).Scan(&ds); err != nil || ds.Int64 != store {
			writeErr(w, 403, "不能操作其他门店的单据")
			return
		}
	}
	res, err := tx.Exec(`UPDATE doc SET status='已确认', confirmed_at=now()
		WHERE id=$1 AND doc_type='stocktake' AND status='草稿'`, req.ID)
	if err != nil {
		writeErr(w, 500, "确认失败: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 400, "确认失败：单据不存在或已被他人确认，请刷新")
		return
	}
	var linesJSON []byte
	var locID sql.NullInt64
	if err := tx.QueryRow(`SELECT draft_lines, distributor_id FROM doc WHERE id=$1`,
		req.ID).Scan(&linesJSON, &locID); err != nil {
		writeErr(w, 500, "读取单据失败: "+err.Error())
		return
	}
	var scans []stScan
	if err := json.Unmarshal(linesJSON, &scans); err != nil || len(scans) == 0 {
		writeErr(w, 400, "没有扫到任何条码，无法确认")
		return
	}
	scanned := map[string]bool{}
	for _, s := range scans {
		scanned[s.Barcode] = true
	}

	// 应在清单：账面在此位置、且没有离场（已售/已退库=实物不该在店里）
	type stRow struct {
		Barcode  string `json:"barcode"`
		Name     string `json:"name"`
		Status   string `json:"status"`
		Location string `json:"location,omitempty"`
		RefDocNo string `json:"refDocNo,omitempty"`
	}
	var erows *sql.Rows
	if locID.Int64 == 0 {
		erows, err = tx.Query(`SELECT barcode, name, status FROM item
			WHERE location_type='总库' AND status NOT IN ('已售','已退库') ORDER BY id`)
	} else {
		erows, err = tx.Query(`SELECT barcode, name, status FROM item
			WHERE location_type='分销商' AND distributor_id=$1 AND status NOT IN ('已售','已退库') ORDER BY id`,
			locID.Int64)
	}
	if err != nil {
		writeErr(w, 500, "应在清单查询失败: "+err.Error())
		return
	}
	expected := map[string]stRow{}
	for erows.Next() {
		var x stRow
		if err := erows.Scan(&x.Barcode, &x.Name, &x.Status); err != nil {
			erows.Close()
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		expected[x.Barcode] = x
	}
	erows.Close()

	normal, loss, gain := []stRow{}, []stRow{}, []stRow{}
	for bc, x := range expected {
		if scanned[bc] {
			normal = append(normal, x)
		} else {
			loss = append(loss, x) // 账面应在、实物没扫到
		}
	}
	for _, s := range scans {
		if _, ok := expected[s.Barcode]; ok {
			continue
		}
		// 盘盈：扫到了但账面不在此位置——带出当前状态/位置/最近关联单据，无需人工填原因
		var x stRow
		x.Barcode = s.Barcode
		var lt string
		var did sql.NullInt64
		if err := tx.QueryRow(`SELECT name, status, location_type, distributor_id FROM item WHERE barcode=$1`,
			s.Barcode).Scan(&x.Name, &x.Status, &lt, &did); err != nil {
			writeErr(w, 500, "盘盈明细查询失败: "+err.Error())
			return
		}
		if lt == "总库" {
			x.Location = "总库"
		} else {
			x.Location = locName(tx, did.Int64)
		}
		_ = tx.QueryRow(`SELECT d.doc_no FROM stock_flow sf
			JOIN doc d ON d.id = sf.doc_id
			JOIN item it ON it.id = sf.item_id
			WHERE it.barcode=$1 ORDER BY sf.id DESC LIMIT 1`, s.Barcode).Scan(&x.RefDocNo)
		gain = append(gain, x)
	}

	result, _ := json.Marshal(map[string]any{
		"normal": len(normal), "lossList": loss, "gainList": gain,
		"loss": len(loss), "gain": len(gain), "scanned": len(scans),
	})
	if _, err := tx.Exec(`UPDATE doc SET stocktake_result=$1 WHERE id=$2`, result, req.ID); err != nil {
		writeErr(w, 500, "写入结果失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "status": "已确认",
		"normal": len(normal), "loss": len(loss), "gain": len(gain)})
}

// POST /api/doc/stocktake/unconfirm {id} —— 回到草稿重盘（结果快照作废）
func handleStocktakeUnconfirm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == 0 {
		writeErr(w, 400, "参数错误：缺少单据id")
		return
	}
	if store := currentStore(r); store != 0 {
		var ds sql.NullInt64
		if err := db.QueryRow(`SELECT distributor_id FROM doc WHERE id=$1 AND doc_type='stocktake'`,
			req.ID).Scan(&ds); err != nil || ds.Int64 != store {
			writeErr(w, 403, "不能操作其他门店的单据")
			return
		}
	}
	res, err := db.Exec(`UPDATE doc SET status='草稿', confirmed_at=NULL, stocktake_result=NULL
		WHERE id=$1 AND doc_type='stocktake' AND status='已确认'`, req.ID)
	if err != nil {
		writeErr(w, 500, "反确认失败: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 400, "反确认失败：单据不存在或不是已确认状态")
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "status": "草稿"})
}

// POST /api/doc/stocktake/delete {id}
func handleStocktakeDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == 0 {
		writeErr(w, 400, "参数错误：缺少单据id")
		return
	}
	if store := currentStore(r); store != 0 {
		var ds sql.NullInt64
		if err := db.QueryRow(`SELECT distributor_id FROM doc WHERE id=$1 AND doc_type='stocktake'`,
			req.ID).Scan(&ds); err != nil || ds.Int64 != store {
			writeErr(w, 403, "不能操作其他门店的单据")
			return
		}
	}
	res, err := db.Exec(`DELETE FROM doc WHERE id=$1 AND doc_type='stocktake' AND status='草稿'`, req.ID)
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

// GET /api/doc/stocktake —— 盘点单列表（门店账号只见本店）
func handleStocktakeList(w http.ResponseWriter, r *http.Request) {
	cond, args := "", []any{}
	if store := currentStore(r); store != 0 {
		cond, args = " AND d.distributor_id=$1", []any{store}
	}
	rows, err := db.Query(`SELECT d.id, d.doc_no, d.status, d.draft_lines,
		d.distributor_id, COALESCE(dd.name,'总库'), d.stocktake_result,
		to_char(d.created_at,'YYYY-MM-DD HH24:MI:SS')
		FROM doc d LEFT JOIN distributor dd ON dd.id = d.distributor_id
		WHERE d.doc_type='stocktake'`+cond+` ORDER BY d.id DESC LIMIT 20`, args...)
	if err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	defer rows.Close()
	type PDoc struct {
		ID       int64           `json:"id"`
		DocNo    string          `json:"docNo"`
		Status   string          `json:"status"`
		LocID    int64           `json:"distributorId"`
		LocName  string          `json:"locName"`
		Scans    []stScan        `json:"scans"`
		Result   json.RawMessage `json:"result"`
		MadeAt   string          `json:"madeAt"`
	}
	docs := []PDoc{}
	for rows.Next() {
		var d PDoc
		var dl, res []byte
		var lid sql.NullInt64
		if err := rows.Scan(&d.ID, &d.DocNo, &d.Status, &dl, &lid, &d.LocName, &res, &d.MadeAt); err != nil {
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		d.LocID = lid.Int64
		d.Scans = []stScan{}
		_ = json.Unmarshal(dl, &d.Scans)
		if len(res) > 0 {
			d.Result = res
		} else {
			d.Result = json.RawMessage("null")
		}
		docs = append(docs, d)
	}
	writeJSON(w, 200, map[string]any{"list": docs, "total": len(docs)})
}

// ===== 纯旧料回收单（v0.25，HS） =====
// 顾客不买东西、单纯把旧料卖给店里，店里付钱。
// 旧料行复用 OldLine（全部按回收价，无换新部分）；提成走"旧料回收"规则；
// 付款复用 pay_method 字典（方向：付给顾客）；反确认限当日（付出去的钱是财务事实）。
// 不动库存（旧料入库=旧料库，另章）。

// POST /api/doc/recycle/save {id?, salespersonIds, payments, lines:[{category,purity,weightG}]}
func handleRecycleSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID             int64     `json:"id"`
		SalespersonIDs []int64   `json:"salespersonIds"`
		Payments       []PayLine `json:"payments"`
		Lines          []OldLine `json:"lines"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Lines) == 0 {
		writeErr(w, 400, "参数错误：至少要有一行旧料")
		return
	}
	if err := validatePayments(req.Payments); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	for i, ol := range req.Lines {
		var catOK, puOK bool
		_ = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM dict_item WHERE dict_type='category' AND name=$1 AND enabled=true)`, ol.Category).Scan(&catOK)
		_ = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM dict_item WHERE dict_type='purity' AND name=$1 AND enabled=true)`, ol.Purity).Scan(&puOK)
		if !catOK {
			writeErr(w, 400, fmt.Sprintf("第%d行大类「%s」不存在或已停用", i+1, ol.Category))
			return
		}
		if !puOK {
			writeErr(w, 400, fmt.Sprintf("第%d行成色「%s」不存在或已停用", i+1, ol.Purity))
			return
		}
		if ol.WeightG <= 0 {
			writeErr(w, 400, fmt.Sprintf("第%d行克重必须大于0", i+1))
			return
		}
	}
	spSeen := map[int64]bool{}
	spIDs := []int64{}
	for _, id := range req.SalespersonIDs {
		if id > 0 && !spSeen[id] {
			spSeen[id] = true
			spIDs = append(spIDs, id)
		}
	}
	if len(spIDs) > 3 {
		writeErr(w, 400, "售货员最多3人")
		return
	}
	spIDsJSON, _ := json.Marshal(spIDs)
	if req.Payments == nil {
		req.Payments = []PayLine{}
	}
	paysJSON, _ := json.Marshal(req.Payments)
	linesJSON, _ := json.Marshal(req.Lines)
	store := currentStore(r)

	tx, err := db.Begin()
	if err != nil {
		writeErr(w, 500, "开启事务失败: "+err.Error())
		return
	}
	defer tx.Rollback()

	if req.ID == 0 {
		today := time.Now().Format("20060102")
		seq, err := nextSeq(tx, "HS", today)
		if err != nil {
			writeErr(w, 500, "单号发号失败: "+err.Error())
			return
		}
		if seq > 99 {
			writeErr(w, 400, "当日回收单号已满99张")
			return
		}
		docNo := fmt.Sprintf("HS%s%02d", today, seq)
		var docID int64
		err = tx.QueryRow(`INSERT INTO doc (doc_no, doc_type, status, maker_id, old_lines, salesperson_ids, payments, distributor_id)
			VALUES ($1,'recycle','草稿',$2,$3,$4,$5,$6) RETURNING id`,
			docNo, currentUID(r), linesJSON, spIDsJSON, paysJSON, nullableID(store)).Scan(&docID)
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
	var docStore sql.NullInt64
	err = tx.QueryRow(`SELECT distributor_id FROM doc
		WHERE id=$1 AND doc_type='recycle' AND status='草稿' FOR UPDATE`, req.ID).Scan(&docStore)
	if err == sql.ErrNoRows {
		writeErr(w, 400, "该单据不是草稿状态（可能已被确认或删除），请刷新")
		return
	}
	if err != nil {
		writeErr(w, 500, "读取单据失败: "+err.Error())
		return
	}
	if store != 0 && docStore.Int64 != store {
		writeErr(w, 403, "不能操作其他门店的单据")
		return
	}
	if _, err := tx.Exec(`UPDATE doc SET old_lines=$1, salesperson_ids=$2, payments=$3 WHERE id=$4`,
		linesJSON, spIDsJSON, paysJSON, req.ID); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "status": "草稿"})
}

// POST /api/doc/recycle/confirm {id} —— 回收价快照+应付校验+提成入账，一个事务
func handleRecycleConfirm(w http.ResponseWriter, r *http.Request) {
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
	if store := currentStore(r); store != 0 {
		var ds sql.NullInt64
		if err := tx.QueryRow(`SELECT distributor_id FROM doc WHERE id=$1 AND doc_type='recycle'`,
			req.ID).Scan(&ds); err != nil || ds.Int64 != store {
			writeErr(w, 403, "不能操作其他门店的单据")
			return
		}
	}
	res, err := tx.Exec(`UPDATE doc SET status='已确认', confirmed_at=now()
		WHERE id=$1 AND doc_type='recycle' AND status='草稿'`, req.ID)
	if err != nil {
		writeErr(w, 500, "确认失败: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 400, "确认失败：单据不存在或已被他人确认，请刷新")
		return
	}
	var linesJSON, spIDsJSON, paysJSON []byte
	var docStore sql.NullInt64
	if err := tx.QueryRow(`SELECT old_lines, salesperson_ids, payments, distributor_id
		FROM doc WHERE id=$1`, req.ID).Scan(&linesJSON, &spIDsJSON, &paysJSON, &docStore); err != nil {
		writeErr(w, 500, "读取单据失败: "+err.Error())
		return
	}
	var lines []OldLine
	if err := json.Unmarshal(linesJSON, &lines); err != nil || len(lines) == 0 {
		writeErr(w, 400, "没有旧料明细，无法确认")
		return
	}
	// 售货员校验（回收给提成，必填）：在职且归属与单据门店一致
	var spIDs []int64
	_ = json.Unmarshal(spIDsJSON, &spIDs)
	if len(spIDs) == 0 {
		writeErr(w, 400, "请先选择售货员再确认")
		return
	}
	sps := []spInfo{}
	for _, id := range spIDs {
		s, err := loadSalesperson(tx, id)
		if err != nil {
			writeErr(w, 400, "售货员不存在，请重新选择")
			return
		}
		if !s.Enabled {
			writeErr(w, 400, fmt.Sprintf("售货员「%s」已停用，请重新选择", s.Name))
			return
		}
		if s.StoreID != docStore.Int64 {
			writeErr(w, 400, fmt.Sprintf("售货员「%s」属于「%s」，不能在「%s」的回收单上记提成",
				s.Name, locName(tx, s.StoreID), locName(tx, docStore.Int64)))
			return
		}
		sps = append(sps, s)
	}
	var mgr *spInfo
	{
		var m spInfo
		var did sql.NullInt64
		var e error
		if docStore.Int64 == 0 {
			e = tx.QueryRow(`SELECT id, name, distributor_id, role, manager_rate, enabled FROM salesperson
				WHERE role='店长' AND enabled=true AND distributor_id IS NULL`).
				Scan(&m.ID, &m.Name, &did, &m.Role, &m.ManagerRate, &m.Enabled)
		} else {
			e = tx.QueryRow(`SELECT id, name, distributor_id, role, manager_rate, enabled FROM salesperson
				WHERE role='店长' AND enabled=true AND distributor_id=$1`, docStore.Int64).
				Scan(&m.ID, &m.Name, &did, &m.Role, &m.ManagerRate, &m.Enabled)
		}
		if e == nil {
			m.StoreID = did.Int64
			mgr = &m
		}
	}
	// 逐行：回收价快照 + 应付累计 + 提成入账
	rates := map[string]float64{}
	var total float64
	for i := range lines {
		ol := &lines[i]
		var rp float64
		err := tx.QueryRow(`SELECT recycle_price FROM gold_price
			WHERE purity=$1 ORDER BY id DESC LIMIT 1`, ol.Purity).Scan(&rp)
		if err == sql.ErrNoRows || rp <= 0 {
			writeErr(w, 400, fmt.Sprintf("成色「%s」未发布回收价，整单未确认", ol.Purity))
			return
		}
		if err != nil {
			writeErr(w, 500, "金价查询失败: "+err.Error())
			return
		}
		ol.TradeG, ol.RecycleG, ol.TradePrice, ol.RecyclePrice = 0, ol.WeightG, 0, rp
		ol.Credit = round2(ol.WeightG * rp)
		rates[ol.Purity] = rp
		total += ol.Credit
		c, e := oldCommission(tx, ol.Category, "旧料回收", ol.WeightG, ol.Credit)
		if e != nil {
			writeErr(w, 500, "提成规则查询失败: "+e.Error())
			return
		}
		if e := postCommission(tx, req.ID, "旧料", "旧料回收", c, sps, mgr); e != nil {
			writeErr(w, 500, "提成入账失败: "+e.Error())
			return
		}
	}
	total = round2(total)
	// 付款校验：付给顾客的钱必须分毫不差
	var pays []PayLine
	_ = json.Unmarshal(paysJSON, &pays)
	if len(pays) == 0 {
		writeErr(w, 400, fmt.Sprintf("应付顾客 ¥%.2f——请先录入付款方式再确认", total))
		return
	}
	if err := validatePayments(pays); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var paySum float64
	for _, p := range pays {
		var en bool
		err := tx.QueryRow(`SELECT enabled FROM dict_item WHERE dict_type='pay_method' AND name=$1`,
			p.Method).Scan(&en)
		if err != nil || !en {
			writeErr(w, 400, fmt.Sprintf("付款方式「%s」不存在或已停用", p.Method))
			return
		}
		paySum += p.Amount
	}
	if round2(paySum) != total {
		writeErr(w, 400, fmt.Sprintf("付款合计 ¥%.2f 与应付顾客 ¥%.2f 不一致，整单未确认", paySum, total))
		return
	}
	snapshot, _ := json.Marshal(map[string]any{"recycleRates": rates, "oldLines": lines, "payout": total})
	processed, _ := json.Marshal(lines)
	if _, err := tx.Exec(`UPDATE doc SET total_amount=$1, gold_price_snapshot=$2, old_lines=$3 WHERE id=$4`,
		total, snapshot, processed, req.ID); err != nil {
		writeErr(w, 500, "写入合计失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "status": "已确认", "payout": total})
}

// POST /api/doc/recycle/unconfirm {id} —— 限当日；撤销提成
func handleRecycleUnconfirm(w http.ResponseWriter, r *http.Request) {
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
	if store := currentStore(r); store != 0 {
		var ds sql.NullInt64
		if err := tx.QueryRow(`SELECT distributor_id FROM doc WHERE id=$1 AND doc_type='recycle'`,
			req.ID).Scan(&ds); err != nil || ds.Int64 != store {
			writeErr(w, 403, "不能操作其他门店的单据")
			return
		}
	}
	res, err := tx.Exec(`UPDATE doc SET status='草稿', confirmed_at=NULL, total_amount=0, gold_price_snapshot=NULL
		WHERE id=$1 AND doc_type='recycle' AND status='已确认'
		AND confirmed_at::date = CURRENT_DATE`, req.ID)
	if err != nil {
		writeErr(w, 500, "反确认失败: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 400, "反确认失败：单据不存在、不是已确认状态，或已跨日")
		return
	}
	if _, err := tx.Exec(`DELETE FROM commission_flow WHERE doc_id=$1`, req.ID); err != nil {
		writeErr(w, 500, "撤销提成失败: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "提交失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": req.ID, "status": "草稿"})
}

// POST /api/doc/recycle/delete {id}
func handleRecycleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == 0 {
		writeErr(w, 400, "参数错误：缺少单据id")
		return
	}
	if store := currentStore(r); store != 0 {
		var ds sql.NullInt64
		if err := db.QueryRow(`SELECT distributor_id FROM doc WHERE id=$1 AND doc_type='recycle'`,
			req.ID).Scan(&ds); err != nil || ds.Int64 != store {
			writeErr(w, 403, "不能操作其他门店的单据")
			return
		}
	}
	res, err := db.Exec(`DELETE FROM doc WHERE id=$1 AND doc_type='recycle' AND status='草稿'`, req.ID)
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

// GET /api/doc/recycle —— 回收单列表（门店只见本店）
func handleRecycleList(w http.ResponseWriter, r *http.Request) {
	cond, args := "", []any{}
	if store := currentStore(r); store != 0 {
		cond, args = " AND d.distributor_id=$1", []any{store}
	}
	spNames := map[int64]string{}
	if nrows, err := db.Query(`SELECT id, name FROM salesperson`); err == nil {
		for nrows.Next() {
			var id int64
			var n string
			_ = nrows.Scan(&id, &n)
			spNames[id] = n
		}
		nrows.Close()
	}
	rows, err := db.Query(`SELECT d.id, d.doc_no, d.status, d.old_lines, d.total_amount,
		d.salesperson_ids, d.payments, to_char(d.created_at,'YYYY-MM-DD HH24:MI:SS')
		FROM doc d WHERE d.doc_type='recycle'`+cond+` ORDER BY d.id DESC LIMIT 20`, args...)
	if err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	defer rows.Close()
	type HDoc struct {
		ID              int64     `json:"id"`
		DocNo           string    `json:"docNo"`
		Status          string    `json:"status"`
		Payout          float64   `json:"payout"`
		SalespersonIDs  []int64   `json:"salespersonIds"`
		SalespersonName string    `json:"salespersonName"`
		Payments        []PayLine `json:"payments"`
		Lines           []OldLine `json:"lines"`
		MadeAt          string    `json:"madeAt"`
	}
	docs := []HDoc{}
	for rows.Next() {
		var d HDoc
		var dl, spj, pj []byte
		if err := rows.Scan(&d.ID, &d.DocNo, &d.Status, &dl, &d.Payout, &spj, &pj, &d.MadeAt); err != nil {
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		d.SalespersonIDs = []int64{}
		_ = json.Unmarshal(spj, &d.SalespersonIDs)
		names := []string{}
		for _, id := range d.SalespersonIDs {
			if n, ok := spNames[id]; ok {
				names = append(names, n)
			}
		}
		d.SalespersonName = strings.Join(names, "、")
		d.Lines = []OldLine{}
		_ = json.Unmarshal(dl, &d.Lines)
		d.Payments = []PayLine{}
		_ = json.Unmarshal(pj, &d.Payments)
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
	// v0.19：门店账号强制只看本店——无视客户端传什么（服务端推导，防伪造）
	if store := currentStore(r); store != 0 {
		add("it.distributor_id=$%d", store)
	} else if v := strings.TrimSpace(q.Get("loc")); v != "" {
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
		CASE WHEN it.location_type='总库' THEN '总库' ELSE COALESCE(dst.name,'未知分销商') END,
		it.labor_fee_mode, it.labor_fee, it.cost_gold_price, it.cost_fee_mode, it.cost_fee
		FROM item it LEFT JOIN distributor dst ON dst.id = it.distributor_id
		WHERE `+cond+` ORDER BY it.id DESC LIMIT 500`, args...)
	if err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	defer rows.Close()
	isAdm := currentIsAdmin(r)
	list := []Item{}
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.Barcode, &it.Name, &it.Category, &it.Purity, &it.WeightG, &it.Price, &it.Status, &it.Location,
			&it.SaleFeeMode, &it.SaleFee, &it.CostGoldPrice, &it.CostFeeMode, &it.CostFee); err != nil {
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		if !isAdm { // 进货成本是机密，非管理员不可见（前端隐藏是体验，这里才是安全）
			it.CostGoldPrice, it.CostFee, it.CostFeeMode = 0, 0, ""
		}
		list = append(list, it)
	}
	writeJSON(w, 200, map[string]any{"list": list, "total": total, "sumWeightG": sumW})
}

// ===== 标签数据导出（v0.26）—— 方案1：借 Label Matrix 的力 =====
// 复刻 JMP 写给 Label Matrix 的「标签数据」表格式（111列，照原文件逐列抄录），
// LM 模板按列名取数，我们填得上的填、填不上的留空。导出 .xlsx，
// 用户在 LM 里把数据源指向这个文件（一次性操作），之后照老习惯选打印机打标签。

var labelHeaders = []string{
	"条码号", "金料成色", "首饰类别", "首饰大类", "首饰小类", "净金重", "含配金重", "手寸", "销售工费方式", "销售工费", "售价", "主石名称",
	"成色含量", "金料成色大名称", "主石大名称", "首饰类别大名称", "首饰品牌", "首饰系列", "款式系列", "外部款号", "首饰工艺1", "首饰工艺2", "首饰工艺3",
	"证书号", "证书号2", "原编号", "内部款号", "线上编号", "二维码", "监管码", "供应商", "供应商代号", "自定列1", "自定列2", "自定列3",
	"下拉自定列1", "下拉自定列2", "下拉自定列3", "数值自定列1", "数值自定列2", "总件重", "配件1名称", "配件1数量", "配件1重量", "配件2名称",
	"配件2数量", "配件2重量", "主石石号", "主石规格", "主石粒数", "主石重量", "主石单位", "主石形状", "主石颜色", "主石净度", "主石切工", "主石对称性",
	"主石抛光度", "主石荧光", "主石全深比", "主石台宽比", "主石镶法", "主石爪型", "副石1号", "副石1名", "副石1规格", "副石1粒数", "副石1重量",
	"副石1单位", "副石1形状", "副石1颜色", "副石1净度", "副石1切工", "副石2号", "副石2名", "副石2规格", "副石2粒数", "副石2重量", "副石2单位",
	"副石2形状", "副石2颜色", "副石2净度", "副石2切工", "副石3号", "副石3名", "副石3粒数", "副石3重量", "副石4号", "副石4名", "副石4粒数",
	"副石4重量", "副石5号", "副石5名", "副石5粒数", "副石5重量", "副石总粒数", "副石总重量", "其他费", "促销原价", "售价2", "首饰备注", "入库顺序",
	"退库顺序", "进货顺序", "调柜顺序", "调退顺序", "修改顺序", "销售顺序", "销退顺序", "盘点顺序", "首饰图片",
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// writeXLSX 用标准库手写一个最小可用的 .xlsx（zip 里几个 XML，单元格全部内联字符串）。
// 不引第三方库——我们只需要"Excel 和 Label Matrix 能读"这一件事。
func writeXLSX(w io.Writer, sheetName string, rows [][]string) error {
	zw := zip.NewWriter(w)
	add := func(name, content string) error {
		f, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = f.Write([]byte(content))
		return err
	}
	if err := add("[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>
<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>
</Types>`); err != nil {
		return err
	}
	if err := add("_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
</Relationships>`); err != nil {
		return err
	}
	if err := add("xl/workbook.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
<sheets><sheet name="`+xmlEscape(sheetName)+`" sheetId="1" r:id="rId1"/></sheets>
</workbook>`); err != nil {
		return err
	}
	if err := add("xl/_rels/workbook.xml.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
</Relationships>`); err != nil {
		return err
	}
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for _, row := range rows {
		sb.WriteString("<row>")
		for _, cell := range row {
			if cell == "" {
				sb.WriteString(`<c t="inlineStr"><is><t/></is></c>`)
			} else {
				sb.WriteString(`<c t="inlineStr"><is><t>` + xmlEscape(cell) + `</t></is></c>`)
			}
		}
		sb.WriteString("</row>")
	}
	sb.WriteString(`</sheetData></worksheet>`)
	if err := add("xl/worksheets/sheet1.xml", sb.String()); err != nil {
		return err
	}
	return zw.Close()
}

// GET /api/doc/inbound/labels?id=N —— 导出该入库单货品的标签数据（仅已确认单）
func handleInboundLabels(w http.ResponseWriter, r *http.Request) {
	if !requireHQ(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if id == 0 {
		writeErr(w, 400, "参数错误：缺少单据id")
		return
	}
	var docNo, status string
	err := db.QueryRow(`SELECT doc_no, status FROM doc WHERE id=$1 AND doc_type='inbound'`, id).
		Scan(&docNo, &status)
	if err == sql.ErrNoRows {
		writeErr(w, 400, "入库单不存在")
		return
	}
	if err != nil {
		writeErr(w, 500, "查询失败: "+err.Error())
		return
	}
	if status != "已确认" {
		writeErr(w, 400, "只有已确认的入库单才能导出标签（草稿还没发条码号）")
		return
	}
	rows, err := db.Query(`SELECT it.barcode, it.name, it.category, it.purity, it.weight_g, it.price,
		it.labor_fee_mode, it.labor_fee, it.jewel_type, it.stone_name
		FROM doc_line dl JOIN item it ON it.id = dl.item_id
		WHERE dl.doc_id=$1 ORDER BY dl.line_no`, id)
	if err != nil {
		writeErr(w, 500, "明细查询失败: "+err.Error())
		return
	}
	defer rows.Close()
	col := map[string]int{}
	for i, h := range labelHeaders {
		col[h] = i
	}
	data := [][]string{labelHeaders}
	for rows.Next() {
		var barcode, name, category, purity, feeMode, jewelType, stoneName string
		var weight, price, fee float64
		if err := rows.Scan(&barcode, &name, &category, &purity, &weight, &price, &feeMode, &fee, &jewelType, &stoneName); err != nil {
			writeErr(w, 500, "读取失败: "+err.Error())
			return
		}
		if jewelType == "" { // 老货没有类别字段，退回用名称
			jewelType = name
		}
		row := make([]string, len(labelHeaders))
		row[col["条码号"]] = barcode
		row[col["金料成色"]] = purity
		row[col["首饰类别"]] = jewelType
		row[col["主石名称"]] = stoneName
		row[col["首饰大类"]] = category
		row[col["净金重"]] = fmt.Sprintf("%.4f", weight)
		row[col["含配金重"]] = fmt.Sprintf("%.4f", weight)
		row[col["总件重"]] = fmt.Sprintf("%.4f", weight)
		row[col["售价"]] = fmt.Sprintf("%g", price)
		row[col["销售工费方式"]] = feeMode
		if fee > 0 {
			row[col["销售工费"]] = fmt.Sprintf("%g", fee)
		}
		data = append(data, row)
	}
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="labels.xlsx"`)
	if err := writeXLSX(w, "数据", data); err != nil {
		// 响应头已发出，只能记录——实践中 zip 写内存缓冲更稳，此处简化
		return
	}
	_ = docNo
}

// ===== 入库单：保存草稿 =====
// POST /api/doc/inbound/save  请求: {id?, category, lines[]}
// id 为空 → 新建草稿并取单号（草稿即占号，删除不回收）；id 非空 → 更新已有草稿。
func handleInboundSave(w http.ResponseWriter, r *http.Request) {
	if !requireHQ(w, r) { // v0.19：入库/退库/调拨是总部职能
		return
	}
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
	if !requireHQ(w, r) { // v0.19：入库/退库/调拨是总部职能
		return
	}
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

	// v0.28：直填的新值自动补进字典（下次就在下拉里）
	pset, jset, sset := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, l := range lines {
		pset[l.Purity] = true
		jset[l.JewelType] = true
		sset[l.StoneName] = true
	}
	if err := ensureDictValues(tx, "purity", pset); err != nil {
		writeErr(w, 500, "字典更新失败: "+err.Error())
		return
	}
	if err := ensureDictValues(tx, "jewel_type", jset); err != nil {
		writeErr(w, 500, "字典更新失败: "+err.Error())
		return
	}
	if err := ensureDictValues(tx, "stone_name", sset); err != nil {
		writeErr(w, 500, "字典更新失败: "+err.Error())
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
			INSERT INTO item (barcode, name, category, purity, weight_g, price, status,
				labor_fee_mode, labor_fee, cost_gold_price, cost_fee_mode, cost_fee, jewel_type, stone_name)
			VALUES ($1,$2,$3,$4,$5,$6,'在库',$7,$8,$9,$10,$11,$12,$13) RETURNING id`,
			bc, l.Name, category, l.Purity, l.WeightG, l.Price,
			l.SaleFeeMode, l.SaleFee, l.CostGoldPrice, l.CostFeeMode, l.CostFee,
			l.JewelType, l.StoneName).Scan(&itemID)
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
	if !requireHQ(w, r) { // v0.19：入库/退库/调拨是总部职能
		return
	}
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
		SELECT it.id, it.barcode, it.status, it.name, it.purity, it.weight_g, it.price,
			it.labor_fee_mode, it.labor_fee, it.cost_gold_price, it.cost_fee_mode, it.cost_fee,
			it.jewel_type, it.stone_name
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
		var bc, st, name, purity, sfm, cfm, jt, sn string
		var wg, pr, sf, cgp, cf float64
		if err := rows.Scan(&id, &bc, &st, &name, &purity, &wg, &pr, &sfm, &sf, &cgp, &cfm, &cf, &jt, &sn); err != nil {
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
		rebuilt = append(rebuilt, DraftLine{Barcode: bc, Name: name, Purity: purity, WeightG: wg, Price: pr,
			SaleFeeMode: sfm, SaleFee: sf, CostGoldPrice: cgp, CostFeeMode: cfm, CostFee: cf,
			JewelType: jt, StoneName: sn})
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
	if !requireHQ(w, r) { // v0.19：入库/退库/调拨是总部职能
		return
	}
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
	if currentStore(r) != 0 { // v0.19：门店账号无此视图
		writeJSON(w, 200, map[string]any{"list": []any{}, "total": 0})
		return
	}
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
					Purity: l.Purity, WeightG: l.WeightG, Price: l.Price, Status: "草稿",
					SaleFeeMode: l.SaleFeeMode, SaleFee: l.SaleFee,
					CostGoldPrice: l.CostGoldPrice, CostFeeMode: l.CostFeeMode, CostFee: l.CostFee,
					JewelType: l.JewelType, StoneName: l.StoneName})
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
	mux.HandleFunc("GET /api/commission-report", withAuth(handleCommissionReport))
	mux.HandleFunc("GET /api/commission-rules", withAuth(handleCommissionRuleList))
	mux.HandleFunc("POST /api/commission-rules", withAuth(handleCommissionRuleCreate))
	mux.HandleFunc("POST /api/commission-rules/update", withAuth(handleCommissionRuleUpdate))
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
	mux.HandleFunc("GET /api/doc/inbound/labels", withAuth(handleInboundLabels))
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
	mux.HandleFunc("POST /api/doc/stocktake/save", withAuth(handleStocktakeSave))
	mux.HandleFunc("POST /api/doc/stocktake/confirm", withAuth(handleStocktakeConfirm))
	mux.HandleFunc("POST /api/doc/stocktake/unconfirm", withAuth(handleStocktakeUnconfirm))
	mux.HandleFunc("POST /api/doc/stocktake/delete", withAuth(handleStocktakeDelete))
	mux.HandleFunc("GET /api/doc/stocktake", withAuth(handleStocktakeList))
	mux.HandleFunc("POST /api/doc/recycle/save", withAuth(handleRecycleSave))
	mux.HandleFunc("POST /api/doc/recycle/confirm", withAuth(handleRecycleConfirm))
	mux.HandleFunc("POST /api/doc/recycle/unconfirm", withAuth(handleRecycleUnconfirm))
	mux.HandleFunc("POST /api/doc/recycle/delete", withAuth(handleRecycleDelete))
	mux.HandleFunc("GET /api/doc/recycle", withAuth(handleRecycleList))

	fmt.Println("后端已启动: http://localhost:8080/api/health  (Ctrl+C 停止)")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		panic(err)
	}
}
