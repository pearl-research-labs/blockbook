package pearl

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/url"
	"strconv"
	"strings"

	"github.com/golang/glog"
	"github.com/juju/errors"
	"github.com/pearl-research-labs/pearl/node/btcjson"
	"github.com/pearl-research-labs/pearl/node/rpcclient"
	"github.com/trezor/blockbook/bchain"
	"github.com/trezor/blockbook/common"
)

// Configuration is the blockchaincfg.json shape for Bitcoin-type chains.
// It mirrors the fields Blockbook expects for RPC auth + mempool settings.
type Configuration struct {
	CoinName               string `json:"coin_name"`
	CoinShortcut           string `json:"coin_shortcut"`
	RPCURL                 string `json:"rpc_url"`
	RPCUser                string `json:"rpc_user"`
	RPCPass                string `json:"rpc_pass"`
	RPCTimeout             int    `json:"rpc_timeout"`
	Parse                  bool   `json:"parse"`
	MessageQueueBinding    string `json:"message_queue_binding"`
	Subversion             string `json:"subversion"`
	AddressFormat          string `json:"address_format"`
	XPubMagic              uint32 `json:"xpub_magic,omitempty"`
	BlockAddressesToKeep   int    `json:"block_addresses_to_keep"`
	MempoolWorkers         int    `json:"mempool_workers"`
	MempoolSubWorkers      int    `json:"mempool_sub_workers"`
	MempoolResyncBatchSize int    `json:"mempool_resync_batch_size,omitempty"`
}

// PearlRPC is a Blockbook chain implementation for Pearl that uses pearl/node libraries.
//
// Unlike the generic btc implementation in this repo, this avoids martinboehm/* dependencies.
type PearlRPC struct {
	*bchain.BaseChain

	cfg         Configuration
	client      *rpcclient.Client
	mempool     *bchain.MempoolBitcoinType
	pushHandler func(bchain.NotificationType)
	mq          *bchain.MQ
}

// NewPearlRPC returns a new PearlRPC instance.
func NewPearlRPC(config json.RawMessage, pushHandler func(bchain.NotificationType)) (bchain.BlockChain, error) {
	var c Configuration
	if err := json.Unmarshal(config, &c); err != nil {
		return nil, errors.Annotate(err, "Invalid configuration file")
	}
	if c.RPCURL == "" {
		return nil, errors.New("rpc_url is required")
	}
	// keep at least 100 mappings block->addresses to allow rollback
	if c.BlockAddressesToKeep < 100 {
		c.BlockAddressesToKeep = 100
	}
	// at least 1 mempool worker/subworker for synchronous mempool synchronization
	if c.MempoolWorkers < 1 {
		c.MempoolWorkers = 1
	}
	if c.MempoolSubWorkers < 1 {
		c.MempoolSubWorkers = 1
	}
	if c.MempoolResyncBatchSize < 1 {
		c.MempoolResyncBatchSize = 1
	}

	u, err := url.Parse(c.RPCURL)
	if err != nil {
		return nil, errors.Annotate(err, "Invalid rpc_url")
	}
	host := u.Host
	if host == "" {
		host = c.RPCURL
	}
	disableTLS := true
	if strings.EqualFold(u.Scheme, "https") {
		disableTLS = false
	}

	connCfg := &rpcclient.ConnConfig{
		Host:         host,
		User:         c.RPCUser,
		Pass:         c.RPCPass,
		HTTPPostMode: true,
		DisableTLS:   disableTLS,
	}
	rc, err := rpcclient.New(connCfg, nil)
	if err != nil {
		return nil, err
	}

	s := &PearlRPC{
		BaseChain:   &bchain.BaseChain{},
		cfg:         c,
		client:      rc,
		pushHandler: pushHandler,
	}
	return s, nil
}

