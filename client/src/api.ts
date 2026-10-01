// 统一的 API 调用封装：自动带 token、统一抛中文错误。
// 所有接口调用都走这里，别在页面里直接写 fetch。

let token = ''

export function setToken(t: string) {
  token = t
}

async function request(method: string, path: string, body?: unknown) {
  const res = await fetch(path, {
    method,
    headers: {
      'Content-Type': 'application/json',
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const data = await res.json().catch(() => ({}))
  if (!res.ok) {
    throw new Error(data.error || `请求失败(${res.status})`)
  }
  return data
}

export const api = {
  login: (username: string, password: string) =>
    request('POST', '/api/login', { username, password }),
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

  // 用户管理（仅管理员）
  userList: () => request('GET', '/api/users'),
  userCreate: (username: string, name: string, password: string) =>
    request('POST', '/api/users', { username, name, password }),
  userSetStatus: (id: number, status: number) =>
    request('POST', '/api/users/status', { id, status }),
  userResetPassword: (id: number, newPassword: string) =>
    request('POST', '/api/users/password', { id, newPassword }),
}
