// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tabletserver

import (
	"encoding/binary"
	"strconv"
	"time"

	log "github.com/ngaut/logging"

	"github.com/wandoulabs/cm/mysql"
	"github.com/youtube/vitess/go/stats"
)

var cacheStats = stats.NewTimings("Rowcache")

var pack = binary.BigEndian

const (
	RC_DELETED = 1

	// MAX_KEY_LEN is a value less than memcache's limit of 250.
	MAX_KEY_LEN = 200

	// MAX_DATA_LEN prevents large rows from being inserted in rowcache.
	MAX_DATA_LEN = 8000
)

type RowCache struct {
	tableInfo *TableInfo
	prefix    string
	cachePool *CachePool
}

type RCResult struct {
	Row mysql.RowValue
	Cas uint64
}

func NewRowCache(tableInfo *TableInfo, cachePool *CachePool) *RowCache {
	prefix := strconv.FormatInt(cachePool.maxPrefix.Add(1), 36) + "."
	return &RowCache{tableInfo, prefix, cachePool}
}

func (rc *RowCache) Get(keys []string) (results map[string]RCResult) {
	mkeys := make([]string, 0, len(keys))
	for _, key := range keys {
		if len(key) > MAX_KEY_LEN {
			continue
		}
		mkeys = append(mkeys, rc.prefix+key)
	}
	prefixlen := len(rc.prefix)
	conn := rc.cachePool.Get(0)
	// This is not the same as defer rc.cachePool.Put(conn)
	defer func() { rc.cachePool.Put(conn) }()

	defer cacheStats.Record("Exec", time.Now())
	mcresults, err := conn.Gets(mkeys...)
	if err != nil {
		conn.Close()
		conn = nil
		log.Fatalf("%s", err)
	}
	results = make(map[string]RCResult, len(mkeys))
	for _, mcresult := range mcresults {
		if mcresult.Flags == RC_DELETED {
			// The row was recently invalidated.
			// If the caller reads the row from db, they can update it
			// back as long as it's not updated again.
			results[mcresult.Key[prefixlen:]] = RCResult{Cas: mcresult.Cas}
			continue
		}
		row := rc.decodeRow(mcresult.Value)
		if row == nil {
			log.Fatalf("Corrupt data for %s", mcresult.Key)
		}
		results[mcresult.Key[prefixlen:]] = RCResult{Row: row, Cas: mcresult.Cas}
	}
	return
}

func (rc *RowCache) Set(key string, row mysql.RowValue, cas uint64) {
	if len(key) > MAX_KEY_LEN {
		return
	}
	b := rc.encodeRow(row)
	if b == nil {
		return
	}
	conn := rc.cachePool.Get(0)
	defer func() { rc.cachePool.Put(conn) }()
	mkey := rc.prefix + key

	var err error
	if cas == 0 {
		// Either caller didn't find the value at all
		// or they didn't look for it in the first place.
		_, err = conn.Add(mkey, 0, 0, b)
	} else {
		// Caller is trying to update a row that recently changed.
		_, err = conn.Cas(mkey, 0, 0, b, cas)
	}
	if err != nil {
		conn.Close()
		conn = nil
		log.Fatalf("%s", err)
	}
}

func (rc *RowCache) Delete(key string) {
	if len(key) > MAX_KEY_LEN {
		return
	}
	conn := rc.cachePool.Get(0)
	defer func() { rc.cachePool.Put(conn) }()
	mkey := rc.prefix + key

	_, err := conn.Set(mkey, RC_DELETED, rc.cachePool.DeleteExpiry, nil)
	if err != nil {
		conn.Close()
		conn = nil
		log.Fatalf("%s", err)
	}
}

func (rc *RowCache) encodeRow(row mysql.RowValue) (b []byte) {
	length := 0
	for _, v := range row {
		raw, err := mysql.Raw(v)
		if err != nil {
			log.Error(err)
			return nil
		}
		length += len(raw)
		if length > MAX_DATA_LEN {
			return nil
		}
	}
	datastart := 4 + len(row)*4
	b = make([]byte, datastart+length)
	data := b[datastart:datastart]
	pack.PutUint32(b, uint32(len(row)))
	for i, v := range row {
		raw, _ := mysql.Raw(v)

		if raw == nil {
			pack.PutUint32(b[4+i*4:], 0xFFFFFFFF)
			continue
		}
		data = append(data, raw...)
		pack.PutUint32(b[4+i*4:], uint32(len(raw)))
	}
	return b
}

func (rc *RowCache) decodeRow(b []byte) (row mysql.RowValue) {
	rowlen := pack.Uint32(b)
	data := b[4+rowlen*4:]
	row = mysql.RowValue(make([]mysql.Value, rowlen))
	for i := range row {
		length := pack.Uint32(b[4+i*4:])
		if length == 0xFFFFFFFF {
			continue
		}
		if length > uint32(len(data)) {
			// Corrupt data
			return nil
		}

		row[i], _ = mysql.DecodeRaw(data[:length], rc.tableInfo.Columns[i].Category)
		data = data[length:]
	}
	return row
}
