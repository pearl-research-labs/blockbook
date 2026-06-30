package pearl

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/juju/errors"
	"github.com/pearl-research-labs/pearl/node/btcjson"
	"github.com/pearl-research-labs/pearl/node/btcutil"
	"github.com/trezor/blockbook/bchain"
	"github.com/trezor/blockbook/common"
)

const defaultRPCTimeoutSeconds = 15

// Configuration is the blockchaincfg.json shape for Bitcoin-type chains.
// It mirrors the fields Blockbook expects for RPC auth + mempool settings.
type Configuration struct {
	CoinName             string `json:"coin_name"`
	CoinShortcut         string `json:"coin_shortcut"`
	RPCURL               string `json:"rpc_url"`
	RPCUser              string `json:"rpc_user"`
	RPCPass              string `json:"rpc_pass"`
	RPCTimeout           int    `json:"rpc_timeout"`
	Parse                bool   `json:"parse"`
	MessageQueueBinding  string `json:"message_queue_binding"`
	Subversion           string `json:"subversion"`
	AddressFormat        string `json:"address_format"`
	XPubMagic            uint32 `json:"xpub_magic,omitempty"`
	BlockAddressesToKeep int    `json:"block_addresses_to_keep"`
	MempoolWorkers       int    `json:"mempool_workers"`
	MempoolSubWorkers    int    `json:"mempool_sub_workers"`
}

// PearlRPC is a Blockbook chain implementation for Pearl that uses pearl/node libraries.
//
// Unlike the generic btc implementation in this repo, this avoids martinboehm/* dependencies.
type PearlRPC struct {
	*bchain.BaseChain

	cfg         Configuration
	client      http.Client
	rpcURL      string
	user        string
	password    string
	callCtx     context.Context
	cancelCall  context.CancelFunc
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
	if c.RPCTimeout <= 0 {
		glog.Warningf("rpc_timeout=%d is invalid, using default %d seconds", c.RPCTimeout, defaultRPCTimeoutSeconds)
		c.RPCTimeout = defaultRPCTimeoutSeconds
	}

	u, err := url.Parse(c.RPCURL)
	if err != nil {
		return nil, errors.Annotate(err, "Invalid rpc_url")
	}
	if u.Scheme == "" {
		u.Scheme = "http"
		u.Host = c.RPCURL
		u.Path = ""
	}

	s := &PearlRPC{
		BaseChain: &bchain.BaseChain{},
		cfg:       c,
		client: http.Client{
			Timeout: time.Duration(c.RPCTimeout) * time.Second,
			Transport: &http.Transport{
				DisableKeepAlives: true,
			},
		},
		rpcURL:      u.String(),
		user:        c.RPCUser,
		password:    c.RPCPass,
		pushHandler: pushHandler,
	}
	s.callCtx, s.cancelCall = context.WithCancel(context.Background())
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
		d.mempool = bchain.NewMempoolBitcoinType(chain, d.cfg.MempoolWorkers, d.cfg.MempoolSubWorkers, 0, "", false, 1)
	}
	return d.mempool, nil
}

func (d *PearlRPC) InitializeMempool(addrDescForOutpoint bchain.AddrDescForOutpointFunc, onNewTx bchain.OnNewTxFunc) error {
	if d.mempool == nil {
		return errors.New("Mempool not created")
	}
	d.mempool.AddrDescForOutpoint = addrDescForOutpoint
	d.mempool.OnNewTx = onNewTx

	if d.mq != nil {
		return nil
	}
	if d.cfg.MessageQueueBinding == "" {
		glog.Warning("ZeroMQ subscription disabled: message_queue_binding is empty; relying on polling")
		return nil
	}

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
	return nil
}

