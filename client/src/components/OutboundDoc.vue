<script setup lang="ts">
// 退库单据页（v0.32）：扫码集件 → 保存/确认；已确认转只读。
import { computed, onMounted, ref } from 'vue'
import { api } from '../api'

interface Item { barcode: string; name: string; purity: string; weightG: number; status: string }
interface Doc { id: number; docNo: string; supplier: string; status: string; items: Item[]; madeAt: string }

const props = defineProps<{ docId: number; initial: Doc | null; items: Item[] }>()
const emit = defineEmits<{ close: []; refresh: []; flash: [string]; rename: [string] }>()

const id = ref(props.docId)
const docNo = ref('')
const status = ref('新单')
const madeAt = ref('')
const supplier = ref('')
const barcodes = ref<string[]>([])
const input = ref('')
const items = ref<Item[]>([]) // 已确认只读明细
const err = ref('')

const editable = computed(() => status.value !== '已确认')
const itemOf = (bc: string) => props.items.find(x => x.barcode === bc)

function initFrom(d: Doc) {
  id.value = d.id
  docNo.value = d.docNo
  status.value = d.status
  madeAt.value = d.madeAt
  supplier.value = d.supplier
  if (d.status === '草稿') barcodes.value = d.items.map(it => it.barcode)
  else items.value = d.items
}
async function reload() {
  const r = await api.outboundList()
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
  if (barcodes.value.includes(bc)) {
    err.value = `条码 ${bc} 已在本单中`
    return
  }
  const it = itemOf(bc)
  if (!it) {
    err.value = `条码 ${bc} 不在库存列表中（保存时以服务端校验为准）`
  } else if (it.status !== '在库') {
    err.value = it.status === '销售中' || it.status === '退库中'
      ? `条码 ${bc} 当前「${it.status}」（被某张草稿占着）——先处理那张草稿`
      : `条码 ${bc} 当前状态「${it.status}」，不能退库`
    return
  } else {
    err.value = ''
  }
  barcodes.value.push(bc)
  input.value = ''
}
function removeAt(i: number) { barcodes.value.splice(i, 1) }

async function save(confirmAfter: boolean) {
  err.value = ''
  try {
    const r = await api.outboundSave({ id: id.value, supplier: supplier.value, barcodes: barcodes.value })
    if (id.value === 0) {
      id.value = r.id
      docNo.value = r.docNo
      emit('rename', r.docNo)
    }
    status.value = '草稿'
    if (confirmAfter) {
      await api.outboundConfirm(id.value)
      status.value = '已确认'
      await reload()
      emit('flash', `退库已确认：${docNo.value}`)
    } else {
      emit('flash', `退库草稿已保存：${docNo.value}`)
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
    await api.outboundUnconfirm(id.value)
    status.value = '草稿'
    await reload()
    emit('flash', `退库已反确认：${docNo.value}，货品已回到在库`)
    emit('refresh')
  } catch (e) {
    err.value = (e as Error).message
  }
}
async function delDraft() {
  err.value = ''
  try {
    await api.outboundDelete(id.value)
    emit('flash', `退库草稿 ${docNo.value} 已删除`)
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
      <span v-else>新建退库单</span>
      <span :class="['badge', status === '已确认' ? 'ok2' : 'draft']">{{ status }}</span>
      <span v-if="madeAt" class="hint">制单：{{ madeAt }}</span>
      <span class="spacer"></span>
      <button class="mini gray" @click="emit('close')">关闭</button>
    </h2>

    <template v-if="editable">
      <div class="row">
        <label>退往供应商 <input v-model="supplier" placeholder="选填" /></label>
        <label>条码 <input v-model="input" placeholder="扫码或输入后回车" @keyup.enter="add" class="mono" /></label>
        <button class="mini" @click="add">添加</button>
      </div>
      <table v-if="barcodes.length">
        <thead><tr><th>#</th><th>条码</th><th>名称</th><th>成色</th><th>重量(g)</th><th></th></tr></thead>
        <tbody>
          <tr v-for="(bc, i) in barcodes" :key="bc">
            <td>{{ i + 1 }}</td>
            <td class="mono">{{ bc }}</td>
            <td>{{ itemOf(bc)?.name || '?' }}</td>
            <td>{{ itemOf(bc)?.purity || '?' }}</td>
            <td>{{ (itemOf(bc)?.weightG ?? 0).toFixed(2) }}</td>
            <td><button class="mini" @click="removeAt(i)">移除</button></td>
          </tr>
        </tbody>
      </table>
      <div class="row" v-if="barcodes.length">
        <span class="spacer"></span>
        <button v-if="status === '草稿'" class="mini danger" @click="delDraft">删除草稿</button>
        <button class="gray" @click="save(false)">保存草稿</button>
        <button @click="save(true)">保存并确认退库</button>
      </div>
      <p class="hint">确认后货品状态变为"已退库"；退过库的货品会阻止其入库单反确认（下游校验）。</p>
    </template>

    <template v-else>
      <p class="hint" v-if="supplier">退往供应商：{{ supplier }}</p>
      <table>
        <thead><tr><th>#</th><th>条码</th><th>名称</th><th>成色</th><th>重量(g)</th></tr></thead>
        <tbody>
          <tr v-for="(it, j) in items" :key="j">
            <td>{{ j + 1 }}</td>
            <td class="mono">{{ it.barcode }}</td>
            <td>{{ it.name }}</td>
            <td>{{ it.purity }}</td>
            <td>{{ (it.weightG ?? 0).toFixed(2) }}</td>
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
