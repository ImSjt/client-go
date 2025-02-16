// Copyright 2021 TiKV Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// NOTE: The code in this file is based on code from the
// TiDB project, licensed under the Apache License v 2.0
//
// https://github.com/pingcap/tidb/tree/cc5e161ac06827589c4966674597c137cc9e809c/store/tikv/mockstore/mocktikv/cluster.go
//

// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mocktikv

import (
	"bytes"
	"context"
	"math"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/golang/protobuf/proto" //nolint:staticcheck
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/tikv/client-go/v2/internal/mockstore/cluster"
	"github.com/tikv/client-go/v2/util"
	"github.com/tikv/pd/client/clients/router"
	"github.com/tikv/pd/client/opt"
)

var _ cluster.Cluster = &Cluster{}

// Cluster simulates a TiKV cluster. It focuses on management and the change of
// meta data. A Cluster mainly includes following 3 kinds of meta data:
//  1. Region: A Region is a fragment of TiKV's data whose range is [start, end).
//     The data of a Region is duplicated to multiple Peers and distributed in
//     multiple Stores.
//  2. Peer: A Peer is a replica of a Region's data. All peers of a Region form
//     a group, each group elects a Leader to provide services.
//  3. Store: A Store is a storage/service node. Try to think it as a TiKV server
//     process. Only the store with request's Region's leader Peer could respond
//     to client's request.
type Cluster struct {
	sync.RWMutex
	id        uint64
	stores    map[uint64]*Store
	regions   map[uint64]*Region
	downPeers map[uint64]struct{}

	mvccStore MVCCStore

	// delayEvents is used to control the execution sequence of rpc requests for test.
	delayEvents map[delayKey]time.Duration
	delayMu     sync.Mutex
}

type delayKey struct {
	startTS  uint64
	regionID uint64
}

// NewCluster creates an empty cluster. It needs to be bootstrapped before
// providing service.
func NewCluster(mvccStore MVCCStore) *Cluster {
	return &Cluster{
		stores:      make(map[uint64]*Store),
		regions:     make(map[uint64]*Region),
		downPeers:   make(map[uint64]struct{}),
		delayEvents: make(map[delayKey]time.Duration),
		mvccStore:   mvccStore,
	}
}

// AllocID creates an unique ID in cluster. The ID could be used as either
// StoreID, RegionID, or PeerID.
func (c *Cluster) AllocID() uint64 {
	c.Lock()
	defer c.Unlock()

	return c.allocID()
}

// AllocIDs creates multiple IDs.
func (c *Cluster) AllocIDs(n int) []uint64 {
	c.Lock()
	defer c.Unlock()

	var ids []uint64
	for len(ids) < n {
		ids = append(ids, c.allocID())
	}
	return ids
}

func (c *Cluster) allocID() uint64 {
	c.id++
	return c.id
}

// GetAllRegions gets all the regions in the cluster.
func (c *Cluster) GetAllRegions() []*Region {
	regions := make([]*Region, 0, len(c.regions))
	for _, region := range c.regions {
		regions = append(regions, region)
	}
	return regions
}

// GetStore returns a Store's meta.
func (c *Cluster) GetStore(storeID uint64) *metapb.Store {
	c.RLock()
	defer c.RUnlock()

	if store := c.stores[storeID]; store != nil {
		return proto.Clone(store.meta).(*metapb.Store)
	}
	return nil
}

// GetAllStores returns all Stores' meta.
func (c *Cluster) GetAllStores() []*metapb.Store {
	c.RLock()
	defer c.RUnlock()

	stores := make([]*metapb.Store, 0, len(c.stores))
	for _, store := range c.stores {
		stores = append(stores, proto.Clone(store.meta).(*metapb.Store))
	}
	return stores
}

// StopStore stops a store with storeID.
func (c *Cluster) StopStore(storeID uint64) {
	c.Lock()
	defer c.Unlock()

	if store := c.stores[storeID]; store != nil {
		store.meta.State = metapb.StoreState_Offline
	}
}

