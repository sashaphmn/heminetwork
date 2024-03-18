// Copyright (c) 2024 Hemi Labs, Inc.
// Use of this source code is governed by the MIT License,
// which can be found in the LICENSE file.

package tbc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/dustin/go-humanize"
	"github.com/hemilabs/heminetwork/database"
	"github.com/hemilabs/heminetwork/service/deucalion"
	"github.com/juju/loggo"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hemilabs/heminetwork/database/tbcd"
	"github.com/hemilabs/heminetwork/database/tbcd/level"
)

const (
	logLevel = "INFO"

	promSubsystem = "tbc_service" // Prometheus

	mainnetPort = "8333"
	testnetPort = "18333"

	defaultPeersWanted   = 64
	defaultPendingBlocks = 128 // 128 * ~4MB max memory use
)

var (
	testnetSeeds = []string{
		"testnet-seed.bitcoin.jonasschnelli.ch",
		"seed.tbtc.petertodd.org",
		"seed.testnet.bitcoin.sprovoost.nl",
		"testnet-seed.bluematt.me",
	}
	mainnetSeeds = []string{
		"seed.bitcoin.sipa.be",
		"dnsseed.bluematt.me",
		"dnsseed.bitcoin.dashjr.org",
		"seed.bitcoinstats.com",
		"seed.bitnodes.io",
		"seed.bitcoin.jonasschnelli.ch",
	}
)

var log = loggo.GetLogger("tbc")

func init() {
	loggo.ConfigureLoggers(logLevel)
	rand.Seed(time.Now().UnixNano()) // used for seeding, ok to be math.rand
}

