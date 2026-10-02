<script setup lang="ts">
// 入库单界面 v0.3：支持单据生命周期——保存草稿 → 确认 → 反确认 / 删除草稿
// 草稿可反复编辑；确认后生成货品件进入库存；反确认撤回（条码保留）。
import { computed, onMounted, ref } from 'vue'
import { api, setToken, hasToken, clearToken, downloadFile } from './api'
import Icon from './components/Icon.vue'
import DocWorkbench from './components/DocWorkbench.vue'
import InboundDoc from './components/InboundDoc.vue'
import OutboundDoc from './components/OutboundDoc.vue'
import TransferDoc from './components/TransferDoc.vue'
import SaleDoc from './components/SaleDoc.vue'
import SaleReturnDoc from './components/SaleReturnDoc.vue'
import RecycleDoc from './components/RecycleDoc.vue'
import StocktakeDoc from './components/StocktakeDoc.vue'

// ===== UI壳（v0.30）：16:9等比画布 + 侧边栏分页 =====
// ===== 页签（v0.31，仿JMP多页同开）=====
// 模块页签按 kind 单例（再点导航=聚焦）；入库单据页签按 docId 单例；新建单每点一次开一张。
interface Tab { id: string; kind: string; title: string; icon: string; docId?: number }
const tabs = ref<Tab[]>([])
const activeTabId = ref('')
let tabSeq = 0
const page = computed(() => tabs.value.find(t => t.id === activeTabId.value)?.kind ?? '')
// 导航高亮：单据页签归属它的模块
const docKindModule: Record<string, string> = {
  inboundDoc: 'inbound', outboundDoc: 'outbound', transferDoc: 'transfer',
  saleDoc: 'sale', saleReturnDoc: 'saleReturn', recycleDoc: 'recycle', stocktakeDoc: 'stocktake',
}
const navKey = computed(() => docKindModule[page.value] ?? page.value)

function openTab(kind: string, title: string, icon: string, opts?: { docId?: number; multi?: boolean }): Tab {
  if (!opts?.multi) {
    const exist = tabs.value.find(t => t.kind === kind
      && (opts?.docId === undefined ? t.docId === undefined : t.docId === opts.docId))
    if (exist) { activeTabId.value = exist.id; return exist }
  }
  const t: Tab = { id: 'tab' + ++tabSeq, kind, title, icon, docId: opts?.docId }
  tabs.value.push(t)
  activeTabId.value = t.id
  return t
}
function closeTab(tid: string) {
  const i = tabs.value.findIndex(t => t.id === tid)
  if (i < 0) return
  tabs.value.splice(i, 1)
  delete docInitials.value[tid]
  if (activeTabId.value === tid) activeTabId.value = tabs.value[Math.min(i, tabs.value.length - 1)]?.id ?? ''
}
function renameTab(tid: string, title: string) {
  const t = tabs.value.find(x => x.id === tid)
  if (t) t.title = title
}
const stageScale = ref(1)
function fitStage() {
  stageScale.value = Math.min(window.innerWidth / 1600, window.innerHeight / 900)
}
onMounted(() => {
  fitStage()
  window.addEventListener('resize', fitStage)
})

interface NavItem { key: string; label: string; icon: string; hq?: boolean; admin?: boolean; todo?: boolean }
const navDef: { title: string; items: NavItem[] }[] = [
  { title: '销售', items: [
    { key: 'sale', label: '销售开单', icon: 'sale' },
    { key: 'saleReturn', label: '销退单', icon: 'return' },
    { key: 'recycle', label: '旧料回收', icon: 'recycle' },
  ]},
  { title: '库存', items: [
    { key: 'inventory', label: '库存查询', icon: 'search' },
    { key: 'inbound', label: '入库', icon: 'inbound', hq: true },
    { key: 'outbound', label: '退库', icon: 'outbound', hq: true },
    { key: 'transfer', label: '调拨', icon: 'transfer', hq: true },
    { key: 'stocktake', label: '盘点', icon: 'stocktake' },
  ]},
  { title: '资料', items: [
    { key: 'goldprice', label: '今日金价', icon: 'coins' },
    { key: 'dicts', label: '基础资料', icon: 'book', admin: true },
    { key: 'salespersons', label: '售货员', icon: 'users', admin: true },
    { key: 'distributors', label: '分销商', icon: 'store', admin: true },
    { key: 'rules', label: '提成规则', icon: 'percent', admin: true },
  ]},
  { title: '系统', items: [
    { key: 'report', label: '提成报表', icon: 'chart', admin: true },
    { key: 'users', label: '用户管理', icon: 'user', admin: true },
    { key: 'salesReport', label: '销售报表', icon: 'chart', admin: true, todo: true },
    { key: 'oldmat', label: '旧料库', icon: 'archive', admin: true, todo: true },
    { key: 'perms', label: '权限管理', icon: 'shield', admin: true, todo: true },
    { key: 'settings', label: '系统参数', icon: 'settings', admin: true, todo: true },
  ]},
]
const visibleNav = computed(() => navDef
  .map(g => ({ ...g, items: g.items.filter(m => (!m.hq || isHQ.value) && (!m.admin || isAdmin.value)) }))
  .filter(g => g.items.length))
const placeholderPages: Record<string, { label: string; icon: string }> = {
  salesReport: { label: '销售报表', icon: 'chart' },
  oldmat: { label: '旧料库', icon: 'archive' },
  perms: { label: '权限管理', icon: 'shield' },
  settings: { label: '系统参数', icon: 'settings' },
}
function goPage(k: string) {
  errMsg.value = ''
  const def = navDef.flatMap(g => g.items).find(m => m.key === k)
  openTab(k, def?.label ?? k, def?.icon ?? 'book')
  // 懒加载的管理页
  if (k === 'users') loadUsers()
  if (k === 'rules') loadRules()
  if (k === 'report') loadReport()
}

