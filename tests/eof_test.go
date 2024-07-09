package tests

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/ledgerwatch/erigon-lib/log/v3"
)

func TestEOFValidation(t *testing.T) {
	defer log.Root().SetHandler(log.Root().GetHandler())
	log.Root().SetHandler(log.LvlFilterHandler(log.LvlError, log.StderrHandler))

	bt := new(testMatcher)

	dir := filepath.Join(".", "eof_tests/prague/eip7692_eof_v1")

	bt.walk(t, dir, func(t *testing.T, name string, test *EOFTest) {
		// import pre accounts & construct test genesis block & state root
		if err := et.checkFailure(t, test.Run(t)); err != nil {
			t.Error(err)
		}
		fmt.Println("---------------------------------")
	})
}

func TestEOFBlockchain(t *testing.T) {
	defer log.Root().SetHandler(log.Root().GetHandler())
	log.Root().SetHandler(log.LvlFilterHandler(log.LvlError, log.StderrHandler))

	bt := new(testMatcher)

	dir := filepath.Join(".", "blockchain_tests/prague/eip7692_eof_v1/eip663_dupn_swapn_exchange/dupn")

	bt.walk(t, dir, func(t *testing.T, name string, test *BlockTest) {
		// import pre accounts & construct test genesis block & state root
		if err := bt.checkFailure(t, test.Run(t, true)); err != nil {
			t.Error(err)
		}
		fmt.Println("---------------------------------")
	})
}
