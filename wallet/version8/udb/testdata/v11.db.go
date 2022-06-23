// Copyright (c) 2018 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

// This file should be compiled from the commit the file was introduced,
// otherwise it may not compile due to API changes, or may not create the
// database with the correct old version.  This file should not be updated for
// API changes.

// V11 test database layout and v12 upgrade test plan
//
// The v12 database upgrade introduces buckets to track ticket commitments
// information in the transaction manager namespace (bucketTicketCommitments and
// bucketTicketCommitmentsUsp). These buckets are meant to track outstanding
// ticket commitment outputs for the purposes of correct balance calculation: it
// allows non-voting wallets (eg: funding wallets in solo-voting setups or
// non-voter participants of split tickets) to track their proportional locked
// funds. In standard (single-voter) VSP setups, it also allows to correctly
// discount the pool fee for correct accounting of total locked funds.
//
// The v11 database generated by this file is meant to be used on the v12
// upgrade verification test (verifyV12Upgrade). This database is setup with a
// set of tickets, votes and revocations in order to test that the upgrade was
// done successfully and that the balances generated after the upgrade are
// correct.
//
// Each generated ticket uses an address from an odd-numbered account as vote
// (ticket submission) address, an address from the next account as commitment
// address and a fixed, non-wallet address for pool fee commitment. Votes and
// revocations are generated from a corresponding ticket as needed.
//
// Using different accounts for each combination of
// {mined,unmined}{ticket,vote,revocation} allows the upgrade test to easily
// verify that the upgrade and balance functions are performing correctly by
// checking each account for the expected balance.

package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"github.com/Decred-Next/dcrnd/chaincfg/chainhash/v8"
	"github.com/Decred-Next/dcrnd/gcs/version1/v8"
	"github.com/Decred-Next/dcrnd/wire/v8"
	errors "github.com/Decred-Next/dcrnwallet/v8/errors/version8"
	_ "github.com/Decred-Next/dcrnwallet/v8/wallet/version8/internal/bdb"
	"github.com/Decred-Next/dcrnwallet/v8/wallet/version8/udb"
	"github.com/Decred-Next/dcrnwallet/v8/wallet/version8/walletdb"
	"github.com/decred/dcrd/blockchain/stake"
	"github.com/decred/dcrd/chaincfg"
	"github.com/decred/dcrd/dcrutil"
	"github.com/decred/dcrd/txscript"
)

const dbname = "v11.db"

var (
	epoch    time.Time
	pubPass  = []byte("public")
	privPass = []byte("private")
)

var chainParams = &chaincfg.TestNet3Params

func main() {
	err := setup()
	if err != nil {
		fmt.Fprintf(os.Stderr, "setup: %v\n", err)
		os.Exit(1)
	}
	err = compress()
	if err != nil {
		fmt.Fprintf(os.Stderr, "compress: %v\n", err)
		os.Exit(1)
	}
}

func pay2ssgen(addr dcrutil.Address) []byte {
	s, err := txscript.PayToSSGen(addr)
	if err != nil {
		panic(err)
	}
	return s
}

func pay2ssrtx(addr dcrutil.Address) []byte {
	s, err := txscript.PayToSSRtx(addr)
	if err != nil {
		panic(err)
	}
	return s
}

func pay2sstx(addr dcrutil.Address) []byte {
	s, err := txscript.PayToSStx(addr)
	if err != nil {
		panic(err)
	}
	return s
}

func pay2sstxChange() []byte {
	//OP_SSTXCHANGE OP_DUP OP_HASH160 0000000000000000000000000000000000000000 OP_EQUALVERIFY OP_CHECKSIG
	return []byte{0xbd, 0x76, 0xa9, 0x14, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x88, 0xac}
}

func sstxAddrPush(addr dcrutil.Address, amount int64) []byte {
	s, err := txscript.GenerateSStxAddrPush(addr, dcrutil.Amount(amount), 0x0058)
	if err != nil {
		panic(err)
	}
	return s
}

func dummyTxIn(idx *uint32) *wire.TxIn {
	var prevHash chainhash.Hash
	*idx++
	return wire.NewTxIn(wire.NewOutPoint(&prevHash, *idx, 0), 0, nil)
}

func stakeBaseTxIn() *wire.TxIn {
	var prevHash chainhash.Hash
	return wire.NewTxIn(wire.NewOutPoint(&prevHash, math.MaxUint32, 0), 0, nil)
}

func ticketSpendTxIn(ticket *wire.MsgTx) *wire.TxIn {
	th := ticket.TxHash()
	return wire.NewTxIn(wire.NewOutPoint(&th, 0, 1), 0, nil)
}

