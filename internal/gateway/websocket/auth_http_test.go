package websocket

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type authTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ClientID     string `json:"client_id"`
}

func TestDeviceTokenRefreshAndRevokeHTTPFlow(t *testing.T) {
	wg, broker, _ := newWebSocketTestGateway(t)
	defer broker.Shutdown()

	deviceResponse := callAuthHandler(t, wg.handleDeviceAuthorization, `{"client_name":"Laptop"}`)
	var device struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
		Interval   int    `json:"interval"`
	}
	if err := json.Unmarshal(deviceResponse.Body.Bytes(), &device); err != nil {
		t.Fatal(err)
	}
	if device.DeviceCode == "" || device.UserCode == "" || device.Interval != 5 {
		t.Fatalf("unexpected device response: %+v", device)
	}
	userID, err := wg.Links.EnsureAccount("websocket", "alice", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wg.Auth.ApproveForUser(context.Background(), userID, "Alice", device.UserCode); err != nil {
		t.Fatal(err)
	}

	tokenResponse := callAuthHandler(t, wg.handleToken, `{"grant_type":"device_code","device_code":"`+device.DeviceCode+`"}`)
	var first authTokenResponse
	if err := json.Unmarshal(tokenResponse.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.AccessToken == "" || first.RefreshToken == "" || first.ClientID == "" {
		t.Fatalf("unexpected token response: %+v", first)
	}

	refreshResponse := callAuthHandler(t, wg.handleToken, `{"grant_type":"refresh_token","refresh_token":"`+first.RefreshToken+`"}`)
	var refreshed authTokenResponse
	if err := json.Unmarshal(refreshResponse.Body.Bytes(), &refreshed); err != nil {
		t.Fatal(err)
	}
	if refreshed.RefreshToken == "" || refreshed.RefreshToken == first.RefreshToken || refreshed.ClientID != first.ClientID {
		t.Fatalf("unexpected refreshed credentials: %+v", refreshed)
	}

	revokeResponse := callAuthHandler(t, wg.handleRevoke, `{"refresh_token":"`+refreshed.RefreshToken+`"}`)
	if revokeResponse.Code != http.StatusOK {
		t.Fatalf("revoke status = %d", revokeResponse.Code)
	}
	if _, err := wg.Auth.VerifyAccess(context.Background(), refreshed.AccessToken); err == nil {
		t.Fatal("revoked access token remained valid")
	}
}

func callAuthHandler(t *testing.T, handler http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("auth response status=%d body=%s", response.Code, response.Body.String())
	}
	return response
}
