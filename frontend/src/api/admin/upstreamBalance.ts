/**
 * Admin upstream relay balance API.
 *
 * 探测「上游中转站账号」在对端面板的余额（New-API / One-API 系）。
 * 与 cnProviders 的区别：那个查厂商官方端点（moonshot / deepseek），
 * 这个查第三方中转面板，账号凭证里需显式声明 upstream_protocol。
 */

import { apiClient } from '../client'

/** 上游中转站余额探测结果，对齐后端 service.UpstreamBalanceResult。 */
export interface UpstreamBalanceResult {
  account_id: number
  protocol: string
  success: boolean
  /**
   * "usd" 时 balance/used 已是美元；"quota" 时是面板内部整数单位
   * （站长改过折算率，拿不到币种语义），前端不得加 $ 前缀。
   */
  unit: 'usd' | 'quota'
  balance: number
  used: number
  /** 面板原始 quota 整数（排查用）。 */
  quota_raw: number
  status_code?: number
  fetched_at: number
  persisted: boolean
  error?: string
}

/** 查询上游中转站账号余额。 */
export async function queryUpstreamBalance(id: number): Promise<UpstreamBalanceResult> {
  const { data } = await apiClient.get<UpstreamBalanceResult>(
    `/admin/upstream-balance/accounts/${id}/balance`
  )
  return data
}

export default {
  queryUpstreamBalance
}
