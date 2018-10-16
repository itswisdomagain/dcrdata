// Copyright (c) 2017, Jonathan Chappelow
// See LICENSE for details.

package dcrsqlite

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/decred/dcrd/wire"
	apitypes "github.com/decred/dcrdata/v3/api/types"
	"github.com/decred/dcrdata/v3/blockdata"
	"github.com/decred/dcrdata/v3/db/dbtypes"
	"github.com/decred/slog"
	_ "github.com/mattn/go-sqlite3" // register sqlite driver with database/sql
)

// StakeInfoDatabaser is the interface for an extended stake info saving database
type StakeInfoDatabaser interface {
	StoreStakeInfoExtended(bd *apitypes.StakeInfoExtended) error
	RetrieveStakeInfoExtended(ind int64) (*apitypes.StakeInfoExtended, error)
}

// BlockSummaryDatabaser is the interface for a block data saving database
type BlockSummaryDatabaser interface {
	StoreBlockSummary(bd *apitypes.BlockDataBasic) error
	RetrieveBlockSummary(ind int64) (*apitypes.BlockDataBasic, error)
}

// DBInfo contains db configuration
type DBInfo struct {
	FileName string
}

const (
	// TableNameSummaries is name of the table used to store block summary data
	TableNameSummaries = "dcrdata_block_summary"
	// TableNameStakeInfo is name of the table used to store extended stake info
	TableNameStakeInfo = "dcrdata_stakeinfo_extended"
)

// DB is a wrapper around sql.DB that adds methods for storing and retrieving
// chain data. Use InitDB to get a new instance. This may be unexported in the
// future.
type DB struct {
	*sql.DB
	sync.RWMutex
	dbSummaryHeight                                              int64
	dbStakeInfoHeight                                            int64
	getPoolSQL, getPoolRangeSQL, getPoolValSizeRangeSQL          string
	getPoolByHashSQL                                             string
	getWinnersByHashSQL, getWinnersSQL                           string
	getSDiffSQL, getSDiffRangeSQL                                string
	getLatestBlockSQL                                            string
	getBlockSQL, insertBlockSQL                                  string
	getBlockByHashSQL, getBlockByTimeRangeSQL, getBlockByTimeSQL string
	getBlockHashSQL, getBlockHeightSQL                           string
	getBlockSizeRangeSQL                                         string
	getBestBlockHashSQL, getBestBlockHeightSQL                   string
	getLatestStakeInfoExtendedSQL                                string
	getStakeInfoExtendedSQL, insertStakeInfoExtendedSQL          string
	getStakeInfoWinnersSQL                                       string
	getAllPoolValSize                                            string
	getAllFeeInfoPerBlock                                        string
	// returns difficulty in 24hrs or immediately after 24hrs.
	getDifficulty string
}

