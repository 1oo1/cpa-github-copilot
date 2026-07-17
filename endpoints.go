package main

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

var hostnameLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

type providerEndpoints struct {
	GitHubHost      string
	DeviceCodeURL   string
	AccessTokenURL  string
	CopilotTokenURL string
	FallbackAPIURL  string
	GitHubUserURL   string
}

func normalizeGitHubHost(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = defaultGitHubHost
	}
	candidate := raw
	if !strings.Contains(candidate, "://") {
		candidate = "https://" + candidate
	}
	u, errParse := url.Parse(candidate)
	if errParse != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "", fmt.Errorf("github_host must be an HTTPS hostname")
	}
	if u.User != nil || u.Port() != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("github_host must not contain credentials, a port, path, query, or fragment")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if errHost := validateHostname(host); errHost != nil {
		return "", fmt.Errorf("invalid github_host")
	}
	if net.ParseIP(host) != nil || host == "localhost" || !strings.Contains(host, ".") {
		return "", fmt.Errorf("github_host must be a non-local DNS hostname")
	}
	return host, nil
}

func validateHostname(host string) error {
	if host == "" || len(host) > 253 || strings.ContainsAny(host, "\x00\r\n\t ") {
		return fmt.Errorf("invalid hostname")
	}
	for _, label := range strings.Split(host, ".") {
		if !hostnameLabel.MatchString(label) {
			return fmt.Errorf("invalid hostname")
		}
	}
	return nil
}

func endpointsForHost(host string) (providerEndpoints, error) {
	normalized, errHost := normalizeGitHubHost(host)
	if errHost != nil {
		return providerEndpoints{}, errHost
	}
	endpoints := providerEndpoints{
		GitHubHost:     normalized,
		DeviceCodeURL:  "https://" + normalized + "/login/device/code",
		AccessTokenURL: "https://" + normalized + "/login/oauth/access_token",
	}
	if normalized == defaultGitHubHost {
		endpoints.CopilotTokenURL = "https://api.github.com/copilot_internal/v2/token"
		endpoints.FallbackAPIURL = "https://api.individual.githubcopilot.com"
		endpoints.GitHubUserURL = "https://api.github.com/user"
	} else {
		endpoints.CopilotTokenURL = "https://api." + normalized + "/copilot_internal/v2/token"
		endpoints.FallbackAPIURL = "https://copilot-api." + normalized
		endpoints.GitHubUserURL = "https://" + normalized + "/api/v3/user"
	}
	return endpoints, nil
}

func apiBaseFromSessionToken(token, githubHost string) string {
	endpoints, errEndpoints := endpointsForHost(githubHost)
	if errEndpoints != nil {
		return ""
	}
	for _, field := range strings.Split(token, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok || key != "proxy-ep" {
			continue
		}
		host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
		if errHost := validateHostname(host); errHost != nil || net.ParseIP(host) != nil || !trustedCopilotHost(host, endpoints.GitHubHost) {
			break
		}
		if strings.HasPrefix(host, "proxy.") {
			host = "api." + strings.TrimPrefix(host, "proxy.")
		}
		return "https://" + host
	}
	return endpoints.FallbackAPIURL
}

func trustedCopilotHost(host, githubHost string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	githubHost = strings.ToLower(strings.TrimSuffix(githubHost, "."))
	return host == "githubcopilot.com" || strings.HasSuffix(host, ".githubcopilot.com") ||
		host == githubHost || strings.HasSuffix(host, "."+githubHost)
}

func validateVerificationURL(raw, githubHost string) (string, error) {
	u, errParse := url.Parse(strings.TrimSpace(raw))
	if errParse != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return "", fmt.Errorf("device authorization returned an untrusted verification URL")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host != githubHost || u.Port() != "" {
		return "", fmt.Errorf("device authorization returned an untrusted verification URL")
	}
	u.Fragment = ""
	return u.String(), nil
}

func verificationURLWithCode(raw, userCode string) string {
	u, errParse := url.Parse(raw)
	if errParse != nil {
		return raw
	}
	query := u.Query()
	query.Set("user_code", userCode)
	u.RawQuery = query.Encode()
	return u.String()
}

func sameOrigin(raw, base string) bool {
	u, errURL := url.Parse(raw)
	b, errBase := url.Parse(base)
	if errURL != nil || errBase != nil || u.Scheme != "https" || b.Scheme != "https" {
		return false
	}
	return strings.EqualFold(u.Scheme, b.Scheme) && strings.EqualFold(u.Host, b.Host) && u.User == nil
}
