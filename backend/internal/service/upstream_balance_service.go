package service

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/tidwall/gjson"
	"golang.org/x/sync/singleflight"
)

// 上游中转站账号余额探测服务。
//
// 与 cn_provider_balance_service.go 的区别：那个服务查的是**厂商官方**端点
// （moonshot / deepseek），端点由平台固定或由 base_url 衍生；本服务查的是
// **第三方中转面板**的余额端点，端点路径随面板程序而异，所以协议可配。
//
// 已覆盖协议：
//   - New-API / One-API 系：GET {base}/api/user/self  (Authorization: Bearer <数据面 key>)
//     → data.quota（剩余额度）/ data.used_quota（已用额度）
//
//   - sub2api 自身：**无 API key 读法**。其管理面（/api/v1/user/profile、
//     /api/v1/user/platform-quotas）全部挂在 JWTAuth 之下（internal/server/routes/user.go:26），
//     只用数据面 key 调用一律 401。若上游确实是另一台 sub2api，本服务会返回
//     UPSTREAM_BALANCE_BAD_RESPONSE（拿不到 data.quota），而不是静默出 0 —— 见 readme 说明。
//
// 单位：New-API 的 quota 是**内部整数单位**，默认 500000 = $1（quota_per_unit），
// 且站长可改。拿不到换算率时 result.Unit="quota"，调用方必须按「额度单位」展示，
// 不得当成美元。
const (
	upstreamBalanceTimeout    = 15 * time.Second
	upstreamBalanceMaxBody    = 256 * 1024
	upstreamDefaultQuotaPerUnit = 500000.0

	// Extra 快照键（前缀 upstream_，与 cn 供应商的 <provider>_balance 区分，
	// 避免同一个账号两种探测互相覆盖）。
	upstreamExtraBalance    = "upstream_balance"
	upstreamExtraUsed       = "upstream_balance_used"
	upstreamExtraUnit       = "upstream_balance_unit" // "usd" | "quota"
	upstreamExtraUpdated    = "upstream_balance_updated_at"
	upstreamExtraError      = "upstream_balance_error"
	upstreamExtraProtocol   = "upstream_balance_protocol"
)

// 上游中转面板协议标识。
const (
	UpstreamProtocolNewAPI = "newapi"
	// UpstreamProtocolSub2API 走网关面的 GET /v1/usage（数据面 key 鉴权），
	// 与 newapi 的面板管理面端点不是一回事。
	UpstreamProtocolSub2API = "sub2api"
)

// upstreamProtocolCredentialKey 是账号凭证里声明「这是一台上游中转站」的键。
// 与 account_mode（payg/coding/zen/go，见 constants.go）同层同思路：
// 它决定探测走哪条分支，而不是引入新平台。
const upstreamProtocolCredentialKey = "upstream_protocol"

// UpstreamBalanceResult 是上游中转站余额探测的返回结构（管理端 + UI 消费）。
type UpstreamBalanceResult struct {
	AccountID int64  `json:"account_id"`
	Protocol  string `json:"protocol"`
	Success   bool   `json:"success"`
	// Unit 为 "usd" 时 Balance/Used 已是美元；为 "quota" 时是面板内部整数单位，
	// 前端必须按额度单位展示（不能加 $）。
	Unit       string  `json:"unit"`
	Balance    float64 `json:"balance"`      // 剩余
	Used       float64 `json:"used"`         // 已用
	QuotaRaw   float64 `json:"quota_raw"`    // 面板原始 quota 整数（排查用）
	StatusCode int     `json:"status_code,omitempty"`
	FetchedAt  int64   `json:"fetched_at"`
	Persisted  bool    `json:"persisted"`
	Error      string  `json:"error,omitempty"`
}

// UpstreamBalanceService 探测上游中转站账号的余额。
type UpstreamBalanceService struct {
	accountRepo  AccountRepository
	proxyRepo    ProxyRepository
	httpUpstream HTTPUpstream
	cfg          *config.Config
	flight       singleflight.Group
}

// NewUpstreamBalanceService 构造上游中转站余额探测服务。
func NewUpstreamBalanceService(
	accountRepo AccountRepository,
	proxyRepo ProxyRepository,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
) *UpstreamBalanceService {
	return &UpstreamBalanceService{
		accountRepo:  accountRepo,
		proxyRepo:    proxyRepo,
		httpUpstream: httpUpstream,
		cfg:          cfg,
	}
}

