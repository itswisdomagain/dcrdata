// Copyright (c) 2018, The Decred developers
// Copyright (c) 2017, The dcrdata developers
// See LICENSE for details.

package dcrpg

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/chappjc/trylock"
	"github.com/decred/dcrd/blockchain/stake"
	"github.com/decred/dcrd/chaincfg"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/dcrutil"
	"github.com/decred/dcrd/rpcclient"
	"github.com/decred/dcrd/wire"
	apitypes "github.com/decred/dcrdata/v3/api/types"
	"github.com/decred/dcrdata/v3/blockdata"
	"github.com/decred/dcrdata/v3/db/dbtypes"
	"github.com/decred/dcrdata/v3/db/dcrpg/internal"
	"github.com/decred/dcrdata/v3/explorer"
	"github.com/decred/dcrdata/v3/rpcutils"
	"github.com/decred/dcrdata/v3/stakedb"
	humanize "github.com/dustin/go-humanize"
)

var (
	zeroHash            = chainhash.Hash{}
	zeroHashStringBytes = []byte(chainhash.Hash{}.String())
)

// DevFundBalance is a block-stamped wrapper for explorer.AddressBalance. It is
// intended to be used for the project address.
type DevFundBalance struct {
	sync.RWMutex
	*explorer.AddressBalance
	updating trylock.Mutex
	Height   int64
	Hash     chainhash.Hash
}

// BlockHash is a thread-safe accessor for the block hash.
func (d *DevFundBalance) BlockHash() chainhash.Hash {
	d.RLock()
	defer d.RUnlock()
	return d.Hash
}

// BlockHeight is a thread-safe accessor for the block height.
func (d *DevFundBalance) BlockHeight() int64 {
	d.RLock()
	defer d.RUnlock()
	return d.Height
}

// Balance is a thread-safe accessor for the explorer.AddressBalance.
func (d *DevFundBalance) Balance() *explorer.AddressBalance {
	d.RLock()
	defer d.RUnlock()
	return d.AddressBalance
}

// ticketPoolDataCache stores the most recent ticketpool graphs information
// fetched to minimize the possibility of making multiple queries to the db
// fetching the same information.
type ticketPoolDataCache struct {
	sync.RWMutex
	Height map[dbtypes.ChartGrouping]uint64
	// BarGraphsCache persists data for the Ticket purchase distribution chart
	// and Ticket Price Distribution chart
	BarGraphsCache map[dbtypes.ChartGrouping][]*dbtypes.PoolTicketsData
	// DonutGraphCache persist data for the Number of tickets outputs pie chart.
	DonutGraphCache map[dbtypes.ChartGrouping]*dbtypes.PoolTicketsData
}

// ticketPoolGraphsCache persists the latest ticketpool data queried from the db.
var ticketPoolGraphsCache = &ticketPoolDataCache{
	Height:          make(map[dbtypes.ChartGrouping]uint64),
	BarGraphsCache:  make(map[dbtypes.ChartGrouping][]*dbtypes.PoolTicketsData),
	DonutGraphCache: make(map[dbtypes.ChartGrouping]*dbtypes.PoolTicketsData),
}

// TicketPoolData is a thread-safe way to access the ticketpool graphs data
// stored in the cache.
func TicketPoolData(interval dbtypes.ChartGrouping, height uint64) (barGraphs []*dbtypes.PoolTicketsData,
	donutChart *dbtypes.PoolTicketsData, actualHeight uint64, intervalFound, isStale bool) {
	ticketPoolGraphsCache.RLock()
	defer ticketPoolGraphsCache.RUnlock()

	var found bool
	barGraphs, intervalFound = ticketPoolGraphsCache.BarGraphsCache[interval]
	donutChart, found = ticketPoolGraphsCache.DonutGraphCache[interval]
	intervalFound = intervalFound && found

	actualHeight = ticketPoolGraphsCache.Height[interval]
	isStale = ticketPoolGraphsCache.Height[interval] != height

	return
}

// UpdateTicketPoolData updates the ticket pool cache with the latest data fetched.
// This is a thread-safe way to update ticket pool cache data. TryLock helps avoid
// stacking calls to update the cache.
func UpdateTicketPoolData(interval dbtypes.ChartGrouping, barGraphs []*dbtypes.PoolTicketsData,
	donutcharts *dbtypes.PoolTicketsData, height uint64) {
	ticketPoolGraphsCache.Lock()
	defer ticketPoolGraphsCache.Unlock()

	ticketPoolGraphsCache.Height[interval] = height
	ticketPoolGraphsCache.BarGraphsCache[interval] = barGraphs
	ticketPoolGraphsCache.DonutGraphCache[interval] = donutcharts
}

// ChainDB provides an interface for storing and manipulating extracted
// blockchain data in a PostgreSQL database.
type ChainDB struct {
	db                 *sql.DB
	chainParams        *chaincfg.Params
	devAddress         string
	dupChecks          bool
	bestBlock          int64
	bestBlockHash      string
	lastBlock          map[chainhash.Hash]uint64
	addressCounts      *addressCounter
	stakeDB            *stakedb.StakeDatabase
	unspentTicketCache *TicketTxnIDGetter
	DevFundBalance     *DevFundBalance
	devPrefetch        bool
	InBatchSync        bool
	InReorg            bool
	tpUpdatePermission map[dbtypes.ChartGrouping]*trylock.Mutex
}

// ChainDBRPC provides an interface for storing and manipulating extracted and
// includes the RPC Client blockchain data in a PostgreSQL database.
type ChainDBRPC struct {
	*ChainDB
	Client *rpcclient.Client
}

// NewChainDBRPC contains ChainDB and RPC client parameters. By default,
// duplicate row checks on insertion are enabled. also enables rpc client
func NewChainDBRPC(chaindb *ChainDB, cl *rpcclient.Client) (*ChainDBRPC, error) {
	return &ChainDBRPC{chaindb, cl}, nil
}

// SyncChainDBAsync calls (*ChainDB).SyncChainDBAsync after a nil pointer check
// on the ChainDBRPC receiver.
func (db *ChainDBRPC) SyncChainDBAsync(res chan dbtypes.SyncResult,
	client rpcutils.MasterBlockGetter, quit chan struct{}, updateAllAddresses,
	updateAllVotes, newIndexes bool, updateExplorer chan *chainhash.Hash,
	barLoad chan *dbtypes.ProgressBarLoad) {
	// Allowing db to be nil simplifies logic in caller.
	if db == nil {
		res <- dbtypes.SyncResult{
			Height: -1,
			Error:  fmt.Errorf("ChainDB (psql) disabled"),
		}
		return
	}
	db.ChainDB.SyncChainDBAsync(res, client, quit, updateAllAddresses,
		updateAllVotes, newIndexes, updateExplorer, barLoad)
}

// Store satisfies BlockDataSaver. Blocks stored this way are considered valid
// and part of mainchain. This calls (*ChainDB).Store after a nil pointer check
// on the ChainDBRPC receiver
func (db *ChainDBRPC) Store(blockData *blockdata.BlockData, msgBlock *wire.MsgBlock) error {
	// Allowing db to be nil simplifies logic in caller.
	if db == nil {
		return nil
	}
	return db.ChainDB.Store(blockData, msgBlock)
}

// MissingSideChainBlocks identifies side chain blocks that are missing from the
// DB. Side chains known to dcrd are listed via the getchaintips RPC. Each block
// presence in the postgres DB is checked, and any missing block is returned in
// a SideChain along with a count of the total number of missing blocks.
func (db *ChainDBRPC) MissingSideChainBlocks() ([]dbtypes.SideChain, int, error) {
	// First get the side chain tips (head blocks).
	tips, err := rpcutils.SideChains(db.Client)
	if err != nil {
		return nil, 0, fmt.Errorf("unable to get chain tips from node: %v", err)
	}
	nSideChains := len(tips)

	// Build a list of all the blocks in each side chain that are not
	// already in the database.
	blocksToStore := make([]dbtypes.SideChain, nSideChains)
	var nSideChainBlocks int
	for it := range tips {
		sideHeight := tips[it].Height
		log.Debugf("Getting full side chain with tip %s at %d.", tips[it].Hash, sideHeight)

		sideChain, err := rpcutils.SideChainFull(db.Client, tips[it].Hash)
		if err != nil {
			return nil, 0, fmt.Errorf("unable to get side chain blocks for chain tip %s: %v",
				tips[it].Hash, err)
		}

		// For each block in the side chain, check if it already stored.
		for is := range sideChain {
			// Check for the block hash in the DB.
			sideHeightDB, err := db.BlockHeight(sideChain[is])
			if err == sql.ErrNoRows {
				// This block is NOT already in the DB.
				blocksToStore[it].Hashes = append(blocksToStore[it].Hashes, sideChain[is])
				blocksToStore[it].Heights = append(blocksToStore[it].Heights, sideHeight)
				nSideChainBlocks++
			} else if err == nil {
				// This block is already in the DB.
				log.Tracef("Found block %s in postgres at height %d.",
					sideChain[is], sideHeightDB)
				if sideHeight != sideHeightDB {
					log.Errorf("Side chain block height %d, expected %d.",
						sideHeightDB, sideHeight)
				}
			} else /* err != nil && err != sql.ErrNoRows */ {
				// Unexpected error
				log.Debugf("Failed to retrieve block %s.", sideChain[is])
			}

			// Next block
			sideHeight--
		}
	}

	return blocksToStore, nSideChainBlocks, nil
}

// addressCounter provides a cache for address balances.
type addressCounter struct {
	sync.RWMutex
	validHeight int64
	balance     map[string]explorer.AddressBalance
}

func makeAddressCounter() *addressCounter {
	return &addressCounter{
		validHeight: 0,
		balance:     make(map[string]explorer.AddressBalance),
	}
}

// TicketTxnIDGetter provides a cache for DB row IDs of tickets.
type TicketTxnIDGetter struct {
	sync.RWMutex
	idCache map[string]uint64
	db      *sql.DB
}

// TxnDbID fetches DB row ID for the ticket specified by the input transaction
// hash. A cache is checked first. In the event of a cache hit, the DB ID is
// returned and deleted from the internal cache. In the event of a cache miss,
// the database is queried. If the database query fails, the error is non-nil.
func (t *TicketTxnIDGetter) TxnDbID(txid string, expire bool) (uint64, error) {
	if t == nil {
		panic("You're using an uninitialized TicketTxnIDGetter")
	}
	t.RLock()
	dbID, ok := t.idCache[txid]
	t.RUnlock()
	if ok {
		if expire {
			t.Lock()
			delete(t.idCache, txid)
			t.Unlock()
		}
		return dbID, nil
	}
	// Cache miss. Get the row id by hash from the tickets table.
	return RetrieveTicketIDByHash(t.db, txid)
}

// Set stores the (transaction hash, DB row ID) pair a map for future access.
func (t *TicketTxnIDGetter) Set(txid string, txDbID uint64) {
	if t == nil {
		return
	}
	t.Lock()
	defer t.Unlock()
	t.idCache[txid] = txDbID
}

