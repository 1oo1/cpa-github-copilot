package contract

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// ModelInfo describes a model contributed by a provider.
type ModelInfo struct {
	// ID is the stable model identifier used in API requests.
	ID string
	// Object is the API object type, usually "model".
	Object string
	// Created is the Unix timestamp when the model metadata was created.
	Created int64
	// OwnedBy identifies the model owner or provider.
	OwnedBy string
	// Type classifies the model capability family.
	Type string
	// DisplayName is the user-facing model name.
	DisplayName string
	// Name is the provider-native model name.
	Name string
	// Version identifies the model revision when available.
	Version string
	// Description is a short user-facing model summary.
	Description string
	// InputTokenLimit is the maximum accepted input token count.
	InputTokenLimit int64
	// OutputTokenLimit is the maximum generated output token count.
	OutputTokenLimit int64
	// SupportedGenerationMethods lists supported generation method names.
	SupportedGenerationMethods []string
	// ContextLength is the maximum combined context length.
	ContextLength int64
	// MaxCompletionTokens is the maximum completion token count.
	MaxCompletionTokens int64
	// SupportedParameters lists request parameters supported by the model.
	SupportedParameters []string
	// SupportedInputModalities lists accepted input modality names.
	SupportedInputModalities []string
	// SupportedOutputModalities lists produced output modality names.
	SupportedOutputModalities []string
	// Thinking describes optional reasoning controls for the model.
	Thinking *ThinkingSupport
	// UserDefined reports whether the model was provided by user configuration.
	UserDefined bool
}

// ThinkingSupport describes supported reasoning budget controls.
type ThinkingSupport struct {
	// Min is the minimum accepted reasoning budget.
	Min int
	// Max is the maximum accepted reasoning budget.
	Max int
	// ZeroAllowed reports whether disabling reasoning is supported.
	ZeroAllowed bool
	// DynamicAllowed reports whether automatic reasoning budget selection is supported.
	DynamicAllowed bool
	// Levels lists supported named reasoning levels.
	Levels []string
}

// AuthData describes a Copilot provider auth record exchanged with the service.
type AuthData struct {
	// Provider is the provider key associated with the auth.
	Provider string
	// ID is the stable service auth identifier.
	ID string
	// FileName is the source or persisted auth file name.
	FileName string
	// Label is the user-facing auth label.
	Label string
	// Prefix is the configured model prefix for this auth.
	Prefix string
	// ProxyURL is the auth-specific proxy URL when configured.
	ProxyURL string
	// Disabled reports whether the auth should be skipped.
	Disabled bool
	// StorageJSON contains provider-owned persisted auth data.
	StorageJSON []byte
	// Metadata contains mutable service-managed auth metadata.
	Metadata map[string]any
	// Attributes contains immutable routing and provider attributes.
	Attributes map[string]string
	// NextRefreshAfter is the earliest time the service should refresh this auth.
	NextRefreshAfter time.Time
}

// AuthParseRequest describes auth material offered to a provider parser.
type AuthParseRequest struct {
	// Provider is the provider key being parsed.
	Provider string
	// Path is the source path of the auth material when available.
	Path string
	// FileName is the auth file name.
	FileName string
	// RawJSON contains the raw auth file payload.
	RawJSON []byte
}

// AuthParseResponse returns the parser decision and parsed auth data.
type AuthParseResponse struct {
	// Handled reports whether the provider recognized the auth material.
	Handled bool
	// Auth is the parsed auth record when Handled is true.
	Auth AuthData
	// Auths contains multiple parsed auth records when one auth material expands into several runtime auths.
	Auths []AuthData
}

// AuthLoginStartRequest asks a provider to start a provider login flow.
type AuthLoginStartRequest struct {
	// Provider is the provider key for the login flow.
	Provider string
	// BaseURL is the service callback or login base URL.
	BaseURL string

	// Metadata carries provider-defined login context.
	Metadata map[string]any
}

