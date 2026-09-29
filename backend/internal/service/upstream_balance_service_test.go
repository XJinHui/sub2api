package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// upstreamBalanceUpstream 是可控响应的上游 stub（记录调用次数，用于断言
// 「被策略拒绝时 key 不出站」）。
type upstreamBalanceUpstream struct {
	statusCode int
	body       string
	calls      int
}

func (u *upstreamBalanceUpstream) Do(
	_ *http.Request,
	_ string,
	_ int64,
	_ int,
) (*http.Response, error) {
	u.calls++
	return &http.Response{
		StatusCode: u.statusCode,
		Body:       io.NopCloser(strings.NewReader(u.body)),
		Header:     make(http.Header),
	}, nil
}

func (u *upstreamBalanceUpstream) DoWithTLS(
	req *http.Request,
	proxyURL string,
	accountID int64,
	accountConcurrency int,
	_ *tlsfingerprint.Profile,
) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

type upstreamBalanceRepo struct {
	AccountRepository
	account     *Account
	extraWrites []map[string]any
}

func (r *upstreamBalanceRepo) GetByID(_ context.Context, _ int64) (*Account, error) {
	return r.account, nil
}

func (r *upstreamBalanceRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.extraWrites = append(r.extraWrites, updates)
	return nil
}

// newRelayAccount 造一个「上游中转站」账号：apikey 类型 + 显式声明 upstream_protocol。
func newRelayAccount() *Account {
	return &Account{
		ID:       99,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"upstream_protocol": UpstreamProtocolNewAPI,
			"api_key":           "sk-relay-test",
			"base_url":          "https://relay.example.com",
		},
	}
}

// newUpstreamService 造服务。allowlist 为空时关闭 URL 白名单（只做格式校验），
// 便于单测不依赖具体域名。
func newUpstreamService(repo AccountRepository, upstream HTTPUpstream, allowHosts ...string) *UpstreamBalanceService {
	cfg := &config.Config{}
	if len(allowHosts) > 0 {
		cfg.Security.URLAllowlist.Enabled = true
		cfg.Security.URLAllowlist.UpstreamHosts = allowHosts
	}
	return NewUpstreamBalanceService(repo, nil, upstream, cfg)
}

func TestUpstreamBalance_NewAPIResponseParsesToUSD(t *testing.T) {
	repo := &upstreamBalanceRepo{account: newRelayAccount()}
	upstream := &upstreamBalanceUpstream{
		statusCode: http.StatusOK,
		// 默认折算率 500000 quota = $1 → 1_250_000 quota = $2.50
		body: `{"success":true,"data":{"quota":1250000,"used_quota":2500000}}`,
	}
	svc := newUpstreamService(repo, upstream)

	result, err := svc.QueryBalance(context.Background(), 99)
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, "usd", result.Unit)
	require.InDelta(t, 2.50, result.Balance, 1e-9)
	require.InDelta(t, 5.00, result.Used, 1e-9)
	require.Empty(t, result.Error)
	require.True(t, result.Persisted)
	require.Len(t, repo.extraWrites, 1)
	require.InDelta(t, 2.50, repo.extraWrites[0][upstreamExtraBalance], 1e-9)
}

// 站点改过折算率时不得擅自当美元——原样按「额度单位」展示。
func TestUpstreamBalance_CustomQuotaPerUnitStaysRaw(t *testing.T) {
	repo := &upstreamBalanceRepo{account: newRelayAccount()}
	upstream := &upstreamBalanceUpstream{
		statusCode: http.StatusOK,
		body:       `{"success":true,"data":{"quota":3000,"used_quota":1000,"quota_per_unit":1000}}`,
	}
	svc := newUpstreamService(repo, upstream)

	result, err := svc.QueryBalance(context.Background(), 99)
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, "quota", result.Unit)
	require.InDelta(t, 3000, result.Balance, 1e-9)
	require.InDelta(t, 3000, result.QuotaRaw, 1e-9)
}

