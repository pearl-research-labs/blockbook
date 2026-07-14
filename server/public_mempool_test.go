package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicServerAPIMempool(t *testing.T) {
	parser, chain := setupChain(t)
	s, dbpath := setupPublicHTTPServer(parser, chain, t, false)
	defer closeAndDestroyPublicServer(t, s, dbpath)
	s.ConnectFullPublicInterface()
	ts := httptest.NewServer(s.https.Handler)
	defer ts.Close()

	// The test harness never syncs the fake chain mempool, so entries are
	// always empty; paging metadata still reflects the sanitized parameters.
	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "default paging",
			url:  "/api/v2/mempool/",
			want: `{"page":1,"totalPages":1,"itemsOnPage":1000,"mempool":[],"mempoolSize":0}`,
		},
		{
			name: "custom page size",
			url:  "/api/v2/mempool/?page=1&pageSize=25",
			want: `{"page":1,"totalPages":1,"itemsOnPage":25,"mempool":[],"mempoolSize":0}`,
		},
		{
			name: "page size above maximum is clamped",
			url:  "/api/v2/mempool/?pageSize=999999",
			want: `{"page":1,"totalPages":1,"itemsOnPage":10000,"mempool":[],"mempoolSize":0}`,
		},
		{
			name: "invalid params fall back to defaults",
			url:  "/api/v2/mempool/?page=-3&pageSize=xyz",
			want: `{"page":1,"totalPages":1,"itemsOnPage":1000,"mempool":[],"mempoolSize":0}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := http.DefaultClient.Do(newGetRequest(ts.URL + tt.url))
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusOK)
			}
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(body)); got != tt.want {
				t.Errorf("body = %s, want %s", got, tt.want)
			}
		})
	}
}