// StartStore starts a store with storeID.
func (c *Cluster) StartStore(storeID uint64) {
	c.Lock()
	defer c.Unlock()

	if store := c.stores[storeID]; store != nil {
		store.meta.State = metapb.StoreState_Up
	}
}

// CancelStore makes the store with cancel state true.
func (c *Cluster) CancelStore(storeID uint64) {
	c.Lock()
	defer c.Unlock()

	// A store returns context.Cancelled Error when cancel is true.
	if store := c.stores[storeID]; store != nil {
		store.cancel = true
	}
}

// UnCancelStore makes the store with cancel state false.
func (c *Cluster) UnCancelStore(storeID uint64) {
	c.Lock()
	defer c.Unlock()

	if store := c.stores[storeID]; store != nil {
		store.cancel = false
	}
}

// GetStoreByAddr returns a Store's meta by an addr.
func (c *Cluster) GetStoreByAddr(addr string) *metapb.Store {
	c.RLock()
	defer c.RUnlock()

	for _, s := range c.stores {
		if s.meta.GetAddress() == addr {
			return proto.Clone(s.meta).(*metapb.Store)
		}
	}
	return nil
}

// GetAndCheckStoreByAddr checks and returns a Store's meta by an addr
func (c *Cluster) GetAndCheckStoreByAddr(addr string) (ss []*metapb.Store, err error) {
	c.RLock()
	defer c.RUnlock()

	for _, s := range c.stores {
		if s.cancel {
			err = context.Canceled
			return
		}
		if s.meta.GetAddress() == addr {
			ss = append(ss, proto.Clone(s.meta).(*metapb.Store))
		}
	}
	return
}

// AddStore add a new Store to the cluster.
func (c *Cluster) AddStore(storeID uint64, addr string, labels ...*metapb.StoreLabel) {
	c.Lock()
	defer c.Unlock()

	c.stores[storeID] = newStore(storeID, addr, addr, labels...)
}

// RemoveStore removes a Store from the cluster.
func (c *Cluster) RemoveStore(storeID uint64) {
	c.Lock()
	defer c.Unlock()

	delete(c.stores, storeID)
}

// MarkTombstone marks store as tombstone.
func (c *Cluster) MarkTombstone(storeID uint64) {
	c.Lock()
	defer c.Unlock()
	nm := *c.stores[storeID].meta
	nm.State = metapb.StoreState_Tombstone
	c.stores[storeID].meta = &nm
}

func (c *Cluster) MarkPeerDown(peerID uint64) {
	c.Lock()
	defer c.Unlock()
	c.downPeers[peerID] = struct{}{}
}

func (c *Cluster) RemoveDownPeer(peerID uint64) {
	c.Lock()
	defer c.Unlock()
	delete(c.downPeers, peerID)
}

// UpdateStoreAddr updates store address for cluster.
func (c *Cluster) UpdateStoreAddr(storeID uint64, addr string, labels ...*metapb.StoreLabel) {
	c.Lock()
	defer c.Unlock()
	c.stores[storeID] = newStore(storeID, addr, addr, labels...)
}

// UpdateStorePeerAddr updates store peer address for cluster.
func (c *Cluster) UpdateStorePeerAddr(storeID uint64, peerAddr string, labels ...*metapb.StoreLabel) {
	c.Lock()
	defer c.Unlock()
	addr := c.stores[storeID].meta.Address
	c.stores[storeID] = newStore(storeID, addr, peerAddr, labels...)
}

// GetRegion returns a Region's meta and leader ID.
func (c *Cluster) GetRegion(regionID uint64) (*metapb.Region, uint64) {
	c.RLock()
	defer c.RUnlock()

	r := c.regions[regionID]
	if r == nil {
		return nil, 0
	}
	return proto.Clone(r.Meta).(*metapb.Region), r.leader
}

