<script setup lang="ts">
// 销退单据页（v0.32）：扫已售件→带出原单与成交价，退款可下调，组合退款两讫。
import { computed, onMounted, ref } from 'vue'
import { api } from '../api'

interface DictItem { id: number; name: string; sort: number; enabled: boolean }
interface PayLine { method: string; amount: number | null }
interface SRLine {
  barcode: string; name: string; purity: string; weightG: number
  origDocId: number; origDocNo: string; soldPrice: number; refundPrice: number | null
}
interface Doc { id: number; docNo: string; status: string; totalAmount: number; payments: PayLine[]; lines: SRLine[]; madeAt: string }

const props = defineProps<{ docId: number; initial: Doc | null; dicts: Record<string, DictItem[]> }>()
const emit = defineEmits<{ close: []; refresh: []; flash: [string]; rename: [string] }>()

const id = ref(props.docId)
const docNo = ref('')
const status = ref('新单')
const madeAt = ref('')
const lines = ref<SRLine[]>([])
const input = ref('')
const pays = ref<PayLine[]>([])
const totalAmount = ref(0)
const err = ref('')

const editable = computed(() => status.value !== '已确认')
const enabledPayMethods = computed(() => (props.dicts.pay_method ?? []).filter(d => d.enabled))
const total = () => lines.value.reduce((s, l) => s + (Number(l.refundPrice) || 0), 0)
const payTotal = () => pays.value.reduce((s, p) => s + (Number(p.amount) || 0), 0)

function initFrom(d: Doc) {
  id.value = d.id
  docNo.value = d.docNo
  status.value = d.status
  madeAt.value = d.madeAt
  totalAmount.value = d.totalAmount
  lines.value = (d.lines ?? []).map(l => ({ ...l }))
  pays.value = (d.payments ?? []).map(p => ({ ...p }))
}
async function reload() {
  const r = await api.saleReturnList()
  const d = (r.list as Doc[]).find(x => x.id === id.value)
  if (d) initFrom(d)
}
onMounted(() => {
  if (props.initial) initFrom(props.initial)
  else if (id.value > 0) reload().catch(e => (err.value = (e as Error).message))
})

async function add() {
  const bc = input.value.trim().toUpperCase()
  if (!bc) return
  if (lines.value.some(x => x.barcode === bc)) {
    err.value = `条码 ${bc} 已在本单中`
    return
  }
  try {
    const r = await api.saleReturnLookup(bc)
    lines.value.push({ ...r })
    input.value = ''
    err.value = ''
  } catch (e) {
    err.value = (e as Error).message
  }
}
function removeAt(i: number) { lines.value.splice(i, 1) }
function addPay() {
  const used = new Set(pays.value.map(p => p.method))
  const next = enabledPayMethods.value.find(m => !used.has(m.name))
  pays.value.push({ method: next?.name ?? '', amount: null })
}
function removePay(i: number) { pays.value.splice(i, 1) }
function fillPay(p: PayLine) {
  const others = pays.value.filter(x => x !== p).reduce((s, x) => s + (Number(x.amount) || 0), 0)
  p.amount = Math.round((total() - others) * 100) / 100
}

