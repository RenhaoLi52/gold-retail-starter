<script setup lang="ts">
// 入库单界面 v0.3：支持单据生命周期——保存草稿 → 确认 → 反确认 / 删除草稿
// 草稿可反复编辑；确认后生成货品件进入库存；反确认撤回（条码保留）。
import { onMounted, ref } from 'vue'
import { api, setToken, hasToken, clearToken, downloadFile } from './api'

// ===== 登录 =====
const logged = ref(false)
const username = ref('admin')
const password = ref('123456')
const userLabel = ref('')
const isAdmin = ref(false)
const userStore = ref('')  // v0.19：所属门店名，空=总部
const userStoreId = ref(0) // 所属门店id，0=总部
const isHQ = ref(true)     // 总部账号才有入库/退库/调拨视图
const errMsg = ref('')
const okMsg = ref('')

// ===== 库存 =====
interface Item {
  id: number
  barcode: string
  name: string
  category?: string
  purity: string
  weightG: number
  price?: number
  status: string
  location?: string
  saleFeeMode?: string
  saleFee?: number
  costGoldPrice?: number
  costFeeMode?: string
  costFee?: number
  jewelType?: string
  stoneName?: string
}
const items = ref<Item[]>([])
const itemsTotal = ref(0)
const itemsSumW = ref(0)
// 库存筛选条件
const fStatus = ref('')
const fCategory = ref('')
const fKeyword = ref('')
const fLoc = ref('') // ''=全部 '0'=总库 其他=分销商id

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
  purity: string
  stoneName: string
  jewelType: string
  weightG: number | null
  price: number | null
  saleFeeMode: string
  saleFee: number | null
  costGoldPrice: number | null
  costFeeMode: string
  costFee: number | null
}
// ===== 金价 =====
interface GoldPrice {
  purity: string
  retailPrice: number
  recyclePrice: number
  tradePrice: number
  by: string
  at: string
}
const goldPrices = ref<GoldPrice[]>([])
const gpHistory = ref<GoldPrice[]>([])
const showGpHistory = ref(false)
const gpPurity = ref('足金999.9')
const gpRetail = ref<number | null>(null)
const gpRecycle = ref<number | null>(null)
const gpTrade = ref<number | null>(null)

