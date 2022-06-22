// Copyright (c) 2018 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package udb

import (
	"github.com/Decred-Next/dcrnd/chaincfg/chainhash/v8"
	"github.com/Decred-Next/dcrnd/gcs/version1/v8"
	"github.com/Decred-Next/dcrnd/gcs/version1/v8/blockcf"
	"github.com/decred/dcrwallet/wallet/v3/walletdb"
)

// CFilter returns the saved regular compact filter for a block.
func (s *Store) CFilter(dbtx walletdb.ReadTx, blockHash *chainhash.Hash) (*gcs.Filter, error) {
	ns := dbtx.ReadBucket(wtxmgrBucketKey)
	v, err := fetchRawCFilter(ns, blockHash[:])
	if err != nil {
		return nil, err
	}
	vc := make([]byte, len(v)) // Copy for FromNBytes which stores passed slice
	copy(vc, v)
	return gcs.FromNBytes(blockcf.P, vc)
}