<template>
  <v-app>
    <v-navigation-drawer
      permanent
      width="240"
      class="sidebar"
      color="sidebar-bg"
    >
      <!-- Logo -->
      <div class="d-flex align-center pa-4 pb-2">
        <v-avatar size="36" color="primary" class="mr-3">
          <v-icon size="20" color="white">mdi-flask-round-bottom</v-icon>
        </v-avatar>
        <div>
          <div class="text-body-2 font-weight-bold" style="line-height: 1.2">Med. Pharma</div>
          <div class="text-caption text-medium-emphasis">Analytics Hub</div>
        </div>
      </div>

      <!-- Navigation sections -->
      <template v-for="(section, idx) in sections" :key="idx">
        <v-list-subheader class="mt-2">{{ section.title }}</v-list-subheader>
        <v-list density="compact" nav>
          <v-list-item
            v-for="item in section.items"
            :key="item.to"
            :to="item.to"
            :prepend-icon="item.icon"
            :title="item.title"
            rounded="lg"
          >
            <template v-if="item.badge" #append>
              <v-badge
                :content="item.badge"
                color="primary"
                inline
              />
            </template>
          </v-list-item>
        </v-list>
      </template>

      <template #append>
        <div class="pa-3">
          <v-divider class="mb-3" />
          <div class="d-flex align-center">
            <v-avatar size="36" color="teal-lighten-4" class="mr-3">
              <span class="text-caption font-weight-bold text-teal-darken-2">SM</span>
            </v-avatar>
            <div class="flex-grow-1">
              <div class="text-body-2 font-weight-medium">Sarah Milley</div>
            </div>
            <v-btn icon="mdi-unfold-more-horizontal" size="x-small" variant="text" />
          </div>
        </div>
      </template>
    </v-navigation-drawer>

    <v-main class="bg-background">
      <slot />
    </v-main>
  </v-app>
</template>

<script setup lang="ts">
const { sections } = useNavigation()
</script>
