<script setup lang="ts">
// 入库单据页（v0.31）：一个页签一张单，新建/编辑草稿/查看已确认 三态一体。
// 状态全在组件内部——因此可以同时开多张单据页签互不串数据（这是从 App.vue
// 全局表单搬出来成组件的根本原因）。
import { computed, onMounted, ref } from 'vue'
import { api, downloadFile, uploadFile } from '../api'

interface DictItem { id: number; name: string; sort: number; enabled: boolean }
interface DocItem {
  barcode: string; name: string; purity: string; weightG: number; price?: number; status?: string
  stoneName?: string; jewelType?: string
  saleFeeMode?: string; saleFee?: number
  costGoldPrice?: number; costFeeMode?: string; costFee?: number
}
interface Doc { id: number; docNo: string; category: string; status: string; items: DocItem[]; madeAt: string; madeBy?: string }
interface Line {
  barcode: string; purity: string; stoneName: string; jewelType: string
  weightG: number | null; price: number | null
  saleFeeMode: string; saleFee: number | null
  costGoldPrice: number | null; costFeeMode: string; costFee: number | null
}

const props = defineProps<{
  docId: number            // 0=新建
  initial: Doc | null      // 从工作台双击带进来的单据快照
  dicts: Record<string, DictItem[]>
  isAdmin: boolean
}>()
const emit = defineEmits<{
  close: []                // 请求关闭本页签
  refresh: []              // 单据有变动，请工作台刷新
  flash: [string]
  rename: [string]         // 新单拿到单号后改页签标题
}>()

const id = ref(props.docId)
const docNo = ref('')
const status = ref('新单') // 新单 / 草稿 / 已确认
const category = ref('黄金')
const madeAt = ref('')
const madeBy = ref('')
const lines = ref<Line[]>([blankLine()])
const items = ref<DocItem[]>([]) // 已确认时的只读明细
const err = ref('')

// datalist 的 id 是全局的——多页签同开时必须互不相同，用随机实例号隔离
const uid = 'ibd' + Math.random().toString(36).slice(2, 8)

function blankLine(): Line {
  return { barcode: '', purity: '足金999.9', stoneName: '', jewelType: '', weightG: null, price: null,
    saleFeeMode: '按克', saleFee: null, costGoldPrice: null, costFeeMode: '按克', costFee: null }
}
const composedName = (l: Line) => `${l.purity}${l.stoneName}${l.jewelType}`
const enabledCats = computed(() => (props.dicts.category ?? []).filter(d => d.enabled))
const enabledPurities = computed(() => (props.dicts.purity ?? []).filter(d => d.enabled))
const enabledStones = computed(() => (props.dicts.stone_name ?? []).filter(d => d.enabled))
const enabledJewels = computed(() => (props.dicts.jewel_type ?? []).filter(d => d.enabled))

const editable = computed(() => status.value !== '已确认')
const count = computed(() => (editable.value ? lines.value.length : items.value.length))
const sumW = computed(() => {
  const arr: number[] = editable.value
    ? lines.value.map(l => Number(l.weightG) || 0)
    : items.value.map(it => it.weightG ?? 0)
  return arr.reduce((a, b) => a + b, 0)
})

function initFrom(d: Doc) {
  id.value = d.id
  docNo.value = d.docNo
  status.value = d.status
  category.value = d.category
  madeAt.value = d.madeAt
  madeBy.value = d.madeBy ?? ''
  if (d.status === '草稿') {
    lines.value = d.items.map(it => ({
      barcode: it.barcode,
      purity: it.purity,
      stoneName: it.stoneName ?? '',
      jewelType: it.jewelType ?? (it.stoneName === undefined ? it.name : ''), // 老草稿退回整名
      weightG: it.weightG,
      price: it.price ?? 0,
      saleFeeMode: it.saleFeeMode || '按克',
      saleFee: it.saleFee ?? 0,
      costGoldPrice: it.costGoldPrice ?? 0,
      costFeeMode: it.costFeeMode || '按克',
      costFee: it.costFee ?? 0,
    }))
    if (!lines.value.length) lines.value = [blankLine()]
  } else {
    items.value = d.items
  }
}

// 反确认后草稿行要带回 主石/类别/工费——列表接口的草稿分支才有这些字段，重拉一次
async function reload() {
  const r = await api.inboundList()
  const d = (r.list as Doc[]).find(x => x.id === id.value)
  if (d) initFrom(d)
}

onMounted(() => {
  if (props.initial) initFrom(props.initial)
  else if (id.value > 0) reload().catch(e => (err.value = (e as Error).message))
})

