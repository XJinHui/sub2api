import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import UpstreamBalanceCell from '../UpstreamBalanceCell.vue'
import type { Account } from '@/types'

const { queryUpstreamBalance } = vi.hoisted(() => ({
  queryUpstreamBalance: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    upstreamBalance: { queryUpstreamBalance }
  }
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => key
  })
}))

const account = {
  id: 7,
  platform: 'openai',
  type: 'apikey',
  credentials: { upstream_protocol: 'newapi' },
  extra: {
    upstream_balance: 10,
    upstream_balance_updated_at: '2026-01-01T00:00:00Z'
  }
} as Account

describe('UpstreamBalanceCell', () => {
  beforeEach(() => {
    queryUpstreamBalance.mockReset()
  })

  it('clears the stale badge after a successful manual probe', async () => {
    queryUpstreamBalance.mockResolvedValue({
      success: true,
      account_id: account.id,
      protocol: 'newapi',
      unit: 'usd',
      balance: 12,
      used: 3,
      quota_raw: 12,
      fetched_at: Math.floor(Date.now() / 1000),
      persisted: true
    })

    const wrapper = mount(UpstreamBalanceCell, { props: { account } })
    expect(wrapper.text()).toContain('admin.accounts.upstreamBalance.stale')

    await wrapper.get('[data-test="upstream-balance-probe"]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).not.toContain('admin.accounts.upstreamBalance.stale')
    expect(wrapper.get('[data-test="upstream-balance-value"]').text()).toContain('$12.00')
  })
})