async function loadGoldPrices() {
  const r = await api.goldPriceCurrent()
  goldPrices.value = r.list
}
async function publishGoldPrice() {
  errMsg.value = ''
  try {
    await api.goldPricePublish(gpPurity.value, Number(gpRetail.value) || 0,
      Number(gpRecycle.value) || 0, Number(gpTrade.value) || 0)
    flash(`已发布 ${gpPurity.value} 金价`)
    gpRetail.value = null
    gpRecycle.value = null
    gpTrade.value = null
    await loadGoldPrices()
    if (showGpHistory.value) await loadGpHistory()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
async function loadGpHistory() {
  const r = await api.goldPriceHistory()
  gpHistory.value = r.list
}
async function toggleGpHistory() {
  showGpHistory.value = !showGpHistory.value
  if (showGpHistory.value) await loadGpHistory()
}

// ===== 销售单 =====
interface SaleLine {
  barcode: string
  name: string
  purity: string
  weightG: number
  price: number
  mode: string
  soldPrice: number | null
  saleFeeMode?: string
  saleFee?: number
}
interface PayLine {
  method: string
  amount: number | null
}
interface SDoc {
  id: number
  docNo: string
  status: string
  totalAmount: number
  salespersonIds: number[]
  salespersonName: string
  payments: PayLine[]
  oldLines: { category: string; purity: string; weightG: number; tradeG?: number; recycleG?: number; credit?: number }[]
  lines: SaleLine[]
  madeAt: string
}
const sdocs = ref<SDoc[]>([])
const slEditingId = ref(0)
const slEditingNo = ref('')
const slLines = ref<SaleLine[]>([])
const slInput = ref('')
const slSalespersonIds = ref<number[]>([])
const slPays = ref<PayLine[]>([])
// v0.24 旧料区（以旧换新/旧料回收）
interface SlOld {
  category: string
  purity: string
  weightG: number | null
}
const slOld = ref<SlOld[]>([])
function slAddOld() {
  slOld.value.push({
    category: enabledCats()[0]?.name ?? '',
    purity: enabledPurities()[0]?.name ?? '',
    weightG: null,
  })
}
function slRemoveOld(i: number) {
  slOld.value.splice(i, 1)
}
function tradeRateOf(purity: string): number {
  return goldPrices.value.find(g => g.purity === purity)?.tradePrice ?? 0
}
function recycleRateOf(purity: string): number {
  return goldPrices.value.find(g => g.purity === purity)?.recyclePrice ?? 0
}
// 预览拆分（与服务端同口径：金换金银换银、按录入顺序占额度；确认时以服务端为准）
function slOldSplit() {
  const quota: Record<string, number> = {}
  for (const l of slLines.value) {
    const it = items.value.find(x => x.barcode === l.barcode)
    const c = it?.category ?? ''
    quota[c] = (quota[c] ?? 0) + (Number(l.weightG) || 0)
  }
  const used: Record<string, number> = {}
  return slOld.value.map(o => {
    const w = Number(o.weightG) || 0
    const avail = Math.max((quota[o.category] ?? 0) - (used[o.category] ?? 0), 0)
    const tradeG = Math.min(w, avail)
    used[o.category] = (used[o.category] ?? 0) + tradeG
    const recycleG = w - tradeG
    const credit = tradeG * tradeRateOf(o.purity) + recycleG * recycleRateOf(o.purity)
    return { tradeG, recycleG, credit: Math.round(credit * 100) / 100 }
  })
}
const slCredit = () => slOldSplit().reduce((s2, x) => s2 + x.credit, 0)
const slNet = () => Math.round((slTotal() - slCredit()) * 100) / 100

function slReset() {
  slEditingId.value = 0
  slEditingNo.value = ''
  slLines.value = []
  slInput.value = ''
  slSalespersonIds.value = []
  slPays.value = []
  slOld.value = []
}
// 多售货员：点名字切换选中，最多3人（v0.20）
function slToggleSp(id: number) {
  const i = slSalespersonIds.value.indexOf(id)
  if (i >= 0) {
    slSalespersonIds.value.splice(i, 1)
  } else {
    if (slSalespersonIds.value.length >= 3) {
      errMsg.value = '售货员最多3人'
      return
    }
    slSalespersonIds.value.push(id)
  }
}

// ===== 组合收款（v0.14） =====
function slAddPay() {
  // 默认选第一个还没用过的方式
  const used = new Set(slPays.value.map(p => p.method))
  const next = enabledPayMethods().find(m => !used.has(m.name))
  slPays.value.push({ method: next?.name ?? '', amount: null })
}
function slRemovePay(i: number) {
  slPays.value.splice(i, 1)
}
const slPayTotal = () => slPays.value.reduce((s2, p) => s2 + (Number(p.amount) || 0), 0)
// 补足：把这一笔金额填成"应收 - 其他各笔合计"（顾客现金+微信各付一部分时特别省事）
function slFillPay(p: PayLine) {
  const others = slPays.value.filter(x => x !== p).reduce((s2, x) => s2 + (Number(x.amount) || 0), 0)
  p.amount = Math.round((Math.abs(slNet()) - others) * 100) / 100
}
function goldRateOf(purity: string): number {
  return goldPrices.value.find(g => g.purity === purity)?.retailPrice ?? 0
}
// 建议价：标签价→售价；变金价→克重×金价+销售工费，四舍五入到元（JMP同款口径）
function suggestPrice(l: SaleLine): number {
  if (l.mode === '标签价') return l.price
  const fee = (l.saleFeeMode === '按件') ? (l.saleFee ?? 0) : l.weightG * (l.saleFee ?? 0)
  return Math.round(l.weightG * goldRateOf(l.purity) + fee)
}
function slModeChanged(l: SaleLine) {
  l.soldPrice = suggestPrice(l)
}
function slAdd() {
  const bc = slInput.value.trim().toUpperCase()
  if (!bc) return
  if (slLines.value.some(x => x.barcode === bc)) {
    errMsg.value = `条码 ${bc} 已在本单中`
    return
  }
  const it = items.value.find(x => x.barcode === bc)
  if (!it) {
    errMsg.value = `条码 ${bc} 不在当前库存列表中`
    return
  }
  if (it.status !== '在库') {
    errMsg.value = it.status === '销售中' || it.status === '退库中'
      ? `条码 ${bc} 当前「${it.status}」（被某张草稿占着）——先处理那张草稿，或换一件`
      : `条码 ${bc} 当前状态「${it.status}」，不能销售`
    return
  }
  // 默认结算方式：有标签价用标签价，否则变金价
  const mode = (it.price ?? 0) > 0 ? '标签价' : '变金价'
  const line: SaleLine = {
    barcode: bc, name: it.name, purity: it.purity,
    weightG: it.weightG, price: it.price ?? 0, mode, soldPrice: null,
    saleFeeMode: it.saleFeeMode || '按克', saleFee: it.saleFee ?? 0,
  }
  line.soldPrice = suggestPrice(line)
  if (mode === '变金价' && goldRateOf(it.purity) <= 0) {
    errMsg.value = `成色「${it.purity}」今日未发布金价——请先发布金价或改用标签价`
  } else {
    errMsg.value = ''
  }
  slLines.value.push(line)
  slInput.value = ''
}
function slRemove(i: number) {
  slLines.value.splice(i, 1)
}
const slTotal = () => slLines.value.reduce((s2, l) => s2 + (Number(l.soldPrice) || 0), 0)

async function slSave(confirmAfter: boolean) {
  errMsg.value = ''
  try {
    const r = await api.saleSave({
      id: slEditingId.value,
      salespersonIds: slSalespersonIds.value,
      // 完全空白的收款行（没选方式）不上传；选了方式的原样上传让服务端把关
      payments: slPays.value
        .filter(p => p.method)
        .map(p => ({ method: p.method, amount: Number(p.amount) || 0 })),
      oldLines: slOld.value.filter(o => (Number(o.weightG) || 0) > 0)
        .map(o => ({ category: o.category, purity: o.purity, weightG: Number(o.weightG) })),
      lines: slLines.value.map(l => ({
        barcode: l.barcode, mode: l.mode, soldPrice: Number(l.soldPrice) || 0,
      })),
    })
    const id = slEditingId.value || r.id
    if (!slEditingId.value) {
      slEditingId.value = r.id
      slEditingNo.value = r.docNo
    }
    if (confirmAfter) {
      const c = await api.saleConfirm(id)
      flash(`已收款确认：${slEditingNo.value}，合计 ¥${c.totalAmount}`)
      slReset()
    } else {
      flash(`销售草稿已保存：${slEditingNo.value}`)
    }
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
    await refreshAll()
  }
}
async function slConfirmDoc(d: SDoc) {
  errMsg.value = ''
  try {
    const c = await api.saleConfirm(d.id)
    flash(`已收款确认：${d.docNo}，合计 ¥${c.totalAmount}`)
    if (slEditingId.value === d.id) slReset()
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
    await refreshAll()
  }
}
async function slUnconfirmDoc(d: SDoc) {
  errMsg.value = ''
  try {
    await api.saleUnconfirm(d.id)
    flash(`销售已反确认：${d.docNo}，货品回到在库`)
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
    await refreshAll()
  }
}
async function slDeleteDoc(d: SDoc) {
  errMsg.value = ''
  try {
    await api.saleDelete(d.id)
    flash(`销售草稿 ${d.docNo} 已删除`)
    if (slEditingId.value === d.id) slReset()
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
function slEdit(d: SDoc) {
  slEditingId.value = d.id
  slEditingNo.value = d.docNo
  slLines.value = d.lines.map(l => ({ ...l }))
  slSalespersonIds.value = [...(d.salespersonIds ?? [])]
  slPays.value = (d.payments ?? []).map(p => ({ ...p }))
  slOld.value = (d.oldLines ?? []).map(o => ({ category: o.category, purity: o.purity, weightG: o.weightG }))
  window.scrollTo({ top: 0, behavior: 'smooth' })
}

// ===== 分销商与调拨单（v0.18） =====
interface Distributor {
  id: number
  name: string
  status: number
}
const distributors = ref<Distributor[]>([])
const showDists = ref(false)
const ndistName = ref('')
const enabledDists = () => distributors.value.filter(d => d.status === 1)

async function createDist() {
  errMsg.value = ''
  try {
    await api.distributorCreate(ndistName.value)
    flash(`已新增分销商：${ndistName.value}`)
    ndistName.value = ''
    const r = await api.distributorList()
    distributors.value = r.list
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
async function toggleDist(d: Distributor) {
  errMsg.value = ''
  try {
    await api.distributorUpdate({ id: d.id, status: d.status === 1 ? 0 : 1 })
    flash(`${d.name} 已${d.status === 1 ? '停用' : '启用'}`)
    const r = await api.distributorList()
    distributors.value = r.list
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
async function saveDist(d: Distributor) {
  errMsg.value = ''
  try {
    await api.distributorUpdate({ id: d.id, name: d.name })
    flash(`分销商已保存：${d.name}`)
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
    const r = await api.distributorList()
    distributors.value = r.list
  }
}

interface TDoc {
  id: number
  docNo: string
  status: string
  fromDistributorId: number
  fromName: string
  toDistributorId: number
  toName: string
  lines: { barcode: string; name: string; purity: string; weightG: number }[]
  madeAt: string
}
const tdocs = ref<TDoc[]>([])
const tfEditingId = ref(0)
const tfEditingNo = ref('')
const tfFrom = ref(0) // 0=总库
const tfTo = ref(0)
const tfInput = ref('')
const tfBarcodes = ref<{ barcode: string; name: string; location: string }[]>([])

function tfReset() {
  tfEditingId.value = 0
  tfEditingNo.value = ''
  tfFrom.value = 0
  tfTo.value = 0
  tfInput.value = ''
  tfBarcodes.value = []
}
function tfLocName(id: number): string {
  if (!id) return '总库'
  return distributors.value.find(d => d.id === id)?.name ?? '?'
}
function tfAdd() {
  const bc = tfInput.value.trim().toUpperCase()
  if (!bc) return
  if (tfBarcodes.value.some(x => x.barcode === bc)) {
    errMsg.value = `条码 ${bc} 已在本单中`
    return
  }
  const it = items.value.find(x => x.barcode === bc)
  if (it) {
    if (it.status !== '在库') {
      errMsg.value = `条码 ${bc} 当前状态「${it.status}」，不能调拨`
      return
    }
    if ((it.location ?? '总库') !== tfLocName(tfFrom.value)) {
      errMsg.value = `条码 ${bc} 在「${it.location}」处，不在调出方「${tfLocName(tfFrom.value)}」`
      return
    }
  }
  // 不在当前列表里（可能被筛选条件滤掉了）也允许先加——保存时服务端把关
  tfBarcodes.value.push({ barcode: bc, name: it?.name ?? '', location: it?.location ?? '' })
  tfInput.value = ''
  errMsg.value = ''
}
function tfRemove(i: number) {
  tfBarcodes.value.splice(i, 1)
}
async function tfSave(confirmAfter: boolean) {
  errMsg.value = ''
  try {
    const r = await api.transferSave({
      id: tfEditingId.value,
      fromDistributorId: tfFrom.value,
      toDistributorId: tfTo.value,
      barcodes: tfBarcodes.value.map(x => x.barcode),
    })
    const id = tfEditingId.value || r.id
    if (!tfEditingId.value) {
      tfEditingId.value = r.id
      tfEditingNo.value = r.docNo
    }
    if (confirmAfter) {
      await api.transferConfirm(id)
      flash(`调拨已确认：${tfEditingNo.value}（${tfLocName(tfFrom.value)} → ${tfLocName(tfTo.value)}）`)
      tfReset()
    } else {
      flash(`调拨草稿已保存：${tfEditingNo.value}`)
    }
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
    await refreshAll()
  }
}
async function tfConfirmDoc(d: TDoc) {
  errMsg.value = ''
  try {
    await api.transferConfirm(d.id)
    flash(`调拨已确认：${d.docNo}`)
    if (tfEditingId.value === d.id) tfReset()
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
    await refreshAll()
  }
}
async function tfUnconfirmDoc(d: TDoc) {
  errMsg.value = ''
  try {
    await api.transferUnconfirm(d.id)
    flash(`调拨已反确认：${d.docNo}，货品拉回「${d.fromName}」待处理`)
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
    await refreshAll()
  }
}
async function tfDeleteDoc(d: TDoc) {
  errMsg.value = ''
  try {
    await api.transferDelete(d.id)
    flash(`调拨草稿 ${d.docNo} 已删除`)
    if (tfEditingId.value === d.id) tfReset()
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
function tfEdit(d: TDoc) {
  tfEditingId.value = d.id
  tfEditingNo.value = d.docNo
  tfFrom.value = d.fromDistributorId || 0
  tfTo.value = d.toDistributorId || 0
  tfBarcodes.value = d.lines.map(l => ({ barcode: l.barcode, name: l.name, location: '' }))
  window.scrollTo({ top: 0, behavior: 'smooth' })
}

// ===== 提成报表（仅管理员，v0.21） =====
interface CommRow {
  salespersonId: number
  name: string
  storeName: string
  role: string
  saleComm: number
  tradeComm: number
  managerComm: number
  returnOffset: number
  net: number
  docCount: number
}
const showReport = ref(false)
const crRows = ref<CommRow[]>([])
const crTotals = ref({ sale: 0, trade: 0, mgr: 0, ret: 0, net: 0 })
const today = new Date().toISOString().slice(0, 10)
const crFrom = ref(today.slice(0, 8) + '01') // 本月1号
const crTo = ref(today)

async function toggleReport() {
  showReport.value = !showReport.value
  if (showReport.value) await loadReport()
}
async function loadReport() {
  errMsg.value = ''
  try {
    const r = await api.commissionReport(crFrom.value, crTo.value)
    crRows.value = r.list
    crTotals.value = { sale: r.totalSale, trade: r.totalTrade, mgr: r.totalManager, ret: r.totalReturn, net: r.totalNet }
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}

// ===== 纯旧料回收单（v0.25） =====
interface HDoc {
  id: number
  docNo: string
  status: string
  payout: number
  salespersonIds: number[]
  salespersonName: string
  payments: PayLine[]
  lines: { category: string; purity: string; weightG: number; recyclePrice?: number; credit?: number }[]
  madeAt: string
}
const hdocs = ref<HDoc[]>([])
const hsEditingId = ref(0)
const hsEditingNo = ref('')
const hsLines = ref<SlOld[]>([])
const hsSalespersonIds = ref<number[]>([])
const hsPays = ref<PayLine[]>([])

function hsReset() {
  hsEditingId.value = 0
  hsEditingNo.value = ''
  hsLines.value = []
  hsSalespersonIds.value = []
  hsPays.value = []
}
function hsAddLine() {
  hsLines.value.push({
    category: enabledCats()[0]?.name ?? '',
    purity: enabledPurities()[0]?.name ?? '',
    weightG: null,
  })
}
function hsRemoveLine(i: number) {
  hsLines.value.splice(i, 1)
}
const hsTotal = () => hsLines.value.reduce(
  (s2, o) => s2 + (Number(o.weightG) || 0) * recycleRateOf(o.purity), 0)
const hsPayTotal = () => hsPays.value.reduce((s2, p) => s2 + (Number(p.amount) || 0), 0)
function hsToggleSp(id: number) {
  const i = hsSalespersonIds.value.indexOf(id)
  if (i >= 0) hsSalespersonIds.value.splice(i, 1)
  else if (hsSalespersonIds.value.length < 3) hsSalespersonIds.value.push(id)
  else errMsg.value = '售货员最多3人'
}
function hsAddPay() {
  const used = new Set(hsPays.value.map(p => p.method))
  const next = enabledPayMethods().find(m => !used.has(m.name))
  hsPays.value.push({ method: next?.name ?? '', amount: null })
}
function hsRemovePay(i: number) {
  hsPays.value.splice(i, 1)
}
function hsFillPay(p: PayLine) {
  const others = hsPays.value.filter(x => x !== p).reduce((s2, x) => s2 + (Number(x.amount) || 0), 0)
  p.amount = Math.round((hsTotal() - others) * 100) / 100
}
async function hsSave(confirmAfter: boolean) {
  errMsg.value = ''
  try {
    const r = await api.recycleSave({
      id: hsEditingId.value,
      salespersonIds: hsSalespersonIds.value,
      payments: hsPays.value.filter(p => p.method)
        .map(p => ({ method: p.method, amount: Number(p.amount) || 0 })),
      lines: hsLines.value.filter(o => (Number(o.weightG) || 0) > 0)
        .map(o => ({ category: o.category, purity: o.purity, weightG: Number(o.weightG) })),
    })
    const id = hsEditingId.value || r.id
    if (!hsEditingId.value) {
      hsEditingId.value = r.id
      hsEditingNo.value = r.docNo
    }
    if (confirmAfter) {
      const c = await api.recycleConfirm(id)
      flash(`回收已确认：${hsEditingNo.value}，付顾客 ¥${c.payout}`)
      hsReset()
    } else {
      flash(`回收草稿已保存：${hsEditingNo.value}`)
    }
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
    await refreshAll()
  }
}
async function hsConfirmDoc(d: HDoc) {
  errMsg.value = ''
  try {
    const c = await api.recycleConfirm(d.id)
    flash(`回收已确认：${d.docNo}，付顾客 ¥${c.payout}`)
    if (hsEditingId.value === d.id) hsReset()
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
    await refreshAll()
  }
}
async function hsUnconfirmDoc(d: HDoc) {
  errMsg.value = ''
  try {
    await api.recycleUnconfirm(d.id)
    flash(`回收已反确认：${d.docNo}`)
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
async function hsDeleteDoc(d: HDoc) {
  errMsg.value = ''
  try {
    await api.recycleDelete(d.id)
    flash(`回收草稿 ${d.docNo} 已删除`)
    if (hsEditingId.value === d.id) hsReset()
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
function hsEdit(d: HDoc) {
  hsEditingId.value = d.id
  hsEditingNo.value = d.docNo
  hsLines.value = d.lines.map(o => ({ category: o.category, purity: o.purity, weightG: o.weightG }))
  hsSalespersonIds.value = [...(d.salespersonIds ?? [])]
  hsPays.value = (d.payments ?? []).map(p => ({ ...p }))
  window.scrollTo({ top: 0, behavior: 'smooth' })
}

// ===== 盘点单（v0.23） =====
interface STRow {
  barcode: string
  name: string
  status: string
  location?: string
  refDocNo?: string
}
interface STResult {
  normal: number
  loss: number
  gain: number
  scanned: number
  lossList: STRow[]
  gainList: STRow[]
}
interface PDoc {
  id: number
  docNo: string
  status: string
  distributorId: number
  locName: string
  scans: { barcode: string; name: string }[]
  result: STResult | null
  madeAt: string
}
const pdocs = ref<PDoc[]>([])
const stEditingId = ref(0)
const stEditingNo = ref('')
const stLoc = ref(0)
const stInput = ref('')
const stScans = ref<{ barcode: string; name: string }[]>([])
const stShowDetail = ref(0) // 展开结果详情的单据id

function stReset() {
  stEditingId.value = 0
  stEditingNo.value = ''
  stLoc.value = userStoreId.value
  stInput.value = ''
  stScans.value = []
}
function stAdd() {
  const bc = stInput.value.trim().toUpperCase()
  if (!bc) return
  if (stScans.value.some(x => x.barcode === bc)) {
    stInput.value = '' // 重复扫到静默忽略——盘点时扫两遍很正常
    return
  }
  const it = items.value.find(x => x.barcode === bc)
  stScans.value.push({ barcode: bc, name: it?.name ?? '' })
  stInput.value = ''
  errMsg.value = ''
}
function stRemove(i: number) {
  stScans.value.splice(i, 1)
}
async function stSave(confirmAfter: boolean) {
  errMsg.value = ''
  try {
    const r = await api.stocktakeSave({
      id: stEditingId.value,
      distributorId: stLoc.value,
      barcodes: stScans.value.map(x => x.barcode),
    })
    const id = stEditingId.value || r.id
    if (!stEditingId.value) {
      stEditingId.value = r.id
      stEditingNo.value = r.docNo
    }
    if (confirmAfter) {
      const c = await api.stocktakeConfirm(id)
      flash(`盘点已确认：${stEditingNo.value}——正常${c.normal} / 盘亏${c.loss} / 盘盈${c.gain}`)
      stShowDetail.value = id
      stReset()
    } else {
      flash(`盘点草稿已保存：${stEditingNo.value}（已扫${stScans.value.length}件）`)
    }
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
async function stConfirmDoc(d: PDoc) {
  errMsg.value = ''
  try {
    const c = await api.stocktakeConfirm(d.id)
    flash(`盘点已确认：${d.docNo}——正常${c.normal} / 盘亏${c.loss} / 盘盈${c.gain}`)
    stShowDetail.value = d.id
    if (stEditingId.value === d.id) stReset()
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
    await refreshAll()
  }
}
async function stUnconfirmDoc(d: PDoc) {
  errMsg.value = ''
  try {
    await api.stocktakeUnconfirm(d.id)
    flash(`盘点已反确认：${d.docNo}，可继续补扫后重新确认`)
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
async function stDeleteDoc(d: PDoc) {
  errMsg.value = ''
  try {
    await api.stocktakeDelete(d.id)
    flash(`盘点草稿 ${d.docNo} 已删除`)
    if (stEditingId.value === d.id) stReset()
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
function stEdit(d: PDoc) {
  stEditingId.value = d.id
  stEditingNo.value = d.docNo
  stLoc.value = d.distributorId || 0
  stScans.value = d.scans.map(x => ({ ...x }))
  window.scrollTo({ top: 0, behavior: 'smooth' })
}

// ===== 销退单（v0.17） =====
interface SRLine {
  barcode: string
  name: string
  purity: string
  weightG: number
  origDocId: number
  origDocNo: string
  soldPrice: number
  refundPrice: number | null
}
interface RDoc {
  id: number
  docNo: string
  status: string
  totalAmount: number
  payments: PayLine[]
  lines: SRLine[]
  madeAt: string
}
const srdocs = ref<RDoc[]>([])
const srEditingId = ref(0)
const srEditingNo = ref('')
const srLines = ref<SRLine[]>([])
const srInput = ref('')
const srPays = ref<PayLine[]>([])

function srReset() {
  srEditingId.value = 0
  srEditingNo.value = ''
  srLines.value = []
  srInput.value = ''
  srPays.value = []
}
async function srAdd() {
  const bc = srInput.value.trim().toUpperCase()
  if (!bc) return
  if (srLines.value.some(x => x.barcode === bc)) {
    errMsg.value = `条码 ${bc} 已在本单中`
    return
  }
  try {
    const r = await api.saleReturnLookup(bc)
    srLines.value.push({ ...r })
    srInput.value = ''
    errMsg.value = ''
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
function srRemove(i: number) {
  srLines.value.splice(i, 1)
}
const srTotal = () => srLines.value.reduce((s2, l) => s2 + (Number(l.refundPrice) || 0), 0)
const srPayTotal = () => srPays.value.reduce((s2, p) => s2 + (Number(p.amount) || 0), 0)
function srAddPay() {
  const used = new Set(srPays.value.map(p => p.method))
  const next = enabledPayMethods().find(m => !used.has(m.name))
  srPays.value.push({ method: next?.name ?? '', amount: null })
}
function srRemovePay(i: number) {
  srPays.value.splice(i, 1)
}
function srFillPay(p: PayLine) {
  const others = srPays.value.filter(x => x !== p).reduce((s2, x) => s2 + (Number(x.amount) || 0), 0)
  p.amount = Math.round((srTotal() - others) * 100) / 100
}
async function srSave(confirmAfter: boolean) {
  errMsg.value = ''
  try {
    const r = await api.saleReturnSave({
      id: srEditingId.value,
      payments: srPays.value.filter(p => p.method)
        .map(p => ({ method: p.method, amount: Number(p.amount) || 0 })),
      lines: srLines.value.map(l => ({
        barcode: l.barcode, refundPrice: Number(l.refundPrice) || 0,
      })),
    })
    const id = srEditingId.value || r.id
    if (!srEditingId.value) {
      srEditingId.value = r.id
      srEditingNo.value = r.docNo
    }
    if (confirmAfter) {
      const c = await api.saleReturnConfirm(id)
      flash(`销退已确认：${srEditingNo.value}，退款合计 ¥${c.totalAmount}`)
      srReset()
    } else {
      flash(`销退草稿已保存：${srEditingNo.value}`)
    }
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
    await refreshAll()
  }
}
async function srConfirmDoc(d: RDoc) {
  errMsg.value = ''
  try {
    const c = await api.saleReturnConfirm(d.id)
    flash(`销退已确认：${d.docNo}，退款合计 ¥${c.totalAmount}`)
    if (srEditingId.value === d.id) srReset()
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
    await refreshAll()
  }
}
async function srUnconfirmDoc(d: RDoc) {
  errMsg.value = ''
  try {
    await api.saleReturnUnconfirm(d.id)
    flash(`销退已反确认：${d.docNo}`)
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
    await refreshAll()
  }
}
async function srDeleteDoc(d: RDoc) {
  errMsg.value = ''
  try {
    await api.saleReturnDelete(d.id)
    flash(`销退草稿 ${d.docNo} 已删除，货品回到"已售"`)
    if (srEditingId.value === d.id) srReset()
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
function srEdit(d: RDoc) {
  srEditingId.value = d.id
  srEditingNo.value = d.docNo
  srLines.value = d.lines.map(l => ({ ...l }))
  srPays.value = (d.payments ?? []).map(p => ({ ...p }))
  window.scrollTo({ top: 0, behavior: 'smooth' })
}

// ===== 退库单 =====
interface ODoc {
  id: number
  docNo: string
  supplier: string
  status: string
  items: Item[]
  madeAt: string
}
const odocs = ref<ODoc[]>([])
const obEditingId = ref(0)
const obEditingNo = ref('')
const obSupplier = ref('')
const obBarcodes = ref<string[]>([])
const obInput = ref('')

function obReset() {
  obEditingId.value = 0
  obEditingNo.value = ''
  obSupplier.value = ''
  obBarcodes.value = []
  obInput.value = ''
}
// 扫码/输入条码后回车或点添加：本地先查库存给出即时反馈，保存时服务端再校验
function obAdd() {
  const bc = obInput.value.trim().toUpperCase()
  if (!bc) return
  if (obBarcodes.value.includes(bc)) {
    errMsg.value = `条码 ${bc} 已在本单中`
    return
  }
  const it = items.value.find(x => x.barcode === bc)
  if (!it) {
    errMsg.value = `条码 ${bc} 不在库存列表中（保存时以服务端校验为准）`
  } else if (it.status !== '在库') {
    errMsg.value = it.status === '销售中' || it.status === '退库中'
      ? `条码 ${bc} 当前「${it.status}」（被某张草稿占着）——先处理那张草稿`
      : `条码 ${bc} 当前状态「${it.status}」，不能退库`
    return
  } else {
    errMsg.value = ''
  }
  obBarcodes.value.push(bc)
  obInput.value = ''
}
function obItemOf(bc: string) {
  return items.value.find(x => x.barcode === bc)
}
function obRemove(i: number) {
  obBarcodes.value.splice(i, 1)
}
async function obSave(confirmAfter: boolean) {
  errMsg.value = ''
  try {
    const r = await api.outboundSave({
      id: obEditingId.value,
      supplier: obSupplier.value,
      barcodes: obBarcodes.value,
    })
    const id = obEditingId.value || r.id
    if (!obEditingId.value) {
      obEditingId.value = r.id
      obEditingNo.value = r.docNo
    }
    if (confirmAfter) {
      await api.outboundConfirm(id)
      flash(`退库已确认：${obEditingNo.value}`)
      obReset()
    } else {
      flash(`退库草稿已保存：${obEditingNo.value}`)
    }
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
    await refreshAll()
  }
}
async function obConfirmDoc(d: ODoc) {
  errMsg.value = ''
  try {
    await api.outboundConfirm(d.id)
    flash(`退库已确认：${d.docNo}`)
    if (obEditingId.value === d.id) obReset()
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
    await refreshAll()
  }
}
async function obUnconfirmDoc(d: ODoc) {
  errMsg.value = ''
  try {
    await api.outboundUnconfirm(d.id)
    flash(`退库已反确认：${d.docNo}，货品已回到在库`)
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
    await refreshAll()
  }
}
async function obDeleteDoc(d: ODoc) {
  errMsg.value = ''
  try {
    await api.outboundDelete(d.id)
    flash(`退库草稿 ${d.docNo} 已删除`)
    if (obEditingId.value === d.id) obReset()
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
function obEdit(d: ODoc) {
  obEditingId.value = d.id
  obEditingNo.value = d.docNo
  obSupplier.value = d.supplier
  obBarcodes.value = d.items.map(it => it.barcode)
}

// ===== 基础资料字典 =====
interface DictItem {
  id: number
  name: string
  sort: number
  enabled: boolean
}
const dicts = ref<Record<string, DictItem[]>>({ category: [], purity: [], jewel_type: [], pay_method: [], stone_name: [] })
const dictLabels: Record<string, string> = { category: '首饰大类', purity: '成色', jewel_type: '首饰类别', stone_name: '主石名称', pay_method: '收款方式' }
const dictTab = ref('category')
const showDicts = ref(false)
const ndName = ref('')
const ndSort = ref(0)

async function loadDicts() {
  const [c, pu, j, pm, sn] = await Promise.all([
    api.dictList('category'), api.dictList('purity'), api.dictList('jewel_type'), api.dictList('pay_method'),
    api.dictList('stone_name'),
  ])
  dicts.value = { category: c.list, purity: pu.list, jewel_type: j.list, pay_method: pm.list, stone_name: sn.list }
}
// 开单下拉只用启用项
const enabledCats = () => dicts.value.category.filter(d => d.enabled)
const enabledPurities = () => dicts.value.purity.filter(d => d.enabled)
const enabledPayMethods = () => dicts.value.pay_method.filter(d => d.enabled)

// ===== 售货员档案（v0.14，v0.20加门店/角色/抽成） =====
interface Salesperson {
  id: number
  name: string
  sort: number
  enabled: boolean
  distributorId: number
  storeName: string
  role: string
  managerRate: number
}
const salespersons = ref<Salesperson[]>([])
const showSps = ref(false)
const nspName = ref('')
const nspSort = ref(0)
const nspStore = ref(0)
const nspRole = ref('店员')
const nspRate = ref(0)

async function loadSalespersons() {
  const r = await api.salespersonList()
  salespersons.value = r.list
}
const enabledSalespersons = () => salespersons.value.filter(s => s.enabled)
// 开单只选"本账号所在位置"的售货员（总部账号选总部的，门店账号选本店的）
const saleSalespersons = () =>
  salespersons.value.filter(s => s.enabled && s.distributorId === userStoreId.value)

async function createSp() {
  errMsg.value = ''
  try {
    await api.salespersonCreate({
      name: nspName.value, sort: Number(nspSort.value) || 0,
      distributorId: nspStore.value, role: nspRole.value,
      managerRate: Number(nspRate.value) || 0,
    })
    flash(`已新增售货员：${nspName.value}`)
    nspName.value = ''
    nspRate.value = 0
    await loadSalespersons()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
async function toggleSp(s: Salesperson) {
  errMsg.value = ''
  try {
    await api.salespersonUpdate({ id: s.id, enabled: !s.enabled })
    flash(`${s.name} 已${s.enabled ? '停用' : '启用'}`)
    await loadSalespersons()
  } catch (e) {
    errMsg.value = (e as Error).message
    await loadSalespersons()
  }
}
async function saveSp(s: Salesperson) {
  errMsg.value = ''
  try {
    await api.salespersonUpdate({
      id: s.id, name: s.name, sort: Number(s.sort) || 0,
      distributorId: s.distributorId, role: s.role,
      managerRate: Number(s.managerRate) || 0,
    })
    flash(`售货员资料已保存：${s.name}`)
    await loadSalespersons()
  } catch (e) {
    errMsg.value = (e as Error).message
    await loadSalespersons() // 失败时还原回服务端数据
  }
}

// ===== 提成规则（仅管理员，v0.20；v0.22版本化） =====
interface CommRule {
  id: number
  category: string
  bizType: string
  mode: string
  calcType: string
  value: number
  validFrom: string
  validTo: string
  status: string
}
const commRules = ref<CommRule[]>([])
const showRules = ref(false)
const nrCategory = ref('')
const nrBiz = ref('正常销售')
const nrMode = ref('标签价')
const nrCalc = ref('销售额百分比')
const nrValue = ref<number | null>(null)
const nrFrom = ref(new Date().toISOString().slice(0, 10))
const calcUnit = (t: string) => t === '销售额百分比' ? '%' : t === '每克固定' ? '元/克' : '元/件'

async function toggleRules() {
  showRules.value = !showRules.value
  if (showRules.value) await loadRules()
}
async function loadRules() {
  try {
    const r = await api.commissionRuleList()
    commRules.value = r.list
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
async function createRule() {
  errMsg.value = ''
  try {
    await api.commissionRuleCreate({
      category: nrCategory.value, bizType: nrBiz.value,
      mode: nrBiz.value === '正常销售' ? nrMode.value : '',
      calcType: nrCalc.value, value: Number(nrValue.value) || 0,
      validFrom: nrFrom.value,
    })
    flash(`提成规则新版本已建立：${nrCategory.value}×${nrBiz.value}，${nrFrom.value}起生效`)
    nrValue.value = null
    await loadRules()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
// 结束一个开放版本（默认今天收尾；此后该维度无提成，直到建新版本）
async function endRule(x: CommRule) {
  errMsg.value = ''
  try {
    await api.commissionRuleUpdate({ id: x.id, validTo: new Date().toISOString().slice(0, 10) })
    flash(`规则已结束：${x.category}×${x.mode}（今日为最后生效日）`)
    await loadRules()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
// 取消还没生效的排期版本
async function cancelRule(x: CommRule) {
  errMsg.value = ''
  try {
    await api.commissionRuleUpdate({ id: x.id, cancel: true })
    flash(`排期已取消：${x.category}×${x.mode}（原定${x.validFrom}生效）`)
    await loadRules()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}

async function createDict() {
  errMsg.value = ''
  try {
    await api.dictCreate({
      dictType: dictTab.value,
      name: ndName.value,
      sort: Number(ndSort.value) || 0,
    })
    flash(`已新增${dictLabels[dictTab.value]}：${ndName.value}`)
    ndName.value = ''
    await loadDicts()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
async function toggleDict(d: DictItem) {
  errMsg.value = ''
  try {
    await api.dictUpdate({ id: d.id, enabled: !d.enabled })
    flash(`${d.name} 已${d.enabled ? '停用' : '启用'}`)
    await loadDicts()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}

// ===== 用户管理（仅管理员） =====
interface User {
  id: number
  username: string
  name: string
  status: number
  isAdmin: boolean
  store: string
  created: string
}
const users = ref<User[]>([])
const showUsers = ref(false)
const nuUsername = ref('')
const nuName = ref('')
const nuPassword = ref('')
const nuStore = ref(0) // 0=总部

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
    await api.userCreate(nuUsername.value, nuName.value, nuPassword.value, nuStore.value)
    flash(`已创建用户 ${nuUsername.value}`)
    nuUsername.value = ''
    nuName.value = ''
    nuPassword.value = ''
    nuStore.value = 0
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
const lines = ref<Line[]>([{ barcode: '', purity: '足金999.9', stoneName: '', jewelType: '', weightG: null, price: null,
  saleFeeMode: '按克', saleFee: null, costGoldPrice: null, costFeeMode: '按克', costFee: null }])
// v0.28：名称=成色+主石+类别 实时预览（真正的拼接在服务端做）
const composedName = (l: Line) => `${l.purity}${l.stoneName}${l.jewelType}`

function resetForm() {
  editingId.value = 0
  editingNo.value = ''
  category.value = '黄金'
  lines.value = [{ barcode: '', name: '', purity: '足金999.9', weightG: null, price: null }]
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
    userStore.value = r.storeName || ''
    userStoreId.value = r.storeId || 0
    isHQ.value = !r.storeId
    logged.value = true
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}

function doLogout() {
  clearToken()
  logged.value = false
  userLabel.value = ''
  isAdmin.value = false
  userStore.value = ''
  userStoreId.value = 0
  isHQ.value = true
  password.value = ''
}

// 启动时恢复登录态（v0.13）：本地仓库里有令牌就拿去问后端"我是谁"。
// 令牌过期/账号被禁用会收到401（request里顺手清掉废令牌），安静地留在登录页。
onMounted(async () => {
  if (!hasToken()) return
  try {
    const r = await api.me()
    userLabel.value = r.name
    isAdmin.value = !!r.isAdmin
    userStore.value = r.storeName || ''
    userStoreId.value = r.storeId || 0
    isHQ.value = !r.storeId
    logged.value = true
    await refreshAll()
  } catch {
    // 恢复失败不弹错——用户看到登录页自然会重新登录
  }
})

async function refreshAll() {
  const params: Record<string, string> = {}
  if (fStatus.value) params.status = fStatus.value
  if (fCategory.value) params.category = fCategory.value
  if (fKeyword.value.trim()) params.q = fKeyword.value.trim()
  if (fLoc.value !== '') params.loc = fLoc.value
  const [ri, rd, ro, rs, rr, rt, rdist, rpd, rhs] = await Promise.all([
    api.items(params), api.inboundList(), api.outboundList(), api.saleList(), api.saleReturnList(),
    api.transferList(), api.distributorList(), api.stocktakeList(), api.recycleList(),
  ])
  tdocs.value = rt.list
  distributors.value = rdist.list
  pdocs.value = rpd.list
  hdocs.value = rhs.list
  items.value = ri.list
  itemsTotal.value = ri.total
  itemsSumW.value = ri.sumWeightG
  docs.value = rd.list
  odocs.value = ro.list
  sdocs.value = rs.list
  srdocs.value = rr.list
  await loadDicts()
  await loadGoldPrices()
  await loadSalespersons()
}

// v0.26：导出该入库单的 Label Matrix 标签数据文件
async function exportLabels(d: Doc) {
  errMsg.value = ''
  try {
    await downloadFile(`/api/doc/inbound/labels?id=${d.id}`, `标签数据-${d.docNo}.xlsx`)
    flash(`标签数据已导出：${d.docNo}——在 Label Matrix 里把数据源指向该文件即可打印`)
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}

function addLine() {
  lines.value.push({ barcode: '', purity: '足金999.9', stoneName: '', jewelType: '', weightG: null, price: null,
    saleFeeMode: '按克', saleFee: null, costGoldPrice: null, costFeeMode: '按克', costFee: null })
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
        当前用户：{{ userLabel }}<b v-if="userStore">（{{ userStore }}）</b>
        <button class="mini" @click="showPwd = !showPwd">修改密码</button>
        <button v-if="isAdmin" class="mini" @click="toggleUsers">用户管理</button>
        <button v-if="isAdmin" class="mini" @click="showDicts = !showDicts">基础资料</button>
        <button v-if="isAdmin" class="mini" @click="showSps = !showSps">售货员</button>
        <button v-if="isAdmin" class="mini" @click="toggleRules">提成规则</button>
        <button v-if="isAdmin" class="mini" @click="toggleReport">提成报表</button>
        <button v-if="isAdmin" class="mini" @click="showDists = !showDists">分销商</button>
        <button class="mini" @click="doLogout">退出登录</button>
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

      <!-- 基础资料（仅管理员可见） -->
      <section v-if="isAdmin && showDicts" class="card">
        <h2>
          基础资料
          <button v-for="(label, t) in dictLabels" :key="t" class="mini"
            :style="dictTab === t ? 'background:#2f7d4f' : ''" @click="dictTab = t">{{ label }}</button>
        </h2>
        <table>
          <thead>
            <tr><th>名称</th><th>排序</th><th>状态</th><th>操作</th></tr>
          </thead>
          <tbody>
            <tr v-for="d in dicts[dictTab]" :key="d.id">
              <td>{{ d.name }}</td>
              <td>{{ d.sort }}</td>
              <td>{{ d.enabled ? '启用' : '已停用' }}</td>
              <td>
                <button :class="['mini', d.enabled ? 'danger' : '']" @click="toggleDict(d)">
                  {{ d.enabled ? '停用' : '启用' }}
                </button>
              </td>
            </tr>
          </tbody>
        </table>
        <div class="row">
          <label>名称 <input v-model="ndName" /></label>
          <label>排序 <input v-model.number="ndSort" type="number" style="width:70px" /></label>
          <button @click="createDict">新增{{ dictLabels[dictTab] }}</button>
        </div>
        <p class="hint">字典只停用不删除——历史单据和货品引用着这些名字。改名也暂不开放，避免历史数据失去解释。</p>
      </section>

      <!-- 售货员维护（仅管理员可见，v0.14；v0.20加门店/角色/抽成） -->
      <section v-if="isAdmin && showSps" class="card">
        <h2>售货员维护</h2>
        <table>
          <thead>
            <tr><th>姓名</th><th>门店</th><th>角色</th><th>店长抽成%</th><th>排序</th><th>状态</th><th>操作</th></tr>
          </thead>
          <tbody>
            <tr v-for="s in salespersons" :key="s.id">
              <td><input v-model="s.name" style="width:100px" /></td>
              <td>
                <select v-model.number="s.distributorId">
                  <option :value="0">总部</option>
                  <option v-for="d in enabledDists()" :key="d.id" :value="d.id">{{ d.name }}</option>
                </select>
              </td>
              <td>
                <select v-model="s.role">
                  <option>店员</option>
                  <option>店长</option>
                </select>
              </td>
              <td><input v-model.number="s.managerRate" type="number" step="0.5" style="width:60px"
                :disabled="s.role !== '店长'" /></td>
              <td><input v-model.number="s.sort" type="number" style="width:50px" /></td>
              <td>{{ s.enabled ? '在职' : '已停用' }}</td>
              <td>
                <button class="mini" @click="saveSp(s)">保存</button>
                <button :class="['mini', s.enabled ? 'danger' : '']" @click="toggleSp(s)">
                  {{ s.enabled ? '停用' : '启用' }}
                </button>
              </td>
            </tr>
          </tbody>
        </table>
        <div class="row">
          <label>姓名 <input v-model="nspName" style="width:100px" /></label>
          <label>门店
            <select v-model.number="nspStore">
              <option :value="0">总部</option>
              <option v-for="d in enabledDists()" :key="d.id" :value="d.id">{{ d.name }}</option>
            </select>
          </label>
          <label>角色
            <select v-model="nspRole">
              <option>店员</option>
              <option>店长</option>
            </select>
          </label>
          <label v-if="nspRole === '店长'">抽成% <input v-model.number="nspRate" type="number" step="0.5" style="width:60px" /></label>
          <label>排序 <input v-model.number="nspSort" type="number" style="width:60px" /></label>
          <button @click="createSp">新增售货员</button>
        </div>
        <p class="hint">每个门店最多一位启用的店长；店长从本店店员每笔提成中抽上面的百分比（店员到手=份额×(1-抽成%)），店长自己卖货按规则全额拿。改门店/角色只影响之后确认的单。</p>
      </section>

      <!-- 提成规则（仅管理员可见，v0.20；v0.22版本化） -->
      <section v-if="isAdmin && showRules" class="card">
        <h2>提成规则 <button class="mini" @click="loadRules">刷新</button></h2>
        <table>
          <thead>
            <tr><th>大类</th><th>业务类型</th><th>结算方式</th><th>计算方式</th><th>数值</th><th>生效从</th><th>失效至</th><th>状态</th><th>操作</th></tr>
          </thead>
          <tbody>
            <tr v-for="x in commRules" :key="x.id" :style="x.status === '已失效' ? 'color:#999' : ''">
              <td>{{ x.category }}</td>
              <td>{{ x.bizType }}</td>
              <td>{{ x.mode || '—' }}</td>
              <td>{{ x.calcType }}</td>
              <td :style="x.value < 0 ? 'color:#c0392b' : ''">{{ x.value }} {{ calcUnit(x.calcType) }}</td>
              <td class="mono">{{ x.validFrom }}</td>
              <td class="mono">{{ x.validTo || '—' }}</td>
              <td>
                <b v-if="x.status === '生效中'" style="color:#2f7d4f">生效中</b>
                <b v-else-if="x.status === '未生效'" style="color:#b8860b">未生效</b>
                <span v-else>已失效</span>
              </td>
              <td>
                <button v-if="x.status === '生效中' && !x.validTo" class="mini danger" @click="endRule(x)">今日结束</button>
                <button v-if="x.status === '未生效'" class="mini danger" @click="cancelRule(x)">取消排期</button>
              </td>
            </tr>
          </tbody>
        </table>
        <div class="row">
          <label>大类
            <select v-model="nrCategory">
              <option value="">— 选择 —</option>
              <option v-for="c in enabledCats()" :key="c.id" :value="c.name">{{ c.name }}</option>
            </select>
          </label>
          <label>业务类型
            <select v-model="nrBiz">
              <option>正常销售</option>
              <option>以旧换新</option>
              <option>旧料回收</option>
            </select>
          </label>
          <label v-if="nrBiz === '正常销售'">结算方式
            <select v-model="nrMode">
              <option>标签价</option>
              <option>变金价</option>
            </select>
          </label>
          <label>计算方式
            <select v-model="nrCalc">
              <option>销售额百分比</option>
              <option>每克固定</option>
              <option>每件固定</option>
            </select>
          </label>
          <label>数值 <input v-model.number="nrValue" type="number" step="0.1" style="width:80px"
            :placeholder="nrBiz === '以旧换新' ? '可负,如-6' : ''" /> {{ calcUnit(nrCalc) }}</label>
          <label>生效日期 <input v-model="nrFrom" type="date" /></label>
          <button @click="createRule">新增版本</button>
        </div>
        <p class="hint">调整规则=对同一"大类×结算方式"新增一个版本，旧版本自动在新版本生效前一天关闭；生效日期可以填未来（排期）。历史版本不可修改——已入账的提成是确认时刻的快照，这里留的是"当时按什么算"的痕。没有生效版本的货没有提成（不报错）。</p>
      </section>

      <!-- 提成报表（仅管理员可见，v0.21） -->
      <section v-if="isAdmin && showReport" class="card">
        <h2>提成报表</h2>
        <div class="row">
          <label>从 <input v-model="crFrom" type="date" /></label>
          <label>到 <input v-model="crTo" type="date" /></label>
          <button @click="loadReport">查询</button>
        </div>
        <table v-if="crRows.length">
          <thead>
            <tr><th>售货员</th><th>门店</th><th>角色</th><th>成交单数</th><th>销售提成</th><th>旧料/换新</th><th>店长抽成</th><th>销退冲减</th><th>净提成</th></tr>
          </thead>
          <tbody>
            <tr v-for="x in crRows" :key="x.salespersonId">
              <td>{{ x.name }}</td>
              <td>{{ x.storeName }}</td>
              <td>{{ x.role }}</td>
              <td>{{ x.docCount }}</td>
              <td>¥{{ x.saleComm.toFixed(2) }}</td>
              <td :style="x.tradeComm < 0 ? 'color:#c0392b' : ''">{{ x.tradeComm ? '¥' + x.tradeComm.toFixed(2) : '—' }}</td>
              <td>{{ x.managerComm ? '¥' + x.managerComm.toFixed(2) : '—' }}</td>
              <td :style="x.returnOffset < 0 ? 'color:#c0392b' : ''">
                {{ x.returnOffset ? '¥' + x.returnOffset.toFixed(2) : '—' }}</td>
              <td><b>¥{{ x.net.toFixed(2) }}</b></td>
            </tr>
            <tr style="border-top:2px solid #999">
              <td colspan="4"><b>合计</b></td>
              <td><b>¥{{ crTotals.sale.toFixed(2) }}</b></td>
              <td :style="crTotals.trade < 0 ? 'color:#c0392b' : ''"><b>¥{{ crTotals.trade.toFixed(2) }}</b></td>
              <td><b>¥{{ crTotals.mgr.toFixed(2) }}</b></td>
              <td :style="crTotals.ret < 0 ? 'color:#c0392b' : ''"><b>¥{{ crTotals.ret.toFixed(2) }}</b></td>
              <td><b>¥{{ crTotals.net.toFixed(2) }}</b></td>
            </tr>
          </tbody>
        </table>
        <p v-else class="hint">该期间没有提成记录。</p>
        <p class="hint">净提成 = 销售提成 + 店长抽成 + 销退冲减（冲减为负数）。按台账入账时间统计。</p>
      </section>

      <!-- 分销商维护（仅管理员可见，v0.18） -->
      <section v-if="isAdmin && showDists" class="card">
        <h2>分销商维护</h2>
        <table>
          <thead>
            <tr><th>名称</th><th>状态</th><th>操作</th></tr>
          </thead>
          <tbody>
            <tr v-for="d in distributors" :key="d.id">
              <td><input v-model="d.name" style="width:160px" /></td>
              <td>{{ d.status === 1 ? '启用' : '已停用' }}</td>
              <td>
                <button class="mini" @click="saveDist(d)">保存</button>
                <button :class="['mini', d.status === 1 ? 'danger' : '']" @click="toggleDist(d)">
                  {{ d.status === 1 ? '停用' : '启用' }}
                </button>
              </td>
            </tr>
          </tbody>
        </table>
        <div class="row">
          <label>名称 <input v-model="ndistName" /></label>
          <button @click="createDist">新增分销商</button>
        </div>
        <p class="hint">分销商（门店/下级代理）只停用不删除；单据按编号引用，改名安全。停用后不能作为调拨的调出/调入方。</p>
      </section>

      <!-- 用户管理（仅管理员可见） -->
      <section v-if="isAdmin && showUsers" class="card">
        <h2>用户管理 <button class="mini" @click="loadUsers">刷新</button></h2>
        <table>
          <thead>
            <tr><th>用户名</th><th>姓名</th><th>状态</th><th>角色</th><th>所属</th><th>创建日期</th><th>操作</th></tr>
          </thead>
          <tbody>
            <tr v-for="u in users" :key="u.id">
              <td class="mono">{{ u.username }}</td>
              <td>{{ u.name }}</td>
              <td>{{ u.status === 1 ? '启用' : '已禁用' }}</td>
              <td>{{ u.isAdmin ? '管理员' : '店员' }}</td>
              <td>{{ u.store }}</td>
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
          <label>所属
            <select v-model.number="nuStore">
              <option :value="0">总部</option>
              <option v-for="d in enabledDists()" :key="d.id" :value="d.id">{{ d.name }}</option>
            </select>
          </label>
          <button @click="createUser">新建用户</button>
        </div>
      </section>

      <!-- 今日金价（所有人可见；管理员可发布） -->
      <section class="card">
        <h2>今日金价 <button class="mini" @click="toggleGpHistory">{{ showGpHistory ? '收起历史' : '调价历史' }}</button></h2>
        <table v-if="goldPrices.length">
          <thead><tr><th>成色</th><th>零售(元/克)</th><th>换新(元/克)</th><th>回收(元/克)</th><th>发布</th></tr></thead>
          <tbody>
            <tr v-for="g in goldPrices" :key="g.purity">
              <td>{{ g.purity }}</td>
              <td><b>{{ g.retailPrice.toFixed(2) }}</b></td>
              <td>{{ (g.tradePrice ?? 0) > 0 ? g.tradePrice.toFixed(2) : '—' }}</td>
              <td>{{ g.recyclePrice > 0 ? g.recyclePrice.toFixed(2) : '—' }}</td>
              <td class="hint">{{ g.by }} · {{ g.at }}</td>
            </tr>
          </tbody>
        </table>
        <p v-else class="hint">今日尚未发布金价。</p>
        <div class="row" v-if="isAdmin">
          <label>成色
            <select v-model="gpPurity">
              <option v-for="pu in enabledPurities()" :key="pu.id" :value="pu.name">{{ pu.name }}</option>
            </select>
          </label>
          <label>零售价 <input v-model.number="gpRetail" type="number" step="0.01" style="width:100px" /></label>
          <label>回收价 <input v-model.number="gpRecycle" type="number" step="0.01" style="width:100px" placeholder="可空" /></label>
          <label>换新价 <input v-model.number="gpTrade" type="number" step="0.01" style="width:100px" placeholder="可空" /></label>
          <button @click="publishGoldPrice">发布</button>
        </div>
        <table v-if="showGpHistory">
          <thead><tr><th>时间</th><th>成色</th><th>零售</th><th>换新</th><th>回收</th><th>发布人</th></tr></thead>
          <tbody>
            <tr v-for="(g, i) in gpHistory" :key="i">
              <td class="hint">{{ g.at }}</td>
              <td>{{ g.purity }}</td>
              <td>{{ g.retailPrice.toFixed(2) }}</td>
              <td>{{ (g.tradePrice ?? 0) > 0 ? g.tradePrice.toFixed(2) : '—' }}</td>
              <td>{{ g.recyclePrice > 0 ? g.recyclePrice.toFixed(2) : '—' }}</td>
              <td class="hint">{{ g.by }}</td>
            </tr>
          </tbody>
        </table>
      </section>

      <!-- 销售开单 -->
      <section class="card">
        <h2>
          {{ slEditingId ? `编辑销售草稿 ${slEditingNo}` : '销售开单' }}
          <button v-if="slEditingId" class="mini" @click="slReset">放弃编辑，新建</button>
        </h2>
        <div class="row">
          <label>条码 <input v-model="slInput" placeholder="扫码或输入后回车" @keyup.enter="slAdd" class="mono" /></label>
          <button class="mini" @click="slAdd">添加</button>
        </div>
        <table v-if="slLines.length">
          <thead>
            <tr><th>#</th><th>条码</th><th>名称</th><th>成色</th><th>克重</th><th>标签价</th><th>结算方式</th><th>参考价</th><th>实售价(¥)</th><th></th></tr>
          </thead>
          <tbody>
            <tr v-for="(l, i) in slLines" :key="l.barcode">
              <td>{{ i + 1 }}</td>
              <td class="mono">{{ l.barcode }}</td>
              <td>{{ l.name }}</td>
              <td>{{ l.purity }}</td>
              <td>{{ l.weightG.toFixed(2) }}g</td>
              <td>{{ l.price > 0 ? '¥' + l.price.toFixed(0) : '—' }}</td>
              <td>
                <select v-model="l.mode" @change="slModeChanged(l)">
                  <option :disabled="l.price <= 0">标签价</option>
                  <option>变金价</option>
                </select>
              </td>
              <td class="hint">¥{{ suggestPrice(l).toFixed(2) }}<span v-if="l.mode === '变金价'">（{{ l.weightG.toFixed(2) }}g × {{ goldRateOf(l.purity).toFixed(2) }}）</span></td>
              <td><input v-model.number="l.soldPrice" type="number" step="0.01" style="width:110px" /></td>
              <td><button class="mini" @click="slRemove(i)">移除</button></td>
            </tr>
          </tbody>
        </table>
        <!-- 旧料区（v0.24 以旧换新/旧料回收）：金换金银换银，额度=本单同大类新品克重 -->
        <template v-if="slLines.length">
          <div class="row" v-for="(o, i) in slOld" :key="i">
            <label>旧料大类
              <select v-model="o.category">
                <option v-for="c in enabledCats()" :key="c.id" :value="c.name">{{ c.name }}</option>
              </select>
            </label>
            <label>成色
              <select v-model="o.purity">
                <option v-for="pu in enabledPurities()" :key="pu.id" :value="pu.name">{{ pu.name }}</option>
              </select>
            </label>
            <label>克重 <input v-model.number="o.weightG" type="number" step="0.01" style="width:90px" /></label>
            <span class="hint" v-if="(Number(o.weightG) || 0) > 0">
              换新 {{ slOldSplit()[i].tradeG.toFixed(2) }}g×{{ tradeRateOf(o.purity).toFixed(0) }}
              + 回收 {{ slOldSplit()[i].recycleG.toFixed(2) }}g×{{ recycleRateOf(o.purity).toFixed(0) }}
              = 抵 ¥{{ slOldSplit()[i].credit.toFixed(2) }}
            </span>
            <button class="mini danger" @click="slRemoveOld(i)">移除</button>
          </div>
          <div class="row">
            <button class="mini" @click="slAddOld">+ 顾客带旧料（以旧换新/回收）</button>
          </div>
        </template>

        <!-- 售货员（1~3人，点名字选中，整单平分）+ 组合收款 -->
        <div class="row" v-if="slLines.length">
          <span>售货员(可多选，整单平分)：</span>
          <button v-for="s in saleSalespersons()" :key="s.id" class="mini"
            :style="slSalespersonIds.includes(s.id) ? 'background:#2f7d4f' : ''"
            @click="slToggleSp(s.id)">
            {{ s.name }}{{ s.role === '店长' ? '(店长)' : '' }}
          </button>
          <span class="hint" v-if="!saleSalespersons().length">（先在"售货员"里给{{ userStore || '总部' }}添加人员）</span>
        </div>
        <template v-if="slLines.length">
          <div class="row" v-for="(p, i) in slPays" :key="i">
            <label>收款方式
              <select v-model="p.method">
                <option v-for="m in enabledPayMethods()" :key="m.id" :value="m.name">{{ m.name }}</option>
              </select>
            </label>
            <label>金额 <input v-model.number="p.amount" type="number" step="0.01" style="width:110px" /></label>
            <button class="mini" @click="slFillPay(p)">补足</button>
            <button class="mini danger" @click="slRemovePay(i)">移除</button>
          </div>
          <div class="row">
            <button class="mini" @click="slAddPay">+ 添加收款方式</button>
            <span class="hint" v-if="slPays.length">
              {{ slNet() >= 0 ? '已收' : '已退' }} ¥{{ slPayTotal().toFixed(2) }} /
              {{ slNet() >= 0 ? '净应收' : '应退顾客' }} ¥{{ Math.abs(slNet()).toFixed(2) }}
              <b v-if="Math.abs(slPayTotal() - Math.abs(slNet())) > 0.005" style="color:#c0392b">（差 ¥{{ (Math.abs(slNet()) - slPayTotal()).toFixed(2) }}）</b>
              <b v-else style="color:#2f7d4f">✓ 两讫</b>
            </span>
          </div>
        </template>
        <div class="row" v-if="slLines.length">
          <p class="ok" style="margin:0">
            货款 ¥{{ slTotal().toFixed(2) }}
            <template v-if="slCredit() > 0"> − 旧料抵扣 ¥{{ slCredit().toFixed(2) }} =
              <b :style="slNet() < 0 ? 'color:#c0392b' : ''">{{ slNet() >= 0 ? '净应收' : '应退顾客' }} ¥{{ Math.abs(slNet()).toFixed(2) }}</b>
            </template>
          </p>
          <span class="spacer"></span>
          <button class="gray" @click="slSave(false)">保存草稿(挂单)</button>
          <button @click="slSave(true)">确认收款</button>
        </div>
        <p class="hint">标签价=按货品售价；变金价=克重×该成色今日零售金价。实售价可在参考价上议价修改。</p>
      </section>

      <!-- 销售单列表 -->
      <section class="card" v-if="sdocs.length">
        <h2>销售单列表（{{ sdocs.length }} 张）</h2>
        <div v-for="d in sdocs" :key="d.id" class="doc">
          <div class="dochead">
            <span class="mono">{{ d.docNo }}</span>
            <span :class="['badge', d.status === '草稿' ? 'draft' : 'ok2']">{{ d.status }}</span>
            <span class="ok" v-if="d.totalAmount !== 0">{{ d.totalAmount < 0 ? '退 ' : '' }}¥{{ Math.abs(d.totalAmount).toFixed(2) }}</span>
            <span class="hint" v-if="d.oldLines?.length">
              旧料 {{ d.oldLines.reduce((s2, o) => s2 + (o.weightG || 0), 0).toFixed(2) }}g
              {{ d.oldLines.some(o => o.credit) ? '抵 ¥' + d.oldLines.reduce((s2, o) => s2 + (o.credit || 0), 0).toFixed(2) : '' }}
            </span>
            <span v-if="d.salespersonName">售货员：{{ d.salespersonName }}</span>
            <span class="hint" v-if="d.payments?.length">
              {{ d.payments.map(p => `${p.method}¥${Number(p.amount ?? 0).toFixed(2)}`).join(' + ') }}
            </span>
            <span class="hint">{{ (d.lines?.length ?? 0) }} 件 · {{ d.madeAt }}</span>
            <span class="spacer"></span>
            <template v-if="d.status === '草稿'">
              <button class="mini" @click="slEdit(d)">取单</button>
              <button class="mini" @click="slConfirmDoc(d)">确认收款</button>
              <button class="mini danger" @click="slDeleteDoc(d)">删除</button>
            </template>
            <template v-else>
              <button class="mini danger" @click="slUnconfirmDoc(d)">反确认(限当日)</button>
            </template>
          </div>
          <table v-if="d.lines?.length">
            <tbody>
              <tr v-for="(l, j) in d.lines" :key="j">
                <td class="mono" style="width:150px">{{ l.barcode }}</td>
                <td>{{ l.name }}</td>
                <td style="width:90px">{{ l.mode }}</td>
                <td style="width:110px">¥{{ Number(l.soldPrice ?? 0).toFixed(2) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      <!-- 调拨单（v0.18） -->
      <section v-if="isHQ" class="card">
        <h2>
          {{ tfEditingId ? `编辑调拨草稿 ${tfEditingNo}` : '调拨单（分货/退总库/互调）' }}
          <button v-if="tfEditingId" class="mini" @click="tfReset">放弃，新建</button>
        </h2>
        <div class="row">
          <label>调出方
            <select v-model.number="tfFrom">
              <option :value="0">总库</option>
              <option v-for="d in enabledDists()" :key="d.id" :value="d.id">{{ d.name }}</option>
            </select>
          </label>
          <span>→</span>
          <label>调入方
            <select v-model.number="tfTo">
              <option :value="0">总库</option>
              <option v-for="d in enabledDists()" :key="d.id" :value="d.id">{{ d.name }}</option>
            </select>
          </label>
          <label>条码 <input v-model="tfInput" placeholder="扫码或输入后回车" @keyup.enter="tfAdd" class="mono" /></label>
          <button class="mini" @click="tfAdd">添加</button>
        </div>
        <table v-if="tfBarcodes.length">
          <thead>
            <tr><th>#</th><th>条码</th><th>名称</th><th></th></tr>
          </thead>
          <tbody>
            <tr v-for="(x, i) in tfBarcodes" :key="x.barcode">
              <td>{{ i + 1 }}</td>
              <td class="mono">{{ x.barcode }}</td>
              <td>{{ x.name || '—' }}</td>
              <td><button class="mini" @click="tfRemove(i)">删行</button></td>
            </tr>
          </tbody>
        </table>
        <div class="row" v-if="tfBarcodes.length">
          <p class="ok" style="margin:0">共 {{ tfBarcodes.length }} 件：{{ tfLocName(tfFrom) }} → {{ tfLocName(tfTo) }}</p>
          <span class="spacer"></span>
          <button class="gray" @click="tfSave(false)">保存草稿</button>
          <button @click="tfSave(true)">确认调拨</button>
        </div>
        <p class="hint">同一张单覆盖三种用法：总库→分销商＝分货；分销商→总库＝退回；分销商→分销商＝互调。草稿即占用（调拨中）。</p>
      </section>

      <!-- 调拨单列表 -->
      <section class="card" v-if="isHQ && tdocs.length">
        <h2>调拨单列表（{{ tdocs.length }} 张）</h2>
        <div v-for="d in tdocs" :key="d.id" class="doc">
          <div class="dochead">
            <span class="mono">{{ d.docNo }}</span>
            <span :class="['badge', d.status === '草稿' ? 'draft' : 'ok2']">{{ d.status }}</span>
            <span>{{ d.fromName }} → {{ d.toName }}</span>
            <span class="hint">{{ (d.lines?.length ?? 0) }} 件 · {{ d.madeAt }}</span>
            <span class="spacer"></span>
            <template v-if="d.status === '草稿'">
              <button class="mini" @click="tfEdit(d)">编辑</button>
              <button class="mini" @click="tfConfirmDoc(d)">确认调拨</button>
              <button class="mini danger" @click="tfDeleteDoc(d)">删除</button>
            </template>
            <template v-else>
              <button class="mini danger" @click="tfUnconfirmDoc(d)">反确认</button>
            </template>
          </div>
          <table v-if="d.lines?.length">
            <tbody>
              <tr v-for="(l, j) in d.lines" :key="j">
                <td class="mono" style="width:160px">{{ l.barcode }}</td>
                <td>{{ l.name }}</td>
                <td style="width:110px">{{ l.purity }}</td>
                <td style="width:90px">{{ (l.weightG ?? 0).toFixed(2) }}g</td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      <!-- 纯旧料回收单（v0.25） -->
      <section class="card">
        <h2>
          {{ hsEditingId ? `编辑回收草稿 ${hsEditingNo}` : '旧料回收（顾客卖料给店里）' }}
          <button v-if="hsEditingId" class="mini" @click="hsReset">放弃，新建</button>
        </h2>
        <div class="row" v-for="(o, i) in hsLines" :key="i">
          <label>大类
            <select v-model="o.category">
              <option v-for="c in enabledCats()" :key="c.id" :value="c.name">{{ c.name }}</option>
            </select>
          </label>
          <label>成色
            <select v-model="o.purity">
              <option v-for="pu in enabledPurities()" :key="pu.id" :value="pu.name">{{ pu.name }}</option>
            </select>
          </label>
          <label>克重 <input v-model.number="o.weightG" type="number" step="0.01" style="width:90px" /></label>
          <span class="hint" v-if="(Number(o.weightG) || 0) > 0">
            × 回收价 {{ recycleRateOf(o.purity).toFixed(2) }} = ¥{{ ((Number(o.weightG) || 0) * recycleRateOf(o.purity)).toFixed(2) }}
          </span>
          <button class="mini danger" @click="hsRemoveLine(i)">移除</button>
        </div>
        <div class="row">
          <button class="mini" @click="hsAddLine">+ 添加旧料</button>
        </div>
        <template v-if="hsLines.length">
          <div class="row">
            <span>经手售货员(可多选)：</span>
            <button v-for="s in saleSalespersons()" :key="s.id" class="mini"
              :style="hsSalespersonIds.includes(s.id) ? 'background:#2f7d4f' : ''"
              @click="hsToggleSp(s.id)">
              {{ s.name }}{{ s.role === '店长' ? '(店长)' : '' }}
            </button>
          </div>
          <div class="row" v-for="(p, i) in hsPays" :key="i">
            <label>付款方式
              <select v-model="p.method">
                <option v-for="m in enabledPayMethods()" :key="m.id" :value="m.name">{{ m.name }}</option>
              </select>
            </label>
            <label>金额 <input v-model.number="p.amount" type="number" step="0.01" style="width:110px" /></label>
            <button class="mini" @click="hsFillPay(p)">补足</button>
            <button class="mini danger" @click="hsRemovePay(i)">移除</button>
          </div>
          <div class="row">
            <button class="mini" @click="hsAddPay">+ 添加付款方式</button>
            <span class="hint" v-if="hsPays.length">
              已付 ¥{{ hsPayTotal().toFixed(2) }} / 应付顾客 ¥{{ hsTotal().toFixed(2) }}
              <b v-if="Math.abs(hsPayTotal() - hsTotal()) > 0.005" style="color:#c0392b">（差 ¥{{ (hsTotal() - hsPayTotal()).toFixed(2) }}）</b>
              <b v-else style="color:#2f7d4f">✓ 两讫</b>
            </span>
          </div>
          <div class="row">
            <p class="ok" style="margin:0">应付顾客：¥{{ hsTotal().toFixed(2) }}</p>
            <span class="spacer"></span>
            <button class="gray" @click="hsSave(false)">保存草稿</button>
            <button @click="hsSave(true)">确认付款</button>
          </div>
        </template>
        <p class="hint">全部按当日回收价折算（确认时快照）；回收提成按"旧料回收"规则入账；反确认限当日。旧料暂不入库存（旧料库待做）。</p>
      </section>

      <!-- 回收单列表 -->
      <section class="card" v-if="hdocs.length">
        <h2>回收单列表（{{ hdocs.length }} 张）</h2>
        <div v-for="d in hdocs" :key="d.id" class="doc">
          <div class="dochead">
            <span class="mono">{{ d.docNo }}</span>
            <span :class="['badge', d.status === '草稿' ? 'draft' : 'ok2']">{{ d.status }}</span>
            <span class="ok" v-if="d.payout > 0">付 ¥{{ d.payout.toFixed(2) }}</span>
            <span v-if="d.salespersonName">经手：{{ d.salespersonName }}</span>
            <span class="hint">{{ (d.lines?.length ?? 0) }} 行 · {{ d.madeAt }}</span>
            <span class="spacer"></span>
            <template v-if="d.status === '草稿'">
              <button class="mini" @click="hsEdit(d)">取单</button>
              <button class="mini" @click="hsConfirmDoc(d)">确认付款</button>
              <button class="mini danger" @click="hsDeleteDoc(d)">删除</button>
            </template>
            <template v-else>
              <button class="mini danger" @click="hsUnconfirmDoc(d)">反确认(限当日)</button>
            </template>
          </div>
          <table v-if="d.lines?.length">
            <tbody>
              <tr v-for="(o, j) in d.lines" :key="j">
                <td style="width:120px">{{ o.category }}</td>
                <td style="width:140px">{{ o.purity }}</td>
                <td style="width:100px">{{ (o.weightG ?? 0).toFixed(2) }}g</td>
                <td>{{ o.recyclePrice ? `×${o.recyclePrice.toFixed(2)} = ¥${(o.credit ?? 0).toFixed(2)}` : '—' }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      <!-- 销退单（v0.17） -->
      <section class="card">
        <h2>
          {{ srEditingId ? `编辑销退草稿 ${srEditingNo}` : '销退单（退货）' }}
          <button v-if="srEditingId" class="mini" @click="srReset">放弃，新建</button>
        </h2>
        <div class="row">
          <label>条码 <input v-model="srInput" placeholder="扫已售件的条码后回车" @keyup.enter="srAdd" class="mono" /></label>
          <button class="mini" @click="srAdd">添加</button>
        </div>
        <table v-if="srLines.length">
          <thead>
            <tr><th>条码</th><th>名称</th><th>原销售单</th><th>原成交价</th><th>退款金额(可下调)</th><th></th></tr>
          </thead>
          <tbody>
            <tr v-for="(l, i) in srLines" :key="l.barcode">
              <td class="mono">{{ l.barcode }}</td>
              <td>{{ l.name }}</td>
              <td class="mono">{{ l.origDocNo }}</td>
              <td>¥{{ (l.soldPrice ?? 0).toFixed(2) }}</td>
              <td><input v-model.number="l.refundPrice" type="number" step="0.01" style="width:110px" /></td>
              <td><button class="mini" @click="srRemove(i)">删行</button></td>
            </tr>
          </tbody>
        </table>
        <template v-if="srLines.length">
          <div class="row" v-for="(p, i) in srPays" :key="i">
            <label>退款方式
              <select v-model="p.method">
                <option v-for="m in enabledPayMethods()" :key="m.id" :value="m.name">{{ m.name }}</option>
              </select>
            </label>
            <label>金额 <input v-model.number="p.amount" type="number" step="0.01" style="width:110px" /></label>
            <button class="mini" @click="srFillPay(p)">补足</button>
            <button class="mini danger" @click="srRemovePay(i)">移除</button>
          </div>
          <div class="row">
            <button class="mini" @click="srAddPay">+ 添加退款方式</button>
            <span class="hint" v-if="srPays.length">
              已退 ¥{{ srPayTotal().toFixed(2) }} / 应退 ¥{{ srTotal().toFixed(2) }}
              <b v-if="Math.abs(srPayTotal() - srTotal()) > 0.005" style="color:#c0392b">（差 ¥{{ (srTotal() - srPayTotal()).toFixed(2) }}）</b>
              <b v-else style="color:#2f7d4f">✓ 两讫</b>
            </span>
          </div>
          <div class="row">
            <p class="ok" style="margin:0">应退合计：¥{{ srTotal().toFixed(2) }}</p>
            <span class="spacer"></span>
            <button class="gray" @click="srSave(false)">保存草稿</button>
            <button @click="srSave(true)">确认退款</button>
          </div>
        </template>
        <p class="hint">只收"已售"的件；退款不能超过原成交价；反确认限当日。</p>
      </section>

      <!-- 销退单列表 -->
      <section class="card" v-if="srdocs.length">
        <h2>销退单列表（{{ srdocs.length }} 张）</h2>
        <div v-for="d in srdocs" :key="d.id" class="doc">
          <div class="dochead">
            <span class="mono">{{ d.docNo }}</span>
            <span :class="['badge', d.status === '草稿' ? 'draft' : 'ok2']">{{ d.status }}</span>
            <span class="ok" v-if="d.totalAmount > 0">退 ¥{{ d.totalAmount.toFixed(2) }}</span>
            <span class="hint" v-if="d.payments?.length">
              {{ d.payments.map(p => `${p.method}¥${Number(p.amount ?? 0).toFixed(2)}`).join(' + ') }}
            </span>
            <span class="hint">{{ (d.lines?.length ?? 0) }} 件 · {{ d.madeAt }}</span>
            <span class="spacer"></span>
            <template v-if="d.status === '草稿'">
              <button class="mini" @click="srEdit(d)">取单</button>
              <button class="mini" @click="srConfirmDoc(d)">确认退款</button>
              <button class="mini danger" @click="srDeleteDoc(d)">删除</button>
            </template>
            <template v-else>
              <button class="mini danger" @click="srUnconfirmDoc(d)">反确认(限当日)</button>
            </template>
          </div>
          <table v-if="d.lines?.length">
            <tbody>
              <tr v-for="(l, j) in d.lines" :key="j">
                <td class="mono" style="width:150px">{{ l.barcode }}</td>
                <td>{{ l.name }}</td>
                <td class="mono" style="width:150px">原单 {{ l.origDocNo }}</td>
                <td style="width:110px">退 ¥{{ Number(l.refundPrice ?? 0).toFixed(2) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      <!-- 开单表单 -->
      <section v-if="isHQ" class="card">
        <h2>
          {{ editingId ? `编辑草稿 ${editingNo}` : '新建入库单' }}
          <button v-if="editingId" class="mini" @click="resetForm">放弃编辑，新建</button>
        </h2>
        <div class="row">
          <label>首饰大类
            <select v-model="category">
              <option v-for="c in enabledCats()" :key="c.id" :value="c.name">{{ c.name }}</option>
            </select>
          </label>
        </div>
        <table>
          <thead>
            <tr><th>#</th><th>条码号(留空自动生成)</th><th>成色</th><th>主石名称</th><th>首饰类别</th><th>名称(自动)</th><th>总件重(g)</th><th>售价(¥)</th><th>销售工费</th><th>进货金价</th><th>进货工费</th><th></th></tr>
          </thead>
          <tbody>
            <tr v-for="(l, i) in lines" :key="i">
              <td>{{ i + 1 }}</td>
              <td><input v-model="l.barcode" placeholder="自动生成" style="width:110px" /></td>
              <td><input v-model="l.purity" list="dlPurity" style="width:92px" /></td>
              <td><input v-model="l.stoneName" list="dlStone" placeholder="素金留空" style="width:84px" /></td>
              <td><input v-model="l.jewelType" list="dlJewel" style="width:76px" /></td>
              <td class="hint">{{ composedName(l) || '—' }}</td>
              <td><input v-model.number="l.weightG" type="number" step="0.01" style="width:80px" /></td>
              <td><input v-model.number="l.price" type="number" step="1" placeholder="0" style="width:80px" /></td>
              <td>
                <select v-model="l.saleFeeMode" style="width:64px"><option>按克</option><option>按件</option></select>
                <input v-model.number="l.saleFee" type="number" step="0.5" placeholder="0" style="width:64px" />
              </td>
              <td><input v-model.number="l.costGoldPrice" type="number" step="0.01" placeholder="0" style="width:80px" /></td>
              <td>
                <select v-model="l.costFeeMode" style="width:64px"><option>按克</option><option>按件</option></select>
                <input v-model.number="l.costFee" type="number" step="0.5" placeholder="0" style="width:64px" />
              </td>
              <td><button class="mini" @click="removeLine(i)" :disabled="lines.length === 1">删行</button></td>
            </tr>
          </tbody>
        </table>
        <datalist id="dlPurity">
          <option v-for="pu in enabledPurities()" :key="pu.id" :value="pu.name" />
        </datalist>
        <datalist id="dlStone">
          <option v-for="s in dicts.stone_name.filter(x => x.enabled)" :key="s.id" :value="s.name" />
        </datalist>
        <datalist id="dlJewel">
          <option v-for="jt in dicts.jewel_type.filter(x => x.enabled)" :key="jt.id" :value="jt.name" />
        </datalist>
        <div class="row">
          <button class="mini" @click="addLine">+ 增加行</button>
          <button class="gray" @click="saveDraft">保存草稿</button>
          <button @click="saveAndConfirm">保存并确认</button>
        </div>
        <p class="hint">名称=成色+主石名称+首饰类别 自动拼接；三个字段可下拉选也可直接填，确认时新值自动补进字典。按克货售价可填0，销售按 克重×金价+销售工费；进货金价/工费是成本（仅管理员可见）。</p>
      </section>

      <!-- 单据列表 -->
      <section v-if="isHQ" class="card">
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
              <button class="mini" @click="exportLabels(d)">导出标签</button>
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

      <!-- 退库单 -->
      <section v-if="isHQ" class="card">
        <h2>
          {{ obEditingId ? `编辑退库草稿 ${obEditingNo}` : '新建退库单' }}
          <button v-if="obEditingId" class="mini" @click="obReset">放弃编辑，新建</button>
        </h2>
        <div class="row">
          <label>退往供应商 <input v-model="obSupplier" placeholder="选填" /></label>
          <label>条码 <input v-model="obInput" placeholder="扫码或输入后回车" @keyup.enter="obAdd" class="mono" /></label>
          <button class="mini" @click="obAdd">添加</button>
        </div>
        <table v-if="obBarcodes.length">
          <thead><tr><th>#</th><th>条码</th><th>名称</th><th>成色</th><th>重量(g)</th><th></th></tr></thead>
          <tbody>
            <tr v-for="(bc, i) in obBarcodes" :key="bc">
              <td>{{ i + 1 }}</td>
              <td class="mono">{{ bc }}</td>
              <td>{{ obItemOf(bc)?.name || '?' }}</td>
              <td>{{ obItemOf(bc)?.purity || '?' }}</td>
              <td>{{ (obItemOf(bc)?.weightG ?? 0).toFixed(2) }}</td>
              <td><button class="mini" @click="obRemove(i)">移除</button></td>
            </tr>
          </tbody>
        </table>
        <div class="row" v-if="obBarcodes.length">
          <button class="gray" @click="obSave(false)">保存草稿</button>
          <button @click="obSave(true)">保存并确认退库</button>
        </div>
        <p class="hint">确认后货品状态变为"已退库"；退过库的货品会阻止其入库单反确认（下游校验）。</p>
      </section>

      <!-- 退库单列表 -->
      <section class="card" v-if="isHQ && odocs.length">
        <h2>退库单列表（{{ odocs.length }} 张）</h2>
        <div v-for="d in odocs" :key="d.id" class="doc">
          <div class="dochead">
            <span class="mono">{{ d.docNo }}</span>
            <span :class="['badge', d.status === '草稿' ? 'draft' : 'ok2']">{{ d.status }}</span>
            <span>{{ d.supplier || '—' }}</span>
            <span class="hint">{{ (d.items?.length ?? 0) }} 件 · {{ d.madeAt }}</span>
            <span class="spacer"></span>
            <template v-if="d.status === '草稿'">
              <button class="mini" @click="obEdit(d)">编辑</button>
              <button class="mini" @click="obConfirmDoc(d)">确认</button>
              <button class="mini danger" @click="obDeleteDoc(d)">删除</button>
            </template>
            <template v-else>
              <button class="mini danger" @click="obUnconfirmDoc(d)">反确认</button>
            </template>
          </div>
          <table v-if="d.items?.length">
            <tbody>
              <tr v-for="(it, j) in d.items" :key="j">
                <td class="mono" style="width:160px">{{ it.barcode }}</td>
                <td>{{ it.name }}</td>
                <td style="width:110px">{{ it.purity }}</td>
                <td style="width:90px">{{ (it.weightG ?? 0).toFixed(2) }}g</td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      <!-- 盘点单（v0.23） -->
      <section class="card">
        <h2>
          {{ stEditingId ? `编辑盘点草稿 ${stEditingNo}` : '盘点' }}
          <button v-if="stEditingId" class="mini" @click="stReset">放弃，新建</button>
        </h2>
        <div class="row">
          <label v-if="isHQ">盘点位置
            <select v-model.number="stLoc">
              <option :value="0">总库</option>
              <option v-for="d in enabledDists()" :key="d.id" :value="d.id">{{ d.name }}</option>
            </select>
          </label>
          <span v-else>盘点位置：{{ userStore }}</span>
          <label>条码 <input v-model="stInput" placeholder="逐件扫码后回车" @keyup.enter="stAdd" class="mono" /></label>
          <button class="mini" @click="stAdd">添加</button>
          <span class="ok" v-if="stScans.length">已扫 {{ stScans.length }} 件</span>
        </div>
        <table v-if="stScans.length">
          <tbody>
            <tr v-for="(x, i) in stScans" :key="x.barcode">
              <td style="width:40px">{{ i + 1 }}</td>
              <td class="mono" style="width:160px">{{ x.barcode }}</td>
              <td>{{ x.name || '—' }}</td>
              <td style="width:70px"><button class="mini" @click="stRemove(i)">删行</button></td>
            </tr>
          </tbody>
        </table>
        <div class="row" v-if="stScans.length">
          <span class="spacer"></span>
          <button class="gray" @click="stSave(false)">保存草稿(明天接着盘)</button>
          <button @click="stSave(true)">完成盘点(对账)</button>
        </div>
        <p class="hint">只收系统存在过的条码；重复扫自动去重。确认那一刻对账：账面应在没扫到=盘亏；扫到了但账面不在此位置=盘盈（自动带出关联单据）。盘点不改库存——处理差异走各自的业务单据。</p>
      </section>

      <!-- 盘点单列表 -->
      <section class="card" v-if="pdocs.length">
        <h2>盘点单列表（{{ pdocs.length }} 张）</h2>
        <div v-for="d in pdocs" :key="d.id" class="doc">
          <div class="dochead">
            <span class="mono">{{ d.docNo }}</span>
            <span :class="['badge', d.status === '草稿' ? 'draft' : 'ok2']">{{ d.status }}</span>
            <span>{{ d.locName }}</span>
            <template v-if="d.result">
              <span class="ok">正常 {{ d.result.normal }}</span>
              <span :style="d.result.loss ? 'color:#c0392b;font-weight:bold' : ''">盘亏 {{ d.result.loss }}</span>
              <span :style="d.result.gain ? 'color:#b8860b;font-weight:bold' : ''">盘盈 {{ d.result.gain }}</span>
            </template>
            <span class="hint" v-else>已扫 {{ d.scans?.length ?? 0 }} 件</span>
            <span class="hint">{{ d.madeAt }}</span>
            <span class="spacer"></span>
            <template v-if="d.status === '草稿'">
              <button class="mini" @click="stEdit(d)">继续盘</button>
              <button class="mini" @click="stConfirmDoc(d)">完成盘点</button>
              <button class="mini danger" @click="stDeleteDoc(d)">删除</button>
            </template>
            <template v-else>
              <button class="mini" @click="stShowDetail = stShowDetail === d.id ? 0 : d.id">
                {{ stShowDetail === d.id ? '收起' : '差异详情' }}
              </button>
              <button class="mini danger" @click="stUnconfirmDoc(d)">反确认(重盘)</button>
            </template>
          </div>
          <template v-if="d.result && stShowDetail === d.id">
            <table v-if="d.result.lossList?.length">
              <thead><tr><th colspan="3" style="color:#c0392b">盘亏（账面应在、实物没扫到）</th></tr></thead>
              <tbody>
                <tr v-for="x in d.result.lossList" :key="x.barcode">
                  <td class="mono" style="width:160px">{{ x.barcode }}</td>
                  <td>{{ x.name }}</td>
                  <td style="width:110px">账面「{{ x.status }}」</td>
                </tr>
              </tbody>
            </table>
            <table v-if="d.result.gainList?.length">
              <thead><tr><th colspan="4" style="color:#b8860b">盘盈（扫到了、账面不在此位置）</th></tr></thead>
              <tbody>
                <tr v-for="x in d.result.gainList" :key="x.barcode">
                  <td class="mono" style="width:160px">{{ x.barcode }}</td>
                  <td>{{ x.name }}</td>
                  <td style="width:150px">账面「{{ x.status }}」@{{ x.location }}</td>
                  <td class="mono" style="width:150px">{{ x.refDocNo ? '关联 ' + x.refDocNo : '—' }}</td>
                </tr>
              </tbody>
            </table>
            <p class="hint" v-if="!d.result.lossList?.length && !d.result.gainList?.length">账实完全一致，没有差异。</p>
          </template>
        </div>
      </section>

      <!-- 库存 -->
      <section class="card">
        <h2>库存查询</h2>
        <div class="row">
          <label v-if="isHQ">位置
            <select v-model="fLoc" @change="refreshAll">
              <option value="">全部</option>
              <option value="0">总库</option>
              <option v-for="d in distributors" :key="d.id" :value="String(d.id)">{{ d.name }}</option>
            </select>
          </label>
          <label>状态
            <select v-model="fStatus" @change="refreshAll">
              <option value="">全部</option>
              <option>在库</option>
              <option>销售中</option>
              <option>退库中</option>
              <option>退货中</option>
              <option>调拨中</option>
              <option>已售</option>
              <option>已退库</option>
            </select>
          </label>
          <label>大类
            <select v-model="fCategory" @change="refreshAll">
              <option value="">全部</option>
              <option v-for="c in dicts.category" :key="c.id" :value="c.name">{{ c.name }}</option>
            </select>
          </label>
          <label>搜索 <input v-model="fKeyword" placeholder="条码前缀或名称" @keyup.enter="refreshAll" /></label>
          <button class="mini" @click="refreshAll">查询</button>
          <button class="mini" @click="fStatus=''; fCategory=''; fKeyword=''; refreshAll()">清空</button>
        </div>
        <p class="ok">共 {{ itemsTotal }} 件 · 合计克重 {{ itemsSumW.toFixed(2) }} g</p>
        <table>
          <thead>
            <tr><th>条码号</th><th>首饰名称</th><th>大类</th><th>成色</th><th>总件重(g)</th><th>售价(¥)</th><th>销售工费</th><th>位置</th><th>状态</th></tr>
          </thead>
          <tbody>
            <tr v-for="it in items" :key="it.id">
              <td class="mono">{{ it.barcode }}</td>
              <td>{{ it.name }}</td>
              <td>{{ it.category }}</td>
              <td>{{ it.purity }}</td>
              <td>{{ (it.weightG ?? 0).toFixed(2) }}</td>
              <td>{{ (it.price ?? 0) > 0 ? (it.price ?? 0).toFixed(0) : '—' }}</td>
              <td>{{ (it.saleFee ?? 0) > 0 ? `${it.saleFee}/${it.saleFeeMode === '按件' ? '件' : '克'}` : '—' }}</td>
              <td>{{ it.location ?? '总库' }}</td>
              <td>{{ it.status }}</td>
            </tr>
          </tbody>
        </table>
        <p class="hint" v-if="itemsTotal > items.length">仅显示最新 {{ items.length }} 条，共 {{ itemsTotal }} 条——用筛选缩小范围。</p>
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
