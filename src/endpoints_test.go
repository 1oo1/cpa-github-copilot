package main

import "testing"

func TestNormalizeGitHubHost(t *testing.T) {
	for _, test := range []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "default", input: "", want: "github.com"},
		{name: "hostname", input: "Company.GHE.com", want: "company.ghe.com"},
		{name: "https URL", input: "https://company.ghe.com/", want: "company.ghe.com"},
		{name: "http", input: "http://company.ghe.com", wantErr: true},
		{name: "path", input: "https://company.ghe.com/path", wantErr: true},
		{name: "userinfo", input: "https://user@company.ghe.com", wantErr: true},
		{name: "port", input: "https://company.ghe.com:8443", wantErr: true},
		{name: "ip", input: "127.0.0.1", wantErr: true},
		{name: "localhost", input: "localhost", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, errHost := normalizeGitHubHost(test.input)
			if (errHost != nil) != test.wantErr {
				t.Fatalf("normalizeGitHubHost(%q) error = %v", test.input, errHost)
			}
			if got != test.want {
				t.Fatalf("normalizeGitHubHost(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestAPIBaseFromSessionToken(t *testing.T) {
	for _, test := range []struct {
		name  string
		token string
		host  string
		want  string
	}{
		{name: "individual", token: "tid=x;proxy-ep=proxy.individual.githubcopilot.com;exp=1", host: "github.com", want: "https://api.individual.githubcopilot.com"},
		{name: "business", token: "proxy-ep=proxy.business.githubcopilot.com", host: "github.com", want: "https://api.business.githubcopilot.com"},
		{name: "malicious host falls back", token: "proxy-ep=proxy.example.invalid", host: "github.com", want: "https://api.individual.githubcopilot.com"},
		{name: "missing falls back", token: "tid=x", host: "company.ghe.com", want: "https://copilot-api.company.ghe.com"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := apiBaseFromSessionToken(test.token, test.host); got != test.want {
				t.Fatalf("apiBaseFromSessionToken() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestValidateVerificationURL(t *testing.T) {
	if got, errURL := validateVerificationURL("https://github.com/login/device#fragment", "github.com"); errURL != nil || got != "https://github.com/login/device" {
		t.Fatalf("valid URL = %q, %v", got, errURL)
	}
	for _, raw := range []string{"http://github.com/login/device", "https://evil.example/login/device", "file:///tmp/run", "$(id)"} {
		if _, errURL := validateVerificationURL(raw, "github.com"); errURL == nil {
			t.Fatalf("validateVerificationURL(%q) unexpectedly succeeded", raw)
		}
	}
}

func TestSameOrigin(t *testing.T) {
	base := "https://api.individual.githubcopilot.com"
	if !sameOrigin(base+"/responses?x=1", base) {
		t.Fatal("same origin URL was rejected")
	}
	for _, raw := range []string{"http://api.individual.githubcopilot.com/responses", "https://evil.example/responses", "https://api.individual.githubcopilot.com.evil/responses"} {
		if sameOrigin(raw, base) {
			t.Fatalf("sameOrigin(%q) unexpectedly succeeded", raw)
		}
	}
}
