// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tabletserver

import (
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/ngaut/logging"
	"github.com/ngaut/memcache"
	"github.com/ngaut/pools"
	"github.com/ngaut/sync2"
)

const statsURL = "/debug/memcache/"

type CreateCacheFunc func() (*memcache.Connection, error)

//todo: copy from vitess
type RowCacheConfig struct {
	Binary      string `json:"binary"`
	Memory      int    `json:"mem"`
	Socket      string `json:"socket"`
	TcpPort     int    `json:"port"`
	Connections int    `json:"connections"`
	Threads     int    `json:"threads"`
	LockPaged   bool   `json:"lock_paged"`
}

func (c *RowCacheConfig) GetSubprocessFlags() []string {
	cmd := []string{}
	if c.Binary == "" {
		return cmd
	}
	cmd = append(cmd, c.Binary)
	if c.Memory > 0 {
		// memory is given in bytes and rowcache expects in MBs
		cmd = append(cmd, "-m", strconv.Itoa(c.Memory))
	}
	if c.Socket != "" {
		cmd = append(cmd, "-s", c.Socket)
	}
	if c.TcpPort > 0 {
		cmd = append(cmd, "-p", strconv.Itoa(c.TcpPort))
	}
	if c.Connections > 0 {
		cmd = append(cmd, "-c", strconv.Itoa(c.Connections))
	}
	if c.Threads > 0 {
		cmd = append(cmd, "-t", strconv.Itoa(c.Threads))
	}
	if c.LockPaged {
		cmd = append(cmd, "-k")
	}
	return cmd
}

var maxPrefix sync2.AtomicInt64

func GetMaxPrefix() int64 {
	return maxPrefix.Add(1)
}

type CachePool struct {
	name           string
	pool           *pools.ResourcePool
	cmd            *exec.Cmd
	rowCacheConfig RowCacheConfig
	capacity       int
	port           string
	idleTimeout    time.Duration
	DeleteExpiry   uint64
	memcacheStats  *MemcacheStats
	mu             sync.Mutex
}

func NewCachePool(name string, rowCacheConfig RowCacheConfig, queryTimeout time.Duration, idleTimeout time.Duration) *CachePool {
	cp := &CachePool{name: name, idleTimeout: idleTimeout}
	if rowCacheConfig.Binary == "" {
		return cp
	}
	cp.rowCacheConfig = rowCacheConfig

	// Start with memcached defaults
	cp.capacity = 1024 - 50
	cp.port = "11211"
	if rowCacheConfig.Socket != "" {
		cp.port = rowCacheConfig.Socket
	}

	if rowCacheConfig.TcpPort > 0 {
		//liuqi: missing ":" in origin code
		cp.port = ":" + strconv.Itoa(rowCacheConfig.TcpPort)
	}

	if rowCacheConfig.Connections > 0 {
		if rowCacheConfig.Connections <= 50 {
			log.Fatalf("insufficient capacity: %d", rowCacheConfig.Connections)
		}
		cp.capacity = rowCacheConfig.Connections - 50
	}

	seconds := uint64(queryTimeout / time.Second)
	// Add an additional grace period for
	// memcache expiry of deleted items
	if seconds != 0 {
		cp.DeleteExpiry = 2*seconds + 15
	}
	return cp
}

func (cp *CachePool) Open() {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.pool != nil {
		panic("rowcache is already open")
	}
	if cp.rowCacheConfig.Binary == "" {
		panic("rowcache binary not specified")
	}
	cp.startMemcache()
	log.Infof("rowcache is enabled")
	f := func() (pools.Resource, error) {
		return memcache.Connect(cp.port, 10*time.Second)
	}
	cp.pool = pools.NewResourcePool(f, cp.capacity, cp.capacity, cp.idleTimeout)
	if cp.memcacheStats != nil {
		cp.memcacheStats.Open()
	}
}