// SetN stores several (transaction hash, DB row ID) pairs in the map.
func (t *TicketTxnIDGetter) SetN(txid []string, txDbID []uint64) {
	if t == nil {
		return
	}
	t.Lock()
	defer t.Unlock()
	for i := range txid {
		t.idCache[txid[i]] = txDbID[i]
	}
}

// NewTicketTxnIDGetter constructs a new TicketTxnIDGetter with an empty cache.
func NewTicketTxnIDGetter(db *sql.DB) *TicketTxnIDGetter {
	return &TicketTxnIDGetter{
		db:      db,
		idCache: make(map[string]uint64),
	}
}

// DBInfo holds the PostgreSQL database connection information.
type DBInfo struct {
	Host, Port, User, Pass, DBName string
}

// NewChainDB constructs a ChainDB for the given connection and Decred network
// parameters. By default, duplicate row checks on insertion are enabled.
func NewChainDB(dbi *DBInfo, params *chaincfg.Params, stakeDB *stakedb.StakeDatabase,
	devPrefetch bool) (*ChainDB, error) {
	// Connect to the PostgreSQL daemon and return the *sql.DB
	db, err := Connect(dbi.Host, dbi.Port, dbi.User, dbi.Pass, dbi.DBName)
	if err != nil {
		return nil, err
	}
	// Attempt to get DB best block height from tables, but if the tables are
	// empty or not yet created, it is not an error.
	bestHeight, bestHash, _, err := RetrieveBestBlockHeight(db)
	if err != nil && !(err == sql.ErrNoRows ||
		strings.HasSuffix(err.Error(), "does not exist")) {
		return nil, err
	}

	// Development subsidy address of the current network
	var devSubsidyAddress string
	if devSubsidyAddress, err = dbtypes.DevSubsidyAddress(params); err != nil {
		log.Warnf("ChainDB.NewChainDB: %v", err)
	}

	if err = setupTables(db); err != nil {
		log.Warnf("ATTENTION! %v", err)
		// TODO: Actually handle the upgrades/reindexing somewhere.
		return nil, err
	}

	log.Infof("Pre-loading unspent ticket info for InsertVote optimization.")
	unspentTicketCache := NewTicketTxnIDGetter(db)
	unspentTicketDbIDs, unspentTicketHashes, err := RetrieveUnspentTickets(db)
	if err != nil && err != sql.ErrNoRows && !strings.HasSuffix(err.Error(), "does not exist") {
		return nil, err
	}
	if len(unspentTicketDbIDs) != 0 {
		log.Infof("Storing data for %d unspent tickets in cache.", len(unspentTicketDbIDs))
		unspentTicketCache.SetN(unspentTicketHashes, unspentTicketDbIDs)
	}

	// For each chart grouping type create a non-blocking updater mutex.
	tpUpdatePermissions := make(map[dbtypes.ChartGrouping]*trylock.Mutex)
	for g := range dbtypes.ChartGroupings {
		tpUpdatePermissions[g] = new(trylock.Mutex)
	}

	return &ChainDB{
		db:                 db,
		chainParams:        params,
		devAddress:         devSubsidyAddress,
		dupChecks:          true,
		bestBlock:          int64(bestHeight),
		bestBlockHash:      bestHash,
		lastBlock:          make(map[chainhash.Hash]uint64),
		addressCounts:      makeAddressCounter(),
		stakeDB:            stakeDB,
		unspentTicketCache: unspentTicketCache,
		DevFundBalance:     new(DevFundBalance),
		devPrefetch:        devPrefetch,
		tpUpdatePermission: tpUpdatePermissions,
	}, nil
}

// Close closes the underlying sql.DB connection to the database.
func (pgb *ChainDB) Close() error {
	return pgb.db.Close()
}

// UseStakeDB is used to assign a stakedb.StakeDatabase for ticket tracking.
// This may be useful when it is necessary to construct a ChainDB prior to
// creating or loading a StakeDatabase, such as when dropping tables.
func (pgb *ChainDB) UseStakeDB(stakeDB *stakedb.StakeDatabase) {
	pgb.stakeDB = stakeDB
}

// EnableDuplicateCheckOnInsert specifies whether SQL insertions should check
// for row conflicts (duplicates), and avoid adding or updating.
func (pgb *ChainDB) EnableDuplicateCheckOnInsert(dupCheck bool) {
	pgb.dupChecks = dupCheck
}

// SetupTables creates the required tables and type, and prints table versions
// stored in the table comments when debug level logging is enabled.
func (pgb *ChainDB) SetupTables() error {
	return setupTables(pgb.db)
}

func setupTables(db *sql.DB) error {
	if err := CreateTypes(db); err != nil {
		return err
	}

	return CreateTables(db)
}

// VersionCheck checks the current version of all known tables and notifies when
// an upgrade is required. If there is no automatic upgrade supported, an error
// is returned when any table is not of the correct version.
// A smart client is passed to implement the supported upgrades if need be.
func (pgb *ChainDB) VersionCheck(client *rpcclient.Client) error {
	vers := TableVersions(pgb.db)
	for tab, ver := range vers {
		log.Debugf("Table %s: v%s", tab, ver)
	}

	if tableUpgrades := TableUpgradesRequired(vers); len(tableUpgrades) > 0 {
		if tableUpgrades[0].UpgradeType == "upgrade" || tableUpgrades[0].UpgradeType == "reindex" {
			// CheckForAuxDBUpgrade makes db upgrades that are currently supported.
			isSuccess, err := pgb.CheckForAuxDBUpgrade(client)
			if err != nil {
				return err
			}
			// Upgrade was successful, no need to proceed.
			if isSuccess {
				return nil
			}
		}

		// ensure all tables have "ok" status
		OK := true
		for _, u := range tableUpgrades {
			if u.UpgradeType != "ok" {
				log.Warnf(u.String())
				OK = false
			}
		}
		if OK {
			log.Debugf("All tables at correct version (%v)", tableUpgrades[0].RequiredVer)
			return nil
		}

		return fmt.Errorf("rebuild of PostgreSQL tables required (drop with rebuilddb2 -D)")
	}
	return nil
}

// DropTables drops (deletes) all of the known dcrdata tables.
func (pgb *ChainDB) DropTables() {
	DropTables(pgb.db)
}

// SideChainBlocks retrieves all known side chain blocks.
func (pgb *ChainDB) SideChainBlocks() ([]*dbtypes.BlockStatus, error) {
	return RetrieveSideChainBlocks(pgb.db)
}

// SideChainTips retrieves the tip/head block for all known side chains.
func (pgb *ChainDB) SideChainTips() ([]*dbtypes.BlockStatus, error) {
	return RetrieveSideChainTips(pgb.db)
}

// DisapprovedBlocks retrieves all blocks disapproved by stakeholder votes.
func (pgb *ChainDB) DisapprovedBlocks() ([]*dbtypes.BlockStatus, error) {
	return RetrieveDisapprovedBlocks(pgb.db)
}

// BlockStatus retrieves the block chain status of the specified block.
func (pgb *ChainDB) BlockStatus(hash string) (dbtypes.BlockStatus, error) {
	return RetrieveBlockStatus(pgb.db, hash)
}

// TransactionBlocks retrieves the blocks in which the specified transaction
// appears, along with the index of the transaction in each of the blocks. The
// next and previous block hashes are NOT SET in each BlockStatus.
func (pgb *ChainDB) TransactionBlocks(txHash string) ([]*dbtypes.BlockStatus, []uint32, error) {
	hashes, heights, inds, valids, mainchains, err := RetrieveTxnsBlocks(pgb.db, txHash)
	if err != nil {
		return nil, nil, err
	}

	blocks := make([]*dbtypes.BlockStatus, len(hashes))

	for i := range hashes {
		blocks[i] = &dbtypes.BlockStatus{
			IsValid:     valids[i],
			IsMainchain: mainchains[i],
			Height:      heights[i],
			Hash:        hashes[i],
			// Next and previous hash not set
		}
	}

	return blocks, inds, nil
}

// HeightDB queries the DB for the best block height.
func (pgb *ChainDB) HeightDB() (uint64, error) {
	bestHeight, _, _, err := RetrieveBestBlockHeight(pgb.db)
	return bestHeight, err
}

// HashDB queries the DB for the best block's hash.
func (pgb *ChainDB) HashDB() (string, error) {
	_, bestHash, _, err := RetrieveBestBlockHeight(pgb.db)
	return bestHash, err
}

// Height uses the last stored height.
func (pgb *ChainDB) Height() uint64 {
	return uint64(pgb.bestBlock)
}

// HashStr uses the last stored block hash.
func (pgb *ChainDB) HashStr() string {
	return pgb.bestBlockHash
}

// Hash uses the last stored block hash.
func (pgb *ChainDB) Hash() *chainhash.Hash {
	// Caller should check hash instead of error
	hash, _ := chainhash.NewHashFromStr(pgb.bestBlockHash)
	return hash
}

// BlockHeight queries the DB for the height of the specified hash.
func (pgb *ChainDB) BlockHeight(hash string) (int64, error) {
	return RetrieveBlockHeight(pgb.db, hash)
}

// BlockHash queries the DB for the hash of the mainchain block at the given
// height.
func (pgb *ChainDB) BlockHash(height int64) (string, error) {
	return RetrieveBlockHash(pgb.db, height)
}

// VotesInBlock returns the number of votes mined in the block with the
// specified hash.
func (pgb *ChainDB) VotesInBlock(hash string) (int16, error) {
	voters, err := RetrieveBlockVoteCount(pgb.db, hash)
	if err != nil {
		log.Errorf("Unable to get block voter count for hash %s: %v", hash, err)
		return -1, err
	}
	return voters, nil
}

// SpendingTransactions retrieves all transactions spending outpoints from the
// specified funding transaction. The spending transaction hashes, the spending
// tx input indexes, and the corresponding funding tx output indexes, and an
// error value are returned.
func (pgb *ChainDB) SpendingTransactions(fundingTxID string) ([]string, []uint32, []uint32, error) {
	_, spendingTxns, vinInds, voutInds, err := RetrieveSpendingTxsByFundingTx(pgb.db, fundingTxID)
	return spendingTxns, vinInds, voutInds, err
}

// SpendingTransaction returns the transaction that spends the specified
// transaction outpoint, if it is spent. The spending transaction hash, input
// index, tx tree, and an error value are returned.
func (pgb *ChainDB) SpendingTransaction(fundingTxID string,
	fundingTxVout uint32) (string, uint32, int8, error) {
	_, spendingTx, vinInd, tree, err := RetrieveSpendingTxByTxOut(pgb.db, fundingTxID, fundingTxVout)
	return spendingTx, vinInd, tree, err
}

// BlockTransactions retrieves all transactions in the specified block, their
// indexes in the block, their tree, and an error value.
func (pgb *ChainDB) BlockTransactions(blockHash string) ([]string, []uint32, []int8, error) {
	_, blockTransactions, blockInds, trees, _, err := RetrieveTxsByBlockHash(pgb.db, blockHash)
	return blockTransactions, blockInds, trees, err
}

