package handler

import "testing"

// A webhook URL is audited as scheme and host only: a token in the path
// or query, or credentials before the host, are not written down.
func TestWebhookTarget(t *testing.T) {
	for in, want := range map[string]string{
		"https://hooks.slack.com/services/T0/B0/secret":   "https://hooks.slack.com",
		"https://user:pass@example.com:8443/hook?token=x": "https://example.com:8443",
		"://bad": "",
	} {
		if got := webhookTarget(in); got != want {
			t.Errorf("webhookTarget(%q) = %q, want %q", in, got, want)
		}
	}
}
