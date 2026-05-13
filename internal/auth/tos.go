package auth

// settingSubscriptionTOSAcknowledged is the key under which we record that
// the user has read and accepted the subscription-auth TOS warning.
const settingSubscriptionTOSAcknowledged = "subscription_tos_acknowledged"

// TOSText is the warning shown to the user on first subscription-auth
// action (login or import). Stored as a constant so the dashboard and
// CLI display identical copy. The wording is part of the user-facing
// contract — see tos_test.go which asserts the required phrases are
// present.
const TOSText = `Subscription auth uses your existing LLM subscription credentials
(Claude Pro/Max, ChatGPT Plus/Pro, GitHub Copilot, or Gemini).

Some providers may restrict third-party use of subscription tokens in
their Terms of Service, which could change at any time. If your access
is revoked, you can switch the affected connection to API-key auth and
supply an API key — your other connections continue to work.

Sage-router does not store your raw token unencrypted; all credentials
are encrypted at rest. Refresh tokens are used only to obtain new
access tokens, never sent to third parties.

Proceed?`

// settingsStore is the narrow contract the TOS helpers need. Both the
// dashboard route handler and the CLI satisfy this via internal/store.Store.
type settingsStore interface {
	GetSetting(key string) (string, error)
	SetSetting(key, value string) error
}

// IsTOSAcknowledged reports whether the user has accepted the
// subscription-auth TOS at least once. Errors from the store are
// treated as "not acknowledged" so a transient read failure forces a
// fresh disclosure rather than silently bypassing it (fail-closed for
// the consent gate).
func IsTOSAcknowledged(s settingsStore) bool {
	v, err := s.GetSetting(settingSubscriptionTOSAcknowledged)
	if err != nil {
		return false
	}
	return v == "1"
}

// AcknowledgeTOS records that the user accepted the TOS warning. Idempotent.
func AcknowledgeTOS(s settingsStore) error {
	return s.SetSetting(settingSubscriptionTOSAcknowledged, "1")
}