// Transaction retrieves all rows from the transactions table for the given
// transaction hash.
func (pgb *ChainDB) Transaction(txHash string) ([]*dbtypes.Tx, error) {
	_, dbTxs, err := RetrieveDbTxsByHash(pgb.db, txHash)
	return dbTxs, err
}

// BlockMissedVotes retrieves the ticket IDs for all missed votes in the
// specified block, and an error value.
func (pgb *ChainDB) BlockMissedVotes(blockHash string) ([]string, error) {
	return RetrieveMissedVotesInBlock(pgb.db, blockHash)
}

// PoolStatusForTicket retrieves the specified ticket's spend status and ticket
// pool status, and an error value.
func (pgb *ChainDB) PoolStatusForTicket(txid string) (dbtypes.TicketSpendType, dbtypes.TicketPoolStatus, error) {
	_, spendType, poolStatus, err := RetrieveTicketStatusByHash(pgb.db, txid)
	return spendType, poolStatus, err
}

// VoutValue retrieves the value of the specified transaction outpoint in atoms.
func (pgb *ChainDB) VoutValue(txID string, vout uint32) (uint64, error) {
	// txDbID, _, _, err := RetrieveTxByHash(pgb.db, txID)
	// if err != nil {
	// 	return 0, fmt.Errorf("RetrieveTxByHash: %v", err)
	// }
	voutValue, err := RetrieveVoutValue(pgb.db, txID, vout)
	if err != nil {
		return 0, fmt.Errorf("RetrieveVoutValue: %v", err)
	}
	return voutValue, nil
}

// VoutValues retrieves the values of each outpoint of the specified
// transaction. The corresponding indexes in the block and tx trees of the
// outpoints, and an error value are also returned.
func (pgb *ChainDB) VoutValues(txID string) ([]uint64, []uint32, []int8, error) {
	// txDbID, _, _, err := RetrieveTxByHash(pgb.db, txID)
	// if err != nil {
	// 	return nil, fmt.Errorf("RetrieveTxByHash: %v", err)
	// }
	voutValues, txInds, txTrees, err := RetrieveVoutValues(pgb.db, txID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("RetrieveVoutValues: %v", err)
	}
	return voutValues, txInds, txTrees, nil
}

// TransactionBlock retrieves the hash of the block containing the specified
// transaction. The index of the transaction within the block, the transaction
// index, and an error value are also returned.
func (pgb *ChainDB) TransactionBlock(txID string) (string, uint32, int8, error) {
	_, blockHash, blockInd, tree, err := RetrieveTxByHash(pgb.db, txID)
	return blockHash, blockInd, tree, err
}

// AgendaVotes fetches the data used to plot a graph of votes cast per day per
// choice for the provided agenda.
func (pgb *ChainDB) AgendaVotes(agendaID string, chartType int) (*dbtypes.AgendaVoteChoices, error) {
	return retrieveAgendaVoteChoices(pgb.db, agendaID, chartType)
}

// GetOldestTxBlockTime returns the block time of the oldest transaction made in
// relation to the provided address. This helps provide more meaningful graphs
// with the addresses history plotted.
func (pgb *ChainDB) GetOldestTxBlockTime(addr string) (int64, error) {
	return retrieveOldestTxBlockTime(pgb.db, addr)
}

// AddressTransactions retrieves a slice of *dbtypes.AddressRow for a given
// address and transaction type (i.e. all, credit, or debit) from the DB. Only
// the first N transactions starting from the offset element in the set of all
// txnType transactions.
func (pgb *ChainDB) AddressTransactions(address string, N, offset int64,
	txnType dbtypes.AddrTxnType) (addressRows []*dbtypes.AddressRow, err error) {
	var addrFunc func(*sql.DB, string, int64, int64) ([]uint64, []*dbtypes.AddressRow, error)
	switch txnType {
	case dbtypes.AddrTxnCredit:
		addrFunc = RetrieveAddressCreditTxns
	case dbtypes.AddrTxnAll:
		addrFunc = RetrieveAddressTxns
	case dbtypes.AddrTxnDebit:
		addrFunc = RetrieveAddressDebitTxns

	case dbtypes.AddrMergedTxnDebit:
		addrFunc = RetrieveAddressMergedDebitTxns
	default:
		return nil, fmt.Errorf("unknown AddrTxnType %v", txnType)
	}

	_, addressRows, err = addrFunc(pgb.db, address, N, offset)
	return
}

// AddressHistoryAll queries the database for all rows of the addresses table
// for the given address.
func (pgb *ChainDB) AddressHistoryAll(address string, N, offset int64) ([]*dbtypes.AddressRow, *explorer.AddressBalance, error) {
	return pgb.AddressHistory(address, N, offset, dbtypes.AddrTxnAll)
}

// GetTicketPoolBlockMaturity returns the block at which all tickets with height
// greater than it are immature.
func (pgb *ChainDB) GetTicketPoolBlockMaturity() int64 {
	bestBlock := int64(pgb.stakeDB.Height())
	return bestBlock - int64(pgb.chainParams.TicketMaturity)
}

// GetTicketPoolByDateAndInterval fetches the tickets ordered by the purchase date
// interval provided and an error value.
func (pgb *ChainDB) GetTicketPoolByDateAndInterval(maturityBlock int64,
	interval dbtypes.ChartGrouping) (*dbtypes.PoolTicketsData, error) {
	val, err := dbtypes.ChartGroupingToInterval(interval)
	if err != nil {
		return nil, err
	}

	return retrieveTicketsByDate(pgb.db, maturityBlock, int64(val))
}

// TicketPoolVisualization helps block consecutive and duplicate DB queries for
// the requested ticket pool chart data. If the data for the given interval is
// cached and fresh, it is returned. If the cached data is stale and there are
// no queries running to update the cache for the given interval, this launches
// a query and updates the cache. If there is no cached data for the interval,
// this will launch a new query for the data if one is not already running, and
// if one is running, it will wait for the query to complete.
func (pgb *ChainDB) TicketPoolVisualization(interval dbtypes.ChartGrouping) ([]*dbtypes.PoolTicketsData, *dbtypes.PoolTicketsData, uint64, error) {
	// Attempt to retrieve data for the current block from cache.
	heightSeen := pgb.Height() // current block seen *by the ChainDB*
	barcharts, donutCharts, height, intervalFound, stale := TicketPoolData(interval, heightSeen)
	if intervalFound && !stale {
		// The cache was fresh.
		return barcharts, donutCharts, height, nil
	}

	// Cache is stale or empty. Attempt to gain updater status.
	if !pgb.tpUpdatePermission[interval].TryLock() {
		// Another goroutine is running db query to get the updated data.
		if !intervalFound {
			// Do not even have stale data. Must wait for the DB update to
			// complete to get any data at all. Use a blocking call on the
			// updater lock even though we are not going to actually do an
			// update ourselves so we do not block the cache while waiting.
			pgb.tpUpdatePermission[interval].Lock()
			defer pgb.tpUpdatePermission[interval].Unlock()
			// Try again to pull it from cache now that the update is completed.
			heightSeen = pgb.Height()
			barcharts, donutCharts, height, intervalFound, stale = TicketPoolData(interval, heightSeen)
			// We waited for the updater of this interval, so it should be found
			// at this point. If not, this is an error.
			if !intervalFound {
				log.Errorf("Charts data for interval %v failed to update.", interval)
				return nil, nil, 0, fmt.Errorf("no charts data available")
			}
			if stale {
				log.Warnf("Charts data for interval %v updated, but still stale.", interval)
			}
		}
		// else return the stale data instead of waiting.

		return barcharts, donutCharts, height, nil
	}
	// This goroutine is now the cache updater.
	defer pgb.tpUpdatePermission[interval].Unlock()

	// Retrieve chart data for best block in DB.
	var err error
	barcharts, donutCharts, height, err = pgb.ticketPoolVisualization(interval)
	if err != nil {
		log.Errorf("Failed to fetch ticket pool data: %v", err)
		return nil, nil, 0, err
	}

	// Update the cache with the new ticket pool data.
	UpdateTicketPoolData(interval, barcharts, donutCharts, height)

	return barcharts, donutCharts, height, nil
}

// ticketPoolVisualization fetches the following ticketpool data: tickets
// grouped on the specified interval, tickets grouped by price, and ticket
// counts by ticket type (solo, pool, other split). The interval may be one of:
// "mo", "wk", "day", or "all". The data is needed to populate the ticketpool
// graphs. The data grouped by time and price are returned in a slice.
func (pgb *ChainDB) ticketPoolVisualization(interval dbtypes.ChartGrouping) (byTimeAndPrice []*dbtypes.PoolTicketsData, byInputs *dbtypes.PoolTicketsData, height uint64, err error) {
	// Ensure DB height is the same before and after queries since they are not
	// atomic. Initial height:
	height = pgb.Height()

	for {
		// Latest block where mature tickets may have been mined.
		maturityBlock := pgb.GetTicketPoolBlockMaturity()

		// Tickets grouped by time interval
		ticketsByTime, err := pgb.GetTicketPoolByDateAndInterval(maturityBlock, interval)
		if err != nil {
			return nil, nil, 0, err
		}

		// Tickets grouped by price
		ticketsByPrice, err := retrieveTicketByPrice(pgb.db, maturityBlock)
		if err != nil {
			return nil, nil, 0, err
		}

		// Return time- and price-grouped data in a slice
		byTimeAndPrice = []*dbtypes.PoolTicketsData{ticketsByTime, ticketsByPrice}

		// Tickets grouped by number of inputs.
		byInputs, err = retrieveTicketsGroupedByType(pgb.db)
		if err != nil {
			return nil, nil, 0, err
		}

		heightEnd := pgb.Height()
		if heightEnd == height {
			break
		}
		// otherwise try again to ensure charts are consistent.
		height = heightEnd
	}

	return
}

// retrieveDevBalance retrieves a new DevFundBalance without regard to the cache
func (pgb *ChainDB) retrieveDevBalance() (*DevFundBalance, error) {
	bb, hash, _, err := RetrieveBestBlockHeight(pgb.db)
	if err != nil {
		return nil, err
	}
	blockHeight := int64(bb)
	blockHash, err := chainhash.NewHashFromStr(hash)
	if err != nil {
		return nil, err
	}

	_, devBalance, err := pgb.AddressHistoryAll(pgb.devAddress, 1, 0)
	balance := &DevFundBalance{
		AddressBalance: devBalance,
		Height:         blockHeight,
		Hash:           *blockHash,
	}
	return balance, err
}

// UpdateDevBalance forcibly updates the cached development/project fund balance
// via DB queries. The bool output inidcates if the cached balance was updated
// (if it was stale).
func (pgb *ChainDB) UpdateDevBalance() (bool, error) {
	// See if a DB query is already running
	okToUpdate := pgb.DevFundBalance.updating.TryLock()
	// Wait on readers and possibly a writer regardless so the response will not
	// be stale even when this call doesn't call updateDevBalance.
	pgb.DevFundBalance.Lock()
	defer pgb.DevFundBalance.Unlock()
	// If we got the trylock, do an actual query for the balance
	if okToUpdate {
		defer pgb.DevFundBalance.updating.Unlock()
		return pgb.updateDevBalance()
	}
	// Otherwise the other call will have just updated the balance, and we
	// should not waste the cycles doing it again.
	return false, nil
}

