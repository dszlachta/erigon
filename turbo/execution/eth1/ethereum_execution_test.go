package eth1

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/c2h5oh/datasize"
	libcommon "github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/common/datadir"
	"github.com/erigontech/erigon-lib/common/dbg"
	"github.com/erigontech/erigon-lib/direct"
	"github.com/erigontech/erigon-lib/gointerfaces"
	proto_downloader "github.com/erigontech/erigon-lib/gointerfaces/downloaderproto"
	execution "github.com/erigontech/erigon-lib/gointerfaces/executionproto"
	"github.com/erigontech/erigon-lib/kv/temporal/temporaltest"
	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon-lib/wrap"
	"github.com/erigontech/erigon/consensus"
	"github.com/erigontech/erigon/core"
	"github.com/erigontech/erigon/core/rawdb/blockio"
	"github.com/erigontech/erigon/core/types"
	"github.com/erigontech/erigon/eth/consensuschain"
	"github.com/erigontech/erigon/eth/ethconfig"
	"github.com/erigontech/erigon/eth/ethconsensusconfig"
	"github.com/erigontech/erigon/eth/stagedsync"
	"github.com/erigontech/erigon/eth/stagedsync/stages"
	"github.com/erigontech/erigon/p2p"
	"github.com/erigontech/erigon/p2p/sentry"
	"github.com/erigontech/erigon/p2p/sentry/sentry_multi_client"
	"github.com/erigontech/erigon/turbo/engineapi/engine_helpers"
	"github.com/erigontech/erigon/turbo/execution/eth1/eth1_utils"
	"github.com/erigontech/erigon/turbo/shards"
	"github.com/erigontech/erigon/turbo/snapshotsync/freezeblocks"
	stages2 "github.com/erigontech/erigon/turbo/stages"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"golang.org/x/sync/semaphore"
)

