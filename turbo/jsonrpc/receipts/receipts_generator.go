package receipts

import (
	"context"
	"fmt"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/ledgerwatch/erigon-lib/chain"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/log/v3"
	"github.com/ledgerwatch/erigon/consensus"
	"github.com/ledgerwatch/erigon/core"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/core/state"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/core/vm"
	"github.com/ledgerwatch/erigon/turbo/services"
	"github.com/ledgerwatch/erigon/turbo/transactions"
)

type Generator struct {
	receiptsCache *lru.Cache[common.Hash, []*types.Receipt]
	blockReader   services.FullBlockReader
	engine        consensus.EngineReader
}

func NewGenerator(receiptsCache *lru.Cache[common.Hash, []*types.Receipt], blockReader services.FullBlockReader,
	engine consensus.EngineReader) *Generator {
	return &Generator{
		receiptsCache: receiptsCache,
		blockReader:   blockReader,
		engine:        engine,
	}
}

var yes, no int

func (g *Generator) GetReceipts(ctx context.Context, cfg *chain.Config, tx kv.Tx, block *types.Block, senders []common.Address) (types.Receipts, error) {
	if receipts, ok := g.receiptsCache.Get(block.Hash()); ok {
		yes++
		return receipts, nil
	}
	no++
	if receipts := rawdb.ReadReceipts(tx, block, senders); receipts != nil {
		g.receiptsCache.Add(block.Hash(), receipts)
		return receipts, nil
	}
	if no%10 == 0 {
		fmt.Printf("receipts_generator.go:47: %d/%d\n", no, yes)
	}

	engine := g.engine

	_, _, _, ibs, _, err := transactions.ComputeTxEnv(ctx, engine, block, cfg, g.blockReader, tx, 0)
	if err != nil {
		return nil, err
	}

	usedGas := new(uint64)
	usedBlobGas := new(uint64)
	gp := new(core.GasPool).AddGas(block.GasLimit()).AddBlobGas(cfg.GetMaxBlobGasPerBlock())

	noopWriter := state.NewNoopWriter()

	receipts := make(types.Receipts, len(block.Transactions()))

	getHeader := func(hash common.Hash, number uint64) *types.Header {
		h, e := g.blockReader.Header(ctx, tx, hash, number)
		if e != nil {
			log.Error("getHeader error", "number", number, "hash", hash, "err", e)
		}
		return h
	}
	header := block.HeaderNoCopy()
	for i, txn := range block.Transactions() {
		ibs.SetTxContext(txn.Hash(), block.Hash(), i)
		receipt, _, err := core.ApplyTransaction(cfg, core.GetHashFn(header, getHeader), engine, nil, gp, ibs, noopWriter, header, txn, usedGas, usedBlobGas, vm.Config{})
		if err != nil {
			return nil, err
		}
		receipt.BlockHash = block.Hash()
		receipts[i] = receipt
	}

	g.receiptsCache.Add(block.Hash(), receipts)
	return receipts, nil
}