func (pgb *ChainDB) updateDevBalance() (bool, error) {
	// Query for current balance.
	balance, err := pgb.retrieveDevBalance()
	if err != nil {
		return false, err
	}

	// If cache is stale, update it's fields.
	if balance.Hash != pgb.DevFundBalance.Hash || pgb.DevFundBalance.AddressBalance == nil {
		pgb.DevFundBalance.AddressBalance = balance.AddressBalance
		pgb.DevFundBalance.Height = balance.Height
		pgb.DevFundBalance.Hash = balance.Hash
		return true, nil
	}

	// Cache was not stale
	return false, nil
}

// DevBalance returns the current development/project fund balance, updating the
// cached balance if it is stale.
func (pgb *ChainDB) DevBalance() (*explorer.AddressBalance, error) {
	if !pgb.InReorg {
		hash, err := pgb.HashDB()
		if err != nil {
			return nil, err
		}

		// Update cache if stale
		if pgb.DevFundBalance.BlockHash().String() != hash || pgb.DevFundBalance.Balance() == nil {
			if _, err = pgb.UpdateDevBalance(); err != nil {
				return nil, err
			}
		}
	}

	bal := pgb.DevFundBalance.Balance()
	if bal == nil {
		return nil, fmt.Errorf("failed to update dev balance")
	}

	// return a copy of AddressBalance
	balCopy := *bal
	return &balCopy, nil
}

// addressBalance attempts to retrieve the explorer.AddressBalance from cache,
// and if cache is stale or missing data for the address, a DB query is used. A
// successful DB query will freshen the cache.
func (pgb *ChainDB) addressBalance(address string) (*explorer.AddressBalance, error) {
	bb, err := pgb.HeightDB()
	if err != nil {
		return nil, err
	}
	bestBlock := int64(bb)

	totals := pgb.addressCounts
	totals.Lock()
	defer totals.Unlock()

	var balanceInfo explorer.AddressBalance
	var fresh bool
	if totals.validHeight == bestBlock {
		balanceInfo, fresh = totals.balance[address]
	} else {
		// StoreBlock should do this, but the idea is to clear the old cached
		// results when a new block is encountered.
		log.Debugf("Address receive counter stale, at block %d when best is %d.",
			totals.validHeight, bestBlock)
		totals.balance = make(map[string]explorer.AddressBalance)
		totals.validHeight = bestBlock
		balanceInfo.Address = address
	}

	if !fresh {
		numSpent, numUnspent, amtSpent, amtUnspent, numMergedSpent, err :=
			RetrieveAddressSpentUnspent(pgb.db, address)
		if err != nil {
			return nil, err
		}
		balanceInfo = explorer.AddressBalance{
			Address:        address,
			NumSpent:       numSpent,
			NumUnspent:     numUnspent,
			NumMergedSpent: numMergedSpent,
			TotalSpent:     amtSpent,
			TotalUnspent:   amtUnspent,
		}

		totals.balance[address] = balanceInfo
	}

	return &balanceInfo, nil
}

// AddressHistory queries the database for rows of the addresses table
// containing values for a certain type of transaction (all, credits, or debits)
// for the given address.
func (pgb *ChainDB) AddressHistory(address string, N, offset int64,
	txnType dbtypes.AddrTxnType) ([]*dbtypes.AddressRow, *explorer.AddressBalance, error) {

	bb, err := pgb.HeightDB() // TODO: should be by block hash
	if err != nil {
		return nil, nil, err
	}
	bestBlock := int64(bb)

	// See if address count cache includes a fresh count for this address.
	totals := pgb.addressCounts
	totals.Lock()
	var balanceInfo explorer.AddressBalance
	var fresh bool
	if totals.validHeight == bestBlock {
		balanceInfo, fresh = totals.balance[address]
	} else {
		// StoreBlock should do this, but the idea is to clear the old cached
		// results when a new block is encountered.
		log.Debugf("Address receive counter stale, at block %d when best is %d.",
			totals.validHeight, bestBlock)
		totals.balance = make(map[string]explorer.AddressBalance)
		totals.validHeight = bestBlock
		balanceInfo.Address = address
	}
	totals.Unlock()

	// Retrieve relevant transactions
	addressRows, err := pgb.AddressTransactions(address, N, offset, txnType)
	if err != nil {
		return nil, nil, err
	}
	if fresh || len(addressRows) == 0 {
		return addressRows, &balanceInfo, nil
	}

	// If the address receive count was not cached, compute it and store it in
	// the cache.
	addrInfo := explorer.ReduceAddressHistory(addressRows)
	if addrInfo == nil {
		return addressRows, nil, fmt.Errorf("ReduceAddressHistory failed. len(addressRows) = %d", len(addressRows))
	}

	// You've got all txs when the total number of fetched txs is less than the
	// limit ,txtype is AddrTxnAll and Offset is zero.
	if len(addressRows) < int(N) && offset == 0 && txnType == dbtypes.AddrTxnAll {
		log.Debugf("Taking balance shortcut since address rows includes all.")
		balanceInfo = explorer.AddressBalance{
			Address:      address,
			NumSpent:     addrInfo.NumSpendingTxns,
			NumUnspent:   addrInfo.NumFundingTxns - addrInfo.NumSpendingTxns,
			TotalSpent:   int64(addrInfo.AmountSent),
			TotalUnspent: int64(addrInfo.AmountUnspent),
		}
	} else {
		log.Debugf("Obtaining balance via DB query.")
		numSpent, numUnspent, amtSpent, amtUnspent, numMergedSpent, err :=
			RetrieveAddressSpentUnspent(pgb.db, address)
		if err != nil {
			return nil, nil, err
		}
		balanceInfo = explorer.AddressBalance{
			Address:        address,
			NumSpent:       numSpent,
			NumUnspent:     numUnspent,
			NumMergedSpent: numMergedSpent,
			TotalSpent:     amtSpent,
			TotalUnspent:   amtUnspent,
		}
	}

	log.Infof("%s: %d spent totalling %f DCR, %d unspent totalling %f DCR",
		address, balanceInfo.NumSpent, dcrutil.Amount(balanceInfo.TotalSpent).ToCoin(),
		balanceInfo.NumUnspent, dcrutil.Amount(balanceInfo.TotalUnspent).ToCoin())
	log.Infof("Caching address receive count for address %s: "+
		"count = %d at block %d.", address,
		balanceInfo.NumSpent+balanceInfo.NumUnspent, bestBlock)
	totals.Lock()
	totals.balance[address] = balanceInfo
	totals.Unlock()

	return addressRows, &balanceInfo, nil
}

// FillAddressTransactions is used to fill out the transaction details in an
// explorer.AddressInfo generated by explorer.ReduceAddressHistory, usually from
// the output of AddressHistory. This function also sets the number of
// unconfirmed transactions for the current best block in the database.
func (pgb *ChainDB) FillAddressTransactions(addrInfo *explorer.AddressInfo) error {
	if addrInfo == nil {
		return nil
	}

	var numUnconfirmed int64

	for i, txn := range addrInfo.Transactions {
		_, dbTx, err := RetrieveDbTxByHash(pgb.db, txn.TxID)
		if err != nil {
			return err
		}
		txn.Size = dbTx.Size
		txn.FormattedSize = humanize.Bytes(uint64(dbTx.Size))
		txn.Total = dcrutil.Amount(dbTx.Sent).ToCoin()
		txn.Time = dbTx.BlockTime
		if dbTx.BlockTime > 0 {
			txn.Confirmations = pgb.Height() - uint64(dbTx.BlockHeight) + 1
		} else {
			numUnconfirmed++
			txn.Confirmations = 0
		}
		txn.FormattedTime = time.Unix(dbTx.BlockTime, 0).Format("2006-01-02 15:04:05")

		// Get the funding or spending transaction matching index if there is a
		// matching tx hash already present.  During the next database
		// restructuring we may want to consider including matching tx index
		// along with matching tx hash in the addresses table.
		if txn.MatchedTx != `` {
			if !txn.IsFunding {
				// Lookup by the database row id
				idx, err := RetrieveFundingOutpointIndxByVinID(pgb.db, dbTx.VinDbIds[txn.InOutID])

				if err != nil {
					log.Warnf("Matched Transaction Lookup failed for %s:%d: id: %d:  %v",
						txn.TxID, txn.InOutID, txn.InOutID, err)
				} else {
					addrInfo.Transactions[i].MatchedTxIndex = idx
				}

			} else {
				// Lookup by the matching tx hash and matching tx index
				_, idx, _, err := pgb.SpendingTransaction(txn.TxID, txn.InOutID)

				if err != nil {
					log.Warnf("Matched Transaction Lookup failed for %s:%d: %v",
						txn.TxID, txn.InOutID, err)
				} else {
					addrInfo.Transactions[i].MatchedTxIndex = idx
				}
			}
		}
	}

	addrInfo.NumUnconfirmed = numUnconfirmed

	return nil
}

// AddressTotals queries for the following totals: amount spent, amount unspent,
// number of unspent transaction outputs and number spent.
func (pgb *ChainDB) AddressTotals(address string) (*apitypes.AddressTotals, error) {
	// Fetch address totals
	var err error
	var ab *explorer.AddressBalance
	if address == pgb.devAddress {
		ab, err = pgb.DevBalance()
	} else {
		ab, err = pgb.addressBalance(address)
	}

	if err != nil || ab == nil {
		return nil, err
	}

	bestHeight, bestHash, _, err := RetrieveBestBlockHeight(pgb.db)
	if err != nil {
		return nil, err
	}

	return &apitypes.AddressTotals{
		Address:      address,
		BlockHeight:  bestHeight,
		BlockHash:    bestHash,
		NumSpent:     ab.NumSpent,
		NumUnspent:   ab.NumUnspent,
		CoinsSpent:   dcrutil.Amount(ab.TotalSpent).ToCoin(),
		CoinsUnspent: dcrutil.Amount(ab.TotalUnspent).ToCoin(),
	}, nil
}

func (pgb *ChainDB) addressInfo(addr string, count, skip int64,
	txnType dbtypes.AddrTxnType) (*explorer.AddressInfo, *explorer.AddressBalance, error) {
	address, err := dcrutil.DecodeAddress(addr)
	if err != nil {
		log.Infof("Invalid address %s: %v", addr, err)
		return nil, nil, err
	}

	// Get rows from the addresses table for the address
	addrHist, balance, errH := pgb.AddressHistory(addr, count, skip, txnType)
	if errH != nil {
		log.Errorf("Unable to get address %s history: %v", address, errH)
		return nil, nil, errH
	}

	// Generate AddressInfo skeleton from the address table rows
	addrData := explorer.ReduceAddressHistory(addrHist)
	if addrData == nil {
		// Empty history is not expected for credit txnType with any txns.
		if txnType != dbtypes.AddrTxnDebit && (balance.NumSpent+balance.NumUnspent) > 0 {
			return nil, nil, fmt.Errorf("empty address history (%s): n=%d&start=%d", address, count, skip)
		}
		// No mined transactions. Return Address with nil Transactions slice.
		return nil, balance, nil
	}

	// Transactions to fetch with FillAddressTransactions. This should be a
	// noop if AddressHistory/ReduceAddressHistory are working right.
	switch txnType {
	case dbtypes.AddrTxnAll, dbtypes.AddrMergedTxnDebit:
	case dbtypes.AddrTxnCredit:
		addrData.Transactions = addrData.TxnsFunding
	case dbtypes.AddrTxnDebit:
		addrData.Transactions = addrData.TxnsSpending
	default:
		// shouldn't happen because AddressHistory does this check
		return nil, nil, fmt.Errorf("unknown address transaction type: %v", txnType)
	}

	// Query database for transaction details
	err = pgb.FillAddressTransactions(addrData)
	if err != nil {
		return nil, balance, fmt.Errorf("Unable to fill address %s transactions: %v", address, err)
	}

	return addrData, balance, nil
}

