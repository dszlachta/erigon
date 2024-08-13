// Copyright 2024 The Erigon Authors
// This file is part of Erigon.
//
// Erigon is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Erigon is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Erigon. If not, see <http://www.gnu.org/licenses/>.

package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/erigontech/erigon-lib/kv"
	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon/common/u256"
	"github.com/erigontech/erigon/core"
	"github.com/erigontech/erigon/core/state"
	"github.com/erigontech/erigon/params"
	bortypes "github.com/erigontech/erigon/polygon/bor/types"
	"github.com/erigontech/erigon/polygon/polygoncommon"

	libcommon "github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon/accounts/abi"
	"github.com/erigontech/erigon/core/types"
	"github.com/erigontech/erigon/polygon/bor/borcfg"
	"github.com/erigontech/erigon/polygon/heimdall"
)

var ErrMapNotAvailable = errors.New("map not available")

type fetchSyncEventsType func(ctx context.Context, fromId uint64, to time.Time, limit int) ([]*heimdall.EventRecordWithTime, error)

type Bridge struct {
	store                    Store
	ready                    atomic.Bool
	lastProcessedBlockNumber atomic.Uint64
	lastProcessedEventID     atomic.Uint64

	logger             log.Logger
	borConfig          *borcfg.BorConfig
	stateReceiverABI   abi.ABI
	stateClientAddress libcommon.Address
	fetchSyncEvents    fetchSyncEventsType
}

type Reader struct {
	store              Store
	log                log.Logger
	stateClientAddress libcommon.Address
}

func Assemble(dataDir string, logger log.Logger, borConfig *borcfg.BorConfig, fetchSyncEvents fetchSyncEventsType, stateReceiverABI abi.ABI) *Bridge {
	bridgeDB := polygoncommon.NewDatabase(dataDir, kv.PolygonBridgeDB, databaseTablesCfg, logger, false)
	bridgeStore := NewStore(bridgeDB)
	return NewBridge(bridgeStore, logger, borConfig, fetchSyncEvents, stateReceiverABI)
}

func AssembleReader(ctx context.Context, dataDir string, logger log.Logger) (*Reader, error) {
	bridgeDB := polygoncommon.NewDatabase(dataDir, kv.PolygonBridgeDB, databaseTablesCfg, logger, true)
	bridgeStore := NewStore(bridgeDB)

	borConfig := &borcfg.BorConfig{}
	if err := json.Unmarshal(params.BorMainnetChainConfig.BorJSON, borConfig); err != nil {
		return nil, fmt.Errorf("invalid chain config 'bor' JSON. err: %w", err)
	}

	err := bridgeStore.Prepare(ctx)
	if err != nil {
		return nil, err
	}

	return NewReader(bridgeStore, logger, borConfig.StateReceiverContract), nil
}

func NewBridge(store Store, logger log.Logger, borConfig *borcfg.BorConfig, fetchSyncEvents fetchSyncEventsType, stateReceiverABI abi.ABI) *Bridge {
	return &Bridge{
		store:              store,
		logger:             logger,
		borConfig:          borConfig,
		fetchSyncEvents:    fetchSyncEvents,
		stateReceiverABI:   stateReceiverABI,
		stateClientAddress: libcommon.HexToAddress(borConfig.StateReceiverContract),
	}
}

func NewReader(store Store, log log.Logger, stateReceiverContractAddress string) *Reader {
	return &Reader{
		store:              store,
		log:                log,
		stateClientAddress: libcommon.HexToAddress(stateReceiverContractAddress),
	}
}

func (b *Bridge) Run(ctx context.Context) error {
	err := b.store.Prepare(ctx)
	if err != nil {
		return err
	}
	defer b.Close()

	// get last known sync ID
	lastEventID, err := b.store.LatestEventID(ctx)
	if err != nil {
		return err
	}

	lastProcessedEventID, err := b.store.LastProcessedEventID(ctx)
	if err != nil {
		return err
	}

	b.lastProcessedEventID.Store(lastProcessedEventID)

	// start syncing
	b.logger.Debug(bridgeLogPrefix("Bridge is running"), "lastEventID", lastEventID)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// get all events from last sync ID to now
		to := time.Now()
		events, err := b.fetchSyncEvents(ctx, lastEventID+1, to, 0)
		if err != nil {
			return err
		}

		if len(events) != 0 {
			b.ready.Store(false)
			if err := b.store.PutEvents(ctx, events, b.stateReceiverABI); err != nil {
				return err
			}

			lastEventID = events[len(events)-1].ID
		} else {
			b.ready.Store(true)
			if err := libcommon.Sleep(ctx, 30*time.Second); err != nil {
				return err
			}
		}

		b.logger.Debug(bridgeLogPrefix(fmt.Sprintf("got %v new events, last event ID: %v, ready: %v", len(events), lastEventID, b.ready.Load())))
	}
}

func (b *Bridge) Close() {
	b.store.Close()
}

// EngineService interface implementations