// GetRegionByKey returns the Region and its leader whose range contains the key.
func (c *Cluster) GetRegionByKey(key []byte) (*metapb.Region, *metapb.Peer, *metapb.Buckets, []*metapb.Peer) {
	c.RLock()
	defer c.RUnlock()

	return c.getRegionByKeyNoLock(key)
}

// getRegionByKeyNoLock returns the Region and its leader whose range contains the key without Lock.
func (c *Cluster) getRegionByKeyNoLock(key []byte) (*metapb.Region, *metapb.Peer, *metapb.Buckets, []*metapb.Peer) {
	for _, r := range c.regions {
		if regionContains(r.Meta.StartKey, r.Meta.EndKey, key) {
			return proto.Clone(r.Meta).(*metapb.Region), proto.Clone(r.leaderPeer()).(*metapb.Peer),
				proto.Clone(r.Buckets).(*metapb.Buckets), c.getDownPeers(r)
		}
	}
	return nil, nil, nil, nil
}

// GetPrevRegionByKey returns the previous Region and its leader whose range contains the key.
func (c *Cluster) GetPrevRegionByKey(key []byte) (*metapb.Region, *metapb.Peer, *metapb.Buckets, []*metapb.Peer) {
	c.RLock()
	defer c.RUnlock()

	currentRegion, _, _, _ := c.getRegionByKeyNoLock(key)
	if len(currentRegion.StartKey) == 0 {
		return nil, nil, nil, nil
	}
	for _, r := range c.regions {
		if bytes.Equal(r.Meta.EndKey, currentRegion.StartKey) {
			return proto.Clone(r.Meta).(*metapb.Region), proto.Clone(r.leaderPeer()).(*metapb.Peer),
				proto.Clone(r.Buckets).(*metapb.Buckets), c.getDownPeers(r)
		}
	}
	return nil, nil, nil, nil
}

func (c *Cluster) getDownPeers(region *Region) []*metapb.Peer {
	var downPeers []*metapb.Peer
	for peerID := range c.downPeers {
		for _, peer := range region.Meta.Peers {
			if peer.GetId() == peerID {
				downPeers = append(downPeers, proto.Clone(peer).(*metapb.Peer))
			}
		}
	}
	return downPeers
}

// GetRegionByID returns the Region and its leader whose ID is regionID.
func (c *Cluster) GetRegionByID(regionID uint64) (*metapb.Region, *metapb.Peer, *metapb.Buckets, []*metapb.Peer) {
	c.RLock()
	defer c.RUnlock()

	for _, r := range c.regions {
		if r.Meta.GetId() == regionID {
			return proto.Clone(r.Meta).(*metapb.Region), proto.Clone(r.leaderPeer()).(*metapb.Peer),
				proto.Clone(r.Buckets).(*metapb.Buckets), c.getDownPeers(r)
		}
	}
	return nil, nil, nil, nil
}

