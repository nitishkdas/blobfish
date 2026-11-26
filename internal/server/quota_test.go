package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func TestQuotaAPI(t *testing.T) {
	a := newTestAPI(t)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/quo", a.admin, nil)
	bucketAdmin := a.key("quo:admin")
	writer := a.key("quo:rw")

	// Only server admins set quotas; bucket admins can't raise their own.
	a.must(http.StatusForbidden, "PATCH", "/v1/buckets/quo", bucketAdmin, strings.NewReader(`{"quotaBytes": 10}`))
	a.must(http.StatusForbidden, "PATCH", "/v1/buckets/quo", bucketAdmin, strings.NewReader(`{"public": true, "quotaBytes": 10}`))
	if b, _ := a.eng.GetBucket(t.Context(), "quo"); b.Public {
		t.Error("a refused PATCH still changed something")
	}
	a.must(http.StatusBadRequest, "PATCH", "/v1/buckets/quo", a.admin, strings.NewReader(`{"quotaBytes": -1}`))
	var b bucketJSON
	json.Unmarshal(a.must(http.StatusOK, "PATCH", "/v1/buckets/quo", a.admin, strings.NewReader(`{"quotaBytes": 10}`)).Body.Bytes(), &b)
	if b.QuotaBytes != 10 {
		t.Errorf("PATCH response = %+v", b)
	}

	a.must(http.StatusOK, "PUT", "/v1/b/quo/a", writer, strings.NewReader("123456"))
	rr := a.must(http.StatusInsufficientStorage, "PUT", "/v1/b/quo/b", writer, strings.NewReader("12345"))
	if c := errCode(t, rr); c != CodeQuotaExceeded {
		t.Errorf("code = %s", c)
	}
	if rr.Header().Get("Connection") != "close" {
		t.Error("refused upload keeps the connection open")
	}
	a.must(http.StatusNotFound, "HEAD", "/v1/b/quo/b", writer, nil)

	// Usage shows in the bucket list for anyone who can read the bucket.
	var list struct{ Buckets []bucketJSON }
	json.Unmarshal(a.must(http.StatusOK, "GET", "/v1/buckets", a.key("quo:r"), nil).Body.Bytes(), &list)
	if len(list.Buckets) != 1 || list.Buckets[0].UsedBytes != 6 || list.Buckets[0].Objects != 1 || list.Buckets[0].QuotaBytes != 10 {
		t.Errorf("buckets = %+v", list.Buckets)
	}

	a.must(http.StatusOK, "PATCH", "/v1/buckets/quo", a.admin, strings.NewReader(`{"quotaBytes": 0}`))
	a.must(http.StatusOK, "PUT", "/v1/b/quo/b", writer, strings.NewReader("12345"))
}

// TestQuotaMinimum: with MinQuotaBytes set, a quota below it is refused
// (a typo, not a useful limit), 0 still removes the quota, and whoami
// tells the UI the minimum.
func TestQuotaMinimum(t *testing.T) {
	a := newTestAPI(t)
	const min = 10 << 20
	a.h = New(Options{
		Logger: slog.New(slog.DiscardHandler), MaxObjectSize: 1 << 20, Store: a.eng, Buckets: a.eng, Auth: a.auth,
		MinQuotaBytes: min,
	})
	a.must(http.StatusCreated, "PUT", "/v1/buckets/quo", a.admin, nil)
	for _, tt := range []struct {
		quota int64
		want  int
	}{
		{1, http.StatusBadRequest},
		{min - 1, http.StatusBadRequest},
		{min, http.StatusOK},
		{500 << 20, http.StatusOK},
		{0, http.StatusOK},
		{-1, http.StatusBadRequest},
	} {
		a.must(tt.want, "PATCH", "/v1/buckets/quo", a.admin, strings.NewReader(fmt.Sprintf(`{"quotaBytes": %d}`, tt.quota)))
	}
	if b, _ := a.eng.GetBucket(t.Context(), "quo"); b.QuotaBytes != 0 {
		t.Errorf("quota = %d, want 0 after the last accepted PATCH", b.QuotaBytes)
	}
	var me whoamiResponse
	json.Unmarshal(a.must(http.StatusOK, "GET", "/v1/whoami", a.admin, nil).Body.Bytes(), &me)
	if me.MinQuotaBytes != min {
		t.Errorf("whoami minQuotaBytes = %d, want %d", me.MinQuotaBytes, min)
	}
}