// NewDB creates a new DB instance with pre-generated sql statements from an
// existing sql.DB. Use InitDB to create a new DB without having a sql.DB.
// TODO: if this db exists, figure out best heights
func NewDB(db *sql.DB) (*DB, error) {
	d := DB{
		DB:                db,
		dbSummaryHeight:   -1,
		dbStakeInfoHeight: -1,
	}

	// Ticket pool queries
	d.getPoolSQL = fmt.Sprintf(`SELECT hash, poolsize, poolval, poolavg, winners`+
		` FROM %s WHERE height = ?`, TableNameSummaries)
	d.getPoolByHashSQL = fmt.Sprintf(`SELECT height, poolsize, poolval, poolavg, winners`+
		` FROM %s WHERE hash = ?`, TableNameSummaries)
	d.getPoolRangeSQL = fmt.Sprintf(`SELECT height, hash, poolsize, poolval, poolavg, winners `+
		`FROM %s WHERE height BETWEEN ? AND ?`, TableNameSummaries)
	d.getPoolValSizeRangeSQL = fmt.Sprintf(`SELECT poolsize, poolval `+
		`FROM %s WHERE height BETWEEN ? AND ?`, TableNameSummaries)
	d.getAllPoolValSize = fmt.Sprintf(`SELECT distinct poolsize, poolval, time `+
		`FROM %s ORDER BY time`, TableNameSummaries)
	d.getWinnersSQL = fmt.Sprintf(`SELECT hash, winners FROM %s WHERE height = ?`,
		TableNameSummaries)
	d.getWinnersByHashSQL = fmt.Sprintf(`SELECT height, winners FROM %s WHERE hash = ?`,
		TableNameSummaries)

	d.getSDiffSQL = fmt.Sprintf(`SELECT sdiff FROM %s WHERE height = ?`,
		TableNameSummaries)
	d.getDifficulty = fmt.Sprintf(`SELECT diff FROM %s WHERE time >= ? ORDER BY time LIMIT 1`,
		TableNameSummaries)
	d.getSDiffRangeSQL = fmt.Sprintf(`SELECT sdiff FROM %s WHERE height BETWEEN ? AND ?`,
		TableNameSummaries)

	// Block queries
	d.getBlockSQL = fmt.Sprintf(`SELECT * FROM %s WHERE height = ?`, TableNameSummaries)
	d.getBlockByHashSQL = fmt.Sprintf(`SELECT * FROM %s WHERE hash = ?`, TableNameSummaries)
	d.getLatestBlockSQL = fmt.Sprintf(`SELECT * FROM %s ORDER BY height DESC LIMIT 0, 1`,
		TableNameSummaries)
	d.insertBlockSQL = fmt.Sprintf(`
        INSERT OR REPLACE INTO %s(
            height, size, hash, diff, sdiff, time, poolsize, poolval, poolavg, winners
        ) values(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, TableNameSummaries)

	d.getBlockSizeRangeSQL = fmt.Sprintf(`SELECT size FROM %s WHERE height BETWEEN ? AND ?`,
		TableNameSummaries)
	d.getBlockByTimeRangeSQL = fmt.Sprintf(`SELECT * FROM %s WHERE time BETWEEN ? AND ? ORDER BY time LIMIT ?`,
		TableNameSummaries)
	d.getBlockByTimeSQL = fmt.Sprintf(`SELECT * FROM %s WHERE time = ?`,
		TableNameSummaries)

	d.getBestBlockHashSQL = fmt.Sprintf(`SELECT hash FROM %s ORDER BY height DESC LIMIT 0, 1`, TableNameSummaries)
	d.getBestBlockHeightSQL = fmt.Sprintf(`SELECT height FROM %s ORDER BY height DESC LIMIT 0, 1`, TableNameSummaries)

	d.getBlockHashSQL = fmt.Sprintf(`SELECT hash FROM %s WHERE height = ?`, TableNameSummaries)
	d.getBlockHeightSQL = fmt.Sprintf(`SELECT height FROM %s WHERE hash = ?`, TableNameSummaries)

	// Stake info queries
	d.getStakeInfoExtendedSQL = fmt.Sprintf(`SELECT * FROM %s WHERE height = ?`,
		TableNameStakeInfo)
	d.getStakeInfoWinnersSQL = fmt.Sprintf(`SELECT winners FROM %s WHERE height = ?`,
		TableNameStakeInfo)
	d.getLatestStakeInfoExtendedSQL = fmt.Sprintf(
		`SELECT * FROM %s ORDER BY height DESC LIMIT 0, 1`, TableNameStakeInfo)
	d.insertStakeInfoExtendedSQL = fmt.Sprintf(`
        INSERT OR REPLACE INTO %s(
            height, num_tickets, fee_min, fee_max, fee_mean, fee_med, fee_std,
			sdiff, window_num, window_ind, pool_size, pool_val, pool_valavg, winners
        ) values(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, TableNameStakeInfo)

	d.getAllFeeInfoPerBlock = fmt.Sprintf(`SELECT distinct height, fee_med FROM %s ORDER BY height;`, TableNameStakeInfo)

	var err error
	if d.dbSummaryHeight, err = d.GetBlockSummaryHeight(); err != nil {
		return nil, err
	}
	if d.dbStakeInfoHeight, err = d.GetStakeInfoHeight(); err != nil {
		return nil, err
	}

	return &d, nil
}

// InitDB creates a new DB instance from a DBInfo containing the name of the
// file used to back the underlying sql database.
func InitDB(dbInfo *DBInfo) (*DB, error) {
	dbPath, err := filepath.Abs(dbInfo.FileName)
	if err != nil {
		return nil, err
	}

	// Ensures target DB-file has a parent folder
	parent := filepath.Dir(dbPath)
	err = os.MkdirAll(parent, 0755)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil || db == nil {
		return nil, err
	}

	createBlockSummaryStmt := fmt.Sprintf(`
        PRAGMA cache_size = 32768;
        pragma synchronous = OFF;
        create table if not exists %s(
            height INTEGER PRIMARY KEY,
            size INTEGER,
            hash TEXT,
            diff FLOAT,
            sdiff FLOAT,
            time INTEGER,
            poolsize INTEGER,
            poolval FLOAT,
			poolavg FLOAT,
			winners TEXT
        );
        `, TableNameSummaries)

	_, err = db.Exec(createBlockSummaryStmt)
	if err != nil {
		log.Errorf("%q: %s\n", err, createBlockSummaryStmt)
		return nil, err
	}

	createStakeInfoExtendedStmt := fmt.Sprintf(`
        PRAGMA cache_size = 32768;
        pragma synchronous = OFF;
        create table if not exists %s(
            height INTEGER PRIMARY KEY,
            num_tickets INTEGER,
            fee_min FLOAT, fee_max FLOAT, fee_mean FLOAT,
			fee_med FLOAT, fee_std FLOAT,
			sdiff FLOAT, window_num INTEGER, window_ind INTEGER,
			pool_size INTEGER, pool_val FLOAT, pool_valavg FLOAT,
			winners TEXT
        );
        `, TableNameStakeInfo)

	_, err = db.Exec(createStakeInfoExtendedStmt)
	if err != nil {
		log.Errorf("%q: %s\n", err, createStakeInfoExtendedStmt)
		return nil, err
	}

	if err = db.Ping(); err != nil {
		return nil, err
	}

	dataBase, err := NewDB(db)
	return dataBase, err
}

// DBDataSaver models a DB with a channel to communicate new block height to the
// web interface.
type DBDataSaver struct {
	*DB
	updateStatusChan chan uint32
}

// Store satisfies the blockdata.BlockDataSaver interface.
func (db *DBDataSaver) Store(data *blockdata.BlockData, _ *wire.MsgBlock) error {
	summary := data.ToBlockSummary()
	err := db.DB.StoreBlockSummary(&summary)
	if err != nil {
		return err
	}

	select {
	case db.updateStatusChan <- summary.Height:
	default:
	}

	stakeInfoExtended := data.ToStakeInfoExtended()
	return db.DB.StoreStakeInfoExtended(&stakeInfoExtended)
}

// StoreBlockSummary attempts to store the block data in the database, and
// returns an error on failure.
func (db *DB) StoreBlockSummary(bd *apitypes.BlockDataBasic) error {
	stmt, err := db.Prepare(db.insertBlockSQL)
	if err != nil {
		return err
	}
	defer stmt.Close()

	// If input block data lacks non-nil PoolInfo, set to a zero-value
	// TicketPoolInfo.
	if bd.PoolInfo == nil {
		bd.PoolInfo = new(apitypes.TicketPoolInfo)
	}

	// Insert the block.
	winners := strings.Join(bd.PoolInfo.Winners, ";")

	res, err := stmt.Exec(&bd.Height, &bd.Size, &bd.Hash,
		&bd.Difficulty, &bd.StakeDiff, &bd.Time,
		&bd.PoolInfo.Size, &bd.PoolInfo.Value, &bd.PoolInfo.ValAvg,
		&winners)
	if err != nil {
		return err
	}

	// Update the DB block summary height.
	db.Lock()
	defer db.Unlock()
	if err = logDBResult(res); err == nil {
		// TODO: atomic with CAS
		height := int64(bd.Height)
		if height > db.dbSummaryHeight {
			db.dbSummaryHeight = height
		}
	}

	return err
}

// GetBestBlockHash returns the hash of the best block
func (db *DB) GetBestBlockHash() string {
	hash, err := db.RetrieveBestBlockHash()
	if err != nil {
		log.Errorf("RetrieveBestBlockHash failed: %v", err)
		return ""
	}
	return hash
}

// GetBestBlockHeight returns the height of the best block
func (db *DB) GetBestBlockHeight() int64 {
	h, _ := db.GetBlockSummaryHeight()
	return h
}

// GetBlockSummaryHeight returns the largest block height for which the database
// can provide a block summary
func (db *DB) GetBlockSummaryHeight() (int64, error) {
	db.RLock()
	defer db.RUnlock()
	if db.dbSummaryHeight < 0 {
		height, err := db.RetrieveBestBlockHeight()
		// No rows returned is not considered an error
		if err != nil && err != sql.ErrNoRows {
			return -1, fmt.Errorf("RetrieveBestBlockHeight failed: %v", err)
		}
		if err == sql.ErrNoRows {
			log.Warn("Block summary DB is empty.")
		} else {
			db.dbSummaryHeight = height
		}
	}
	return db.dbSummaryHeight, nil
}

// GetStakeInfoHeight returns the largest block height for which the database
// can provide a stake info
func (db *DB) GetStakeInfoHeight() (int64, error) {
	db.RLock()
	defer db.RUnlock()
	if db.dbStakeInfoHeight < 0 {
		si, err := db.RetrieveLatestStakeInfoExtended()
		// No rows returned is not considered an error
		if err != nil && err != sql.ErrNoRows {
			return -1, fmt.Errorf("RetrieveLatestStakeInfoExtended failed: %v", err)
		}
		if err == sql.ErrNoRows {
			log.Warn("Stake info DB is empty.")
			return -1, nil
		}
		db.dbStakeInfoHeight = int64(si.Feeinfo.Height)
	}
	return db.dbStakeInfoHeight, nil
}

// RetrievePoolInfoRange returns an array of apitypes.TicketPoolInfo for block
// range ind0 to ind1 and a non-nil error on success
func (db *DB) RetrievePoolInfoRange(ind0, ind1 int64) ([]apitypes.TicketPoolInfo, []string, error) {
	N := ind1 - ind0 + 1
	if N == 0 {
		return []apitypes.TicketPoolInfo{}, []string{}, nil
	}
	if N < 0 {
		return nil, nil, fmt.Errorf("Cannot retrieve pool info range (%d>%d)",
			ind0, ind1)
	}
	db.RLock()
	if ind1 > db.dbSummaryHeight || ind0 < 0 {
		defer db.RUnlock()
		return nil, nil, fmt.Errorf("Cannot retrieve pool info range [%d,%d], have height %d",
			ind0, ind1, db.dbSummaryHeight)
	}
	db.RUnlock()

	tpis := make([]apitypes.TicketPoolInfo, 0, N)
	hashes := make([]string, 0, N)

	stmt, err := db.Prepare(db.getPoolRangeSQL)
	if err != nil {
		return nil, nil, err
	}
	defer stmt.Close()

	rows, err := stmt.Query(ind0, ind1)
	if err != nil {
		log.Errorf("Query failed: %v", err)
		return nil, nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var tpi apitypes.TicketPoolInfo
		var hash, winners string
		if err = rows.Scan(&tpi.Height, &hash, &tpi.Size, &tpi.Value,
			&tpi.ValAvg, &winners); err != nil {
			log.Errorf("Unable to scan for TicketPoolInfo fields: %v", err)
		}
		tpi.Winners = splitToArray(winners)
		tpis = append(tpis, tpi)
		hashes = append(hashes, hash)
	}
	if err = rows.Err(); err != nil {
		log.Error(err)
	}

	return tpis, hashes, nil
}

// RetrievePoolInfo returns ticket pool info for block height ind
func (db *DB) RetrievePoolInfo(ind int64) (*apitypes.TicketPoolInfo, error) {
	tpi := &apitypes.TicketPoolInfo{
		Height: uint32(ind),
	}
	var hash, winners string
	err := db.QueryRow(db.getPoolSQL, ind).Scan(&hash, &tpi.Size,
		&tpi.Value, &tpi.ValAvg, &winners)
	tpi.Winners = splitToArray(winners)
	return tpi, err
}

// RetrieveWinners returns the winning ticket tx IDs drawn after connecting the
// given block height (called to validate the block). The block hash
// corresponding to the input block height is also returned.
func (db *DB) RetrieveWinners(ind int64) ([]string, string, error) {
	var hash, winners string
	err := db.QueryRow(db.getWinnersSQL, ind).Scan(&hash, &winners)
	if err != nil {
		return nil, "", err
	}
	return splitToArray(winners), hash, err
}

// RetrieveWinnersByHash returns the winning ticket tx IDs drawn after
// connecting the block with the given hash. The block height corresponding to
// the input block hash is also returned.
func (db *DB) RetrieveWinnersByHash(hash string) ([]string, uint32, error) {
	var winners string
	var height uint32
	err := db.QueryRow(db.getWinnersByHashSQL, hash).Scan(&height, &winners)
	if err != nil {
		return nil, 0, err
	}
	return splitToArray(winners), height, err
}

// RetrievePoolInfoByHash returns ticket pool info for blockhash hash
func (db *DB) RetrievePoolInfoByHash(hash string) (*apitypes.TicketPoolInfo, error) {
	tpi := new(apitypes.TicketPoolInfo)
	var winners string
	err := db.QueryRow(db.getPoolByHashSQL, hash).Scan(&tpi.Height, &tpi.Size,
		&tpi.Value, &tpi.ValAvg, &winners)
	tpi.Winners = splitToArray(winners)
	return tpi, err
}

// RetrievePoolValAndSizeRange returns an array each of the pool values and sizes
// for block range ind0 to ind1
func (db *DB) RetrievePoolValAndSizeRange(ind0, ind1 int64) ([]float64, []float64, error) {
	N := ind1 - ind0 + 1
	if N == 0 {
		return []float64{}, []float64{}, nil
	}
	if N < 0 {
		return nil, nil, fmt.Errorf("Cannot retrieve pool val and size range (%d>%d)",
			ind0, ind1)
	}
	db.RLock()
	if ind1 > db.dbSummaryHeight || ind0 < 0 {
		defer db.RUnlock()
		return nil, nil, fmt.Errorf("Cannot retrieve pool val and size range [%d,%d], have height %d",
			ind0, ind1, db.dbSummaryHeight)
	}
	db.RUnlock()

	poolvals := make([]float64, 0, N)
	poolsizes := make([]float64, 0, N)

	stmt, err := db.Prepare(db.getPoolValSizeRangeSQL)
	if err != nil {
		return nil, nil, err
	}
	defer stmt.Close()

	rows, err := stmt.Query(ind0, ind1)
	if err != nil {
		log.Errorf("Query failed: %v", err)
		return nil, nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var pval, psize float64
		if err = rows.Scan(&psize, &pval); err != nil {
			log.Errorf("Unable to scan for TicketPoolInfo fields: %v", err)
		}
		poolvals = append(poolvals, pval)
		poolsizes = append(poolsizes, psize)
	}
	if err = rows.Err(); err != nil {
		log.Error(err)
	}

	if len(poolsizes) != int(N) {
		log.Warnf("Retrieved pool values (%d) not expected number (%d)", len(poolsizes), N)
	}

	return poolvals, poolsizes, nil
}

// RetrieveAllPoolValAndSize returns all the pool values and sizes stored since
// the first value was recorded up current height.
func (db *DB) RetrieveAllPoolValAndSize() (*dbtypes.ChartsData, error) {
	db.RLock()
	defer db.RUnlock()

	var chartsData = new(dbtypes.ChartsData)
	var stmt, err = db.Prepare(db.getAllPoolValSize)
	if err != nil {
		return chartsData, err
	}
	defer stmt.Close()

	rows, err := stmt.Query()
	if err != nil {
		log.Errorf("Query failed: %v", err)
		return chartsData, err
	}
	defer rows.Close()

	for rows.Next() {
		var pval, psize float64
		var timestamp uint64
		if err = rows.Scan(&psize, &pval, &timestamp); err != nil {
			log.Errorf("Unable to scan for TicketPoolInfo fields: %v", err)
		}
		chartsData.Time = append(chartsData.Time, timestamp)
		chartsData.SizeF = append(chartsData.SizeF, psize)
		chartsData.ValueF = append(chartsData.ValueF, pval)
	}
	if err = rows.Err(); err != nil {
		log.Error(err)
	}

	if len(chartsData.Time) < 1 {
		log.Warnf("Retrieved pool values (%d) not expected number (%d)", len(chartsData.Time), 1)
	}

	return chartsData, nil
}

// RetrieveBlockFeeInfo fetches the block median fee chart data.
func (db *DB) RetrieveBlockFeeInfo() (*dbtypes.ChartsData, error) {
	db.RLock()
	defer db.RUnlock()

	var chartsData = new(dbtypes.ChartsData)
	var stmt, err = db.Prepare(db.getAllFeeInfoPerBlock)
	if err != nil {
		return chartsData, err
	}
	defer stmt.Close()

	rows, err := stmt.Query()
	if err != nil {
		log.Errorf("Query failed: %v", err)
		return chartsData, err
	}
	defer rows.Close()

	for rows.Next() {
		var feeMed float64
		var height uint64
		if err = rows.Scan(&height, &feeMed); err != nil {
			log.Errorf("Unable to scan for FeeInfoPerBlock fields: %v", err)
		}
		if height == 0 && feeMed == 0 {
			continue
		}

		chartsData.Count = append(chartsData.Count, height)
		chartsData.SizeF = append(chartsData.SizeF, feeMed)
	}
	if err = rows.Err(); err != nil {
		log.Error(err)
	}

	if len(chartsData.Count) < 1 {
		log.Warnf("Retrieved pool values (%d) not expected number (%d)", len(chartsData.Count), 1)
	}

	return chartsData, nil
}

// RetrieveSDiffRange returns an array of stake difficulties for block range ind0 to
// ind1
func (db *DB) RetrieveSDiffRange(ind0, ind1 int64) ([]float64, error) {
	N := ind1 - ind0 + 1
	if N == 0 {
		return []float64{}, nil
	}
	if N < 0 {
		return nil, fmt.Errorf("Cannot retrieve sdiff range (%d>%d)",
			ind0, ind1)
	}
	db.RLock()
	if ind1 > db.dbSummaryHeight || ind0 < 0 {
		defer db.RUnlock()
		return nil, fmt.Errorf("Cannot retrieve sdiff range [%d,%d], have height %d",
			ind0, ind1, db.dbSummaryHeight)
	}
	db.RUnlock()

	sdiffs := make([]float64, 0, N)

	stmt, err := db.Prepare(db.getSDiffRangeSQL)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	rows, err := stmt.Query(ind0, ind1)
	if err != nil {
		log.Errorf("Query failed: %v", err)
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var sdiff float64
		if err = rows.Scan(&sdiff); err != nil {
			log.Errorf("Unable to scan for sdiff fields: %v", err)
		}
		sdiffs = append(sdiffs, sdiff)
	}
	if err = rows.Err(); err != nil {
		log.Error(err)
	}

	return sdiffs, nil
}

func (db *DB) RetrieveBlockSummaryByTimeRange(minTime, maxTime int64, limit int) ([]apitypes.BlockDataBasic, error) {
	blocks := make([]apitypes.BlockDataBasic, 0, limit)

	stmt, err := db.Prepare(db.getBlockByTimeRangeSQL)
	if err != nil {
		return nil, err
	}

	rows, err := stmt.Query(minTime, maxTime, limit)

	if err != nil {
		log.Errorf("Query failed: %v", err)
		return nil, err
	}

	defer rows.Close()

	for rows.Next() {
		bd := apitypes.NewBlockDataBasic()
		if err = rows.Scan(&bd.Height, &bd.Size, &bd.Hash,
			&bd.Difficulty, &bd.StakeDiff, &bd.Time,
			&bd.PoolInfo.Size, &bd.PoolInfo.Value, &bd.PoolInfo.ValAvg); err != nil {
			log.Errorf("Unable to scan for block fields")
		}
		blocks = append(blocks, *bd)
	}
	if err = rows.Err(); err != nil {
		log.Error(err)
	}
	return blocks, nil
}

// RetrieveDiff returns the difficulty in the last 24hrs or immediately after 24hrs.
func (db *DB) RetrieveDiff(timestamp int64) (float64, error) {
	var diff float64
	err := db.QueryRow(db.getDifficulty, timestamp).Scan(&diff)
	return diff, err
}

// RetrieveSDiff returns the stake difficulty for block at the specified chain height.
func (db *DB) RetrieveSDiff(ind int64) (float64, error) {
	var sdiff float64
	err := db.QueryRow(db.getSDiffSQL, ind).Scan(&sdiff)
	return sdiff, err
}

// RetrieveLatestBlockSummary returns the block summary for the best block
func (db *DB) RetrieveLatestBlockSummary() (*apitypes.BlockDataBasic, error) {
	bd := apitypes.NewBlockDataBasic()

	var winners string
	err := db.QueryRow(db.getLatestBlockSQL).Scan(&bd.Height, &bd.Size,
		&bd.Hash, &bd.Difficulty, &bd.StakeDiff, &bd.Time,
		&bd.PoolInfo.Size, &bd.PoolInfo.Value, &bd.PoolInfo.ValAvg,
		&winners)
	if err != nil {
		return nil, err
	}
	bd.PoolInfo.Winners = splitToArray(winners)
	return bd, nil
}

// RetrieveBlockHash returns the block hash for block ind
func (db *DB) RetrieveBlockHash(ind int64) (string, error) {
	var blockHash string
	err := db.QueryRow(db.getBlockHashSQL, ind).Scan(&blockHash)
	return blockHash, err
}

// RetrieveBlockHeight returns the block height for blockhash hash
func (db *DB) RetrieveBlockHeight(hash string) (int64, error) {
	var blockHeight int64
	err := db.QueryRow(db.getBlockHeightSQL, hash).Scan(&blockHeight)
	return blockHeight, err
}

// RetrieveBestBlockHash returns the block hash for the best block
func (db *DB) RetrieveBestBlockHash() (string, error) {
	var blockHash string
	err := db.QueryRow(db.getBestBlockHashSQL).Scan(&blockHash)
	return blockHash, err
}

// RetrieveBestBlockHeight returns the block height for the best block
func (db *DB) RetrieveBestBlockHeight() (int64, error) {
	var blockHeight int64
	err := db.QueryRow(db.getBestBlockHeightSQL).Scan(&blockHeight)
	return blockHeight, err
}

// RetrieveBlockSummaryByHash returns basic block data for a block given its hash
func (db *DB) RetrieveBlockSummaryByHash(hash string) (*apitypes.BlockDataBasic, error) {
	bd := apitypes.NewBlockDataBasic()

	var winners string
	err := db.QueryRow(db.getBlockByHashSQL, hash).Scan(&bd.Height, &bd.Size, &bd.Hash,
		&bd.Difficulty, &bd.StakeDiff, &bd.Time,
		&bd.PoolInfo.Size, &bd.PoolInfo.Value, &bd.PoolInfo.ValAvg,
		&winners)
	if err != nil {
		return nil, err
	}
	bd.PoolInfo.Winners = splitToArray(winners)
	return bd, nil
}

// RetrieveBlockSummary returns basic block data for block ind
func (db *DB) RetrieveBlockSummary(ind int64) (*apitypes.BlockDataBasic, error) {
	bd := apitypes.NewBlockDataBasic()

	// Three different ways

	// 1. chained QueryRow/Scan only
	var winners string
	err := db.QueryRow(db.getBlockSQL, ind).Scan(&bd.Height, &bd.Size, &bd.Hash,
		&bd.Difficulty, &bd.StakeDiff, &bd.Time,
		&bd.PoolInfo.Size, &bd.PoolInfo.Value, &bd.PoolInfo.ValAvg,
		&winners)
	if err != nil {
		return nil, err
	}
	bd.PoolInfo.Winners = splitToArray(winners)
	// 2. Prepare + chained QueryRow/Scan
	// stmt, err := db.Prepare(getBlockSQL)
	// if err != nil {
	//     return nil, err
	// }
	// defer stmt.Close()

	// err = stmt.QueryRow(ind).Scan(&bd.Height, &bd.Size, &bd.Hash, &bd.Difficulty,
	//     &bd.StakeDiff, &bd.Time, &bd.PoolInfo.Size, &bd.PoolInfo.Value,
	//     &bd.PoolInfo.ValAvg)
	// if err != nil {
	//     return nil, err
	// }

	// 3. Prepare + Query + Scan
	// rows, err := stmt.Query(ind)
	// if err != nil {
	//     log.Errorf("Query failed: %v", err)
	//     return nil, err
	// }
	// defer rows.Close()

	// if rows.Next() {
	//     err = rows.Scan(&bd.Height, &bd.Size, &bd.Hash, &bd.Difficulty, &bd.StakeDiff,
	//         &bd.Time, &bd.PoolInfo.Size, &bd.PoolInfo.Value, &bd.PoolInfo.ValAvg)
	//     if err != nil {
	//         log.Errorf("Unable to scan for BlockDataBasic fields: %v", err)
	//     }
	// }
	// if err = rows.Err(); err != nil {
	//     log.Error(err)
	// }

	return bd, nil
}

// RetrieveBlockSizeRange returns an array of block sizes for block range ind0 to ind1
func (db *DB) RetrieveBlockSizeRange(ind0, ind1 int64) ([]int32, error) {
	N := ind1 - ind0 + 1
	if N == 0 {
		return []int32{}, nil
	}
	if N < 0 {
		return nil, fmt.Errorf("Cannot retrieve block size range (%d>%d)",
			ind0, ind1)
	}
	db.RLock()
	if ind1 > db.dbSummaryHeight || ind0 < 0 {
		defer db.RUnlock()
		return nil, fmt.Errorf("Cannot retrieve block size range [%d,%d], have height %d",
			ind0, ind1, db.dbSummaryHeight)
	}
	db.RUnlock()

	blockSizes := make([]int32, 0, N)

	stmt, err := db.Prepare(db.getBlockSizeRangeSQL)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	rows, err := stmt.Query(ind0, ind1)
	if err != nil {
		log.Errorf("Query failed: %v", err)
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var blockSize int32
		if err = rows.Scan(&blockSize); err != nil {
			log.Errorf("Unable to scan for sdiff fields: %v", err)
		}
		blockSizes = append(blockSizes, blockSize)
	}
	if err = rows.Err(); err != nil {
		log.Error(err)
	}

	return blockSizes, nil
}

// StoreStakeInfoExtended stores the extended stake info in the database.
func (db *DB) StoreStakeInfoExtended(si *apitypes.StakeInfoExtended) error {
	stmt, err := db.Prepare(db.insertStakeInfoExtendedSQL)
	if err != nil {
		return err
	}
	defer stmt.Close()

	// If input block data lacks non-nil PoolInfo, set to a zero-value
	// TicketPoolInfo.
	if si.PoolInfo == nil {
		si.PoolInfo = new(apitypes.TicketPoolInfo)
	}

	winners := strings.Join(si.PoolInfo.Winners, ";")

	res, err := stmt.Exec(&si.Feeinfo.Height,
		&si.Feeinfo.Number, &si.Feeinfo.Min, &si.Feeinfo.Max, &si.Feeinfo.Mean,
		&si.Feeinfo.Median, &si.Feeinfo.StdDev,
		&si.StakeDiff, // no next or estimates
		&si.PriceWindowNum, &si.IdxBlockInWindow, &si.PoolInfo.Size,
		&si.PoolInfo.Value, &si.PoolInfo.ValAvg, &winners)
	if err != nil {
		return err
	}

	db.Lock()
	defer db.Unlock()
	if err = logDBResult(res); err == nil {
		height := int64(si.Feeinfo.Height)
		if height > db.dbStakeInfoHeight {
			db.dbStakeInfoHeight = height
		}
	}
	return err
}

// RetrieveLatestStakeInfoExtended returns the extended stake info for the best
// block.
func (db *DB) RetrieveLatestStakeInfoExtended() (*apitypes.StakeInfoExtended, error) {
	si := apitypes.NewStakeInfoExtended()

	var winners string
	err := db.QueryRow(db.getLatestStakeInfoExtendedSQL).Scan(
		&si.Feeinfo.Height, &si.Feeinfo.Number, &si.Feeinfo.Min,
		&si.Feeinfo.Max, &si.Feeinfo.Mean,
		&si.Feeinfo.Median, &si.Feeinfo.StdDev,
		&si.StakeDiff, // no next or estimates
		&si.PriceWindowNum, &si.IdxBlockInWindow, &si.PoolInfo.Size,
		&si.PoolInfo.Value, &si.PoolInfo.ValAvg, &winners)
	if err != nil {
		return nil, err
	}
	si.PoolInfo.Winners = splitToArray(winners)
	return si, nil
}

// RetrieveStakeInfoExtended returns the extended stake info for the block at
// height ind.
func (db *DB) RetrieveStakeInfoExtended(ind int64) (*apitypes.StakeInfoExtended, error) {
	si := apitypes.NewStakeInfoExtended()

	var winners string
	err := db.QueryRow(db.getStakeInfoExtendedSQL, ind).Scan(&si.Feeinfo.Height,
		&si.Feeinfo.Number, &si.Feeinfo.Min, &si.Feeinfo.Max, &si.Feeinfo.Mean,
		&si.Feeinfo.Median, &si.Feeinfo.StdDev,
		&si.StakeDiff, // no next or estimates
		&si.PriceWindowNum, &si.IdxBlockInWindow, &si.PoolInfo.Size,
		&si.PoolInfo.Value, &si.PoolInfo.ValAvg, &winners)
	if err != nil {
		return nil, err
	}
	si.PoolInfo.Winners = splitToArray(winners)
	return si, nil
}

func logDBResult(res sql.Result) error {
	if log.Level() > slog.LevelTrace {
		return nil
	}

	lastID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	rowCnt, err := res.RowsAffected()
	if err != nil {
		return err
	}

	log.Tracef("ID = %d, affected = %d", lastID, rowCnt)

	return nil
}

// splitToArray splits a string into multiple strings using ";" to delimit.
func splitToArray(str string) []string {
	if str == "" {
		// Return a non-nil empty slice.
		return []string{}
	}
	return strings.Split(str, ";")
}
