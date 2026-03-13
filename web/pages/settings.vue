<template>
  <v-container fluid class="pa-6">
    <div class="page-header mb-6">
      <h1>Settings</h1>
    </div>
    <v-card class="pa-6">
      <v-list>
        <v-list-subheader>General</v-list-subheader>
        <v-list-item title="Organization name" subtitle="Med. Pharma Analytics Hub">
          <template #append>
            <v-btn variant="text" size="small" color="primary">Edit</v-btn>
          </template>
        </v-list-item>
        <v-list-item title="Timezone" subtitle="Europe/Berlin (UTC+1)">
          <template #append>
            <v-btn variant="text" size="small" color="primary">Edit</v-btn>
          </template>
        </v-list-item>
        <v-divider class="my-2" />
        <v-list-subheader>Notifications</v-list-subheader>
        <v-list-item title="Email notifications">
          <template #append>
            <v-switch hide-details density="compact" color="primary" />
          </template>
        </v-list-item>
        <v-list-item title="Status update alerts">
          <template #append>
            <v-switch hide-details density="compact" color="primary" model-value />
          </template>
        </v-list-item>
      </v-list>
    </v-card>
  </v-container>
</template>
