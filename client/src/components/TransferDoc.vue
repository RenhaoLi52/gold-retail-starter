<script setup lang="ts">
// 调拨单据页（v0.32）：调出方→调入方 + 扫码集件。
// 同一张单覆盖三种用法：总库→分销商＝分货（分销进货）；分销商→总库＝退回（分销退货）；
// 分销商→分销商＝互调。
import { computed, onMounted, ref } from 'vue'
import { api } from '../api'

interface Item { barcode: string; name: string; status: string; location?: string }
interface Distributor { id: number; name: string; status: number }
interface TLine { barcode: string; name: string; purity?: string; weightG?: number }
interface Doc {
  id: number; docNo: string; status: string
  fromDistributorId: number; fromName: string
  toDistributorId: number; toName: string
  lines: TLine[]; madeAt: string
}

const props = defineProps<{ docId: number; initial: Doc | null; items: Item[]; distributors: Distributor[] }>()
const emit = defineEmits<{ close: []; refresh: []; flash: [string]; rename: [string] }>()

const id = ref(props.docId)
const docNo = ref('')
const status = ref('新单')
const madeAt = ref('')
const from = ref(0) // 0=总库
const to = ref(0)
const input = ref('')
const barcodes = ref<{ barcode: string; name: string }[]>([])
const lines = ref<TLine[]>([]) // 已确认只读
const err = ref('')

const editable = computed(() => status.value !== '已确认')
const enabledDists = computed(() => props.distributors.filter(d => d.status === 1))
const locName = (did: number) => (!did ? '总库' : props.distributors.find(d => d.id === did)?.name ?? '?')

function initFrom(d: Doc) {
  id.value = d.id
  docNo.value = d.docNo
  status.value = d.status
  madeAt.value = d.madeAt
  from.value = d.fromDistributorId || 0
  to.value = d.toDistributorId || 0
  if (d.status === '草稿') barcodes.value = d.lines.map(l => ({ barcode: l.barcode, name: l.name }))
  else lines.value = d.lines
}
async function reload() {
  const r = await api.transferList()
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
  if (barcodes.value.some(x => x.barcode === bc)) {
    err.value = `条码 ${bc} 已在本单中`
    return
  }
  const it = props.items.find(x => x.barcode === bc)
  if (it) {
    if (it.status !== '在库') {
      err.value = `条码 ${bc} 当前状态「${it.status}」，不能调拨`
      return
    }
    if ((it.location ?? '总库') !== locName(from.value)) {
      err.value = `条码 ${bc} 在「${it.location}」处，不在调出方「${locName(from.value)}」`
      return
    }
  }
  // 不在当前列表里（可能被筛选滤掉）也允许先加——保存时服务端把关
  barcodes.value.push({ barcode: bc, name: it?.name ?? '' })
  input.value = ''
  err.value = ''
}
function removeAt(i: number) { barcodes.value.splice(i, 1) }

async function save(confirmAfter: boolean) {
  err.value = ''
  try {
    const r = await api.transferSave({
      id: id.value, fromDistributorId: from.value, toDistributorId: to.value,
      barcodes: barcodes.value.map(x => x.barcode),
    })
    if (id.value === 0) {
      id.value = r.id
      docNo.value = r.docNo
      emit('rename', r.docNo)
    }
    status.value = '草稿'
    if (confirmAfter) {
      await api.transferConfirm(id.value)
      status.value = '已确认'
      await reload()
      emit('flash', `调拨已确认：${docNo.value}（${locName(from.value)} → ${locName(to.value)}）`)
    } else {
      emit('flash', `调拨草稿已保存：${docNo.value}`)
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
    await api.transferUnconfirm(id.value)
    status.value = '草稿'
    await reload()
    emit('flash', `调拨已反确认：${docNo.value}，货品拉回「${locName(from.value)}」待处理`)
    emit('refresh')
  } catch (e) {
    err.value = (e as Error).message
  }
}
async function delDraft() {
  err.value = ''
  try {
    await api.transferDelete(id.value)
    emit('flash', `调拨草稿 ${docNo.value} 已删除`)
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
      <span v-else>新建调拨单</span>
      <span :class="['badge', status === '已确认' ? 'ok2' : 'draft']">{{ status }}</span>
      <span class="hint">{{ locName(from) }} → {{ locName(to) }}</span>
      <span v-if="madeAt" class="hint">制单：{{ madeAt }}</span>
      <span class="spacer"></span>
      <button class="mini gray" @click="emit('close')">关闭</button>
    </h2>

    <template v-if="editable">
      <div class="row">
        <label>调出方
          <select v-model.number="from">
            <option :value="0">总库</option>
            <option v-for="d in enabledDists" :key="d.id" :value="d.id">{{ d.name }}</option>
          </select>
        </label>
        <span>→</span>
        <label>调入方
          <select v-model.number="to">
            <option :value="0">总库</option>
            <option v-for="d in enabledDists" :key="d.id" :value="d.id">{{ d.name }}</option>
          </select>
        </label>
        <label>条码 <input v-model="input" placeholder="扫码或输入后回车" @keyup.enter="add" class="mono" /></label>
        <button class="mini" @click="add">添加</button>
      </div>
      <table v-if="barcodes.length">
        <thead><tr><th>#</th><th>条码</th><th>名称</th><th></th></tr></thead>
        <tbody>
          <tr v-for="(x, i) in barcodes" :key="x.barcode">
            <td>{{ i + 1 }}</td>
            <td class="mono">{{ x.barcode }}</td>
            <td>{{ x.name || '—' }}</td>
            <td><button class="mini" @click="removeAt(i)">删行</button></td>
          </tr>
        </tbody>
      </table>
      <div class="row" v-if="barcodes.length">
        <p class="ok" style="margin:0">共 {{ barcodes.length }} 件：{{ locName(from) }} → {{ locName(to) }}</p>
        <span class="spacer"></span>
        <button v-if="status === '草稿'" class="mini danger" @click="delDraft">删除草稿</button>
        <button class="gray" @click="save(false)">保存草稿</button>
        <button @click="save(true)">确认调拨</button>
      </div>
      <p class="hint">总库→分销商＝分货（分销进货）；分销商→总库＝退回（分销退货）；分销商→分销商＝互调。草稿即占用（调拨中）。</p>
    </template>

    <template v-else>
      <table>
        <thead><tr><th>#</th><th>条码</th><th>名称</th><th>成色</th><th>重量(g)</th></tr></thead>
        <tbody>
          <tr v-for="(l, j) in lines" :key="j">
            <td>{{ j + 1 }}</td>
            <td class="mono">{{ l.barcode }}</td>
            <td>{{ l.name }}</td>
            <td>{{ l.purity }}</td>
            <td>{{ (l.weightG ?? 0).toFixed(2) }}</td>
          </tr>
        </tbody>
      </table>
      <div class="row">
        <span class="spacer"></span>
        <button class="mini danger" @click="unconfirm">反确认</button>
      </div>
    </template>

    <p v-if="err" class="err">{{ err }}</p>
  </section>
</template>
