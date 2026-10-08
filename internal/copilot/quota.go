package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	contract "github.com/1oo1/cpa-github-copilot/internal/contract"
)

func (s *service) fetchQuotaRequest(ctx context.Context, callbackID string, req contract.QuotaFetchRequest) (contract.QuotaFetchResponse, error) {
	if req.Provider != "" && req.Provider != providerIdentifier {
		return contract.QuotaFetchResponse{}, &serviceError{code: "invalid_auth", message: "invalid quota provider", httpStatus: 400}
	}
	storage, err := decodeCopilotStorage(req.StorageJSON)
	if err != nil || strings.TrimSpace(storage.GitHubAccessToken) == "" || strings.ContainsAny(storage.GitHubAccessToken, "\r\n") {
		return contract.QuotaFetchResponse{}, &serviceError{code: "invalid_auth", message: "GitHub Copilot credential is invalid", httpStatus: 401}
	}
	endpoints, err := endpointsForHost(storage.GitHubHost)
	if err != nil || endpoints.GitHubHost != s.loadedConfig().GitHubHost {
		return contract.QuotaFetchResponse{}, &serviceError{code: "invalid_auth", message: "GitHub Copilot host is invalid for this configuration", httpStatus: 400}
	}
	client := s.client(ctx, callbackID)
	response, err := client.do(contract.HTTPRequest{Method: http.MethodGet, URL: endpoints.EntitlementURL, Headers: http.Header{"Authorization": {"Bearer " + storage.GitHubAccessToken}, "Accept": {"application/json"}}})
	if err != nil {
		return contract.QuotaFetchResponse{}, &serviceError{code: "quota_unavailable", message: "GitHub Copilot quota request failed", retryable: true}
	}
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusForbidden && response.Headers.Get("Retry-After") != "" {
		return contract.QuotaFetchResponse{Stale: true, RetryAfterSeconds: quotaRetryAfter(response.Headers.Get("Retry-After"), s.now()), Admission: &contract.QuotaAdmission{State: "unknown"}}, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return contract.QuotaFetchResponse{}, upstreamFailure("quota_http_error", "GitHub Copilot quota request failed", response.StatusCode)
	}
	quota, err := parseQuota(response.Body)
	if err != nil {
		return contract.QuotaFetchResponse{}, &serviceError{code: "invalid_quota", message: "GitHub Copilot quota response is invalid"}
	}
	return quota, nil
}

func quotaRetryAfter(value string, now time.Time) int {
	if n, err := strconv.Atoi(value); err == nil && n > 0 {
		return min(n, 86400)
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return min(86400, int(math.Ceil(date.Sub(now).Seconds())))
	}
	return 60
}

// Decode each field independently: a malformed optional category must not erase
// valid subscription data or become an implicit zero-usage observation.
type quotaObject map[string]json.RawMessage

func (o quotaObject) object(key string) quotaObject {
	var v quotaObject
	_ = json.Unmarshal(o[key], &v)
	return v
}
func (o quotaObject) text(key string) string { var v string; _ = json.Unmarshal(o[key], &v); return v }
func (o quotaObject) boolean(key string) (bool, bool) {
	var v bool
	b := o[key]
	if !bytes.Equal(b, []byte("true")) && !bytes.Equal(b, []byte("false")) {
		return false, false
	}
	_ = json.Unmarshal(b, &v)
	return v, true
}
func (o quotaObject) number(key string) (*big.Rat, bool) {
	raw := o[key]
	if len(raw) == 0 || len(raw) > 128 {
		return nil, false
	}
	value := string(raw)
	if raw[0] == '"' {
		value = o.text(key)
	}
	// json.Number syntax excludes Rat's fractions and hexadecimal notation.
	var number json.Number
	if json.Unmarshal([]byte(value), &number) != nil || number.String() == "" {
		return nil, false
	}
	if f, err := strconv.ParseFloat(number.String(), 64); err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return nil, false
	}
	if i := strings.IndexAny(number.String(), "eE"); i >= 0 {
		exponent, err := strconv.Atoi(number.String()[i+1:])
		if err != nil || exponent < -308 || exponent > 308 {
			return nil, false
		}
	}
	r, ok := new(big.Rat).SetString(number.String())
	if !ok || r.Sign() < 0 {
		return nil, false
	}
	f, _ := r.Float64()
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, false
	}
	return r, true
}
func quotaNumber(o quotaObject, key string) (float64, bool) {
	r, ok := o.number(key)
	if !ok {
		return 0, false
	}
	f, _ := r.Float64()
	return f, true
}

