// Copyright (c) 2024 Hemi Labs, Inc.
// Use of this source code is governed by the MIT License,
// which can be found in the LICENSE file.

package tbcd

import (
	"context"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/hemilabs/heminetwork/database"
)

type Database interface {
	database.Database

	// Metadata
	Version(ctx context.Context) (int, error)
	MetadataGet(ctx context.Context, key []byte) ([]byte, error)
	MetadataPut(ctx context.Context, key, value []byte) error

	// Block header
	BlockHeaderByHash(ctx context.Context, hash []byte) (*BlockHeader, error)
	BlockHeadersBest(ctx context.Context) ([]BlockHeader, error)
	BlockHeadersInsert(ctx context.Context, bhs []BlockHeader) error
	BlockHeadersByHeight(ctx context.Context, height uint64) ([]BlockHeader, error)

	// Block
	BlocksMissing(ctx context.Context, count int) ([]BlockIdentifier, error)
	BlockInsert(ctx context.Context, b *Block) (int64, error)
	// XXX replace BlockInsert with plural version
	// BlocksInsert(ctx context.Context, bs []*Block) (int64, error)

	// Transactions
	//UTxosInsert(ctx context.Context, butxos []BlockUtxo) error
	UTxosInsert(ctx context.Context, blockhash []byte, utxos []Utxo) error

	// Peer manager
	PeersStats(ctx context.Context) (int, int)               // good, bad count
	PeersInsert(ctx context.Context, peers []Peer) error     // insert or update
	PeerDelete(ctx context.Context, host, port string) error // remove peer
	PeersRandom(ctx context.Context, count int) ([]Peer, error)
}

type BlockHeader struct {
	Hash   database.ByteArray
	Height uint64
	Header database.ByteArray
}

type Block struct {
	Hash  database.ByteArray
	Block database.ByteArray
}

//type BlockUtxos struct {
//	BlockHash database.ByteArray
//	Utxos     []BlockUtxo
//}

type Utxo struct {
	Hash        database.ByteArray
	SpendScript database.ByteArray
	Index       uint32
	Value       uint64
}

//type UtxoLocation struct {
//	BlockHash database.ByteArray
//	Index     uint32
//}
//
//type UtxoBalance struct {
//	SpendScript database.ByteArray
//	Value       uint64
//}

// BlockIdentifier uniquely identifies a block using it's hash and height.
type BlockIdentifier struct {
	Height uint64
	Hash   database.ByteArray
}

// Peer
type Peer struct {
	Host      string
	Port      string
	LastAt    database.Timestamp `deep:"-"` // Last time connected
	CreatedAt database.Timestamp `deep:"-"`
}

// BlockUtxos extracts all unspent transaction scripts  from the provided
// block.
func BlockUtxos(cp *chaincfg.Params, bb []byte) (*chainhash.Hash, []Utxo, error) {
	b, err := btcutil.NewBlockFromBytes(bb)
	if err != nil {
		return nil, nil, err
	}

	txs := b.Transactions()
	utxos := make([]Utxo, 0, len(txs))
	for _, tx := range txs {
		for _, txOut := range tx.MsgTx().TxOut {
			txCHash := tx.Hash()
			utxos = append(utxos, Utxo{
				Hash:        txCHash[:],
				SpendScript: txOut.PkScript,
				Index:       uint32(tx.Index()),
				Value:       uint64(txOut.Value),
			})
		}
	}

	return b.Hash(), utxos, nil
}
