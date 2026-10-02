<script setup lang="ts">
// 盘点单据页（v0.32）：选位置逐件扫码；确认那一刻对账，已确认页直接展示差异详情。
import { computed, onMounted, ref } from 'vue'
import { api } from '../api'

interface Item { barcode: string; name: string }
interface Distributor { id: number; name: string; status: number }
interface STRow { barcode: string; name: string; status: string; location?: string; refDocNo?: string }
interface STResult { normal: number; loss: number; gain: number; scanned: number; lossList: STRow[]; gainList: STRow[] }
interface Doc {
  id: number; docNo: string; status: string
  distributorId: number; locName: string
  scans: { barcode: string; name: string }[]
  result: STResult | null; madeAt: string
}

const props = defineProps<{
  docId: number; initial: Doc | null; items: Item[]; distributors: Distributor[]
  isHQ: boolean; userStoreId: number; userStore: string
}>()
const emit = defineEmits<{ close: []; refresh: []; flash: [string]; rename: [string] }>()

const id = ref(props.docId)
const docNo = ref('')
const status = ref('新单')
const madeAt = ref('')
const loc = ref(props.userStoreId)
const input = ref('')
const scans = ref<{ barcode: string; name: string }[]>([])
const result = ref<STResult | null>(null)
const err = ref('')

const editable = computed(() => status.value !== '已确认')
const enabledDists = computed(() => props.distributors.filter(d => d.status === 1))
const locName = computed(() => (!loc.value ? '总库' : props.distributors.find(d => d.id === loc.value)?.name ?? '?'))

function initFrom(d: Doc) {
  id.value = d.id
  docNo.value = d.docNo
  status.value = d.status
  madeAt.value = d.madeAt
  loc.value = d.distributorId || 0
  scans.value = (d.scans ?? []).map(x => ({ ...x }))
  result.value = d.result
}
async function reload() {
  const r = await api.stocktakeList()
  const d = (r.list as Doc[]).find(x => x.id === id.value)
  if (d) initFrom(d)
}
onMounted(() => {
  if (props.initial) initFrom(props.initial)
  else if (id.value > 0) reload().catch(e => (err.value = (e as Error).message))
})

function add() {
  const bc = input.value.trim().toUpperCase()
  if (!bc) return
  if (scans.value.some(x => x.barcode === bc)) {
    input.value = '' // 重复扫到静默忽略——盘点时扫两遍很正常
    return
  }
  const it = props.items.find(x => x.barcode === bc)
  scans.value.push({ barcode: bc, name: it?.name ?? '' })
  input.value = ''
  err.value = ''
}
function removeAt(i: number) { scans.value.splice(i, 1) }

