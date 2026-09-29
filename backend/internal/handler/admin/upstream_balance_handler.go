package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// UpstreamBalanceHandler 暴露上游中转站账号的余额查询端点。
//
//   - GET /admin/upstream-balance/accounts/:id/balance
//
// 「上游中转站账号」的认定不看平台（对端程序无法从平台字段区分），而是看账号凭证里
// 是否显式声明了 upstream_protocol（见 service.validateRelayAccount）。
// 未声明的账号调用此端点会拿到 UPSTREAM_BALANCE_NOT_RELAY，而不是空余额。
type UpstreamBalanceHandler struct {
	balanceService *service.UpstreamBalanceService
}

func NewUpstreamBalanceHandler(balanceService *service.UpstreamBalanceService) *UpstreamBalanceHandler {
	return &UpstreamBalanceHandler{balanceService: balanceService}
}

// QueryBalance 查询上游中转站账号在对端面板的余额。
func (h *UpstreamBalanceHandler) QueryBalance(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h == nil || h.balanceService == nil {
		response.BadRequest(c, "upstream balance service is not enabled")
		return
	}
	result, err := h.balanceService.QueryBalance(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}
