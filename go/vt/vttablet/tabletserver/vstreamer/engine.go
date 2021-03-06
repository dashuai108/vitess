/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vstreamer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"vitess.io/vitess/go/acl"
	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/stats"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/srvtopo"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/vtgate/vindexes"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/schema"

	binlogdatapb "vitess.io/vitess/go/vt/proto/binlogdata"
	vschemapb "vitess.io/vitess/go/vt/proto/vschema"
)

var (
	once           sync.Once
	vschemaErrors  *stats.Counter
	vschemaUpdates *stats.Counter
)

// Engine is the engine for handling vreplication streaming requests.
type Engine struct {
	// cp is initialized by InitDBConfig
	cp *mysql.ConnParams

	// mu protects isOpen, streamers, streamIdx and vschema.
	mu sync.Mutex

	isOpen bool
	// wg is incremented for every Stream, and decremented on end.
	// Close waits for all current streams to end by waiting on wg.
	wg              sync.WaitGroup
	streamers       map[int]*vstreamer
	rowStreamers    map[int]*rowStreamer
	resultStreamers map[int]*resultStreamer
	streamIdx       int

	// watcherOnce is used for initializing vschema
	// and setting up the vschema watch. It's guaranteed that
	// no stream will start until vschema is initialized by
	// the first call through watcherOnce.
	watcherOnce sync.Once
	lvschema    *localVSchema

	// The following members are initialized once at the beginning.
	ts       srvtopo.Server
	se       *schema.Engine
	keyspace string
	cell     string
}

// NewEngine creates a new Engine.
// Initialization sequence is: NewEngine->InitDBConfig->Open.
// Open and Close can be called multiple times and are idempotent.
func NewEngine(ts srvtopo.Server, se *schema.Engine) *Engine {
	vse := &Engine{
		streamers:       make(map[int]*vstreamer),
		rowStreamers:    make(map[int]*rowStreamer),
		resultStreamers: make(map[int]*resultStreamer),
		lvschema:        &localVSchema{vschema: &vindexes.VSchema{}},
		ts:              ts,
		se:              se,
	}
	once.Do(func() {
		vschemaErrors = stats.NewCounter("VSchemaErrors", "Count of VSchema errors")
		vschemaUpdates = stats.NewCounter("VSchemaUpdates", "Count of VSchema updates. Does not include errors")
		http.Handle("/debug/tablet_vschema", vse)
	})
	return vse
}

// InitDBConfig performs saves the required info from dbconfigs for future use.
func (vse *Engine) InitDBConfig(cp *mysql.ConnParams) {
	vse.cp = cp
}

// Open starts the Engine service.
func (vse *Engine) Open(keyspace, cell string) error {
	vse.mu.Lock()
	defer vse.mu.Unlock()
	if vse.isOpen {
		return nil
	}
	vse.isOpen = true
	vse.keyspace = keyspace
	vse.cell = cell
	return nil
}

// IsOpen checks if the engine is opened
func (vse *Engine) IsOpen() bool {
	vse.mu.Lock()
	defer vse.mu.Unlock()
	return vse.isOpen
}

// Close closes the Engine service.
func (vse *Engine) Close() {
	func() {
		vse.mu.Lock()
		defer vse.mu.Unlock()
		if !vse.isOpen {
			return
		}
		// cancels are non-blocking.
		for _, s := range vse.streamers {
			s.Cancel()
		}
		for _, s := range vse.rowStreamers {
			s.Cancel()
		}
		for _, s := range vse.resultStreamers {
			s.Cancel()
		}
		vse.isOpen = false
	}()

	// Wait only after releasing the lock because the end of every
	// stream will use the lock to remove the entry from streamers.
	vse.wg.Wait()
}

func (vse *Engine) vschema() *vindexes.VSchema {
	vse.mu.Lock()
	defer vse.mu.Unlock()
	return vse.lvschema.vschema
}

// Stream starts a new stream.
func (vse *Engine) Stream(ctx context.Context, startPos string, filter *binlogdatapb.Filter, send func([]*binlogdatapb.VEvent) error) error {
	// Ensure vschema is initialized and the watcher is started.
	// Starting of the watcher has to be delayed till the first call to Stream
	// because this overhead should be incurred only if someone uses this feature.
	vse.watcherOnce.Do(vse.setWatch)

	// Create stream and add it to the map.
	streamer, idx, err := func() (*vstreamer, int, error) {
		vse.mu.Lock()
		defer vse.mu.Unlock()
		if !vse.isOpen {
			return nil, 0, errors.New("VStreamer is not open")
		}
		streamer := newVStreamer(ctx, vse.cp, vse.se, startPos, filter, vse.lvschema, send)
		idx := vse.streamIdx
		vse.streamers[idx] = streamer
		vse.streamIdx++
		// Now that we've added the stream, increment wg.
		// This must be done before releasing the lock.
		vse.wg.Add(1)
		return streamer, idx, nil
	}()
	if err != nil {
		return err
	}

	// Remove stream from map and decrement wg when it ends.
	defer func() {
		vse.mu.Lock()
		defer vse.mu.Unlock()
		delete(vse.streamers, idx)
		vse.wg.Done()
	}()

	// No lock is held while streaming, but wg is incremented.
	return streamer.Stream()
}

