package main

import "testing"

// The two Volvo 403s must be classified differently: a missing-scope 403 needs
// re-auth; a quota 403 is transient and must NOT trigger a re-auth prompt (nor
// be confused with the OAuth invalid_grant case).
func TestForbiddenScopeVsQuota(t *testing.T) {
	scope := []string{`status 403: {"error":{"message":"Client not allowed to access connected-vehicle API"}}`}
	quota := []string{`status 403: {"error":{"message":"Out of call volume quota. Quota will be replenished in 00:12:00."}}`}
	grant := []string{"get token: token endpoint returned 400: invalid_grant"}

	if !anyForbiddenScope(scope) {
		t.Error("scope 403 should be classified as forbidden-scope (re-auth)")
	}
	if anyForbiddenScope(quota) {
		t.Error("quota 403 must NOT be classified as forbidden-scope")
	}
	if anyInvalidGrant(quota) || anyForbiddenScope(grant) {
		t.Error("quota/grant errors cross-classified")
	}
	if !anyInvalidGrant(grant) {
		t.Error("invalid_grant should be detected")
	}
}
