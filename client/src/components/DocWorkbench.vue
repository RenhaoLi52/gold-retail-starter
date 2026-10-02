<script setup lang="ts">
// 通用单据工作台（v0.32，仿JMP）：所有单据模块共用的"列表+预览"骨架。
// 上=近期单据概览表（可滚动，单击选中、双击取单），下=选中单据的明细预览。
// 每个模块的差异（概览列、预览表格、操作按钮）通过 columns 属性和插槽传进来，
// 骨架本身不懂任何业务——这样七种单据只写一份列表逻辑。
import { computed, ref, watch } from 'vue'

export interface WbColumn {
  label: string
  get: (d: any) => string | number // 取值函数：怎么从单据对象取出这一列
  mono?: boolean                    // 等宽字体（单号/条码）
}

const props = defineProps<{
  title: string
  docs: any[]        // 单据数组，要求每条有 id/docNo/status
  columns: WbColumn[]
  createLabel?: string // 不传则不显示新建按钮
}>()
const emit = defineEmits<{
  refresh: []
  open: [any]   // 双击取单
  create: []
}>()

const selId = ref(0)
const sel = computed(() => props.docs.find(d => d.id === selId.value) ?? null)

// 列表刷新后保持选中；选中的单没了（被删）就落到第一张
watch(() => props.docs, v => {
  if (!v.find(d => d.id === selId.value)) selId.value = v[0]?.id ?? 0
}, { immediate: true })
</script>

<template>
  <!-- 上：单据概览 -->
  <section class="card">
    <h2>
      {{ title }}（{{ docs.length }} 张）
      <button class="mini" @click="emit('refresh')">刷新</button>
      <span class="spacer"></span>
      <slot name="toolbar"></slot>
      <button v-if="createLabel" @click="emit('create')">+ {{ createLabel }}</button>
    </h2>
    <div class="grid-scroll">
      <table class="pick">
        <thead>
          <tr>
            <th>#</th>
            <th v-for="(c, i) in columns" :key="i">{{ c.label }}</th>
            <th>状态</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(d, i) in docs" :key="d.id"
            :class="{ sel: d.id === selId }"
            @click="selId = d.id" @dblclick="emit('open', d)">
            <td>{{ i + 1 }}</td>
            <td v-for="(c, j) in columns" :key="j" :class="c.mono ? 'mono' : ''">{{ c.get(d) }}</td>
            <td><span :class="['badge', d.status === '草稿' ? 'draft' : 'ok2']">{{ d.status }}</span></td>
          </tr>
        </tbody>
      </table>
    </div>
    <p class="hint">单击一行在下方预览明细；双击打开单据页签（取单），草稿可继续编辑。</p>
  </section>

  <!-- 下：明细预览 -->
  <section class="card">
    <template v-if="sel">
      <h2>
        明细预览 <span class="mono">{{ sel.docNo }}</span>
        <span :class="['badge', sel.status === '草稿' ? 'draft' : 'ok2']">{{ sel.status }}</span>
        <slot name="headinfo" :doc="sel"></slot>
        <span class="spacer"></span>
        <slot name="actions" :doc="sel"></slot>
      </h2>
      <div class="grid-scroll preview">
        <slot name="preview" :doc="sel"></slot>
      </div>
    </template>
    <p v-else class="hint">暂无单据{{ createLabel ? `——点右上角"${createLabel}"开第一张` : '' }}。</p>
  </section>
</template>
