<template>
  <v-container fluid class="pa-6">
    <div class="page-header mb-6">
      <h1>Sample Registration</h1>
    </div>

    <v-card class="pa-6">
      <v-form @submit.prevent="submitForm">
        <v-row>
          <v-col cols="12" md="6">
            <v-text-field
              v-model="form.labOrderNumber"
              label="Lab order number"
              placeholder="e.g. 36-035628"
            />
          </v-col>
          <v-col cols="12" md="6">
            <v-text-field
              v-model="form.jobNumber"
              label="Job number"
              placeholder="Enter job number"
            />
          </v-col>
          <v-col cols="12" md="6">
            <v-text-field
              v-model="form.sampleName"
              label="Sample name"
              placeholder="e.g. Apple, Paprika"
            />
          </v-col>
          <v-col cols="12" md="6">
            <v-text-field
              v-model="form.sampleCount"
              label="Number of samples"
              type="number"
              min="1"
            />
          </v-col>
          <v-col cols="12" md="6">
            <v-select
              v-model="form.priority"
              label="Priority"
              :items="['Normal', 'High', 'Urgent']"
            />
          </v-col>
          <v-col cols="12" md="6">
            <v-text-field
              v-model="form.dueDate"
              label="Due date"
              type="date"
            />
          </v-col>
          <v-col cols="12">
            <v-textarea
              v-model="form.notes"
              label="Notes"
              rows="3"
              placeholder="Additional notes..."
            />
          </v-col>
        </v-row>

        <div class="d-flex ga-3 mt-4">
          <v-btn color="primary" type="submit" prepend-icon="mdi-check">
            Register Sample
          </v-btn>
          <v-btn variant="outlined" color="secondary" to="/sample-submissions">
            Cancel
          </v-btn>
        </div>
      </v-form>
    </v-card>
  </v-container>
</template>

<script setup lang="ts">
const form = ref({
  labOrderNumber: '',
  jobNumber: '',
  sampleName: '',
  sampleCount: 1,
  priority: 'Normal',
  dueDate: '',
  notes: '',
})

const submitForm = () => {
  alert('Sample registered successfully!')
  navigateTo('/sample-submissions')
}
</script>