func setup(tempDir string, testReporter gomock.TestReporter) (*EthereumExecutionModule, *types.Block) {
	dirs := datadir.New(tempDir)

	db, agg := temporaltest.NewTestDB(nil, dirs)

	// db := mdbx2.NewMDBX(log.New()).Path(dirs.DataDir).Exclusive().InMem(dirs.DataDir).Label(kv.ChainDB).MustOpen()

	// Genesis block
	network := "mainnet"
	genesis := core.GenesisBlockByChainName(network)
	tx, err := db.BeginRw(context.Background())
	if err != nil {
		panic(err)
		// t.Fatal(err)
	}
	defer tx.Rollback()
	chainConfig, genesisBlock, err := core.WriteGenesisBlock(tx, genesis, nil, dirs, log.New())
	// require.NoError(t, err)
	// expect := params.GenesisHashByChainName(network)
	// require.NotNil(t, expect, network)
	// require.EqualValues(t, genesisBlock.Hash(), *expect, network)
	tx.Commit()

	// 0. Setup
	cfg := ethconfig.Defaults
	cfg.StateStream = true
	cfg.BatchSize = 1 * datasize.MB
	cfg.Sync.BodyDownloadTimeoutSeconds = 10
	cfg.DeprecatedTxPool.Disable = true
	cfg.DeprecatedTxPool.StartOnInit = true
	cfg.Dirs = dirs
	cfg.Genesis = genesis

	// 10. Logger
	logger := log.Root()
	logger.SetHandler(log.LvlFilterHandler(log.LvlError, log.StderrHandler))

	notifications := &shards.Notifications{
		Events:      shards.NewEvents(),
		Accumulator: shards.NewAccumulator(),
	}

	// 1. Block reader/writer
	allSnapshots := freezeblocks.NewRoSnapshots(ethconfig.Defaults.Snapshot, dirs.Snap, 0, logger)
	allBorSnapshots := freezeblocks.NewBorRoSnapshots(ethconfig.Defaults.Snapshot, dirs.Snap, 0, logger)
	blockReader := freezeblocks.NewBlockReader(allSnapshots, allBorSnapshots)
	blockWriter := blockio.NewBlockWriter()

	// 13. Context
	ctx := context.Background()

	blockSnapBuildSema := semaphore.NewWeighted(int64(dbg.BuildSnapshotAllowance))
	// require.NoError(t, err)
	agg.SetSnapshotBuildSema(blockSnapBuildSema)

	// 11. Consensu Engine
	engine := ethconsensusconfig.CreateConsensusEngineBareBones(ctx, chainConfig, log.New())
	const blockBufferSize = 128

	statusDataProvider := sentry.NewStatusDataProvider(
		db,
		chainConfig,
		genesisBlock,
		chainConfig.ChainID.Uint64(),
		logger,
	)
	// limit "new block" broadcasts to at most 10 random peers at time
	maxBlockBroadcastPeers := func(header *types.Header) uint { return 10 }

	sentriesClient, err := sentry_multi_client.NewMultiClient(
		db,
		chainConfig,
		engine,
		[]direct.SentryClient{}, /*sentries*/
		cfg.Sync,
		blockReader,
		blockBufferSize,
		statusDataProvider,
		false,
		maxBlockBroadcastPeers,
		false, /* disableBlockDownload */
		logger,
	)
	// require.NoError(t, err)

	// 4. Fork validator
	inMemoryExecution := func(txc wrap.TxContainer, header *types.Header, body *types.RawBody, unwindPoint uint64, headersChain []*types.Header, bodiesChain []*types.RawBody,
		notifications *shards.Notifications) error {
		terseLogger := log.New()
		terseLogger.SetHandler(log.LvlFilterHandler(log.LvlWarn, log.StderrHandler))
		// Needs its own notifications to not update RPC daemon and txpool about pending blocks
		stateSync := stages2.NewInMemoryExecution(ctx, db, &cfg, sentriesClient, dirs,
			notifications, blockReader, blockWriter, agg, nil, terseLogger)
		chainReader := consensuschain.NewReader(chainConfig, txc.Tx, blockReader, logger)
		// We start the mining step
		if err := stages2.StateStep(ctx, chainReader, engine, txc, stateSync, header, body, unwindPoint, headersChain, bodiesChain, true); err != nil {
			logger.Warn("Could not validate block", "err", err)
			return errors.Join(consensus.ErrInvalidBlock, err)
		}
		var progress uint64
		progress, err = stages.GetStageProgress(txc.Tx, stages.Execution)
		if err != nil {
			return err
		}
		if progress < header.Number.Uint64() {
			return fmt.Errorf("unsuccessful execution, progress %d < expected %d", progress, header.Number.Uint64())
		}
		return nil
	}
	forkValidator := engine_helpers.NewForkValidator(ctx, 1 /*currentBlockNumber*/, inMemoryExecution, dirs.Tmp, blockReader)

	// 3. Staged Sync
	snapDownloader := proto_downloader.NewMockDownloaderClient(gomock.NewController(testReporter))
	blockRetire := freezeblocks.NewBlockRetire(1, dirs, blockReader, blockWriter, db, chainConfig, notifications.Events, blockSnapBuildSema, logger)

	pipelineStages := stages2.NewPipelineStages(ctx, db, &cfg, p2p.Config{}, sentriesClient, notifications,
		snapDownloader, blockReader, blockRetire, agg, nil, forkValidator, logger, true /*checkStateRoot*/)
	stagedSync := stagedsync.New(cfg.Sync, pipelineStages, stagedsync.PipelineUnwindOrder, stagedsync.PipelinePruneOrder, logger)

	return NewEthereumExecutionModule(blockReader, db, stagedSync, forkValidator, chainConfig, nil /*builderFunc*/, nil /*hook*/, notifications.Accumulator, notifications.StateChangesConsumer, logger, engine, cfg.Sync, ctx),
		genesisBlock
}

func TestExecutionModuleInitialization(t *testing.T) {
	executionModule, _ := setup(t.TempDir(), t)
	require.NotNil(t, executionModule)
}