// sub2api 自身拿数据面 key 打管理面会 401 —— 必须报鉴权失败，不能变成余额 0。
func TestUpstreamBalance_AuthFailureIsNotZeroBalance(t *testing.T) {
	repo := &upstreamBalanceRepo{account: newRelayAccount()}
	upstream := &upstreamBalanceUpstream{
		statusCode: http.StatusUnauthorized,
		body:       `{"code":"UNAUTHORIZED","message":"Authorization header is required"}`,
	}
	svc := newUpstreamService(repo, upstream)

	result, err := svc.QueryBalance(context.Background(), 99)
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Contains(t, result.Error, "Authentication failed")
	require.Zero(t, result.Balance)
	// 失败也要落快照（带错误），否则 UI 会一直显示上一次的成功余额。
	require.True(t, result.Persisted)
	require.Contains(t, repo.extraWrites[0][upstreamExtraError], "Authentication failed")
}

// 200 但结构不是 New-API（例如对端是 sub2api 的其它路径）→ 明确报解析失败。
func TestUpstreamBalance_MissingQuotaFieldDoesNotBecomeZero(t *testing.T) {
	repo := &upstreamBalanceRepo{account: newRelayAccount()}
	upstream := &upstreamBalanceUpstream{
		statusCode: http.StatusOK,
		body:       `{"success":true,"data":{"platform_quotas":[]}}`,
	}
	svc := newUpstreamService(repo, upstream)

	result, err := svc.QueryBalance(context.Background(), 99)
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Contains(t, result.Error, "missing data.quota")
	require.Zero(t, result.Balance)
}

// 成功时清空上一次的错误（否则 UI 挂着旧报错）。
func TestUpstreamBalance_SuccessClearsPreviousError(t *testing.T) {
	repo := &upstreamBalanceRepo{account: newRelayAccount()}
	upstream := &upstreamBalanceUpstream{
		statusCode: http.StatusOK,
		body:       `{"success":true,"data":{"quota":500000,"used_quota":0}}`,
	}
	svc := newUpstreamService(repo, upstream)

	_, err := svc.QueryBalance(context.Background(), 99)
	require.NoError(t, err)
	require.Empty(t, repo.extraWrites[0][upstreamExtraError])
}

// 白名单外的对端域名必须被拒，且不得发起任何上游请求（API key 不出站）。
func TestUpstreamBalance_AllowlistRejectsBeforeSendingKey(t *testing.T) {
	repo := &upstreamBalanceRepo{account: newRelayAccount()}
	upstream := &upstreamBalanceUpstream{
		statusCode: http.StatusOK,
		body:       `{"success":true,"data":{"quota":1}}`,
	}
	// 白名单只放行别的主机，账号 base_url 指向 relay.example.com。
	svc := newUpstreamService(repo, upstream, "api.other-host.example")

	_, err := svc.QueryBalance(context.Background(), 99)
	require.Error(t, err)
	require.Contains(t, err.Error(), "URL security policy")
	require.Zero(t, upstream.calls, "被策略拒绝时不得发起上游请求")
	require.Empty(t, repo.extraWrites)
}

// 未声明 upstream_protocol 的普通账号不能被当成中转站来探余额。
func TestUpstreamBalance_NonRelayAccountRejected(t *testing.T) {
	plain := newRelayAccount()
	delete(plain.Credentials, "upstream_protocol")

	repo := &upstreamBalanceRepo{account: plain}
	upstream := &upstreamBalanceUpstream{statusCode: http.StatusOK, body: `{}`}
	svc := newUpstreamService(repo, upstream)

	_, err := svc.QueryBalance(context.Background(), 99)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not configured as an upstream relay")
	require.Zero(t, upstream.calls)
}

