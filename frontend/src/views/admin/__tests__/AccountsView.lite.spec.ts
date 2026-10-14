import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { DOMWrapper, flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'

import AccountsView from '../AccountsView.vue'
import AccountActionMenu from '@/components/admin/account/AccountActionMenu.vue'
import AccountTableActions from '@/components/admin/account/AccountTableActions.vue'

const {
  listAccounts,
  listWithEtag,
  getById,
  getBatchTodayStats,
  getUpstreamBillingProbeSettings,
  getAllProxies,
  getAllGroups,
  refreshCredentials,
  showError,
  showWarning,
  queryUpstreamBalance
} = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  listWithEtag: vi.fn(),
  getById: vi.fn(),
  getBatchTodayStats: vi.fn(),
  getUpstreamBillingProbeSettings: vi.fn(),
  getAllProxies: vi.fn(),
  getAllGroups: vi.fn(),
  refreshCredentials: vi.fn(),
  showError: vi.fn(),
  showWarning: vi.fn(),
  queryUpstreamBalance: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list: listAccounts,
      getById,
      listWithEtag,
      getBatchTodayStats,
      getUpstreamBillingProbeSettings,
      delete: vi.fn(),
      batchClearError: vi.fn(),
      batchRefresh: vi.fn(),
      toggleSchedulable: vi.fn(),
      refreshCredentials
    },
    proxies: { getAll: getAllProxies },
    upstreamBalance: { queryUpstreamBalance },
    groups: { getAll: getAllGroups }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showWarning, showSuccess: vi.fn(), showInfo: vi.fn() })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ token: 'test-token', isSimpleMode: false })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const DataTableStub = defineComponent({
  props: { data: { type: Array, default: () => [] } },
  template: `
    <div>
      <div v-for="row in data" :key="row.id" :data-account-name="row.name">
        <slot name="cell-groups" :row="row" />
        <slot name="cell-actions" :row="row" />
        <slot name="cell-usage" :row="row" />
      </div>
    </div>
  `
})

const AccountGroupsCellStub = defineComponent({
  props: { groups: { type: Array, default: () => [] } },
  template: '<span data-test="account-groups">{{ groups.map(group => group.name).join(",") }}</span>'
})

const EditAccountModalStub = defineComponent({
  props: { show: Boolean, account: { type: Object, default: null } },
  template: '<div data-test="edit-account">{{ show ? account?.name : "" }}</div>'
})

const AccountTestModalStub = defineComponent({
  props: { show: Boolean, account: { type: Object, default: null } },
  template: '<div data-test="test-account">{{ show ? account?.name : "" }}</div>'
})

const AccountStatsModalStub = defineComponent({
  props: { show: Boolean, account: { type: Object, default: null } },
  template: '<div data-test="stats-account">{{ show ? account?.name : "" }}</div>'
})

function mountView(stubActionMenu = true) {
  return mount(AccountsView, {
    attachTo: document.body,
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: { template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>' },
        DataTable: DataTableStub,
        AccountTableActions: { template: '<div><slot name="after" /></div>' },
        AccountTableFilters: true,
        AccountBulkActionsBar: true,
        Pagination: true,
        ConfirmDialog: true,
        AccountActionMenu: stubActionMenu,
        ImportDataModal: true,
        ReAuthAccountModal: true,
        AccountTestModal: AccountTestModalStub,
        AccountStatsModal: AccountStatsModalStub,
        ScheduledTestsPanel: true,
        SyncFromCrsModal: true,
        TempUnschedStatusModal: true,
        ErrorPassthroughRulesModal: true,
        TLSFingerprintProfilesModal: true,
        CreateAccountModal: true,
        EditAccountModal: EditAccountModalStub,
        BulkEditAccountModal: true,
        PlatformTypeBadge: true,
        AccountCapacityCell: true,
        AccountStatusIndicator: true,
        AccountTodayStatsCell: true,
        AccountGroupsCell: AccountGroupsCellStub,
        AccountUsageCell: {
          props: ['account'],
          template: '<span data-test="balance">{{ account.extra?.upstream_balance }} {{ account.extra?.upstream_balance_error }}</span>'
        },
        UpstreamBillingRateCell: true,
        HelpTooltip: true,
        Icon: true,
        Teleport: stubActionMenu
      }
    }
  })
}

const listRow = {
  id: 42,
  name: 'compact row',
  platform: 'openai',
  type: 'oauth',
  status: 'active',
  schedulable: true,
  concurrency: 2,
  priority: 1,
  group_ids: [7],
  extra: {},
  credentials: {}
}

const fullAccount = {
  ...listRow,
  groups: [{ id: 7, name: 'codex', platform: 'openai' }],
  account_groups: [{ account_id: 42, group_id: 7 }],
  credentials: { api_key: 'redacted' },
  extra: { detail_only: true }
}