var (
	block1RootHash = libcommon.HexToHash("34eca9cd7324e3a1df317e439a18119ad9a3c988fbf4d20783bb7bee56bafd64")
	block2RootHash = libcommon.HexToHash("66d801330ca4ebb926acd4cf890ba4bd015fb5a4716c414fec6ff739c0d4a2a1")
	block3RootHash = libcommon.HexToHash("2b79f72d542fe0f65da292c052be82af08dda0d415b5fdce0cd191121ca0d971")
	block4RootHash = libcommon.HexToHash("e8d36b208e37daa1be5893d4806dcce4573fc9d275271c1f569e399c77d0c157")
)

func SampleBlock(parent *types.Header, rootHash libcommon.Hash) *types.Block {
	return types.NewBlockWithHeader(&types.Header{
		Number:     new(big.Int).Add(parent.Number, big.NewInt(1)),
		Difficulty: new(big.Int).Add(parent.Number, big.NewInt(17000000000)),
		ParentHash: parent.Hash(),
		//Beneficiary: crypto.PubkeyToAddress(crypto.MustGenerateKey().PublicKey),
		TxHash:      types.EmptyRootHash,
		ReceiptHash: types.EmptyRootHash,
		GasLimit:    10000000,
		GasUsed:     0,
		Time:        parent.Time + 12,
		Root:        rootHash,
	})
}

func TestExecutionModuleBlockInsertion(t *testing.T) {
	executionModule, genesisBlock := setup(t.TempDir(), t)

	newBlock := SampleBlock(genesisBlock.Header(), block1RootHash)

	request := &execution.InsertBlocksRequest{
		Blocks: eth1_utils.ConvertBlocksToRPC([]*types.Block{newBlock}),
	}

	result, err := executionModule.InsertBlocks(executionModule.bacgroundCtx, request)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, result.Result, execution.ExecutionStatus_Success)
}

func TestExecutionModuleValidateChainSingleBlock(t *testing.T) {
	executionModule, genesisBlock := setup(t.TempDir(), t)

	newBlock := SampleBlock(genesisBlock.Header(), block1RootHash)

	request := &execution.InsertBlocksRequest{
		Blocks: eth1_utils.ConvertBlocksToRPC([]*types.Block{newBlock}),
	}

	result, err := executionModule.InsertBlocks(executionModule.bacgroundCtx, request)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, result.Result, execution.ExecutionStatus_Success)

	validationRequest := &execution.ValidationRequest{
		Hash:   gointerfaces.ConvertHashToH256(newBlock.Hash()),
		Number: newBlock.Number().Uint64(),
	}

	validationResult, err := executionModule.ValidateChain(executionModule.bacgroundCtx, validationRequest)
	require.NoError(t, err)
	require.NotNil(t, validationResult)
	require.Equal(t, validationResult.ValidationStatus, execution.ExecutionStatus_Success)
}

func TestExecutionModuleForkchoiceUpdateSingleBlock(t *testing.T) {
	executionModule, genesisBlock := setup(t.TempDir(), t)

	newBlock := SampleBlock(genesisBlock.Header(), block1RootHash)

	request := &execution.InsertBlocksRequest{
		Blocks: eth1_utils.ConvertBlocksToRPC([]*types.Block{newBlock}),
	}

	result, err := executionModule.InsertBlocks(executionModule.bacgroundCtx, request)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, result.Result, execution.ExecutionStatus_Success)

	validationRequest := &execution.ValidationRequest{
		Hash:   gointerfaces.ConvertHashToH256(newBlock.Hash()),
		Number: newBlock.Number().Uint64(),
	}

	validationResult, err := executionModule.ValidateChain(executionModule.bacgroundCtx, validationRequest)
	require.NoError(t, err)
	require.NotNil(t, validationResult)
	require.Equal(t, validationResult.ValidationStatus, execution.ExecutionStatus_Success)

	forkchoiceRequest := &execution.ForkChoice{
		HeadBlockHash:      gointerfaces.ConvertHashToH256(newBlock.Hash()),
		Timeout:            10_000,
		FinalizedBlockHash: gointerfaces.ConvertHashToH256(genesisBlock.Hash()),
		SafeBlockHash:      gointerfaces.ConvertHashToH256(genesisBlock.Hash()),
	}

	fcuReceipt, err := executionModule.UpdateForkChoice(executionModule.bacgroundCtx, forkchoiceRequest)
	require.NoError(t, err)
	require.NotNil(t, fcuReceipt)
	require.Equal(t, fcuReceipt.Status, execution.ExecutionStatus_Success)
	require.Equal(t, "", fcuReceipt.ValidationError)
	require.Equal(t, fcuReceipt.LatestValidHash, gointerfaces.ConvertHashToH256(newBlock.Hash()))
}

