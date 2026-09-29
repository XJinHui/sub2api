<template>
  <div v-if="visible" class="space-y-1">
    <!-- 余额展示：快照优先生效，探测成功后覆盖 -->
    <div class="flex flex-wrap items-center gap-1.5">
      <span
        data-test="upstream-balance-value"
        class="text-[10px] font-medium leading-4 text-emerald-700 dark:text-emerald-400"
        :title="tooltipText"
      >
        {{ balanceLabel }}
      </span>

      <!-- 快照过期提示（周期探测停摆或对端改了折算率时，别让旧数字看起来是新的） -->
      <span
        v-if="snapshotStale"
        class="inline-flex items-center rounded bg-amber-100 px-1 py-0.5 text-[10px] font-medium text-amber-700 dark:bg-amber-900/30 dark:text-amber-300"
        :title="t('admin.accounts.upstreamBalance.staleTooltip')"
      >
        {{ t('admin.accounts.upstreamBalance.stale') }}
      </span>
    </div>

    <div class="flex flex-wrap items-center gap-1.5">
      <button
        type="button"
        data-test="upstream-balance-probe"
        class="inline-flex items-center gap-0.5 whitespace-nowrap rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-blue-600 transition-colors hover:bg-blue-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-blue-400 dark:hover:bg-blue-900/30"
        :disabled="loading"
        :title="t('admin.accounts.upstreamBalance.probeTooltip')"
        @click="handleProbe"
      >
        <svg
          class="h-2.5 w-2.5"
          :class="{ 'animate-spin': loading }"
          fill="none"
          stroke="currentColor"
          viewBox="0 0 24 24"
        >
          <path
            stroke-linecap="round"
            stroke-linejoin="round"
            stroke-width="2"
            d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15"
          />
        </svg>
        {{ t('admin.accounts.upstreamBalance.probe') }}
      </button>

      <!-- 已用额度（有值时显示，便于判断对端是否在放量） -->
      <span v-if="usedLabel" class="text-[10px] leading-4 text-gray-400" :title="tooltipText">
        {{ usedLabel }}
      </span>
    </div>

    <div v-if="error" class="truncate text-[10px] text-red-600 dark:text-red-400" :title="error">
      {{ truncatedError }}
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { UpstreamBalanceResult } from '@/api/admin/upstreamBalance'
import type { Account } from '@/types'
import { upstreamBalanceCellVisible } from './credentialsBuilder'

const props = defineProps<{
  account: Account
}>()

const { t } = useI18n()

// 快照超过这个时长就标记为「旧」——与后端周期探测的默认间隔同量级。
const SNAPSHOT_STALE_MS = 30 * 60 * 1000

const visible = computed(() =>
  upstreamBalanceCellVisible(props.account.type, props.account.credentials)
)

const loading = ref(false)
const error = ref<string | null>(null)
const data = ref<UpstreamBalanceResult | null>(null)

// Extra 快照键（后端 UpstreamBalanceService.persist 写入）。
const snapshotBalance = computed(() => {
  const v = props.account.extra?.upstream_balance
  return typeof v === 'number' ? v : null
})
const snapshotUsed = computed(() => {
  const v = props.account.extra?.upstream_balance_used
  return typeof v === 'number' ? v : null
})
const snapshotUnit = computed(() => {
  const v = props.account.extra?.upstream_balance_unit
  return v === 'usd' || v === 'quota' ? v : 'quota'
})
const snapshotError = computed(() => {
  const v = props.account.extra?.upstream_balance_error
  return typeof v === 'string' && v !== '' ? v : ''
})
const snapshotUpdatedAt = computed(() => {
  const v = props.account.extra?.upstream_balance_updated_at
  if (typeof v !== 'string') return null
  const ts = Date.parse(v)
  return Number.isNaN(ts) ? null : ts
})

const snapshotStale = computed(() => {
  if (snapshotBalance.value == null) return false
  if (snapshotUpdatedAt.value == null) return true
  return Date.now() - snapshotUpdatedAt.value > SNAPSHOT_STALE_MS
})

// 探测结果优先于快照。
const currentUnit = computed(() => (data.value?.success ? data.value.unit : snapshotUnit.value))
const hasValue = computed(() =>
  data.value?.success ? true : snapshotBalance.value != null
)

// unit === 'quota' 时不能当美元：面板折算率被改过，我们不知道币种语义，
// 只能按「额度单位」如实展示，绝不加 $ 前缀编一个金额。
const formatAmount = (amount: number, unit: string): string => {
  if (unit === 'usd') {
    const fixed = Math.abs(amount) >= 100 ? amount.toFixed(0) : amount.toFixed(2)
    return `$${fixed}`
  }
  return `${amount.toLocaleString('en-US', { maximumFractionDigits: 0 })} ${t(
    'admin.accounts.upstreamBalance.quotaUnit'
  )}`
}

const balanceLabel = computed(() => {
  if (!hasValue.value) return t('admin.accounts.upstreamBalance.balance')
  const amount = data.value?.success ? data.value.balance : (snapshotBalance.value as number)
  return formatAmount(amount, currentUnit.value)
})

const usedLabel = computed(() => {
  const used = data.value?.success ? data.value.used : snapshotUsed.value
  if (used == null || used === 0) return ''
  return `${t('admin.accounts.upstreamBalance.used')} ${formatAmount(used, currentUnit.value)}`
})

const tooltipText = computed(() => {
  const parts: string[] = [t('admin.accounts.upstreamBalance.probeTooltip')]
  if (snapshotUpdatedAt.value != null) {
    parts.push(
      `${t('admin.accounts.upstreamBalance.updatedAt')} ${new Date(
        snapshotUpdatedAt.value
      ).toLocaleString()}`
    )
  }
  if (currentUnit.value === 'quota') parts.push(t('admin.accounts.upstreamBalance.quotaUnitHint'))
  return parts.join(' · ')
})

const extractErrorMessage = (e: unknown): string => {
  const err = e as {
    message?: string
    reason?: string
    response?: { data?: { message?: string; error?: string } }
  }
  return (
    err?.message ||
    err?.reason ||
    err?.response?.data?.message ||
    err?.response?.data?.error ||
    t('common.error')
  )
}

const truncatedError = computed(() => {
  if (!error.value) return ''
  return error.value.length > 80 ? `${error.value.slice(0, 80)}...` : error.value
})

const handleProbe = async () => {
  if (loading.value) return
  loading.value = true
  error.value = null
  try {
    const result = await adminAPI.upstreamBalance.queryUpstreamBalance(props.account.id)
    // 失败时保留快照展示（仅显示错误行），成功才覆盖。
    if (result.success) {
      data.value = result
    } else {
      error.value = result.error || t('common.error')
    }
  } catch (e) {
    error.value = extractErrorMessage(e)
  } finally {
    loading.value = false
  }
}

// 展示持久化的失败原因（例如对端不是 New-API 面板），这是 UI 知道「为什么
// 这行没有数字」的唯一途径。
watch(
  snapshotError,
  (v) => {
    if (v && !error.value) error.value = v
  },
  { immediate: true }
)

watch(
  () => props.account.id,
  () => {
    data.value = null
    error.value = null
    loading.value = false
  }
)
</script>
