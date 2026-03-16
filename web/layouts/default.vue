<template>
  <v-app class="app-root">
    <!-- Main Background Glows -->
    <div class="bg-glow bg-glow-1"></div>
    <div class="bg-glow bg-glow-2"></div>

    <v-navigation-drawer 
      permanent 
      width="240" 
      class="sidebar-glass"
      border="0"
    >
      <!-- Logo Section -->
      <div class="logo-section pa-6">
        <div class="logo-container">
          <v-icon size="24" color="primary">mdi-flash-circle</v-icon>
          <div class="logo-text ml-3">
            <span class="bolt">Bolt</span><span class="q">Q</span>
            <div class="sub">INFRASTRUCTURE</div>
          </div>
        </div>
      </div>

      <!-- Navigation List -->
      <v-list density="compact" nav class="sidebar-nav flex-grow-1">
        <v-list-item
          v-for="item in items"
          :key="item.to"
          :to="item.to"
          :prepend-icon="item.icon"
          rounded="lg"
          exact
          class="nav-item mb-1"
        >
          <v-list-item-title class="nav-title">{{ item.title }}</v-list-item-title>
        </v-list-item>
      </v-list>

      <!-- Bottom Status -->
      <template #append>
        <div class="pa-6">
          <div class="status-card pa-4">
            <div class="d-flex align-center justify-space-between mb-1">
              <span class="text-caption text-muted font-weight-bold">SERVER STATUS</span>
              <span class="status-dot" :class="isOnline ? 'online' : 'offline'" />
            </div>
            <div class="text-body-2 font-weight-black" :class="isOnline ? 'text-secondary' : 'text-red'">
              {{ isOnline ? 'OPERATIONAL' : 'OFFLINE' }}
            </div>
          </div>
        </div>
      </template>
    </v-navigation-drawer>

    <v-main>
      <div class="app-content-wrapper pa-8">
        <slot />
      </div>
    </v-main>
  </v-app>
</template>

<script setup lang="ts">
const { items } = useNavigation()
const { isOnline } = useServerStatus()
</script>

<style lang="scss" scoped>
.app-root {
  background: transparent !important;
}

.bg-glow {
  position: fixed;
  width: 50vw;
  height: 50vh;
  border-radius: 50%;
  filter: blur(120px);
  z-index: 0;
  pointer-events: none;
  opacity: 0.15;
}

.bg-glow-1 {
  top: -10%;
  left: -10%;
  background: var(--primary);
}

.bg-glow-2 {
  bottom: -10%;
  right: -10%;
  background: var(--accent);
}

.sidebar-glass {
  background: rgba(10, 10, 12, 0.4) !important;
  backdrop-filter: blur(20px) !important;
  -webkit-backdrop-filter: blur(20px) !important;
  border-right: 1px solid var(--glass-border) !important;
  display: flex;
  flex-direction: column;
}

.logo-section {
  .logo-container {
    display: flex;
    align-items: center;
    
    .logo-text {
      line-height: 1;
      .bolt {
        font-weight: 800;
        font-size: 1.25rem;
        letter-spacing: -0.02em;
        color: #fff;
      }
      .q {
        font-weight: 800;
        font-size: 1.25rem;
        color: var(--primary);
      }
      .sub {
        font-size: 0.6rem;
        font-weight: 700;
        letter-spacing: 0.2em;
        color: var(--text-muted);
        margin-top: 2px;
      }
    }
  }
}

.status-card {
  background: rgba(255, 255, 255, 0.03);
  border: 1px solid var(--glass-border);
  border-radius: 12px;
}

.app-content-wrapper {
  position: relative;
  z-index: 1;
  max-width: 1600px;
  margin: 0 auto;
}

.text-muted {
  color: var(--text-muted);
}

.text-secondary {
  color: var(--secondary);
}
</style>