// AddressTransactionDetails returns an apitypes.Address with at most the last
// count transactions of type txnType in which the address was involved,
// starting after skip transactions. This does NOT include unconfirmed
// transactions.
func (pgb *ChainDB) AddressTransactionDetails(addr string, count, skip int64,
	txnType dbtypes.AddrTxnType) (*apitypes.Address, error) {
	// Fetch address history for given transaction range and type
	addrData, _, err := pgb.addressInfo(addr, count, skip, txnType)
	if err != nil {
		return nil, err
	}
	// No transactions found. Not an error.
	if addrData == nil {
		return &apitypes.Address{
			Address:      addr,
			Transactions: make([]*apitypes.AddressTxShort, 0), // not nil for JSON formatting
		}, nil
	}

	// Convert each explorer.AddressTx to apitypes.AddressTxShort
	txs := addrData.Transactions
	txsShort := make([]*apitypes.AddressTxShort, 0, len(txs))
	for i := range txs {
		txsShort = append(txsShort, &apitypes.AddressTxShort{
			TxID:          txs[i].TxID,
			Time:          txs[i].Time,
			Value:         txs[i].Total,
			Confirmations: int64(txs[i].Confirmations),
			Size:          int32(txs[i].Size),
		})
	}

	// put a bow on it
	return &apitypes.Address{
		Address:      addr,
		Transactions: txsShort,
	}, nil
}

// TODO: finish
func (pgb *ChainDB) AddressTransactionRawDetails(addr string, count, skip int64,
	txnType dbtypes.AddrTxnType) ([]*apitypes.AddressTxRaw, error) {
	addrData, _, err := pgb.addressInfo(addr, count, skip, txnType)
	if err != nil {
		return nil, err
	}

	// Convert each explorer.AddressTx to apitypes.AddressTxRaw
	txs := addrData.Transactions
	txsRaw := make([]*apitypes.AddressTxRaw, 0, len(txs))
	for i := range txs {
		txsRaw = append(txsRaw, &apitypes.AddressTxRaw{
			Size: int32(txs[i].Size),
			TxID: txs[i].TxID,
			// Version
			// LockTime
			// Vin
			// Vout
			//
			Confirmations: int64(txs[i].Confirmations),
			//BlockHash: txs[i].
			Time: txs[i].Time,
			//Blocktime:
		})
	}

	return txsRaw, nil
}

// Store satisfies BlockDataSaver. Blocks stored this way are considered valid
// and part of mainchain.
func (pgb *ChainDB) Store(blockData *blockdata.BlockData, msgBlock *wire.MsgBlock) error {
	if pgb == nil {
		return nil
	}
	// New blocks stored this way are considered valid and part of mainchain.
	// When adding side chain blocks manually, call StoreBlock directly with
	// appropriate flags for isMainchain and isValid, and nil winningTickets.
	_, _, err := pgb.StoreBlock(msgBlock, blockData.WinningTickets, true, true, true, true)
	return err
}

// GetTxHistoryData fetches the address history chart data for the provided parameters.
func (pgb *ChainDB) GetTxHistoryData(address string, addrChart dbtypes.HistoryChart,
	chartGroupings dbtypes.ChartGrouping) (*dbtypes.ChartsData, error) {
	timeInterval, err := dbtypes.ChartGroupingToInterval(chartGroupings)
	if err != nil {
		return nil, fmt.Errorf("GetTxHistoryData error: %v", err)
	}

	timestamp := int64(timeInterval)

	switch addrChart {
	case dbtypes.TxsType:
		return retrieveTxHistoryByType(pgb.db, address, timestamp)

	case dbtypes.AmountFlow:
		return retrieveTxHistoryByAmountFlow(pgb.db, address, timestamp)

	case dbtypes.TotalUnspent:
		return retrieveTxHistoryByUnspentAmount(pgb.db, address, timestamp)

	default:
		return nil, fmt.Errorf("unknown error occurred")
	}
}

// GetTicketsPriceByHeight returns the ticket price by height chart data.
// This is the default chart that appears at charts page.
func (pgb *ChainDB) GetTicketsPriceByHeight() (*dbtypes.ChartsData, error) {
	d, err := RetrieveTicketsPriceByHeight(pgb.db, pgb.chainParams.StakeDiffWindowSize)
	if err != nil {
		return nil, fmt.Errorf("RetrieveTicketsPriceByHeight: %v", err)
	}
	return &dbtypes.ChartsData{Time: d.Time, ValueF: d.ValueF}, nil
}

// GetPgChartsData fetches the charts data that is stored in pg
func (pgb *ChainDB) GetPgChartsData() (map[string]*dbtypes.ChartsData, error) {
	tickets, err := RetrieveTicketsPriceByHeight(pgb.db, pgb.chainParams.StakeDiffWindowSize)
	if err != nil {
		return nil, fmt.Errorf("RetrieveTicketsPriceByHeight: %v", err)
	}

	supply, err := retrieveCoinSupply(pgb.db)
	if err != nil {
		return nil, fmt.Errorf("retrieveCoinSupply: %v", err)
	}

	size, err := retrieveBlockTicketsPoolValue(pgb.db)
	if err != nil {
		return nil, fmt.Errorf("retrieveBlockTicketsPoolValue: %v", err)
	}

	txRate, err := retrieveTxPerDay(pgb.db)
	if err != nil {
		return nil, fmt.Errorf("retrieveTxPerDay: %v", err)
	}

	ticketsSpendType, err := retrieveTicketSpendTypePerBlock(pgb.db)
	if err != nil {
		return nil, fmt.Errorf("retrieveTicketSpendTypePerBlock: %v", err)
	}

	ticketsByOutputsAllBlocks, err := retrieveTicketByOutputCount(pgb.db, outputCountByAllBlocks)
	if err != nil {
		return nil, fmt.Errorf("retrieveTicketByOutputCount by All Blocks: %v", err)
	}

	ticketsByOutputsTPWindow, err := retrieveTicketByOutputCount(pgb.db, outputCountByTicketPoolWindow)
	if err != nil {
		return nil, fmt.Errorf("retrieveTicketByOutputCount by All TP window: %v", err)
	}

	var data = map[string]*dbtypes.ChartsData{
		"avg-block-size":            {Time: size.Time, Size: size.Size},
		"blockchain-size":           {Time: size.Time, ChainSize: size.ChainSize},
		"tx-per-block":              {Value: size.Value, Count: size.Count},
		"duration-btw-blocks":       {Value: size.Value, ValueF: size.ValueF},
		"tx-per-day":                txRate,
		"pow-difficulty":            {Time: tickets.Time, Difficulty: tickets.Difficulty},
		"ticket-price":              {Time: tickets.Time, ValueF: tickets.ValueF},
		"coin-supply":               supply,
		"ticket-spend-type":         ticketsSpendType,
		"ticket-by-outputs-blocks":  ticketsByOutputsAllBlocks,
		"ticket-by-outputs-windows": ticketsByOutputsTPWindow,
	}

	return data, nil
}

func (pgb *ChainDB) SetVinsMainchainByBlock(blockHash string) (int64, []dbtypes.UInt64Array, []dbtypes.UInt64Array, error) {
	// Get vins DB IDs for the block
	onlyRegularTxns := false
	vinDbIDsBlk, voutDbIDsBlk, areMainchain, err := RetrieveTxnsVinsVoutsByBlock(pgb.db, blockHash, onlyRegularTxns)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("unable to retrieve vin data for block %s: %v", blockHash, err)
	}
	vinsUpdated, err := pgb.setVinsMainchainForMany(vinDbIDsBlk, areMainchain)
	return vinsUpdated, vinDbIDsBlk, voutDbIDsBlk, err
}

func (pgb *ChainDB) setVinsMainchainForMany(vinDbIDsBlk []dbtypes.UInt64Array,
	areMainchain []bool) (int64, error) {
	var rowsUpdated int64
	// each transaction
	for it, vs := range vinDbIDsBlk {
		// each vin
		numUpd, err := pgb.setVinsMainchainOneTxn(vs, areMainchain[it])
		if err != nil {
			continue
		}
		rowsUpdated += numUpd
	}
	return rowsUpdated, nil
}

func (pgb *ChainDB) setVinsMainchainOneTxn(vinDbIDs dbtypes.UInt64Array,
	isMainchain bool) (int64, error) {
	var rowsUpdated int64

	// each vin
	for _, vinDbID := range vinDbIDs {
		result, err := pgb.db.Exec(internal.SetIsMainchainByVinID,
			vinDbID, isMainchain)
		if err != nil {
			log.Warnf("db ID not found: %d", vinDbID)
			continue
		}

		c, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}

		rowsUpdated += c
	}

	return rowsUpdated, nil
}

// VinsForTx returns a slice of dbtypes.VinTxProperty values for each vin
// referenced by the transaction dbTx.
func (pgb *ChainDB) VinsForTx(dbTx *dbtypes.Tx) ([]dbtypes.VinTxProperty, []string, []uint16, error) {
	var prevPkScripts []string
	var versions []uint16
	for _, id := range dbTx.VinDbIds {
		pkScript, ver, err := RetrievePkScriptByID(pgb.db, id)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("RetrievePkScriptByID: %v", err)
		}
		prevPkScripts = append(prevPkScripts, hex.EncodeToString(pkScript))
		versions = append(versions, ver)
	}
	vins, err := RetrieveVinsByIDs(pgb.db, dbTx.VinDbIds)
	if err != nil {
		err = fmt.Errorf("RetrieveVinsByIDs: %v", err)
	}
	return vins, prevPkScripts, versions, err
}

// VoutsForTx returns a slice of dbtypes.Vout values for each vout referenced by
// the transaction dbTx.
func (pgb *ChainDB) VoutsForTx(dbTx *dbtypes.Tx) ([]dbtypes.Vout, error) {
	return RetrieveVoutsByIDs(pgb.db, dbTx.VoutDbIds)
}

