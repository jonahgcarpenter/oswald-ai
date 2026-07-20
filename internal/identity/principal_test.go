package identity

import "testing"

func TestPrincipalValidityAndAuthentication(t *testing.T) {
	tests := []struct {
		name          string
		principal     Principal
		valid         bool
		authenticated bool
	}{
		{name: "zero", principal: Principal{}, valid: false},
		{name: "unknown assurance", principal: Principal{CanonicalUserID: "usr_1", Gateway: "test", ExternalID: "one", Assurance: "unknown"}, valid: false},
		{name: "mismatched assurance", principal: Principal{CanonicalUserID: "usr_1", Gateway: "websocket", ExternalID: "one", Assurance: AssuranceDiscordGateway}, valid: false},
		{name: "self asserted", principal: Principal{CanonicalUserID: "usr_1", Gateway: "websocket", ExternalID: "one", Assurance: AssuranceSelfAsserted}, valid: true},
		{name: "signed websocket", principal: Principal{CanonicalUserID: "usr_1", Gateway: "websocket", ExternalID: "one", Assurance: AssuranceWebSocketSignedToken}, valid: true, authenticated: true},
		{name: "signed websocket on discord", principal: Principal{CanonicalUserID: "usr_1", Gateway: "discord", ExternalID: "one", Assurance: AssuranceWebSocketSignedToken}, valid: false},
		{name: "discord", principal: Principal{CanonicalUserID: "usr_1", Gateway: "discord", ExternalID: "1", Assurance: AssuranceDiscordGateway}, valid: true, authenticated: true},
		{name: "imessage", principal: Principal{CanonicalUserID: "usr_1", Gateway: "imessage", ExternalID: "+15551234567", Assurance: AssuranceBlueBubblesWebhook}, valid: true, authenticated: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.principal.Valid(); got != tt.valid {
				t.Fatalf("Valid() = %t, want %t", got, tt.valid)
			}
			if got := tt.principal.Authenticated(); got != tt.authenticated {
				t.Fatalf("Authenticated() = %t, want %t", got, tt.authenticated)
			}
		})
	}
}