func setup() error {
	db, err := walletdb.Create("bdb", dbname)
	if err != nil {
		return err
	}
	defer db.Close()
	var seed [32]byte
	err = udb.Initialize(db, chainParams, seed[:], pubPass, privPass)
	if err != nil {
		return err
	}

	amgr, txmgr, _, err := udb.Open(db, chainParams, pubPass)
	if err != nil {
		return err
	}

	return walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		amgrns := tx.ReadWriteBucket([]byte("waddrmgr"))
		txmgrns := tx.ReadWriteBucket([]byte("wtxmgr"))
		err := amgr.Unlock(amgrns, privPass)
		if err != nil {
			return err
		}

		// The following are constants used throughout the db setup process.

		// Assumes the manager on a new wallet only (and always) creates account 0
		lastAcctIdx := uint32(0)
		branchIdx := uint32(0)
		commitmentAmt := int64(1000)
		commitmentAmtReward := int64(300)
		poolFee := int64(100)
		poolFeeReward := int64(30)
		ticketPrice := commitmentAmt + poolFee
		poolFeeAddr, _ := dcrutil.DecodeAddress("TsR28UZRprhgQQhzWns2M6cAwchrNVvbYq2")
		txinIdx := uint32(0)
		var blockHash chainhash.Hash
		var block udb.Block
		var blockMeta udb.BlockMeta

		// The following are helper functions, defined as closures to generate
		// test data.

		// Returns the first usable address for the next account of the test.
		nextAddress := func() (dcrutil.Address, error) {
			lastAcctIdx++
			_, err := amgr.NewAccount(amgrns, fmt.Sprintf("%d", lastAcctIdx))
			if err != nil {
				return nil, err
			}
			err = amgr.SyncAccountToAddrIndex(amgrns, lastAcctIdx, 1, branchIdx)
			if err != nil {
				return nil, err
			}
			xpubBranch, err := amgr.AccountBranchExtendedPubKey(tx, lastAcctIdx,
				branchIdx)
			if err != nil {
				return nil, err
			}
			xpubChild, err := xpubBranch.Child(0)
			if err != nil {
				return nil, err
			}
			addr, err := xpubChild.Address(chainParams)
			if err != nil {
				return nil, err
			}
			return addr, nil
		}

		// Generate a ticket. Generates 2 new accounts, uses an address from the
		// first as vote address and an address from the second as commitment.
		// Also adds a pool fee commitment to an address not from the wallet.
		genTicket := func() (*wire.MsgTx, error) {
			voteAddr, err := nextAddress()
			if err != nil {
				return nil, err
			}

			commitAddr, err := nextAddress()
			if err != nil {
				return nil, err
			}

			tx := wire.NewMsgTx()
			tx.AddTxIn(dummyTxIn(&txinIdx))
			tx.AddTxIn(dummyTxIn(&txinIdx))
			tx.AddTxOut(wire.NewTxOut(ticketPrice, pay2sstx(voteAddr)))
			tx.AddTxOut(wire.NewTxOut(0, sstxAddrPush(poolFeeAddr, poolFee)))
			tx.AddTxOut(wire.NewTxOut(0, pay2sstxChange()))
			tx.AddTxOut(wire.NewTxOut(0, sstxAddrPush(commitAddr, commitmentAmt)))
			tx.AddTxOut(wire.NewTxOut(0, pay2sstxChange()))
			return tx, nil
		}

		// Generate a vote for the given ticket.
		genVote := func(ticket *wire.MsgTx) (*wire.MsgTx, error) {
			poolFeeAddr, err := stake.AddrFromSStxPkScrCommitment(ticket.TxOut[1].PkScript,
				chainParams)
			if err != nil {
				return nil, err
			}

			commitAddr, err := stake.AddrFromSStxPkScrCommitment(ticket.TxOut[3].PkScript,
				chainParams)
			if err != nil {
				return nil, err
			}

			blockRef := bytes.Repeat([]byte{0x00}, 36)
			prevBlockScript := append([]byte{0x6a, 0x24}, blockRef...)

			tx := wire.NewMsgTx()
			tx.AddTxIn(stakeBaseTxIn())
			tx.AddTxIn(ticketSpendTxIn(ticket))
			tx.AddTxOut(wire.NewTxOut(0, prevBlockScript))                      // prev block
			tx.AddTxOut(wire.NewTxOut(0, []byte{0x6a, 0x03, 0x00, 0x00, 0x00})) // vote bits
			tx.AddTxOut(wire.NewTxOut(poolFee+poolFeeReward, pay2ssgen(poolFeeAddr)))
			tx.AddTxOut(wire.NewTxOut(commitmentAmt+commitmentAmtReward, pay2ssgen(commitAddr)))

			return tx, nil
		}

		// Generate a revocation for the given ticket.
		genRevoke := func(ticket *wire.MsgTx) (*wire.MsgTx, error) {
			poolFeeAddr, err := stake.AddrFromSStxPkScrCommitment(ticket.TxOut[1].PkScript,
				chainParams)
			if err != nil {
				return nil, err
			}

			commitAddr, err := stake.AddrFromSStxPkScrCommitment(ticket.TxOut[3].PkScript,
				chainParams)
			if err != nil {
				return nil, err
			}

			tx := wire.NewMsgTx()
			tx.AddTxIn(ticketSpendTxIn(ticket))
			tx.AddTxOut(wire.NewTxOut(poolFee-poolFeeReward, pay2ssrtx(poolFeeAddr)))
			tx.AddTxOut(wire.NewTxOut(commitmentAmt-commitmentAmtReward, pay2ssrtx(commitAddr)))
			return tx, nil
		}

		// Insert this transaction and its credits into the tx store. This is a
		// subset of what happens in chainntfns' ProcessTransaction() as of
		// version 11 of the database.
		addTx := func(tx *wire.MsgTx, mined bool) error {
			rec, err := udb.NewTxRecordFromMsgTx(tx, epoch)
			if err != nil {
				return err
			}
			var blockMetaToUse *udb.BlockMeta
			if mined {
				err = txmgr.InsertMinedTx(txmgrns, amgrns, rec, &blockHash)
				if err != nil {
					return err
				}
				blockMetaToUse = &blockMeta
			} else {
				err = txmgr.InsertMemPoolTx(txmgrns, rec)
				if err != nil {
					return err
				}
			}
			for i, txout := range tx.TxOut {
				if txout.Value == 0 {
					continue
				}

				_, addrs, _, err := txscript.ExtractPkScriptAddrs(txout.Version,
					txout.PkScript, chainParams)
				if err != nil {
					return nil
				}
				if len(addrs) == 0 {
					return errors.New("should have an address")
				}
				ma, err := amgr.Address(amgrns, addrs[0])
				if errors.Is(err, errors.NotExist) {
					continue
				}
				if err != nil {
					return err
				}
				err = txmgr.AddCredit(txmgrns, rec, blockMetaToUse,
					uint32(i), ma.Internal(), ma.Account())
				if err != nil {
					return err
				}
			}

			return nil
		}

		// Generate and add a ticket as mined.
		addMinedTicket := func() (*wire.MsgTx, error) {
			ticket, err := genTicket()
			if err != nil {
				return nil, err
			}
			err = addTx(ticket, true)
			if err != nil {
				return nil, err
			}
			return ticket, nil
		}

		// Add a block to the database. All mined transactions added will be
		// registered on this block. While this breaks many consensus for
		// stake transactions, it is fine for simple database testing.
		prevBlock := chainParams.GenesisHash
		buf := bytes.Buffer{}
		blockHeader := &wire.BlockHeader{
			Version:      1,
			PrevBlock:    *prevBlock,
			StakeVersion: 1,
			VoteBits:     1,
			Height:       uint32(1),
		}
		blockHash = blockHeader.BlockHash()
		headerData := udb.BlockHeaderData{
			BlockHash: blockHash,
		}
		copy(headerData.SerializedHeader[:], buf.Bytes())
		nullgcskey := [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
		nullb := bytes.Repeat([]byte{0}, 16)
		gcsFilter, err := gcs.NewFilter(1, nullgcskey, [][]byte{nullb})
		if err != nil {
			return err
		}
		err = txmgr.ExtendMainChain(txmgrns, blockHeader, gcsFilter)
		if err != nil {
			return err
		}
		block = udb.Block{
			Hash:   headerData.BlockHash,
			Height: int32(1),
		}
		blockMeta = udb.BlockMeta{
			Block: block,
			Time:  epoch,
		}

		// Start adding the test data. The initial default account is 0.
		var ticket, vote, revoke *wire.MsgTx

		// Unmined ticket (accounts 1 & 2)
		if ticket, err = genTicket(); err != nil {
			return err
		}
		if err = addTx(ticket, false); err != nil {
			return err
		}

		// Mined ticket (accounts 3 & 4)
		if ticket, err = genTicket(); err != nil {
			return err
		}
		if err = addTx(ticket, true); err != nil {
			return err
		}

		// Mined ticket with unmined vote (accounts 5 & 6).
		if ticket, err = addMinedTicket(); err != nil {
			return err
		}
		if vote, err = genVote(ticket); err != nil {
			return err
		}
		if err = addTx(vote, false); err != nil {
			return err
		}

		// Mined ticket with mined vote (accounts 7 & 8).
		if ticket, err = addMinedTicket(); err != nil {
			return err
		}
		if vote, err = genVote(ticket); err != nil {
			return err
		}
		if err = addTx(vote, true); err != nil {
			return err
		}

		// Mined ticket with unmined revocation (accounts 9 & 10).
		if ticket, err = addMinedTicket(); err != nil {
			return err
		}
		if revoke, err = genRevoke(ticket); err != nil {
			return err
		}
		if err = addTx(revoke, false); err != nil {
			return err
		}

		// Mined ticket with mined revocation (accounts 11 & 12).
		if ticket, err = addMinedTicket(); err != nil {
			return err
		}
		if revoke, err = genRevoke(ticket); err != nil {
			return err
		}
		if err = addTx(revoke, true); err != nil {
			return err
		}

		return nil
	})
}

func compress() error {
	db, err := os.Open(dbname)
	if err != nil {
		return err
	}
	defer os.Remove(dbname)
	defer db.Close()
	dbgz, err := os.Create(dbname + ".gz")
	if err != nil {
		return err
	}
	defer dbgz.Close()
	gz := gzip.NewWriter(dbgz)
	_, err = io.Copy(gz, db)
	if err != nil {
		return err
	}
	return gz.Close()
}
