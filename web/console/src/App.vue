<script setup lang="ts">
import { useRoute } from 'vue-router'
import ConsoleLayout from './layouts/ConsoleLayout.vue'

const route = useRoute()
</script>

<template>
  <router-view v-if="route.path === '/login'" />
  <ConsoleLayout v-else>
    <router-view />
  </ConsoleLayout>
</template>