// ScanRegions returns at most `limit` regions from given `key` and their leaders.
func (c *Cluster) ScanRegions(startKey, endKey []byte, limit int, opts ...opt.GetRegionOption) []*router.Region {
	c.RLock()
	defer c.RUnlock()

	regions := make([]*Region, 0, len(c.regions))
	for _, region := range c.regions {
		regions = append(regions, region)
	}

	slices.SortFunc(regions, func(i, j *Region) int {
		return bytes.Compare(i.Meta.GetStartKey(), j.Meta.GetStartKey())
	})

	startPos := sort.Search(len(regions), func(i int) bool {
		if len(regions[i].Meta.GetEndKey()) == 0 {
			return true
		}
		return bytes.Compare(regions[i].Meta.GetEndKey(), startKey) > 0
	})
	regions = regions[startPos:]
	if len(endKey) > 0 {
		endPos := sort.Search(len(regions), func(i int) bool {
			return bytes.Compare(regions[i].Meta.GetStartKey(), endKey) >= 0
		})
		if endPos > 0 {
			regions = regions[:endPos]
		}
	}
	if rid, err := util.EvalFailpoint("mockSplitRegionNotReportToPD"); err == nil {
		notReportRegionID := uint64(rid.(int))
		for i, r := range regions {
			if r.Meta.Id == notReportRegionID {
				regions = append(regions[:i], regions[i+1:]...)
				break
			}
		}
	}
	if limit > 0 && len(regions) > limit {
		regions = regions[:limit]
	}

	result := make([]*router.Region, 0, len(regions))
	for _, region := range regions {
		leader := region.leaderPeer()
		if leader == nil {
			leader = &metapb.Peer{}
		} else {
			leader = proto.Clone(leader).(*metapb.Peer)
		}

		r := &router.Region{
			Meta:      proto.Clone(region.Meta).(*metapb.Region),
			Leader:    leader,
			DownPeers: c.getDownPeers(region),
			Buckets:   proto.Clone(region.Buckets).(*metapb.Buckets),
		}
		result = append(result, r)
	}

	return result
}

// Bootstrap creates the first Region. The Stores should be in the Cluster before
// bootstrap.
func (c *Cluster) Bootstrap(regionID uint64, storeIDs, peerIDs []uint64, leaderPeerID uint64) {
	c.Lock()
	defer c.Unlock()

	if len(storeIDs) != len(peerIDs) {
		panic("len(storeIDs) != len(peerIDs)")
	}
	c.regions[regionID] = newRegion(regionID, storeIDs, peerIDs, leaderPeerID)
}

// PutRegion adds or replaces a region.
func (c *Cluster) PutRegion(regionID, confVer, ver uint64, storeIDs, peerIDs []uint64, leaderPeerID uint64) {
	c.Lock()
	defer c.Unlock()

	c.regions[regionID] = newRegion(regionID, storeIDs, peerIDs, leaderPeerID, confVer, ver)
}

// AddPeer adds a new Peer for the Region on the Store.
func (c *Cluster) AddPeer(regionID, storeID, peerID uint64) {
	c.Lock()
	defer c.Unlock()

	c.regions[regionID].addPeer(peerID, storeID, metapb.PeerRole_Voter)
}

// AddLearner adds a new learner for the Region on the Store.
func (c *Cluster) AddLearner(regionID, storeID, peerID uint64) {
	c.Lock()
	defer c.Unlock()

	c.regions[regionID].addPeer(peerID, storeID, metapb.PeerRole_Learner)
}

// RemovePeer removes the Peer from the Region. Note that if the Peer is leader,
// the Region will have no leader before calling ChangeLeader().
func (c *Cluster) RemovePeer(regionID, peerID uint64) {
	c.Lock()
	defer c.Unlock()

	c.regions[regionID].removePeer(peerID)
}

// ChangeLeader sets the Region's leader Peer. Caller should guarantee the Peer
// exists.
func (c *Cluster) ChangeLeader(regionID, leaderPeerID uint64) {
	c.Lock()
	defer c.Unlock()

	c.regions[regionID].changeLeader(leaderPeerID)
}

// GiveUpLeader sets the Region's leader to 0. The Region will have no leader
// before calling ChangeLeader().
func (c *Cluster) GiveUpLeader(regionID uint64) {
	c.ChangeLeader(regionID, 0)
}

// Split splits a Region at the key (encoded) and creates new Region.
func (c *Cluster) Split(regionID, newRegionID uint64, key []byte, peerIDs []uint64, leaderPeerID uint64) {
	c.SplitRaw(regionID, newRegionID, NewMvccKey(key), peerIDs, leaderPeerID)
}