func TestExecutionModuleForkchoiceUpdateNoPreviousVerify(t *testing.T) {
	executionModule, genesisBlock := setup(t.TempDir(), t)

	newBlock := SampleBlock(genesisBlock.Header(), block1RootHash)

	request := &execution.InsertBlocksRequest{
		Blocks: eth1_utils.ConvertBlocksToRPC([]*types.Block{newBlock}),
	}

	result, err := executionModule.InsertBlocks(executionModule.bacgroundCtx, request)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, result.Result, execution.ExecutionStatus_Success)

	forkchoiceRequest := &execution.ForkChoice{
		HeadBlockHash:      gointerfaces.ConvertHashToH256(newBlock.Hash()),
		Timeout:            10_000,
		FinalizedBlockHash: gointerfaces.ConvertHashToH256(genesisBlock.Hash()),
		SafeBlockHash:      gointerfaces.ConvertHashToH256(genesisBlock.Hash()),
	}

	fcuReceipt, err := executionModule.UpdateForkChoice(executionModule.bacgroundCtx, forkchoiceRequest)
	require.NoError(t, err)
	require.NotNil(t, fcuReceipt)
	require.Equal(t, fcuReceipt.Status, execution.ExecutionStatus_Success)
}

func TestExecutionModuleForkchoiceUpdateMultipleBlocksFirst(t *testing.T) {
	executionModule, genesisBlock := setup(t.TempDir(), t)

	blocks := []*types.Block{
		SampleBlock(genesisBlock.Header(), block1RootHash),
		SampleBlock(genesisBlock.Header(), block1RootHash),
		SampleBlock(genesisBlock.Header(), block1RootHash),
	}

	for _, block := range blocks {
		request := &execution.InsertBlocksRequest{
			Blocks: eth1_utils.ConvertBlocksToRPC([]*types.Block{block}),
		}

		result, err := executionModule.InsertBlocks(executionModule.bacgroundCtx, request)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, result.Result, execution.ExecutionStatus_Success)

	}

	for _, block := range blocks {
		validationRequest := &execution.ValidationRequest{
			Hash:   gointerfaces.ConvertHashToH256(block.Hash()),
			Number: block.Number().Uint64(),
		}

		validationResult, err := executionModule.ValidateChain(executionModule.bacgroundCtx, validationRequest)
		require.NoError(t, err)
		require.NotNil(t, validationResult)
		require.Equal(t, validationResult.ValidationStatus, execution.ExecutionStatus_Success)
	}

	forkchoiceRequest := &execution.ForkChoice{
		HeadBlockHash:      gointerfaces.ConvertHashToH256(blocks[0].Hash()),
		Timeout:            10_000,
		FinalizedBlockHash: gointerfaces.ConvertHashToH256(genesisBlock.Hash()),
		SafeBlockHash:      gointerfaces.ConvertHashToH256(genesisBlock.Hash()),
	}

	fcuReceipt, err := executionModule.UpdateForkChoice(executionModule.bacgroundCtx, forkchoiceRequest)
	require.NoError(t, err)
	require.NotNil(t, fcuReceipt)
	require.Equal(t, fcuReceipt.Status, execution.ExecutionStatus_Success)
}

