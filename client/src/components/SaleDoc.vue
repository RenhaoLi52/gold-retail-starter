<script setup lang="ts">
// 销售单据页（v0.32）：扫码加件（标签价/变金价）＋ 旧料区（以旧换新/回收）
// ＋ 售货员(1~3人平分) ＋ 组合收款三分支两讫。逻辑原样从 App.vue 搬家成组件，
// 每张销售页签一套独立状态，可同时挂多张单。
import { computed, onMounted, ref } from 'vue'
import { api } from '../api'

interface DictItem { id: number; name: string; sort: number; enabled: boolean }
interface GoldPrice { purity: string; retailPrice: number; recyclePrice: number; tradePrice: number }
interface Salesperson { id: number; name: string; role: string }
interface InvItem {
  barcode: string; name: string; category?: string; purity: string; weightG: number
  price?: number; status: string; saleFeeMode?: string; saleFee?: number
}
interface SaleLine {
  barcode: string; name: string; purity: string; weightG: number; price: number
  mode: string; soldPrice: number | null; saleFeeMode?: string; saleFee?: number
}
interface PayLine { method: string; amount: number | null }
interface OldLine { category: string; purity: string; weightG: number | null; tradeG?: number; recycleG?: number; credit?: number }
interface Doc {
  id: number; docNo: string; status: string; totalAmount: number
  salespersonIds: number[]; salespersonName: string
  payments: PayLine[]; oldLines: OldLine[]; lines: SaleLine[]; madeAt: string
}

const props = defineProps<{
  docId: number; initial: Doc | null
  items: InvItem[]; goldPrices: GoldPrice[]; salespersons: Salesperson[]
  dicts: Record<string, DictItem[]>; userStore: string
}>()
const emit = defineEmits<{ close: []; refresh: []; flash: [string]; rename: [string] }>()

const id = ref(props.docId)
const docNo = ref('')
const status = ref('新单')
const madeAt = ref('')
const lines = ref<SaleLine[]>([])
const input = ref('')
const spIds = ref<number[]>([])
const pays = ref<PayLine[]>([])
const old = ref<OldLine[]>([])
const totalAmount = ref(0) // 已确认：净额
const spName = ref('')
const err = ref('')

const editable = computed(() => status.value !== '已确认')
const enabledCats = computed(() => (props.dicts.category ?? []).filter(d => d.enabled))
const enabledPurities = computed(() => (props.dicts.purity ?? []).filter(d => d.enabled))
const enabledPayMethods = computed(() => (props.dicts.pay_method ?? []).filter(d => d.enabled))

// --- 金价三表 ---
const goldRateOf = (purity: string) => props.goldPrices.find(g => g.purity === purity)?.retailPrice ?? 0
const tradeRateOf = (purity: string) => props.goldPrices.find(g => g.purity === purity)?.tradePrice ?? 0
const recycleRateOf = (purity: string) => props.goldPrices.find(g => g.purity === purity)?.recyclePrice ?? 0

// 建议价：标签价→售价；变金价→克重×金价+销售工费，四舍五入到元（JMP同款口径）
function suggestPrice(l: SaleLine): number {
  if (l.mode === '标签价') return l.price
  const fee = (l.saleFeeMode === '按件') ? (l.saleFee ?? 0) : l.weightG * (l.saleFee ?? 0)
  return Math.round(l.weightG * goldRateOf(l.purity) + fee)
}
function modeChanged(l: SaleLine) { l.soldPrice = suggestPrice(l) }

function initFrom(d: Doc) {
  id.value = d.id
  docNo.value = d.docNo
  status.value = d.status
  madeAt.value = d.madeAt
  totalAmount.value = d.totalAmount
  spName.value = d.salespersonName ?? ''
  lines.value = (d.lines ?? []).map(l => ({ ...l }))
  spIds.value = [...(d.salespersonIds ?? [])]
  pays.value = (d.payments ?? []).map(p => ({ ...p }))
  old.value = (d.oldLines ?? []).map(o => ({ ...o }))
}
async function reload() {
  const r = await api.saleList()
  const d = (r.list as Doc[]).find(x => x.id === id.value)
  if (d) initFrom(d)
}
onMounted(() => {
  if (props.initial) initFrom(props.initial)
  else if (id.value > 0) reload().catch(e => (err.value = (e as Error).message))
})