function addLine() { lines.value.push(blankLine()) }
function removeLine(i: number) { lines.value.splice(i, 1) }

function payload() {
  return {
    id: id.value,
    category: category.value,
    lines: lines.value.map(l => ({
      barcode: l.barcode,
      purity: l.purity,
      stoneName: l.stoneName.trim(),
      jewelType: l.jewelType.trim(),
      weightG: Number(l.weightG) || 0,
      price: Number(l.price) || 0,
      saleFeeMode: l.saleFeeMode,
      saleFee: Number(l.saleFee) || 0,
      costGoldPrice: Number(l.costGoldPrice) || 0,
      costFeeMode: l.costFeeMode,
      costFee: Number(l.costFee) || 0,
    })),
  }
}

async function saveDraft() {
  err.value = ''
  try {
    const r = await api.inboundSave(payload())
    if (id.value === 0) {
      id.value = r.id
      docNo.value = r.docNo
      emit('rename', r.docNo)
    }
    status.value = '草稿'
    emit('flash', `草稿已保存：${docNo.value}`)
    emit('refresh')
  } catch (e) {
    err.value = (e as Error).message
  }
}

async function saveAndConfirm() {
  err.value = ''
  try {
    const r = await api.inboundSave(payload())
    if (id.value === 0) {
      id.value = r.id
      docNo.value = r.docNo
      emit('rename', r.docNo)
    }
    const doc = await api.inboundConfirm(id.value)
    items.value = doc.items
    status.value = '已确认'
    emit('flash', `已确认：${doc.docNo}，生成 ${doc.items.length} 件货品`)
    emit('refresh')
  } catch (e) {
    err.value = (e as Error).message
    emit('refresh') // 保存可能已成功，让工作台看到草稿
  }
}

async function unconfirm() {
  err.value = ''
  try {
    await api.inboundUnconfirm(id.value)
    status.value = '草稿'
    await reload()
    emit('flash', `已反确认：${docNo.value} 退回草稿，货品已撤回`)
    emit('refresh')
  } catch (e) {
    err.value = (e as Error).message
  }
}

async function delDraft() {
  err.value = ''
  try {
    await api.inboundDelete(id.value)
    emit('flash', `草稿 ${docNo.value} 已删除（单号不回收）`)
    emit('refresh')
    emit('close')
  } catch (e) {
    err.value = (e as Error).message
  }
}

async function exportLabels() {
  err.value = ''
  try {
    await downloadFile(`/api/doc/inbound/labels?id=${id.value}`, `标签数据-${docNo.value}.xlsx`)
    emit('flash', `标签数据已导出：${docNo.value}——在 Label Matrix 里把数据源指向该文件即可打印`)
  } catch (e) {
    err.value = (e as Error).message
  }
}

// ===== Excel 导入（v0.32：列名映射 + 追加行）=====
// 两步走：①上传文件让服务端解析出表头和数据行；②用户确认"Excel列→货品属性"
// 的映射（列名完全相同的自动配上，配不上的手选），确认后把行【追加】到当前
// 表格——不覆盖已手工录入的行。校验留给保存/确认时的统一通道，报错行号
// 与表格里看到的行号一致。
const importTargets = [
  { key: 'barcode', label: '条码号' },
  { key: 'purity', label: '成色' },
  { key: 'stoneName', label: '主石名称' },
  { key: 'jewelType', label: '首饰类别' },
  { key: 'weightG', label: '总件重' },
  { key: 'price', label: '售价' },
  { key: 'saleFeeMode', label: '销售工费方式' },
  { key: 'saleFee', label: '销售工费' },
  { key: 'costGoldPrice', label: '进货金价' },
  { key: 'costFeeMode', label: '进货工费方式' },
  { key: 'costFee', label: '进货工费' },
] as const
const numKeys = new Set(['weightG', 'price', 'saleFee', 'costGoldPrice', 'costFee'])
const importFileEl = ref<HTMLInputElement | null>(null)
const imp = ref<null | { headers: string[]; rows: string[][]; map: string[] }>(null)

