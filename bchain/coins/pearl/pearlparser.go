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
	"github.com/pearl-research-labs/pearl/node/btcec/schnorr"
	"github.com/pearl-research-labs/pearl/node/btcutil"
	"github.com/pearl-research-labs/pearl/node/btcutil/hdkeychain"
	"github.com/pearl-research-labs/pearl/node/chaincfg"
	"github.com/pearl-research-labs/pearl/node/txscript"
	"github.com/trezor/blockbook/bchain"
	"github.com/trezor/blockbook/common"
)

// PearlParser is a minimal Bitcoin-type parser backed by pearl/node libs.
//
// AddressDescriptor is the output script bytes (same convention as other BTC-type parsers in Blockbook).
type PearlParser struct {
	*bchain.BaseParser
	params    *chaincfg.Params
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
		params:    p,
		xpubMagic: c.XPubMagic,
	}
}

func (p *PearlParser) SupportsVSize() bool { return false }

func (p *PearlParser) MinimumCoinbaseConfirmations() int { return 0 }

func (p *PearlParser) AmountToDecimalString(a *big.Int) string {
	return p.BaseParser.AmountToDecimalString(a)
}
func (p *PearlParser) AmountToBigInt(n common.JSONNumber) (big.Int, error) {
	return p.BaseParser.AmountToBigInt(n)
}

func (p *PearlParser) KeepBlockAddresses() int { return p.BaseParser.KeepBlockAddresses() }
func (p *PearlParser) AmountDecimals() int     { return p.BaseParser.AmountDecimals() }
func (p *PearlParser) UseAddressAliases() bool { return p.BaseParser.UseAddressAliases() }

func (p *PearlParser) PackedTxidLen() int                    { return p.BaseParser.PackedTxidLen() }
func (p *PearlParser) PackTxid(txid string) ([]byte, error)  { return p.BaseParser.PackTxid(txid) }
func (p *PearlParser) UnpackTxid(buf []byte) (string, error) { return p.BaseParser.UnpackTxid(buf) }

func (p *PearlParser) PackBlockHash(hash string) ([]byte, error) {
	return p.BaseParser.PackBlockHash(hash)
}
func (p *PearlParser) UnpackBlockHash(buf []byte) (string, error) {
	return p.BaseParser.UnpackBlockHash(buf)
}

func (p *PearlParser) ParseTxFromJson(msg json.RawMessage) (*bchain.Tx, error) {
	return p.BaseParser.ParseTxFromJson(msg)
}

func (p *PearlParser) ParseTx(b []byte) (*bchain.Tx, error)       { return p.BaseParser.ParseTx(b) }
func (p *PearlParser) ParseBlock(b []byte) (*bchain.Block, error) { return p.BaseParser.ParseBlock(b) }

func (p *PearlParser) PackTx(tx *bchain.Tx, height uint32, blockTime int64) ([]byte, error) {
	return p.BaseParser.PackTx(tx, height, blockTime)
}
func (p *PearlParser) UnpackTx(buf []byte) (*bchain.Tx, uint32, error) {
	return p.BaseParser.UnpackTx(buf)
}
func (p *PearlParser) GetAddrDescForUnknownInput(tx *bchain.Tx, input int) bchain.AddressDescriptor {
	return p.BaseParser.GetAddrDescForUnknownInput(tx, input)
}

func (p *PearlParser) GetAddrDescFromVout(output *bchain.Vout) (bchain.AddressDescriptor, error) {
	if output.ScriptPubKey.Hex == "" {
		// Log warning if scriptPubKey.hex is empty - this would cause indexing issues
		return nil, nil
	}
	script, err := hex.DecodeString(output.ScriptPubKey.Hex)
	if err != nil {
		return nil, err
	}
	return script, nil
}

func (p *PearlParser) GetAddrDescFromAddress(address string) (bchain.AddressDescriptor, error) {
	da, err := btcutil.DecodeAddress(address, p.params)
	if err != nil {
		return nil, err
	}
	return txscript.PayToAddrScript(da)
}

func (p *PearlParser) GetAddressesFromAddrDesc(addrDesc bchain.AddressDescriptor) ([]string, bool, error) {
	sc, addrs, _, err := txscript.ExtractPkScriptAddrs(addrDesc, p.params)
	if err != nil {
		return nil, false, err
	}
	rv := make([]string, len(addrs))
	for i, a := range addrs {
		rv[i] = a.EncodeAddress()
	}
	// Pearl only supports Taproot addresses
	searchable := sc == txscript.WitnessV1TaprootTy
	return rv, searchable, nil
}

func (p *PearlParser) GetScriptFromAddrDesc(addrDesc bchain.AddressDescriptor) ([]byte, error) {
	return addrDesc, nil
}

func (p *PearlParser) IsAddrDescIndexable(addrDesc bchain.AddressDescriptor) bool {
	if len(addrDesc) == 0 || addrDesc[0] == txscript.OP_RETURN {
		return false
	}
	return true
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
	if bytes.Equal(version, p.params.HDPublicKeyID[:]) {
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
	addr, err := btcutil.NewAddressTaproot(schnorr.SerializePubKey(taprootKey), p.params)
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
	return "m/" + descriptor.Bip + "'/" + strconv.Itoa(int(p.params.HDCoinType)) + "'/" + account, nil
}