func header2Bytes(wbh *wire.BlockHeader) ([]byte, error) {
	var b bytes.Buffer
	err := wbh.Serialize(&b)
	if err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func h2b(wbh *wire.BlockHeader) []byte {
	hb, err := header2Bytes(wbh)
	if err != nil {
		panic(err)
	}
	return hb
}

func bytes2Header(header []byte) (*wire.BlockHeader, error) {
	var bh wire.BlockHeader
	err := bh.Deserialize(bytes.NewReader(header))
	if err != nil {
		return nil, fmt.Errorf("Deserialize: %v", err)
	}
	return &bh, nil
}

func headerTime(header []byte) *time.Time {
	h, err := bytes2Header(header)
	if err != nil {
		return nil
	}
	return &h.Timestamp
}

func hashEqual(h1 chainhash.Hash, h2 chainhash.Hash) bool {
	// Fuck you chainhash package
	return h1.IsEqual(&h2)
}

func sliceChainHash(ch chainhash.Hash) []byte {
	// Fuck you chainhash package
	return ch[:]
}

type blockPeer struct {
	expire time.Time // when does this command expire
	peer   string    // who was handling it
}

type Config struct {
	LevelDBHome             string
	LogLevel                string
	PgURI                   string
	PrometheusListenAddress string
	Network                 string
	BlockSanity             bool
}

func NewDefaultConfig() *Config {
	return &Config{
		LogLevel: logLevel,
	}
}

type Server struct {
	mtx sync.RWMutex
	wg  sync.WaitGroup

	cfg *Config

	// stats
	printTime       time.Time
	blocksSize      uint64 // cumulative block size written
	blocksInserted  map[string]struct{}
	blocksDuplicate int

	// bitcoin network
	wireNet     wire.BitcoinNet
	chainParams *chaincfg.Params
	timeSource  blockchain.MedianTimeSource
	port        string
	seeds       []string

	peers  map[string]*peer      // active but not necessarily connected
	blocks map[string]*blockPeer // outstanding block downloads [hash]when/where XXX audit

	isWorking       bool // reentrancy flag
	insertedGenesis bool // reentrancy flag

	db tbcd.Database

	// Prometheus
	isRunning bool
}

func NewServer(cfg *Config) (*Server, error) {
	if cfg == nil {
		cfg = NewDefaultConfig()
	}
	s := &Server{
		cfg:            cfg,
		printTime:      time.Now().Add(10 * time.Second),
		blocks:         make(map[string]*blockPeer, defaultPendingBlocks),
		peers:          make(map[string]*peer, defaultPeersWanted),
		blocksInserted: make(map[string]struct{}, 8192), // stats
		timeSource:     blockchain.NewMedianTime(),
	}

	// We could use a PGURI verification here.

	switch cfg.Network {
	case "mainnet":
		s.port = mainnetPort
		s.wireNet = wire.MainNet
		s.chainParams = &chaincfg.MainNetParams
		s.seeds = mainnetSeeds
	case "testnet3":
		s.port = testnetPort
		s.wireNet = wire.TestNet3
		s.chainParams = &chaincfg.TestNet3Params
		s.seeds = testnetSeeds
	default:
		return nil, fmt.Errorf("invalid network: %v", cfg.Network)
	}

	return s, nil
}

var (
	errCacheFull     = errors.New("cache full")
	errNoPeers       = errors.New("no peers")
	errAlreadyCached = errors.New("already cached")
	errExpiredPeer   = errors.New("expired peer")
)

// blockPeerAdd adds a block to the pending list at the selected peer. Lock
// must be held.
func (s *Server) blockPeerAdd(hash, peer string) error {
	if _, ok := s.peers[peer]; !ok {
		return errExpiredPeer // XXX should not happen
	}
	if _, ok := s.blocks[hash]; ok {
		return errAlreadyCached
	}
	s.blocks[hash] = &blockPeer{
		expire: time.Now().Add(37 * time.Second), // XXX make variable?
		peer:   peer,
	}
	return nil
}

// blockPeerExpire removes expired block downloads from the cache and returns
// the number of used cache slots.
func (s *Server) blockPeerExpire() int {
	log.Tracef("blockPeerExpire exit")
	defer log.Tracef("blockPeerExpire exit")

	now := time.Now()
	s.mtx.Lock()
	defer s.mtx.Unlock()

	for k, v := range s.blocks {
		if !now.After(v.expire) {
			continue
		}
		delete(s.blocks, k)
		log.Infof("expired block: %v", k) // XXX remove

		// kill peer as well since it is slow
		if p := s.peers[v.peer]; p != nil && p.conn != nil {
			p.conn.Close() // this will tear down peer
		}
	}
	return len(s.blocks)
}

func (s *Server) getHeaders(ctx context.Context, p *peer, lastHeaderHash []byte) error {
	bh, err := bytes2Header(lastHeaderHash)
	if err != nil {
		return fmt.Errorf("invalid header: %v", err)
	}
	hash := bh.BlockHash()
	ghs := wire.NewMsgGetHeaders()
	ghs.AddBlockLocatorHash(&hash)
	err = p.write(ghs)
	if err != nil {
		return fmt.Errorf("write get headers: %v", err)
	}

	return nil
}

func (s *Server) seed(pctx context.Context, peersWanted int) ([]tbcd.Peer, error) {
	log.Tracef("seed")
	defer log.Tracef("seed exit")

	peers, err := s.db.PeersRandom(pctx, peersWanted)
	if err != nil {
		return nil, fmt.Errorf("peers random: %v", err)
	}
	// return peers from db first
	if len(peers) >= peersWanted {
		return peers, nil
	}

	// Seed
	resolver := &net.Resolver{}
	ctx, cancel := context.WithTimeout(pctx, 15*time.Second)
	defer cancel()

	errorsSeen := 0
	var addrs []net.IP
	for k := range s.seeds {
		log.Infof("DNS seeding %v", s.seeds[k])
		ips, err := resolver.LookupIP(ctx, "ip", s.seeds[k])
		if err != nil {
			log.Errorf("lookup: %v", err)
			errorsSeen++
			continue
		}
		addrs = append(addrs, ips...)
	}
	if errorsSeen == len(s.seeds) {
		return nil, fmt.Errorf("could not seed")
	}

	// insert into peers table
	for k := range addrs {
		peers = append(peers, tbcd.Peer{
			Host: addrs[k].String(),
			Port: s.port,
		})
	}

	// return fake peers but don't save them to the database
	return peers, nil
}

func (s *Server) seedForever(ctx context.Context, peersWanted int) ([]tbcd.Peer, error) {
	log.Tracef("seedForever")
	defer log.Tracef("seedForever")

	minW := 5
	maxW := 59
	for {
		holdOff := time.Duration(minW+rand.Intn(maxW-minW)) * time.Second
		var em string
		peers, err := s.seed(ctx, peersWanted)
		if err != nil {
			em = fmt.Sprintf("seed error: %v, retrying in %v", err, holdOff)
		} else if peers != nil && len(peers) == 0 {
			em = fmt.Sprintf("no peers found, retrying in %v", holdOff)
		} else {
			// great success!
			return peers, nil
		}
		log.Errorf("%v", em)

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(holdOff):
		}
	}
}