// QueryBalance 探测指定上游中转站账号的余额并落 Extra 快照。
func (s *UpstreamBalanceService) QueryBalance(ctx context.Context, accountID int64) (*UpstreamBalanceResult, error) {
	account, err := s.loadRelayAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return s.QueryBalanceForAccount(ctx, account)
}

// QueryBalanceForAccount 探测已加载账号（周期检测 / 监控抓取复用，避免二次 GetByID）。
func (s *UpstreamBalanceService) QueryBalanceForAccount(ctx context.Context, account *Account) (*UpstreamBalanceResult, error) {
	if s == nil || s.accountRepo == nil || s.httpUpstream == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "UPSTREAM_BALANCE_NOT_CONFIGURED", "upstream balance service is not configured")
	}
	if err := validateRelayAccount(account); err != nil {
		return nil, err
	}
	key := "upstream_balance:" + strconv.FormatInt(account.ID, 10)
	resultCh := s.flight.DoChan(key, func() (any, error) {
		probeCtx, cancel := context.WithTimeout(context.Background(), upstreamBalanceTimeout+5*time.Second)
		defer cancel()
		return s.queryBalanceForAccount(probeCtx, account)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case flightResult := <-resultCh:
		if flightResult.Err != nil {
			return nil, flightResult.Err
		}
		result, ok := flightResult.Val.(*UpstreamBalanceResult)
		if !ok || result == nil {
			return nil, infraerrors.New(http.StatusInternalServerError, "UPSTREAM_BALANCE_RESULT_INVALID", "invalid upstream balance probe result")
		}
		cloned := *result
		return &cloned, nil
	}
}

func (s *UpstreamBalanceService) queryBalanceForAccount(ctx context.Context, account *Account) (*UpstreamBalanceResult, error) {
	protocol := strings.TrimSpace(account.GetCredential(upstreamProtocolCredentialKey))
	if protocol == "" {
		protocol = UpstreamProtocolNewAPI
	}
	if protocol != UpstreamProtocolNewAPI && protocol != UpstreamProtocolSub2API {
		return nil, infraerrors.Newf(http.StatusBadRequest, "UPSTREAM_BALANCE_UNSUPPORTED_PROTOCOL", "unsupported upstream protocol: %s", protocol)
	}

	apiKey := strings.TrimSpace(account.GetCredential("api_key"))
	if apiKey == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "UPSTREAM_BALANCE_NO_APIKEY", "account api_key is empty")
	}

	targetURL := upstreamBalanceURL(account, protocol)
	if targetURL == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "UPSTREAM_BALANCE_NO_BASE_URL", "account base_url is empty")
	}
	// 出站前过 URL 安全策略（与网关转发 / CN 余额探测同一套校验）：对端域名必须在
	// security.url_allowlist.upstream_hosts 内，否则拒绝且不发出任何请求（key 不出站）。
	validatedURL, err := cnValidateProbeURL(s.cfg, targetURL)
	if err != nil {
		return nil, infraerrors.New(http.StatusForbidden, "UPSTREAM_BALANCE_URL_REJECTED", err.Error())
	}
	targetURL = validatedURL

	proxyURL := s.resolveProxyURL(ctx, account)
	callCtx, cancel := context.WithTimeout(ctx, upstreamBalanceTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "UPSTREAM_BALANCE_REQUEST_BUILD_FAILED", "build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	account.ApplyHeaderOverrides(req.Header)

	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, maxInt(account.Concurrency, 1))
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "UPSTREAM_BALANCE_REQUEST_FAILED", "upstream request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamBalanceMaxBody))

	now := time.Now().UTC()
	result := &UpstreamBalanceResult{
		AccountID:  account.ID,
		Protocol:   protocol,
		Unit:       "quota",
		FetchedAt:  now.Unix(),
		StatusCode: resp.StatusCode,
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		result.Error = fmt.Sprintf("Authentication failed (HTTP %d)", resp.StatusCode)
		s.persist(ctx, account, result)
		return result, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.Error = fmt.Sprintf("API error (HTTP %d): %s", resp.StatusCode, truncate(strings.TrimSpace(string(bodyBytes)), 240))
		s.persist(ctx, account, result)
		return result, nil
	}

	if protocol == UpstreamProtocolSub2API {
		s.parseSub2APIBalance(bodyBytes, result)
		s.persist(ctx, account, result)
		return result, nil
	}

	// New-API 兼容响应：{"success":true,"data":{"quota":<remaining>,"used_quota":<used>}}
	dataNode := gjson.GetBytes(bodyBytes, "data.quota")
	quotaRaw, ok := cnParseF64(dataNode.Value())
	if !dataNode.Exists() || !ok {
		result.Error = "Invalid balance response: missing data.quota (upstream is probably not a New-API panel)"
		s.persist(ctx, account, result)
		return result, nil
	}
	usedRaw, _ := cnParseF64(gjson.GetBytes(bodyBytes, "data.used_quota").Value())

	perUnit := gjson.GetBytes(bodyBytes, "data.quota_per_unit")
	perUnitF, okPerUnit := cnParseF64(perUnit.Value())
	if !perUnit.Exists() || !okPerUnit || perUnitF <= 0 {
		perUnitF = upstreamDefaultQuotaPerUnit
	}

	result.QuotaRaw = quotaRaw
	if perUnitF == upstreamDefaultQuotaPerUnit {
		// New-API 默认折算率：面板未显式给出时按默认换算。
		result.Unit = "usd"
		result.Balance = quotaRaw / perUnitF
		result.Used = usedRaw / perUnitF
	} else {
		// 站长改过折算率，但我们不知道币种语义，原样按额度单位展示。
		result.Unit = "quota"
		result.Balance = quotaRaw
		result.Used = usedRaw
	}
	result.Success = true

	s.persist(ctx, account, result)
	return result, nil
}

