// Copyright (c) 2024 Hemi Labs, Inc.
// Use of this source code is governed by the MIT License,
// which can be found in the LICENSE file.

package level

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/hemilabs/heminetwork/database"
	"github.com/hemilabs/heminetwork/database/level"
	"github.com/hemilabs/heminetwork/database/tbcd"
	"github.com/juju/loggo"
	"github.com/syndtr/goleveldb/leveldb"
)

// XXX before committing this conver json to gob

// Locking order:
//	BlockHeaders
// 	BlocksMissing
// 	Blocks

const (
	ldbVersion = 1

	logLevel = "INFO"
	verbose  = false

	bhsLastKey = "last"
)

var log = loggo.GetLogger("level")

func init() {
	loggo.ConfigureLoggers(logLevel)
}

type ldb struct {
	mtx sync.Mutex

	*level.Database
	pool level.Pool
}

var _ tbcd.Database = (*ldb)(nil)

func New(ctx context.Context, home string) (*ldb, error) {
	log.Tracef("New")
	defer log.Tracef("New exit")

	ld, err := level.New(ctx, home, ldbVersion)
	if err != nil {
		return nil, err
	}
	log.Debugf("tbcdb database version: %v", ldbVersion)
	l := &ldb{
		Database: ld,
		pool:     ld.DB(),
	}

	return l, nil
}

func (l *ldb) Version(ctx context.Context) (int, error) {
	// XXX
	return ldbVersion, nil
}

func (l *ldb) BlockHeaderByHash(ctx context.Context, hash []byte) (*tbcd.BlockHeader, error) {
	log.Tracef("BlockHeaderByHash")
	defer log.Tracef("BlockHeaderByHash exit")

	// XXX this pattern repeats itself, see if we can make this generic

	bhsDB := l.pool[level.BlockHeadersDB]
	tx, err := bhsDB.OpenTransaction()
	if err != nil {
		return nil, fmt.Errorf("block headers best transaction: %w", err)
	}
	discard := true
	defer func() {
		if discard {
			log.Debugf("BlockHeadersBest discarding transaction")
			tx.Discard()
		}
	}()

	// Get last record
	j, err := tx.Get(hash, nil)
	if err != nil {
		if err == leveldb.ErrNotFound {
			return nil, database.NotFoundError(fmt.Sprintf("header not found: %x", hash))
		}
		return nil, fmt.Errorf("block headers best: %w", err)
	}
	var bh tbcd.BlockHeader
	err = json.Unmarshal(j, &bh)
	if err != nil {
		return nil, fmt.Errorf("block headers best unmarshal: %w", err)
	}

	err = tx.Commit()
	if err != nil {
		return nil, fmt.Errorf("block headers best: %w", err)
	}

	discard = false

	return &bh, nil
}

func (l *ldb) BlockHeadersBest(ctx context.Context) ([]tbcd.BlockHeader, error) {
	log.Tracef("BlockHeadersBest")
	defer log.Tracef("BlockHeadersBest exit")

	bhsDB := l.pool[level.BlockHeadersDB]
	tx, err := bhsDB.OpenTransaction()
	if err != nil {
		return nil, fmt.Errorf("block headers best transaction: %w", err)
	}
	discard := true
	defer func() {
		if discard {
			log.Debugf("BlockHeadersBest discarding transaction")
			tx.Discard()
		}
	}()

	// Get last record
	j, err := tx.Get([]byte(bhsLastKey), nil)
	if err != nil {
		if err == leveldb.ErrNotFound {
			return []tbcd.BlockHeader{}, nil
		}
		return nil, fmt.Errorf("block headers best: %w", err)
	}
	var bh tbcd.BlockHeader
	err = json.Unmarshal(j, &bh)
	if err != nil {
		return nil, fmt.Errorf("block headers best unmarshal: %w", err)
	}

	err = tx.Commit()
	if err != nil {
		return nil, fmt.Errorf("block headers best: %w", err)
	}

	discard = false

	return []tbcd.BlockHeader{bh}, nil
}

// heightHashToKey generates a sortable key from height and hash. With this key
// we can iterate over the block headers table and see what block records are
// missing.
func heightHashToKey(height uint64, hash []byte) []byte {
	if len(hash) != chainhash.HashSize {
		panic(fmt.Sprintf("invalid hash size: %v", len(hash)))
	}
	key := make([]byte, 8+1+chainhash.HashSize)
	binary.BigEndian.PutUint64(key[0:8], height)
	copy(key[9:], hash)
	return key
}