// AuthLoginStartResponse returns login flow state for polling.
type AuthLoginStartResponse struct {
	// Provider is the provider key for the login flow.
	Provider string
	// URL is the user-facing login URL.
	URL string
	// State is the opaque provider login state used for polling.
	State string
	// ExpiresAt is the time when this login flow expires.
	ExpiresAt time.Time
	// Metadata carries provider-defined polling context.
	Metadata map[string]any
}

// AuthLoginPollRequest asks a provider to poll a provider login flow.
type AuthLoginPollRequest struct {
	// Provider is the provider key for the login flow.
	Provider string
	// State is the opaque provider login state returned by StartLogin.
	State string

	// Metadata carries provider-defined polling context.
	Metadata map[string]any
}

// AuthLoginStatus describes the current provider login state.
type AuthLoginStatus string

const (
	// AuthLoginStatusPending means the login flow is still waiting.
	AuthLoginStatusPending AuthLoginStatus = "pending"
	// AuthLoginStatusSuccess means the login flow produced auth data.
	AuthLoginStatusSuccess AuthLoginStatus = "success"
	// AuthLoginStatusError means the login flow failed.
	AuthLoginStatusError AuthLoginStatus = "error"
)

// AuthLoginPollResponse returns the login poll status and auth data.
type AuthLoginPollResponse struct {
	// Status is the current login flow state.
	Status AuthLoginStatus
	// Message contains provider-facing login progress or error text.
	Message string
	// Auth is the completed auth record when Status is success.
	Auth AuthData
	// Auths contains multiple completed auth records when one login flow expands into several runtime auths.
	Auths []AuthData
}

// AuthRefreshRequest asks a provider to refresh provider auth data.
type AuthRefreshRequest struct {
	// AuthID identifies the auth record to refresh.
	AuthID string
	// AuthProvider identifies the credential provider.
	AuthProvider string
	// StorageJSON contains provider-owned persisted auth data.
	StorageJSON []byte
	// Metadata contains mutable service-managed auth metadata.
	Metadata map[string]any
	// Attributes contains immutable routing and provider attributes.
	Attributes map[string]string
}

// AuthRefreshResponse returns refreshed provider auth data.
type AuthRefreshResponse struct {
	// Auth is the refreshed auth record.
	Auth AuthData
	// NextRefreshAfter is the earliest time the service should refresh again.
	NextRefreshAfter time.Time
}

// AuthModelRequest carries auth context for provider model discovery.
type AuthModelRequest struct {

	// AuthID identifies the auth record used for discovery.
	AuthID string
	// AuthProvider identifies the credential provider.
	AuthProvider string
	// StorageJSON contains provider-owned persisted auth data.
	StorageJSON []byte
	// Metadata contains mutable service-managed auth metadata.
	Metadata map[string]any
	// Attributes contains immutable routing and provider attributes.
	Attributes map[string]string
}

// ModelResponse returns provider and model metadata discovered by a provider.
type ModelResponse struct {
	// Provider is the provider key associated with the returned models.
	Provider string
	// Models is the complete set of discovered provider models.
	Models []ModelInfo
	// AuthUpdate contains updated auth data from model discovery when needed.
	AuthUpdate AuthData
}

// HTTPWireProfile configures transport-level wire representation for provider HTTP requests.
type HTTPWireProfile struct {
	// HTTP1Only forces the transport to use HTTP/1.1 and disables HTTP/2 negotiation.
	HTTP1Only bool `json:"http1_only,omitempty"`
	// DisableAutoCompression prevents transparent decompression and automatic Accept-Encoding injection.
	DisableAutoCompression bool `json:"disable_auto_compression,omitempty"`
	// HeaderProfile defines desired header-name order and exact casing on the wire.
	HeaderProfile []string `json:"header_profile,omitempty"`
}

// HTTPRequest describes an upstream HTTP request issued through the service.
type HTTPRequest struct {
	// Method is the HTTP method.
	Method string
	// URL is the absolute upstream URL.
	URL string
	// Headers contains request headers.
	Headers http.Header
	// Body contains the raw request body.
	Body []byte
	// WireProfile specifies optional outbound HTTP wire profile settings.
	WireProfile *HTTPWireProfile `json:"wire_profile,omitempty"`
}