// randPeerWrite really is randPeerWrite block. Don't use it for other
// commands. Lock must be held!
func (s *Server) randPeerWrite(ctx context.Context, hash string, msg wire.Message) error {
	log.Tracef("randPeerWrite")
	defer log.Tracef("randPeerWrite")

	var p *peer
	// Select random peer
	// s.mtx.Lock()
	if len(s.blocks) >= defaultPendingBlocks {
		// s.mtx.Unlock()
		return errCacheFull
	}
	for k, v := range s.peers {
		if v.conn == nil {
			// Not connected yet
			continue
		}

		// maybe insert into cache
		err := s.blockPeerAdd(hash, k)
		if err != nil {
			continue
		}

		// cached, now execute
		p = v
		break
	}
	// s.mtx.Unlock()

	if p == nil {
		return errNoPeers
	}
	return p.write(msg)
}

func (s *Server) peerAdd(p *peer) {
	log.Tracef("peerAdd: %v", p.address)
	s.mtx.Lock()
	s.peers[p.address] = p
	s.mtx.Unlock()
}

func (s *Server) peerDelete(address string) {
	log.Tracef("peerDelete: %v", address)
	s.mtx.Lock()
	delete(s.peers, address)
	s.mtx.Unlock()
}

func (s *Server) peersLen() int {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return len(s.peers)
}

func (s *Server) peerManager(ctx context.Context) error {
	log.Tracef("peerManager")
	defer log.Tracef("peerManager exit")

	// Channel for peering signals
	peersWanted := defaultPeersWanted
	peerC := make(chan string, peersWanted)

	log.Infof("Peer manager connecting to %v peers", peersWanted)
	seeds, err := s.seedForever(ctx, peersWanted)
	if err != nil {
		// context canceled
		return fmt.Errorf("seed: %w", err)
	}
	if len(seeds) == 0 {
		// should not happen
		return fmt.Errorf("no seeds found")
	}

	loopTimeout := 27 * time.Second
	loopTicker := time.NewTicker(loopTimeout)

	x := 0
	for {
		loopTicker.Reset(loopTimeout)

		peersActive := s.peersLen()
		log.Debugf("peerManager active %v wanted %v", peersActive, peersWanted)
		if peersActive < peersWanted {
			// XXX we may want to make peers play along with waitgroup

			// Connect peer
			for i := 0; i < peersWanted-peersActive; i++ {
				address := net.JoinHostPort(seeds[x].Host, seeds[x].Port)
				if len(address) < 7 {
					// weed out anything < len("0.0.0.0")
					continue
				}

				peer, err := NewPeer(s.wireNet, address)
				if err != nil {
					// This really should not happen
					log.Errorf("new peer: %v", err)
					continue
				}
				s.peerAdd(peer)

				go s.peerConnect(ctx, peerC, peer)

				x++
				if x >= len(seeds) {
					// XXX duplicate code from above
					seeds, err = s.seedForever(ctx, peersWanted)
					if err != nil {
						// Context canceled
						return fmt.Errorf("seed: %w", err)
					}
					if len(seeds) == 0 {
						// should not happen
						return fmt.Errorf("no seeds found")
					}
					x = 0
				}
			}
		}

		// Unfortunately we need a timer here to restart the loop.  The
		// error is a laptop goes to sleep, all peers disconnect, RSTs
		// are not seen by sleeping laptop, laptop wakes up. Now the
		// expiration timers are all expired but not noticed by the
		// laptop.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case address := <-peerC:
			// peer exited, connect to new one
			s.peerDelete(address)
			log.Debugf("peer exited: %v", address)
		case <-loopTicker.C:
			log.Infof("peer manager wakeup") // XXX maybe too loud
			go s.pingAllPeers(ctx)
		}
	}
}

func (s *Server) pingAllPeers(ctx context.Context) {
	log.Tracef("pingAllPeers")
	defer log.Tracef("pingAllPeers exit")

	// XXX reason and explain why this cannot be reentrant
	s.mtx.Lock()
	defer s.mtx.Unlock()

	for _, p := range s.peers {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if p.conn == nil {
			continue
		}

		// We don't really care about the response. We just want to
		// write to the connection to make it fail if the other side
		// went away.
		log.Debugf("Pinging: %v", p)
		err := p.write(wire.NewMsgPing(uint64(time.Now().Unix())))
		if err != nil {
			log.Errorf("ping %v: %v", p, err)
		}
	}
}

