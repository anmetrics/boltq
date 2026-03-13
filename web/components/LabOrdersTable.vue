<template>
  <v-card class="lab-orders-table">
    <!-- Table header -->
    <div class="d-flex align-center justify-space-between pa-4 pb-2">
      <div class="d-flex align-center ga-2">
        <v-icon size="20" color="success">mdi-check-circle-outline</v-icon>
        <span class="text-body-1 font-weight-bold">Lab orders</span>
        <span class="text-caption text-medium-emphasis ml-1">{{ totalItems }} items in total</span>
      </div>

      <div class="d-flex align-center ga-2">
        <v-btn icon="mdi-download-outline" size="small" variant="text" color="secondary" />
        <v-btn icon="mdi-cog-outline" size="small" variant="text" color="secondary" />

        <v-btn-toggle
          v-model="internalViewMode"
          mandatory
          density="compact"
          class="view-toggle"
          rounded="lg"
          variant="outlined"
          divided
        >
          <v-btn value="order" size="small">By order</v-btn>
          <v-btn value="sample" size="small">By sample</v-btn>
        </v-btn-toggle>
      </div>
    </div>

    <!-- Table -->
    <v-table hover>
      <thead>
        <tr>
          <th class="text-left">Lab order number</th>
          <th class="text-left">Book in date</th>
          <th class="text-left">Summary</th>
          <th class="text-left">Status</th>
          <th class="text-left">Documents</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="order in orders" :key="order.labOrderNumber">
          <td class="font-weight-medium">{{ order.labOrderNumber }}</td>
          <td>{{ order.bookInDate }}</td>
          <td>{{ order.summary }}</td>
          <td>
            <StatusChip :status="order.status" />
          </td>
          <td>
            <DocumentIcons :documents="order.documents" />
          </td>
        </tr>
      </tbody>
    </v-table>
  </v-card>
</template>

<script setup lang="ts">
import type { LabOrder } from '~/composables/useLabOrders'

const props = defineProps<{
  orders: LabOrder[]
  totalItems: number
  viewMode: 'order' | 'sample'
}>()

const emit = defineEmits<{
  'update:viewMode': [value: 'order' | 'sample']
}>()

const internalViewMode = computed({
  get: () => props.viewMode,
  set: (val) => emit('update:viewMode', val),
})
</script>