// SplitRegionBuckets splits a Region to buckets logically.
func (c *Cluster) SplitRegionBuckets(regionID uint64, keys [][]byte, bucketVer uint64) {
	c.Lock()
	defer c.Unlock()
	region := c.regions[regionID]
	mvccKeys := make([][]byte, 0, len(keys))
	for _, k := range keys {
		mvccKeys = append(mvccKeys, NewMvccKey(k))
	}
	region.Buckets = &metapb.Buckets{RegionId: regionID, Version: bucketVer, Keys: mvccKeys}
}

// SplitRaw splits a Region at the key (not encoded) and creates new Region.
func (c *Cluster) SplitRaw(regionID, newRegionID uint64, rawKey []byte, peerIDs []uint64, leaderPeerID uint64) *metapb.Region {
	c.Lock()
	defer c.Unlock()

	newRegion := c.regions[regionID].split(newRegionID, rawKey, peerIDs, leaderPeerID)
	c.regions[newRegionID] = newRegion
	// The mocktikv should return a deep copy of meta info to avoid data race
	meta := proto.Clone(newRegion.Meta)
	return meta.(*metapb.Region)
}

// Merge merges 2 regions, their key ranges should be adjacent.
func (c *Cluster) Merge(regionID1, regionID2 uint64) {
	c.Lock()
	defer c.Unlock()

	c.regions[regionID1].merge(c.regions[regionID2].Meta.GetEndKey())
	delete(c.regions, regionID2)
}

// SplitKeys evenly splits the start, end key into "count" regions.
// Only works for single store.
func (c *Cluster) SplitKeys(start, end []byte, count int) {
	c.splitRange(c.mvccStore, NewMvccKey(start), NewMvccKey(end), count)
}

// ScheduleDelay schedules a delay event for a transaction on a region.
func (c *Cluster) ScheduleDelay(startTS, regionID uint64, dur time.Duration) {
	c.delayMu.Lock()
	c.delayEvents[delayKey{startTS: startTS, regionID: regionID}] = dur
	c.delayMu.Unlock()
}

// UpdateStoreLabels merge the target and owned labels together
func (c *Cluster) UpdateStoreLabels(storeID uint64, labels []*metapb.StoreLabel) {
	c.Lock()
	defer c.Unlock()
	c.stores[storeID].mergeLabels(labels)
}

func (c *Cluster) handleDelay(startTS, regionID uint64) {
	key := delayKey{startTS: startTS, regionID: regionID}
	c.delayMu.Lock()
	dur, ok := c.delayEvents[key]
	if ok {
		delete(c.delayEvents, key)
	}
	c.delayMu.Unlock()
	if ok {
		time.Sleep(dur)
	}
}

func (c *Cluster) splitRange(mvccStore MVCCStore, start, end MvccKey, count int) {
	c.Lock()
	defer c.Unlock()
	c.evacuateOldRegionRanges(start, end)
	regionPairs := c.getEntriesGroupByRegions(mvccStore, start, end, count)
	c.createNewRegions(regionPairs, start, end)
}

// getEntriesGroupByRegions groups the key value pairs into splitted regions.
func (c *Cluster) getEntriesGroupByRegions(mvccStore MVCCStore, start, end MvccKey, count int) [][]Pair {
	startTS := uint64(math.MaxUint64)
	limit := math.MaxInt32
	pairs := mvccStore.Scan(start.Raw(), end.Raw(), limit, startTS, kvrpcpb.IsolationLevel_SI, nil)
	regionEntriesSlice := make([][]Pair, 0, count)
	quotient := len(pairs) / count
	remainder := len(pairs) % count
	i := 0
	for i < len(pairs) {
		regionEntryCount := quotient
		if remainder > 0 {
			remainder--
			regionEntryCount++
		}
		regionEntries := pairs[i : i+regionEntryCount]
		regionEntriesSlice = append(regionEntriesSlice, regionEntries)
		i += regionEntryCount
	}
	return regionEntriesSlice
}

