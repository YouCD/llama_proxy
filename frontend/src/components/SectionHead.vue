<template>
  <div class="sec-head">
    <span class="sec-glow"></span>
    <h2 class="sec-t">{{ title }}</h2>
    <slot name="sub"><span v-if="sub" class="sec-sub">{{ sub }}</span></slot>
    <span class="sec-line"></span>
    <div v-if="slots.meta" class="sec-meta"><slot name="meta" /></div>
  </div>
</template>

<script setup>
import { useSlots } from 'vue'

defineProps({
  title: { type: String, required: true },
  sub: { type: String, default: '' },
})

// meta 插槽用 <template #meta v-if="..."> 条件提供, 未提供时整个 .sec-meta 不渲染
const slots = useSlots()
</script>