// keyToHeightHash reverses the process of heightHashToKey.
func keyToHeightHash(key []byte) (uint64, []byte) {
	if len(key) != 8+1+chainhash.HashSize {
		panic(fmt.Sprintf("invalid key size: %v", len(key)))
	}
	hash := make([]byte, chainhash.HashSize) // must make copy!
	copy(hash, key[9:])
	return binary.BigEndian.Uint64(key[0:8]), hash
}

func (l *ldb) BlockHeadersInsert(ctx context.Context, bhs []tbcd.BlockHeader) error {
	log.Tracef("BlockHeadersInsert")
	defer log.Tracef("BlockHeadersInsert exit")

	if len(bhs) == 0 {
		return fmt.Errorf("block headers insert: no block headers to insert")
	}

	// Open the block headers database transaction early to block db
	bhsDB := l.pool[level.BlockHeadersDB]
	bhsTx, err := bhsDB.OpenTransaction()
	if err != nil {
		return fmt.Errorf("block headers open transaction: %w", err)
	}
	bhsDiscard := true
	defer func() {
		if bhsDiscard {
			log.Debugf("BlockHeadersInsert discarding transaction: %v",
				len(bhs))
			bhsTx.Discard()
		}
	}()

	// Open the blocks missing database transaction early to block db
	bmDB := l.pool[level.BlocksMissingDB]
	bmTx, err := bmDB.OpenTransaction()
	if err != nil {
		return fmt.Errorf("blocks missing open transaction: %w", err)
	}
	bmDiscard := true
	defer func() {
		if bmDiscard {
			log.Debugf("BlockHeadersInsert discarding transaction: %v",
				len(bhs)) // Yes, bhs, this is not a bug.
			bmTx.Discard()
		}
	}()

	// Make sure we are not inserting the same blocks
	has, err := bhsTx.Has(bhs[0].Hash, nil)
	if err != nil {
		return fmt.Errorf("block headers insert has: %v", err)
	}
	if has {
		return database.DuplicateError("block headers insert duplicate")
	}

	// Insert missing blocks and block headers
	var lastRecord []byte
	bmBatch := new(leveldb.Batch)
	bhsBatch := new(leveldb.Batch)
	for k := range bhs {
		// Height 0 is genesis, we do not want a missing block record for that.
		if bhs[k].Height != 0 {
			// Insert a synthesized height_hash key that serves as
			// an index to see which blocks are missing.
			bmBatch.Put(heightHashToKey(bhs[k].Height, bhs[k].Hash[:]), []byte{})
		}

		// Insert JSON encoded block header record
		bhs[k].CreatedAt = database.NewTimestamp(time.Now())
		bhj, err := json.Marshal(bhs[k])
		if err != nil {
			return fmt.Errorf("json marshal %v: %w", k, err)
		}
		bhsBatch.Put(bhs[k].Hash, bhj)
		lastRecord = bhj
	}

	// Insert last height into block headers XXX this does not deal with forks
	bhsBatch.Put([]byte(bhsLastKey), lastRecord)

	// Write missing blocks batch
	err = bmTx.Write(bmBatch, nil)
	if err != nil {
		return fmt.Errorf("blocks missing insert: %w", err)
	}

	// Write block headers batch
	err = bhsTx.Write(bhsBatch, nil)
	if err != nil {
		return fmt.Errorf("block headers insert: %w", err)
	}

	// Reverse order commit missing blocks.
	// If this is committed and the block headers fail, that is ok. It will
	// simply be overwritten later.
	err = bmTx.Commit()
	if err != nil {
		return fmt.Errorf("blocks missing commit: %w", err)
	}
	bmDiscard = false

	// Commit block headers table
	err = bhsTx.Commit()
	if err != nil {
		return fmt.Errorf("block headers commit: %w", err)
	}
	bhsDiscard = false

	return nil
}

// XXX return hash and height only
func (l *ldb) BlocksMissing(ctx context.Context, count int) ([]tbcd.BlockIdentifier, error) {
	log.Tracef("BlockHeadersMissing")
	defer log.Tracef("BlockHeadersMissing exit")

	bmDB := l.pool[level.BlocksMissingDB]
	bmTx, err := bmDB.OpenTransaction()
	if err != nil {
		return nil, fmt.Errorf("blocks missing open transaction: %w", err)
	}
	bmDiscard := true
	defer func() {
		if bmDiscard {
			log.Debugf("BlockHeadersMissing discarding transaction")
			bmTx.Discard()
		}
	}()

	x := 0
	bis := make([]tbcd.BlockIdentifier, 0, count)
	it := bmTx.NewIterator(nil, nil)
	defer it.Release()
	for it.Next() {
		bh := tbcd.BlockIdentifier{}
		bh.Height, bh.Hash = keyToHeightHash(it.Key())
		bis = append(bis, bh)

		x++
		if x >= count {
			break
		}
	}

	err = bmTx.Commit()
	if err != nil {
		return nil, fmt.Errorf("blocks missing commit: %w", err)
	}
	bmDiscard = false

	return bis, nil
}