// ProcessNewBlocks iterates through all blocks and constructs a map from block number to sync events
func (b *Bridge) ProcessNewBlocks(ctx context.Context, blocks []*types.Block) error {
	if len(blocks) == 0 {
		return nil
	}

	if err := b.Synchronize(ctx, blocks[len(blocks)-1].NumberU64()); err != nil {
		return err
	}

	eventMap := make(map[uint64]uint64)
	txMap := make(map[libcommon.Hash]uint64)
	var prevSprintTime time.Time

	for _, block := range blocks {
		// check if block is start of span
		blockNum := block.NumberU64()
		if !b.isSprintStart(blockNum) {
			continue
		}

		var timeLimit time.Time
		if b.borConfig.IsIndore(blockNum) {
			stateSyncDelay := b.borConfig.CalculateStateSyncDelay(blockNum)
			timeLimit = time.Unix(int64(block.Time()-stateSyncDelay), 0)
		} else {
			timeLimit = prevSprintTime
		}

		prevSprintTime = time.Unix(int64(block.Time()), 0)

		lastDBID, err := b.store.SprintLastEventID(ctx, b.lastProcessedEventID.Load(), timeLimit, b.stateReceiverABI)
		if err != nil {
			return err
		}

		if lastDBID > b.lastProcessedEventID.Load() {
			b.logger.Debug(bridgeLogPrefix(fmt.Sprintf("Creating map for block %d, start ID %d, end ID %d", blockNum, b.lastProcessedEventID.Load(), lastDBID)))

			k := bortypes.ComputeBorTxHash(blockNum, block.Hash())
			eventMap[blockNum] = b.lastProcessedEventID.Load()
			txMap[k] = blockNum

			b.lastProcessedEventID.Store(lastDBID)
		}

		b.lastProcessedBlockNumber.Store(blockNum)
	}

	err := b.store.PutEventIDs(ctx, eventMap)
	if err != nil {
		return err
	}

	err = b.store.PutEventTxnToBlockNum(ctx, txMap)
	if err != nil {
		return err
	}

	return nil
}

// Synchronize blocks till bridge has map at tip
func (b *Bridge) Synchronize(ctx context.Context, blockNum uint64) error {
	b.logger.Debug(bridgeLogPrefix("synchronizing events..."), "blockNum", blockNum)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if b.ready.Load() || b.lastProcessedBlockNumber.Load() >= blockNum {
			return nil
		}
	}
}

// Unwind deletes map entries till tip
func (b *Bridge) Unwind(ctx context.Context, blockNum uint64) error {
	return b.store.PruneEventIDs(ctx, blockNum)
}

// Events returns all sync events at blockNum
func (b *Bridge) Events(ctx context.Context, blockNum uint64) ([]*types.Message, error) {
	start, end, err := b.store.EventIDRange(ctx, blockNum)
	if err != nil {
		if errors.Is(err, ErrMapNotAvailable) {
			return nil, nil
		}

		return nil, err
	}

	if end == 0 { // exception for tip processing
		end = b.lastProcessedEventID.Load()
	}

	eventsRaw := make([]*types.Message, 0, end-start+1)

	// get events from DB
	events, err := b.store.Events(ctx, start+1, end+1)
	if err != nil {
		return nil, err
	}

	b.logger.Debug(bridgeLogPrefix(fmt.Sprintf("got %v events for block %v", len(events), blockNum)))

	// convert to message
	for _, event := range events {
		msg := types.NewMessage(
			state.SystemAddress,
			&b.stateClientAddress,
			0, u256.Num0,
			core.SysCallGasLimit,
			u256.Num0,
			nil, nil,
			event, nil, false,
			true,
			nil,
		)

		eventsRaw = append(eventsRaw, &msg)
	}

	return eventsRaw, nil
}

func (b *Bridge) EventTxnLookup(ctx context.Context, borTxHash libcommon.Hash) (uint64, bool, error) {
	return b.store.EventTxnToBlockNum(ctx, borTxHash)
}

// Helper functions
func (b *Bridge) isSprintStart(headerNum uint64) bool {
	if headerNum%b.borConfig.CalculateSprintLength(headerNum) != 0 || headerNum == 0 {
		return false
	}

	return true
}

// Reader

// Events returns all sync events at blockNum
func (r *Reader) Events(ctx context.Context, blockNum uint64) ([]*types.Message, error) {
	start, end, err := r.store.EventIDRange(ctx, blockNum)
	if err != nil {
		return nil, err
	}

	if end == 0 { // exception for tip processing
		end, err = r.store.LastProcessedEventID(ctx)
		if err != nil {
			return nil, err
		}
	}

	eventsRaw := make([]*types.Message, 0, end-start+1)

	// get events from DB
	events, err := r.store.Events(ctx, start+1, end+1)
	if err != nil {
		return nil, err
	}

	r.log.Debug(bridgeLogPrefix(fmt.Sprintf("got %v events for block %v", len(events), blockNum)))

	address := libcommon.HexToAddress("0x0000000000000000000000000000000000001001")

	// convert to message
	for _, event := range events {
		msg := types.NewMessage(
			state.SystemAddress,
			&address, // TODO: use variable from struct
			0, u256.Num0,
			core.SysCallGasLimit,
			u256.Num0,
			nil, nil,
			event, nil, false,
			true,
			nil,
		)

		eventsRaw = append(eventsRaw, &msg)
	}

	return eventsRaw, nil
}

func (r *Reader) EventTxnLookup(ctx context.Context, borTxHash libcommon.Hash) (uint64, bool, error) {
	return r.store.EventTxnToBlockNum(ctx, borTxHash)
}

func (r *Reader) Close() {
	r.store.Close()
}