func (s *Server) peerConnect(ctx context.Context, peerC chan string, p *peer) {
	log.Tracef("peerConnect %v", p)
	defer func() {
		select {
		case peerC <- p.String():
		default:
			log.Tracef("could not signal peer channel: %v", p)
		}
		log.Tracef("peerConnect exit %v", p)
	}()

	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := p.connect(tctx)
	if err != nil {
		go func(pp *peer) {
			// Remove from database; it's ok to be aggressive if it
			// failed with no route to host or failed with i/o
			// timeout or invalid network (ipv4/ipv6).
			//
			// This does have the side-effect of draining the peer
			// table during network outages but that is ok. The
			// peers table will be rebuild based on DNS seeds.
			host, port, err := net.SplitHostPort(pp.String())
			if err != nil {
				log.Errorf("split host port: %v", err)
				return
			}
			err = s.db.PeerDelete(ctx, host, port)
			if err != nil {
				log.Debugf("peer delete (%v): %v", pp, err)
			} else {
				log.Debugf("Peer delete: %v", pp)
			}
		}(p)
		log.Debugf("connect: %v", err)
		return
	}
	defer func() {
		err := p.close()
		if err != nil {
			log.Errorf("peer disconnect: %v %v", p, err)
		}
	}()

	_ = p.write(wire.NewMsgSendHeaders()) // Ask peer to send headers
	_ = p.write(wire.NewMsgGetAddr())     // Try to get network information

	log.Infof("Peer connected: %v", p)

	// Pretend we are always in IBD.
	//
	// This obviously will put a pressure on the internet connection and
	// database because each and every peer is racing at start of day.  As
	// multiple answers come in the insert of the headers fails or
	// succeeds. If it fails no more headers will be requested from that
	// peer.
	bhs, err := s.blockHeadersBest(ctx)
	if err != nil {
		log.Errorf("block headers best: %v", err)
	}
	if len(bhs) != 1 {
		// XXX fix multiple tips
		panic(len(bhs))
	}

	err = s.getHeaders(ctx, p, bhs[0].Header)
	if err != nil {
		// This should not happen
		log.Errorf("get headers: %v", err)
		return
	}

	// XXX kickstart block download, should happen in getHeaders

	verbose := false
	for {
		// See if we were interrupted, for the love of pete add ctx to wire
		select {
		case <-ctx.Done():
			return
		default:
		}

		msg, err := p.read()
		if err == wire.ErrUnknownMessage {
			// skip unknown
			continue
		} else if err != nil {
			// reevaluate pending blocks cache
			cacheUsed := s.blockPeerExpire()
			log.Errorf("read (%v): %v -- pending blocks %v", p, err, cacheUsed)
			return
		}

		if verbose {
			spew.Dump(msg)
		}

		// XXX send wire message to pool reader
		switch m := msg.(type) {
		case *wire.MsgAddr:
			go s.handleAddr(ctx, p, m)

		case *wire.MsgAddrV2:
			go s.handleAddrV2(ctx, p, m)

		case *wire.MsgBlock:
			go s.handleBlock(ctx, p, m)

		case *wire.MsgFeeFilter:
			// XXX shut up

		case *wire.MsgInv:
			go s.handleInv(ctx, p, m)

		case *wire.MsgHeaders:
			go s.handleHeaders(ctx, p, m)

		case *wire.MsgPing:
			go s.handlePing(ctx, p, m)
		default:
			log.Tracef("unhandled message type %v: %T\n", p, msg)
		}
	}
}

func (s *Server) running() bool {
	s.mtx.RLock()
	defer s.mtx.RUnlock()
	return s.isRunning
}

func (s *Server) testAndSetRunning(b bool) bool {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	old := s.isRunning
	s.isRunning = b
	return old != s.isRunning
}

func (s *Server) promRunning() float64 {
	r := s.running()
	if r {
		return 1
	}
	return 0
}

func (s *Server) handleAddr(ctx context.Context, p *peer, msg *wire.MsgAddr) {
	log.Tracef("handleAddr (%v): %v", p, len(msg.AddrList))
	defer log.Tracef("handleAddr exit (%v)", p)

	peers := make([]tbcd.Peer, 0, len(msg.AddrList))
	for k := range msg.AddrList {
		peers = append(peers, tbcd.Peer{
			Host: msg.AddrList[k].IP.String(),
			Port: strconv.Itoa(int(msg.AddrList[k].Port)),
		})
	}
	err := s.db.PeersInsert(ctx, peers)
	// Don't log insert 0, its a dup.
	if err != nil && !database.ErrZeroRows.Is(err) {
		log.Errorf("%v", err)
	}
}