func (l *ldb) BlockInsert(ctx context.Context, b *tbcd.Block) (int64, error) {
	log.Tracef("BlockInsert")
	defer log.Tracef("BlockInsert exit")

	// Open the block headers database transaction
	bhsDB := l.pool[level.BlockHeadersDB]
	bhsTx, err := bhsDB.OpenTransaction()
	if err != nil {
		return -1, fmt.Errorf("block headers open transaction: %w", err)
	}
	bhsDiscard := true
	defer func() {
		if bhsDiscard {
			log.Debugf("BlockInsert discarding transaction")
			bhsTx.Discard()
		}
	}()

	// Open the blocks missing database transaction
	bmDB := l.pool[level.BlocksMissingDB]
	bmTx, err := bmDB.OpenTransaction()
	if err != nil {
		return -1, fmt.Errorf("blocks missing open transaction: %w", err)
	}
	bmDiscard := true
	defer func() {
		if bmDiscard {
			log.Debugf("BlockInsert block missing discarding transaction")
			bmTx.Discard()
		}
	}()

	// Open the blocks database transaction
	bDB := l.pool[level.BlocksDB]
	bTx, err := bDB.OpenTransaction()
	if err != nil {
		return -1, fmt.Errorf("blocks open transaction: %w", err)
	}
	bDiscard := true
	defer func() {
		if bDiscard {
			log.Debugf("BlockInsert discarding transaction")
			bTx.Discard()
		}
	}()

	// Determine block height
	bhj, err := bhsTx.Get(b.Hash[:], nil)
	if err != nil {
		if err == leveldb.ErrNotFound {
			return -1, database.NotFoundError(fmt.Sprintf("block header not found: %x", b.Hash))
		}
		return -1, fmt.Errorf("block insert block header: %w", err)
	}
	var bh tbcd.BlockHeader
	err = json.Unmarshal(bhj, &bh)
	if err != nil {
		return -1, fmt.Errorf("block insert unmarshal: %w", err)
	}

	// Remove block identifier from blocks missing
	key := heightHashToKey(bh.Height, bh.Hash)
	err = bmTx.Delete(key, nil)
	if err != nil {
		// Ignore not found
		if err == leveldb.ErrNotFound {
			log.Errorf("block insert delete from missing: %v", err)
		} else {
			return -1, fmt.Errorf("block insert delete from missing: %v", err)
		}
	}

	// Insert block
	bj, err := json.Marshal(b)
	if err != nil {
		return -1, fmt.Errorf("block insert marshal: %v", err)
	}
	err = bTx.Put(b.Hash[:], bj, nil)
	if err != nil {
		return -1, fmt.Errorf("block insert put: %v", err)
	}

	// Reverse order unlock
	err = bTx.Commit()
	if err != nil {
		return -1, fmt.Errorf("block commit: %w", err)
	}
	bDiscard = false

	err = bmTx.Commit()
	if err != nil {
		return -1, fmt.Errorf("blocks missing commit: %w", err)
	}
	bmDiscard = false

	err = bhsTx.Commit()
	if err != nil {
		return -1, fmt.Errorf("blocks headers commit: %w", err)
	}
	bhsDiscard = false

	// XXX think about Height type; why are we forced to mix types?
	return int64(bh.Height), nil
}

func (l *ldb) PeersInsert(ctx context.Context, peers []tbcd.Peer) error {
	log.Tracef("PeersInsert")
	defer log.Tracef("PeersInsert exit")

	return fmt.Errorf("PeersInsert")
}

func (l *ldb) PeerDelete(ctx context.Context, host, port string) error {
	log.Tracef("PeerDelete")
	defer log.Tracef("PeerDelete exit")

	return fmt.Errorf("PeerDelete")
}

func (l *ldb) PeersRandom(ctx context.Context, count int) ([]tbcd.Peer, error) {
	log.Tracef("PeersRandom")
	defer log.Tracef("PeersRandom exit")

	// XXX always return nothing for now to DNS seed
	return []tbcd.Peer{}, nil
}