async function save(confirmAfter: boolean) {
  err.value = ''
  try {
    const r = await api.saleReturnSave({
      id: id.value,
      payments: pays.value.filter(p => p.method).map(p => ({ method: p.method, amount: Number(p.amount) || 0 })),
      lines: lines.value.map(l => ({ barcode: l.barcode, refundPrice: Number(l.refundPrice) || 0 })),
    })
    if (id.value === 0) {
      id.value = r.id
      docNo.value = r.docNo
      emit('rename', r.docNo)
    }
    status.value = '草稿'
    if (confirmAfter) {
      const c = await api.saleReturnConfirm(id.value)
      status.value = '已确认'
      await reload()
      emit('flash', `销退已确认：${docNo.value}，退款合计 ¥${c.totalAmount}`)
    } else {
      emit('flash', `销退草稿已保存：${docNo.value}`)
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
    await api.saleReturnUnconfirm(id.value)
    status.value = '草稿'
    await reload()
    emit('flash', `销退已反确认：${docNo.value}`)
    emit('refresh')
  } catch (e) {
    err.value = (e as Error).message
  }
}
async function delDraft() {
  err.value = ''
  try {
    await api.saleReturnDelete(id.value)
    emit('flash', `销退草稿 ${docNo.value} 已删除，货品回到"已售"`)
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
      <span v-else>新建销退单（退货）</span>
      <span :class="['badge', status === '已确认' ? 'ok2' : 'draft']">{{ status }}</span>
      <span v-if="madeAt" class="hint">制单：{{ madeAt }}</span>
      <span class="spacer"></span>
      <button class="mini gray" @click="emit('close')">关闭</button>
    </h2>

    <template v-if="editable">
      <div class="row">
        <label>条码 <input v-model="input" placeholder="扫已售件的条码后回车" @keyup.enter="add" class="mono" /></label>
        <button class="mini" @click="add">添加</button>
      </div>
      <table v-if="lines.length">
        <thead>
          <tr><th>条码</th><th>名称</th><th>原销售单</th><th>原成交价</th><th>退款金额(可下调)</th><th></th></tr>
        </thead>
        <tbody>
          <tr v-for="(l, i) in lines" :key="l.barcode">
            <td class="mono">{{ l.barcode }}</td>
            <td>{{ l.name }}</td>
            <td class="mono">{{ l.origDocNo }}</td>
            <td>¥{{ (l.soldPrice ?? 0).toFixed(2) }}</td>
            <td><input v-model.number="l.refundPrice" type="number" step="0.01" style="width:110px" /></td>
            <td><button class="mini" @click="removeAt(i)">删行</button></td>
          </tr>
        </tbody>
      </table>
      <template v-if="lines.length">
        <div class="row" v-for="(p, i) in pays" :key="i">
          <label>退款方式
            <select v-model="p.method">
              <option v-for="m in enabledPayMethods" :key="m.id" :value="m.name">{{ m.name }}</option>
            </select>
          </label>
          <label>金额 <input v-model.number="p.amount" type="number" step="0.01" style="width:110px" /></label>
          <button class="mini" @click="fillPay(p)">补足</button>
          <button class="mini danger" @click="removePay(i)">移除</button>
        </div>
        <div class="row">
          <button class="mini" @click="addPay">+ 添加退款方式</button>
          <span class="hint" v-if="pays.length">
            已退 ¥{{ payTotal().toFixed(2) }} / 应退 ¥{{ total().toFixed(2) }}
            <b v-if="Math.abs(payTotal() - total()) > 0.005" style="color:#c0392b">（差 ¥{{ (total() - payTotal()).toFixed(2) }}）</b>
            <b v-else style="color:#2f7d4f">✓ 两讫</b>
          </span>
        </div>
        <div class="row">
          <p class="ok" style="margin:0">应退合计：¥{{ total().toFixed(2) }}</p>
          <span class="spacer"></span>
          <button v-if="status === '草稿'" class="mini danger" @click="delDraft">删除草稿</button>
          <button class="gray" @click="save(false)">保存草稿</button>
          <button @click="save(true)">确认退款</button>
        </div>
      </template>
      <p class="hint">只收"已售"的件；退款不能超过原成交价；反确认限当日。</p>
    </template>

    <template v-else>
      <div class="row">
        <span class="ok" v-if="totalAmount > 0">退 ¥{{ totalAmount.toFixed(2) }}</span>
        <span class="hint" v-if="pays.length">
          {{ pays.map(p => `${p.method}¥${Number(p.amount ?? 0).toFixed(2)}`).join(' + ') }}
        </span>
      </div>
      <table>
        <thead><tr><th>#</th><th>条码</th><th>名称</th><th>原销售单</th><th>退款(¥)</th></tr></thead>
        <tbody>
          <tr v-for="(l, j) in lines" :key="j">
            <td>{{ j + 1 }}</td>
            <td class="mono">{{ l.barcode }}</td>
            <td>{{ l.name }}</td>
            <td class="mono">{{ l.origDocNo }}</td>
            <td>¥{{ Number(l.refundPrice ?? 0).toFixed(2) }}</td>
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