func (d *PearlRPC) Shutdown(ctx context.Context) error {
	if d.cancelCall != nil {
		d.cancelCall()
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

func (d *PearlRPC) requestContext() context.Context {
	if d.callCtx != nil {
		return d.callCtx
	}
	return context.Background()
}

func (d *PearlRPC) call(cmd interface{}, result interface{}) error {
	reqBytes, err := btcjson.MarshalCmd(btcjson.RpcVersion1, "blockbook", cmd)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(d.requestContext(), http.MethodPost, d.rpcURL, bytes.NewReader(reqBytes))
	if err != nil {
		return err
	}
	req.SetBasicAuth(d.user, d.password)
	req.Close = true
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.client.Do(req)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return err
	}

	// pearld returns non-200 with a plain-text body, e.g. HTTP 503
	// "503 Too busy.  Try again later." when concurrent requests exceed its
	// rpcmaxclients limit. Decoding that body as JSON yields a misleading
	// "cannot unmarshal number into btcjson.Response". Surface the status
	// explicitly instead; http.StatusText keeps the message matchable by the
	// sync retry classifier (it treats "503 service unavailable" et al. as
	// retryable).
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return errors.Errorf("pearl rpc: HTTP %d %s: %s", resp.StatusCode, http.StatusText(resp.StatusCode), strings.TrimSpace(string(body)))
	}

	var rpcResp btcjson.Response
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return err
	}
	if rpcResp.Error != nil {
		return rpcResp.Error
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(rpcResp.Result, result)
}

func (d *PearlRPC) GetChainInfo() (*bchain.ChainInfo, error) {
	var ci btcjson.GetBlockChainInfoResult
	if err := d.call(btcjson.NewGetBlockChainInfoCmd(), &ci); err != nil {
		return nil, err
	}
	var ni btcjson.GetNetworkInfoResult
	err := d.call(btcjson.NewGetNetworkInfoCmd(), &ni)
	if err != nil {
		// Fall back to getinfo for older nodes.
		if rpcErr, ok := err.(*btcjson.RPCError); ok && int(rpcErr.Code) == -1 && rpcErr.Message == "Command unimplemented" {
			var gi btcjson.InfoWalletResult
			if err := d.call(btcjson.NewGetInfoCmd(), &gi); err != nil {
				return nil, err
			}
			return &bchain.ChainInfo{
				Chain:           ci.Chain,
				Blocks:          int(ci.Blocks),
				Headers:         int(ci.Headers),
				Bestblockhash:   ci.BestBlockHash,
				Difficulty:      formatFloat(ci.Difficulty),
				SizeOnDisk:      ci.SizeOnDisk,
				Subversion:      "",
				Timeoffset:      float64(gi.TimeOffset),
				Version:         strconv.Itoa(int(gi.Version)),
				ProtocolVersion: strconv.Itoa(int(gi.ProtocolVersion)),
				Warnings:        gi.Errors,
			}, nil
		}
		return nil, err
	}

	return &bchain.ChainInfo{
		Chain:           ci.Chain,
		Blocks:          int(ci.Blocks),
		Headers:         int(ci.Headers),
		Bestblockhash:   ci.BestBlockHash,
		Difficulty:      formatFloat(ci.Difficulty),
		SizeOnDisk:      ci.SizeOnDisk,
		Subversion:      ni.SubVersion,
		Timeoffset:      float64(ni.TimeOffset),
		Version:         strconv.Itoa(int(ni.Version)),
		ProtocolVersion: strconv.Itoa(int(ni.ProtocolVersion)),
		Warnings:        strings.Join(ni.Warnings, "; "),
	}, nil
}

func (d *PearlRPC) GetBestBlockHash() (string, error) {
	var hash string
	if err := d.call(btcjson.NewGetBestBlockHashCmd(), &hash); err != nil {
		return "", err
	}
	return hash, nil
}

func (d *PearlRPC) GetBestBlockHeight() (uint32, error) {
	var height int64
	if err := d.call(btcjson.NewGetBlockCountCmd(), &height); err != nil {
		return 0, err
	}
	return uint32(height), nil
}

func (d *PearlRPC) GetBlockHash(height uint32) (string, error) {
	var hash string
	if err := d.call(btcjson.NewGetBlockHashCmd(int64(height)), &hash); err != nil {
		return "", err
	}
	return hash, nil
}

