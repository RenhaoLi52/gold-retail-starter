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
  items: () => request('GET', '/api/items'),

  // 入库单：单据生命周期四件套 + 列表
  inboundSave: (payload: unknown) => request('POST', '/api/doc/inbound/save', payload),
  inboundConfirm: (id: number) => request('POST', '/api/doc/inbound/confirm', { id }),
  inboundUnconfirm: (id: number) => request('POST', '/api/doc/inbound/unconfirm', { id }),
  inboundDelete: (id: number) => request('POST', '/api/doc/inbound/delete', { id }),
  inboundList: () => request('GET', '/api/doc/inbound'),
}