func (s *Server) handleAddrV2(ctx context.Context, p *peer, msg *wire.MsgAddrV2) {
	log.Tracef("handleAddrV2 (%v): %v", p, len(msg.AddrList))
	defer log.Tracef("handleAddrV2 exit (%v)", p)

	peers := make([]tbcd.Peer, 0, len(msg.AddrList))
	for k := range msg.AddrList {
		peers = append(peers, tbcd.Peer{
			Host: msg.AddrList[k].Addr.String(),
			Port: strconv.Itoa(int(msg.AddrList[k].Port)),
		})
	}
	err := s.db.PeersInsert(ctx, peers)
	// Don't log insert 0, its a dup.
	if err != nil && !database.ErrZeroRows.Is(err) {
		log.Errorf("%v", err)
	}
}

func (s *Server) handlePing(ctx context.Context, p *peer, msg *wire.MsgPing) {
	log.Tracef("handlePing %v", p.address)
	defer log.Tracef("handlePing exit %v", p.address)

	pong := wire.NewMsgPong(msg.Nonce)
	err := p.write(pong)
	if err != nil {
		log.Errorf("could not write pong message %v: %v", p.address, err)
		return
	}
	log.Tracef("handlePing %v: pong %v", p.address, pong.Nonce)
}

func (s *Server) handleInv(ctx context.Context, p *peer, msg *wire.MsgInv) {
	log.Tracef("handleInv (%v)", p)
	defer log.Tracef("handleInv exit (%v)", p)

	var bis []tbcd.BlockIdentifier
	for k := range msg.InvList {
		switch msg.InvList[k].Type {
		case wire.InvTypeBlock:

			// XXX height is missing here, looks right but assert
			// that this isn't broken.
			log.Infof("handleInv: block %v", msg.InvList[k].Hash)

			bis = append(bis, tbcd.BlockIdentifier{
				Hash: msg.InvList[k].Hash[:], // fake out
			})
			log.Infof("handleInv: block %v", msg.InvList[k].Hash)
		case wire.InvTypeTx:
			// XXX silence for now
		default:
			log.Infof("handleInv: skipping inv type %v", msg.InvList[k].Type)
		}
	}

	if len(bis) > 0 {
		err := s.downloadBlocks(ctx, bis)
		if err != nil {
			log.Errorf("download blocks: %v", err)
			return
		}
	}
}

// XXX see how we send in peer, that is not what we want
func (s *Server) handleHeaders(ctx context.Context, p *peer, msg *wire.MsgHeaders) {
	log.Tracef("handleHeaders %v", p)
	defer log.Tracef("handleHeaders exit %v", p)

	log.Debugf("handleHeaders (%v): %v", p, len(msg.Headers))

	if len(msg.Headers) == 0 {
		// This signifies the end of IBD
		s.checkBlockCache(ctx)
		return
	}

	// XXX do some nominal height check and reject blocks we have seen.
	// Maybe move host to peersBad as well.

	// Make sure we can connect these headers in database
	dbpbh, err := s.db.BlockHeaderByHash(ctx, msg.Headers[0].PrevBlock[:])
	if err != nil {
		log.Errorf("handle headers no previous block header: %v",
			msg.Headers[0].BlockHash())
		return
	}
	pbh, err := bytes2Header(dbpbh.Header)
	if err != nil {
		log.Errorf("invalid block header: %v", err)
		return
	}

	// Construct insert list and nominally validate headers
	headers := make([]tbcd.BlockHeader, 0, len(msg.Headers))
	height := dbpbh.Height + 1
	for k := range msg.Headers {
		if !hashEqual(msg.Headers[k].PrevBlock, pbh.BlockHash()) {
			log.Errorf("cannot connect %v at height %v",
				msg.Headers[k].PrevBlock, height)
			return
		}

		headers = append(headers, tbcd.BlockHeader{
			Hash:   sliceChainHash(msg.Headers[k].BlockHash()),
			Height: height,
			Header: h2b(msg.Headers[k]),
		})

		pbh = msg.Headers[k]
		height++
	}

	if len(headers) > 0 {
		err := s.db.BlockHeadersInsert(ctx, headers)
		if err != nil {
			// This ends the race between peers during IBD.
			if !database.ErrDuplicate.Is(err) {
				log.Errorf("block headers insert: %v", err)
			}
			//log.Errorf("block headers insert: %v", err)
			//log.Infof("%v", spew.Sdump(headers))
			log.Errorf("block headers insert peer not synced: %v", p.close())
			return
		}

		lbh := headers[len(headers)-1]
		log.Infof("Inserted %v block headers height %v",
			len(headers), lbh.Height)

		// Ask for next batch of headers
		err = s.getHeaders(ctx, p, lbh.Header)
		if err != nil {
			log.Errorf("get headers: %v", err)
			return
		}
	}
}

