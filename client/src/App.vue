<script setup lang="ts">
// 入库单界面 v0.3：支持单据生命周期——保存草稿 → 确认 → 反确认 / 删除草稿
// 草稿可反复编辑；确认后生成货品件进入库存；反确认撤回（条码保留）。
import { ref } from 'vue'
import { api, setToken } from './api'

// ===== 登录 =====
const logged = ref(false)
const username = ref('admin')
const password = ref('123456')
const userLabel = ref('')
const isAdmin = ref(false)
const errMsg = ref('')
const okMsg = ref('')

// ===== 库存 =====
interface Item {
  id: number
  barcode: string
  name: string
  purity: string
  weightG: number
  status: string
}
const items = ref<Item[]>([])

// ===== 单据列表 =====
interface Doc {
  id: number
  docNo: string
  category: string
  status: string
  items: Item[]
  madeAt: string
}
const docs = ref<Doc[]>([])

// ===== 编辑中的单据（表单状态） =====
interface Line {
  barcode: string
  name: string
  purity: string
  weightG: number | null
}
// ===== 用户管理（仅管理员） =====
interface User {
  id: number
  username: string
  name: string
  status: number
  isAdmin: boolean
  created: string
}
const users = ref<User[]>([])
const showUsers = ref(false)
const nuUsername = ref('')
const nuName = ref('')
const nuPassword = ref('')