async function save(confirmAfter: boolean) {
  err.value = ''
  try {
    const r = await api.stocktakeSave({
      id: id.value, distributorId: loc.value, barcodes: scans.value.map(x => x.barcode),
    })
    if (id.value === 0) {
      id.value = r.id
      docNo.value = r.docNo
      emit('rename', r.docNo)
    }
    status.value = '草稿'
    if (confirmAfter) {
      const c = await api.stocktakeConfirm(id.value)
      status.value = '已确认'
      await reload() // 把对账结果（含差异详情）装回来
      emit('flash', `盘点已确认：${docNo.value}——正常${c.normal} / 盘亏${c.loss} / 盘盈${c.gain}`)
    } else {
      emit('flash', `盘点草稿已保存：${docNo.value}（已扫${scans.value.length}件）`)
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
    await api.stocktakeUnconfirm(id.value)
    status.value = '草稿'
    result.value = null
    await reload()
    emit('flash', `盘点已反确认：${docNo.value}，可继续补扫后重新确认`)
    emit('refresh')
  } catch (e) {
    err.value = (e as Error).message
  }
}
async function delDraft() {
  err.value = ''
  try {
    await api.stocktakeDelete(id.value)
    emit('flash', `盘点草稿 ${docNo.value} 已删除`)
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
      <span v-else>新建盘点单</span>
      <span :class="['badge', status === '已确认' ? 'ok2' : 'draft']">{{ status }}</span>
      <span class="hint">盘点位置：{{ locName }}</span>
      <span v-if="madeAt" class="hint">制单：{{ madeAt }}</span>
      <span class="spacer"></span>
      <button class="mini gray" @click="emit('close')">关闭</button>
    </h2>

    <template v-if="editable">
      <div class="row">
        <label v-if="isHQ">盘点位置
          <select v-model.number="loc">
            <option :value="0">总库</option>
            <option v-for="d in enabledDists" :key="d.id" :value="d.id">{{ d.name }}</option>
          </select>
        </label>
        <span v-else>盘点位置：{{ userStore }}</span>
        <label>条码 <input v-model="input" placeholder="逐件扫码后回车" @keyup.enter="add" class="mono" /></label>
        <button class="mini" @click="add">添加</button>
        <span class="ok" v-if="scans.length">已扫 {{ scans.length }} 件</span>
      </div>
      <table v-if="scans.length">
        <tbody>
          <tr v-for="(x, i) in scans" :key="x.barcode">
            <td style="width:40px">{{ i + 1 }}</td>
            <td class="mono" style="width:160px">{{ x.barcode }}</td>
            <td>{{ x.name || '—' }}</td>
            <td style="width:70px"><button class="mini" @click="removeAt(i)">删行</button></td>
          </tr>
        </tbody>
      </table>
      <div class="row" v-if="scans.length">
        <span class="spacer"></span>
        <button v-if="status === '草稿'" class="mini danger" @click="delDraft">删除草稿</button>
        <button class="gray" @click="save(false)">保存草稿(明天接着盘)</button>
        <button @click="save(true)">完成盘点(对账)</button>
      </div>
      <p class="hint">只收系统存在过的条码；重复扫自动去重。确认那一刻对账：账面应在没扫到=盘亏；扫到了但账面不在此位置=盘盈（自动带出关联单据）。盘点不改库存——处理差异走各自的业务单据。</p>
    </template>

    <template v-else-if="result">
      <div class="row">
        <span class="ok">正常 {{ result.normal }}</span>
        <span :style="result.loss ? 'color:#c0392b;font-weight:bold' : ''">盘亏 {{ result.loss }}</span>
        <span :style="result.gain ? 'color:#b8860b;font-weight:bold' : ''">盘盈 {{ result.gain }}</span>
        <span class="hint">实扫 {{ result.scanned }} 件</span>
      </div>
      <table v-if="result.lossList?.length">
        <thead><tr><th colspan="3" style="color:#c0392b">盘亏（账面应在、实物没扫到）</th></tr></thead>
        <tbody>
          <tr v-for="x in result.lossList" :key="x.barcode">
            <td class="mono" style="width:160px">{{ x.barcode }}</td>
            <td>{{ x.name }}</td>
            <td style="width:110px">账面「{{ x.status }}」</td>
          </tr>
        </tbody>
      </table>
      <table v-if="result.gainList?.length">
        <thead><tr><th colspan="4" style="color:#b8860b">盘盈（扫到了、账面不在此位置）</th></tr></thead>
        <tbody>
          <tr v-for="x in result.gainList" :key="x.barcode">
            <td class="mono" style="width:160px">{{ x.barcode }}</td>
            <td>{{ x.name }}</td>
            <td style="width:150px">账面「{{ x.status }}」@{{ x.location }}</td>
            <td class="mono" style="width:150px">{{ x.refDocNo ? '关联 ' + x.refDocNo : '—' }}</td>
          </tr>
        </tbody>
      </table>
      <p class="hint" v-if="!result.lossList?.length && !result.gainList?.length">账实完全一致，没有差异。</p>
      <div class="row">
        <span class="spacer"></span>
        <button class="mini danger" @click="unconfirm">反确认(重盘)</button>
      </div>
    </template>

    <p v-if="err" class="err">{{ err }}</p>
  </section>
</template>