func (d *PearlRPC) Initialize() error {
	// Determine chain name from backend.
	ci, err := d.GetChainInfo()
	if err != nil {
		return err
	}

	// Parser must exist even when parse=false (address/script conversions).
	d.Parser = NewPearlParser(ci.Chain, &d.cfg)

	// Set Network/Testnet flags for Blockbook.
	// Pearl node uses "main" / "test" style chain names.
	if strings.Contains(strings.ToLower(ci.Chain), "test") || strings.Contains(strings.ToLower(ci.Chain), "reg") || strings.Contains(strings.ToLower(ci.Chain), "signet") {
		d.Testnet = true
		d.Network = "testnet"
	} else {
		d.Testnet = false
		d.Network = "livenet"
	}

	glog.Info("rpc: block chain ", ci.Chain)
	return nil
}

func (d *PearlRPC) CreateMempool(chain bchain.BlockChain) (bchain.Mempool, error) {
	if d.mempool == nil {
		d.mempool = bchain.NewMempoolBitcoinType(chain, d.cfg.MempoolWorkers, d.cfg.MempoolSubWorkers, 0, "", false, d.cfg.MempoolResyncBatchSize)
	}
	return d.mempool, nil
}

func (d *PearlRPC) InitializeMempool(addrDescForOutpoint bchain.AddrDescForOutpointFunc, onNewTxAddr bchain.OnNewTxAddrFunc, onNewTx bchain.OnNewTxFunc) error {
	if d.mempool == nil {
		return errors.New("Mempool not created")
	}
	d.mempool.AddrDescForOutpoint = addrDescForOutpoint
	d.mempool.OnNewTxAddr = onNewTxAddr
	d.mempool.OnNewTx = onNewTx

	// If MQ binding is not configured, skip ZMQ subscription.
	if d.cfg.MessageQueueBinding == "" {
		glog.Info("MQ disabled (empty message_queue_binding)")
		return nil
	}

	if d.mq == nil {
		pearlTopics := bchain.SubscriptionTopics{
			BlockSubscribe: "hashblock",
			BlockReceive:   "hashblock",
			TxSubscribe:    "hashtx",
			TxReceive:      "hashtx",
		}

		mq, err := bchain.NewMQ(d.cfg.MessageQueueBinding, d.pushHandler, pearlTopics)
		if err != nil {
			glog.Error("mq: ", err)
			return err
		}
		d.mq = mq
	}
	return nil
}