func TestExecutionModuleForkchoiceUpdateMultipleBlocksLast(t *testing.T) {
	executionModule, genesisBlock := setup(t.TempDir(), t)

	blocks := []*types.Block{
		SampleBlock(genesisBlock.Header(), block1RootHash),
		SampleBlock(genesisBlock.Header(), block1RootHash),
		SampleBlock(genesisBlock.Header(), block1RootHash),
	}

	for _, block := range blocks {
		request := &execution.InsertBlocksRequest{
			Blocks: eth1_utils.ConvertBlocksToRPC([]*types.Block{block}),
		}

		result, err := executionModule.InsertBlocks(executionModule.bacgroundCtx, request)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, result.Result, execution.ExecutionStatus_Success)

	}

	for _, block := range blocks {
		validationRequest := &execution.ValidationRequest{
			Hash:   gointerfaces.ConvertHashToH256(block.Hash()),
			Number: block.Number().Uint64(),
		}

		validationResult, err := executionModule.ValidateChain(executionModule.bacgroundCtx, validationRequest)
		require.NoError(t, err)
		require.NotNil(t, validationResult)
		require.Equal(t, validationResult.ValidationStatus, execution.ExecutionStatus_Success)
	}

	forkchoiceRequest := &execution.ForkChoice{
		HeadBlockHash:      gointerfaces.ConvertHashToH256(blocks[2].Hash()),
		Timeout:            10_000,
		FinalizedBlockHash: gointerfaces.ConvertHashToH256(genesisBlock.Hash()),
		SafeBlockHash:      gointerfaces.ConvertHashToH256(genesisBlock.Hash()),
	}

	fcuReceipt, err := executionModule.UpdateForkChoice(executionModule.bacgroundCtx, forkchoiceRequest)
	require.NoError(t, err)
	require.NotNil(t, fcuReceipt)
	require.Equal(t, fcuReceipt.Status, execution.ExecutionStatus_Success)
}

func TestExecutionModuleForkchoiceUpdateLongChain(t *testing.T) {
	executionModule, genesisBlock := setup(t.TempDir(), t)

	newBlock1 := SampleBlock(genesisBlock.Header(), block1RootHash)
	newBlock2 := SampleBlock(newBlock1.Header(), block2RootHash)
	newBlock3 := SampleBlock(newBlock2.Header(), block3RootHash)
	newBlock4 := SampleBlock(newBlock3.Header(), block4RootHash)

	blocks := []*types.Block{
		newBlock1,
		newBlock2,
		newBlock3,
		newBlock4,
	}

	for _, block := range blocks {
		request := &execution.InsertBlocksRequest{
			Blocks: eth1_utils.ConvertBlocksToRPC([]*types.Block{block}),
		}

		result, err := executionModule.InsertBlocks(executionModule.bacgroundCtx, request)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, result.Result, execution.ExecutionStatus_Success)
	}

	validationRequest := &execution.ValidationRequest{
		Hash:   gointerfaces.ConvertHashToH256(newBlock4.Hash()),
		Number: newBlock4.Number().Uint64(),
	}

	validationResult, err := executionModule.ValidateChain(executionModule.bacgroundCtx, validationRequest)
	require.NoError(t, err)
	require.NotNil(t, validationResult)
	require.Equal(t, validationResult.ValidationStatus, execution.ExecutionStatus_Success)

	forkchoiceRequest := &execution.ForkChoice{
		HeadBlockHash:      gointerfaces.ConvertHashToH256(newBlock4.Hash()),
		Timeout:            10_000,
		FinalizedBlockHash: gointerfaces.ConvertHashToH256(genesisBlock.Hash()),
		SafeBlockHash:      gointerfaces.ConvertHashToH256(genesisBlock.Hash()),
	}

	fcuReceipt, err := executionModule.UpdateForkChoice(executionModule.bacgroundCtx, forkchoiceRequest)
	require.NoError(t, err)
	require.NotNil(t, fcuReceipt)
	require.Equal(t, fcuReceipt.Status, execution.ExecutionStatus_Success)
}