func (pgb *ChainDB) TipToSideChain(mainRoot string) (string, int64, error) {
	tipHash := pgb.bestBlockHash
	var blocksMoved, txnsUpdated, vinsUpdated, votesUpdated, ticketsUpdated, addrsUpdated int64
	for tipHash != mainRoot {
		// 1. Block. Set is_mainchain=false on the tip block, return hash of
		// previous block.
		now := time.Now()
		previousHash, err := SetMainchainByBlockHash(pgb.db, tipHash, false)
		if err != nil {
			log.Errorf("Failed to set block %s as a sidechain block: %v",
				tipHash, err)
		}
		blocksMoved++
		log.Debugf("SetMainchainByBlockHash: %v", time.Since(now))

		// 2. Transactions. Set is_mainchain=false on all transactions in the
		// tip block, returning only the number of transactions updated.
		now = time.Now()
		rowsUpdated, _, err := UpdateTransactionsMainchain(pgb.db, tipHash, false)
		if err != nil {
			log.Errorf("Failed to set transactions in block %s as sidechain: %v",
				tipHash, err)
		}
		txnsUpdated += rowsUpdated
		log.Debugf("UpdateTransactionsMainchain: %v", time.Since(now))

		// 3. Vins. Set is_mainchain=false on all vins, returning the number of
		// vins updated, the vins table row IDs, and the vouts table row IDs.
		now = time.Now()
		rowsUpdated, vinDbIDsBlk, voutDbIDsBlk, err := pgb.SetVinsMainchainByBlock(tipHash) // isMainchain from transactions table
		if err != nil {
			log.Errorf("Failed to set vins in block %s as sidechain: %v",
				tipHash, err)
		}
		vinsUpdated += rowsUpdated
		log.Debugf("SetVinsMainchainByBlock: %v", time.Since(now))

		// 4. Addresses. Set valid_mainchain=false on all addresses rows
		// corresponding to the spending transactions specified by the vins DB
		// row IDs, and the funding transactions specified by the vouts DB row
		// IDs. The IDs come for free via RetrieveTxnsVinsVoutsByBlock.
		now = time.Now()
		numAddrSpending, numAddrFunding, err := UpdateAddressesMainchainByIDs(pgb.db,
			vinDbIDsBlk, voutDbIDsBlk, false)
		if err != nil {
			log.Errorf("Failed to set addresses rows in block %s as sidechain: %v",
				tipHash, err)
		}
		addrsUpdated += numAddrSpending + numAddrFunding
		log.Debugf("UpdateAddressesMainchainByIDs: %v", time.Since(now))

		// 5. Votes. Sets is_mainchain=false on all votes in the tip block.
		now = time.Now()
		rowsUpdated, err = UpdateVotesMainchain(pgb.db, tipHash, false)
		if err != nil {
			log.Errorf("Failed to set votes in block %s as sidechain: %v",
				tipHash, err)
		}
		votesUpdated += rowsUpdated
		log.Debugf("UpdateVotesMainchain: %v", time.Since(now))

		// 6. Tickets. Sets is_mainchain=false on all tickets in the tip block.
		now = time.Now()
		rowsUpdated, err = UpdateTicketsMainchain(pgb.db, tipHash, false)
		if err != nil {
			log.Errorf("Failed to set tickets in block %s as sidechain: %v",
				tipHash, err)
		}
		ticketsUpdated += rowsUpdated
		log.Debugf("UpdateTicketsMainchain: %v", time.Since(now))

		// move on to next block
		tipHash = previousHash

		pgb.bestBlock, err = pgb.BlockHeight(tipHash)
		if err != nil {
			log.Errorf("Failed to retrieve block height for %s", tipHash)
		}
		pgb.bestBlockHash = tipHash
	}

	log.Debugf("Reorg orphaned: %d blocks, %d txns, %d vins, %d addresses, %d votes, %d tickets",
		blocksMoved, txnsUpdated, vinsUpdated, addrsUpdated, votesUpdated, ticketsUpdated)

	return tipHash, blocksMoved, nil
}

// StoreBlock processes the input wire.MsgBlock, and saves to the data tables.
// The number of vins, and vouts stored are also returned.
func (pgb *ChainDB) StoreBlock(msgBlock *wire.MsgBlock, winningTickets []string,
	isValid, isMainchain, updateAddressesSpendingInfo, updateTicketsSpendingInfo bool) (numVins int64, numVouts int64, err error) {
	// Convert the wire.MsgBlock to a dbtypes.Block
	dbBlock := dbtypes.MsgBlockToDBBlock(msgBlock, pgb.chainParams)

	// Get the previous winners (stake DB pool info cache has this info). If the
	// previous block is side chain, stakedb will not have the
	// winners/validators. Since Validators are only used to identify misses in
	// InsertVotes, we will leave the Validators empty and assume there are no
	// misses. If this block becomes main chain at some point via a
	// reorganization, its table entries will be updated appropriately, which
	// will include inserting any misses since the stakeDB will then include the
	// block, thus allowing the winning tickets to be known at that time.
	// TODO: Somehow verify reorg operates as described when switching manually
	// imported side chain blocks over to main chain.
	prevBlockHash := msgBlock.Header.PrevBlock
	var winners []string
	if isMainchain && !bytes.Equal(zeroHash[:], prevBlockHash[:]) {
		tpi, found := pgb.stakeDB.PoolInfo(prevBlockHash)
		if !found {
			err = fmt.Errorf("stakedb.PoolInfo failed for block %s", msgBlock.BlockHash())
			return
		}
		winners = tpi.Winners
	}

	// Wrap the message block
	MsgBlockPG := &MsgBlockPG{
		MsgBlock:       msgBlock,
		WinningTickets: winningTickets,
		Validators:     winners,
	}

	// Extract transactions and their vouts, and insert vouts into their pg table,
	// returning their DB PKs, which are stored in the corresponding transaction
	// data struct. Insert each transaction once they are updated with their
	// vouts' IDs, returning the transaction PK ID, which are stored in the
	// containing block data struct.

	// regular transactions
	resChanReg := make(chan storeTxnsResult)
	go func() {
		resChanReg <- pgb.storeTxns(MsgBlockPG, wire.TxTreeRegular,
			pgb.chainParams, &dbBlock.TxDbIDs, isValid, isMainchain,
			updateAddressesSpendingInfo, updateTicketsSpendingInfo)
	}()

	// stake transactions
	resChanStake := make(chan storeTxnsResult)
	go func() {
		resChanStake <- pgb.storeTxns(MsgBlockPG, wire.TxTreeStake,
			pgb.chainParams, &dbBlock.STxDbIDs, isValid, isMainchain,
			updateAddressesSpendingInfo, updateTicketsSpendingInfo)
	}()

	errReg := <-resChanReg
	errStk := <-resChanStake
	if errStk.err != nil {
		if errReg.err == nil {
			err = errStk.err
			numVins = errReg.numVins
			numVouts = errReg.numVouts
			return
		}
		err = errors.New(errReg.Error() + ", " + errStk.Error())
		return
	} else if errReg.err != nil {
		err = errReg.err
		numVins = errStk.numVins
		numVouts = errStk.numVouts
		return
	}

	numVins = errStk.numVins + errReg.numVins
	numVouts = errStk.numVouts + errReg.numVouts

	// Store the block now that it has all it's transaction PK IDs
	var blockDbID uint64
	blockDbID, err = InsertBlock(pgb.db, dbBlock, isValid, isMainchain, pgb.dupChecks)
	if err != nil {
		log.Error("InsertBlock:", err)
		return
	}
	pgb.lastBlock[msgBlock.BlockHash()] = blockDbID

	pgb.bestBlock = int64(dbBlock.Height)
	pgb.bestBlockHash = dbBlock.Hash

	err = InsertBlockPrevNext(pgb.db, blockDbID, dbBlock.Hash,
		dbBlock.PreviousHash, "")
	if err != nil && err != sql.ErrNoRows {
		log.Error("InsertBlockPrevNext:", err)
		return
	}

	// Update last block in db with this block's hash as it's next. Also update
	// isValid flag in last block if votes in this block invalidated it.
	lastBlockHash := msgBlock.Header.PrevBlock
	// Only update if last was not genesis, which is not in the table (implied).
	if lastBlockHash != zeroHash {
		// Ensure previous block has the same main/sidechain status. If the
		// current block being added is side chain, do not invalidate the
		// mainchain block or any of its components, or update the block_chain
		// table to point to this block.
		blockStatus, errB := pgb.BlockStatus(lastBlockHash.String())
		if errB != nil {
			log.Errorf("Unable to determine status of previous block %v: %v",
				lastBlockHash, errB)
			return // do not return an error, but this should not happen
		}
		// Do not update previous block data if it is not the blockchain branch.
		if blockStatus.IsMainchain != isMainchain {
			// A main chain block should never have a side chain parent.
			if isMainchain {
				log.Errorf("Previous block %v on a different branch (main=%v)"+
					" from current block (main=%v). Not updating previous block's data.",
					lastBlockHash, blockStatus.IsMainchain, isMainchain)
			}
			return
		}

		// Attempt to find the row id of the block hash in cache.
		lastBlockDbID, ok := pgb.lastBlock[lastBlockHash]
		if !ok {
			log.Debugf("The previous block %s for block %s not found in cache, "+
				"looking it up.", lastBlockHash, msgBlock.BlockHash())
			lastBlockDbID, err = RetrieveBlockChainDbID(pgb.db, lastBlockHash.String())
			if err != nil {
				log.Criticalf("Unable to locate block %s in block_chain table: %v",
					lastBlockHash, err)
				return
			}
		}

		// Was the previous block invalidated? Update it's is_valid flag in the
		// blocks table if needed.
		lastIsValid := dbBlock.VoteBits&1 != 0
		if !lastIsValid {
			log.Infof("Setting last block %s as INVALID", lastBlockHash)
			err = UpdateLastBlockValid(pgb.db, lastBlockDbID, lastIsValid)
			if err != nil {
				log.Error("UpdateLastBlock:", err)
				return
			}
		}

		// Update the previous block's next block hash in the block_chain table.
		err = UpdateBlockNext(pgb.db, lastBlockDbID, dbBlock.Hash)
		if err != nil {
			log.Error("UpdateBlockNext:", err)
			return
		}

		// If the previous block is invalidated by this one, flag all the vins,
		// transactions, and addresses table rows from the previous block's
		// transactions as invalid. Do nothing otherwise since blocks'
		// transactions are initially added as valid.
		if !lastIsValid {
			// Update the is_valid flag in the transactions from the previous block.
			err = UpdateLastVins(pgb.db, lastBlockHash.String(), lastIsValid, isMainchain)
			if err != nil {
				log.Error("UpdateLastVins:", err)
				return
			}

			// Update last block's regular transactions.
			_, _, err = UpdateTransactionsValid(pgb.db, lastBlockHash.String(), lastIsValid)
			if err != nil {
				log.Error("UpdateTransactionsValid:", err)
				return
			}

			// Update addresses table for last block's regular transactions.
			err = UpdateLastAddressesValid(pgb.db, lastBlockHash.String(), lastIsValid)
			if err != nil {
				log.Error("UpdateLastAddressesValid:", err)
				return
			}

			// NOTE: Updating the tickets, votes, and misses tables is not
			// necessary since the stake tree is not subject to stakeholder
			// approval.
		}
	}

	// If not in batch sync, lazy update the dev fund balance
	if !pgb.InBatchSync {
		pgb.addressCounts.Lock()
		pgb.addressCounts.validHeight = int64(msgBlock.Header.Height)
		pgb.addressCounts.balance = map[string]explorer.AddressBalance{}
		pgb.addressCounts.Unlock()

		// Lazy update of DevFundBalance
		if pgb.devPrefetch && !pgb.InReorg {
			go func() {
				runtime.Gosched()
				if _, err = pgb.UpdateDevBalance(); err != nil {
					log.Errorf("Failed to update development fund balance: %v", err)
				}
			}()
		}
	}

	return
}