func parseQuota(raw []byte) (contract.QuotaFetchResponse, error) {
	var account quotaObject
	if err := json.Unmarshal(raw, &account); err != nil {
		return contract.QuotaFetchResponse{}, err
	}
	if account == nil {
		return contract.QuotaFetchResponse{}, &serviceError{message: "invalid quota object"}
	}
	out := contract.QuotaFetchResponse{Admission: &contract.QuotaAdmission{State: "unknown"}}
	plan := quotaPlan(account)
	if plan != "" {
		out.Subscription = &contract.QuotaSubscription{Plan: plan, TierName: plan}
	}
	snapshots := account.object("quota_snapshots")
	premium := snapshots.object("premium_interactions")
	tbb, _ := account.boolean("token_based_billing")
	selected := premium
	window, label := "premium_requests", "Premium requests"
	if tbb {
		window, label = "credits", "Credits"
	}
	if plan == "Copilot Free" {
		selected = snapshots.object("chat")
		if !tbb {
			window, label = "chat", "Chat messages"
		}
	}
	reset := quotaReset(selected, account)
	buckets := []contract.QuotaBucket{}
	unlimited, hasUnlimited := selected.boolean("unlimited")
	if unlimited {
		hasQuota, known := selected.boolean("has_quota")
		if used, ok := quotaNumber(selected, "credits_used"); ok && (!known || hasQuota) {
			out.Summary = append(out.Summary, contract.QuotaMetric{Key: "credits_used", Label: "Credits Used", Value: used, Format: "number"})
		}
	} else if remaining, ok := quotaNumber(selected, "percent_remaining"); ok && hasUnlimited {
		entitlement, allocated := quotaNumber(selected, "entitlement")
		if !allocated || entitlement > 0 {
			buckets = append(buckets, contract.QuotaBucket{Window: window, Description: label, RemainingFraction: min(100, remaining) / 100, ResetTime: reset})
		}
	} else if plan == "Copilot Free" && selected == nil {
		total, ok := quotaNumber(account.object("monthly_quotas"), "chat")
		remaining, valid := quotaNumber(account.object("limited_user_quotas"), "chat")
		if ok && valid && total > 0 {
			buckets = append(buckets, contract.QuotaBucket{Window: window, Description: label, RemainingFraction: min(1, remaining/total), ResetTime: reset})
		}
	}
	count, validCount := premium.number("overage_count")
	total, validTotal := premium.number("overage_entitlement")
	budgetReset := quotaReset(premium, account)
	out.Admission.ResetTime = budgetReset
	if validCount && validTotal {
		if total.Sign() > 0 {
			used := new(big.Rat).Quo(count, total)
			ratio, _ := used.Float64()
			buckets = append(buckets, contract.QuotaBucket{Window: "additional_budget", Description: "Additional Budget", RemainingFraction: max(0, 1-ratio), ResetTime: budgetReset})
			out.Admission.State = "allow"
			if used.Cmp(big.NewRat(99, 100)) > 0 {
				out.Admission.State = "block"
				out.Admission.Reason = "additional_budget_exceeded"
			}
		} else if count.Sign() == 0 {
			out.Admission.State = "allow"
		}
	}
	if len(buckets) > 0 {
		out.Groups = []contract.QuotaGroup{{DisplayName: "GitHub Copilot", Buckets: buckets}}
	}
	return out, nil
}

func quotaPlan(account quotaObject) string {
	switch account.text("access_type_sku") {
	case "free_limited_copilot":
		return "Copilot Free"
	case "free_educational_quota":
		return "Copilot Student"
	}
	if signup, _ := account.boolean("can_signup_for_limited"); signup {
		return ""
	}
	return map[string]string{"individual_edu": "Copilot Student", "individual": "Copilot Pro", "individual_pro": "Copilot Pro+", "individual_max": "Copilot Max", "business": "Copilot Business", "enterprise": "Copilot Enterprise"}[account.text("copilot_plan")]
}
func quotaReset(snapshot, account quotaObject) string {
	if seconds, ok := quotaNumber(snapshot, "quota_reset_at"); ok && seconds > 0 && seconds < 253402300800 {
		return time.Unix(int64(seconds), 0).UTC().Format(time.RFC3339)
	}
	for _, key := range []string{"quota_reset_date_utc", "quota_reset_date", "limited_user_reset_date"} {
		value := account.text(key)
		if t, err := time.Parse(time.RFC3339, value); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
		if _, err := time.Parse("2006-01-02", value); err == nil {
			return value
		}
	}
	return ""
}
