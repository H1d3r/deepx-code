package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// BalanceResult 是一次账户余额查询的结果。Display 已是可直接渲染的金额串(含币种符号,如 "¥110.00")。
type BalanceResult struct {
	Display   string // 可渲染金额串;Supported=false 时为空
	Supported bool   // 该供应商是否支持凭模型 API Key 查余额
}

// ProbeBalance 用模型 API Key 查询账户剩余金额。
//
// 现状:四家预置供应商里只有 DeepSeek 和 Kimi(Moonshot)提供可凭模型 Key 直接调用的余额接口。
// qwen 需阿里云 AccessKey/SecretKey 走 BSS OpenAPI、mimo 无任何余额接口 —— 这两家(及未知 custom 端点)
// 返回 Supported=false,调用方据此显示 "-"。
//
// 返回 (result, err):
//   - err != nil:网络 / 5xx 等瞬时错误 → 调用方不更新,下次再探;
//   - err == nil 且 Supported=false:该供应商不支持查询;
//   - err == nil 且 Supported=true:Display 为可渲染金额串。
func ProbeBalance(ctx context.Context, entry ModelEntry) (BalanceResult, error) {
	if entry.BaseURL == "" || entry.APIKey == "" {
		return BalanceResult{Supported: false}, nil
	}
	host := balanceHostOf(entry.BaseURL)
	switch {
	case strings.Contains(host, "deepseek"):
		return probeDeepSeekBalance(ctx, entry)
	case strings.Contains(host, "moonshot"):
		return probeKimiBalance(ctx, entry)
	default:
		// qwen / mimo / 未知 custom 端点:无可凭 Key 调用的余额接口。
		return BalanceResult{Supported: false}, nil
	}
}

// probeDeepSeekBalance 调 GET {base_url}/user/balance(DeepSeek base_url 不含 /v1,拼出 /user/balance)。
func probeDeepSeekBalance(ctx context.Context, entry ModelEntry) (BalanceResult, error) {
	body, err := httpGetJSON(ctx, entry.BaseURL+"/user/balance", entry.APIKey)
	if err != nil {
		return BalanceResult{}, err
	}
	var r struct {
		IsAvailable  bool `json:"is_available"`
		BalanceInfos []struct {
			Currency     string `json:"currency"`
			TotalBalance string `json:"total_balance"`
		} `json:"balance_infos"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return BalanceResult{}, err
	}
	if len(r.BalanceInfos) == 0 {
		return BalanceResult{Supported: true, Display: "—"}, nil
	}
	// DeepSeek 会按币种返回多条(常见 USD 0.00 在前、CNY 有钱在后)。挑金额最大的那条展示,
	// 全为 0 则退回第一条 —— 避免误显 "$0.00" 而把真实的 ¥24.86 漏掉。
	best := r.BalanceInfos[0]
	bestAmt := -1.0
	for _, b := range r.BalanceInfos {
		amt, err := strconv.ParseFloat(b.TotalBalance, 64)
		if err != nil {
			continue
		}
		if amt > bestAmt {
			bestAmt, best = amt, b
		}
	}
	return BalanceResult{Supported: true, Display: currencySymbol(best.Currency) + best.TotalBalance}, nil
}

// probeKimiBalance 调 GET {base_url}/users/me/balance(Kimi base_url 带 /v1,拼出 /v1/users/me/balance)。
func probeKimiBalance(ctx context.Context, entry ModelEntry) (BalanceResult, error) {
	body, err := httpGetJSON(ctx, entry.BaseURL+"/users/me/balance", entry.APIKey)
	if err != nil {
		return BalanceResult{}, err
	}
	var r struct {
		Status bool `json:"status"`
		Data   struct {
			AvailableBalance float64 `json:"available_balance"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return BalanceResult{}, err
	}
	// Kimi 余额均为人民币,且 available_balance = 现金 + 代金券,即"还能花多少"。
	return BalanceResult{Supported: true, Display: fmt.Sprintf("¥%.2f", r.Data.AvailableBalance)}, nil
}

// httpGetJSON 发一个带 Bearer 鉴权的 GET,返回响应体(非 200 视为错误,调用方按瞬时错误处理)。
func httpGetJSON(ctx context.Context, url, apiKey string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("balance: http %d", resp.StatusCode)
	}
	return body, nil
}

// currencySymbol 把币种代码转成符号;未知币种回退成 "CODE "(带尾空格,数字前留隔)。
func currencySymbol(cur string) string {
	switch strings.ToUpper(cur) {
	case "CNY", "RMB":
		return "¥"
	case "USD":
		return "$"
	default:
		if cur == "" {
			return ""
		}
		return cur + " "
	}
}

// balanceHostOf 从 base_url 抽出 host(去 scheme / path),用于判定供应商。
func balanceHostOf(rawURL string) string {
	h := rawURL
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.IndexAny(h, "/?"); i >= 0 {
		h = h[:i]
	}
	return h
}
