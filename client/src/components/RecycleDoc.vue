<script setup lang="ts">
// 旧料回收单据页（v0.32）：顾客卖料给店里。旧料行×当日回收价 + 经手售货员 + 组合付款。
import { computed, onMounted, ref } from 'vue'
import { api } from '../api'

interface DictItem { id: number; name: string; sort: number; enabled: boolean }
interface GoldPrice { purity: string; retailPrice: number; recyclePrice: number; tradePrice: number }
interface Salesperson { id: number; name: string; role: string }
interface PayLine { method: string; amount: number | null }
interface HLine { category: string; purity: string; weightG: number | null; recyclePrice?: number; credit?: number }
interface Doc {
  id: number; docNo: string; status: string; payout: number
  salespersonIds: number[]; salespersonName: string
  payments: PayLine[]; lines: HLine[]; madeAt: string
}

const props = defineProps<{
  docId: number; initial: Doc | null
  dicts: Record<string, DictItem[]>; goldPrices: GoldPrice[]; salespersons: Salesperson[]
}>()
const emit = defineEmits<{ close: []; refresh: []; flash: [string]; rename: [string] }>()

const id = ref(props.docId)
const docNo = ref('')
const status = ref('新单')
const madeAt = ref('')
const lines = ref<HLine[]>([])
const spIds = ref<number[]>([])
const pays = ref<PayLine[]>([])
const payout = ref(0)          // 已确认：实付金额
const spName = ref('')         // 已确认：经手人
const err = ref('')

const editable = computed(() => status.value !== '已确认')
const enabledCats = computed(() => (props.dicts.category ?? []).filter(d => d.enabled))
const enabledPurities = computed(() => (props.dicts.purity ?? []).filter(d => d.enabled))
const enabledPayMethods = computed(() => (props.dicts.pay_method ?? []).filter(d => d.enabled))
const recycleRateOf = (purity: string) => props.goldPrices.find(g => g.purity === purity)?.recyclePrice ?? 0
const total = () => lines.value.reduce((s, o) => s + (Number(o.weightG) || 0) * recycleRateOf(o.purity), 0)
const payTotal = () => pays.value.reduce((s, p) => s + (Number(p.amount) || 0), 0)

function initFrom(d: Doc) {
  id.value = d.id
  docNo.value = d.docNo
  status.value = d.status
  madeAt.value = d.madeAt
  payout.value = d.payout
  spName.value = d.salespersonName ?? ''
  spIds.value = [...(d.salespersonIds ?? [])]
  pays.value = (d.payments ?? []).map(p => ({ ...p }))
  lines.value = (d.lines ?? []).map(o => ({ ...o }))
}
async function reload() {
  const r = await api.recycleList()
  const d = (r.list as Doc[]).find(x => x.id === id.value)
  if (d) initFrom(d)
}
onMounted(() => {
  if (props.initial) initFrom(props.initial)
  else if (id.value > 0) reload().catch(e => (err.value = (e as Error).message))
})

