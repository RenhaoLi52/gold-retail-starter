<script setup lang="ts">
// 入门骨架前端：登录 → 看库存 → 开入库单
// 刻意没有用任何 UI 组件库，先让你看清最原始的数据流；
// 正式开发时换成 Element Plus 的表格和表单组件即可。
import { ref } from 'vue'
import { api, setToken } from './api'

// ===== 登录状态 =====
const logged = ref(false)
const username = ref('admin')
const password = ref('123456')
const userLabel = ref('')
const errMsg = ref('')

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

// ===== 入库单草稿（页面上的编辑状态） =====
interface Line {
  barcode: string
  name: string
  purity: string
  weightG: number | null
}
const category = ref('黄金')
const lines = ref<Line[]>([{ barcode: '', name: '', purity: '足金999.9', weightG: null }])
const lastDocNo = ref('')

async function doLogin() {
  errMsg.value = ''
  try {
    const r = await api.login(username.value, password.value)
    setToken(r.token)
    userLabel.value = r.name
    logged.value = true
    await refreshItems()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}

async function refreshItems() {
  const r = await api.items()
  items.value = r.list
}

function addLine() {
  lines.value.push({ barcode: '', name: '', purity: '足金999.9', weightG: null })
}

function removeLine(i: number) {
  lines.value.splice(i, 1)
}

async function submitInbound() {
  errMsg.value = ''
  lastDocNo.value = ''
  try {
    const payload = {
      category: category.value,
      lines: lines.value.map((l) => ({
        barcode: l.barcode,
        name: l.name,
        purity: l.purity,
        weightG: Number(l.weightG) || 0,
      })),
    }
    const doc = await api.inboundCreate(payload)
    lastDocNo.value = doc.docNo
    lines.value = [{ barcode: '', name: '', purity: '足金999.9', weightG: null }]
    await refreshItems()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
</script>

<template>
  <main class="wrap">
    <h1>黄金零售系统 · 入门骨架</h1>

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

    <!-- 主界面 -->
    <template v-else>
      <p class="hint">当前用户：{{ userLabel }}</p>

      <section class="card">
        <h2>新建入库单</h2>
        <div class="row">
          <label>首饰大类
            <select v-model="category">
              <option>黄金</option><option>玉器类</option><option>钻石类</option>
            </select>
          </label>
        </div>
        <table>
          <thead>
            <tr><th>#</th><th>条码号(留空自动生成)</th><th>首饰名称</th><th>成色</th><th>总件重(g)</th><th></th></tr>
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
          <button @click="submitInbound">确认入库</button>
        </div>
        <p v-if="lastDocNo" class="ok">入库成功，单号：{{ lastDocNo }}</p>
        <p v-if="errMsg" class="err">{{ errMsg }}</p>
      </section>

      <section class="card">
        <h2>总库库存（{{ items.length }} 件） <button class="mini" @click="refreshItems">刷新</button></h2>
        <table>
          <thead>
            <tr><th>条码号</th><th>首饰名称</th><th>成色</th><th>总件重(g)</th><th>状态</th></tr>
          </thead>
          <tbody>
            <tr v-for="it in items" :key="it.id">
              <td class="mono">{{ it.barcode }}</td>
              <td>{{ it.name }}</td>
              <td>{{ it.purity }}</td>
              <td>{{ it.weightG.toFixed(2) }}</td>
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
.wrap { max-width: 900px; margin: 0 auto; padding: 24px; }
h1 { font-size: 20px; }
.card { background: #fff; border: 1px solid #e2e4e8; border-radius: 8px; padding: 16px 20px; margin-bottom: 16px; }
.card h2 { font-size: 16px; margin-top: 0; }
.row { display: flex; gap: 12px; align-items: center; margin: 8px 0; flex-wrap: wrap; }
label { font-size: 14px; }
input, select { padding: 6px 8px; border: 1px solid #ccc; border-radius: 4px; font-size: 14px; }
button { padding: 7px 18px; border: none; border-radius: 4px; background: #2f7d4f; color: #fff; cursor: pointer; font-size: 14px; }
button.mini { padding: 4px 10px; font-size: 13px; background: #6b7280; }
button:disabled { opacity: 0.4; cursor: not-allowed; }
table { width: 100%; border-collapse: collapse; font-size: 14px; margin: 8px 0; }
th, td { border: 1px solid #e2e4e8; padding: 6px 8px; text-align: left; }
th { background: #f0f2f4; font-weight: 600; }
td input { width: 100%; box-sizing: border-box; border: 1px solid #ddd; }
.hint { color: #888; font-size: 13px; }
.err { color: #c0392b; font-size: 14px; }
.ok { color: #2f7d4f; font-size: 14px; font-weight: 600; }
.mono { font-family: ui-monospace, Menlo, monospace; }
</style>