describe('admin AccountsView lite account list', () => {
  beforeEach(() => {
    localStorage.clear()
    listAccounts.mockReset().mockResolvedValue({ items: [listRow], total: 1, page: 1, page_size: 20, pages: 1 })
    listWithEtag.mockReset().mockResolvedValue({ notModified: true, etag: 'compact-etag', data: null })
    getById.mockReset().mockResolvedValue(fullAccount)
    getBatchTodayStats.mockReset().mockResolvedValue({ stats: {} })
    getUpstreamBillingProbeSettings.mockReset().mockResolvedValue({ enabled: true })
    getAllProxies.mockReset().mockResolvedValue([])
    getAllGroups.mockReset().mockResolvedValue([{ id: 7, name: 'codex', platform: 'openai' }])
    refreshCredentials.mockReset()
    showError.mockReset()
    showWarning.mockReset()
    queryUpstreamBalance.mockReset().mockImplementation((id: number) => Promise.resolve({
      success: true, account_id: id, protocol: 'sub2api', unit: 'usd',
      balance: 12, used: 3, fetched_at: Math.floor(Date.now() / 1000), persisted: true
    }))
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('keeps lite=1 on the initial list request', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(listAccounts).toHaveBeenCalledWith(
      1,
      20,
      expect.objectContaining({ lite: '1' }),
      expect.objectContaining({ signal: expect.any(AbortSignal) })
    )
    wrapper.unmount()
  })

  it('maps group_ids through the group catalog for the table cell', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-test="account-groups"]').text()).toBe('codex')
    wrapper.unmount()
  })

  it('keeps the action menu open during internal scrolling but closes it on table scrolling', async () => {
    const wrapper = mountView(false)
    await flushPromises()

    const trigger = wrapper.findAll('button').find(button => button.text() === 'common.more')!
    await trigger.trigger('click')
    const menu = new DOMWrapper(document.body.querySelector('.action-menu-content')!)
    menu.element.dispatchEvent(new Event('scroll'))
    await flushPromises()
    expect(wrapper.findComponent(AccountActionMenu).props('show')).toBe(true)

    menu.get('button').element.dispatchEvent(new Event('scroll'))
    await flushPromises()
    expect(wrapper.findComponent(AccountActionMenu).props('show')).toBe(true)

    wrapper.getComponent(DataTableStub).element.dispatchEvent(new Event('scroll'))
    await flushPromises()
    expect(wrapper.findComponent(AccountActionMenu).props('show')).toBe(false)
    wrapper.unmount()
  })

  it('keeps lite=1 on automatic ETag refreshes', async () => {
    vi.useFakeTimers()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    localStorage.setItem('account-auto-refresh', JSON.stringify({ enabled: true, interval_seconds: 5 }))
    const wrapper = mountView()
    await flushPromises()

    await vi.advanceTimersByTimeAsync(6000)
    await flushPromises()

    expect(listWithEtag).toHaveBeenCalledWith(
      1,
      20,
      expect.objectContaining({ lite: '1' }),
      expect.objectContaining({ etag: null })
    )
    wrapper.unmount()
  })

  it('queries both relay protocols even when the automatic list response is 304', async () => {
    vi.useFakeTimers()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    localStorage.setItem('account-auto-refresh', JSON.stringify({ enabled: true, interval_seconds: 5 }))
    listAccounts.mockResolvedValue({ items: [listRow, ...['sub2api', 'newapi'].map((protocol, index) => ({
      ...listRow, id: index + 1, type: 'apikey', credentials: { upstream_protocol: protocol },
      extra: { upstream_balance: 9, upstream_balance_error: 'old error' }
    }))], total: 3, pages: 1 })
    const wrapper = mountView()
    await flushPromises()
    expect(queryUpstreamBalance).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(6000)
    await flushPromises()
    expect(queryUpstreamBalance.mock.calls.map(call => call[0])).toEqual([1, 2])
    expect(wrapper.findAll('[data-test="balance"]').map(cell => cell.text())).toEqual(['', '12', '12'])
    wrapper.unmount()
  })

  it('runs relay queries serially without overlapping rounds and aborts on unmount', async () => {
    vi.useFakeTimers()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    localStorage.setItem('account-auto-refresh', JSON.stringify({ enabled: true, interval_seconds: 5 }))
    listAccounts.mockResolvedValue({ items: [1, 2].map(id => ({
      ...listRow, id, type: 'apikey', credentials: { upstream_protocol: 'sub2api' }
    })), total: 2, pages: 1 })
    let resolveFirst!: (value: unknown) => void
    let resolveSecond!: (value: unknown) => void
    queryUpstreamBalance.mockImplementationOnce(() => new Promise(resolve => { resolveFirst = resolve }))
      .mockImplementationOnce(() => new Promise(resolve => { resolveSecond = resolve }))
    const wrapper = mountView()
    await flushPromises()
    // Do not await the asynchronous timer while the deliberately slow query is pending.
    vi.advanceTimersByTime(6000)
    await flushPromises()
    expect(queryUpstreamBalance).toHaveBeenCalledTimes(1)
    vi.advanceTimersByTime(20000)
    await flushPromises()
    expect(queryUpstreamBalance).toHaveBeenCalledTimes(1)
    resolveFirst({ success: true, fetched_at: Math.floor(Date.now() / 1000), balance: 4 })
    await flushPromises()
    expect(queryUpstreamBalance.mock.calls.map(call => call[0])).toEqual([1, 2])
    const signal = queryUpstreamBalance.mock.calls[1][1].signal as AbortSignal
    wrapper.unmount()
    expect(signal.aborted).toBe(true)
    resolveSecond({ success: true, fetched_at: Math.floor(Date.now() / 1000), balance: 4 })
    await flushPromises()
    expect(queryUpstreamBalance).toHaveBeenCalledTimes(2)
  })

  it('retains the previous balance on failures and continues refreshing the next relay', async () => {
    vi.useFakeTimers()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    localStorage.setItem('account-auto-refresh', JSON.stringify({ enabled: true, interval_seconds: 5 }))
    listAccounts.mockResolvedValue({ items: [1, 2, 3].map(id => ({
      ...listRow, id, type: 'apikey', credentials: { upstream_protocol: 'sub2api' },
      extra: { upstream_balance: 9 }
    })), total: 3, pages: 1 })
    queryUpstreamBalance.mockResolvedValueOnce({ success: false, fetched_at: Math.floor(Date.now() / 1000), balance: 0, error: 'HTTP 401' })
      .mockRejectedValueOnce(new Error('timeout'))
    const wrapper = mountView()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(6000)
    await flushPromises()
    expect(wrapper.findAll('[data-test="balance"]').map(cell => cell.text())).toEqual(['9 HTTP 401', '9 timeout', '12'])
    wrapper.unmount()
  })

  it('pauses automatic relay polling while the page is hidden', async () => {
    vi.useFakeTimers()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(true)
    localStorage.setItem('account-auto-refresh', JSON.stringify({ enabled: true, interval_seconds: 5 }))
    listAccounts.mockResolvedValue({ items: [{ ...listRow, type: 'apikey', credentials: { upstream_protocol: 'sub2api' } }], total: 1, pages: 1 })
    const wrapper = mountView()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(10000)
    expect(queryUpstreamBalance).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('refreshes relay balances from the toolbar even with automatic refresh disabled', async () => {
    listAccounts.mockResolvedValue({ items: [{ ...listRow, type: 'apikey', credentials: { upstream_protocol: 'sub2api' } }], total: 1, pages: 1 })
    const wrapper = mountView()
    await flushPromises()
    expect(queryUpstreamBalance).not.toHaveBeenCalled()
    wrapper.findComponent(AccountTableActions).vm.$emit('refresh')
    await flushPromises()
    expect(queryUpstreamBalance).toHaveBeenCalledWith(42, { signal: expect.any(AbortSignal) })
    expect(wrapper.get('[data-test="balance"]').text()).toBe('12')
    wrapper.unmount()
  })

  it('loads the full account by id before opening edit, test, and stats actions', async () => {
    const wrapper = mountView()
    await flushPromises()

    const editButton = wrapper.findAll('button').find(button => button.text().includes('common.edit'))
    expect(editButton).toBeTruthy()
    await editButton!.trigger('click')
    await flushPromises()
    expect(getById).toHaveBeenCalledWith(42)
    expect(wrapper.get('[data-test="edit-account"]').text()).toBe('compact row')

    const menu = wrapper.findComponent(AccountActionMenu)
    menu.vm.$emit('test', listRow)
    await flushPromises()
    expect(getById).toHaveBeenCalledTimes(2)
    expect(wrapper.get('[data-test="test-account"]').text()).toBe('compact row')

    menu.vm.$emit('stats', listRow)
    await flushPromises()
    expect(getById).toHaveBeenCalledTimes(3)
    expect(wrapper.get('[data-test="stats-account"]').text()).toBe('compact row')
    wrapper.unmount()
  })

  it('shows the warning and patches the account after a partial Antigravity refresh', async () => {
    refreshCredentials.mockResolvedValue({
      account: { ...fullAccount, name: 'refreshed account' },
      message: 'Token refreshed, but project_id is temporarily unavailable',
      warning: 'missing_project_id_temporary'
    })
    const wrapper = mountView(false)
    await flushPromises()

    wrapper.findComponent(AccountActionMenu).vm.$emit('refresh-token', listRow)
    await flushPromises()

    expect(refreshCredentials).toHaveBeenCalledWith(42)
    expect(wrapper.get('[data-account-name]').attributes('data-account-name')).toBe('refreshed account')
    expect(showWarning).toHaveBeenCalledWith('Token refreshed, but project_id is temporarily unavailable')
    wrapper.unmount()
  })

  it('shows an error and keeps the modal closed when detail loading fails', async () => {
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {})
    getById.mockRejectedValueOnce(new Error('detail failed'))
    const wrapper = mountView()
    await flushPromises()

    const editButton = wrapper.findAll('button').find(button => button.text().includes('common.edit'))
    await editButton!.trigger('click')
    await flushPromises()

    expect(showError).toHaveBeenCalledWith('detail failed')
    expect(wrapper.get('[data-test="edit-account"]').text()).toBe('')
    consoleError.mockRestore()
    wrapper.unmount()
  })
})
