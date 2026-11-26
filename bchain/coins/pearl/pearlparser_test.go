//go:build unittest

package pearl

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/pearl-research-labs/pearl/node/btcutil/hdkeychain"
	"github.com/pearl-research-labs/pearl/node/chaincfg"
	"github.com/pearl-research-labs/pearl/node/chaincfg/chainhash"
	"github.com/pearl-research-labs/pearl/node/wire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/trezor/blockbook/bchain"
)

const testXPubMagic = 78792518

// testPearlXpub derives a standard (xpub-versioned) BIP86 Pearl account key for tests.
func testPearlXpub(t *testing.T) string {
	t.Helper()

	key, err := hdkeychain.NewMaster([]byte("0123456789abcdef0123456789abcdef"), &chaincfg.MainNetParams)
	require.NoError(t, err)

	for _, child := range []uint32{
		86 + hdkeychain.HardenedKeyStart,
		chaincfg.HDCoinTypePearl + hdkeychain.HardenedKeyStart,
		0 + hdkeychain.HardenedKeyStart,
	} {
		key, err = key.Derive(child)
		require.NoError(t, err)
	}

	xpub, err := key.Neuter()
	require.NoError(t, err)
	standardXpub, err := xpub.CloneWithVersion([]byte{0x04, 0x88, 0xb2, 0x1e})
	require.NoError(t, err)
	return standardXpub.String()
}

func TestPearlParser_XpubDerivation(t *testing.T) {
	parser := NewPearlParser("main", &Configuration{XPubMagic: testXPubMagic})

	descriptor, err := parser.ParseXpub(testPearlXpub(t))
	require.NoError(t, err)
	assert.Equal(t, bchain.P2TR, descriptor.Type)
	assert.Equal(t, []uint32{0, 1}, descriptor.ChangeIndexes)

	addrDescs, err := parser.DeriveAddressDescriptorsFromTo(descriptor, 0, 0, 1)
	require.NoError(t, err)
	require.Len(t, addrDescs, 1)

	addrs, searchable, err := parser.GetAddressesFromAddrDesc(addrDescs[0])
	require.NoError(t, err)
	assert.True(t, searchable, "derived Taproot address should be searchable")
	require.Len(t, addrs, 1)
	assert.True(t, strings.HasPrefix(addrs[0], "prl1p"), "want Pearl Taproot address, got %q", addrs[0])
}

// TestPearlParser_P2MRAddress verifies witness-v2 (P2MR / Pay-to-Merkle-Root)
// support both directions using the reference test vector from docs/p2mr.md: the
// OP_2 <32-byte root> script must decode to a searchable prl1z… address, and that
// address must re-encode to the same script.
func TestPearlParser_P2MRAddress(t *testing.T) {
	parser := NewPearlParser("main", &Configuration{XPubMagic: testXPubMagic})

	const (
		address = "prl1zqu04ax80tw03rs0v90rel24a77z722yyrdum43fcdvdtfgug4svquahpxa"
		script  = "5220071f5e98ef5b9f11c1ec2bc79faabdf785e528841b79bac5386b1ab4a388ac18"
	)
	spk, err := hex.DecodeString(script)
	require.NoError(t, err)

	// scriptPubKey -> address
	addrs, searchable, err := parser.GetAddressesFromAddrDesc(spk)
	require.NoError(t, err)
	assert.True(t, searchable, "P2MR output should be searchable")
	require.Len(t, addrs, 1)
	assert.Equal(t, address, addrs[0])
	assert.True(t, strings.HasPrefix(addrs[0], "prl1z"), "P2MR mainnet address should start with prl1z")

	// address -> scriptPubKey
	got, err := parser.GetAddrDescFromAddress(address)
	require.NoError(t, err)
	assert.Equal(t, spk, []byte(got))
}