// StreamRows streams rows.
func (vse *Engine) StreamRows(ctx context.Context, query string, lastpk []sqltypes.Value, send func(*binlogdatapb.VStreamRowsResponse) error) error {
	// Ensure vschema is initialized and the watcher is started.
	// Starting of the watcher has to be delayed till the first call to Stream
	// because this overhead should be incurred only if someone uses this feature.
	vse.watcherOnce.Do(vse.setWatch)
	log.Infof("Streaming rows for query %s, lastpk: %s", query, lastpk)

	// Create stream and add it to the map.
	rowStreamer, idx, err := func() (*rowStreamer, int, error) {
		vse.mu.Lock()
		defer vse.mu.Unlock()
		if !vse.isOpen {
			return nil, 0, errors.New("VStreamer is not open")
		}
		rowStreamer := newRowStreamer(ctx, vse.cp, vse.se, query, lastpk, vse.lvschema, send)
		idx := vse.streamIdx
		vse.rowStreamers[idx] = rowStreamer
		vse.streamIdx++
		// Now that we've added the stream, increment wg.
		// This must be done before releasing the lock.
		vse.wg.Add(1)
		return rowStreamer, idx, nil
	}()
	if err != nil {
		return err
	}

	// Remove stream from map and decrement wg when it ends.
	defer func() {
		vse.mu.Lock()
		defer vse.mu.Unlock()
		delete(vse.rowStreamers, idx)
		vse.wg.Done()
	}()

	// No lock is held while streaming, but wg is incremented.
	return rowStreamer.Stream()
}

// StreamResults streams results of the query with the gtid.
func (vse *Engine) StreamResults(ctx context.Context, query string, send func(*binlogdatapb.VStreamResultsResponse) error) error {
	// Create stream and add it to the map.
	resultStreamer, idx, err := func() (*resultStreamer, int, error) {
		vse.mu.Lock()
		defer vse.mu.Unlock()
		if !vse.isOpen {
			return nil, 0, errors.New("VStreamer is not open")
		}
		resultStreamer := newResultStreamer(ctx, vse.cp, query, send)
		idx := vse.streamIdx
		vse.resultStreamers[idx] = resultStreamer
		vse.streamIdx++
		// Now that we've added the stream, increment wg.
		// This must be done before releasing the lock.
		vse.wg.Add(1)
		return resultStreamer, idx, nil
	}()
	if err != nil {
		return err
	}

	// Remove stream from map and decrement wg when it ends.
	defer func() {
		vse.mu.Lock()
		defer vse.mu.Unlock()
		delete(vse.resultStreamers, idx)
		vse.wg.Done()
	}()

	// No lock is held while streaming, but wg is incremented.
	return resultStreamer.Stream()
}

// ServeHTTP shows the current VSchema.
func (vse *Engine) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if err := acl.CheckAccessHTTP(request, acl.DEBUGGING); err != nil {
		acl.SendError(response, err)
		return
	}
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	vs := vse.vschema()
	if vs == nil {
		response.Write([]byte("{}"))
	}
	b, err := json.MarshalIndent(vs, "", "  ")
	if err != nil {
		response.Write([]byte(err.Error()))
		return
	}
	buf := bytes.NewBuffer(nil)
	json.HTMLEscape(buf, b)
	response.Write(buf.Bytes())
}

func (vse *Engine) setWatch() {
	// WatchSrvVSchema does not return until the inner func has been called at least once.
	vse.ts.WatchSrvVSchema(context.TODO(), vse.cell, func(v *vschemapb.SrvVSchema, err error) {
		switch {
		case err == nil:
			// Build vschema down below.
		case topo.IsErrType(err, topo.NoNode):
			v = nil
		default:
			log.Errorf("Error fetching vschema: %v", err)
			vschemaErrors.Add(1)
			return
		}
		var vschema *vindexes.VSchema
		if v != nil {
			vschema, err = vindexes.BuildVSchema(v)
			if err != nil {
				log.Errorf("Error building vschema: %v", err)
				vschemaErrors.Add(1)
				return
			}
		} else {
			vschema = &vindexes.VSchema{}
		}

		// Broadcast the change to all streamers.
		vse.mu.Lock()
		defer vse.mu.Unlock()
		vse.lvschema = &localVSchema{
			keyspace: vse.keyspace,
			vschema:  vschema,
		}
		b, _ := json.MarshalIndent(vschema, "", "  ")
		log.Infof("Updated vschema: %s", b)
		for _, s := range vse.streamers {
			s.SetVSchema(vse.lvschema)
		}
		vschemaUpdates.Add(1)
	})
}
