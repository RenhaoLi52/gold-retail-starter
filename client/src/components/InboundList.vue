<script setup lang="ts">
// 入库工作台（v0.31，仿 JMP）：上=近期入库单概览表（可滚动），
// 单击行→下方明细预览；双击行→开独立单据页签（取单）；新建/导入也开页签。
import { computed, ref, watch } from 'vue'
import { api, downloadFile, uploadFile } from '../api'

interface DictItem { id: number; name: string; sort: number; enabled: boolean }
interface DocItem { barcode: string; name: string; purity: string; weightG: number; price?: number }
interface Doc { id: number; docNo: string; category: string; status: string; items: DocItem[]; madeAt: string; madeBy?: string }

const props = defineProps<{ docs: Doc[]; dicts: Record<string, DictItem[]> }>()
const emit = defineEmits<{
  refresh: []
  flash: [string]
  open: [Doc]        // 双击/取单：开单据页签
  create: []         // 新建：开空白单据页签
  imported: [{ id: number; docNo: string; count: number }] // Excel导入成功，请App刷新并打开该草稿
}>()

const selId = ref(0)
const sel = computed(() => props.docs.find(d => d.id === selId.value) ?? null)
const err = ref('')

// 列表刷新后保持选中；选中的单没了（被删）就选第一张
watch(() => props.docs, v => {
  if (!v.find(d => d.id === selId.value)) selId.value = v[0]?.id ?? 0
}, { immediate: true })

const sumW = (d: Doc) => (d.items ?? []).reduce((a, it) => a + (it.weightG ?? 0), 0)

// ---- Excel 导入（导入成的草稿直接开页签复核）----
const importCat = ref('黄金')
const importFileEl = ref<HTMLInputElement | null>(null)
const enabledCats = computed(() => (props.dicts.category ?? []).filter(d => d.enabled))

async function downloadTemplate() {
  err.value = ''
  try {
    await downloadFile('/api/doc/inbound/import-template', '入库导入模板.xlsx')
  } catch (e) {
    err.value = (e as Error).message
  }
}
async function importExcel(ev: Event) {
  err.value = ''
  const input = ev.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (!file) return
  try {
    const r = await uploadFile('/api/doc/inbound/import', file, { category: importCat.value })
    emit('imported', r as { id: number; docNo: string; count: number })
  } catch (e) {
    err.value = (e as Error).message
  }
}

// ---- 选中单据的快捷操作（确认/反确认/删除/导出标签，不开页签也能办事）----
async function confirmSel() {
  if (!sel.value) return
  err.value = ''
  try {
    const doc = await api.inboundConfirm(sel.value.id)
    emit('flash', `已确认：${doc.docNo}`)
    emit('refresh')
  } catch (e) {
    err.value = (e as Error).message
    emit('refresh')
  }
}
async function unconfirmSel() {
  if (!sel.value) return
  err.value = ''
  try {
    await api.inboundUnconfirm(sel.value.id)
    emit('flash', `已反确认：${sel.value.docNo} 退回草稿，货品已撤回`)
    emit('refresh')
  } catch (e) {
    err.value = (e as Error).message
    emit('refresh')
  }
}
async function deleteSel() {
  if (!sel.value) return
  err.value = ''
  try {
    await api.inboundDelete(sel.value.id)
    emit('flash', `草稿 ${sel.value.docNo} 已删除（单号不回收）`)
    emit('refresh')
  } catch (e) {
    err.value = (e as Error).message
  }
}
async function exportLabelsSel() {
  if (!sel.value) return
  err.value = ''
  try {
    await downloadFile(`/api/doc/inbound/labels?id=${sel.value.id}`, `标签数据-${sel.value.docNo}.xlsx`)
    emit('flash', `标签数据已导出：${sel.value.docNo}`)
  } catch (e) {
    err.value = (e as Error).message
  }
}
</script>

<template>
  <!-- 上：单据概览 -->
  <section class="card">
    <h2>
      入库单（{{ docs.length }} 张）
      <button class="mini" @click="emit('refresh')">刷新</button>
      <span class="spacer"></span>
      <label class="hint">导入大类
        <select v-model="importCat">
          <option v-for="c in enabledCats" :key="c.id" :value="c.name">{{ c.name }}</option>
        </select>
      </label>
      <button class="mini" @click="downloadTemplate">下载导入模板</button>
      <button class="mini" @click="importFileEl?.click()">Excel导入</button>
      <input ref="importFileEl" type="file" accept=".xlsx" style="display:none" @change="importExcel" />
      <button @click="emit('create')">+ 新建入库单</button>
    </h2>
    <div class="grid-scroll">
      <table class="pick">
        <thead>
          <tr><th>#</th><th>入库单号</th><th>首饰大类</th><th>入库时间</th><th>件数</th><th>总重(g)</th><th>状态</th><th>制单人</th></tr>
        </thead>
        <tbody>
          <tr v-for="(d, i) in docs" :key="d.id"
            :class="{ sel: d.id === selId }"
            @click="selId = d.id" @dblclick="emit('open', d)">
            <td>{{ i + 1 }}</td>
            <td class="mono">{{ d.docNo }}</td>
            <td>{{ d.category }}</td>
            <td>{{ d.madeAt }}</td>
            <td>{{ d.items?.length ?? 0 }}</td>
            <td>{{ sumW(d).toFixed(2) }}</td>
            <td><span :class="['badge', d.status === '草稿' ? 'draft' : 'ok2']">{{ d.status }}</span></td>
            <td>{{ d.madeBy || '—' }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <p class="hint">单击一行在下方预览明细；双击打开单据页签（取单），草稿可继续编辑。</p>
  </section>

  <!-- 下：明细预览 -->
  <section class="card">
    <template v-if="sel">
      <h2>
        明细预览 <span class="mono">{{ sel.docNo }}</span>
        <span :class="['badge', sel.status === '草稿' ? 'draft' : 'ok2']">{{ sel.status }}</span>
        <span class="hint">{{ sel.category }} · {{ sel.items?.length ?? 0 }} 件 · {{ sumW(sel).toFixed(2) }}g</span>
        <span class="spacer"></span>
        <template v-if="sel.status === '草稿'">
          <button class="mini" @click="emit('open', sel)">打开编辑</button>
          <button class="mini" @click="confirmSel">确认</button>
          <button class="mini danger" @click="deleteSel">删除</button>
        </template>
        <template v-else>
          <button class="mini" @click="emit('open', sel)">打开</button>
          <button class="mini" @click="exportLabelsSel">导出标签</button>
          <button class="mini danger" @click="unconfirmSel">反确认</button>
        </template>
      </h2>
      <div class="grid-scroll preview">
        <table>
          <thead><tr><th>#</th><th>条码号</th><th>首饰名称</th><th>成色</th><th>克重(g)</th><th>售价(¥)</th></tr></thead>
          <tbody>
            <tr v-for="(it, j) in sel.items" :key="j">
              <td>{{ j + 1 }}</td>
              <td class="mono">{{ it.barcode || '(待发号)' }}</td>
              <td>{{ it.name }}</td>
              <td>{{ it.purity }}</td>
              <td>{{ (it.weightG ?? 0).toFixed(2) }}</td>
              <td>{{ (it.price ?? 0).toFixed(2) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>
    <p v-else class="hint">暂无入库单——点右上角"新建入库单"开第一张。</p>
    <p v-if="err" class="err">{{ err }}</p>
  </section>
</template>