func (cp *CachePool) startMemcache() {
	if strings.Contains(cp.port, "/") {
		_ = os.Remove(cp.port)
	}
	commandLine := cp.rowCacheConfig.GetSubprocessFlags()
	cp.cmd = exec.Command(commandLine[0], commandLine[1:]...)
	if err := cp.cmd.Start(); err != nil {
		log.Fatalf("can't start memcache: %v", err)
	}
	attempts := 0
	for {
		time.Sleep(100 * time.Millisecond)
		c, err := memcache.Connect(cp.port, 30*time.Millisecond)
		if err != nil {
			attempts++
			if attempts >= 50 {
				cp.cmd.Process.Kill()
				// Avoid zombies
				go cp.cmd.Wait()
				// FIXME(sougou): Throw proper error if we can recover
				log.Fatal("Can't connect to memcache")
			}
			continue
		}
		if _, err = c.Set("health", 0, 0, []byte("ok")); err != nil {
			log.Fatalf("can't communicate with memcache: %v", err)
		}
		c.Close()
		break
	}
}

func (cp *CachePool) Close() {
	// Close the underlying pool first.
	// You cannot close the pool while holding the
	// lock because we have to still allow Put to
	// return outstanding connections, if any.
	pool := cp.getPool()
	if pool == nil {
		return
	}
	pool.Close()

	// No new operations will be allowed now.
	// Safe to cleanup.
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.pool == nil {
		return
	}
	if cp.memcacheStats != nil {
		cp.memcacheStats.Close()
	}
	cp.cmd.Process.Kill()
	// Avoid zombies
	go cp.cmd.Wait()
	if strings.Contains(cp.port, "/") {
		_ = os.Remove(cp.port)
	}
	cp.pool = nil
}

func (cp *CachePool) IsClosed() bool {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.pool == nil
}

func (cp *CachePool) getPool() *pools.ResourcePool {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.pool
}

// You must call Put after Get.
func (cp *CachePool) Get(timeout time.Duration) *memcache.Connection {
	pool := cp.getPool()
	if pool == nil {
		log.Fatal("cache pool is not open")
	}
	r, err := pool.Get()
	if err != nil {
		log.Fatal(err)
	}
	return r.(*memcache.Connection)
}

func (cp *CachePool) Put(conn *memcache.Connection) {
	pool := cp.getPool()
	if pool == nil {
		return
	}
	if conn == nil {
		pool.Put(nil)
	} else {
		pool.Put(conn)
	}
}

func (cp *CachePool) StatsJSON() string {
	pool := cp.getPool()
	if pool == nil {
		return "{}"
	}
	return pool.StatsJSON()
}

func (cp *CachePool) Capacity() int64 {
	pool := cp.getPool()
	if pool == nil {
		return 0
	}
	return pool.Capacity()
}

func (cp *CachePool) Available() int64 {
	pool := cp.getPool()
	if pool == nil {
		return 0
	}
	return pool.Available()
}

func (cp *CachePool) MaxCap() int64 {
	pool := cp.getPool()
	if pool == nil {
		return 0
	}
	return pool.MaxCap()
}

func (cp *CachePool) WaitCount() int64 {
	pool := cp.getPool()
	if pool == nil {
		return 0
	}
	return pool.WaitCount()
}

func (cp *CachePool) WaitTime() time.Duration {
	pool := cp.getPool()
	if pool == nil {
		return 0
	}
	return pool.WaitTime()
}

func (cp *CachePool) IdleTimeout() time.Duration {
	pool := cp.getPool()
	if pool == nil {
		return 0
	}
	return pool.IdleTimeout()
}

func (cp *CachePool) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	defer func() {
		if x := recover(); x != nil {
			response.Write(([]byte)(x.(error).Error()))
		}
	}()
	response.Header().Set("Content-Type", "text/plain")
	pool := cp.getPool()
	if pool == nil {
		response.Write(([]byte)("closed"))
		return
	}
	command := request.URL.Path[len(statsURL):]
	if command == "stats" {
		command = ""
	}
	conn := cp.Get(0)
	// This is not the same as defer rc.cachePool.Put(conn)
	defer func() { cp.Put(conn) }()
	r, err := conn.Stats(command)
	if err != nil {
		conn.Close()
		conn = nil
		response.Write(([]byte)(err.Error()))
	} else {
		response.Write(r)
	}
}
