export interface Room {
  id: string
  name: string
  floor1: string
  floor2: string
  floor3: string
  cap_end: string
  open_time: string
  capacity: number
  is_open: number
}

export interface Task {
  id: number
  user_id: number
  type: string
  mode: string
  room_id: string
  seat_id: string
  seat_num: string
  alt_seats: string
  room_name: string
  start_time: string
  duration_minutes: number
  cap_end: string
  recur_daily: boolean
  auto_renew: boolean
  status: string
  last_action: string
  last_ok: boolean
  reserve_id: number
  reserve_end_at: number
  grab_ms: number
  grab_at: number
  created_at: string
  updated_at: string
}

export interface Reserve {
  id: number
  status: number
  startTime: number
  endTime: number
  seatNum: string
  roomId: string
  firstLevelName: string
  secondLevelName: string
  thirdLevelName: string
  today: string
}

export interface QrCode {
  id: number
  room_id: string
  seat_id: string
  seat_num: string
  room_name: string
  cap_end: string
  upload_count: number
  created_at: string
  updated_at: string
}

export interface Account {
  id: number
  username: string
  seat_id?: string
  dept_id_enc?: string
  seat_id_enc?: string
  captcha_id?: string
  school?: string
  open_time?: string
  max_hours?: number
  api_style?: string
  mapp_id?: string
  hall_url?: string
  window_mode?: string
  full_day?: boolean
  base_url?: string
  login_mode?: string
  school_close?: string
  created_at?: string
}

const TOKEN_KEY = 'seatbook_token'

export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) || ''
}

export function setToken(t: string) {
  localStorage.setItem(TOKEN_KEY, t)
}

export function clearToken() {
  localStorage.removeItem(TOKEN_KEY)
}

export async function api<T = any>(path: string, options: RequestInit = {}): Promise<T> {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    ...(options.headers as Record<string, string> || {})
  }
  const token = getToken()
  if (token) headers['Authorization'] = token
  const res = await fetch(`/api${path}`, { ...options, headers })
  const data = await res.json().catch(() => ({}))
  if (!res.ok) {
    throw new Error((data as any).error || `请求失败 (${res.status})`)
  }
  return data as T
}

export function fmtTime(ms: number): string {
  if (!ms) return '-'
  return new Date(ms).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}

export const STATUS_TEXT: Record<number, string> = {
  0: '待签到', 1: '使用中', 2: '已退座', 3: '暂离中', 5: '被监督', 7: '已取消', 9: '待签到'
}
