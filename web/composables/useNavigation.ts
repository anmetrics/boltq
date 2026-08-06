export interface NavItem {
  title: string
  icon: string
  to: string
}

export interface NavSection {
  title: string
  items: NavItem[]
}

/**
 * Navigation is grouped because BoltQ is two independent subsystems sharing a
 * process. A flat list would put "Topics" (queue pub/sub broadcast) next to
 * "Streams" (the chat log) with nothing to say they are unrelated — which is
 * precisely the confusion worth avoiding.
 */
export const useNavigation = () => {
  const sections: NavSection[] = [
    {
      title: 'Overview',
      items: [{ title: 'Dashboard', icon: 'mdi-view-dashboard', to: '/' }],
    },
    {
      title: 'Queue',
      items: [
        { title: 'Queues', icon: 'mdi-tray-full', to: '/queues' },
        { title: 'Topics', icon: 'mdi-broadcast', to: '/topics' },
        { title: 'Dead Letters', icon: 'mdi-email-alert', to: '/dead-letters' },
      ],
    },
    {
      title: 'Messaging',
      items: [
        { title: 'Messaging', icon: 'mdi-message-text', to: '/messaging' },
        { title: 'Streams', icon: 'mdi-database-clock', to: '/streams' },
        { title: 'Cursors & Lag', icon: 'mdi-progress-clock', to: '/cursors' },
        { title: 'Presence', icon: 'mdi-account-multiple-check', to: '/presence' },
        { title: 'Gateway', icon: 'mdi-lan-connect', to: '/gateway' },
      ],
    },
    {
      title: 'System',
      items: [
        { title: 'Cache', icon: 'mdi-database-outline', to: '/cache' },
        { title: 'Cluster', icon: 'mdi-server-network', to: '/cluster' },
        { title: 'Metrics', icon: 'mdi-chart-line', to: '/metrics' },
      ],
    },
  ]

  // Flat list retained so existing consumers keep working unchanged.
  const items: NavItem[] = sections.flatMap((s) => s.items)

  return { items, sections }
}