// ===== 单据页签（v0.31入库首发，v0.32七种单据通用）=====
// 打开时把列表里的单据快照带给组件做初始数据；之后组件自管状态（每页签一套，互不串）。
const docInitials = ref<Record<string, unknown>>({})
const docTabDefs: Record<string, { newTitle: string; icon: string }> = {
  inboundDoc: { newTitle: '新建入库单', icon: 'inbound' },
  outboundDoc: { newTitle: '新建退库单', icon: 'outbound' },
  transferDoc: { newTitle: '新建调拨单', icon: 'transfer' },
  saleDoc: { newTitle: '新建销售单', icon: 'sale' },
  saleReturnDoc: { newTitle: '新建销退单', icon: 'return' },
  recycleDoc: { newTitle: '新建回收单', icon: 'recycle' },
  stocktakeDoc: { newTitle: '新建盘点单', icon: 'stocktake' },
}
function openDocTab(kind: string, d: { id: number; docNo: string }) {
  const t = openTab(kind, d.docNo, docTabDefs[kind].icon, { docId: d.id })
  if (!(t.id in docInitials.value)) docInitials.value[t.id] = d
}
function openNewDocTab(kind: string) {
  const t = openTab(kind, docTabDefs[kind].newTitle, docTabDefs[kind].icon, { multi: true })
  docInitials.value[t.id] = null
}

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
// ===== 分销商与调拨单（v0.18） =====
interface Distributor {
  id: number
  name: string
  status: number
}
const distributors = ref<Distributor[]>([])
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
const crRows = ref<CommRow[]>([])
const crTotals = ref({ sale: 0, trade: 0, mgr: 0, ret: 0, net: 0 })
const today = new Date().toISOString().slice(0, 10)
const crFrom = ref(today.slice(0, 8) + '01') // 本月1号
const crTo = ref(today)

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
// ===== 工作台快捷操作（v0.32）=====
// 预览区的 确认/反确认/删除/导出标签：小事不用开页签。完整编辑走单据页签。
async function quickAct(fn: () => Promise<string>) {
  errMsg.value = ''
  try {
    flash(await fn())
  } catch (e) {
    errMsg.value = (e as Error).message
  }
  await refreshAll()
}
const ibConfirmDoc = (d: Doc) => quickAct(async () => { const c = await api.inboundConfirm(d.id); return `已确认：${c.docNo}，生成 ${c.items.length} 件货品` })
const ibUnconfirmDoc = (d: Doc) => quickAct(async () => { await api.inboundUnconfirm(d.id); return `已反确认：${d.docNo} 退回草稿，货品已撤回` })
const ibDeleteDoc = (d: Doc) => quickAct(async () => { await api.inboundDelete(d.id); return `草稿 ${d.docNo} 已删除（单号不回收）` })
async function ibExportLabels(d: Doc) {
  errMsg.value = ''
  try {
    await downloadFile(`/api/doc/inbound/labels?id=${d.id}`, `标签数据-${d.docNo}.xlsx`)
    flash(`标签数据已导出：${d.docNo}——在 Label Matrix 里把数据源指向该文件即可打印`)
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}
const obConfirmDoc = (d: ODoc) => quickAct(async () => { await api.outboundConfirm(d.id); return `退库已确认：${d.docNo}` })
const obUnconfirmDoc = (d: ODoc) => quickAct(async () => { await api.outboundUnconfirm(d.id); return `退库已反确认：${d.docNo}，货品已回到在库` })
const obDeleteDoc = (d: ODoc) => quickAct(async () => { await api.outboundDelete(d.id); return `退库草稿 ${d.docNo} 已删除` })
const tfConfirmDoc = (d: TDoc) => quickAct(async () => { await api.transferConfirm(d.id); return `调拨已确认：${d.docNo}` })
const tfUnconfirmDoc = (d: TDoc) => quickAct(async () => { await api.transferUnconfirm(d.id); return `调拨已反确认：${d.docNo}，货品拉回「${d.fromName}」待处理` })
const tfDeleteDoc = (d: TDoc) => quickAct(async () => { await api.transferDelete(d.id); return `调拨草稿 ${d.docNo} 已删除` })
const slConfirmDoc = (d: SDoc) => quickAct(async () => { const c = await api.saleConfirm(d.id); return `已收款确认：${d.docNo}，合计 ¥${c.totalAmount}` })
const slUnconfirmDoc = (d: SDoc) => quickAct(async () => { await api.saleUnconfirm(d.id); return `销售已反确认：${d.docNo}，货品回到在库` })
const slDeleteDoc = (d: SDoc) => quickAct(async () => { await api.saleDelete(d.id); return `销售草稿 ${d.docNo} 已删除` })
const srConfirmDoc = (d: RDoc) => quickAct(async () => { const c = await api.saleReturnConfirm(d.id); return `销退已确认：${d.docNo}，退款合计 ¥${c.totalAmount}` })
const srUnconfirmDoc = (d: RDoc) => quickAct(async () => { await api.saleReturnUnconfirm(d.id); return `销退已反确认：${d.docNo}` })
const srDeleteDoc = (d: RDoc) => quickAct(async () => { await api.saleReturnDelete(d.id); return `销退草稿 ${d.docNo} 已删除，货品回到"已售"` })
const hsConfirmDoc = (d: HDoc) => quickAct(async () => { const c = await api.recycleConfirm(d.id); return `回收已确认：${d.docNo}，付顾客 ¥${c.payout}` })
const hsUnconfirmDoc = (d: HDoc) => quickAct(async () => { await api.recycleUnconfirm(d.id); return `回收已反确认：${d.docNo}` })
const hsDeleteDoc = (d: HDoc) => quickAct(async () => { await api.recycleDelete(d.id); return `回收草稿 ${d.docNo} 已删除` })
const stConfirmDoc = (d: PDoc) => quickAct(async () => { const c = await api.stocktakeConfirm(d.id); return `盘点已确认：${d.docNo}——正常${c.normal} / 盘亏${c.loss} / 盘盈${c.gain}` })
const stUnconfirmDoc = (d: PDoc) => quickAct(async () => { await api.stocktakeUnconfirm(d.id); return `盘点已反确认：${d.docNo}，可继续补扫后重新确认` })
const stDeleteDoc = (d: PDoc) => quickAct(async () => { await api.stocktakeDelete(d.id); return `盘点草稿 ${d.docNo} 已删除` })

// ===== 工作台概览列（v0.32）：每个模块一张"列清单"，骨架是同一个 DocWorkbench =====
interface WbCol { label: string; get: (d: any) => string | number; mono?: boolean }
const sumItemsW = (arr?: { weightG?: number }[]) => (arr ?? []).reduce((a, x) => a + (x.weightG ?? 0), 0)
const ibCols: WbCol[] = [
  { label: '入库单号', get: d => d.docNo, mono: true },
  { label: '首饰大类', get: d => d.category },
  { label: '入库时间', get: d => d.madeAt },
  { label: '件数', get: d => d.items?.length ?? 0 },
  { label: '总重(g)', get: d => sumItemsW(d.items).toFixed(2) },
  { label: '制单人', get: d => d.madeBy || '—' },
]
const obCols: WbCol[] = [
  { label: '退库单号', get: d => d.docNo, mono: true },
  { label: '退往供应商', get: d => d.supplier || '—' },
  { label: '时间', get: d => d.madeAt },
  { label: '件数', get: d => d.items?.length ?? 0 },
  { label: '总重(g)', get: d => sumItemsW(d.items).toFixed(2) },
]
const tfCols: WbCol[] = [
  { label: '调拨单号', get: d => d.docNo, mono: true },
  { label: '调出 → 调入', get: d => `${d.fromName} → ${d.toName}` },
  { label: '时间', get: d => d.madeAt },
  { label: '件数', get: d => d.lines?.length ?? 0 },
  { label: '总重(g)', get: d => sumItemsW(d.lines).toFixed(2) },
]
const slCols: WbCol[] = [
  { label: '销售单号', get: d => d.docNo, mono: true },
  { label: '时间', get: d => d.madeAt },
  { label: '件数', get: d => d.lines?.length ?? 0 },
  { label: '净额(¥)', get: d => d.totalAmount ? `${d.totalAmount < 0 ? '退 ' : ''}${Math.abs(d.totalAmount).toFixed(2)}` : '—' },
  { label: '旧料(g)', get: d => d.oldLines?.length ? d.oldLines.reduce((a: number, o: any) => a + (o.weightG || 0), 0).toFixed(2) : '—' },
  { label: '售货员', get: d => d.salespersonName || '—' },
]
const srCols: WbCol[] = [
  { label: '销退单号', get: d => d.docNo, mono: true },
  { label: '时间', get: d => d.madeAt },
  { label: '件数', get: d => d.lines?.length ?? 0 },
  { label: '退款(¥)', get: d => d.totalAmount > 0 ? d.totalAmount.toFixed(2) : '—' },
]
const hsCols: WbCol[] = [
  { label: '回收单号', get: d => d.docNo, mono: true },
  { label: '时间', get: d => d.madeAt },
  { label: '行数', get: d => d.lines?.length ?? 0 },
  { label: '付顾客(¥)', get: d => d.payout > 0 ? d.payout.toFixed(2) : '—' },
  { label: '经手', get: d => d.salespersonName || '—' },
]
const stCols: WbCol[] = [
  { label: '盘点单号', get: d => d.docNo, mono: true },
  { label: '位置', get: d => d.locName },
  { label: '时间', get: d => d.madeAt },
  { label: '结果', get: d => d.result ? `正常${d.result.normal} / 亏${d.result.loss} / 盈${d.result.gain}` : `已扫 ${d.scans?.length ?? 0} 件` },
]

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
const nrCategory = ref('')
const nrBiz = ref('正常销售')
const nrMode = ref('标签价')
const nrCalc = ref('销售额百分比')
const nrValue = ref<number | null>(null)
const nrFrom = ref(new Date().toISOString().slice(0, 10))
const calcUnit = (t: string) => t === '销售额百分比' ? '%' : t === '每克固定' ? '元/克' : '元/件'

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
const nuUsername = ref('')
const nuName = ref('')
const nuPassword = ref('')
const nuStore = ref(0) // 0=总部

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
    goPage('sale')
    await refreshAll()
  } catch (e) {
    errMsg.value = (e as Error).message
  }
}

