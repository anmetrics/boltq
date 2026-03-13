<template>
  <v-card class="filter-bar pa-4 mb-6">
    <div class="d-flex align-center justify-space-between mb-3">
      <div class="d-flex align-center ga-2">
        <v-icon size="20" color="primary">mdi-filter-variant</v-icon>
        <span class="text-body-1 font-weight-bold">Filters</span>
      </div>
      <v-btn
        variant="text"
        color="primary"
        size="small"
        @click="$emit('clear')"
      >
        Clear All
      </v-btn>
    </div>

    <div class="d-flex flex-wrap ga-2 align-center">
      <v-checkbox
        :model-value="showOutOfScope"
        label="Show only out of scope"
        density="compact"
        hide-details
        class="flex-grow-0"
        @update:model-value="$emit('update:showOutOfScope', $event)"
      />

      <v-chip
        class="filter-chip"
        variant="outlined"
        size="small"
        prepend-icon="mdi-pound"
      >
        Job number
      </v-chip>

      <v-chip
        class="filter-chip"
        variant="outlined"
        size="small"
        prepend-icon="mdi-calendar-outline"
      >
        Due Date
      </v-chip>

      <v-chip
        class="filter-chip"
        variant="outlined"
        size="small"
        prepend-icon="mdi-calendar-outline"
      >
        Start date
      </v-chip>

      <v-chip
        class="filter-chip"
        variant="outlined"
        size="small"
        prepend-icon="mdi-pound"
      >
        Lab order number
      </v-chip>

      <v-chip
        class="filter-chip"
        variant="outlined"
        size="small"
        prepend-icon="mdi-calendar-outline"
      >
        Book in date
      </v-chip>

      <v-btn
        variant="text"
        color="primary"
        size="small"
        prepend-icon="mdi-plus"
      >
        Add filter
      </v-btn>
    </div>
  </v-card>
</template>

<script setup lang="ts">
defineProps<{
  showOutOfScope: boolean
}>()

defineEmits<{
  clear: []
  'update:showOutOfScope': [value: boolean]
}>()
</script>