// parseSub2APIBalance 解析对端 sub2api 的 GET /v1/usage 响应。
//
// 该端点有三种响应形态（见 internal/handler/gateway_handler.go）：
//   - 余额模式（unrestricted）      ：{"remaining":<余额>,"balance":<余额>,"unit":"USD",...}
//   - 订阅模式（unrestricted）      ：{"remaining":<剩余额度>,"unit":"USD","subscription":{...}}
//   - Key 配额模式（quota_limited）：{"remaining":<剩余>,"unit":"USD","quota":{limit,used,remaining}}
//
// 取 remaining 是最稳的（三种形态都有），并按 quota.remaining → balance 兜底。
// 只有 quota_limited 形态带 used；其余形态不编造已用值。
func (s *UpstreamBalanceService) parseSub2APIBalance(bodyBytes []byte, result *UpstreamBalanceResult) {
	remainingNode := gjson.GetBytes(bodyBytes, "remaining")
	if !remainingNode.Exists() {
		remainingNode = gjson.GetBytes(bodyBytes, "quota.remaining")
	}
	if !remainingNode.Exists() {
		remainingNode = gjson.GetBytes(bodyBytes, "balance")
	}
	remaining, ok := cnParseF64(remainingNode.Value())
	if !remainingNode.Exists() || !ok {
		result.Error = "Invalid balance response: missing remaining/quota.remaining/balance (upstream is not a sub2api gateway?)"
		return
	}

	result.Balance = remaining
	result.QuotaRaw = remaining

	// quota_limited 形态才带 used；其它形态 used 保持 0（不编造）。
	if used, ok := cnParseF64(gjson.GetBytes(bodyBytes, "quota.used").Value()); ok {
		result.Used = used
	}

	// 单位：端点恒返回 USD；缺失也按 USD（sub2api 的额度本就是美元计价，
	// 与 New-API 的整数 quota 不同，不存在折算率问题）。
	unit := strings.ToUpper(strings.TrimSpace(gjson.GetBytes(bodyBytes, "unit").String()))
	if unit == "" || unit == "USD" {
		result.Unit = "usd"
	} else {
		result.Unit = "quota"
	}

	result.Success = true
}