func (c *Cluster) createNewRegions(regionPairs [][]Pair, start, end MvccKey) {
	for i := range regionPairs {
		peerID := c.allocID()
		newRegion := newRegion(c.allocID(), []uint64{c.firstStoreID()}, []uint64{peerID}, peerID)
		var regionStartKey, regionEndKey MvccKey
		if i == 0 {
			regionStartKey = start
		} else {
			regionStartKey = NewMvccKey(regionPairs[i][0].Key)
		}
		if i == len(regionPairs)-1 {
			regionEndKey = end
		} else {
			// Use the next region's first key as region end key.
			regionEndKey = NewMvccKey(regionPairs[i+1][0].Key)
		}
		newRegion.updateKeyRange(regionStartKey, regionEndKey)
		c.regions[newRegion.Meta.Id] = newRegion
	}
}

// evacuateOldRegionRanges evacuate the range [start, end].
// Old regions has intersection with [start, end) will be updated or deleted.
func (c *Cluster) evacuateOldRegionRanges(start, end MvccKey) {
	oldRegions := c.getRegionsCoverRange(start, end)
	for _, oldRegion := range oldRegions {
		startCmp := bytes.Compare(oldRegion.Meta.StartKey, start)
		endCmp := bytes.Compare(oldRegion.Meta.EndKey, end)
		if len(oldRegion.Meta.EndKey) == 0 {
			endCmp = 1
		}
		if startCmp >= 0 && endCmp <= 0 {
			// The region is within table data, it will be replaced by new regions.
			delete(c.regions, oldRegion.Meta.Id)
		} else if startCmp < 0 && endCmp > 0 {
			// A single Region covers table data, split into two regions that do not overlap table data.
			oldEnd := oldRegion.Meta.EndKey
			oldRegion.updateKeyRange(oldRegion.Meta.StartKey, start)
			peerID := c.allocID()
			newRegion := newRegion(c.allocID(), []uint64{c.firstStoreID()}, []uint64{peerID}, peerID)
			newRegion.updateKeyRange(end, oldEnd)
			c.regions[newRegion.Meta.Id] = newRegion
		} else if startCmp < 0 {
			oldRegion.updateKeyRange(oldRegion.Meta.StartKey, start)
		} else {
			oldRegion.updateKeyRange(end, oldRegion.Meta.EndKey)
		}
	}
}

func (c *Cluster) firstStoreID() uint64 {
	for id := range c.stores {
		return id
	}
	return 0
}

// getRegionsCoverRange gets regions in the cluster that has intersection with [start, end).
func (c *Cluster) getRegionsCoverRange(start, end MvccKey) []*Region {
	regions := make([]*Region, 0, len(c.regions))
	for _, region := range c.regions {
		onRight := bytes.Compare(end, region.Meta.StartKey) <= 0
		onLeft := bytes.Compare(region.Meta.EndKey, start) <= 0
		if len(region.Meta.EndKey) == 0 {
			onLeft = false
		}
		if onLeft || onRight {
			continue
		}
		regions = append(regions, region)
	}
	return regions
}

// Region is the Region meta data.
type Region struct {
	Meta    *metapb.Region
	leader  uint64
	Buckets *metapb.Buckets
}

func newPeerMeta(peerID, storeID uint64) *metapb.Peer {
	return &metapb.Peer{
		Id:      peerID,
		StoreId: storeID,
	}
}

func newRegion(regionID uint64, storeIDs, peerIDs []uint64, leaderPeerID uint64, epoch ...uint64) *Region {
	if len(storeIDs) != len(peerIDs) {
		panic("len(storeIDs) != len(peerIds)")
	}
	peers := make([]*metapb.Peer, 0, len(storeIDs))
	for i := range storeIDs {
		peers = append(peers, newPeerMeta(peerIDs[i], storeIDs[i]))
	}
	meta := &metapb.Region{
		Id:          regionID,
		Peers:       peers,
		RegionEpoch: &metapb.RegionEpoch{},
	}
	if len(epoch) == 2 {
		meta.RegionEpoch.ConfVer = epoch[0]
		meta.RegionEpoch.Version = epoch[1]
	}
	return &Region{
		Meta:   meta,
		leader: leaderPeerID,
	}
}

