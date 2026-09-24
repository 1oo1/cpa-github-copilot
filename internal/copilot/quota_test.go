package copilot

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	contract "github.com/1oo1/cpa-github-copilot/internal/contract"
	operation "github.com/1oo1/cpa-github-copilot/internal/operation"
)

func TestQuotaThresholdAndDisplay(t *testing.T) {
	for _, tc := range []struct{ count, state string }{{"98.99", "allow"}, {"99", "allow"}, {"99.01", "block"}, {"100", "block"}, {"101", "block"}, {"0", "allow"}} {
		t.Run(tc.count, func(t *testing.T) {
			q, err := parseQuota([]byte(fmt.Sprintf(`{"copilot_plan":"individual_max","token_based_billing":true,"quota_reset_date_utc":"2026-10-01T00:00:00Z","quota_snapshots":{"premium_interactions":{"unlimited":false,"entitlement":"1000","credits_used":800,"percent_remaining":96,"overage_count":%s,"overage_entitlement":100}}}`, tc.count)))
			if err != nil || q.Subscription.Plan != "Copilot Max" || q.Admission.State != tc.state {
				t.Fatalf("quota=%+v err=%v", q, err)
			}
			buckets := q.Groups[0].Buckets
			if buckets[0].Window != "credits" || buckets[0].RemainingFraction != .96 || buckets[0].ResetTime != "2026-10-01T00:00:00Z" || buckets[1].Window != "additional_budget" {
				t.Fatalf("buckets=%+v", buckets)
			}
		})
	}
}
func TestQuotaMalformedCannotAllow(t *testing.T) {
	for _, budget := range []string{`{}`, `{"overage_count":null,"overage_entitlement":100}`, `{"overage_count":-1,"overage_entitlement":100}`, `{"overage_count":false,"overage_entitlement":100}`, `{"overage_count":"NaN","overage_entitlement":100}`, `{"overage_count":1,"overage_entitlement":0}`} {
		q, err := parseQuota([]byte(`{"quota_snapshots":{"premium_interactions":` + budget + `}}`))
		if err != nil || q.Admission.State != "unknown" {
			t.Fatalf("budget %s: %+v %v", budget, q.Admission, err)
		}
	}
}
func TestQuotaPlansAndReset(t *testing.T) {
	for id, name := range map[string]string{"individual": "Copilot Pro", "individual_pro": "Copilot Pro+", "individual_max": "Copilot Max", "individual_edu": "Copilot Student", "business": "Copilot Business", "enterprise": "Copilot Enterprise"} {
		q, _ := parseQuota([]byte(`{"copilot_plan":"` + id + `"}`))
		if q.Subscription == nil || q.Subscription.Plan != name {
			t.Fatal(id, q)
		}
	}
	q, _ := parseQuota([]byte(`{"copilot_plan":"unknown"}`))
	if q.Subscription != nil {
		t.Fatal(q)
	}
	for _, tc := range []struct{ raw, reset string }{
		{`{"quota_reset_date":"2026-10-01"}`, "2026-10-01"},
		{`{"quota_reset_date_utc":"invalid","quota_reset_date":"2026-10-01"}`, "2026-10-01"},
		{`{"quota_reset_date":"invalid"}`, ""},
	} {
		var o quotaObject
		_ = json.Unmarshal([]byte(tc.raw), &o)
		if got := quotaReset(nil, o); got != tc.reset {
			t.Fatal(got)
		}
	}
	q, _ = parseQuota([]byte(`{"token_based_billing":true,"quota_reset_date":"2026-11-01","quota_snapshots":{"premium_interactions":{"unlimited":false,"percent_remaining":0,"quota_reset_at":1790812800,"overage_entitlement":100,"overage_count":100}}}`))
	if q.Admission.ResetTime != "2026-10-01T00:00:00Z" {
		t.Fatal(q.Admission)
	}
}
func TestQuotaFreeAndUnlimited(t *testing.T) {
	q, _ := parseQuota([]byte(`{"access_type_sku":"free_limited_copilot","token_based_billing":true,"quota_snapshots":{"chat":{"unlimited":false,"percent_remaining":75}}}`))
	if q.Subscription.Plan != "Copilot Free" || q.Groups[0].Buckets[0].Window != "credits" {
		t.Fatal(q)
	}
	q, _ = parseQuota([]byte(`{"quota_snapshots":{"premium_interactions":{"unlimited":true,"credits_used":12.5,"has_quota":true}}}`))
	if len(q.Groups) != 0 || len(q.Summary) != 1 || q.Summary[0].Value != 12.5 {
		t.Fatal(q)
	}
	q, _ = parseQuota([]byte(`{"quota_snapshots":{"premium_interactions":{"unlimited":true,"credits_used":12.5,"has_quota":false}}}`))
	if len(q.Summary) != 0 {
		t.Fatal(q)
	}
}
func TestQuotaHostTransportAndRedaction(t *testing.T) {
	for _, status := range []int{200, 401, 403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			bridge := &fakeBridge{handler: func(method string, payload any) (any, error) {
				if method != operation.MethodHostHTTPDo {
					t.Fatal(method)
				}
				req := payload.(localHostHTTPRequest)
				if req.URL != "https://api.github.com/copilot_internal/user" || req.Headers["Authorization"][0] != "Bearer secret-github" || req.HostCallbackID != "quota-context" {
					t.Fatal("invalid quota wire")
				}
				return contract.HTTPResponse{StatusCode: status, Headers: http.Header{"Retry-After": {"120"}}, Body: []byte(`{"copilot_plan":"individual_max","secret":"secret-upstream"}`)}, nil
			}}
			service := newService(bridge)
			req, _ := json.Marshal(localQuotaFetchRequest{QuotaFetchRequest: contract.QuotaFetchRequest{Provider: providerIdentifier, StorageJSON: []byte(`{"type":"github-copilot","github_access_token":"secret-github"}`)}, HostCallbackID: "quota-context"})
			raw, err := service.fetchQuota(req)
			if strings.Contains(string(raw), "secret") || err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("secret surfaced")
			}
			if status == 200 && err != nil {
				t.Fatal(err)
			}
			if status == 401 || status == 500 {
				if err == nil {
					t.Fatal("expected failure")
				}
			}
		})
	}
}

func TestQuotaRejectsUnsafeHostAndMalformedResponse(t *testing.T) {
	for _, host := range []string{"https://evil.example/path", "127.0.0.1", "github.com:8443", "unconfigured.example"} {
		bridge := &fakeBridge{}
		request, _ := json.Marshal(localQuotaFetchRequest{QuotaFetchRequest: contract.QuotaFetchRequest{StorageJSON: mustJSON(t, copilotStorage{Type: providerIdentifier, GitHubAccessToken: "test", GitHubHost: host})}})
		_, err := newService(bridge).fetchQuota(request)
		if err == nil || len(bridge.calls) != 0 {
			t.Fatalf("unsafe host %s was dispatched", host)
		}
	}
	for _, payload := range []string{"null", "[]", "not json"} {
		if _, err := parseQuota([]byte(payload)); err == nil {
			t.Fatal("accepted malformed quota")
		}
	}
	q, err := parseQuota([]byte(`{"quota_snapshots":{"premium_interactions":{"overage_count":1e-100000,"overage_entitlement":100}}}`))
	if err != nil || q.Admission.State != "unknown" {
		t.Fatal("unbounded decimal admitted")
	}
	endpoints, err := endpointsForHost("enterprise.example")
	if err != nil || endpoints.EntitlementURL != "https://api.enterprise.example/copilot_internal/user" {
		t.Fatal(endpoints, err)
	}
}
