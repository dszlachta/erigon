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

package builder

import (
	"sync"

	"github.com/ledgerwatch/erigon/core/types"
)

type LatestBlockBuiltStore struct {
	block *types.Block

	lock sync.Mutex
}

func NewLatestBlockBuiltStore() *LatestBlockBuiltStore {
	return &LatestBlockBuiltStore{}
}

func (s *LatestBlockBuiltStore) AddBlockBuilt(block *types.Block) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.block = block
}

func (s *LatestBlockBuiltStore) BlockBuilt() *types.Block {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.block
}
