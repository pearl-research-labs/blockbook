package pearl

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"github.com/juju/errors"
	"github.com/pearl-research-labs/pearl/node/blockchain"
	"github.com/pearl-research-labs/pearl/node/btcec/schnorr"
	"github.com/pearl-research-labs/pearl/node/btcjson"
	"github.com/pearl-research-labs/pearl/node/btcutil"
	"github.com/pearl-research-labs/pearl/node/btcutil/hdkeychain"
	"github.com/pearl-research-labs/pearl/node/chaincfg"
	"github.com/pearl-research-labs/pearl/node/txscript"
	"github.com/pearl-research-labs/pearl/node/wire"
	"github.com/trezor/blockbook/bchain"
)

// PearlParser is a minimal Bitcoin-type parser backed by pearl/node libs.
//
// AddressDescriptor is the output script bytes (same convention as other BTC-type parsers in Blockbook).
type PearlParser struct {
	*bchain.BaseParser
	Params    *chaincfg.Params
	xpubMagic uint32
}

func NewPearlParser(chainName string, c *Configuration) *PearlParser {
	// Select chain params based on chain name from backend.
	var p *chaincfg.Params
	lc := strings.ToLower(chainName)
	switch {
	case strings.Contains(lc, "reg"):
		p = &chaincfg.RegressionNetParams
	case strings.Contains(lc, "testnet2"):
		p = &chaincfg.TestNet2Params
	case strings.Contains(lc, "test") || strings.Contains(lc, "signet"):
		p = &chaincfg.TestNetParams
	default:
		p = &chaincfg.MainNetParams
	}
	return &PearlParser{
		BaseParser: &bchain.BaseParser{
			BlockAddressesToKeep: c.BlockAddressesToKeep,
			AmountDecimalPoint:   8,
		},
		Params:    p,
		xpubMagic: c.XPubMagic,
	}
}

func (p *PearlParser) SupportsVSize() bool { return true }

func (p *PearlParser) ParseTxFromJson(msg json.RawMessage) (*bchain.Tx, error) {
	var raw btcjson.TxRawResult
	if err := json.Unmarshal(msg, &raw); err != nil {
		return nil, err
	}
	return p.TxFromTxRawResult(&raw)
}

func (p *PearlParser) ParseTx(b []byte) (*bchain.Tx, error) {
	var msgTx wire.MsgTx
	if err := msgTx.Deserialize(bytes.NewReader(b)); err != nil {
		return nil, err
	}
	tx := p.TxFromMsgTx(&msgTx, true)
	tx.Hex = hex.EncodeToString(b)
	return &tx, nil
}

func (p *PearlParser) ParseBlock(b []byte) (*bchain.Block, error) {
	var msgBlock wire.MsgBlock
	if err := msgBlock.Deserialize(bytes.NewReader(b)); err != nil {
		return nil, err
	}
	txs := make([]bchain.Tx, len(msgBlock.Transactions))
	for i, tx := range msgBlock.Transactions {
		txs[i] = p.TxFromMsgTx(tx, false)
	}
	header := msgBlock.BlockHeader()
	return &bchain.Block{
		BlockHeader: bchain.BlockHeader{
			Prev: header.PrevBlock.String(),
			Size: len(b),
			Time: header.Timestamp.Unix(),
		},
		Txs: txs,
	}, nil
}

func (p *PearlParser) PackTx(tx *bchain.Tx, height uint32, blockTime int64) ([]byte, error) {
	if tx.Hex == "" {
		return nil, errors.New("missing raw Pearl transaction hex")
	}
	buf := make([]byte, 12+len(tx.Hex)/2)
	binary.BigEndian.PutUint32(buf[:4], height)
	binary.BigEndian.PutUint64(buf[4:12], uint64(blockTime))
	n, err := hex.Decode(buf[12:], []byte(tx.Hex))
	if err != nil {
		return nil, err
	}
	return buf[:12+n], nil
}

