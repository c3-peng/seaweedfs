package topology

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"

	"github.com/chrislusf/raft"
	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/pb/master_pb"
	"github.com/chrislusf/seaweedfs/weed/sequence"
	"github.com/chrislusf/seaweedfs/weed/storage"
	"github.com/chrislusf/seaweedfs/weed/storage/needle"
	"github.com/chrislusf/seaweedfs/weed/util"
)

type Topology struct {
	NodeImpl

	collectionMap  *util.ConcurrentReadMap
	ecShardMap     map[needle.VolumeId]*EcShardLocations
	ecShardMapLock sync.RWMutex

	pulse int64

	volumeSizeLimit uint64

	Sequence sequence.Sequencer

	chanFullVolumes chan storage.VolumeInfo

	Configuration *Configuration

	RaftServer raft.Server
}

func NewTopology(id string, seq sequence.Sequencer, volumeSizeLimit uint64, pulse int) *Topology {
	t := &Topology{}
	t.id = NodeId(id)
	t.nodeType = "Topology"
	t.NodeImpl.value = t
	t.children = make(map[NodeId]Node)
	t.collectionMap = util.NewConcurrentReadMap()
	t.ecShardMap = make(map[needle.VolumeId]*EcShardLocations)
	t.pulse = int64(pulse)
	t.volumeSizeLimit = volumeSizeLimit

	t.Sequence = seq

	t.chanFullVolumes = make(chan storage.VolumeInfo)

	t.Configuration = &Configuration{}

	return t
}

func (t *Topology) IsLeader() bool {
	if t.RaftServer != nil {
		return t.RaftServer.State() == raft.Leader
	}
	return false
}

func (t *Topology) Leader() (string, error) {
	l := ""
	if t.RaftServer != nil {
		l = t.RaftServer.Leader()
	} else {
		return "", errors.New("Raft Server not ready yet!")
	}

	if l == "" {
		// We are a single node cluster, we are the leader
		return t.RaftServer.Name(), errors.New("Raft Server not initialized!")
	}

	return l, nil
}

func (t *Topology) Lookup(collection string, vid needle.VolumeId) (dataNodes []*DataNode) {
	//maybe an issue if lots of collections?
	if collection == "" {
		for _, c := range t.collectionMap.Items() {
			if list := c.(*Collection).Lookup(vid); list != nil {
				return list
			}
		}
	} else {
		if c, ok := t.collectionMap.Find(collection); ok {
			return c.(*Collection).Lookup(vid)
		}
	}

	if locations, found := t.LookupEcShards(vid); found {
		for _, loc := range locations.Locations {
			dataNodes = append(dataNodes, loc...)
		}
		return dataNodes
	}

	return nil
}

func (t *Topology) NextVolumeId() (needle.VolumeId, error) {
	vid := t.GetMaxVolumeId()
	next := vid.Next()
	if _, err := t.RaftServer.Do(NewMaxVolumeIdCommand(next)); err != nil {
		return 0, err
	}
	return next, nil
}

func (t *Topology) HasWritableVolume(option *VolumeGrowOption) bool {
	vl := t.GetVolumeLayout(option.Collection, option.ReplicaPlacement, option.Ttl)
	return vl.GetActiveVolumeCount(option) > 0
}

func (t *Topology) PickForWrite(count uint64, option *VolumeGrowOption) (string, uint64, *DataNode, error) {
	vid, count, datanodes, err := t.GetVolumeLayout(option.Collection, option.ReplicaPlacement, option.Ttl).PickForWrite(count, option)
	if err != nil {
		return "", 0, nil, fmt.Errorf("failed to find writable volumes for collectio:%s replication:%s ttl:%s error: %v", option.Collection, option.ReplicaPlacement.String(), option.Ttl.String(), err)
	}
	if datanodes.Length() == 0 {
		return "", 0, nil, fmt.Errorf("no writable volumes available for for collectio:%s replication:%s ttl:%s", option.Collection, option.ReplicaPlacement.String(), option.Ttl.String())
	}
	fileId, count := t.Sequence.NextFileId(count)
	return needle.NewFileId(*vid, fileId, rand.Uint32()).String(), count, datanodes.Head(), nil
}