func BenchmarkExecutionModuleValidateSingleBlock(b *testing.B) {
	executionModule, genesisBlock := setup(b.TempDir(), b)

	blocks := []*types.Block{
		SampleBlock(genesisBlock.Header(), block1RootHash),
		SampleBlock(genesisBlock.Header(), block1RootHash),
		SampleBlock(genesisBlock.Header(), block1RootHash),
		SampleBlock(genesisBlock.Header(), block1RootHash),
		SampleBlock(genesisBlock.Header(), block1RootHash),
		SampleBlock(genesisBlock.Header(), block1RootHash),
		SampleBlock(genesisBlock.Header(), block1RootHash),
		SampleBlock(genesisBlock.Header(), block1RootHash),
		SampleBlock(genesisBlock.Header(), block1RootHash),
		SampleBlock(genesisBlock.Header(), block1RootHash),
	}

	for _, block := range blocks {
		request := &execution.InsertBlocksRequest{
			Blocks: eth1_utils.ConvertBlocksToRPC([]*types.Block{block}),
		}

		result, err := executionModule.InsertBlocks(executionModule.bacgroundCtx, request)
		require.NoError(b, err)
		require.NotNil(b, result)
		require.Equal(b, result.Result, execution.ExecutionStatus_Success)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {

		selectedBlockNo := i % len(blocks)

		validationRequest := &execution.ValidationRequest{
			Hash:   gointerfaces.ConvertHashToH256(blocks[selectedBlockNo].Hash()),
			Number: blocks[0].Number().Uint64(),
		}

		validationResult, err := executionModule.ValidateChain(executionModule.bacgroundCtx, validationRequest)
		require.NoError(b, err)
		require.NotNil(b, validationResult)
		require.Equal(b, validationResult.ValidationStatus, execution.ExecutionStatus_Success)
	}
}

func BenchmarkExecutionModuleInsertValidateFcu(b *testing.B) {
	executionModule, genesisBlock := setup(b.TempDir(), b)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		//insert block
		block := SampleBlock(genesisBlock.Header(), block1RootHash)
		request := &execution.InsertBlocksRequest{
			Blocks: eth1_utils.ConvertBlocksToRPC([]*types.Block{block}),
		}
		result, err := executionModule.InsertBlocks(executionModule.bacgroundCtx, request)
		require.NoError(b, err)
		require.NotNil(b, result)
		require.Equal(b, execution.ExecutionStatus_Success, result.Result)

		//validate block
		validationRequest := &execution.ValidationRequest{
			Hash:   gointerfaces.ConvertHashToH256(block.Hash()),
			Number: block.Number().Uint64(),
		}
		validationResult, err := executionModule.ValidateChain(executionModule.bacgroundCtx, validationRequest)
		require.NoError(b, err)
		require.NotNil(b, validationResult)
		require.Equal(b, execution.ExecutionStatus_Success, validationResult.ValidationStatus)

		//update forkchoice
		forkchoiceRequest := &execution.ForkChoice{
			HeadBlockHash:      gointerfaces.ConvertHashToH256(block.Hash()),
			Timeout:            10_000,
			FinalizedBlockHash: gointerfaces.ConvertHashToH256(genesisBlock.Hash()),
			SafeBlockHash:      gointerfaces.ConvertHashToH256(genesisBlock.Hash()),
		}

		fcuReceipt, err := executionModule.UpdateForkChoice(executionModule.bacgroundCtx, forkchoiceRequest)
		require.NoError(b, err)
		require.NotNil(b, fcuReceipt)
		require.Equal(b, execution.ExecutionStatus_Success, fcuReceipt.Status)

		// //wait until execution module is ready
		// for {
		// 	ready, err := executionModule.Ready(executionModule.bacgroundCtx, &emptypb.Empty{})
		// 	require.NoError(b, err)
		// 	if ready.Ready {
		// 		break
		// 	}
		// }
	}
}