func (p *PearlParser) UnpackTx(buf []byte) (*bchain.Tx, uint32, error) {
	if len(buf) < 12 {
		return nil, 0, errors.New("short packed Pearl transaction")
	}
	height := binary.BigEndian.Uint32(buf[:4])
	blockTime := int64(binary.BigEndian.Uint64(buf[4:12]))
	tx, err := p.ParseTx(buf[12:])
	if err != nil {
		return nil, 0, err
	}
	tx.Blocktime = blockTime
	tx.Time = blockTime
	return tx, height, nil
}

func (p *PearlParser) GetAddrDescFromVout(output *bchain.Vout) (bchain.AddressDescriptor, error) {
	if output.ScriptPubKey.Hex == "" {
		return nil, nil
	}
	return hex.DecodeString(output.ScriptPubKey.Hex)
}

func (p *PearlParser) GetAddrDescFromAddress(address string) (bchain.AddressDescriptor, error) {
	da, err := btcutil.DecodeAddress(address, p.Params)
	if err != nil {
		return nil, err
	}
	return txscript.PayToAddrScript(da)
}

func (p *PearlParser) GetAddressesFromAddrDesc(addrDesc bchain.AddressDescriptor) ([]string, bool, error) {
	sc, addrs, _, err := txscript.ExtractPkScriptAddrs(addrDesc, p.Params)
	if err != nil {
		return nil, false, err
	}
	rv := make([]string, len(addrs))
	for i, a := range addrs {
		rv[i] = a.EncodeAddress()
	}
	return rv, sc == txscript.WitnessV1TaprootTy, nil
}

func (p *PearlParser) GetScriptFromAddrDesc(addrDesc bchain.AddressDescriptor) ([]byte, error) {
	return addrDesc, nil
}

func (p *PearlParser) IsAddrDescIndexable(addrDesc bchain.AddressDescriptor) bool {
	return len(addrDesc) != 0 && addrDesc[0] != txscript.OP_RETURN
}

func (p *PearlParser) TxFromMsgTx(t *wire.MsgTx, parseAddresses bool) bchain.Tx {
	vin := make([]bchain.Vin, len(t.TxIn))
	coinbase := blockchain.IsCoinBaseTx(t)
	for i, in := range t.TxIn {
		if coinbase {
			vin[i] = bchain.Vin{
				Coinbase: hex.EncodeToString(in.SignatureScript),
				Sequence: in.Sequence,
			}
			break
		}
		vin[i] = bchain.Vin{
			Txid:     in.PreviousOutPoint.Hash.String(),
			Vout:     in.PreviousOutPoint.Index,
			Sequence: in.Sequence,
			ScriptSig: bchain.ScriptSig{
				Hex: hex.EncodeToString(in.SignatureScript),
			},
			Witness: in.Witness,
		}
	}

	vout := make([]bchain.Vout, len(t.TxOut))
	for i, out := range t.TxOut {
		addrs := []string{}
		if parseAddresses {
			addrs, _, _ = p.GetAddressesFromAddrDesc(out.PkScript)
		}
		var valueSat big.Int
		valueSat.SetInt64(out.Value)
		vout[i] = bchain.Vout{
			ValueSat: valueSat,
			N:        uint32(i),
			ScriptPubKey: bchain.ScriptPubKey{
				Hex:       hex.EncodeToString(out.PkScript),
				Addresses: addrs,
			},
		}
	}

	return bchain.Tx{
		Txid:     t.TxHash().String(),
		Version:  t.Version,
		LockTime: t.LockTime,
		VSize:    blockchain.GetTransactionVsize(btcutil.NewTx(t)),
		Vin:      vin,
		Vout:     vout,
	}
}