func (s *Server) handleBlock(ctx context.Context, p *peer, msg *wire.MsgBlock) {
	log.Tracef("handleBlock (%v)", p)
	defer log.Tracef("handleBlock exit (%v)", p)

	block := btcutil.NewBlock(msg)
	//err := msg.Serialize(block) // XXX we should not being doing this twice
	//if err != nil {
	//	log.Errorf("block serialize: %v", err)
	//	return
	//}

	// bh := msg.Header.BlockHash()
	// bhs := bh.String()
	bhs := block.Hash().String()
	bb, err := block.Bytes()
	if err != nil {
		log.Errorf("block bytes %v: %v", block.Hash(), err)
		return
	}
	b := &tbcd.Block{
		Hash:  sliceChainHash(*block.Hash()),
		Block: bb,
	}

	if s.cfg.BlockSanity {
		err = blockchain.CheckBlockSanity(block, s.chainParams.PowLimit,
			s.timeSource)
		if err != nil {
			log.Errorf("Unable to validate block hash %v: %v", bhs, err)
			return
		}

		// Contextual check of block
		//
		// We do want these checks however we download the blockchain
		// out of order this we will have to do something clever for
		// prevNode.
		//
		//header := &block.MsgBlock().Header
		//flags := blockchain.BFNone
		//err := blockchain.CheckBlockHeaderContext(header, prevNode, flags, bctxt, false)
		//if err != nil {
		//	log.Errorf("Unable to validate context of block hash %v: %v", bhs, err)
		//	return
		//}
	}

	height, err := s.db.BlockInsert(ctx, b)
	if err != nil {
		// XXX ignore duplicate error printing since we will hit that
		log.Errorf("block insert %v: %v", bhs, err)
	} else {
		log.Infof("Insert block %v at %v txs %v %v", bhs, height,
			len(msg.Transactions), msg.Header.Timestamp)
	}

	// Whatever happens,, delete from cache and potentially try again
	var (
		printStats      bool
		blocksSize      uint64
		blocksInserted  int
		blocksDuplicate int // keep track of this until have less of them
		delta           time.Duration

		// blocks pending
		blocksPending int

		// peers
		goodPeers      int
		badPeers       int
		activePeers    int
		connectedPeers int
	)
	s.mtx.Lock()
	delete(s.blocks, bhs) // remove inserted block

	// Stats
	if err == nil {
		s.blocksSize += uint64(len(b.Block) + len(b.Hash))
		if _, ok := s.blocksInserted[bhs]; ok {
			s.blocksDuplicate++
		} else {
			s.blocksInserted[bhs] = struct{}{}
		}
	}
	now := time.Now()
	if now.After(s.printTime) {
		printStats = true

		blocksSize = s.blocksSize
		blocksInserted = len(s.blocksInserted)
		blocksDuplicate = s.blocksDuplicate
		// This is super awkward but prevents calculating N inserts *
		// time.Before(10*time.Second).
		delta = now.Sub(s.printTime.Add(-10 * time.Second))

		s.blocksSize = 0
		s.blocksInserted = make(map[string]struct{}, 8192)
		s.blocksDuplicate = 0
		s.printTime = now.Add(10 * time.Second)

		// Grab pending block cache stats
		blocksPending = len(s.blocks)

		// Grab some peer stats as well
		activePeers = len(s.peers)
		goodPeers, badPeers = s.db.PeersStats(ctx)
		// Gonna take it right into the Danger Zone! (double mutex)
		for _, peer := range s.peers {
			if peer.isConnected() {
				connectedPeers++
			}
		}
	}
	s.mtx.Unlock()

	if printStats {
		// XXX this coun't errors somehow after ibd, probably because
		// duplicate blocks are downloaded when an inv comes in.
		log.Infof("Inserted %v blocks (%v, %v duplicates) in the last %v",
			blocksInserted, humanize.Bytes(blocksSize), blocksDuplicate, delta)
		log.Infof("Pending blocks %v/%v active peers %v connected peers %v "+
			"good peers %v bad peers %v",
			blocksPending, defaultPendingBlocks, activePeers, connectedPeers,
			goodPeers, badPeers)
	}

	s.checkBlockCache(ctx)
}

