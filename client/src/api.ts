// 统一的 API 调用封装：
// 1. 自动带上登录 token
// 2. 后端返回 {error: "..."} 时统一抛出中文错误
// 以后所有接口调用都走这里，别在页面里直接写 fetch。

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
  inboundList: () => request('GET', '/api/doc/inbound'),
  inboundCreate: (payload: unknown) => request('POST', '/api/doc/inbound/create', payload),
}