// persist 把探测结果写入 account.Extra。失败只告警不返回错误——与
// CNProviderBalanceService.queryBalanceForAccount 的处理一致：
// 探测结果本身有价值，落库失败不该让调用方拿不到余额。
func (s *UpstreamBalanceService) persist(ctx context.Context, account *Account, result *UpstreamBalanceResult) {
	updates := map[string]any{
		upstreamExtraBalance:  result.Balance,
		upstreamExtraUsed:     result.Used,
		upstreamExtraUnit:     result.Unit,
		upstreamExtraUpdated:  time.Unix(result.FetchedAt, 0).UTC().Format(time.RFC3339),
		upstreamExtraProtocol: result.Protocol,
	}
	// 成功必须显式清空上一次的错误，否则 UI 会一直挂着旧报错。
	updates[upstreamExtraError] = result.Error
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, updates); err != nil {
		slog.Warn("upstream_balance_persist_failed", "account_id", account.ID, "protocol", result.Protocol, "error", err)
		return
	}
	result.Persisted = true
}

// loadRelayAccount 加载上游中转站账号。
func (s *UpstreamBalanceService) loadRelayAccount(ctx context.Context, accountID int64) (*Account, error) {
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusNotFound, "UPSTREAM_BALANCE_ACCOUNT_NOT_FOUND", "account not found: %v", err)
	}
	if err := validateRelayAccount(account); err != nil {
		return nil, err
	}
	return account, nil
}

// validateRelayAccount 非 DB 校验（ForAccount 入口复用，保证直传 account 也不绕过检查）。
//
// 认定条件：apikey 类型 + 在凭证里显式声明了 upstream_protocol。不用平台白名单，
// 因为中转站的「平台」在面板里就是 openai / anthropic 之类，无法据此区分对端程序；
// 显式声明才是可靠的信号。
func validateRelayAccount(account *Account) error {
	if account == nil {
		return infraerrors.New(http.StatusNotFound, "UPSTREAM_BALANCE_ACCOUNT_NOT_FOUND", "account not found")
	}
	if account.Type != AccountTypeAPIKey {
		return infraerrors.New(http.StatusBadRequest, "UPSTREAM_BALANCE_NOT_APIKEY", "only apikey accounts have an upstream balance")
	}
	if strings.TrimSpace(account.GetCredential(upstreamProtocolCredentialKey)) == "" {
		return infraerrors.New(http.StatusBadRequest, "UPSTREAM_BALANCE_NOT_RELAY", "account is not configured as an upstream relay (missing upstream_protocol)")
	}
	return nil
}

func (s *UpstreamBalanceService) resolveProxyURL(ctx context.Context, account *Account) string {
	if account == nil || account.ProxyID == nil {
		return ""
	}
	if account.Proxy != nil {
		return account.Proxy.URL()
	}
	if s != nil && s.proxyRepo != nil {
		if proxy, err := s.proxyRepo.GetByID(ctx, *account.ProxyID); err == nil && proxy != nil {
			account.Proxy = proxy
			return proxy.URL()
		}
	}
	return ""
}

// upstreamBalanceURL 按协议拼接对端余额端点。
//
//   - newapi：{base}/api/user/self      —— New-API / One-API 面板管理面
//   - sub2api：{base 去掉 /v1}/v1/usage —— 网关面，数据面 key 鉴权
func upstreamBalanceURL(account *Account, protocol string) string {
	base := strings.TrimRight(upstreamRelayBaseURL(account), "/")
	if base == "" {
		return ""
	}
	if protocol == UpstreamProtocolSub2API {
		// base 通常已带 /v1（如 https://host/v1），去掉再拼，避免 /v1/v1/usage。
		if strings.HasSuffix(base, "/v1") {
			base = strings.TrimSuffix(base, "/v1")
		}
		return base + "/v1/usage"
	}
	return base + "/api/user/self"
}

// upstreamRelayBaseURL 取中转站账号填写的 base_url，与平台无关。
// GetOpenAIFormatBaseURL 对 anthropic / gemini 等非 OpenAI 平台返回空串，
// 不能用来定位对端；GetBaseURL 又会给 antigravity 追加路径、给空值兜底官方域名。
func upstreamRelayBaseURL(account *Account) string {
	if base := strings.TrimSpace(account.GetCredential("base_url")); base != "" {
		return base
	}
	return strings.TrimSpace(account.GetOpenAIFormatBaseURL())
}