func (s *Server) checkBlockCache(ctx context.Context) {
	log.Tracef("checkBlockCache")
	defer log.Tracef("checkBlockCache exit")

	// Deal with expired block downloads
	used := s.blockPeerExpire()
	want := defaultPendingBlocks - used
	if want <= 0 {
		return
	}

	//log.Infof("checkBlockCache inside mutex")
	//defer log.Infof("checkBlockCache outside mutex")
	//// XXX make better reentrant
	//s.mtx.Lock()
	//if s.isWorking {
	//	s.mtx.Unlock()
	//	return
	//}
	//s.isWorking = true
	//s.mtx.Unlock()
	//defer func() {
	//	s.mtx.Lock()
	//	s.isWorking = false
	//	s.mtx.Unlock()
	//}()

	// XXX is this too much lock?
	s.mtx.Lock()
	defer s.mtx.Unlock()

	bm, err := s.db.BlocksMissing(ctx, want)
	if err != nil {
		log.Errorf("block headers missing: %v", err)
		return
	}
	// log.Infof("checkBlockCache db")

	// XXX prune list if outstanding but there are too many mutexes happening here
	prunedBM := make([]tbcd.BlockIdentifier, 0, len(bm))
	for k := range bm {
		if _, ok := s.blocks[string(bm[k].Hash)]; ok {
			continue
		}
		prunedBM = append(prunedBM, bm[k])
	}

	if len(prunedBM) != len(bm) {
		log.Infof("PRUNE %v BM %v", len(prunedBM), len(bm))
	}
	if len(prunedBM) == 0 {
		log.Infof("everything pending %v", spew.Sdump(s.blocks))
	}

	// downdloadBlocks will only insert unseen in the cache
	// log.Infof("checkBlockCache download")
	err = s.downloadBlocks(ctx, prunedBM)
	if err != nil {
		log.Errorf("download blocks: %v", err)
		return
	}
	// log.Infof("checkBlockCache AFTER download")
}

var genesisBlockHeader *tbcd.BlockHeader // XXX

func (s *Server) insertGenesis(ctx context.Context) error {
	log.Tracef("insertGenesis")
	defer log.Tracef("insertGenesis exit")

	// It's ok to take an expensive mutex here because this only happens at
	// start-of-day. This mutex is to prevent excessive logging, which in
	// itself is harmless, but may throw people looking at logs of.
	s.mtx.Lock()
	if s.insertedGenesis {
		return nil
	}
	s.insertedGenesis = true
	defer s.mtx.Unlock()

	// We really should be inserting the block first but block insert
	// verifies that a block heade exists.
	log.Infof("Inserting genesis block header hash: %v",
		s.chainParams.GenesisHash)
	gbh, err := header2Bytes(&s.chainParams.GenesisBlock.Header)
	if err != nil {
		return fmt.Errorf("serialize genesis block header: %v", err)
	}

	genesisBlockHeader = &tbcd.BlockHeader{
		Height: 0,
		Hash:   s.chainParams.GenesisHash[:],
		Header: gbh,
	}
	err = s.db.BlockHeadersInsert(ctx, []tbcd.BlockHeader{*genesisBlockHeader})
	if err != nil {
		return fmt.Errorf("genesis block header insert: %v", err)
	}

	log.Infof("Inserting genesis block")
	gb, err := btcutil.NewBlock(s.chainParams.GenesisBlock).Bytes()
	if err != nil {
		return fmt.Errorf("genesis block encode: %v", err)
	}
	_, err = s.db.BlockInsert(ctx, &tbcd.Block{
		Hash:  s.chainParams.GenesisHash[:],
		Block: gb,
	})
	if err != nil {
		return fmt.Errorf("genesis block insert: %v", err)
	}

	return nil
}

func (s *Server) blockHeadersBest(ctx context.Context) ([]tbcd.BlockHeader, error) {
	log.Tracef("blockHeadersBest")
	defer log.Tracef("blockHeadersBest exit")

	// Find out where IBD is at
	bhs, err := s.db.BlockHeadersBest(ctx)
	if err != nil {
		return nil, fmt.Errorf("block headers best: %v", err)
	}

	// No entries means we are at genesis
	// XXX this can hit several times at start of day. Figure out if we
	// want to insert genesis earlier to prevent this error.
	if len(bhs) == 0 {
		err := s.insertGenesis(ctx)
		if err != nil {
			return nil, fmt.Errorf("insert genesis: %v", err)
		}
		bhs = append(bhs, *genesisBlockHeader)
	}

	if len(bhs) != 1 {
		// XXX this needs to be handled, for now try to just unfork
		return nil, fmt.Errorf("unhandled best tip count: %v", spew.Sdump(bhs))
	}

	return bhs, nil
}

