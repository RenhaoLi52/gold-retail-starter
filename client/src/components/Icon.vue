<script setup lang="ts">
// 内联 SVG 线条图标（v0.30）：每个图标就是几条 path，整个组件约2KB——
// 不引图标库/字体文件的原因：体积、随画布缩放不糊、颜色跟随文字(currentColor)。
// 图标画在 24×24 网格上，描边风格统一。
withDefaults(defineProps<{ name: string; size?: number }>(), { size: 16 })

const paths: Record<string, string[]> = {
  gem: ['M6 3h12l4 6-10 12L2 9z', 'M2 9h20', 'M12 21 8 9l4-6 4 6z'],
  sale: ['M12 3v18', 'M17 6.5a4 4 0 0 0-4.5-2.3c-2.2.3-3.8 1.6-3.8 3.3 0 1.9 1.8 2.7 4.3 3.3 2.8.7 4.5 1.6 4.5 3.7 0 1.8-1.8 3.2-4.2 3.3A4.6 4.6 0 0 1 7 15.5'],
  return: ['M9 14 4 9l5-5', 'M4 9h10a6 6 0 0 1 6 6v2'],
  recycle: ['M20 11a8 8 0 1 0-2.3 6.3', 'M20 4v7h-7'],
  search: ['M11 5a6 6 0 1 0 0 12 6 6 0 0 0 0-12z', 'm15.5 15.5 5 5'],
  inbound: ['M4 17v3h16v-3', 'M12 3v11', 'm8 10 4 4 4-4'],
  outbound: ['M4 17v3h16v-3', 'M12 14V3', 'm8 6 4-4 4 4'],
  transfer: ['M4 8h13', 'm14 5 3 3-3 3', 'M20 16H7', 'm10 13-3 3 3 3'],
  stocktake: ['M9 3h6v3H9z', 'M15 4h3a1 1 0 0 1 1 1v15a1 1 0 0 1-1 1H6a1 1 0 0 1-1-1V5a1 1 0 0 1 1-1h3', 'm9 13 2 2 4-4'],
  coins: ['M7 8a5 3 0 1 0 10 0 5 3 0 1 0-10 0', 'M7 8v4c0 1.7 2.2 3 5 3s5-1.3 5-3V8', 'M7 12v4c0 1.7 2.2 3 5 3s5-1.3 5-3v-4'],
  book: ['M4 19.5A2.5 2.5 0 0 1 6.5 17H20', 'M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z'],
  users: ['M9 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8z', 'M2 21v-1a7 7 0 0 1 14 0v1', 'M16 3.5a4 4 0 0 1 0 7.4', 'M22 21v-1a7 7 0 0 0-4-6.3'],
  user: ['M12 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8z', 'M5 21v-1a7 7 0 0 1 14 0v1'],
  store: ['M4 9 5.5 4h13L20 9', 'M4 9h16v11H4z', 'M9 20v-6h6v6'],
  percent: ['M19 5 5 19', 'M7.5 4.5a2 2 0 1 0 0 4 2 2 0 0 0 0-4z', 'M16.5 15.5a2 2 0 1 0 0 4 2 2 0 0 0 0-4z'],
  chart: ['M5 20v-6', 'M10 20V8', 'M15 20v-4', 'M20 20V11', 'M3 21h18'],
  archive: ['M4 4h16v4H4z', 'M5 8v12h14V8', 'M10 12h4'],
  shield: ['M12 3l8 3v6c0 4.5-3.2 7.8-8 9-4.8-1.2-8-4.5-8-9V6z', 'm9 12 2 2 4-4'],
  settings: ['M4 6h16', 'M4 12h16', 'M4 18h16', 'M8 4a2 2 0 1 0 0 4 2 2 0 0 0 0-4z', 'M16 10a2 2 0 1 0 0 4 2 2 0 0 0 0-4z', 'M10 16a2 2 0 1 0 0 4 2 2 0 0 0 0-4z'],
  logout: ['M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4', 'm16 17 5-5-5-5', 'M21 12H9'],
}
</script>

<template>
  <svg :width="size" :height="size" viewBox="0 0 24 24" fill="none" stroke="currentColor"
    stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"
    style="vertical-align: -0.15em; flex-shrink: 0">
    <path v-for="(d, i) in paths[name] || []" :key="i" :d="d" />
  </svg>
</template>
