package builder

import (
	"github.com/ledgerwatch/erigon/cl/cltypes"
)

type ExecutionPayloadHeader struct {
	Version string `json:"version"`
	Data    struct {
		Message   cltypes.Eth1Header `json:"message"`
		Signature string             `json:"signature"`
	} `json:"data"`
}

type BlindedBlockResponse struct {
	Version string            `json:"version"`
	Data    cltypes.Eth1Block `json:"data"`
}