// HTTPResponse describes a non-streaming service HTTP response.
type HTTPResponse struct {
	// StatusCode is the upstream HTTP status code.
	StatusCode int
	// Headers contains upstream response headers.
	Headers http.Header
	// Body contains the raw response body.
	Body []byte
}

// ExecutorHTTPRequest describes an executor-owned HTTP request.
type ExecutorHTTPRequest struct {
	// AuthID identifies the selected credential.
	AuthID string
	// AuthProvider identifies the credential provider.
	AuthProvider string
	// Method is the HTTP method.
	Method string
	// URL is the absolute upstream URL.
	URL string
	// Headers contains request headers.
	Headers http.Header
	// Body contains the raw request body.
	Body []byte
	// StorageJSON contains provider-owned auth storage for this concrete auth.
	StorageJSON []byte
	// Metadata contains mutable service-managed auth metadata.
	Metadata map[string]any
	// Attributes contains immutable routing and provider attributes.
	Attributes map[string]string
}

// ExecutorHTTPResponse describes an executor-owned HTTP response.
type ExecutorHTTPResponse struct {
	// StatusCode is the upstream HTTP status code.
	StatusCode int
	// Headers contains upstream response headers.
	Headers http.Header
	// Body contains the raw response body.
	Body []byte
}

// ExecutorRequest describes a model execution or token counting call.
type ExecutorRequest struct {
	// AuthID identifies the selected credential.
	AuthID string
	// AuthProvider identifies the credential provider.
	AuthProvider string
	// Model is the requested model identifier.
	Model string
	// Format is the target request or response protocol format.
	Format string
	// Stream reports whether the request expects streaming output.
	Stream bool
	// Alt carries an alternate route or mode suffix when present.
	Alt string
	// Headers contains request headers passed to the executor.
	Headers http.Header
	// Query contains request query parameters passed to the executor.
	Query url.Values
	// OriginalRequest contains the raw client request body.
	OriginalRequest []byte
	// SourceFormat is the original client protocol format.
	SourceFormat string
	// Payload contains the translated provider payload.
	Payload []byte
	// Metadata is an extension bag for service and provider coordination data.
	Metadata map[string]any
	// StorageJSON contains provider-owned auth storage for this concrete auth.
	StorageJSON []byte
	// AuthMetadata contains mutable service-managed auth metadata.
	AuthMetadata map[string]any
	// AuthAttributes contains immutable routing and provider attributes.
	AuthAttributes map[string]string
}

// ExecutorResponse returns a non-streaming executor result.
type ExecutorResponse struct {
	// Payload contains the raw response body.
	Payload []byte
	// Headers contains response headers to forward or inspect.
	Headers http.Header
	// Metadata is an extension bag for executor-specific response data.
	Metadata map[string]any
}

// QuotaFetchRequest carries credential context for querying quota.
type QuotaFetchRequest struct {
	// AuthIndex identifies the credential index.
	AuthIndex string `json:"auth_index"`
	// AuthID identifies the credential ID.
	AuthID string `json:"auth_id"`
	// Provider identifies the credential provider.
	Provider string `json:"provider"`
	// StorageJSON contains provider-owned persisted auth data.
	StorageJSON []byte `json:"storage_json,omitempty"`
	// Metadata contains mutable service-managed auth metadata.
	Metadata map[string]any `json:"metadata,omitempty"`
	// Attributes contains immutable routing and provider attributes.
	Attributes map[string]string `json:"attributes,omitempty"`
}

// QuotaSubscription describes plan and tier information in normalized quota.
type QuotaSubscription struct {
	Plan     string `json:"plan,omitempty"`
	TierName string `json:"tierName,omitempty"`
	TierID   string `json:"tierId,omitempty"`
}

