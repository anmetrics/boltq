<template>
  <v-container fluid class="pa-6">
    <!-- Page header -->
    <div class="d-flex align-center justify-space-between mb-6 page-header">
      <h1>Sample submissions</h1>
      <v-btn
        color="primary"
        prepend-icon="mdi-plus"
        rounded="lg"
        to="/sample-registration"
      >
        Sample registration
      </v-btn>
    </div>

    <!-- Filters -->
    <FilterBar
      :show-out-of-scope="showOutOfScope"
      @clear="clearFilters"
      @update:show-out-of-scope="showOutOfScope = $event"
    />

    <!-- Lab orders table -->
    <LabOrdersTable
      :orders="filteredOrders"
      :total-items="totalItems"
      v-model:view-mode="viewMode"
    />
  </v-container>
</template>

<script setup lang="ts">
const {
  totalItems,
  viewMode,
  showOutOfScope,
  filteredOrders,
  clearFilters,
} = useLabOrders()
</script>
