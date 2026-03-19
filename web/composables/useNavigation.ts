export interface NavItem {
  title: string
  icon: string
  to: string
}

export const useNavigation = () => {
  const items: NavItem[] = [
    { title: 'Dashboard', icon: 'mdi-view-dashboard', to: '/' },
    { title: 'Queues', icon: 'mdi-tray-full', to: '/queues' },
    { title: 'Topics', icon: 'mdi-broadcast', to: '/topics' },
    { title: 'Dead Letters', icon: 'mdi-email-alert', to: '/dead-letters' },
    { title: 'Cache', icon: 'mdi-database-outline', to: '/cache' },
    { title: 'Cluster', icon: 'mdi-server-network', to: '/cluster' },
    { title: 'Metrics', icon: 'mdi-chart-line', to: '/metrics' },
  ]

  return { items }
}
