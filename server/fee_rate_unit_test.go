//go:build unittest

package server

import (
	"math/big"
	"testing"

	"github.com/trezor/blockbook/api"
	"github.com/trezor/blockbook/common"
)

func TestFeePerByteUnit(t *testing.T) {
	fees := (*api.Amount)(big.NewInt(2250))
	tests := []struct {
		name         string
		coinShortcut string
		tx           *api.Tx
		want         string
	}{
		{"pearl vsize", "PRL", &api.Tx{FeesSat: fees, VSize: 150, Size: 200}, "15.00 grain/vByte"},
		{"pearl size only", "PRL", &api.Tx{FeesSat: fees, Size: 225}, "10.00 grain/Byte"},
		{"pearl lowercase shortcut", " prl ", &api.Tx{FeesSat: fees, VSize: 150}, "15.00 grain/vByte"},
		{"bitcoin vsize", "BTC", &api.Tx{FeesSat: fees, VSize: 150}, "15.00 sat/vByte"},
		{"bitcoin size only", "BTC", &api.Tx{FeesSat: fees, Size: 225}, "10.00 sat/Byte"},
		{"no fee", "PRL", &api.Tx{VSize: 150}, ""},
		{"no size", "PRL", &api.Tx{FeesSat: fees}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &PublicServer{is: &common.InternalState{CoinShortcut: tt.coinShortcut}}
			if got := s.feePerByte(tt.tx); got != tt.want {
				t.Errorf("feePerByte() = %q, want %q", got, tt.want)
			}
		})
	}
}
