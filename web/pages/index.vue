<template>
  <v-container fluid class="pa-6">
    <div class="page-header mb-6">
      <h1>Dashboard</h1>
    </div>

    <v-row>
      <v-col cols="12" md="3">
        <v-card class="pa-4">
          <div class="text-caption text-medium-emphasis mb-1">Total Orders</div>
          <div class="text-h4 font-weight-bold">248</div>
          <div class="text-caption text-success mt-1">
            <v-icon size="14">mdi-arrow-up</v-icon> 12% from last month
          </div>
        </v-card>
      </v-col>
      <v-col cols="12" md="3">
        <v-card class="pa-4">
          <div class="text-caption text-medium-emphasis mb-1">Completed</div>
          <div class="text-h4 font-weight-bold text-success">186</div>
          <div class="text-caption text-medium-emphasis mt-1">75% completion rate</div>
        </v-card>
      </v-col>
      <v-col cols="12" md="3">
        <v-card class="pa-4">
          <div class="text-caption text-medium-emphasis mb-1">Booked In</div>
          <div class="text-h4 font-weight-bold text-info">42</div>
          <div class="text-caption text-medium-emphasis mt-1">Awaiting processing</div>
        </v-card>
      </v-col>
      <v-col cols="12" md="3">
        <v-card class="pa-4">
          <div class="text-caption text-medium-emphasis mb-1">Pending</div>
          <div class="text-h4 font-weight-bold text-warning">20</div>
          <div class="text-caption text-medium-emphasis mt-1">Requires attention</div>
        </v-card>
      </v-col>
    </v-row>

    <v-card class="mt-6 pa-6">
      <div class="text-h6 font-weight-bold mb-4">Recent Activity</div>
      <v-list>
        <v-list-item
          v-for="i in 5"
          :key="i"
          :subtitle="`Jan ${30 - i}, 2025`"
        >
          <template #prepend>
            <v-avatar size="36" color="primary" variant="tonal">
              <v-icon size="18">mdi-flask</v-icon>
            </v-avatar>
          </template>
          <v-list-item-title>Lab order {{ 36 - i }}-035628 updated</v-list-item-title>
        </v-list-item>
      </v-list>
    </v-card>
  </v-container>
</template>

<script setup lang="ts">
</script>
