// Copyright 2018 The go-ethereum Authors
// (original work)
// Copyright 2024 The Erigon Authors
// (modifications)
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

package ethdb

import (
	"errors"

	"github.com/ledgerwatch/erigon-lib/kv"
)

// DESCRIBED: For info on database buckets see docs/programmers_guide/db_walkthrough.MD

// ErrKeyNotFound is returned when key isn't found in the database.
var ErrKeyNotFound = errors.New("db: key not found")

type HasTx interface {
	Tx() kv.Tx
}
