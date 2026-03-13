<template>
  <div class="d-flex align-center ga-1">
    <template v-if="documents.length === 0">
      <span class="text-caption text-medium-emphasis">No documents</span>
    </template>
    <template v-else>
      <span
        v-for="(doc, idx) in documents"
        :key="idx"
        class="doc-icon"
        :class="`doc-icon--${doc}`"
      >
        {{ doc }}
      </span>
    </template>
  </div>
</template>

<script setup lang="ts">
import type { DocType } from '~/composables/useLabOrders'

defineProps<{
  documents: DocType[]
}>()
</script>