function addLine() {
  lines.value.push({
    category: enabledCats.value[0]?.name ?? '',
    purity: enabledPurities.value[0]?.name ?? '',
    weightG: null,
  })
}
function removeLine(i: number) { lines.value.splice(i, 1) }
function toggleSp(sid: number) {
  const i = spIds.value.indexOf(sid)
  if (i >= 0) spIds.value.splice(i, 1)
  else if (spIds.value.length < 3) spIds.value.push(sid)
  else err.value = '售货员最多3人'
}
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
    const r = await api.recycleSave({
      id: id.value,
      salespersonIds: spIds.value,
      payments: pays.value.filter(p => p.method).map(p => ({ method: p.method, amount: Number(p.amount) || 0 })),
      lines: lines.value.filter(o => (Number(o.weightG) || 0) > 0)
        .map(o => ({ category: o.category, purity: o.purity, weightG: Number(o.weightG) })),
    })
    if (id.value === 0) {
      id.value = r.id
      docNo.value = r.docNo
      emit('rename', r.docNo)
    }
    status.value = '草稿'
    if (confirmAfter) {
      const c = await api.recycleConfirm(id.value)
      status.value = '已确认'
      await reload() // 装回快照回收价/实付等确认后数据
      emit('flash', `回收已确认：${docNo.value}，付顾客 ¥${c.payout}`)
    } else {
      emit('flash', `回收草稿已保存：${docNo.value}`)
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
    await api.recycleUnconfirm(id.value)
    status.value = '草稿'
    await reload()
    emit('flash', `回收已反确认：${docNo.value}`)
    emit('refresh')
  } catch (e) {
    err.value = (e as Error).message
  }
}
async function delDraft() {
  err.value = ''
  try {
    await api.recycleDelete(id.value)
    emit('flash', `回收草稿 ${docNo.value} 已删除`)
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
      <span v-else>新建回收单（顾客卖料给店里）</span>
      <span :class="['badge', status === '已确认' ? 'ok2' : 'draft']">{{ status }}</span>
      <span v-if="madeAt" class="hint">制单：{{ madeAt }}</span>
      <span class="spacer"></span>
      <button class="mini gray" @click="emit('close')">关闭</button>
    </h2>

    <template v-if="editable">
      <div class="row" v-for="(o, i) in lines" :key="i">
        <label>大类
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
          × 回收价 {{ recycleRateOf(o.purity).toFixed(2) }} = ¥{{ ((Number(o.weightG) || 0) * recycleRateOf(o.purity)).toFixed(2) }}
        </span>
        <button class="mini danger" @click="removeLine(i)">移除</button>
      </div>
      <div class="row">
        <button class="mini" @click="addLine">+ 添加旧料</button>
      </div>
      <template v-if="lines.length">
        <div class="row">
          <span>经手售货员(可多选)：</span>
          <button v-for="s in salespersons" :key="s.id" class="mini"
            :style="spIds.includes(s.id) ? 'background:#2f7d4f' : ''" @click="toggleSp(s.id)">
            {{ s.name }}{{ s.role === '店长' ? '(店长)' : '' }}
          </button>
        </div>
        <div class="row" v-for="(p, i) in pays" :key="i">
          <label>付款方式
            <select v-model="p.method">
              <option v-for="m in enabledPayMethods" :key="m.id" :value="m.name">{{ m.name }}</option>
            </select>
          </label>
          <label>金额 <input v-model.number="p.amount" type="number" step="0.01" style="width:110px" /></label>
          <button class="mini" @click="fillPay(p)">补足</button>
          <button class="mini danger" @click="removePay(i)">移除</button>
        </div>
        <div class="row">
          <button class="mini" @click="addPay">+ 添加付款方式</button>
          <span class="hint" v-if="pays.length">
            已付 ¥{{ payTotal().toFixed(2) }} / 应付顾客 ¥{{ total().toFixed(2) }}
            <b v-if="Math.abs(payTotal() - total()) > 0.005" style="color:#c0392b">（差 ¥{{ (total() - payTotal()).toFixed(2) }}）</b>
            <b v-else style="color:#2f7d4f">✓ 两讫</b>
          </span>
        </div>
        <div class="row">
          <p class="ok" style="margin:0">应付顾客：¥{{ total().toFixed(2) }}</p>
          <span class="spacer"></span>
          <button v-if="status === '草稿'" class="mini danger" @click="delDraft">删除草稿</button>
          <button class="gray" @click="save(false)">保存草稿</button>
          <button @click="save(true)">确认付款</button>
        </div>
      </template>
      <p class="hint">全部按当日回收价折算（确认时快照）；回收提成按"旧料回收"规则入账；反确认限当日。旧料暂不入库存（旧料库待做）。</p>
    </template>

    <template v-else>
      <div class="row">
        <span class="ok" v-if="payout > 0">付顾客 ¥{{ payout.toFixed(2) }}</span>
        <span v-if="spName">经手：{{ spName }}</span>
        <span class="hint" v-if="pays.length">
          {{ pays.map(p => `${p.method}¥${Number(p.amount ?? 0).toFixed(2)}`).join(' + ') }}
        </span>
      </div>
      <table>
        <thead><tr><th>#</th><th>大类</th><th>成色</th><th>克重(g)</th><th>折算</th></tr></thead>
        <tbody>
          <tr v-for="(o, j) in lines" :key="j">
            <td>{{ j + 1 }}</td>
            <td>{{ o.category }}</td>
            <td>{{ o.purity }}</td>
            <td>{{ (Number(o.weightG) || 0).toFixed(2) }}</td>
            <td>{{ o.recyclePrice ? `×${o.recyclePrice.toFixed(2)} = ¥${(o.credit ?? 0).toFixed(2)}` : '—' }}</td>
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