// --- 新品行 ---
function add() {
  const bc = input.value.trim().toUpperCase()
  if (!bc) return
  if (lines.value.some(x => x.barcode === bc)) {
    err.value = `条码 ${bc} 已在本单中`
    return
  }
  const it = props.items.find(x => x.barcode === bc)
  if (!it) {
    err.value = `条码 ${bc} 不在当前库存列表中`
    return
  }
  if (it.status !== '在库') {
    err.value = it.status === '销售中' || it.status === '退库中'
      ? `条码 ${bc} 当前「${it.status}」（被某张草稿占着）——先处理那张草稿，或换一件`
      : `条码 ${bc} 当前状态「${it.status}」，不能销售`
    return
  }
  const mode = (it.price ?? 0) > 0 ? '标签价' : '变金价'
  const line: SaleLine = {
    barcode: bc, name: it.name, purity: it.purity,
    weightG: it.weightG, price: it.price ?? 0, mode, soldPrice: null,
    saleFeeMode: it.saleFeeMode || '按克', saleFee: it.saleFee ?? 0,
  }
  line.soldPrice = suggestPrice(line)
  if (mode === '变金价' && goldRateOf(it.purity) <= 0) {
    err.value = `成色「${it.purity}」今日未发布金价——请先发布金价或改用标签价`
  } else {
    err.value = ''
  }
  lines.value.push(line)
  input.value = ''
}
function removeAt(i: number) { lines.value.splice(i, 1) }
const total = () => lines.value.reduce((s, l) => s + (Number(l.soldPrice) || 0), 0)

// --- 旧料区（金换金银换银，额度=本单同大类新品克重）---
function addOld() {
  old.value.push({
    category: enabledCats.value[0]?.name ?? '',
    purity: enabledPurities.value[0]?.name ?? '',
    weightG: null,
  })
}
function removeOld(i: number) { old.value.splice(i, 1) }
// 预览拆分（与服务端同口径：按录入顺序占额度；确认时以服务端为准）
function oldSplit() {
  const quota: Record<string, number> = {}
  for (const l of lines.value) {
    const it = props.items.find(x => x.barcode === l.barcode)
    const c = it?.category ?? ''
    quota[c] = (quota[c] ?? 0) + (Number(l.weightG) || 0)
  }
  const used: Record<string, number> = {}
  return old.value.map(o => {
    const w = Number(o.weightG) || 0
    const avail = Math.max((quota[o.category] ?? 0) - (used[o.category] ?? 0), 0)
    const tradeG = Math.min(w, avail)
    used[o.category] = (used[o.category] ?? 0) + tradeG
    const recycleG = w - tradeG
    const credit = tradeG * tradeRateOf(o.purity) + recycleG * recycleRateOf(o.purity)
    return { tradeG, recycleG, credit: Math.round(credit * 100) / 100 }
  })
}
const credit = () => oldSplit().reduce((s, x) => s + x.credit, 0)
const net = () => Math.round((total() - credit()) * 100) / 100

// --- 售货员 ---
function toggleSp(sid: number) {
  const i = spIds.value.indexOf(sid)
  if (i >= 0) spIds.value.splice(i, 1)
  else if (spIds.value.length < 3) spIds.value.push(sid)
  else err.value = '售货员最多3人'
}

// --- 组合收款 ---
function addPay() {
  const used = new Set(pays.value.map(p => p.method))
  const next = enabledPayMethods.value.find(m => !used.has(m.name))
  pays.value.push({ method: next?.name ?? '', amount: null })
}
function removePay(i: number) { pays.value.splice(i, 1) }
const payTotal = () => pays.value.reduce((s, p) => s + (Number(p.amount) || 0), 0)
function fillPay(p: PayLine) {
  const others = pays.value.filter(x => x !== p).reduce((s, x) => s + (Number(x.amount) || 0), 0)
  p.amount = Math.round((Math.abs(net()) - others) * 100) / 100
}

