package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func createWebhookJSON(t *testing.T, a *testAPI, token string, req map[string]any, want int) webhookJSON {
	t.Helper()
	body, _ := json.Marshal(req)
	var w webhookJSON
	json.Unmarshal(a.must(want, "POST", "/v1/webhooks", token, strings.NewReader(string(body))).Body.Bytes(), &w)
	return w
}

func TestWebhookAPI(t *testing.T) {
	a := newTestAPI(t)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/hooks", a.admin, nil)
	bucketAdmin := a.key("hooks:admin")
	writer := a.key("hooks:rw")
	pub := "https://app.example.com/hooks/blobfish"

	createWebhookJSON(t, a, writer, map[string]any{"bucket": "hooks", "url": pub}, http.StatusForbidden)
	createWebhookJSON(t, a, "", map[string]any{"bucket": "hooks", "url": pub}, http.StatusUnauthorized)
	for _, bad := range []map[string]any{
		{"bucket": "hooks", "url": "ftp://x.example.com/"},
		{"bucket": "hooks", "url": "https://user:pw@x.example.com/"},
		{"bucket": "hooks", "url": "/relative"},
		{"bucket": "hooks", "url": pub, "events": []string{"object.exploded"}},
		// A bucket admin may not aim at this machine or private networks.
		{"bucket": "hooks", "url": "http://localhost:3000/hook"},
		{"bucket": "hooks", "url": "http://127.0.0.1:3000/hook"},
		{"bucket": "hooks", "url": "http://[::1]/hook"},
		{"bucket": "hooks", "url": "http://10.0.0.5/hook"},
		{"bucket": "hooks", "url": "http://169.254.169.254/latest/meta-data"},
	} {
		createWebhookJSON(t, a, bucketAdmin, bad, http.StatusBadRequest)
	}

	w := createWebhookJSON(t, a, bucketAdmin, map[string]any{"bucket": "hooks", "url": pub, "events": []string{"object.deleted"}}, http.StatusCreated)
	if !strings.HasPrefix(w.Secret, "whsec_") || w.AllowPrivate || len(w.Events) != 1 {
		t.Fatalf("created = %+v", w)
	}
	// A server admin may target an app on the same machine.
	local := createWebhookJSON(t, a, a.admin, map[string]any{"bucket": "hooks", "url": "http://127.0.0.1:3000/hook"}, http.StatusCreated)
	if !local.AllowPrivate || len(local.Events) != 3 {
		t.Errorf("server admin webhook = %+v", local)
	}

	// The secret is shown once, never in listings.
	rr := a.must(http.StatusOK, "GET", "/v1/webhooks?bucket=hooks", bucketAdmin, nil)
	if strings.Contains(rr.Body.String(), "whsec_") {
		t.Errorf("listing leaks the secret: %s", rr.Body)
	}
	var list struct{ Webhooks []webhookJSON }
	json.Unmarshal(rr.Body.Bytes(), &list)
	if len(list.Webhooks) != 2 {
		t.Fatalf("list = %+v", list.Webhooks)
	}
	a.must(http.StatusForbidden, "GET", "/v1/webhooks?bucket=hooks", writer, nil)

	// Test pings queue a delivery.
	a.must(http.StatusAccepted, "POST", "/v1/webhooks/"+w.ID+"/test", bucketAdmin, nil)
	a.must(http.StatusForbidden, "POST", "/v1/webhooks/"+w.ID+"/test", writer, nil)
	if got, _ := a.eng.WebhookByID(t.Context(), w.ID); got.Pending != 1 {
		t.Errorf("pending after test = %d", got.Pending)
	}

	a.must(http.StatusForbidden, "DELETE", "/v1/webhooks/"+w.ID, writer, nil)
	a.must(http.StatusNoContent, "DELETE", "/v1/webhooks/"+w.ID, bucketAdmin, nil)
	if c := errCode(t, a.must(http.StatusNotFound, "DELETE", "/v1/webhooks/"+w.ID, bucketAdmin, nil)); c != CodeNoSuchWebhook {
		t.Errorf("code %s", c)
	}
}