func (t *Topology) GetVolumeLayout(collectionName string, rp *storage.ReplicaPlacement, ttl *needle.TTL) *VolumeLayout {
	return t.collectionMap.Get(collectionName, func() interface{} {
		return NewCollection(collectionName, t.volumeSizeLimit)
	}).(*Collection).GetOrCreateVolumeLayout(rp, ttl)
}

func (t *Topology) ListCollections(includeNormalVolumes, includeEcVolumes bool) (ret []string) {

	mapOfCollections := make(map[string]bool)
	for _, c := range t.collectionMap.Items() {
		mapOfCollections[c.(*Collection).Name] = true
	}

	if includeEcVolumes {
		t.ecShardMapLock.RLock()
		for _, ecVolumeLocation := range t.ecShardMap {
			mapOfCollections[ecVolumeLocation.Collection] = true
		}
		t.ecShardMapLock.RUnlock()
	}

	for k, _ := range mapOfCollections {
		ret = append(ret, k)
	}
	return ret
}

func (t *Topology) FindCollection(collectionName string) (*Collection, bool) {
	c, hasCollection := t.collectionMap.Find(collectionName)
	if !hasCollection {
		return nil, false
	}
	return c.(*Collection), hasCollection
}

func (t *Topology) DeleteCollection(collectionName string) {
	t.collectionMap.Delete(collectionName)
}

func (t *Topology) RegisterVolumeLayout(v storage.VolumeInfo, dn *DataNode) {
	t.GetVolumeLayout(v.Collection, v.ReplicaPlacement, v.Ttl).RegisterVolume(&v, dn)
}
func (t *Topology) UnRegisterVolumeLayout(v storage.VolumeInfo, dn *DataNode) {
	glog.Infof("removing volume info:%+v", v)
	volumeLayout := t.GetVolumeLayout(v.Collection, v.ReplicaPlacement, v.Ttl)
	volumeLayout.UnRegisterVolume(&v, dn)
	if volumeLayout.isEmpty() {
		t.DeleteCollection(v.Collection)
	}
}

func (t *Topology) GetOrCreateDataCenter(dcName string) *DataCenter {
	for _, c := range t.Children() {
		dc := c.(*DataCenter)
		if string(dc.Id()) == dcName {
			return dc
		}
	}
	dc := NewDataCenter(dcName)
	t.LinkChildNode(dc)
	return dc
}

func (t *Topology) SyncDataNodeRegistration(volumes []*master_pb.VolumeInformationMessage, dn *DataNode) (newVolumes, deletedVolumes []storage.VolumeInfo) {
	// convert into in memory struct storage.VolumeInfo
	var volumeInfos []storage.VolumeInfo
	for _, v := range volumes {
		if vi, err := storage.NewVolumeInfo(v); err == nil {
			volumeInfos = append(volumeInfos, vi)
		} else {
			glog.V(0).Infof("Fail to convert joined volume information: %v", err)
		}
	}
	// find out the delta volumes
	newVolumes, deletedVolumes = dn.UpdateVolumes(volumeInfos)
	for _, v := range newVolumes {
		t.RegisterVolumeLayout(v, dn)
	}
	for _, v := range deletedVolumes {
		t.UnRegisterVolumeLayout(v, dn)
	}
	return
}

func (t *Topology) IncrementalSyncDataNodeRegistration(newVolumes, deletedVolumes []*master_pb.VolumeShortInformationMessage, dn *DataNode) {
	var newVis, oldVis []storage.VolumeInfo
	for _, v := range newVolumes {
		vi, err := storage.NewVolumeInfoFromShort(v)
		if err != nil {
			glog.V(0).Infof("NewVolumeInfoFromShort %v: %v", v, err)
			continue
		}
		newVis = append(newVis, vi)
	}
	for _, v := range deletedVolumes {
		vi, err := storage.NewVolumeInfoFromShort(v)
		if err != nil {
			glog.V(0).Infof("NewVolumeInfoFromShort %v: %v", v, err)
			continue
		}
		oldVis = append(oldVis, vi)
	}
	dn.DeltaUpdateVolumes(newVis, oldVis)

	for _, vi := range newVis {
		t.RegisterVolumeLayout(vi, dn)
	}
	for _, vi := range oldVis {
		t.UnRegisterVolumeLayout(vi, dn)
	}

	return
}