function doLogout() {
  clearToken()
  logged.value = false
  tabs.value = []
  activeTabId.value = ''
  docInitials.value = {}
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
    goPage('sale')
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

</script>

<template>
  <!-- v0.30 UI壳：外层 viewport 铺满窗口，内层 stage 固定 1600×900 设计稿，
       按窗口大小整体等比缩放（长宽同比例），不做逐元素自适应。 -->
  <div class="viewport">
    <div class="stage" :style="{ transform: `translate(-50%, -50%) scale(${stageScale})` }">

      <!-- 登录屏 -->
      <div v-if="!logged" class="login-screen">
        <div class="login-card">
          <div class="login-brand"><Icon name="gem" :size="30" /></div>
          <h1 class="login-title">黄金零售管理系统</h1>
          <label>账号 <input v-model="username" /></label>
          <label>密码 <input v-model="password" type="password" @keyup.enter="doLogin" /></label>
          <button class="login-btn" @click="doLogin">登 录</button>
          <p v-if="errMsg" class="err">{{ errMsg }}</p>
        </div>
      </div>

      <!-- 主界面：左侧导航 + 右侧内容 -->
      <div v-else class="app">
        <aside class="sidebar">
          <div class="brand"><Icon name="gem" :size="18" /><span>黄金零售系统</span></div>
          <nav class="nav">
            <div v-for="g in visibleNav" :key="g.title" class="nav-group">
              <div class="nav-title">{{ g.title }}</div>
              <a v-for="m in g.items" :key="m.key"
                :class="['nav-item', navKey === m.key ? 'active' : '']"
                @click="goPage(m.key)">
                <Icon :name="m.icon" :size="15" />
                <span>{{ m.label }}</span>
                <i v-if="m.todo" class="todo-dot" title="规划中"></i>
              </a>
            </div>
          </nav>
        </aside>

        <div class="main">
          <header class="topbar">
            <div class="gold-ticker">
              <span v-for="g in goldPrices.slice(0, 3)" :key="g.purity" class="tick">
                {{ g.purity }} <b>¥{{ g.retailPrice }}</b>
              </span>
            </div>
            <span class="spacer"></span>
            <span class="who">
              <Icon name="user" :size="14" />
              {{ userLabel }}<template v-if="userStore">（{{ userStore }}）</template>
            </span>
            <button class="mini" @click="showPwd = !showPwd">修改密码</button>
            <button class="mini" @click="doLogout"><Icon name="logout" :size="13" /> 退出</button>
          </header>

          <!-- 页签栏（v0.31）：多业务页同开，点切换，×关闭 -->
          <div class="tabstrip">
            <div v-for="t in tabs" :key="t.id" :class="['tab', t.id === activeTabId ? 'active' : '']"
              @click="activeTabId = t.id">
              <Icon :name="t.icon" :size="13" />
              <span>{{ t.title }}</span>
              <span class="tab-x" title="关闭" @click.stop="closeTab(t.id)">×</span>
            </div>
          </div>

          <div class="content">
            <section v-if="!tabs.length" class="card placeholder">
              <Icon name="gem" :size="40" />
              <h2>从左侧菜单打开一个页面</h2>
              <p class="hint">页签可以同时开多个，互相切换互不干扰。</p>
            </section>
      <div v-if="showPwd" class="card">
        <div class="row">
          <label>旧密码 <input v-model="oldPwd" type="password" /></label>
          <label>新密码(至少6位) <input v-model="newPwd" type="password" /></label>
          <button @click="changePwd">确认修改</button>
        </div>
      </div>
      <p v-if="okMsg" class="ok banner">{{ okMsg }}</p>
      <p v-if="errMsg" class="err banner">{{ errMsg }}</p>

      <!-- 基础资料（仅管理员可见） -->
      <section v-if="isAdmin && page==='dicts'" class="card">
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
      <section v-if="isAdmin && page==='salespersons'" class="card">
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
      <section v-if="isAdmin && page==='rules'" class="card">
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
      <section v-if="isAdmin && page==='report'" class="card">
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
      <section v-if="isAdmin && page==='distributors'" class="card">
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
      <section v-if="isAdmin && page==='users'" class="card">
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
      <section v-if="page==='goldprice'" class="card">
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

      <template v-if="page==='sale'">
        <DocWorkbench title="销售单" :docs="sdocs" :columns="slCols" create-label="销售开单"
          @refresh="refreshAll" @open="d => openDocTab('saleDoc', d)" @create="openNewDocTab('saleDoc')">
          <template #headinfo="{ doc }">
            <span class="hint" v-if="doc.payments?.length">
              {{ doc.payments.map((p: any) => `${p.method}¥${Number(p.amount ?? 0).toFixed(2)}`).join(' + ') }}
            </span>
          </template>
          <template #actions="{ doc }">
            <template v-if="doc.status === '草稿'">
              <button class="mini" @click="openDocTab('saleDoc', doc)">取单</button>
              <button class="mini" @click="slConfirmDoc(doc)">确认收款</button>
              <button class="mini danger" @click="slDeleteDoc(doc)">删除</button>
            </template>
            <template v-else>
              <button class="mini" @click="openDocTab('saleDoc', doc)">打开</button>
              <button class="mini danger" @click="slUnconfirmDoc(doc)">反确认(限当日)</button>
            </template>
          </template>
          <template #preview="{ doc }">
            <table>
              <thead><tr><th>#</th><th>条码</th><th>名称</th><th>结算方式</th><th>实售价(¥)</th></tr></thead>
              <tbody>
                <tr v-for="(l, j) in doc.lines" :key="j">
                  <td>{{ j + 1 }}</td>
                  <td class="mono">{{ l.barcode }}</td>
                  <td>{{ l.name }}</td>
                  <td>{{ l.mode }}</td>
                  <td>¥{{ Number(l.soldPrice ?? 0).toFixed(2) }}</td>
                </tr>
              </tbody>
            </table>
          </template>
        </DocWorkbench>
      </template>

      <template v-if="page==='transfer'">
        <DocWorkbench v-if="isHQ" title="调拨单（分货/退总库/互调）" :docs="tdocs" :columns="tfCols" create-label="新建调拨单"
          @refresh="refreshAll" @open="d => openDocTab('transferDoc', d)" @create="openNewDocTab('transferDoc')">
          <template #actions="{ doc }">
            <template v-if="doc.status === '草稿'">
              <button class="mini" @click="openDocTab('transferDoc', doc)">打开编辑</button>
              <button class="mini" @click="tfConfirmDoc(doc)">确认调拨</button>
              <button class="mini danger" @click="tfDeleteDoc(doc)">删除</button>
            </template>
            <template v-else>
              <button class="mini" @click="openDocTab('transferDoc', doc)">打开</button>
              <button class="mini danger" @click="tfUnconfirmDoc(doc)">反确认</button>
            </template>
          </template>
          <template #preview="{ doc }">
            <table>
              <thead><tr><th>#</th><th>条码</th><th>名称</th><th>成色</th><th>重量(g)</th></tr></thead>
              <tbody>
                <tr v-for="(l, j) in doc.lines" :key="j">
                  <td>{{ j + 1 }}</td>
                  <td class="mono">{{ l.barcode }}</td>
                  <td>{{ l.name }}</td>
                  <td>{{ l.purity }}</td>
                  <td>{{ (l.weightG ?? 0).toFixed(2) }}</td>
                </tr>
              </tbody>
            </table>
          </template>
        </DocWorkbench>
      </template>

      <template v-if="page==='recycle'">
        <DocWorkbench title="回收单" :docs="hdocs" :columns="hsCols" create-label="新建回收单"
          @refresh="refreshAll" @open="d => openDocTab('recycleDoc', d)" @create="openNewDocTab('recycleDoc')">
          <template #actions="{ doc }">
            <template v-if="doc.status === '草稿'">
              <button class="mini" @click="openDocTab('recycleDoc', doc)">取单</button>
              <button class="mini" @click="hsConfirmDoc(doc)">确认付款</button>
              <button class="mini danger" @click="hsDeleteDoc(doc)">删除</button>
            </template>
            <template v-else>
              <button class="mini" @click="openDocTab('recycleDoc', doc)">打开</button>
              <button class="mini danger" @click="hsUnconfirmDoc(doc)">反确认(限当日)</button>
            </template>
          </template>
          <template #preview="{ doc }">
            <table>
              <thead><tr><th>#</th><th>大类</th><th>成色</th><th>克重(g)</th><th>折算</th></tr></thead>
              <tbody>
                <tr v-for="(o, j) in doc.lines" :key="j">
                  <td>{{ j + 1 }}</td>
                  <td>{{ o.category }}</td>
                  <td>{{ o.purity }}</td>
                  <td>{{ (o.weightG ?? 0).toFixed(2) }}</td>
                  <td>{{ o.recyclePrice ? `×${o.recyclePrice.toFixed(2)} = ¥${(o.credit ?? 0).toFixed(2)}` : '—' }}</td>
                </tr>
              </tbody>
            </table>
          </template>
        </DocWorkbench>
      </template>

      <template v-if="page==='saleReturn'">
        <DocWorkbench title="销退单" :docs="srdocs" :columns="srCols" create-label="新建销退单"
          @refresh="refreshAll" @open="d => openDocTab('saleReturnDoc', d)" @create="openNewDocTab('saleReturnDoc')">
          <template #headinfo="{ doc }">
            <span class="hint" v-if="doc.payments?.length">
              {{ doc.payments.map((p: any) => `${p.method}¥${Number(p.amount ?? 0).toFixed(2)}`).join(' + ') }}
            </span>
          </template>
          <template #actions="{ doc }">
            <template v-if="doc.status === '草稿'">
              <button class="mini" @click="openDocTab('saleReturnDoc', doc)">取单</button>
              <button class="mini" @click="srConfirmDoc(doc)">确认退款</button>
              <button class="mini danger" @click="srDeleteDoc(doc)">删除</button>
            </template>
            <template v-else>
              <button class="mini" @click="openDocTab('saleReturnDoc', doc)">打开</button>
              <button class="mini danger" @click="srUnconfirmDoc(doc)">反确认(限当日)</button>
            </template>
          </template>
          <template #preview="{ doc }">
            <table>
              <thead><tr><th>#</th><th>条码</th><th>名称</th><th>原销售单</th><th>退款(¥)</th></tr></thead>
              <tbody>
                <tr v-for="(l, j) in doc.lines" :key="j">
                  <td>{{ j + 1 }}</td>
                  <td class="mono">{{ l.barcode }}</td>
                  <td>{{ l.name }}</td>
                  <td class="mono">{{ l.origDocNo }}</td>
                  <td>¥{{ Number(l.refundPrice ?? 0).toFixed(2) }}</td>
                </tr>
              </tbody>
            </table>
          </template>
        </DocWorkbench>
      </template>

      <template v-if="page==='inbound'">
        <DocWorkbench v-if="isHQ" title="入库单" :docs="docs" :columns="ibCols" create-label="新建入库单"
          @refresh="refreshAll" @open="d => openDocTab('inboundDoc', d)" @create="openNewDocTab('inboundDoc')">
          <template #headinfo="{ doc }">
            <span class="hint">{{ doc.category }} · {{ sumItemsW(doc.items).toFixed(2) }}g</span>
          </template>
          <template #actions="{ doc }">
            <template v-if="doc.status === '草稿'">
              <button class="mini" @click="openDocTab('inboundDoc', doc)">打开编辑</button>
              <button class="mini" @click="ibConfirmDoc(doc)">确认</button>
              <button class="mini danger" @click="ibDeleteDoc(doc)">删除</button>
            </template>
            <template v-else>
              <button class="mini" @click="openDocTab('inboundDoc', doc)">打开</button>
              <button class="mini" @click="ibExportLabels(doc)">导出标签</button>
              <button class="mini danger" @click="ibUnconfirmDoc(doc)">反确认</button>
            </template>
          </template>
          <template #preview="{ doc }">
            <table>
              <thead><tr><th>#</th><th>条码号</th><th>首饰名称</th><th>成色</th><th>克重(g)</th><th>售价(¥)</th></tr></thead>
              <tbody>
                <tr v-for="(it, j) in doc.items" :key="j">
                  <td>{{ j + 1 }}</td>
                  <td class="mono">{{ it.barcode || '(待发号)' }}</td>
                  <td>{{ it.name }}</td>
                  <td>{{ it.purity }}</td>
                  <td>{{ (it.weightG ?? 0).toFixed(2) }}</td>
                  <td>{{ (it.price ?? 0).toFixed(2) }}</td>
                </tr>
              </tbody>
            </table>
          </template>
        </DocWorkbench>
      </template>

      <!-- 单据页签（v0.31入库首发，v0.32七种通用）：v-show 保活，可同开多张单互不丢编辑状态 -->
      <template v-for="t in tabs" :key="t.id">
        <InboundDoc v-if="t.kind === 'inboundDoc'" v-show="activeTabId === t.id"
          :doc-id="t.docId ?? 0" :initial="(docInitials[t.id] as any) ?? null"
          :dicts="dicts" :is-admin="isAdmin"
          @close="closeTab(t.id)" @refresh="refreshAll" @flash="flash"
          @rename="(sNew) => renameTab(t.id, sNew)" />
        <OutboundDoc v-if="t.kind === 'outboundDoc'" v-show="activeTabId === t.id"
          :doc-id="t.docId ?? 0" :initial="(docInitials[t.id] as any) ?? null" :items="items"
          @close="closeTab(t.id)" @refresh="refreshAll" @flash="flash"
          @rename="(sNew) => renameTab(t.id, sNew)" />
        <TransferDoc v-if="t.kind === 'transferDoc'" v-show="activeTabId === t.id"
          :doc-id="t.docId ?? 0" :initial="(docInitials[t.id] as any) ?? null"
          :items="items" :distributors="distributors"
          @close="closeTab(t.id)" @refresh="refreshAll" @flash="flash"
          @rename="(sNew) => renameTab(t.id, sNew)" />
        <SaleDoc v-if="t.kind === 'saleDoc'" v-show="activeTabId === t.id"
          :doc-id="t.docId ?? 0" :initial="(docInitials[t.id] as any) ?? null"
          :items="items" :gold-prices="goldPrices" :salespersons="saleSalespersons()"
          :dicts="dicts" :user-store="userStore"
          @close="closeTab(t.id)" @refresh="refreshAll" @flash="flash"
          @rename="(sNew) => renameTab(t.id, sNew)" />
        <SaleReturnDoc v-if="t.kind === 'saleReturnDoc'" v-show="activeTabId === t.id"
          :doc-id="t.docId ?? 0" :initial="(docInitials[t.id] as any) ?? null" :dicts="dicts"
          @close="closeTab(t.id)" @refresh="refreshAll" @flash="flash"
          @rename="(sNew) => renameTab(t.id, sNew)" />
        <RecycleDoc v-if="t.kind === 'recycleDoc'" v-show="activeTabId === t.id"
          :doc-id="t.docId ?? 0" :initial="(docInitials[t.id] as any) ?? null"
          :dicts="dicts" :gold-prices="goldPrices" :salespersons="saleSalespersons()"
          @close="closeTab(t.id)" @refresh="refreshAll" @flash="flash"
          @rename="(sNew) => renameTab(t.id, sNew)" />
        <StocktakeDoc v-if="t.kind === 'stocktakeDoc'" v-show="activeTabId === t.id"
          :doc-id="t.docId ?? 0" :initial="(docInitials[t.id] as any) ?? null"
          :items="items" :distributors="distributors"
          :is-h-q="isHQ" :user-store-id="userStoreId" :user-store="userStore"
          @close="closeTab(t.id)" @refresh="refreshAll" @flash="flash"
          @rename="(sNew) => renameTab(t.id, sNew)" />
      </template>

      <template v-if="page==='outbound'">
        <DocWorkbench v-if="isHQ" title="退库单" :docs="odocs" :columns="obCols" create-label="新建退库单"
          @refresh="refreshAll" @open="d => openDocTab('outboundDoc', d)" @create="openNewDocTab('outboundDoc')">
          <template #actions="{ doc }">
            <template v-if="doc.status === '草稿'">
              <button class="mini" @click="openDocTab('outboundDoc', doc)">打开编辑</button>
              <button class="mini" @click="obConfirmDoc(doc)">确认</button>
              <button class="mini danger" @click="obDeleteDoc(doc)">删除</button>
            </template>
            <template v-else>
              <button class="mini" @click="openDocTab('outboundDoc', doc)">打开</button>
              <button class="mini danger" @click="obUnconfirmDoc(doc)">反确认</button>
            </template>
          </template>
          <template #preview="{ doc }">
            <table>
              <thead><tr><th>#</th><th>条码</th><th>名称</th><th>成色</th><th>重量(g)</th></tr></thead>
              <tbody>
                <tr v-for="(it, j) in doc.items" :key="j">
                  <td>{{ j + 1 }}</td>
                  <td class="mono">{{ it.barcode }}</td>
                  <td>{{ it.name }}</td>
                  <td>{{ it.purity }}</td>
                  <td>{{ (it.weightG ?? 0).toFixed(2) }}</td>
                </tr>
              </tbody>
            </table>
          </template>
        </DocWorkbench>
      </template>

      <template v-if="page==='stocktake'">
        <DocWorkbench title="盘点单" :docs="pdocs" :columns="stCols" create-label="新建盘点单"
          @refresh="refreshAll" @open="d => openDocTab('stocktakeDoc', d)" @create="openNewDocTab('stocktakeDoc')">
          <template #actions="{ doc }">
            <template v-if="doc.status === '草稿'">
              <button class="mini" @click="openDocTab('stocktakeDoc', doc)">继续盘</button>
              <button class="mini" @click="stConfirmDoc(doc)">完成盘点</button>
              <button class="mini danger" @click="stDeleteDoc(doc)">删除</button>
            </template>
            <template v-else>
              <button class="mini" @click="openDocTab('stocktakeDoc', doc)">差异详情</button>
              <button class="mini danger" @click="stUnconfirmDoc(doc)">反确认(重盘)</button>
            </template>
          </template>
          <template #preview="{ doc }">
            <table v-if="doc.status === '草稿'">
              <tbody>
                <tr v-for="(x, j) in doc.scans" :key="j">
                  <td style="width:40px">{{ j + 1 }}</td>
                  <td class="mono" style="width:160px">{{ x.barcode }}</td>
                  <td>{{ x.name || '—' }}</td>
                </tr>
              </tbody>
            </table>
            <p v-else-if="doc.result" class="hint" style="padding:8px">
              正常 {{ doc.result.normal }} / 盘亏 {{ doc.result.loss }} / 盘盈 {{ doc.result.gain }}（实扫 {{ doc.result.scanned }} 件）
              ——双击或点"差异详情"打开单据页看明细。
            </p>
          </template>
        </DocWorkbench>
      </template>

      <template v-if="page==='inventory'">

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

      <!-- 规划中的空页面（v0.30 占位） -->
      <section v-if="placeholderPages[page]" class="card placeholder">
        <Icon :name="placeholderPages[page].icon" :size="40" />
        <h2>{{ placeholderPages[page].label }}</h2>
        <p class="hint">该模块已在规划中，界面先留空，后续版本开放。</p>
      </section>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style>
/* ===== v0.30 UI壳：深色侧栏 + 金色点缀，紧凑密度(13px)，1600×900 固定画布等比缩放 ===== */
:root {
  --sidebar-bg: #182420;      /* 深墨绿近黑 */
  --sidebar-bg2: #121b18;
  --gold: #d4af5a;            /* 金色点缀 */
  --gold-dim: #a8893f;
  --ink: #1d2823;
  --content-bg: #eef0ee;
  --line: #dfe3df;
  --green: #2f7d4f;
}
* { box-sizing: border-box; }
html, body, #app { margin: 0; padding: 0; width: 100%; height: 100%; }
body {
  font-family: -apple-system, "PingFang SC", "Microsoft YaHei", sans-serif;
  background: #0d1512; overflow: hidden; font-size: 13px; color: var(--ink);
}

/* --- 画布：外层铺满窗口，内层固定 1600×900 居中整体缩放 --- */
.viewport { position: fixed; inset: 0; background: #0d1512; overflow: hidden; }
.stage {
  position: absolute; left: 50%; top: 50%;
  width: 1600px; height: 900px;
  transform-origin: center center;
  background: var(--content-bg);
}

/* --- 登录屏 --- */
.login-screen {
  width: 100%; height: 100%; display: flex; align-items: center; justify-content: center;
  background: radial-gradient(900px 600px at 30% 20%, #243830 0%, var(--sidebar-bg2) 60%, #0d1512 100%);
}
.login-card {
  width: 360px; background: #fff; border-radius: 10px; padding: 36px 40px 30px;
  box-shadow: 0 18px 50px rgba(0,0,0,.45); display: flex; flex-direction: column; gap: 12px;
  border-top: 3px solid var(--gold);
}
.login-brand { color: var(--gold-dim); text-align: center; }
.login-title { font-size: 18px; text-align: center; margin: 0 0 8px; letter-spacing: 2px; }
.login-card label { display: flex; flex-direction: column; gap: 4px; font-size: 13px; color: #555; }
.login-card input { width: 100%; }
.login-btn { margin-top: 6px; padding: 9px 0; font-size: 14px; letter-spacing: 6px; }

/* --- 主布局：侧栏 208px + 右侧内容 --- */
.app { display: flex; width: 100%; height: 100%; }
.sidebar {
  width: 208px; flex-shrink: 0; display: flex; flex-direction: column;
  background: linear-gradient(180deg, var(--sidebar-bg) 0%, var(--sidebar-bg2) 100%);
  color: #c9d2cc;
}
.brand {
  display: flex; align-items: center; gap: 8px; padding: 16px 18px 14px;
  color: var(--gold); font-size: 15px; font-weight: 600; letter-spacing: 1px;
  border-bottom: 1px solid rgba(212,175,90,.18);
}
.nav { flex: 1; overflow-y: auto; padding: 8px 0 12px; }
.nav-group { margin-top: 8px; }
.nav-title {
  padding: 6px 18px 4px; font-size: 11px; letter-spacing: 3px;
  color: rgba(212,175,90,.55);
}
.nav-item {
  display: flex; align-items: center; gap: 9px; padding: 7px 18px;
  cursor: pointer; font-size: 13px; color: #c9d2cc; user-select: none;
  border-left: 3px solid transparent;
}
.nav-item:hover { background: rgba(255,255,255,.05); color: #fff; }
.nav-item.active {
  background: rgba(212,175,90,.12); color: var(--gold);
  border-left-color: var(--gold); font-weight: 600;
}
.todo-dot {
  width: 5px; height: 5px; border-radius: 50%; background: rgba(201,210,204,.35);
  margin-left: auto;
}

/* --- 右侧：顶栏 + 内容区 --- */
.main { flex: 1; min-width: 0; display: flex; flex-direction: column; }
.topbar {
  height: 46px; flex-shrink: 0; display: flex; align-items: center; gap: 10px;
  padding: 0 18px; background: #fff; border-bottom: 1px solid var(--line);
}
.gold-ticker { display: flex; gap: 14px; font-size: 12.5px; color: #666; }
.gold-ticker .tick b { color: var(--gold-dim); font-size: 13.5px; margin-left: 2px; }
.who { display: flex; align-items: center; gap: 5px; font-size: 13px; color: #444; }
.content { flex: 1; overflow-y: auto; padding: 14px 18px; }

/* --- 卡片与通用控件（紧凑密度） --- */
.card { background: #fff; border: 1px solid var(--line); border-radius: 8px; padding: 12px 16px; margin-bottom: 12px; }
.card h2 { font-size: 14.5px; margin: 0 0 8px; display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
.row { display: flex; gap: 10px; align-items: center; margin: 6px 0; flex-wrap: wrap; }
label { font-size: 13px; }
input, select { padding: 4px 7px; border: 1px solid #ccc; border-radius: 4px; font-size: 13px; background: #fff; }
button {
  padding: 5px 14px; border: none; border-radius: 4px; background: var(--green);
  color: #fff; cursor: pointer; font-size: 13px;
  display: inline-flex; align-items: center; gap: 4px; vertical-align: middle;
}
button.mini { padding: 3px 9px; font-size: 12px; background: #6b7280; }
button.gray { background: #4b5563; }
button.danger { background: #b4552d; }
button:disabled { opacity: 0.4; cursor: not-allowed; }
table { width: 100%; border-collapse: collapse; font-size: 13px; margin: 6px 0; }
th, td { border: 1px solid var(--line); padding: 4px 7px; text-align: left; }
th { background: #f4f5f3; font-weight: 600; }
td input { width: 100%; box-sizing: border-box; border: 1px solid #ddd; }
.hint { color: #888; font-size: 12px; }
.err { color: #c0392b; font-size: 13px; }
.ok { color: var(--green); font-size: 13px; font-weight: 600; }
.banner { background: #fff; border-radius: 6px; padding: 7px 12px; border: 1px solid var(--line); margin: 0 0 10px; }
.mono { font-family: ui-monospace, Menlo, monospace; }
.doc { border: 1px solid #e8eaed; border-radius: 6px; padding: 6px 10px; margin-bottom: 8px; background: #fff; }
.dochead { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
.badge { padding: 1px 9px; border-radius: 10px; font-size: 11.5px; }
.badge.draft { background: #fdf2d0; color: #8a6d1a; }
.badge.ok2 { background: #ddf0e3; color: #22663d; }
.spacer { flex: 1; }

/* --- 页签栏（v0.31） --- */
.tabstrip {
  height: 34px; flex-shrink: 0; display: flex; align-items: flex-end; gap: 2px;
  padding: 0 10px; background: #e2e5e1; border-bottom: 1px solid var(--line);
  overflow-x: auto; overflow-y: hidden;
}
.tab {
  display: flex; align-items: center; gap: 6px; padding: 5px 7px 5px 12px;
  font-size: 12.5px; color: #555; background: #d4d8d3; border-radius: 6px 6px 0 0;
  cursor: pointer; user-select: none; white-space: nowrap;
}
.tab.active { background: var(--content-bg); color: var(--ink); font-weight: 600; box-shadow: inset 0 2px 0 var(--gold); }
.tab-x {
  width: 16px; height: 16px; line-height: 15px; text-align: center;
  border-radius: 50%; font-size: 13px; color: #888;
}
.tab-x:hover { background: rgba(0,0,0,.14); color: #000; }

/* --- 工作台概览/预览表格（v0.31） --- */
.grid-scroll { max-height: 330px; overflow-y: auto; border: 1px solid var(--line); border-radius: 6px; }
.grid-scroll.preview { max-height: 290px; }
.grid-scroll table { margin: 0; }
.grid-scroll thead th { position: sticky; top: 0; z-index: 1; }
table.pick tbody tr { cursor: pointer; }
table.pick tbody tr:hover { background: #f5f7f3; }
table.pick tbody tr.sel { background: #fcf4de; }

/* --- Excel导入列名映射面板（v0.32） --- */
.impmap {
  border: 1px solid #ecd9a6; background: #fdfaf0; border-radius: 6px;
  padding: 8px 12px; margin: 8px 0;
}

/* --- 规划中的空页面 --- */
.placeholder {
  display: flex; flex-direction: column; align-items: center; justify-content: center;
  gap: 8px; min-height: 340px; color: #9aa29c;
}
.placeholder h2 { margin: 0; color: #6b736d; }
</style>