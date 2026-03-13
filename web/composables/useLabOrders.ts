export type OrderStatus = 'Completed' | 'Booked in' | 'Unknown'
export type DocType = 'doc' | 'pdf' | 'xlsx'

export interface LabOrder {
  labOrderNumber: string
  bookInDate: string
  summary: string
  status: OrderStatus
  documents: DocType[]
}

const mockOrders: LabOrder[] = [
  { labOrderNumber: '36-035628', bookInDate: 'Feb 3, 2025', summary: '1 sample: ObjectNotFound', status: 'Completed', documents: ['doc', 'pdf', 'xlsx'] },
  { labOrderNumber: '36-035627', bookInDate: 'Feb 2, 2025', summary: '2 samples: Paprika', status: 'Booked in', documents: ['doc', 'xlsx'] },
  { labOrderNumber: '35-087456', bookInDate: 'Feb 2, 2025', summary: '4 samples: Kondensmilch', status: 'Unknown', documents: ['doc', 'xlsx'] },
  { labOrderNumber: '35-035108', bookInDate: 'Feb 1, 2025', summary: '8 samples: Pears', status: 'Booked in', documents: ['doc', 'xlsx'] },
  { labOrderNumber: '33-035628', bookInDate: 'Jan 31, 2025', summary: '1 sample: Apple', status: 'Completed', documents: [] },
  { labOrderNumber: '30-035651', bookInDate: 'Jan 30, 2025', summary: '1 sample: Pears', status: 'Booked in', documents: ['doc', 'xlsx'] },
  { labOrderNumber: '28-080968', bookInDate: 'Jan 30, 2025', summary: '4 samples: Kondensmilch', status: 'Unknown', documents: ['doc', 'xlsx'] },
  { labOrderNumber: '28-087444', bookInDate: 'Jan 29, 2025', summary: '1 sample: Paprika', status: 'Unknown', documents: ['doc', 'xlsx'] },
  { labOrderNumber: '28-043190', bookInDate: 'Jan 28, 2025', summary: '2 samples: Kondensmilch', status: 'Completed', documents: [] },
  { labOrderNumber: '21-043080', bookInDate: 'Jan 28, 2025', summary: '1 sample: ObjectNotFound', status: 'Unknown', documents: ['doc', 'xlsx'] },
  { labOrderNumber: '21-043600', bookInDate: 'Jan 27, 2025', summary: '8 samples: Apple', status: 'Completed', documents: [] },
  { labOrderNumber: '21-043614', bookInDate: 'Jan 26, 2025', summary: '2 samples: Pears', status: 'Completed', documents: [] },
  { labOrderNumber: '20-098712', bookInDate: 'Jan 25, 2025', summary: '3 samples: Tomato', status: 'Booked in', documents: ['doc', 'pdf', 'xlsx'] },
  { labOrderNumber: '20-098700', bookInDate: 'Jan 25, 2025', summary: '1 sample: Carrot', status: 'Completed', documents: ['doc'] },
  { labOrderNumber: '19-076543', bookInDate: 'Jan 24, 2025', summary: '5 samples: Spinach', status: 'Unknown', documents: ['xlsx'] },
  { labOrderNumber: '19-076500', bookInDate: 'Jan 24, 2025', summary: '2 samples: Broccoli', status: 'Booked in', documents: ['doc', 'xlsx'] },
  { labOrderNumber: '18-065432', bookInDate: 'Jan 23, 2025', summary: '4 samples: Milk', status: 'Completed', documents: ['doc', 'pdf'] },
  { labOrderNumber: '18-065400', bookInDate: 'Jan 22, 2025', summary: '1 sample: Butter', status: 'Unknown', documents: [] },
  { labOrderNumber: '17-054321', bookInDate: 'Jan 21, 2025', summary: '3 samples: Cheese', status: 'Booked in', documents: ['doc', 'xlsx'] },
  { labOrderNumber: '17-054300', bookInDate: 'Jan 20, 2025', summary: '6 samples: Yogurt', status: 'Completed', documents: ['doc', 'pdf', 'xlsx'] },
  { labOrderNumber: '16-043210', bookInDate: 'Jan 19, 2025', summary: '2 samples: Cream', status: 'Unknown', documents: ['doc'] },
  { labOrderNumber: '16-043200', bookInDate: 'Jan 18, 2025', summary: '1 sample: Honey', status: 'Booked in', documents: ['xlsx'] },
  { labOrderNumber: '15-032109', bookInDate: 'Jan 17, 2025', summary: '4 samples: Sugar', status: 'Completed', documents: ['doc', 'xlsx'] },
  { labOrderNumber: '15-032100', bookInDate: 'Jan 16, 2025', summary: '7 samples: Salt', status: 'Completed', documents: ['doc', 'pdf'] },
]

export const useLabOrders = () => {
  const orders = ref<LabOrder[]>(mockOrders)
  const totalItems = computed(() => orders.value.length)
  const viewMode = ref<'order' | 'sample'>('order')
  const showOutOfScope = ref(false)

  const filters = ref({
    jobNumber: '',
    dueDate: '',
    startDate: '',
    labOrderNumber: '',
    bookInDate: '',
  })

  const filteredOrders = computed(() => {
    let result = orders.value

    if (filters.value.labOrderNumber) {
      result = result.filter(o =>
        o.labOrderNumber.includes(filters.value.labOrderNumber)
      )
    }

    if (filters.value.bookInDate) {
      result = result.filter(o =>
        o.bookInDate.includes(filters.value.bookInDate)
      )
    }

    return result
  })

  const clearFilters = () => {
    filters.value = {
      jobNumber: '',
      dueDate: '',
      startDate: '',
      labOrderNumber: '',
      bookInDate: '',
    }
    showOutOfScope.value = false
  }

  return {
    orders,
    totalItems,
    viewMode,
    showOutOfScope,
    filters,
    filteredOrders,
    clearFilters,
  }
}
