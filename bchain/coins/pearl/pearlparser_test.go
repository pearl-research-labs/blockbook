//go:build unittest

package pearl

import (
	"strings"
	"testing"

	"github.com/pearl-research-labs/pearl/node/btcutil/hdkeychain"
	"github.com/pearl-research-labs/pearl/node/chaincfg"
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
