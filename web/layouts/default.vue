<template>
  <v-app>
    <v-navigation-drawer permanent width="220" color="surface">
      <!-- Logo -->
      <div class="d-flex align-center pa-4 pb-6">
        <v-avatar size="38" color="primary" class="mr-3">
          <v-icon size="22" color="black">mdi-flash</v-icon>
        </v-avatar>
        <div>
          <div class="text-body-1 font-weight-bold" style="color: #00d4aa">BoltQ</div>
          <div class="text-caption" style="opacity: 0.5">Admin Dashboard</div>
        </div>
      </div>

      <!-- Navigation -->
      <v-list density="compact" nav class="sidebar-nav">
        <v-list-item
          v-for="item in items"
          :key="item.to"
          :to="item.to"
          :prepend-icon="item.icon"
          :title="item.title"
          rounded="lg"
          exact
        />
      </v-list>

      <template #append>
        <div class="pa-3">
          <v-divider class="mb-3" style="opacity: 0.1" />
          <div class="d-flex align-center justify-center">
            <span class="status-dot mr-2" :class="isOnline ? 'online' : 'offline'" />
            <span class="text-caption" style="opacity: 0.5">
              Server {{ isOnline ? 'Connected' : 'Disconnected' }}
            </span>
          </div>
        </div>
      </template>
    </v-navigation-drawer>

    <v-main>
      <div class="pa-6">
        <slot />
      </div>
    </v-main>
  </v-app>
</template>

<script setup lang="ts">
const { items } = useNavigation()
const { isOnline } = useServerStatus()
</script>