async function save(confirmAfter: boolean) {
  err.value = ''
  try {
    const r = await api.saleSave({
      id: id.value,
      salespersonIds: spIds.value,
      payments: pays.value.filter(p => p.method).map(p => ({ method: p.method, amount: Number(p.amount) || 0 })),
      oldLines: old.value.filter(o => (Number(o.weightG) || 0) > 0)
        .map(o => ({ category: o.category, purity: o.purity, weightG: Number(o.weightG) })),
      lines: lines.value.map(l => ({ barcode: l.barcode, mode: l.mode, soldPrice: Number(l.soldPrice) || 0 })),
    })
    if (id.value === 0) {
      id.value = r.id
      docNo.value = r.docNo
      emit('rename', r.docNo)
    }
    status.value = '草稿'
    if (confirmAfter) {
      const c = await api.saleConfirm(id.value)
      status.value = '已确认'
      await reload() // 装回服务端确认后的拆分/快照/净额
      emit('flash', `已收款确认：${docNo.value}，合计 ¥${c.totalAmount}`)
    } else {
      emit('flash', `销售草稿已保存：${docNo.value}`)
    }
    emit('refresh')
  } catch (e) {
    err.value = (e as Error).message
    emit('refresh')
  }
}
async function unconfirm() {
  err.value = ''
  try {
    await api.saleUnconfirm(id.value)
    status.value = '草稿'
    await reload()
    emit('flash', `销售已反确认：${docNo.value}，货品回到在库`)
    emit('refresh')
  } catch (e) {
    err.value = (e as Error).message
  }
}
async function delDraft() {
  err.value = ''
  try {
    await api.saleDelete(id.value)
    emit('flash', `销售草稿 ${docNo.value} 已删除`)
    emit('refresh')
    emit('close')
  } catch (e) {
    err.value = (e as Error).message
  }
}
</script>