async function toggleUsers() {
  showUsers.value = !showUsers.value
  if (showUsers.value) await loadUsers()
}
async function loadUsers() {
  try {
    const r = await api.userList()
    users.value = r.list
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
async function createUser() {
  errMsg.value = ''
  try {
    await api.userCreate(nuUsername.value, nuName.value, nuPassword.value)
    flash(`已创建用户 ${nuUsername.value}`)
    nuUsername.value = ''
    nuName.value = ''
    nuPassword.value = ''
    await loadUsers()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
async function setUserStatus(u: User, status: number) {
  errMsg.value = ''
  try {
    await api.userSetStatus(u.id, status)
    flash(`${u.name} 已${status === 1 ? '启用' : '禁用'}`)
    await loadUsers()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
async function resetUserPwd(u: User) {
  const p = window.prompt(`为 ${u.name}(${u.username}) 设置新密码（至少6位）：`)
  if (!p) return
  errMsg.value = ''
  try {
    await api.userResetPassword(u.id, p)
    flash(`${u.name} 的密码已重置`)
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}

// ===== 修改密码 =====
const showPwd = ref(false)
const oldPwd = ref('')
const newPwd = ref('')
async function changePwd() {
  errMsg.value = ''
  try {
    await api.changePassword(oldPwd.value, newPwd.value)
    flash('密码已修改，下次登录请用新密码')
    showPwd.value = false
    oldPwd.value = ''
    newPwd.value = ''
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}

const editingId = ref(0) // 0=新单；>0=正在编辑的草稿id
const editingNo = ref('')
const category = ref('黄金')
const lines = ref<Line[]>([{ barcode: '', name: '', purity: '足金999.9', weightG: null }])

function resetForm() {
  editingId.value = 0
  editingNo.value = ''
  category.value = '黄金'
  lines.value = [{ barcode: '', name: '', purity: '足金999.9', weightG: null }]
}

function flash(ok: string) {
  okMsg.value = ok
  errMsg.value = ''
  setTimeout(() => (okMsg.value = ''), 4000)
}

async function doLogin() {
  errMsg.value = ''
  try {
    const r = await api.login(username.value, password.value)
    setToken(r.token)
    userLabel.value = r.name
    isAdmin.value = !!r.isAdmin
    logged.value = true
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}

async function refreshAll() {
  const [ri, rd] = await Promise.all([api.items(), api.inboundList()])
  items.value = ri.list
  docs.value = rd.list
}

function addLine() {
  lines.value.push({ barcode: '', name: '', purity: '足金999.9', weightG: null })
}
function removeLine(i: number) {
  lines.value.splice(i, 1)
}

function payload() {
  return {
    id: editingId.value,
    category: category.value,
    lines: lines.value.map((l) => ({
      barcode: l.barcode,
      name: l.name,
      purity: l.purity,
      weightG: Number(l.weightG) || 0,
    })),
  }
}

// 保存草稿：新单取号占号；旧草稿原地更新
async function saveDraft() {
  errMsg.value = ''
  try {
    const r = await api.inboundSave(payload())
    if (editingId.value === 0) {
      editingId.value = r.id
      editingNo.value = r.docNo
    }
    flash(`草稿已保存${editingNo.value ? '：' + editingNo.value : ''}`)
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}

// 保存并确认：先存草稿再确认（库存在"确认"那一刻才真正变动）
async function saveAndConfirm() {
  errMsg.value = ''
  try {
    const r = await api.inboundSave(payload())
    const id = editingId.value || r.id
    const doc = await api.inboundConfirm(id)
    flash(`已确认：${doc.docNo}，生成 ${doc.items.length} 件货品`)
    resetForm()
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
    await refreshAll() // 保存可能已成功，刷新列表让草稿可见
  }
}

// 列表操作
async function confirmDoc(d: Doc) {
  errMsg.value = ''
  try {
    const doc = await api.inboundConfirm(d.id)
    flash(`已确认：${doc.docNo}`)
    if (editingId.value === d.id) resetForm()
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
    await refreshAll()
  }
}

async function unconfirmDoc(d: Doc) {
  errMsg.value = ''
  try {
    await api.inboundUnconfirm(d.id)
    flash(`已反确认：${d.docNo} 退回草稿，货品已撤回`)
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
    await refreshAll()
  }
}

async function deleteDoc(d: Doc) {
  errMsg.value = ''
  try {
    await api.inboundDelete(d.id)
    flash(`草稿 ${d.docNo} 已删除（单号不回收）`)
    if (editingId.value === d.id) resetForm()
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}

// 把草稿装进表单继续编辑
function editDoc(d: Doc) {
  editingId.value = d.id
  editingNo.value = d.docNo
  category.value = d.category
  lines.value = d.items.map((it) => ({
    barcode: it.barcode,
    name: it.name,
    purity: it.purity,
    weightG: it.weightG,
  }))
  window.scrollTo({ top: 0, behavior: 'smooth' })
}
</script>

<template>
  <main class="wrap">
    <h1>黄金零售系统 · 入库管理</h1>

    <!-- 登录页 -->
    <section v-if="!logged" class="card">
      <h2>登录</h2>
      <div class="row">
        <label>账号 <input v-model="username" /></label>
        <label>密码 <input v-model="password" type="password" @keyup.enter="doLogin" /></label>
        <button @click="doLogin">登录</button>
      </div>
      <p class="hint">教学账号：admin / 123456</p>
      <p v-if="errMsg" class="err">{{ errMsg }}</p>
    </section>

    <template v-else>
      <p class="hint">
        当前用户：{{ userLabel }}
        <button class="mini" @click="showPwd = !showPwd">修改密码</button>
        <button v-if="isAdmin" class="mini" @click="toggleUsers">用户管理</button>
      </p>
      <div v-if="showPwd" class="card">
        <div class="row">
          <label>旧密码 <input v-model="oldPwd" type="password" /></label>
          <label>新密码(至少6位) <input v-model="newPwd" type="password" /></label>
          <button @click="changePwd">确认修改</button>
        </div>
      </div>
      <p v-if="okMsg" class="ok">{{ okMsg }}</p>
      <p v-if="errMsg" class="err">{{ errMsg }}</p>

      <!-- 用户管理（仅管理员可见） -->
      <section v-if="isAdmin && showUsers" class="card">
        <h2>用户管理 <button class="mini" @click="loadUsers">刷新</button></h2>
        <table>
          <thead>
            <tr><th>用户名</th><th>姓名</th><th>状态</th><th>角色</th><th>创建日期</th><th>操作</th></tr>
          </thead>
          <tbody>
            <tr v-for="u in users" :key="u.id">
              <td class="mono">{{ u.username }}</td>
              <td>{{ u.name }}</td>
              <td>{{ u.status === 1 ? '启用' : '已禁用' }}</td>
              <td>{{ u.isAdmin ? '管理员' : '店员' }}</td>
              <td>{{ u.created }}</td>
              <td>
                <button v-if="u.status === 1" class="mini danger" @click="setUserStatus(u, 0)">禁用</button>
                <button v-else class="mini" @click="setUserStatus(u, 1)">启用</button>
                <button class="mini" @click="resetUserPwd(u)">重置密码</button>
              </td>
            </tr>
          </tbody>
        </table>
        <div class="row">
          <label>用户名 <input v-model="nuUsername" placeholder="字母数字下划线" /></label>
          <label>姓名 <input v-model="nuName" /></label>
          <label>初始密码 <input v-model="nuPassword" type="password" /></label>
          <button @click="createUser">新建用户</button>
        </div>
      </section>

      <!-- 开单表单 -->
      <section class="card">
        <h2>
          {{ editingId ? `编辑草稿 ${editingNo}` : '新建入库单' }}
          <button v-if="editingId" class="mini" @click="resetForm">放弃编辑，新建</button>
        </h2>
        <div class="row">
          <label>首饰大类
            <select v-model="category">
              <option>黄金</option><option>玉器类</option><option>钻石类</option><option>万足银(克)</option>
            </select>
          </label>
        </div>
        <table>
          <thead>
            <tr><th>#</th><th>条码号(留空确认时自动生成)</th><th>首饰名称</th><th>成色</th><th>总件重(g)</th><th></th></tr>
          </thead>
          <tbody>
            <tr v-for="(l, i) in lines" :key="i">
              <td>{{ i + 1 }}</td>
              <td><input v-model="l.barcode" placeholder="自动生成" /></td>
              <td><input v-model="l.name" /></td>
              <td><input v-model="l.purity" /></td>
              <td><input v-model.number="l.weightG" type="number" step="0.01" /></td>
              <td><button class="mini" @click="removeLine(i)" :disabled="lines.length === 1">删行</button></td>
            </tr>
          </tbody>
        </table>
        <div class="row">
          <button class="mini" @click="addLine">+ 增加行</button>
          <button class="gray" @click="saveDraft">保存草稿</button>
          <button @click="saveAndConfirm">保存并确认</button>
        </div>
        <p class="hint">草稿不动库存；点"确认"的那一刻才生成货品件。</p>
      </section>

      <!-- 单据列表 -->
      <section class="card">
        <h2>入库单列表（{{ docs.length }} 张） <button class="mini" @click="refreshAll">刷新</button></h2>
        <div v-for="d in docs" :key="d.id" class="doc">
          <div class="dochead">
            <span class="mono">{{ d.docNo }}</span>
            <span :class="['badge', d.status === '草稿' ? 'draft' : 'ok2']">{{ d.status }}</span>
            <span>{{ d.category }}</span>
            <span class="hint">{{ (d.items?.length ?? 0) }} 件 · {{ d.madeAt }}</span>
            <span class="spacer"></span>
            <template v-if="d.status === '草稿'">
              <button class="mini" @click="editDoc(d)">编辑</button>
              <button class="mini" @click="confirmDoc(d)">确认</button>
              <button class="mini danger" @click="deleteDoc(d)">删除</button>
            </template>
            <template v-else>
              <button class="mini danger" @click="unconfirmDoc(d)">反确认</button>
            </template>
          </div>
          <table v-if="d.items?.length">
            <tbody>
              <tr v-for="(it, j) in d.items" :key="j">
                <td class="mono" style="width:160px">{{ it.barcode || '(待发号)' }}</td>
                <td>{{ it.name }}</td>
                <td style="width:110px">{{ it.purity }}</td>
                <td style="width:90px">{{ (it.weightG ?? 0).toFixed(2) }}g</td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      <!-- 库存 -->
      <section class="card">
        <h2>总库库存（{{ items.length }} 件）</h2>
        <table>
          <thead>
            <tr><th>条码号</th><th>首饰名称</th><th>成色</th><th>总件重(g)</th><th>状态</th></tr>
          </thead>
          <tbody>
            <tr v-for="it in items" :key="it.id">
              <td class="mono">{{ it.barcode }}</td>
              <td>{{ it.name }}</td>
              <td>{{ it.purity }}</td>
              <td>{{ (it.weightG ?? 0).toFixed(2) }}</td>
              <td>{{ it.status }}</td>
            </tr>
          </tbody>
        </table>
      </section>
    </template>
  </main>
</template>

<style>
body { font-family: -apple-system, "PingFang SC", "Microsoft YaHei", sans-serif; margin: 0; background: #f5f6f7; }
.wrap { max-width: 960px; margin: 0 auto; padding: 24px; }
h1 { font-size: 20px; }
.card { background: #fff; border: 1px solid #e2e4e8; border-radius: 8px; padding: 16px 20px; margin-bottom: 16px; }
.card h2 { font-size: 16px; margin-top: 0; display: flex; gap: 10px; align-items: center; }
.row { display: flex; gap: 12px; align-items: center; margin: 8px 0; flex-wrap: wrap; }
label { font-size: 14px; }
input, select { padding: 6px 8px; border: 1px solid #ccc; border-radius: 4px; font-size: 14px; }
button { padding: 7px 18px; border: none; border-radius: 4px; background: #2f7d4f; color: #fff; cursor: pointer; font-size: 14px; }
button.mini { padding: 4px 10px; font-size: 13px; background: #6b7280; }
button.gray { background: #4b5563; }
button.danger { background: #b4552d; }
button:disabled { opacity: 0.4; cursor: not-allowed; }
table { width: 100%; border-collapse: collapse; font-size: 14px; margin: 8px 0; }
th, td { border: 1px solid #e2e4e8; padding: 6px 8px; text-align: left; }
th { background: #f0f2f4; font-weight: 600; }
td input { width: 100%; box-sizing: border-box; border: 1px solid #ddd; }
.hint { color: #888; font-size: 13px; }
.err { color: #c0392b; font-size: 14px; }
.ok { color: #2f7d4f; font-size: 14px; font-weight: 600; }
.mono { font-family: ui-monospace, Menlo, monospace; }
.doc { border: 1px solid #e8eaed; border-radius: 6px; padding: 8px 12px; margin-bottom: 10px; }
.dochead { display: flex; gap: 10px; align-items: center; flex-wrap: wrap; }
.badge { padding: 1px 10px; border-radius: 10px; font-size: 12px; }
.badge.draft { background: #fdf2d0; color: #8a6d1a; }
.badge.ok2 { background: #ddf0e3; color: #22663d; }
.spacer { flex: 1; }
</style>
