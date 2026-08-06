/**
 * Messaging subsystem API.
 *
 * Kept separate from `useApi` because the two subsystems are independent: a
 * deployment may run the work queue, the messaging backbone, or both. Mixing
 * them into one composable would make every messaging page carry queue types
 * it never uses, and would hide the fact that these endpoints simply do not
 * exist when `messaging.stream.enabled` is false.
 */

export interface PartitionStats {
  id: number
  first_seq: number
  next_seq: number
  records: number
  bytes: number
}

export interface TopicStats {
  name: string
  partitions: PartitionStats[]
  total_bytes: number
}

export interface StreamStats {
  topics: TopicStats[]
  cursors: { tracked: number }
}

export interface PresenceStats {
  users: number
  sessions: number
  by_node: Record<string, number>
  by_region: Record<string, number>
  by_state: Record<string, number>
  watchers: number
}

export interface GatewayStats {
  connections: number
  resumed: number
  auth_failures: number
  forbidden: number
  frames_in: number
  frames_out: number
  records_out: number
  slow_client_drops: number
  sessions: number
  attached: number
}

export interface SignalStats {
  published: number
  delivered: number
  dropped: number
  rate_limited: number
  topics: number
  subscribers: number
}

export interface PushStats {
  scanned: number
  suppressed: number
  sent: number
  failed: number
  dropped: number
  watching: number
}

export interface CursorInfo {
  topic: string
  partition: number
  group: string
  members: Record<string, number>
  watermark?: number
  next_seq?: number
  first_seq?: number
  lag?: number
}

export interface MessagingOverview {
  streams: StreamStats | null
  presence: PresenceStats | null
  gateway: GatewayStats | null
  signals: SignalStats | null
  push: PushStats | null
}

/** Topic namespace prefixes, mirroring internal/fanout. */
export const TOPIC_PREFIX = {
  direct: 'chat.direct.',
  group: 'chat.group.',
  inbox: 'chat.inbox.',
} as const

export type TopicKind = 'direct' | 'group' | 'inbox' | 'other'

/** Classifies a topic so the UI can group and icon it consistently. */
export function topicKind(name: string): TopicKind {
  if (name.startsWith(TOPIC_PREFIX.direct)) return 'direct'
  if (name.startsWith(TOPIC_PREFIX.group)) return 'group'
  if (name.startsWith(TOPIC_PREFIX.inbox)) return 'inbox'
  return 'other'
}

/** Strips the namespace prefix, leaving the conversation or user ID. */
export function topicSubject(name: string): string {
  for (const p of Object.values(TOPIC_PREFIX)) {
    if (name.startsWith(p)) return name.slice(p.length)
  }
  return name
}

export const useMessagingApi = () => {
  const fetchApi = async <T>(path: string, options?: any): Promise<T> => {
    return await $fetch<T>(`/api${path}`, options)
  }

  const getOverview = () => fetchApi<MessagingOverview>('/messaging/overview')
  const getStreams = () => fetchApi<StreamStats>('/streams')
  const getTopic = (name: string) =>
    fetchApi<TopicStats>(`/streams/topic?name=${encodeURIComponent(name)}`)
  const getPresence = () => fetchApi<PresenceStats>('/presence')
  const getGatewayStats = () => fetchApi<GatewayStats>('/gateway/stats')

  const getCursors = (topic: string, partition = 0, group?: string) => {
    const params = new URLSearchParams()
    params.set('topic', topic)
    params.set('partition', String(partition))
    if (group) params.set('group', group)
    return fetchApi<CursorInfo>(`/streams/cursors?${params.toString()}`)
  }

  return {
    getOverview,
    getStreams,
    getTopic,
    getPresence,
    getGatewayStats,
    getCursors,
  }
}

// --- Formatting helpers shared by the messaging pages ---

export function formatBytes(n: number): string {
  if (!n) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}

export function formatCount(n: number): string {
  if (n === undefined || n === null) return '0'
  if (n < 1000) return String(n)
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}K`
  if (n < 1_000_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  return `${(n / 1_000_000_000).toFixed(1)}B`
}

/**
 * Severity for a consumer lag value.
 *
 * The thresholds are deliberately low: for push notifications a lag of a few
 * hundred already means users are waiting on alerts, which is a product
 * problem long before it is a capacity problem.
 */
export function lagSeverity(lag?: number): 'success' | 'warning' | 'error' {
  if (lag === undefined || lag < 100) return 'success'
  if (lag < 1000) return 'warning'
  return 'error'
}