<template>
  <section class="card">
    <h2>
      <span v-if="docNo" class="mono">{{ docNo }}</span>
      <span v-else>销售开单</span>
      <span :class="['badge', status === '已确认' ? 'ok2' : 'draft']">{{ status }}</span>
      <span v-if="madeAt" class="hint">制单：{{ madeAt }}</span>
      <span class="spacer"></span>
      <button class="mini gray" @click="emit('close')">关闭</button>
    </h2>

    <template v-if="editable">
      <div class="row">
        <label>条码 <input v-model="input" placeholder="扫码或输入后回车" @keyup.enter="add" class="mono" /></label>
        <button class="mini" @click="add">添加</button>
      </div>
      <table v-if="lines.length">
        <thead>
          <tr><th>#</th><th>条码</th><th>名称</th><th>成色</th><th>克重</th><th>标签价</th><th>结算方式</th><th>参考价</th><th>实售价(¥)</th><th></th></tr>
        </thead>
        <tbody>
          <tr v-for="(l, i) in lines" :key="l.barcode">
            <td>{{ i + 1 }}</td>
            <td class="mono">{{ l.barcode }}</td>
            <td>{{ l.name }}</td>
            <td>{{ l.purity }}</td>
            <td>{{ l.weightG.toFixed(2) }}g</td>
            <td>{{ l.price > 0 ? '¥' + l.price.toFixed(0) : '—' }}</td>
            <td>
              <select v-model="l.mode" @change="modeChanged(l)">
                <option :disabled="l.price <= 0">标签价</option>
                <option>变金价</option>
              </select>
            </td>
            <td class="hint">¥{{ suggestPrice(l).toFixed(2) }}<span v-if="l.mode === '变金价'">（{{ l.weightG.toFixed(2) }}g × {{ goldRateOf(l.purity).toFixed(2) }}）</span></td>
            <td><input v-model.number="l.soldPrice" type="number" step="0.01" style="width:110px" /></td>
            <td><button class="mini" @click="removeAt(i)">移除</button></td>
          </tr>
        </tbody>
      </table>

      <!-- 旧料区（以旧换新/旧料回收） -->
      <template v-if="lines.length">
        <div class="row" v-for="(o, i) in old" :key="i">
          <label>旧料大类
            <select v-model="o.category">
              <option v-for="c in enabledCats" :key="c.id" :value="c.name">{{ c.name }}</option>
            </select>
          </label>
          <label>成色
            <select v-model="o.purity">
              <option v-for="pu in enabledPurities" :key="pu.id" :value="pu.name">{{ pu.name }}</option>
            </select>
          </label>
          <label>克重 <input v-model.number="o.weightG" type="number" step="0.01" style="width:90px" /></label>
          <span class="hint" v-if="(Number(o.weightG) || 0) > 0">
            换新 {{ oldSplit()[i].tradeG.toFixed(2) }}g×{{ tradeRateOf(o.purity).toFixed(0) }}
            + 回收 {{ oldSplit()[i].recycleG.toFixed(2) }}g×{{ recycleRateOf(o.purity).toFixed(0) }}
            = 抵 ¥{{ oldSplit()[i].credit.toFixed(2) }}
          </span>
          <button class="mini danger" @click="removeOld(i)">移除</button>
        </div>
        <div class="row">
          <button class="mini" @click="addOld">+ 顾客带旧料（以旧换新/回收）</button>
        </div>
      </template>

      <!-- 售货员 + 组合收款 -->
      <div class="row" v-if="lines.length">
        <span>售货员(可多选，整单平分)：</span>
        <button v-for="s in salespersons" :key="s.id" class="mini"
          :style="spIds.includes(s.id) ? 'background:#2f7d4f' : ''" @click="toggleSp(s.id)">
          {{ s.name }}{{ s.role === '店长' ? '(店长)' : '' }}
        </button>
        <span class="hint" v-if="!salespersons.length">（先在"售货员"里给{{ userStore || '总部' }}添加人员）</span>
      </div>
      <template v-if="lines.length">
        <div class="row" v-for="(p, i) in pays" :key="i">
          <label>收款方式
            <select v-model="p.method">
              <option v-for="m in enabledPayMethods" :key="m.id" :value="m.name">{{ m.name }}</option>
            </select>
          </label>
          <label>金额 <input v-model.number="p.amount" type="number" step="0.01" style="width:110px" /></label>
          <button class="mini" @click="fillPay(p)">补足</button>
          <button class="mini danger" @click="removePay(i)">移除</button>
        </div>
        <div class="row">
          <button class="mini" @click="addPay">+ 添加收款方式</button>
          <span class="hint" v-if="pays.length">
            {{ net() >= 0 ? '已收' : '已退' }} ¥{{ payTotal().toFixed(2) }} /
            {{ net() >= 0 ? '净应收' : '应退顾客' }} ¥{{ Math.abs(net()).toFixed(2) }}
            <b v-if="Math.abs(payTotal() - Math.abs(net())) > 0.005" style="color:#c0392b">（差 ¥{{ (Math.abs(net()) - payTotal()).toFixed(2) }}）</b>
            <b v-else style="color:#2f7d4f">✓ 两讫</b>
          </span>
        </div>
      </template>
      <div class="row" v-if="lines.length">
        <p class="ok" style="margin:0">
          货款 ¥{{ total().toFixed(2) }}
          <template v-if="credit() > 0"> − 旧料抵扣 ¥{{ credit().toFixed(2) }} =
            <b :style="net() < 0 ? 'color:#c0392b' : ''">{{ net() >= 0 ? '净应收' : '应退顾客' }} ¥{{ Math.abs(net()).toFixed(2) }}</b>
          </template>
        </p>
        <span class="spacer"></span>
        <button v-if="status === '草稿'" class="mini danger" @click="delDraft">删除草稿</button>
        <button class="gray" @click="save(false)">保存草稿(挂单)</button>
        <button @click="save(true)">确认收款</button>
      </div>
      <p class="hint">标签价=按货品售价；变金价=克重×该成色今日零售金价+销售工费。实售价可在参考价上议价修改。</p>
    </template>

    <template v-else>
      <div class="row">
        <span class="ok" v-if="totalAmount !== 0">{{ totalAmount < 0 ? '退 ' : '' }}¥{{ Math.abs(totalAmount).toFixed(2) }}</span>
        <span v-if="spName">售货员：{{ spName }}</span>
        <span class="hint" v-if="pays.length">
          {{ pays.map(p => `${p.method}¥${Number(p.amount ?? 0).toFixed(2)}`).join(' + ') }}
        </span>
      </div>
      <table>
        <thead><tr><th>#</th><th>条码</th><th>名称</th><th>结算方式</th><th>实售价(¥)</th></tr></thead>
        <tbody>
          <tr v-for="(l, j) in lines" :key="j">
            <td>{{ j + 1 }}</td>
            <td class="mono">{{ l.barcode }}</td>
            <td>{{ l.name }}</td>
            <td>{{ l.mode }}</td>
            <td>¥{{ Number(l.soldPrice ?? 0).toFixed(2) }}</td>
          </tr>
        </tbody>
      </table>
      <table v-if="old.length">
        <thead><tr><th>旧料大类</th><th>成色</th><th>克重(g)</th><th>换新(g)</th><th>回收(g)</th><th>抵扣(¥)</th></tr></thead>
        <tbody>
          <tr v-for="(o, j) in old" :key="j">
            <td>{{ o.category }}</td>
            <td>{{ o.purity }}</td>
            <td>{{ (Number(o.weightG) || 0).toFixed(2) }}</td>
            <td>{{ (o.tradeG ?? 0).toFixed(2) }}</td>
            <td>{{ (o.recycleG ?? 0).toFixed(2) }}</td>
            <td>{{ (o.credit ?? 0).toFixed(2) }}</td>
          </tr>
        </tbody>
      </table>
      <div class="row">
        <span class="spacer"></span>
        <button class="mini danger" @click="unconfirm">反确认(限当日)</button>
      </div>
    </template>

    <p v-if="err" class="err">{{ err }}</p>
  </section>
</template>
