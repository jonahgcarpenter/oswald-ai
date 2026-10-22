package browser

import (
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/vault"
)

func seedLogin(t *testing.T, store *vault.Store, origin string) vault.VaultItemMeta {
	t.Helper()
	meta, err := store.AddLogin("Example", "email", "alice@example.com", "s3cret-password", "", origin)
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

func vaultScriptBackend(href, inspect, fill string) func(string) (string, error) {
	return func(expr string) (string, error) {
		switch {
		case strings.Contains(expr, "location.href"):
			return href, nil
		case strings.Contains(expr, "querySelectorAll('input"):
			return inspect, nil
		default:
			return fill, nil
		}
	}
}

func TestVaultListEmptyAndSeeded(t *testing.T) {
	handlers := NewHandlers(testDeps(t, &fakeBackend{}))
	result, err := handlers[VaultListName](testContext(), map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "No saved logins") {
		t.Fatalf("empty list missing hint: %q", result.Content)
	}
	deps := testDeps(t, &fakeBackend{})
	meta := seedLogin(t, deps.Vault, "https://example.com")
	handlers = NewHandlers(deps)
	result, err = handlers[VaultListName](testContext(), map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{meta.ID, "alice@example.com", "https://example.com"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("list missing %q: %q", want, result.Content)
		}
	}
	if strings.Contains(result.Content, "s3cret-password") {
		t.Fatal("list leaked the password")
	}
}

func TestVaultFillEndToEnd(t *testing.T) {
	inspect := `[{"index":0,"tag":"input","type":"text","name":"user","visible":true},{"index":1,"tag":"input","type":"password","visible":true}]`
	backend := &fakeBackend{vaultEval: vaultScriptBackend(`"https://example.com/login"`, inspect, `{"filled":1}`)}
	deps := testDeps(t, backend)
	meta := seedLogin(t, deps.Vault, "https://example.com")
	handlers := NewHandlers(deps)
	result, err := handlers[VaultFillName](testContext(), map[string]interface{}{"handle": meta.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, `"filled_fields": 1`) {
		t.Fatalf("fill result = %q", result.Content)
	}
	if strings.Contains(result.Content, "s3cret-password") {
		t.Fatal("fill result leaked the password")
	}
}

func TestVaultFillRefusesWrongOrigin(t *testing.T) {
	backend := &fakeBackend{vaultEval: vaultScriptBackend(`"https://evil.example/"`, `[]`, `{"filled":0}`)}
	deps := testDeps(t, backend)
	meta := seedLogin(t, deps.Vault, "https://example.com")
	handlers := NewHandlers(deps)
	if _, err := handlers[VaultFillName](testContext(), map[string]interface{}{"handle": meta.ID}); err == nil {
		t.Fatal("fill on wrong origin succeeded")
	} else if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("fill error = %v", err)
	}
	if _, err := handlers[VaultFillName](testContext(), map[string]interface{}{"handle": "vault_missing"}); err == nil {
		t.Fatal("fill with unknown handle succeeded")
	}
}

func TestVaultSaveLoginGuidesOperator(t *testing.T) {
	backend := &fakeBackend{vaultEval: vaultScriptBackend(`"https://example.com/login"`, `[]`, ``)}
	handlers := NewHandlers(testDeps(t, backend))
	result, err := handlers[VaultSaveLoginName](testContext(), map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "oswald vault add --origin https://example.com") {
		t.Fatalf("save_login guidance = %q", result.Content)
	}
}

func TestVaultFillAddressAndPaymentRefusal(t *testing.T) {
	inspect := `[{"index":0,"tag":"input","type":"text","autocomplete":"address-line1","visible":true},` +
		`{"index":1,"tag":"input","type":"text","autocomplete":"postal-code","visible":true},` +
		`{"index":2,"tag":"input","type":"text","name":"city","visible":true}]`
	backend := &fakeBackend{vaultEval: vaultScriptBackend(`"https://shop.example/checkout"`, inspect, `{"filled":1}`)}
	deps := testDeps(t, backend)
	addr, err := deps.Vault.AddAddress("Home", map[string]string{
		"address_line1": "1 Main St", "city": "Springfield", "postal_code": "12345", "country": "US",
	}, "https://shop.example")
	if err != nil {
		t.Fatal(err)
	}
	handlers := NewHandlers(deps)
	result, err := handlers[VaultFillName](testContext(), map[string]interface{}{"handle": addr.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, `"filled_fields": 3`) {
		t.Fatalf("address fill result = %q", result.Content)
	}
	card, err := deps.Vault.AddPayment("Visa", map[string]string{
		"card_number": "4111111111111111", "exp_month": "12", "exp_year": "2030", "cvc": "123",
	}, "https://shop.example")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handlers[VaultFillName](testContext(), map[string]interface{}{"handle": card.ID}); err == nil {
		t.Fatal("payment fill succeeded")
	} else if !strings.Contains(err.Error(), "card entry needs an interactive confirmation") {
		t.Fatalf("payment error = %v", err)
	}
}

func TestVaultEnterCodeMintAndRefuse(t *testing.T) {
	otpSeed := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	inspect := `[{"index":0,"tag":"input","type":"text","autocomplete":"one-time-code","visible":true}]`
	backend := &fakeBackend{vaultEval: vaultScriptBackend(`"https://example.com/2fa"`, inspect, `{"filled":1}`)}
	deps := testDeps(t, backend)
	meta, err := deps.Vault.AddLogin("Example", "email", "alice@example.com", "s3cret-password", otpSeed, "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	handlers := NewHandlers(deps)
	result, err := handlers[VaultEnterCodeName](testContext(), map[string]interface{}{"handle": meta.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, `"filled_fields": 1`) {
		t.Fatalf("enter_code result = %q", result.Content)
	}
	plain := testDeps(t, &fakeBackend{vaultEval: vaultScriptBackend(`"https://example.com/2fa"`, inspect, `{"filled":1}`)})
	plainMeta := seedLogin(t, plain.Vault, "https://example.com")
	handlers = NewHandlers(plain)
	if _, err := handlers[VaultEnterCodeName](testContext(), map[string]interface{}{"handle": plainMeta.ID}); err == nil {
		t.Fatal("enter_code without seed succeeded")
	} else if !strings.Contains(err.Error(), "prompt_unavailable") {
		t.Fatalf("enter_code error = %v", err)
	}
}

func TestVaultFillNeedsHandle(t *testing.T) {
	handlers := NewHandlers(testDeps(t, &fakeBackend{}))
	if _, err := handlers[VaultFillName](testContext(), map[string]interface{}{}); err == nil {
		t.Fatal("fill without handle succeeded")
	}
	if _, err := handlers[VaultEnterCodeName](testContext(), map[string]interface{}{}); err == nil {
		t.Fatal("enter_code without handle succeeded")
	}
}
