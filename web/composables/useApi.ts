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
  }
}