func (d *PearlRPC) Shutdown(ctx context.Context) error {
	if d.client != nil {
		d.client.Shutdown()
		d.client.WaitForShutdown()
	}
	if d.mq != nil {
		if err := d.mq.Shutdown(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (d *PearlRPC) GetCoinName() string   { return d.cfg.CoinName }
func (d *PearlRPC) GetSubversion() string { return d.cfg.Subversion }

func (d *PearlRPC) raw(method string, params []json.RawMessage) (json.RawMessage, error) {
	if d.client == nil {
		return nil, errors.New("rpc client is nil")
	}
	return d.client.RawRequest(method, params)
}

func (d *PearlRPC) GetChainInfo() (*bchain.ChainInfo, error) {
	ciRaw, err := d.raw("getblockchaininfo", nil)
	if err != nil {
		return nil, err
	}
	var ci struct {
		Chain         string          `json:"chain"`
		Blocks        int             `json:"blocks"`
		Headers       int             `json:"headers"`
		Bestblockhash string          `json:"bestblockhash"`
		Difficulty    json.RawMessage `json:"difficulty"`
		SizeOnDisk    int64           `json:"size_on_disk"`
		Warnings      interface{}     `json:"warnings"`
	}
	if err := json.Unmarshal(ciRaw, &ci); err != nil {
		return nil, err
	}

	niRaw, err := d.raw("getnetworkinfo", nil)
	if err != nil {
		// Fall back to getinfo for older nodes.
		if rpcErr, ok := err.(*btcjson.RPCError); ok && int(rpcErr.Code) == -1 && rpcErr.Message == "Command unimplemented" {
			giRaw, err := d.raw("getinfo", nil)
			if err != nil {
				return nil, err
			}
			var gi struct {
				Version         json.RawMessage `json:"version"`
				ProtocolVersion json.RawMessage `json:"protocolversion"`
				Timeoffset      float64         `json:"timeoffset"`
				Errors          string          `json:"errors"`
			}
			if err := json.Unmarshal(giRaw, &gi); err != nil {
				return nil, err
			}
			return &bchain.ChainInfo{
				Chain:           ci.Chain,
				Blocks:          ci.Blocks,
				Headers:         ci.Headers,
				Bestblockhash:   ci.Bestblockhash,
				Difficulty:      string(bytesTrimSpace(ci.Difficulty)),
				SizeOnDisk:      ci.SizeOnDisk,
				Subversion:      "",
				Timeoffset:      gi.Timeoffset,
				Version:         string(bytesTrimSpace(gi.Version)),
				ProtocolVersion: string(bytesTrimSpace(gi.ProtocolVersion)),
				Warnings:        stringifyWarnings(ci.Warnings, gi.Errors),
			}, nil
		}
		return nil, err
	}

	var ni struct {
		Version         json.RawMessage `json:"version"`
		Subversion      string          `json:"subversion"`
		ProtocolVersion json.RawMessage `json:"protocolversion"`
		Timeoffset      float64         `json:"timeoffset"`
		Warnings        interface{}     `json:"warnings"`
	}
	if err := json.Unmarshal(niRaw, &ni); err != nil {
		return nil, err
	}

	return &bchain.ChainInfo{
		Chain:           ci.Chain,
		Blocks:          ci.Blocks,
		Headers:         ci.Headers,
		Bestblockhash:   ci.Bestblockhash,
		Difficulty:      string(bytesTrimSpace(ci.Difficulty)),
		SizeOnDisk:      ci.SizeOnDisk,
		Subversion:      ni.Subversion,
		Timeoffset:      ni.Timeoffset,
		Version:         string(bytesTrimSpace(ni.Version)),
		ProtocolVersion: string(bytesTrimSpace(ni.ProtocolVersion)),
		Warnings:        stringifyWarnings(ci.Warnings, ni.Warnings),
	}, nil
}

func (d *PearlRPC) GetBestBlockHash() (string, error) {
	raw, err := d.raw("getbestblockhash", nil)
	if err != nil {
		return "", err
	}
	var s string
	return s, json.Unmarshal(raw, &s)
}

func (d *PearlRPC) GetBestBlockHeight() (uint32, error) {
	raw, err := d.raw("getblockcount", nil)
	if err != nil {
		return 0, err
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, err
	}
	return uint32(n), nil
}

func (d *PearlRPC) GetBlockHash(height uint32) (string, error) {
	hb, _ := json.Marshal(height)
	raw, err := d.raw("getblockhash", []json.RawMessage{hb})
	if err != nil {
		return "", err
	}
	var s string
	return s, json.Unmarshal(raw, &s)
}

func (d *PearlRPC) GetBlockHeader(hash string) (*bchain.BlockHeader, error) {
	// verbose=true
	hb, _ := json.Marshal(hash)
	vb, _ := json.Marshal(true)
	raw, err := d.raw("getblockheader", []json.RawMessage{hb, vb})
	if err != nil {
		return nil, err
	}
	var bh bchain.BlockHeader
	if err := json.Unmarshal(raw, &bh); err != nil {
		return nil, err
	}
	return &bh, nil
}

func (d *PearlRPC) GetBlockInfo(hash string) (*bchain.BlockInfo, error) {
	// verbosity=1 (txids)
	hb, _ := json.Marshal(hash)
	vb, _ := json.Marshal(1)
	raw, err := d.raw("getblock", []json.RawMessage{hb, vb})
	if err != nil {
		return nil, err
	}
	var bi bchain.BlockInfo
	if err := json.Unmarshal(raw, &bi); err != nil {
		return nil, err
	}
	return &bi, nil
}

func (d *PearlRPC) GetBlock(hash string, height uint32) (*bchain.Block, error) {
	_ = height
	// verbosity=2 (full txs)
	hb, _ := json.Marshal(hash)
	vb, _ := json.Marshal(2)
	raw, err := d.raw("getblock", []json.RawMessage{hb, vb})
	if err != nil {
		return nil, err
	}
	// Pearl returns full transactions in "rawtx" field (not "tx") when verbosity=2.
	// We need to use a custom struct to properly unmarshal the response.
	var resp struct {
		Hash          string `json:"hash"`
		Confirmations int    `json:"confirmations"`
		Size          int    `json:"size"`
		Height        uint32 `json:"height"`
		Time          int64  `json:"time"`
		Prev          string `json:"previousblockhash"`
		Next          string `json:"nextblockhash"`
		RawTx         []struct {
			Txid     string `json:"txid"`
			Hex      string `json:"hex"`
			Version  int32  `json:"version"`
			LockTime uint32 `json:"locktime"`
			VSize    int64  `json:"vsize"`
			Vin      []struct {
				Coinbase  string `json:"coinbase"`
				Txid      string `json:"txid"`
				Vout      uint32 `json:"vout"`
				Sequence  uint32 `json:"sequence"`
				ScriptSig struct {
					Hex string `json:"hex"`
				} `json:"scriptSig"`
				Witness []string `json:"txinwitness"`
			} `json:"vin"`
			Vout []struct {
				Value        float64 `json:"value"`
				N            uint32  `json:"n"`
				ScriptPubKey struct {
					Asm       string   `json:"asm"`
					Hex       string   `json:"hex"`
					Type      string   `json:"type"`
					Address   string   `json:"address"`
					Addresses []string `json:"addresses"`
				} `json:"scriptPubKey"`
			} `json:"vout"`
			Time      int64 `json:"time"`
			Blocktime int64 `json:"blocktime"`
		} `json:"rawtx"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}

	// Build the bchain.Block from the response.
	blk := &bchain.Block{
		BlockHeader: bchain.BlockHeader{
			Hash:          resp.Hash,
			Prev:          resp.Prev,
			Next:          resp.Next,
			Height:        resp.Height,
			Confirmations: resp.Confirmations,
			Size:          resp.Size,
			Time:          resp.Time,
		},
		Txs: make([]bchain.Tx, len(resp.RawTx)),
	}

	for i, rtx := range resp.RawTx {
		tx := &blk.Txs[i]
		tx.Txid = rtx.Txid
		tx.Hex = rtx.Hex
		tx.Version = rtx.Version
		tx.LockTime = rtx.LockTime
		tx.VSize = rtx.VSize
		tx.Time = rtx.Time
		tx.Blocktime = rtx.Blocktime
		tx.BlockHeight = resp.Height

		// Convert vins
		tx.Vin = make([]bchain.Vin, len(rtx.Vin))
		for j, vin := range rtx.Vin {
			tx.Vin[j] = bchain.Vin{
				Coinbase: vin.Coinbase,
				Txid:     vin.Txid,
				Vout:     vin.Vout,
				Sequence: vin.Sequence,
				ScriptSig: bchain.ScriptSig{
					Hex: vin.ScriptSig.Hex,
				},
			}
			// Convert witness hex strings to bytes
			if len(vin.Witness) > 0 {
				tx.Vin[j].Witness = make([][]byte, len(vin.Witness))
				for k, w := range vin.Witness {
					wb, _ := hex.DecodeString(w)
					tx.Vin[j].Witness[k] = wb
				}
			}
		}

		// Convert vouts
		tx.Vout = make([]bchain.Vout, len(rtx.Vout))
		for j, vout := range rtx.Vout {
			// Convert value (BTC float) to satoshis
			vs, err := d.Parser.AmountToBigInt(common.JSONNumber(formatFloat(vout.Value)))
			if err != nil {
				return nil, err
			}
			tx.Vout[j] = bchain.Vout{
				ValueSat: vs,
				N:        vout.N,
				ScriptPubKey: bchain.ScriptPubKey{
					Hex:       vout.ScriptPubKey.Hex,
					Addresses: vout.ScriptPubKey.Addresses,
				},
			}
			// If addresses array is empty but single address exists, use it
			if len(tx.Vout[j].ScriptPubKey.Addresses) == 0 && vout.ScriptPubKey.Address != "" {
				tx.Vout[j].ScriptPubKey.Addresses = []string{vout.ScriptPubKey.Address}
			}
		}
	}

	return blk, nil
}

// formatFloat converts a float64 to a string representation suitable for AmountToBigInt.
func formatFloat(f float64) string {
	// Use 8 decimal places for satoshi precision
	return strconv.FormatFloat(f, 'f', 8, 64)
}

func (d *PearlRPC) GetBlockRaw(hash string) (string, error) {
	hb, _ := json.Marshal(hash)
	vb, _ := json.Marshal(0)
	raw, err := d.raw("getblock", []json.RawMessage{hb, vb})
	if err != nil {
		return "", err
	}
	var s string
	return s, json.Unmarshal(raw, &s)
}

func (d *PearlRPC) GetMempoolTransactions() ([]string, error) {
	raw, err := d.raw("getrawmempool", nil)
	if err != nil {
		return nil, err
	}
	var txids []string
	return txids, json.Unmarshal(raw, &txids)
}

func (d *PearlRPC) GetTransaction(txid string) (*bchain.Tx, error) {
	tb, _ := json.Marshal(txid)
	vb, _ := json.Marshal(1)
	raw, err := d.raw("getrawtransaction", []json.RawMessage{tb, vb})
	if err != nil {
		return nil, err
	}
	return d.Parser.ParseTxFromJson(raw)
}

func (d *PearlRPC) GetTransactionForMempool(txid string) (*bchain.Tx, error) {
	return d.GetTransaction(txid)
}

func (d *PearlRPC) GetTransactionSpecific(tx *bchain.Tx) (json.RawMessage, error) {
	if tx == nil {
		return nil, bchain.ErrTxNotFound
	}
	// Return the raw transaction JSON from the RPC
	tb, _ := json.Marshal(tx.Txid)
	vb, _ := json.Marshal(1)
	raw, err := d.raw("getrawtransaction", []json.RawMessage{tb, vb})
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (d *PearlRPC) EstimateSmartFee(blocks int, conservative bool) (v big.Int, err error) {
	// Pearl node does not expose estimatesmartfee; fall through so the
	// blockbook handler retries with EstimateFee.
	return v, errors.New("EstimateSmartFee: not supported")
}

func (d *PearlRPC) EstimateFee(blocks int) (v big.Int, err error) {
	glog.V(1).Info("rpc: estimatefee ", blocks)

	bb, _ := json.Marshal(blocks)
	raw, err := d.raw("estimatefee", []json.RawMessage{bb})
	if err != nil {
		return v, err
	}
	var feeRate float64
	if err := json.Unmarshal(raw, &feeRate); err != nil {
		return v, err
	}
	if feeRate < 0 {
		return v, errors.New("EstimateFee: insufficient data")
	}
	return d.Parser.AmountToBigInt(common.JSONNumber(formatFloat(feeRate)))
}

func (d *PearlRPC) SendRawTransaction(tx string, _ bool) (string, error) {
	tb, _ := json.Marshal(tx)
	raw, err := d.raw("sendrawtransaction", []json.RawMessage{tb})
	if err != nil {
		return "", err
	}
	var s string
	return s, json.Unmarshal(raw, &s)
}

func (d *PearlRPC) GetMempoolEntry(txid string) (*bchain.MempoolEntry, error) {
	tb, _ := json.Marshal(txid)
	raw, err := d.raw("getmempoolentry", []json.RawMessage{tb})
	if err != nil {
		return nil, err
	}
	var me bchain.MempoolEntry
	if err := json.Unmarshal(raw, &me); err != nil {
		return nil, err
	}
	return &me, nil
}

// ---- helpers ----

func stringifyWarnings(a interface{}, b interface{}) string {
	wa := warningsToString(a)
	wb := warningsToString(b)
	if wa != "" && wb != "" && wa != wb {
		return wa + " " + wb
	}
	if wa != "" {
		return wa
	}
	return wb
}

func warningsToString(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []string:
		return strings.Join(t, "; ")
	case []interface{}:
		parts := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "; ")
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func bytesTrimSpace(b []byte) []byte {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\n' || b[i] == '\r' || b[i] == '\t') {
		i++
	}
	j := len(b) - 1
	for j >= i && (b[j] == ' ' || b[j] == '\n' || b[j] == '\r' || b[j] == '\t') {
		j--
	}
	if j < i {
		return nil
	}
	return b[i : j+1]
}