// storeTxnsResult is the type of object sent back from the goroutines wrapping
// storeTxns in StoreBlock.
type storeTxnsResult struct {
	numVins, numVouts, numAddresses int64
	err                             error
}

func (r *storeTxnsResult) Error() string {
	return r.err.Error()
}

// MsgBlockPG extends wire.MsgBlock with the winning tickets from the block,
// WinningTickets, and the tickets from the previous block that may vote on this
// block's validity, Validators.
type MsgBlockPG struct {
	*wire.MsgBlock
	WinningTickets []string
	Validators     []string
}

// storeTxns stores the transactions of a given block.
func (pgb *ChainDB) storeTxns(msgBlock *MsgBlockPG, txTree int8,
	chainParams *chaincfg.Params, TxDbIDs *[]uint64, isValid, isMainchain bool,
	updateAddressesSpendingInfo, updateTicketsSpendingInfo bool) storeTxnsResult {
	// For the given block, transaction tree, and network, extract the
	// transactions, vins, and vouts.
	dbTransactions, dbTxVouts, dbTxVins := dbtypes.ExtractBlockTransactions(
		msgBlock.MsgBlock, txTree, chainParams, isValid, isMainchain)

	// The return value, containing counts of inserted vins/vouts/txns, and an
	// error value.
	var txRes storeTxnsResult

	// dbAddressRows contains the data added to the address table, arranged as
	// [tx_i][addr_j], transactions paying to different numbers of addresses.
	dbAddressRows := make([][]dbtypes.AddressRow, len(dbTransactions))
	var totalAddressRows int

	// For a side chain block, set Validators to an empty slice so that there
	// will be no misses even if there are less than 5 votes. Any Validators
	// that do not match a spent ticket hash in InsertVotes are considered
	// misses. By listing no required validators, there are no misses. For side
	// chain blocks, this is acceptable and necessary because the misses table
	// does not record the block hash or main/side chain status.
	if !isMainchain {
		msgBlock.Validators = []string{}
	}

	var err error
	for it, dbtx := range dbTransactions {
		// Insert vouts, and collect AddressRows to add to address table for
		// each output.
		dbtx.VoutDbIds, dbAddressRows[it], err = InsertVouts(pgb.db, dbTxVouts[it], pgb.dupChecks)
		if err != nil && err != sql.ErrNoRows {
			log.Error("InsertVouts:", err)
			txRes.err = err
			return txRes
		}
		totalAddressRows += len(dbAddressRows[it])
		txRes.numVouts += int64(len(dbtx.VoutDbIds))
		if err == sql.ErrNoRows || len(dbTxVouts[it]) != len(dbtx.VoutDbIds) {
			log.Warnf("Incomplete Vout insert.")
		}

		// Insert vins
		dbtx.VinDbIds, err = InsertVins(pgb.db, dbTxVins[it], pgb.dupChecks)
		if err != nil && err != sql.ErrNoRows {
			log.Error("InsertVins:", err)
			txRes.err = err
			return txRes
		}
		txRes.numVins += int64(len(dbtx.VinDbIds))

		// return the transactions vout slice if processing stake tree
		if txTree == wire.TxTreeStake {
			dbtx.Vouts = dbTxVouts[it]
		}
	}

	// Get the tx PK IDs for storage in the blocks, tickets, and votes table
	*TxDbIDs, err = InsertTxns(pgb.db, dbTransactions, pgb.dupChecks)
	if err != nil && err != sql.ErrNoRows {
		log.Error("InsertTxns:", err)
		txRes.err = err
		return txRes
	}

	// If processing stake tree, insert tickets, votes, and misses. Also update
	// pool status and spending information in tickets table pertaining to the
	// new votes, revokes, misses, and expires.
	if txTree == wire.TxTreeStake {
		// Tickets: Insert new (unspent) tickets
		newTicketDbIDs, newTicketTx, err := InsertTickets(pgb.db, dbTransactions, *TxDbIDs, pgb.dupChecks)
		if err != nil && err != sql.ErrNoRows {
			log.Error("InsertTickets:", err)
			txRes.err = err
			return txRes
		}

		// Cache the unspent ticket DB row IDs and and their hashes. Needed do
		// efficiently update their spend status later.
		var unspentTicketCache *TicketTxnIDGetter
		if updateTicketsSpendingInfo {
			for it, tdbid := range newTicketDbIDs {
				pgb.unspentTicketCache.Set(newTicketTx[it].TxID, tdbid)
			}
			unspentTicketCache = pgb.unspentTicketCache
		}

		// Votes: insert votes and misses (tickets that did not vote when
		// called). Return the ticket hash of all misses, which may include
		// revokes at this point. Unrevoked misses are identified when updating
		// ticket spend info below.

		// voteDbIDs, voteTxns, spentTicketHashes, ticketDbIDs, missDbIDs, err := ...
		var missesHashIDs map[string]uint64
		_, _, _, _, missesHashIDs, err = InsertVotes(pgb.db, dbTransactions,
			*TxDbIDs, unspentTicketCache, msgBlock, pgb.dupChecks, pgb.chainParams)
		if err != nil && err != sql.ErrNoRows {
			log.Error("InsertVotes:", err)
			txRes.err = err
			return txRes
		}

		if updateTicketsSpendingInfo {
			// Get information for transactions spending tickets (votes and
			// revokes), and the ticket DB row IDs themselves. Also return
			// tickets table row IDs for newly spent tickets, if we are updating
			// them as we go (SetSpendingForTickets). CollectTicketSpendDBInfo
			// uses ChainDB's ticket DB row ID cache, and immediately expires
			// any found entries.
			spendingTxDbIDs, spendTypes, spentTicketHashes, ticketDbIDs, err :=
				pgb.CollectTicketSpendDBInfo(dbTransactions, *TxDbIDs, msgBlock.MsgBlock)
			if err != nil {
				log.Error("CollectTicketSpendDBInfo:", err)
				txRes.err = err
				return txRes
			}

			// Get a consistent view of the stake node at its present height.
			pgb.stakeDB.LockStakeNode()

			// Classify and record the height of each ticket spend (vote or
			// revoke). For revokes, further distinguish miss or expire.
			revokes := make(map[string]uint64)
			blockHeights := make([]int64, len(spentTicketHashes))
			poolStatuses := make([]dbtypes.TicketPoolStatus, len(spentTicketHashes))
			for iv := range spentTicketHashes {
				blockHeights[iv] = int64(msgBlock.Header.Height) /* voteDbTxns[iv].BlockHeight */

				// Vote or revoke
				switch spendTypes[iv] {
				case dbtypes.TicketVoted:
					poolStatuses[iv] = dbtypes.PoolStatusVoted
				case dbtypes.TicketRevoked:
					revokes[spentTicketHashes[iv]] = ticketDbIDs[iv]
					// Revoke reason
					h, err0 := chainhash.NewHashFromStr(spentTicketHashes[iv])
					if err0 != nil {
						log.Errorf("Invalid hash %v", spentTicketHashes[iv])
						continue // no info about spent ticket!
					}
					expired := pgb.stakeDB.BestNode.ExistsExpiredTicket(*h)
					if !expired {
						poolStatuses[iv] = dbtypes.PoolStatusMissed
					} else {
						poolStatuses[iv] = dbtypes.PoolStatusExpired
					}
				}
			}

			// Update tickets table with spending info.
			_, err = SetSpendingForTickets(pgb.db, ticketDbIDs, spendingTxDbIDs,
				blockHeights, spendTypes, poolStatuses)
			if err != nil {
				log.Error("SetSpendingForTickets:", err)
			}

			// Unspent not-live tickets are also either expired or missed.

			// Missed but not revoked
			var unspentMissedTicketHashes []string
			var missStatuses []dbtypes.TicketPoolStatus
			unspentMisses := make(map[string]struct{})
			// missesHashIDs refers to lottery winners that did not vote.
			for miss := range missesHashIDs {
				if _, ok := revokes[miss]; !ok {
					// unrevoked miss
					unspentMissedTicketHashes = append(unspentMissedTicketHashes, miss)
					unspentMisses[miss] = struct{}{}
					missStatuses = append(missStatuses, dbtypes.PoolStatusMissed)
				}
			}

			// Expired but not revoked
			unspentEnM := make([]string, len(unspentMissedTicketHashes))
			// Start with the unspent misses and append unspent expires to get
			// "unspent expired and missed".
			copy(unspentEnM, unspentMissedTicketHashes)
			unspentExpiresAndMisses := pgb.stakeDB.BestNode.MissedByBlock()
			for _, missHash := range unspentExpiresAndMisses {
				// MissedByBlock includes tickets that missed votes or expired
				// (and which may be revoked in this block); we just want the
				// expires, and not the revoked ones. Screen each ticket from
				// MissedByBlock for the actual unspent expires.
				if pgb.stakeDB.BestNode.ExistsExpiredTicket(missHash) {
					emHash := missHash.String()
					// Next check should not be unnecessary. Make sure not in
					// unspent misses from above, and not just revoked.
					_, justMissed := unspentMisses[emHash] // should be redundant
					_, justRevoked := revokes[emHash]      // exclude if revoked
					if !justMissed && !justRevoked {
						unspentEnM = append(unspentEnM, emHash)
						missStatuses = append(missStatuses, dbtypes.PoolStatusExpired)
					}
				}
			}

			// Release the stake node.
			pgb.stakeDB.UnlockStakeNode()

			// Update status of the unspent expired and missed tickets.
			numUnrevokedMisses, err := SetPoolStatusForTicketsByHash(pgb.db, unspentEnM, missStatuses)
			if err != nil {
				log.Errorf("SetPoolStatusForTicketsByHash: %v", err)
			} else if numUnrevokedMisses > 0 {
				log.Tracef("Noted %d unrevoked newly-missed tickets.", numUnrevokedMisses)
			}
		} // updateTicketsSpendingInfo
	} // txTree == wire.TxTreeStake

	// Store txn block time and mainchain validity status in AddressRows, and
	// set IsFunding to true since InsertVouts is supplying the AddressRows.
	dbAddressRowsFlat := make([]*dbtypes.AddressRow, 0, totalAddressRows)
	for it, tx := range dbTransactions {
		for iv := range dbAddressRows[it] {
			// Transaction that pays to the address
			dba := &dbAddressRows[it][iv]

			// Set fields not set by InsertVouts: TxBlockTime, IsFunding,
			// ValidMainChain, and MatchingTxHash. Only MatchingTxHash goes
			// unset initially, later set by insertSpendingTxByPrptStmt (called
			// by SetSpendingForFundingOP below, and other places).
			dba.TxBlockTime = uint64(tx.BlockTime)
			dba.IsFunding = true
			dba.ValidMainChain = isMainchain && isValid

			// Funding tx hash, vout id, value, and address are already assigned
			// by InsertVouts. Only the block time and is_funding was needed.
			dbAddressRowsFlat = append(dbAddressRowsFlat, dba)
		}
	}

	// Insert each new AddressRow, absent MatchingTxHash (spending txn since
	// these new address rows are *funding*).
	_, err = InsertAddressRows(pgb.db, dbAddressRowsFlat, pgb.dupChecks)
	if err != nil {
		log.Error("InsertAddressRows:", err)
		txRes.err = err
		return txRes
	}

	// Defer update of addresses table spending info for a batch process if
	// requested.
	if !updateAddressesSpendingInfo {
		return txRes
	}

	// Check the new vins and update matching_tx_hash in addresses table.
	for it, tx := range dbTransactions {
		// vins array for this transaction
		txVins := dbTxVins[it]
		for iv := range txVins {
			// Transaction that spends an outpoint paying to >=0 addresses
			vin := &txVins[iv]

			// Skip coinbase inputs (they are generated and thus have no
			// previous outpoint funding them).
			if bytes.Equal(zeroHashStringBytes, []byte(vin.PrevTxHash)) {
				continue
			}

			// Insert spending txn data in addresses table, and updated spend
			// status for the previous outpoints' rows in the same table.
			vinDbID := tx.VinDbIds[iv]
			spendingTxHash := vin.TxID
			spendingTxIndex := vin.TxIndex
			validMainchain := tx.IsValidBlock && tx.IsMainchainBlock
			numAddressRowsSet, err := SetSpendingForFundingOP(pgb.db,
				vin.PrevTxHash, vin.PrevTxIndex, int8(vin.PrevTxTree),
				spendingTxHash, spendingTxIndex, uint64(tx.BlockTime), vinDbID,
				pgb.dupChecks, validMainchain, vin.TxType)
			if err != nil {
				log.Errorf("SetSpendingForFundingOP: %v", err)
			}
			txRes.numAddresses += numAddressRowsSet
		}
	}

	return txRes
}

