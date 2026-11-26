package server

import (
	"encoding/json"
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
