export interface SshKey {
  id: number
  name: string
  fingerprint: string
  last_used_at: string | null
  created_at: string
}

export type PortVisibility = 'private' | 'public'

export interface InstancePort {
  port: number
  visibility: PortVisibility
  forced_private: boolean
  note: string
  url: string
  created_at: string
}

export interface SshSessionBrief {
  auth_method: 'password' | 'publickey'
  client_ip: string
  started_at: string
  ended_at: string | null
}

export interface AdminSshSession {
  id: number
  instance_id: string
  user_id: number
  machine_id: number
  auth_method: 'password' | 'publickey'
  key_id: number | null
  client_ip: string
  bytes_up: number
  bytes_down: number
  started_at: string
  last_seen_at: string
  ended_at: string | null
}
