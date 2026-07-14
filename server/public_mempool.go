package server

import (
	"net/http"

	"github.com/trezor/blockbook/common"
)

// Mempool entries are small (txid+time), so the API default and cap are more
// generous than the explorer UI's mempoolTxsOnPage.
const (
	mempoolTxidsInAPI       = 1000
	maxMempoolTxidsPageSize = 10000
)

// apiMempool is the machine-readable counterpart of the explorer /mempool page
// (and of bitcoind's getrawmempool): a paged list of mempool txids with first
// seen times, newest first.
func (s *PublicServer) apiMempool(r *http.Request, apiVersion int) (interface{}, error) {
	s.metrics.ExplorerViews.With(common.Labels{"action": "api-mempool"}).Inc()
	page := validateIntParam(r.URL.Query().Get("page"), 0, 0, maxPageNumber)
	pageSize := validateIntParam(r.URL.Query().Get("pageSize"), mempoolTxidsInAPI, 1, maxMempoolTxidsPageSize)
	return s.api.GetMempool(page, pageSize)
}