func (d *PearlRPC) GetBlockHeader(hash string) (*bchain.BlockHeader, error) {
	verbose := true
	var header btcjson.GetBlockHeaderVerboseResult
	if err := d.call(btcjson.NewGetBlockHeaderCmd(hash, &verbose), &header); err != nil {
		return nil, err
	}
	return &bchain.BlockHeader{
		Hash:          header.Hash,
		Prev:          header.PreviousHash,
		Next:          header.NextHash,
		Height:        uint32(header.Height),
		Confirmations: int(header.Confirmations),
		Time:          header.Time,
	}, nil
}

func (d *PearlRPC) GetBlockInfo(hash string) (*bchain.BlockInfo, error) {
	verbosity := 1
	var block btcjson.GetBlockVerboseResult
	if err := d.call(btcjson.NewGetBlockCmd(hash, &verbosity), &block); err != nil {
		return nil, err
	}
	return &bchain.BlockInfo{
		BlockHeader: bchain.BlockHeader{
			Hash:          block.Hash,
			Prev:          block.PreviousHash,
			Next:          block.NextHash,
			Height:        uint32(block.Height),
			Confirmations: int(block.Confirmations),
			Size:          int(block.Size),
			Time:          block.Time,
		},
		Version:    common.JSONNumber(strconv.Itoa(int(block.Version))),
		MerkleRoot: block.MerkleRoot,
		Bits:       block.Bits,
		Difficulty: common.JSONNumber(formatFloat(block.Difficulty)),
		Txids:      block.Tx,
	}, nil
}

func (d *PearlRPC) GetBlock(hash string, height uint32) (*bchain.Block, error) {
	if hash == "" {
		var err error
		hash, err = d.GetBlockHash(height)
		if err != nil {
			return nil, err
		}
	}
	if d.cfg.Parse {
		raw, err := d.GetBlockRaw(hash)
		if err != nil {
			return nil, err
		}
		data, err := hex.DecodeString(raw)
		if err != nil {
			return nil, err
		}
		block, err := d.Parser.ParseBlock(data)
		if err != nil {
			return nil, errors.Annotatef(err, "hash %v", hash)
		}
		block.Hash = hash
		block.Height = height
		if height == 0 {
			header, err := d.GetBlockHeader(hash)
			if err != nil {
				return nil, err
			}
			block.BlockHeader = *header
		}
		return block, nil
	}

	return d.GetBlockFull(hash)
}

func (d *PearlRPC) GetBlockFull(hash string) (*bchain.Block, error) {
	verbosity := 2
	var resp btcjson.GetBlockVerboseTxResult
	if err := d.call(btcjson.NewGetBlockCmd(hash, &verbosity), &resp); err != nil {
		return nil, mapPearlBlockError(err)
	}
	txs := resp.Tx
	if len(txs) == 0 && len(resp.RawTx) > 0 {
		txs = resp.RawTx
	}
	block := &bchain.Block{
		BlockHeader: bchain.BlockHeader{
			Hash:          resp.Hash,
			Prev:          resp.PreviousHash,
			Next:          resp.NextHash,
			Height:        uint32(resp.Height),
			Confirmations: int(resp.Confirmations),
			Size:          int(resp.Vsize),
			Time:          resp.Time,
		},
		Txs: make([]bchain.Tx, len(txs)),
	}
	parser := d.Parser.(*PearlParser)
	for i := range txs {
		tx, err := parser.TxFromTxRawResult(&txs[i])
		if err != nil {
			return nil, err
		}
		tx.BlockHeight = uint32(resp.Height)
		if tx.Time == 0 {
			tx.Time = resp.Time
		}
		if tx.Blocktime == 0 {
			tx.Blocktime = resp.Time
		}
		block.Txs[i] = *tx
	}
	return block, nil
}

// formatFloat converts a float64 to a string representation suitable for AmountToBigInt.
func formatFloat(f float64) string {
	// Use 8 decimal places for satoshi precision
	return strconv.FormatFloat(f, 'f', 8, 64)
}

func (d *PearlRPC) GetBlockRaw(hash string) (string, error) {
	verbosity := 0
	var raw string
	if err := d.call(btcjson.NewGetBlockCmd(hash, &verbosity), &raw); err != nil {
		return "", err
	}
	return raw, nil
}