func (p *PearlParser) TxFromTxRawResult(raw *btcjson.TxRawResult) (*bchain.Tx, error) {
	tx := &bchain.Tx{
		Hex:           raw.Hex,
		Txid:          raw.Txid,
		Version:       int32(raw.Version),
		LockTime:      raw.LockTime,
		VSize:         int64(raw.Vsize),
		Confirmations: uint32(raw.Confirmations),
		Time:          raw.Time,
		Blocktime:     raw.Blocktime,
		Vin:           make([]bchain.Vin, len(raw.Vin)),
		Vout:          make([]bchain.Vout, len(raw.Vout)),
	}
	for i := range raw.Vin {
		vin := &raw.Vin[i]
		tx.Vin[i] = bchain.Vin{
			Coinbase: vin.Coinbase,
			Txid:     vin.Txid,
			Vout:     vin.Vout,
			Sequence: vin.Sequence,
		}
		if vin.ScriptSig != nil {
			tx.Vin[i].ScriptSig.Hex = vin.ScriptSig.Hex
		}
		if len(vin.Witness) > 0 {
			tx.Vin[i].Witness = make([][]byte, len(vin.Witness))
			for j, witness := range vin.Witness {
				w, err := hex.DecodeString(witness)
				if err != nil {
					return nil, errors.Annotatef(err, "vin %d witness %d", i, j)
				}
				tx.Vin[i].Witness[j] = w
			}
		}
	}
	for i := range raw.Vout {
		vout := &raw.Vout[i]
		value, err := btcutil.NewAmount(vout.Value)
		if err != nil {
			return nil, err
		}
		var valueSat big.Int
		valueSat.SetInt64(int64(value))
		addresses := vout.ScriptPubKey.Addresses
		if len(addresses) == 0 && vout.ScriptPubKey.Address != "" {
			addresses = []string{vout.ScriptPubKey.Address}
		}
		tx.Vout[i] = bchain.Vout{
			ValueSat: valueSat,
			N:        vout.N,
			ScriptPubKey: bchain.ScriptPubKey{
				Hex:       vout.ScriptPubKey.Hex,
				Addresses: addresses,
			},
		}
	}
	return tx, nil
}

var pearlXpubDescriptorRegex = regexp.MustCompile(`^tr\((?:\[[^\]]*\])?(?P<xpub>\w+)(?:/(?:\{(?P<changelist1>\d+(?:,\d+)*)\}|<(?P<changelist2>\d+(?:;\d+)*)>|(?P<change>\d+))/\*)?\)(?:#[[:alnum:]]+)?$`)

// ParseXpub parses raw extended public keys and Taproot descriptors for Pearl.
func (p *PearlParser) ParseXpub(xpub string) (*bchain.XpubDescriptor, error) {
	descriptor := bchain.XpubDescriptor{
		XpubDescriptor: xpub,
		Type:           bchain.P2TR,
		Bip:            "86",
		ChangeIndexes:  []uint32{0, 1},
	}

	if match := pearlXpubDescriptorRegex.FindStringSubmatch(xpub); len(match) > 0 {
		descriptor.Xpub = match[pearlXpubDescriptorRegex.SubexpIndex("xpub")]
		if err := parsePearlChangeIndexes(match, &descriptor); err != nil {
			return nil, err
		}
	} else {
		descriptor.Xpub = xpub
	}

	extKey, err := hdkeychain.NewKeyFromString(descriptor.Xpub)
	if err != nil {
		return nil, err
	}
	if !p.isSupportedXpubVersion(extKey) {
		return nil, errors.New("Unsupported xpub version")
	}

	descriptor.ExtKey = extKey
	return &descriptor, nil
}

func parsePearlChangeIndexes(match []string, descriptor *bchain.XpubDescriptor) error {
	changeIndex := pearlXpubDescriptorRegex.SubexpIndex("change")
	changeList1Index := pearlXpubDescriptorRegex.SubexpIndex("changelist1")
	changeList2Index := pearlXpubDescriptorRegex.SubexpIndex("changelist2")

	if match[changeIndex] != "" {
		change, err := strconv.ParseUint(match[changeIndex], 10, 32)
		if err != nil {
			return err
		}
		descriptor.ChangeIndexes = []uint32{uint32(change)}
		return nil
	}

	var changes []string
	if match[changeList1Index] != "" {
		changes = strings.Split(match[changeList1Index], ",")
	} else if match[changeList2Index] != "" {
		changes = strings.Split(match[changeList2Index], ";")
	}
	if len(changes) == 0 {
		return nil
	}

	descriptor.ChangeIndexes = make([]uint32, len(changes))
	for i, ch := range changes {
		change, err := strconv.ParseUint(ch, 10, 32)
		if err != nil {
			return err
		}
		descriptor.ChangeIndexes[i] = uint32(change)
	}
	return nil
}