// 非 apikey 类型（如 oauth）同样拒绝——余额探测依赖数据面 key。
func TestUpstreamBalance_OAuthAccountRejected(t *testing.T) {
	oauth := newRelayAccount()
	oauth.Type = AccountTypeOAuth

	repo := &upstreamBalanceRepo{account: oauth}
	upstream := &upstreamBalanceUpstream{statusCode: http.StatusOK, body: `{}`}
	svc := newUpstreamService(repo, upstream)

	_, err := svc.QueryBalance(context.Background(), 99)
	require.Error(t, err)
	require.Contains(t, err.Error(), "only apikey accounts")
	require.Zero(t, upstream.calls)
}

// ===== sub2api 协议（GET /v1/usage，数据面 key 鉴权）=====

func newSub2APIRelayAccount() *Account {
	return &Account{
		ID:       77,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"upstream_protocol": UpstreamProtocolSub2API,
			"api_key":           "sk-sub2api-test",
			// base 通常带 /v1，探测需回退到 /v1/usage 而不是 /v1/v1/usage
			"base_url": "https://upstream.example.com/v1",
		},
	}
}

// 余额模式：{remaining, balance, unit:"USD", isValid}
func TestUpstreamBalance_Sub2APIWalletMode(t *testing.T) {
	repo := &upstreamBalanceRepo{account: newSub2APIRelayAccount()}
	upstream := &upstreamBalanceUpstream{
		statusCode: http.StatusOK,
		body:       `{"mode":"unrestricted","isValid":true,"remaining":42.75,"unit":"USD","balance":42.75,"planName":"钱包余额"}`,
	}
	svc := newUpstreamService(repo, upstream)

	result, err := svc.QueryBalance(context.Background(), 77)
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, UpstreamProtocolSub2API, result.Protocol)
	require.Equal(t, "usd", result.Unit)
	require.InDelta(t, 42.75, result.Balance, 1e-9)
	// 钱包模式不带 used，不得编造。
	require.Zero(t, result.Used)
}

// Key 配额模式：{remaining, quota:{limit,used,remaining}} —— used 取 quota.used
func TestUpstreamBalance_Sub2APIQuotaLimitedMode(t *testing.T) {
	repo := &upstreamBalanceRepo{account: newSub2APIRelayAccount()}
	upstream := &upstreamBalanceUpstream{
		statusCode: http.StatusOK,
		body:       `{"mode":"quota_limited","isValid":true,"status":"active","remaining":7.5,"unit":"USD","quota":{"limit":10,"used":2.5,"remaining":7.5,"unit":"USD"}}`,
	}
	svc := newUpstreamService(repo, upstream)

	result, err := svc.QueryBalance(context.Background(), 77)
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, "usd", result.Unit)
	require.InDelta(t, 7.5, result.Balance, 1e-9)
	require.InDelta(t, 2.5, result.Used, 1e-9)
}

// 订阅模式：remaining 缺失（订阅信息不在 context 中）时兜底 quota.remaining → balance
func TestUpstreamBalance_Sub2APIFallsBackToBalanceField(t *testing.T) {
	repo := &upstreamBalanceRepo{account: newSub2APIRelayAccount()}
	upstream := &upstreamBalanceUpstream{
		statusCode: http.StatusOK,
		body:       `{"mode":"unrestricted","isValid":true,"unit":"USD","balance":15.25,"planName":"Claude Pro"}`,
	}
	svc := newUpstreamService(repo, upstream)

	result, err := svc.QueryBalance(context.Background(), 77)
	require.NoError(t, err)
	require.True(t, result.Success)
	require.InDelta(t, 15.25, result.Balance, 1e-9)
}

// 结构不符（既无 remaining 也无 balance）→ 报错，不能变成余额 0
func TestUpstreamBalance_Sub2APIMissingFieldsDoesNotBecomeZero(t *testing.T) {
	repo := &upstreamBalanceRepo{account: newSub2APIRelayAccount()}
	upstream := &upstreamBalanceUpstream{
		statusCode: http.StatusOK,
		body:       `{"object":"list","data":[]}`,
	}
	svc := newUpstreamService(repo, upstream)

	result, err := svc.QueryBalance(context.Background(), 77)
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Contains(t, result.Error, "missing remaining")
	require.Zero(t, result.Balance)
}

