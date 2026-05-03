//go:build unittest

package pearl

import (
	"reflect"
	"strings"
	"testing"

	"github.com/pearl-research-labs/pearl/node/btcutil/hdkeychain"
	"github.com/pearl-research-labs/pearl/node/chaincfg"
	"github.com/trezor/blockbook/bchain"
)

func testPearlXpub(t *testing.T) string {
	t.Helper()

	seed := []byte("0123456789abcdef0123456789abcdef")
	key, err := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	path := []uint32{
		86 + hdkeychain.HardenedKeyStart,
		chaincfg.HDCoinTypePearl + hdkeychain.HardenedKeyStart,
		0 + hdkeychain.HardenedKeyStart,
	}
	for _, child := range path {
		key, err = key.Derive(child)
		if err != nil {
			t.Fatal(err)
		}
	}
	xpub, err := key.Neuter()
	if err != nil {
		t.Fatal(err)
	}
	standardXpub, err := xpub.CloneWithVersion([]byte{0x04, 0x88, 0xb2, 0x1e})
	if err != nil {
		t.Fatal(err)
	}
	return standardXpub.String()
}

func TestPearlParserXpubDerivation(t *testing.T) {
	parser := NewPearlParser("main", &Configuration{XPubMagic: 78792518})
	xpub := testPearlXpub(t)

	descriptor, err := parser.ParseXpub(xpub)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Type != bchain.P2TR {
		t.Fatalf("descriptor.Type = %v, want %v", descriptor.Type, bchain.P2TR)
	}
	if len(descriptor.ChangeIndexes) != 2 || descriptor.ChangeIndexes[0] != 0 || descriptor.ChangeIndexes[1] != 1 {
		t.Fatalf("descriptor.ChangeIndexes = %v, want [0 1]", descriptor.ChangeIndexes)
	}

	addrDescs, err := parser.DeriveAddressDescriptorsFromTo(descriptor, 0, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	addrs, searchable, err := parser.GetAddressesFromAddrDesc(addrDescs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !searchable {
		t.Fatal("derived address descriptor is not searchable")
	}
	if len(addrs) != 1 || !strings.HasPrefix(addrs[0], "prl1p") {
		t.Fatalf("derived addresses = %v, want one Pearl Taproot address", addrs)
	}
}

func TestPearlParserXpubDescriptor(t *testing.T) {
	parser := NewPearlParser("main", &Configuration{XPubMagic: 78792518})
	xpub := testPearlXpub(t)

	tests := []struct {
		name          string
		descriptor    string
		changeIndexes []uint32
	}{
		{
			name:          "single change index",
			descriptor:    "tr(" + xpub + "/0/*)",
			changeIndexes: []uint32{0},
		},
		{
			name:          "comma change list with checksum",
			descriptor:    "tr([5c9e228d/86'/808276'/0']" + xpub + "/{0,1,2}/*)#4rqwxvej",
			changeIndexes: []uint32{0, 1, 2},
		},
		{
			name:          "semicolon change list with hardened path aliases",
			descriptor:    "tr([5c9e228d/86h/808276h/0h]" + xpub + "/<0;1>/*)#4rqwxvej",
			changeIndexes: []uint32{0, 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			descriptor, err := parser.ParseXpub(tt.descriptor)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(descriptor.ChangeIndexes, tt.changeIndexes) {
				t.Fatalf("descriptor.ChangeIndexes = %v, want %v", descriptor.ChangeIndexes, tt.changeIndexes)
			}
			if _, err := parser.DeriveAddressDescriptors(descriptor, tt.changeIndexes[0], []uint32{0, 1}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPearlParserRejectsUnsupportedDescriptorTypes(t *testing.T) {
	parser := NewPearlParser("main", &Configuration{XPubMagic: 78792518})
	xpub := testPearlXpub(t)

	if _, err := parser.ParseXpub("wpkh(" + xpub + "/0/*)"); err == nil {
		t.Fatal("ParseXpub() error = nil, want error for unsupported descriptor type")
	}
}