// CollectTicketSpendDBInfo processes the stake transactions in msgBlock, which
// correspond to the transaction data in dbTxns, and extracts data for votes and
// revokes, including the spent ticket hash and DB row ID.
func (pgb *ChainDB) CollectTicketSpendDBInfo(dbTxns []*dbtypes.Tx, txDbIDs []uint64,
	msgBlock *wire.MsgBlock) (spendingTxDbIDs []uint64, spendTypes []dbtypes.TicketSpendType,
	ticketHashes []string, ticketDbIDs []uint64, err error) {
	// This only makes sense for stake transactions
	msgTxns := msgBlock.STransactions
	if len(msgTxns) != len(dbTxns) {
		err = fmt.Errorf("number of stake transactions (%d) not as expected (%d)",
			len(msgTxns), len(dbTxns))
		return
	}

	for i, tx := range dbTxns {
		// ensure the transaction slices correspond
		msgTx := msgTxns[i]
		if tx.TxID != msgTx.TxHash().String() {
			err = fmt.Errorf("txid of dbtypes.Tx does not match that of msgTx")
			return
		}

		// Filter for votes and revokes only
		var stakeSubmissionVinInd int
		var spendType dbtypes.TicketSpendType
		switch tx.TxType {
		case int16(stake.TxTypeSSGen):
			spendType = dbtypes.TicketVoted
			stakeSubmissionVinInd = 1
		case int16(stake.TxTypeSSRtx):
			spendType = dbtypes.TicketRevoked
		default:
			continue
		}

		if stakeSubmissionVinInd >= len(msgTx.TxIn) {
			log.Warnf("Invalid vote or ticket with %d inputs", len(msgTx.TxIn))
			continue
		}

		spendTypes = append(spendTypes, spendType)

		// vote/revoke row ID in *transactions* table
		spendingTxDbIDs = append(spendingTxDbIDs, txDbIDs[i])

		// ticket hash
		ticketHash := msgTx.TxIn[stakeSubmissionVinInd].PreviousOutPoint.Hash.String()
		ticketHashes = append(ticketHashes, ticketHash)

		// ticket's row ID in *tickets* table
		t, err0 := pgb.unspentTicketCache.TxnDbID(ticketHash, true) // expire all cache entries
		if err0 != nil {
			err = fmt.Errorf("failed to retrieve ticket %s DB ID: %v", ticketHash, err0)
			return
		}
		ticketDbIDs = append(ticketDbIDs, t)
	}
	return
}

// UpdateSpendingInfoInAllAddresses completely rebuilds the spending transaction
// info columns of the address table. This is intended to be use after syncing
// all other tables and creating their indexes, particularly the indexes on the
// vins table, and the addresses table index on the funding tx columns. This can
// be used instead of using updateAddressesSpendingInfo=true with storeTxns,
// which will update these addresses table columns too, but much more slowly for
// a number of reasons (that are well worth investigating BTW!).
func (pgb *ChainDB) UpdateSpendingInfoInAllAddresses(barLoad chan *dbtypes.ProgressBarLoad) (int64, error) {
	// Get the full list of vinDbIDs
	allVinDbIDs, err := RetrieveAllVinDbIDs(pgb.db)
	if err != nil {
		log.Errorf("RetrieveAllVinDbIDs: %v", err)
		return 0, err
	}

	updatesPerDBTx := 500
	totalVinIbIDs := len(allVinDbIDs)

	timeStart := time.Now()

	log.Infof("Updating spending tx info for %d addresses...", totalVinIbIDs)
	var numAddresses int64
	for i := 0; i < totalVinIbIDs; i += updatesPerDBTx {
		if i%100000 == 0 {
			endRange := i + 100000 - 1
			if endRange > totalVinIbIDs {
				endRange = totalVinIbIDs
			}
			log.Infof("Updating from vins %d to %d...", i, endRange)
		}

		var numAddressRowsSet int64
		endChunk := i + updatesPerDBTx
		if endChunk > totalVinIbIDs {
			endChunk = totalVinIbIDs
		}

		if barLoad != nil {
			// Full mode is definitely running so no need to check.
			timeTakenPerBlock := (time.Since(timeStart).Seconds() / float64(endChunk-i))
			barLoad <- &dbtypes.ProgressBarLoad{
				From:      int64(i),
				To:        int64(totalVinIbIDs),
				Msg:       AddressesSyncStatusMsg,
				BarID:     dbtypes.AddressesTableSync,
				Timestamp: int64(timeTakenPerBlock * float64(totalVinIbIDs-endChunk)),
			}

			timeStart = time.Now()
		}

		_, numAddressRowsSet, err = SetSpendingForVinDbIDs(pgb.db,
			allVinDbIDs[i:endChunk])
		if err != nil {
			log.Errorf("SetSpendingForVinDbIDs: %v", err)
			continue
		}
		numAddresses += numAddressRowsSet
	}

	// Signal the completion of the sync
	if barLoad != nil {
		barLoad <- &dbtypes.ProgressBarLoad{
			From:  int64(totalVinIbIDs),
			To:    int64(totalVinIbIDs),
			Msg:   AddressesSyncStatusMsg,
			BarID: dbtypes.AddressesTableSync,
		}
	}

	return numAddresses, err
}

// UpdateSpendingInfoInAllTickets reviews all votes and revokes and sets this
// spending info in the tickets table.
func (pgb *ChainDB) UpdateSpendingInfoInAllTickets() (int64, error) {
	// Get the full list of votes (DB IDs and heights), and spent ticket hashes
	allVotesDbIDs, allVotesHeights, ticketDbIDs, err :=
		RetrieveAllVotesDbIDsHeightsTicketDbIDs(pgb.db)
	if err != nil {
		log.Errorf("RetrieveAllVotesDbIDsHeightsTicketDbIDs: %v", err)
		return 0, err
	}

	// To update spending info in tickets table, get the spent tickets' DB
	// row IDs and block heights.
	spendTypes := make([]dbtypes.TicketSpendType, len(ticketDbIDs))
	for iv := range ticketDbIDs {
		spendTypes[iv] = dbtypes.TicketVoted
	}
	poolStatuses := ticketpoolStatusSlice(dbtypes.PoolStatusVoted, len(ticketDbIDs))

	// Update tickets table with spending info from new votes
	var totalTicketsUpdated int64
	totalTicketsUpdated, err = SetSpendingForTickets(pgb.db, ticketDbIDs,
		allVotesDbIDs, allVotesHeights, spendTypes, poolStatuses)
	if err != nil {
		log.Warn("SetSpendingForTickets:", err)
	}

	// Revokes

	revokeIDs, _, revokeHeights, vinDbIDs, err := RetrieveAllRevokes(pgb.db)
	if err != nil {
		log.Errorf("RetrieveAllRevokes: %v", err)
		return 0, err
	}

	revokedTicketHashes := make([]string, len(vinDbIDs))
	for i, vinDbID := range vinDbIDs {
		revokedTicketHashes[i], err = RetrieveFundingTxByVinDbID(pgb.db, vinDbID)
		if err != nil {
			log.Errorf("RetrieveFundingTxByVinDbID: %v", err)
			return 0, err
		}
	}

	revokedTicketDbIDs, err := RetrieveTicketIDsByHashes(pgb.db, revokedTicketHashes)
	if err != nil {
		log.Errorf("RetrieveTicketIDsByHashes: %v", err)
		return 0, err
	}

	poolStatuses = ticketpoolStatusSlice(dbtypes.PoolStatusMissed, len(revokedTicketHashes))
	pgb.stakeDB.LockStakeNode()
	for ih := range revokedTicketHashes {
		rh, _ := chainhash.NewHashFromStr(revokedTicketHashes[ih])
		if pgb.stakeDB.BestNode.ExistsExpiredTicket(*rh) {
			poolStatuses[ih] = dbtypes.PoolStatusExpired
		}
	}
	pgb.stakeDB.UnlockStakeNode()

	// To update spending info in tickets table, get the spent tickets' DB
	// row IDs and block heights.
	spendTypes = make([]dbtypes.TicketSpendType, len(revokedTicketDbIDs))
	for iv := range revokedTicketDbIDs {
		spendTypes[iv] = dbtypes.TicketRevoked
	}

	// Update tickets table with spending info from new votes
	var revokedTicketsUpdated int64
	revokedTicketsUpdated, err = SetSpendingForTickets(pgb.db, revokedTicketDbIDs,
		revokeIDs, revokeHeights, spendTypes, poolStatuses)
	if err != nil {
		log.Warn("SetSpendingForTickets:", err)
	}

	return totalTicketsUpdated + revokedTicketsUpdated, err
}

func ticketpoolStatusSlice(ss dbtypes.TicketPoolStatus, N int) []dbtypes.TicketPoolStatus {
	S := make([]dbtypes.TicketPoolStatus, N)
	for ip := range S {
		S[ip] = ss
	}
	return S
}