// key 无效 → 401，报鉴权失败
func TestUpstreamBalance_Sub2APIInvalidKey(t *testing.T) {
	repo := &upstreamBalanceRepo{account: newSub2APIRelayAccount()}
	upstream := &upstreamBalanceUpstream{
		statusCode: http.StatusUnauthorized,
		body:       `{"type":"error","error":{"type":"authentication_error","message":"Invalid API key"}}`,
	}
	svc := newUpstreamService(repo, upstream)

	result, err := svc.QueryBalance(context.Background(), 77)
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Contains(t, result.Error, "Authentication failed")
}

// 端点拼接：base 带 /v1 时不得拼出 /v1/v1/usage
func TestUpstreamBalance_Sub2APIEndpointNormalization(t *testing.T) {
	require.Equal(t,
		"https://upstream.example.com/v1/usage",
		upstreamBalanceURL(newSub2APIRelayAccount(), UpstreamProtocolSub2API))

	// base 不带 /v1 时补上
	noV1 := newSub2APIRelayAccount()
	noV1.Credentials["base_url"] = "https://upstream.example.com"
	require.Equal(t,
		"https://upstream.example.com/v1/usage",
		upstreamBalanceURL(noV1, UpstreamProtocolSub2API))

	// newapi 路径不受影响
	require.Equal(t,
		"https://upstream.example.com/v1/api/user/self",
		upstreamBalanceURL(newSub2APIRelayAccount(), UpstreamProtocolNewAPI))
}

// 回归：anthropic / gemini 平台的中转站账号也要按 base_url 拼端点。
// 旧实现用 GetOpenAIFormatBaseURL，对非 OpenAI 平台返回空串，拼出裸路径 "/v1/usage"
// 被 URL 策略拒绝（invalid url: /v1/usage）。
func TestUpstreamBalance_Sub2APINonOpenAIPlatforms(t *testing.T) {
	for _, platform := range []string{PlatformAnthropic, PlatformGemini, PlatformAntigravity} {
		account := newSub2APIRelayAccount()
		account.Platform = platform
		account.Credentials["base_url"] = "https://upstream.example.com"
		require.Equal(t,
			"https://upstream.example.com/v1/usage",
			upstreamBalanceURL(account, UpstreamProtocolSub2API), platform)
	}

	repo := &upstreamBalanceRepo{account: func() *Account {
		a := newSub2APIRelayAccount()
		a.Platform = PlatformAnthropic
		return a
	}()}
	upstream := &upstreamBalanceUpstream{
		statusCode: http.StatusOK,
		body:       `{"mode":"unrestricted","isValid":true,"remaining":3.5,"unit":"USD","balance":3.5}`,
	}
	svc := newUpstreamService(repo, upstream)
	result, err := svc.QueryBalance(context.Background(), 77)
	require.NoError(t, err)
	require.True(t, result.Success, result.Error)
	require.InDelta(t, 3.5, result.Balance, 1e-9)
}

// base_url 为空时明确报错，不发起请求
func TestUpstreamBalance_EmptyBaseURLRejected(t *testing.T) {
	account := newSub2APIRelayAccount()
	account.Platform = PlatformAnthropic
	delete(account.Credentials, "base_url")
	repo := &upstreamBalanceRepo{account: account}
	upstream := &upstreamBalanceUpstream{statusCode: http.StatusOK, body: `{}`}
	svc := newUpstreamService(repo, upstream)
	_, err := svc.QueryBalance(context.Background(), 77)
	require.Error(t, err)
	require.Contains(t, err.Error(), "base_url")
	require.Equal(t, 0, upstream.calls)
}