func (s *Server) downloadBlocks(ctx context.Context, bis []tbcd.BlockIdentifier) error {
	log.Tracef("downloadBlocks")
	defer log.Tracef("downloadBlocks exit")

	for k := range bis {
		bi := bis[k]
		hash, _ := chainhash.NewHash(bi.Hash[:])
		hashS := hash.String()
		getData := wire.NewMsgGetData()
		getData.InvList = append(getData.InvList,
			&wire.InvVect{
				Type: wire.InvTypeBlock,
				Hash: *hash,
			})
		err := s.randPeerWrite(ctx, hashS, getData)
		switch err {
		case nil:
			log.Debugf("downloadBlocks %v", hashS)
			continue
		case errCacheFull:
			log.Tracef("cache full")
			break
		case errNoPeers:
			log.Tracef("could not write, no peers")
			break
		default:
			log.Errorf("write error: %v", err)
		}
	}

	return nil
}

func (s *Server) BlockHeaderByHash(ctx context.Context, hash *chainhash.Hash) (*wire.BlockHeader, uint64, error) {
	log.Tracef("BlockHeaderByHash")
	defer log.Tracef("BlockHeaderByHash exit")

	bh, err := s.db.BlockHeaderByHash(ctx, hash[:])
	if err != nil {
		return nil, 0, fmt.Errorf("db block header by hash: %w", err)
	}
	bhw, err := bytes2Header(bh.Header)
	if err != nil {
		return nil, 0, fmt.Errorf("bytes to header: %w", err)
	}
	return bhw, bh.Height, nil
}

func (s *Server) BlockHeadersByHeight(ctx context.Context, height uint64) ([]wire.BlockHeader, error) {
	log.Tracef("BlockHeadersByHeight")
	defer log.Tracef("BlockHeadersByHeight exit")

	bhs, err := s.db.BlockHeadersByHeight(ctx, height)
	if err != nil {
		return nil, fmt.Errorf("db block header by height: %w", err)
	}
	bhsw := make([]wire.BlockHeader, 0, len(bhs))
	for k := range bhs {
		bhw, err := bytes2Header(bhs[k].Header)
		if err != nil {
			return nil, fmt.Errorf("bytes to header: %w", err)
		}
		bhsw = append(bhsw, *bhw)
	}
	return bhsw, nil
}

func (s *Server) Run(pctx context.Context) error {
	log.Tracef("Run")
	defer log.Tracef("Run exit")

	if !s.testAndSetRunning(true) {
		return fmt.Errorf("tbc already running")
	}
	defer s.testAndSetRunning(false)

	ctx, cancel := context.WithCancel(pctx)
	defer cancel()

	// This should have been verified but let's not make assumptions.
	switch s.cfg.Network {
	case "testnet3":
	case "mainnet":
	default:
		return fmt.Errorf("unsuported network: %v", s.cfg.Network)
	}

	// Open db.
	var err error
	s.db, err = level.New(ctx, filepath.Join(s.cfg.LevelDBHome, s.cfg.Network))
	if err != nil {
		return fmt.Errorf("Failed to open level database: %v", err)
	}
	defer s.db.Close()

	// Prometheus
	if s.cfg.PrometheusListenAddress != "" {
		d, err := deucalion.New(&deucalion.Config{
			ListenAddress: s.cfg.PrometheusListenAddress,
		})
		if err != nil {
			return fmt.Errorf("failed to create server: %w", err)
		}
		cs := []prometheus.Collector{
			prometheus.NewGaugeFunc(prometheus.GaugeOpts{
				Subsystem: promSubsystem,
				Name:      "running",
				Help:      "Is tbc service running.",
			}, s.promRunning),
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if err := d.Run(ctx, cs); err != context.Canceled {
				log.Errorf("prometheus terminated with error: %v", err)
				return
			}
			log.Infof("prometheus clean shutdown")
		}()
	}

	errC := make(chan error)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		err := s.peerManager(ctx)
		if err != nil {
			select {
			case errC <- err:
			default:
			}
		}
	}()

	select {
	case <-ctx.Done():
		err = ctx.Err()
	case e := <-errC:
		err = e
	}
	cancel()

	log.Infof("tbc service shutting down")
	s.wg.Wait()
	log.Infof("tbc service clean shutdown")

	return err
}
