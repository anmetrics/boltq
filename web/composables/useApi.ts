interface OverviewData {
  health: string
  stats: {
    Queues: Record<string, number>
    Topics: Record<string, number>
    DeadLetters: Record<string, number>
    PendingCount: number
  }
  metrics: Record<string, number>
  cluster: {
    enabled: boolean
    cluster?: {
      node_id: string
      raft_addr: string
      state: string
      leader: string
      leader_id: string
      term: number
      last_index: number
      peers: string[]
    }
  }
  storage: {
    mode: string
    size: number
    compaction_threshold: number
  }
  system: {
    goroutines: number
    memory: number
  }
  uptime_ms: number
}

export const useApi = () => {
  const fetchApi = async <T>(path: string, options?: any): Promise<T> => {
    return await $fetch<T>(`/api${path}`, options)
  }

  const getOverview = () => fetchApi<OverviewData>('/overview')
  const getStats = () => fetchApi<any>('/stats')
  const getHealth = () => fetchApi<{ status: string }>('/health')
  const getMetrics = () => fetchApi<Record<string, number>>('/metrics', {
    headers: { Accept: 'application/json' },
  })
  const getClusterStatus = () => fetchApi<any>('/cluster/status')

  const purgeQueue = (queue: string) => fetchApi<any>('/queues/purge', {
    method: 'POST',
    body: { queue },
  })

  const purgeDeadLetters = (queue: string) => fetchApi<any>('/dead-letters/purge', {
    method: 'POST',
    body: { queue },
  })

  const clusterJoin = (nodeId: string, addr: string) => fetchApi<any>('/cluster/join', {
    method: 'POST',
    body: { node_id: nodeId, addr },
  })

  const clusterLeave = (nodeId: string) => fetchApi<any>('/cluster/leave', {
    method: 'POST',
    body: { node_id: nodeId },
  })

  // Cache / KV Store
  const getCacheStats = () => fetchApi<any>('/cache/stats')
  const getCacheEntries = (pattern?: string, search?: string) => {
    const params = new URLSearchParams()
    if (pattern) params.set('pattern', pattern)
    if (search) params.set('search', search)
    return fetchApi<any>(`/cache/entries?${params.toString()}`)
  }
  const cacheGet = (key: string) => fetchApi<any>(`/cache/get?key=${encodeURIComponent(key)}`)
  const cacheSet = (key: string, value: any, ttl?: number) => fetchApi<any>('/cache/set', {
    method: 'POST',
    body: { key, value, ttl: ttl || 0 },
  })
  const cacheDel = (key: string) => fetchApi<any>('/cache/del', {
    method: 'POST',
    body: { key },
  })
  const cacheFlush = () => fetchApi<any>('/cache/flush', { method: 'POST' })

  return {
    getOverview,
    getStats,
    getHealth,
    getMetrics,
    getClusterStatus,
    purgeQueue,
    purgeDeadLetters,
    clusterJoin,
    clusterLeave,
    getCacheStats,
    getCacheEntries,
    cacheGet,
    cacheSet,
    cacheDel,
    cacheFlush,
  }
}