async function downloadTemplate() {
  err.value = ''
  try {
    await downloadFile('/api/doc/inbound/import-template', '入库导入模板.xlsx')
  } catch (e) {
    err.value = (e as Error).message
  }
}
const norm = (h: string) => h.replace(/[\s\n]/g, '')
async function pickImportFile(ev: Event) {
  err.value = ''
  const input = ev.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (!file) return
  try {
    const r = await uploadFile('/api/import/preview', file, {}) as { headers: string[]; rows: string[][] }
    imp.value = {
      headers: r.headers,
      rows: r.rows,
      // 自动映射：列名与属性名完全相同（忽略空格）就配上；配不上留空待手选
      map: r.headers.map(h => {
        const hit = importTargets.find(t => t.label === norm(h)
          || t.label + '(g)' === norm(h) || t.label + '(¥)' === norm(h)) // 模板里带单位的写法也认
        return hit?.key ?? ''
      }),
    }
  } catch (e) {
    err.value = (e as Error).message
  }
}
// 同一属性被映射到两列是配置错误，提示出来
const dupTargets = computed(() => {
  if (!imp.value) return []
  const seen: Record<string, number> = {}
  for (const k of imp.value.map) if (k) seen[k] = (seen[k] ?? 0) + 1
  return importTargets.filter(t => (seen[t.key] ?? 0) > 1).map(t => t.label)
})
function applyImport() {
  if (!imp.value) return
  err.value = ''
  if (dupTargets.value.length) {
    err.value = `属性「${dupTargets.value.join('、')}」被映射了多列——每个属性最多对应一列`
    return
  }
  for (const need of ['purity', 'jewelType', 'weightG'] as const) {
    if (!imp.value.map.includes(need)) {
      err.value = `必须映射「${importTargets.find(t => t.key === need)!.label}」这一列（成色/首饰类别/总件重是必填属性）`
      return
    }
  }
  const newLines: Line[] = []
  for (const row of imp.value.rows) {
    const l = blankLine()
    l.purity = '' // 导入行不吃默认成色，映射到什么就是什么
    imp.value.map.forEach((key, i) => {
      if (!key) return
      const cell = (row[i] ?? '').trim()
      if (numKeys.has(key)) {
        ;(l as any)[key] = cell === '' ? null : Number(cell)
      } else if (key === 'saleFeeMode' || key === 'costFeeMode') {
        ;(l as any)[key] = cell || '按克'
      } else {
        ;(l as any)[key] = cell
      }
    })
    newLines.push(l)
  }
  // 追加而不是替换：手工录到一半也能并进来；表单里尚是全空白的占位行剔掉
  const notBlank = (l: Line) => l.barcode || l.purity || l.stoneName || l.jewelType
    || l.weightG !== null || l.price !== null || l.saleFee !== null
    || l.costGoldPrice !== null || l.costFee !== null
  lines.value = [...lines.value.filter(notBlank), ...newLines]
  if (!lines.value.length) lines.value = [blankLine()]
  emit('flash', `已追加 ${newLines.length} 行到表格——请复核后保存`)
  imp.value = null
}
</script>

