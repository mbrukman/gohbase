// Copyright (C) 2016  The GoHBase Authors.  All rights reserved.
// This file is part of GoHBase.
// Use of this source code is governed by the Apache License 2.0
// that can be found in the COPYING file.

package gohbase

import (
	"bytes"
	"io"
	"sync"

	"github.com/cznic/b"
	"github.com/tsuna/gohbase/hrpc"
)

// clientRegionCache is client -> region cache. Used to quickly
// look up all the regioninfos that map to a specific client
type clientRegionCache struct {
	m sync.Mutex

	regions map[hrpc.RegionClient][]hrpc.RegionInfo
}

func (rcc *clientRegionCache) put(c hrpc.RegionClient, r hrpc.RegionInfo) {
	rcc.m.Lock()
	defer rcc.m.Unlock()

	lst := rcc.regions[c]
	for _, existing := range lst {
		if existing == r {
			return
		}
	}
	rcc.regions[c] = append(lst, r)
}

func (rcc *clientRegionCache) del(r hrpc.RegionInfo) {
	rcc.m.Lock()
	c := r.Client()
	if c != nil {
		r.SetClient(nil)

		rs := rcc.regions[c]
		var index int
		for i, reg := range rs {
			if reg == r {
				index = i
			}
		}
		rs = append(rs[:index], rs[index+1:]...)

		if len(rs) == 0 {
			// close region client if noone is using it
			delete(rcc.regions, c)
			c.Close()
		} else {
			rcc.regions[c] = rs
		}
	}
	rcc.m.Unlock()
}

func (rcc *clientRegionCache) closeAll() {
	rcc.m.Lock()
	for client, regions := range rcc.regions {
		for _, region := range regions {
			region.MarkUnavailable()
			region.SetClient(nil)
		}
		client.Close()
	}
	rcc.m.Unlock()
}

func (rcc *clientRegionCache) clientDown(reg hrpc.RegionInfo) []hrpc.RegionInfo {
	rcc.m.Lock()
	var downregions []hrpc.RegionInfo
	c := reg.Client()
	for _, sharedReg := range rcc.regions[c] {
		succ := sharedReg.MarkUnavailable()
		sharedReg.SetClient(nil)
		if succ {
			downregions = append(downregions, sharedReg)
		}
	}
	delete(rcc.regions, c)
	rcc.m.Unlock()
	return downregions
}

func (rcc *clientRegionCache) checkForClient(host string, port uint16) hrpc.RegionClient {
	rcc.m.Lock()
	defer rcc.m.Unlock()

	for client := range rcc.regions {
		if client.Host() == host && client.Port() == port {
			return client
		}
	}
	return nil
}

// key -> region cache.
type keyRegionCache struct {
	m sync.Mutex

	// Maps a []byte of a region start key to a hrpc.RegionInfo
	regions *b.Tree
}

func (krc *keyRegionCache) get(key []byte) ([]byte, hrpc.RegionInfo) {
	// When seeking - "The Enumerator's position is possibly after the last item in the tree"
	// http://godoc.org/github.com/cznic/b#Tree.Set
	krc.m.Lock()

	enum, ok := krc.regions.Seek(key)
	k, v, err := enum.Prev()
	if err == io.EOF && krc.regions.Len() > 0 {
		// We're past the end of the tree. Return the last element instead.
		// (Without this code we always get a cache miss and create a new client for each req.)
		k, v = krc.regions.Last()
		err = nil
	} else if !ok {
		k, v, err = enum.Prev()
	}
	enum.Close()
	if err != nil {
		krc.m.Unlock()
		return nil, nil
	}
	krc.m.Unlock()
	return k.([]byte), v.(hrpc.RegionInfo)
}

func isRegionOverlap(regA, regB hrpc.RegionInfo) bool {
	return bytes.Equal(regA.Table(), regB.Table()) &&
		bytes.Compare(regA.StartKey(), regB.StopKey()) < 0 &&
		bytes.Compare(regA.StopKey(), regB.StartKey()) > 0
}

func (krc *keyRegionCache) getOverlaps(reg hrpc.RegionInfo) []hrpc.RegionInfo {
	var overlaps []hrpc.RegionInfo
	var v interface{}
	var err error

	// deal with empty tree in the beginning so that we don't have to check
	// EOF errors for enum later
	if krc.regions.Len() == 0 {
		return overlaps
	}

	enum, ok := krc.regions.Seek(reg.Name())
	if !ok {
		// need to check if there are overlaps before what we found
		_, _, err = enum.Prev()
		if err == io.EOF {
			// we are in the end of tree, get last entry
			_, v = krc.regions.Last()
			currReg := v.(hrpc.RegionInfo)
			if isRegionOverlap(currReg, reg) {
				return append(overlaps, currReg)
			}
		} else {
			_, v, err = enum.Next()
			if err == io.EOF {
				// we are before the beginning of the tree now, get new enum
				enum.Close()
				enum, err = krc.regions.SeekFirst()
			} else {
				// otherwise, check for overlap before us
				currReg := v.(hrpc.RegionInfo)
				if isRegionOverlap(currReg, reg) {
					overlaps = append(overlaps, currReg)
				}
			}
		}
	}

	// now append all regions that overlap until the end of the tree
	// or until they don't overlap
	_, v, err = enum.Next()
	for err == nil && isRegionOverlap(v.(hrpc.RegionInfo), reg) {
		overlaps = append(overlaps, v.(hrpc.RegionInfo))
		_, v, err = enum.Next()
	}
	enum.Close()
	return overlaps
}

func (krc *keyRegionCache) put(reg hrpc.RegionInfo) []hrpc.RegionInfo {
	krc.m.Lock()
	defer krc.m.Unlock()

	// Remove all the entries that are overlap with the range of the new region.
	os := krc.getOverlaps(reg)
	for _, o := range os {
		krc.regions.Delete(o.Name())
	}

	krc.regions.Put(reg.Name(), func(interface{}, bool) (interface{}, bool) {
		return reg, true
	})
	return os
}

func (krc *keyRegionCache) del(key []byte) bool {
	krc.m.Lock()
	success := krc.regions.Delete(key)
	krc.m.Unlock()
	return success
}