// UnmarshalJSON supports both camelCase and snake_case field names.
func (s *QuotaSubscription) UnmarshalJSON(data []byte) error {
	type Alias QuotaSubscription
	aux := struct {
		*Alias
		AltTierName string `json:"tier_name"`
		AltTierID   string `json:"tier_id"`
	}{
		Alias: (*Alias)(s),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if s.TierName == "" && aux.AltTierName != "" {
		s.TierName = aux.AltTierName
	}
	if s.TierID == "" && aux.AltTierID != "" {
		s.TierID = aux.AltTierID
	}
	return nil
}

// QuotaGroup describes a group of quota buckets in normalized quota.
type QuotaGroup struct {
	DisplayName string        `json:"displayName,omitempty"`
	Buckets     []QuotaBucket `json:"buckets,omitempty"`
}

// UnmarshalJSON supports both camelCase and snake_case field names.
func (g *QuotaGroup) UnmarshalJSON(data []byte) error {
	type Alias QuotaGroup
	aux := struct {
		*Alias
		AltDisplayName string `json:"display_name"`
	}{
		Alias: (*Alias)(g),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if g.DisplayName == "" && aux.AltDisplayName != "" {
		g.DisplayName = aux.AltDisplayName
	}
	return nil
}

// QuotaBucket describes a single quota window or limit bucket.
type QuotaBucket struct {
	Window            string  `json:"window,omitempty"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime,omitempty"`
	Description       string  `json:"description,omitempty"`
}

// UnmarshalJSON supports both camelCase and snake_case field names.
func (b *QuotaBucket) UnmarshalJSON(data []byte) error {
	type Alias QuotaBucket
	aux := struct {
		*Alias
		RemainingFraction    *float64 `json:"remainingFraction"`
		AltRemainingFraction *float64 `json:"remaining_fraction"`
		AltResetTime         string   `json:"reset_time"`
	}{
		Alias: (*Alias)(b),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.RemainingFraction != nil {
		b.RemainingFraction = *aux.RemainingFraction
	} else if aux.AltRemainingFraction != nil {
		b.RemainingFraction = *aux.AltRemainingFraction
	}
	if b.ResetTime == "" && aux.AltResetTime != "" {
		b.ResetTime = aux.AltResetTime
	}
	return nil
}

// QuotaMetric is a provider-defined, bounded numeric account summary for management UI rendering.
// Format is "number" or "currency"; Currency is an ISO 4217 code when Format is "currency".
type QuotaMetric struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Value    float64 `json:"value"`
	Unit     string  `json:"unit,omitempty"`
	Format   string  `json:"format,omitempty"`
	Currency string  `json:"currency,omitempty"`
}

// QuotaFetchResponse carries normalized quota information for management UI rendering.
// QuotaAdmission is a provider observation, not permission to clear a service latch.
type QuotaAdmission struct {
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
	ResetTime string `json:"resetTime,omitempty"`
}
type QuotaFetchResponse struct {
	Admission         *QuotaAdmission `json:"admission,omitempty"`
	ObservedAt        time.Time       `json:"observedAt,omitempty"`
	Stale             bool            `json:"stale,omitempty"`
	RetryAfterSeconds int             `json:"retryAfterSeconds,omitempty"`

	Subscription       *QuotaSubscription `json:"subscription,omitempty"`
	Summary            []QuotaMetric      `json:"summary,omitempty"`
	ServerTimeOffsetMs int64              `json:"serverTimeOffsetMs,omitempty"`
	Groups             []QuotaGroup       `json:"groups,omitempty"`
}

// UnmarshalJSON supports both camelCase and snake_case field names.
func (r *QuotaFetchResponse) UnmarshalJSON(data []byte) error {
	type Alias QuotaFetchResponse
	aux := struct {
		*Alias
		AltServerTimeOffsetMs int64 `json:"server_time_offset_ms"`
	}{
		Alias: (*Alias)(r),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if r.ServerTimeOffsetMs == 0 && aux.AltServerTimeOffsetMs != 0 {
		r.ServerTimeOffsetMs = aux.AltServerTimeOffsetMs
	}
	return nil
}