func (p *PearlParser) isSupportedXpubVersion(extKey *hdkeychain.ExtendedKey) bool {
	version := extKey.Version()
	if bytes.Equal(version, p.Params.HDPublicKeyID[:]) {
		return true
	}
	if p.xpubMagic != 0 {
		var configured [4]byte
		binary.BigEndian.PutUint32(configured[:], p.xpubMagic)
		if bytes.Equal(version, configured[:]) {
			return true
		}
	}
	return bytes.Equal(version, []byte{0x04, 0x88, 0xb2, 0x1e})
}

func (p *PearlParser) addrDescFromExtKey(extKey *hdkeychain.ExtendedKey, descriptor *bchain.XpubDescriptor) (bchain.AddressDescriptor, error) {
	if descriptor.Type != bchain.P2TR {
		return nil, errors.New("Unsupported xpub descriptor type")
	}

	pubKey, err := extKey.ECPubKey()
	if err != nil {
		return nil, err
	}
	taprootKey := txscript.ComputeTaprootKeyNoScript(pubKey)
	addr, err := btcutil.NewAddressTaproot(schnorr.SerializePubKey(taprootKey), p.Params)
	if err != nil {
		return nil, err
	}
	return txscript.PayToAddrScript(addr)
}

// DeriveAddressDescriptors derives address descriptors from a parsed Pearl xpub.
func (p *PearlParser) DeriveAddressDescriptors(descriptor *bchain.XpubDescriptor, change uint32, indexes []uint32) ([]bchain.AddressDescriptor, error) {
	ad := make([]bchain.AddressDescriptor, len(indexes))
	changeExtKey, err := descriptor.ExtKey.(*hdkeychain.ExtendedKey).Derive(change)
	if err != nil {
		return nil, err
	}
	for i, index := range indexes {
		indexExtKey, err := changeExtKey.Derive(index)
		if err != nil {
			return nil, err
		}
		ad[i], err = p.addrDescFromExtKey(indexExtKey, descriptor)
		if err != nil {
			return nil, err
		}
	}
	return ad, nil
}

// DeriveAddressDescriptorsFromTo derives address descriptors in the half-open index range [fromIndex, toIndex).
func (p *PearlParser) DeriveAddressDescriptorsFromTo(descriptor *bchain.XpubDescriptor, change uint32, fromIndex uint32, toIndex uint32) ([]bchain.AddressDescriptor, error) {
	if toIndex <= fromIndex {
		return nil, errors.New("toIndex<=fromIndex")
	}

	changeExtKey, err := descriptor.ExtKey.(*hdkeychain.ExtendedKey).Derive(change)
	if err != nil {
		return nil, err
	}
	ad := make([]bchain.AddressDescriptor, toIndex-fromIndex)
	for index := fromIndex; index < toIndex; index++ {
		indexExtKey, err := changeExtKey.Derive(index)
		if err != nil {
			return nil, err
		}
		ad[index-fromIndex], err = p.addrDescFromExtKey(indexExtKey, descriptor)
		if err != nil {
			return nil, err
		}
	}
	return ad, nil
}

// DerivationBasePath returns the account path represented by the xpub.
func (p *PearlParser) DerivationBasePath(descriptor *bchain.XpubDescriptor) (string, error) {
	extKey := descriptor.ExtKey.(*hdkeychain.ExtendedKey)
	child := extKey.ChildIndex()
	suffix := ""
	if child >= hdkeychain.HardenedKeyStart {
		child -= hdkeychain.HardenedKeyStart
		suffix = "'"
	}
	account := strconv.Itoa(int(child)) + suffix
	if extKey.Depth() != 3 {
		return "unknown/" + account, nil
	}
	return "m/" + descriptor.Bip + "'/" + strconv.Itoa(int(p.Params.HDCoinType)) + "'/" + account, nil
}
