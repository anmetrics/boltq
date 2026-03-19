<template>
  <v-app class="app-root">
    <v-navigation-drawer permanent width="256" class="sidebar" border="0">
      <!-- Logo -->
      <div class="logo-section">
        <div class="logo-icon">
          <v-icon size="20" color="white">mdi-flash</v-icon>
        </div>
        <div class="logo-text">
          <span class="brand">BoltQ</span>
          <span class="version">Admin</span>
        </div>
      </div>

      <!-- Navigation -->
      <div class="section-header mt-2">Navigation</div>
      <v-list density="compact" nav class="sidebar-nav flex-grow-1">
        <v-list-item
          v-for="item in items"
          :key="item.to"
          :to="item.to"
          :prepend-icon="item.icon"
          rounded="lg"
          exact
          class="nav-item"
        >
          <v-list-item-title>{{ item.title }}</v-list-item-title>
        </v-list-item>
      </v-list>

      <!-- Bottom -->
      <template #append>
        <div class="sidebar-footer">
          <!-- Theme Toggle -->
          <div class="theme-toggle-row mb-3">
            <v-btn-toggle
              :model-value="isDark ? 1 : 0"
              mandatory
              density="compact"
              rounded="lg"
              class="theme-toggle"
              @update:model-value="(v: number) => { if ((v === 1) !== isDark) toggleTheme() }"
            >
              <v-btn size="small" class="theme-toggle-btn">
                <v-icon size="16">mdi-white-balance-sunny</v-icon>
              </v-btn>
              <v-btn size="small" class="theme-toggle-btn">
                <v-icon size="16">mdi-moon-waning-crescent</v-icon>
              </v-btn>
            </v-btn-toggle>
          </div>

          <!-- Status -->
          <div class="status-card">
            <div class="d-flex align-center justify-space-between">
              <div class="d-flex align-center">
                <span
                  class="status-dot mr-2"
                  :class="isOnline ? 'online' : 'offline'"
                />
                <span class="status-label">
                  {{ isOnline ? 'Connected' : 'Offline' }}
                </span>
              </div>
              <v-icon size="14" :color="isOnline ? 'success' : 'error'">
                {{ isOnline ? 'mdi-check-circle' : 'mdi-alert-circle' }}
              </v-icon>
            </div>
          </div>
        </div>
      </template>
    </v-navigation-drawer>

    <v-main>
      <div class="app-content">
        <slot />
      </div>
    </v-main>
  </v-app>
</template>

<script setup lang="ts">
const { items } = useNavigation()
const { isOnline } = useServerStatus()
const { isDark, toggleTheme, initTheme } = useAppTheme()

onMounted(() => {
  initTheme()
})
</script>

<style lang="scss" scoped>
.app-root {
  background: var(--bg-primary) !important;
}

.sidebar {
  background: var(--bg-secondary) !important;
  border-right: 1px solid var(--border-color) !important;
  display: flex;
  flex-direction: column;
  transition: background-color 0.2s ease, border-color 0.2s ease;
}

.logo-section {
  display: flex;
  align-items: center;
  padding: 20px 20px 16px;
  gap: 12px;

  .logo-icon {
    width: 36px;
    height: 36px;
    border-radius: 10px;
    background: linear-gradient(135deg, #6366f1, #8b5cf6);
    display: flex;
    align-items: center;
    justify-content: center;
    box-shadow: 0 2px 8px rgba(99, 102, 241, 0.3);
  }

  .logo-text {
    display: flex;
    flex-direction: column;
    line-height: 1.2;

    .brand {
      font-weight: 700;
      font-size: 1.1rem;
      color: var(--text-primary);
      letter-spacing: -0.02em;
    }
    .version {
      font-size: 0.7rem;
      font-weight: 500;
      color: var(--text-muted);
    }
  }
}

.sidebar-footer {
  padding: 16px;
  border-top: 1px solid var(--border-color);
}

.theme-toggle-row {
  display: flex;
  justify-content: center;
}

.theme-toggle {
  background: var(--bg-tertiary) !important;
  border: 1px solid var(--border-color) !important;
  border-radius: 10px !important;
  overflow: hidden;

  .theme-toggle-btn {
    min-width: 42px !important;
    height: 32px !important;
    border: none !important;
    background: transparent !important;
    color: var(--text-muted) !important;

    &.v-btn--active {
      background: var(--bg-secondary) !important;
      color: var(--text-primary) !important;
      box-shadow: var(--shadow-sm);
    }
  }
}

.status-card {
  padding: 10px 14px;
  background: var(--bg-tertiary);
  border-radius: 10px;
  border: 1px solid var(--border-color);
  transition: background-color 0.2s ease, border-color 0.2s ease;
}

.status-label {
  font-size: 0.8rem;
  font-weight: 500;
  color: var(--text-secondary);
}

.app-content {
  max-width: 1440px;
  margin: 0 auto;
  padding: 32px;
}
</style>