func (r *Region) addPeer(peerID, storeID uint64, role metapb.PeerRole) {
	peer := newPeerMeta(peerID, storeID)
	peer.Role = role
	r.Meta.Peers = append(r.Meta.Peers, peer)
	r.incConfVer()
}

func (r *Region) removePeer(peerID uint64) {
	for i, peer := range r.Meta.Peers {
		if peer.GetId() == peerID {
			r.Meta.Peers = append(r.Meta.Peers[:i], r.Meta.Peers[i+1:]...)
			break
		}
	}
	if r.leader == peerID {
		r.leader = 0
	}
	r.incConfVer()
}

func (r *Region) changeLeader(leaderID uint64) {
	r.leader = leaderID
}

func (r *Region) leaderPeer() *metapb.Peer {
	for _, p := range r.Meta.Peers {
		if p.GetId() == r.leader {
			return p
		}
	}
	return nil
}

func (r *Region) split(newRegionID uint64, key MvccKey, peerIDs []uint64, leaderPeerID uint64) *Region {
	if len(r.Meta.Peers) != len(peerIDs) {
		panic("len(r.meta.Peers) != len(peerIDs)")
	}
	storeIDs := make([]uint64, 0, len(r.Meta.Peers))
	for _, peer := range r.Meta.Peers {
		storeIDs = append(storeIDs, peer.GetStoreId())
	}
	region := newRegion(newRegionID, storeIDs, peerIDs, leaderPeerID)
	region.updateKeyRange(key, r.Meta.EndKey)
	r.updateKeyRange(r.Meta.StartKey, key)
	return region
}

func (r *Region) merge(endKey MvccKey) {
	r.Meta.EndKey = endKey
	r.incVersion()
}

func (r *Region) updateKeyRange(start, end MvccKey) {
	r.Meta.StartKey = start
	r.Meta.EndKey = end
	r.incVersion()
}

func (r *Region) incConfVer() {
	r.Meta.RegionEpoch = &metapb.RegionEpoch{
		ConfVer: r.Meta.GetRegionEpoch().GetConfVer() + 1,
		Version: r.Meta.GetRegionEpoch().GetVersion(),
	}
}

func (r *Region) incVersion() {
	r.Meta.RegionEpoch = &metapb.RegionEpoch{
		ConfVer: r.Meta.GetRegionEpoch().GetConfVer(),
		Version: r.Meta.GetRegionEpoch().GetVersion() + 1,
	}
}

// Store is the Store's meta data.
type Store struct {
	meta   *metapb.Store
	cancel bool // return context.Cancelled error when cancel is true.
}

func newStore(storeID uint64, addr string, peerAddr string, labels ...*metapb.StoreLabel) *Store {
	return &Store{
		meta: &metapb.Store{
			Id:          storeID,
			Address:     addr,
			PeerAddress: peerAddr,
			Labels:      labels,
		},
	}
}

func (s *Store) mergeLabels(labels []*metapb.StoreLabel) {
	if len(s.meta.Labels) < 1 {
		s.meta.Labels = labels
		return
	}
	kv := make(map[string]string, len(s.meta.Labels))
	for _, label := range s.meta.Labels {
		kv[label.Key] = label.Value
	}
	for _, label := range labels {
		kv[label.Key] = label.Value
	}
	mergedLabels := make([]*metapb.StoreLabel, 0, len(kv))
	for k, v := range kv {
		mergedLabels = append(mergedLabels, &metapb.StoreLabel{
			Key:   k,
			Value: v,
		})
	}
	s.meta.Labels = mergedLabels
}