<template>
  <section class="card">
    <h2>
      <span v-if="docNo" class="mono">{{ docNo }}</span>
      <span v-else>新建入库单</span>
      <span :class="['badge', status === '已确认' ? 'ok2' : 'draft']">{{ status }}</span>
      <span class="spacer"></span>
      <button class="mini gray" @click="emit('close')">关闭</button>
    </h2>

    <div class="row">
      <label>首饰大类
        <select v-model="category" :disabled="!editable">
          <option v-for="c in enabledCats" :key="c.id" :value="c.name">{{ c.name }}</option>
        </select>
      </label>
      <span v-if="madeAt" class="hint">制单：{{ madeAt }}<template v-if="madeBy">（{{ madeBy }}）</template></span>
      <span class="spacer"></span>
      <template v-if="editable">
        <button class="mini" @click="downloadTemplate">下载导入模板</button>
        <button class="mini" @click="importFileEl?.click()">Excel导入</button>
        <input ref="importFileEl" type="file" accept=".xlsx" style="display:none" @change="pickImportFile" />
      </template>
      <span class="hint">{{ count }} 件 · 共 {{ sumW.toFixed(2) }}g</span>
    </div>

    <!-- 列名映射确认（v0.32）：选了文件后出现，确认后把行追加进下面的表格 -->
    <div v-if="editable && imp" class="impmap">
      <p style="margin:4px 0"><b>确认列名映射</b>（{{ imp.rows.length }} 行数据）：
        列名与属性完全相同的已自动配上，其余请手选；不需要的列选"忽略"。</p>
      <table>
        <thead><tr><th>Excel 列名</th><th>第一行示例</th><th>导入为属性</th></tr></thead>
        <tbody>
          <tr v-for="(h, i) in imp.headers" :key="i">
            <td>{{ h || '（无列名）' }}</td>
            <td class="hint">{{ imp.rows[0]?.[i] || '—' }}</td>
            <td>
              <select v-model="imp.map[i]">
                <option value="">— 忽略 —</option>
                <option v-for="t in importTargets" :key="t.key" :value="t.key">{{ t.label }}</option>
              </select>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-if="dupTargets.length" class="err">属性「{{ dupTargets.join('、') }}」被映射了多列，请调整。</p>
      <div class="row">
        <span class="hint">成色 / 首饰类别 / 总件重 必须映射；导入是【追加】，不会清掉已录入的行。</span>
        <span class="spacer"></span>
        <button class="mini gray" @click="imp = null">取消</button>
        <button class="mini" @click="applyImport">确认导入 {{ imp.rows.length }} 行</button>
      </div>
    </div>

    <!-- 草稿/新单：可编辑行表格 -->
    <template v-if="editable">
      <table>
        <thead>
          <tr><th>#</th><th>条码号(留空自动生成)</th><th>成色</th><th>主石名称</th><th>首饰类别</th><th>名称(自动)</th><th>总件重(g)</th><th>售价(¥)</th><th>销售工费</th><th v-if="isAdmin">进货金价</th><th v-if="isAdmin">进货工费</th><th></th></tr>
        </thead>
        <tbody>
          <tr v-for="(l, i) in lines" :key="i">
            <td>{{ i + 1 }}</td>
            <td><input v-model="l.barcode" placeholder="自动生成" style="width:110px" /></td>
            <td><input v-model="l.purity" :list="uid + 'p'" style="width:92px" /></td>
            <td><input v-model="l.stoneName" :list="uid + 's'" placeholder="素金留空" style="width:84px" /></td>
            <td><input v-model="l.jewelType" :list="uid + 'j'" style="width:76px" /></td>
            <td class="hint">{{ composedName(l) || '—' }}</td>
            <td><input v-model.number="l.weightG" type="number" step="0.01" style="width:80px" /></td>
            <td><input v-model.number="l.price" type="number" step="1" placeholder="0" style="width:80px" /></td>
            <td>
              <select v-model="l.saleFeeMode" style="width:64px"><option>按克</option><option>按件</option></select>
              <input v-model.number="l.saleFee" type="number" step="0.5" placeholder="0" style="width:64px" />
            </td>
            <td v-if="isAdmin"><input v-model.number="l.costGoldPrice" type="number" step="0.01" placeholder="0" style="width:80px" /></td>
            <td v-if="isAdmin">
              <select v-model="l.costFeeMode" style="width:64px"><option>按克</option><option>按件</option></select>
              <input v-model.number="l.costFee" type="number" step="0.5" placeholder="0" style="width:64px" />
            </td>
            <td><button class="mini" @click="removeLine(i)" :disabled="lines.length === 1">删行</button></td>
          </tr>
        </tbody>
      </table>
      <datalist :id="uid + 'p'"><option v-for="pu in enabledPurities" :key="pu.id" :value="pu.name" /></datalist>
      <datalist :id="uid + 's'"><option v-for="s in enabledStones" :key="s.id" :value="s.name" /></datalist>
      <datalist :id="uid + 'j'"><option v-for="jt in enabledJewels" :key="jt.id" :value="jt.name" /></datalist>
      <div class="row">
        <button class="mini" @click="addLine">+ 增加行</button>
        <span class="spacer"></span>
        <button v-if="status === '草稿'" class="mini danger" @click="delDraft">删除草稿</button>
        <button class="gray" @click="saveDraft">保存草稿</button>
        <button @click="saveAndConfirm">保存并确认</button>
      </div>
      <p class="hint">名称=成色+主石名称+首饰类别 自动拼接；三个字段可下拉选也可直接填，确认时新值自动补进字典。按克货售价可填0，销售按 克重×金价+销售工费<template v-if="isAdmin">；进货金价/工费是成本（仅管理员可见）</template>。</p>
    </template>

    <!-- 已确认：只读明细 -->
    <template v-else>
      <table>
        <thead><tr><th>#</th><th>条码号</th><th>名称</th><th>成色</th><th>克重(g)</th><th>售价(¥)</th></tr></thead>
        <tbody>
          <tr v-for="(it, j) in items" :key="j">
            <td>{{ j + 1 }}</td>
            <td class="mono">{{ it.barcode }}</td>
            <td>{{ it.name }}</td>
            <td>{{ it.purity }}</td>
            <td>{{ (it.weightG ?? 0).toFixed(2) }}</td>
            <td>{{ (it.price ?? 0).toFixed(2) }}</td>
          </tr>
        </tbody>
      </table>
      <div class="row">
        <span class="spacer"></span>
        <button class="mini" @click="exportLabels">导出标签</button>
        <button class="mini danger" @click="unconfirm">反确认</button>
      </div>
    </template>

    <p v-if="err" class="err">{{ err }}</p>
  </section>
</template>
