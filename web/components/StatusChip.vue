<template>
  <v-chip
    :class="chipClass"
    size="small"
    label
  >
    <v-icon start size="12" :color="dotColor">mdi-circle</v-icon>
    {{ status }}
  </v-chip>
</template>

<script setup lang="ts">
import type { OrderStatus } from '~/composables/useLabOrders'

const props = defineProps<{
  status: OrderStatus
}>()

const chipClass = computed(() => {
  const base = 'status-chip'
  switch (props.status) {
    case 'Completed': return `${base} ${base}--completed`
    case 'Booked in': return `${base} ${base}--booked-in`
    default: return `${base} ${base}--unknown`
  }
})

const dotColor = computed(() => {
  switch (props.status) {
    case 'Completed': return '#10B981'
    case 'Booked in': return '#34D399'
    default: return '#94A3B8'
  }
})
</script>