func (d *PearlRPC) GetMempoolTransactions() ([]string, error) {
	verbose := false
	var txids []string
	if err := d.call(btcjson.NewGetRawMempoolCmd(&verbose), &txids); err != nil {
		return nil, err
	}
	return txids, nil
}

func (d *PearlRPC) GetTransaction(txid string) (*bchain.Tx, error) {
	verbosity := 1
	var raw btcjson.TxRawResult
	if err := d.call(btcjson.NewGetRawTransactionCmd(txid, &verbosity), &raw); err != nil {
		return nil, mapPearlTxError(err)
	}
	return d.Parser.(*PearlParser).TxFromTxRawResult(&raw)
}

func (d *PearlRPC) GetTransactionForMempool(txid string) (*bchain.Tx, error) {
	return d.GetTransaction(txid)
}

func (d *PearlRPC) GetTransactionSpecific(tx *bchain.Tx) (json.RawMessage, error) {
	if tx == nil {
		return nil, bchain.ErrTxNotFound
	}
	verbosity := 1
	var raw json.RawMessage
	if err := d.call(btcjson.NewGetRawTransactionCmd(tx.Txid, &verbosity), &raw); err != nil {
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

	var feeRate float64
	if err := d.call(btcjson.NewEstimateFeeCmd(int64(blocks)), &feeRate); err != nil {
		return v, err
	}
	if feeRate < 0 {
		return v, errors.New("EstimateFee: insufficient data")
	}
	amount, err := btcutil.NewAmount(feeRate)
	if err != nil {
		return v, err
	}
	v.SetInt64(int64(amount))
	return v, nil
}

func (d *PearlRPC) SendRawTransaction(tx string, allowHighFees bool) (string, error) {
	var hash string
	if err := d.call(btcjson.NewSendRawTransactionCmd(tx, &allowHighFees), &hash); err != nil {
		return "", err
	}
	return hash, nil
}

func (d *PearlRPC) GetMempoolEntry(txid string) (*bchain.MempoolEntry, error) {
	var entry btcjson.GetMempoolEntryResult
	if err := d.call(btcjson.NewGetMempoolEntryCmd(txid), &entry); err != nil {
		return nil, err
	}
	fee, err := btcutil.NewAmount(entry.Fee)
	if err != nil {
		return nil, err
	}
	modifiedFee, err := btcutil.NewAmount(entry.ModifiedFee)
	if err != nil {
		return nil, err
	}
	descendantFees, err := btcutil.NewAmount(entry.DescendantFees)
	if err != nil {
		return nil, err
	}
	ancestorFees, err := btcutil.NewAmount(entry.AncestorFees)
	if err != nil {
		return nil, err
	}
	return &bchain.MempoolEntry{
		Size:            uint32(entry.VSize),
		FeeSat:          *big.NewInt(int64(fee)),
		Fee:             common.JSONNumber(formatFloat(entry.Fee)),
		ModifiedFeeSat:  *big.NewInt(int64(modifiedFee)),
		ModifiedFee:     common.JSONNumber(formatFloat(entry.ModifiedFee)),
		Time:            uint64(entry.Time),
		Height:          uint32(entry.Height),
		DescendantCount: uint32(entry.DescendantCount),
		DescendantSize:  uint32(entry.DescendantSize),
		DescendantFees:  uint32(descendantFees),
		AncestorCount:   uint32(entry.AncestorCount),
		AncestorSize:    uint32(entry.AncestorSize),
		AncestorFees:    uint32(ancestorFees),
		Depends:         entry.Depends,
	}, nil
}

// ---- helpers ----

func mapPearlTxError(err error) error {
	if rpcErr, ok := err.(*btcjson.RPCError); ok && rpcErr.Code == btcjson.ErrRPCNoTxInfo {
		return bchain.ErrTxNotFound
	}
	return err
}

func mapPearlBlockError(err error) error {
	if rpcErr, ok := err.(*btcjson.RPCError); ok && rpcErr.Code == btcjson.ErrRPCBlockNotFound {
		return bchain.ErrBlockNotFound
	}
	return err
}
