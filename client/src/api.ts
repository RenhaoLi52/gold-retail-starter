// 统一的 API 调用封装：自动带 token、统一抛中文错误。
// 所有接口调用都走这里，别在页面里直接写 fetch。

// 令牌持久化（v0.13）：JWT 本身有效期12小时，但之前只存在内存变量里，
// 一刷新页面变量清零就"被登出"了。现在同步存进 localStorage（本地小仓库，
// 刷新/重开窗口都不丢），启动时先从仓库恢复。
const TOKEN_KEY = 'gold_token'

function loadSavedToken(): string {
  try {
    return localStorage.getItem(TOKEN_KEY) || ''
  } catch {
    return '' // 某些受限环境没有 localStorage，退化为纯内存模式
  }
}

let token = loadSavedToken()

export function setToken(t: string) {
  token = t
  try {
    if (t) localStorage.setItem(TOKEN_KEY, t)
    else localStorage.removeItem(TOKEN_KEY)
  } catch {}
}

export function hasToken() {
  return !!token
}

export function clearToken() {
  setToken('')
}

// API 基础地址：
// - 浏览器/Electron开发模式：页面由 Vite 提供（http://），走相对路径，由 Vite 代理转给后端。
// - Electron 打包模式：页面是本地文件（file://），没有代理，必须写全后端地址。
const API_BASE = location.protocol === 'file:' ? 'http://localhost:8080' : ''

async function request(method: string, path: string, body?: unknown) {
  const res = await fetch(API_BASE + path, {
    method,
    headers: {
      'Content-Type': 'application/json',
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const data = await res.json().catch(() => ({}))
  if (!res.ok) {
    // 401 = 令牌过期或账号被禁用：把本地保存的令牌清掉，避免下次启动还拿着废令牌
    if (res.status === 401) clearToken()
    throw new Error(data.error || `请求失败(${res.status})`)
  }
  return data
}

export const api = {
  login: (username: string, password: string) =>
    request('POST', '/api/login', { username, password }),
  me: () => request('GET', '/api/me'),
  items: (params?: Record<string, string>) => {
    const qs = params ? '?' + new URLSearchParams(params).toString() : ''
    return request('GET', '/api/items' + qs)
  },

  // 入库单：单据生命周期四件套 + 列表
  inboundSave: (payload: unknown) => request('POST', '/api/doc/inbound/save', payload),
  inboundConfirm: (id: number) => request('POST', '/api/doc/inbound/confirm', { id }),
  inboundUnconfirm: (id: number) => request('POST', '/api/doc/inbound/unconfirm', { id }),
  inboundDelete: (id: number) => request('POST', '/api/doc/inbound/delete', { id }),
  inboundList: () => request('GET', '/api/doc/inbound'),
  outboundSave: (payload: unknown) => request('POST', '/api/doc/outbound/save', payload),
  outboundConfirm: (id: number) => request('POST', '/api/doc/outbound/confirm', { id }),
  outboundUnconfirm: (id: number) => request('POST', '/api/doc/outbound/unconfirm', { id }),
  outboundDelete: (id: number) => request('POST', '/api/doc/outbound/delete', { id }),
  outboundList: () => request('GET', '/api/doc/outbound'),

  saleSave: (payload: unknown) => request('POST', '/api/doc/sale/save', payload),
  saleConfirm: (id: number) => request('POST', '/api/doc/sale/confirm', { id }),
  saleUnconfirm: (id: number) => request('POST', '/api/doc/sale/unconfirm', { id }),
  saleDelete: (id: number) => request('POST', '/api/doc/sale/delete', { id }),
  saleList: () => request('GET', '/api/doc/sale'),

  changePassword: (oldPassword: string, newPassword: string) =>
    request('POST', '/api/me/password', { oldPassword, newPassword }),

  // 金价
  goldPriceCurrent: () => request('GET', '/api/gold-price/current'),
  goldPriceHistory: () => request('GET', '/api/gold-price/history'),
  goldPricePublish: (purity: string, retailPrice: number, recyclePrice: number) =>
    request('POST', '/api/gold-price', { purity, retailPrice, recyclePrice }),

  // 基础资料字典
  dictList: (type: string) => request('GET', `/api/dict?type=${type}`),
  dictCreate: (payload: unknown) => request('POST', '/api/dict', payload),
  dictUpdate: (payload: unknown) => request('POST', '/api/dict/update', payload),

  // 售货员档案（读取所有人；增改仅管理员）
  salespersonList: () => request('GET', '/api/salespersons'),
  salespersonCreate: (name: string, sort: number) =>
    request('POST', '/api/salespersons', { name, sort }),
  salespersonUpdate: (payload: unknown) => request('POST', '/api/salespersons/update', payload),

  // 用户管理（仅管理员）
  userList: () => request('GET', '/api/users'),
  userCreate: (username: string, name: string, password: string) =>
    request('POST', '/api/users', { username, name, password }),
  userSetStatus: (id: number, status: number) =>
    request('POST', '/api/users/status', { id, status }),
  userResetPassword: (id: number, newPassword: string) =>
    request('POST', '/api/users/password', { id, newPassword }),
}