func TestPearlParser_XpubDescriptor(t *testing.T) {
	parser := NewPearlParser("main", &Configuration{XPubMagic: testXPubMagic})
	xpub := testPearlXpub(t)

	tests := []struct {
		name              string
		descriptor        string
		wantChangeIndexes []uint32
	}{
		{
			name:              "single change index",
			descriptor:        "tr(" + xpub + "/0/*)",
			wantChangeIndexes: []uint32{0},
		},
		{
			name:              "comma change list with checksum",
			descriptor:        "tr([5c9e228d/86'/808276'/0']" + xpub + "/{0,1,2}/*)#4rqwxvej",
			wantChangeIndexes: []uint32{0, 1, 2},
		},
		{
			name:              "semicolon change list with hardened path aliases",
			descriptor:        "tr([5c9e228d/86h/808276h/0h]" + xpub + "/<0;1>/*)#4rqwxvej",
			wantChangeIndexes: []uint32{0, 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			descriptor, err := parser.ParseXpub(tt.descriptor)
			require.NoError(t, err)
			assert.Equal(t, tt.wantChangeIndexes, descriptor.ChangeIndexes)

			_, err = parser.DeriveAddressDescriptors(descriptor, tt.wantChangeIndexes[0], []uint32{0, 1})
			require.NoError(t, err)
		})
	}
}

func TestPearlParser_RejectsUnsupportedDescriptorTypes(t *testing.T) {
	parser := NewPearlParser("main", &Configuration{XPubMagic: testXPubMagic})

	_, err := parser.ParseXpub("wpkh(" + testPearlXpub(t) + "/0/*)")
	require.Error(t, err, "non-Taproot descriptor should be rejected")
}

func TestPearlParser_RawTransactionRoundTrip(t *testing.T) {
	parser := NewPearlParser("main", &Configuration{XPubMagic: testXPubMagic})
	rawTx, msgTx, script := testPearlRawTx(t, parser)

	tx, err := parser.ParseTx(rawTx)
	require.NoError(t, err)
	assert.Equal(t, msgTx.TxHash().String(), tx.Txid)
	assert.Equal(t, hex.EncodeToString(rawTx), tx.Hex)
	assert.Positive(t, tx.VSize)
	require.Len(t, tx.Vin, 1)
	require.Len(t, tx.Vin[0].Witness, 1)
	assert.Equal(t, []byte{0x01, 0x02, 0x03}, tx.Vin[0].Witness[0])
	require.Len(t, tx.Vout, 1)
	assert.Equal(t, script, tx.Vout[0].ScriptPubKey.Hex)
	assert.Equal(t, big.NewInt(123456789), &tx.Vout[0].ValueSat)
	require.Len(t, tx.Vout[0].ScriptPubKey.Addresses, 1)
	assert.True(t, strings.HasPrefix(tx.Vout[0].ScriptPubKey.Addresses[0], "prl1p"))

	packed, err := parser.PackTx(tx, 123, 456789)
	require.NoError(t, err)
	got, height, err := parser.UnpackTx(packed)
	require.NoError(t, err)
	assert.Equal(t, uint32(123), height)
	assert.Equal(t, int64(456789), got.Blocktime)
	assert.Equal(t, tx.Txid, got.Txid)
	assert.Equal(t, tx.Hex, got.Hex)
	require.Len(t, got.Vout, 1)
	assert.Equal(t, tx.Vout[0].ValueSat.String(), got.Vout[0].ValueSat.String())
}

func testPearlRawTx(t *testing.T, parser *PearlParser) ([]byte, *wire.MsgTx, string) {
	t.Helper()

	descriptor, err := parser.ParseXpub(testPearlXpub(t))
	require.NoError(t, err)
	addrDescs, err := parser.DeriveAddressDescriptorsFromTo(descriptor, 0, 0, 1)
	require.NoError(t, err)
	script := addrDescs[0]

	prevHash, err := chainhash.NewHashFromStr("1111111111111111111111111111111111111111111111111111111111111111")
	require.NoError(t, err)
	tx := wire.NewMsgTx(1)
	tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(prevHash, 2), []byte{0x51}, [][]byte{{0x01, 0x02, 0x03}}))
	tx.AddTxOut(wire.NewTxOut(123456789, script))

	var buf bytes.Buffer
	require.NoError(t, tx.Serialize(&buf))
	return buf.Bytes(), tx, hex.EncodeToString(script)
}
