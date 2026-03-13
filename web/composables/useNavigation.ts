export interface NavItem {
  title: string
  icon: string
  to: string
  badge?: number
}

export interface NavSection {
  title: string
  items: NavItem[]
}

export const useNavigation = () => {
  const sections: NavSection[] = [
    {
      title: 'BOARDS',
      items: [
        { title: 'Dashboard', icon: 'mdi-view-dashboard-outline', to: '/' },
        { title: 'Sample submissions', icon: 'mdi-flask-outline', to: '/sample-submissions' },
        { title: 'Results', icon: 'mdi-chart-bar', to: '/results' },
        { title: 'Quotes', icon: 'mdi-file-document-outline', to: '/quotes', badge: 6 },
        { title: 'Invoices', icon: 'mdi-receipt-text-outline', to: '/invoices' },
        { title: 'Trending', icon: 'mdi-trending-up', to: '/trending', badge: 2 },
        { title: 'Sample registration', icon: 'mdi-plus-circle-outline', to: '/sample-registration' },
      ],
    },
    {
      title: 'OTHER',
      items: [
        { title: 'Settings', icon: 'mdi-cog-outline', to: '/settings' },
        { title: 'Integrations', icon: 'mdi-puzzle-outline', to: '/integrations' },
        { title: 'Support', icon: 'mdi-help-circle-outline', to: '/support' },
      ],
    },
  ]

  return { sections }
}
