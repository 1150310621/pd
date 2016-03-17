package server

import (
	"bytes"
	"math"
	"math/rand"
	"net"
	"sync/atomic"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/golang/protobuf/proto"
	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pd_jobpb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/kvproto/pkg/raft_cmdpb"
	"github.com/pingcap/kvproto/pkg/raft_serverpb"
	"github.com/pingcap/kvproto/pkg/raftpb"
	"github.com/twinj/uuid"
	"golang.org/x/net/context"
)

const (
	checkJobInterval = 10 * time.Second

	connectTimeout = 3 * time.Second
	readTimeout    = 3 * time.Second
	writeTimeout   = 3 * time.Second

	maxSendRetry = 10
)

func (c *raftCluster) onJobWorker() {
	defer c.wg.Done()

	ticker := time.NewTicker(checkJobInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.quitCh:
			return
		case <-c.askJobCh:
			if !c.s.IsLeader() {
				log.Warnf("we are not leader, no need to handle job")
				continue
			}

			job, err := c.getJob()
			if err != nil {
				log.Errorf("get first job err %v", err)
			} else if job == nil {
				// no job now, wait
				continue
			}
			if err = c.handleJob(job); err != nil {
				log.Errorf("handle job %v err %v, retry", job, err)
				// wait and force retry
				time.Sleep(c.s.cfg.nextRetryDelay)
				asyncNotify(c.askJobCh)
				continue
			}

			if err = c.popJob(job); err != nil {
				log.Errorf("pop job %v err %v", job, err)
			}

			// Notify to job again.
			asyncNotify(c.askJobCh)
		case <-ticker.C:
			// Try to check job regularly.
			asyncNotify(c.askJobCh)
		}
	}
}

func asyncNotify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (c *raftCluster) postJob(req *raft_cmdpb.RaftCommandRequest) error {
	jobID, err := c.s.idAlloc.Alloc()
	if err != nil {
		return errors.Trace(err)
	}

	req.Header.Uuid = uuid.NewV4().Bytes()

	job := &pd_jobpd.Job{
		JobId:   proto.Uint64(jobID),
		Status:  pd_jobpd.JobStatus_Pending.Enum(),
		Request: req,
	}

	jobValue, err := proto.Marshal(job)
	if err != nil {
		return errors.Trace(err)
	}

	jobPath := makeJobKey(c.clusterRoot, jobID)

	resp, err := c.s.client.Txn(context.TODO()).
		If(c.s.leaderCmp()).
		Then(clientv3.OpPut(jobPath, string(jobValue))).
		Commit()
	if err != nil {
		return errors.Trace(err)
	} else if !resp.Succeeded {
		return errors.Errorf("post job %v fail", job)
	}

	// Tell job worker to handle the job
	asyncNotify(c.askJobCh)

	return nil
}

func (c *raftCluster) getJob() (*pd_jobpd.Job, error) {
	job := pd_jobpd.Job{}

	jobKey := makeJobKey(c.clusterRoot, 0)
	maxJobKey := makeJobKey(c.clusterRoot, math.MaxUint64)

	sortOpt := clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend)
	ok, err := getProtoMsg(c.s.client, jobKey, &job, clientv3.WithRange(maxJobKey), clientv3.WithLimit(1), sortOpt)
	if err != nil {
		return nil, errors.Trace(err)
	} else if !ok {
		return nil, nil
	}

	return &job, nil
}

func (c *raftCluster) popJob(job *pd_jobpd.Job) error {
	jobKey := makeJobKey(c.clusterRoot, job.GetJobId())
	resp, err := c.s.client.Txn(context.TODO()).
		If(c.s.leaderCmp()).
		Then(clientv3.OpDelete(jobKey)).
		Commit()
	if err != nil {
		return errors.Trace(err)
	} else if !resp.Succeeded {
		return errors.Errorf("pop first job failed")
	}
	return nil
}

func (c *raftCluster) updateJobStatus(job *pd_jobpd.Job, status pd_jobpd.JobStatus) error {
	jobKey := makeJobKey(c.clusterRoot, job.GetJobId())
	job.Status = status.Enum()
	jobValue, err := proto.Marshal(job)
	if err != nil {
		return errors.Trace(err)
	}

	resp, err := c.s.client.Txn(context.TODO()).
		If(c.s.leaderCmp()).
		Then(clientv3.OpPut(jobKey, string(jobValue))).
		Commit()
	if err != nil {
		return errors.Trace(err)
	} else if !resp.Succeeded {
		return errors.Errorf("pop first job failed")
	}
	return nil
}

func (c *raftCluster) handleJob(job *pd_jobpd.Job) error {
	log.Debugf("begin to handle job %v", job)

	// TODO: if the job status is running, check this job whether
	// finished or not in raft server.
	if job.GetStatus() == pd_jobpd.JobStatus_Pending {
		if err := c.updateJobStatus(job, pd_jobpd.JobStatus_Running); err != nil {
			return errors.Trace(err)
		}
	}

	req := job.GetRequest()
	switch req.AdminRequest.GetCmdType() {
	case raft_cmdpb.AdminCommandType_ChangePeer:
		return c.handleChangePeer(job)
	case raft_cmdpb.AdminCommandType_Split:
		return c.handleSplit(job)
	default:
		log.Errorf("invalid job command %v, ignore", req)
		return nil
	}
}

func (c *raftCluster) chooseStore(bestStores []metapb.Store, matchStores []metapb.Store) metapb.Store {
	var store metapb.Store
	// Select the store randomly, later we will do more better choice.

	if len(bestStores) > 0 {
		store = bestStores[rand.Intn(len(bestStores))]
	} else {
		store = matchStores[rand.Intn(len(matchStores))]
	}

	return store
}

func (c *raftCluster) handleAddPeerReq(region *metapb.Region) (*metapb.Peer, error) {
	peerID, err := c.s.idAlloc.Alloc()
	if err != nil {
		return nil, errors.Trace(err)
	}

	var (
		// The best stores are that the region has not in.
		bestStores []metapb.Store
		// The match stores are that region has not in these stores
		// but in the same node.
		matchStores []metapb.Store
	)

	mu := &c.mu
	mu.RLock()
	defer mu.RUnlock()

	// Find a proper store which the region has not in.
	for _, store := range mu.stores {
		storeID := store.GetStoreId()
		nodeID := store.GetNodeId()

		existNode := false
		existStore := false
		for _, peer := range region.Peers {
			if peer.GetStoreId() == storeID {
				// we can't add peer in the same store.
				existStore = true
				break
			} else if peer.GetNodeId() == nodeID {
				existNode = true
			}
		}

		if existStore {
			continue
		} else if existNode {
			matchStores = append(matchStores, store)
		} else {
			bestStores = append(bestStores, store)
		}
	}

	if len(bestStores) == 0 && len(matchStores) == 0 {
		return nil, errors.Errorf("find no store to add peer for region %v", region)
	}

	store := c.chooseStore(bestStores, matchStores)

	peer := &metapb.Peer{
		NodeId:  proto.Uint64(store.GetNodeId()),
		StoreId: proto.Uint64(store.GetStoreId()),
		PeerId:  proto.Uint64(peerID),
	}

	return peer, nil
}

// If leader is nil, we can remove any peer in the region, or else we can only remove none leader peer.
func (c *raftCluster) handleRemovePeerReq(region *metapb.Region, leader *metapb.Peer) (*metapb.Peer, error) {
	if len(region.Peers) <= 1 {
		return nil, errors.Errorf("can not remove peer for region %v", region)
	}

	for _, peer := range region.Peers {
		if peer.GetPeerId() != leader.GetPeerId() {
			return peer, nil
		}
	}

	// Maybe we can't enter here.
	return nil, errors.Errorf("find no proper peer to remove for region %v", region)
}

func (c *raftCluster) HandleAskChangePeer(request *pdpb.AskChangePeerRequest) error {
	clusterMeta, err := c.GetMeta()
	if err != nil {
		return errors.Trace(err)
	}

	var (
		maxPeerNumber = int(clusterMeta.GetMaxPeerNumber())
		region        = request.GetRegion()
		regionID      = region.GetRegionId()
		peerNumber    = len(region.GetPeers())
		changeType    raftpb.ConfChangeType
		peer          *metapb.Peer
	)

	if peerNumber == maxPeerNumber {
		log.Infof("region %d peer number equals %d, no need to change peer", regionID, maxPeerNumber)
		return nil
	} else if peerNumber < maxPeerNumber {
		log.Infof("region %d peer number %d < %d, need to add peer", regionID, peerNumber, maxPeerNumber)
		changeType = raftpb.ConfChangeType_AddNode
		if peer, err = c.handleAddPeerReq(region); err != nil {
			return errors.Trace(err)
		}
	} else if peerNumber > maxPeerNumber {
		log.Infof("region %d peer number %d > %d, need to remove peer", regionID, peerNumber, maxPeerNumber)
		changeType = raftpb.ConfChangeType_RemoveNode
		if peer, err = c.handleRemovePeerReq(region, request.Leader); err != nil {
			return errors.Trace(err)
		}
	}

	changePeer := &raft_cmdpb.AdminRequest{
		CmdType: raft_cmdpb.AdminCommandType_ChangePeer.Enum(),
		ChangePeer: &raft_cmdpb.ChangePeerRequest{
			ChangeType: changeType.Enum(),
			Peer:       peer,
			Region:     region,
		},
	}

	req := &raft_cmdpb.RaftCommandRequest{
		Header: &raft_cmdpb.RaftRequestHeader{
			RegionId: proto.Uint64(regionID),
			Peer:     request.Leader,
		},
		AdminRequest: changePeer,
	}

	return c.postJob(req)
}

func (c *raftCluster) handleChangePeer(job *pd_jobpd.Job) error {
	request := job.Request
	response, err := c.sendRaftCommand(request, request.AdminRequest.ChangePeer.Region)
	if err != nil {
		return errors.Trace(err)
	}

	var changePeer *raft_cmdpb.ChangePeerResponse

	if response.Header != nil && response.Header.Error != nil {
		log.Errorf("handle %v but failed with response %v, check in raft server", job.Request, response.Header.Error)
		changePeer, err = c.checkChangePeerOK(job.Request)
		if err != nil {
			return errors.Trace(err)
		} else if changePeer == nil {
			log.Warnf("raft server doesn't execute %v, cancel it", job.Request)
			return nil
		}
	} else {
		// Must be change peer response here
		// TODO: check this error later.
		changePeer = response.AdminResponse.ChangePeer
	}

	region := changePeer.Region

	// Update region
	regionSearchPath := makeRegionSearchKey(c.clusterRoot, region.GetEndKey())
	regionValue, err := proto.Marshal(region)
	if err != nil {
		return errors.Trace(err)
	}

	resp, err := c.s.client.Txn(context.TODO()).
		If(c.s.leaderCmp()).
		Then(clientv3.OpPut(regionSearchPath, string(regionValue))).
		Commit()
	if err != nil {
		return errors.Trace(err)
	} else if !resp.Succeeded {
		return errors.New("update change peer region failed")
	}

	return nil
}

func (c *raftCluster) checkChangePeerOK(request *raft_cmdpb.RaftCommandRequest) (*raft_cmdpb.ChangePeerResponse, error) {
	// TODO: check region conf change version later.
	regionID := request.Header.GetRegionId()
	leader := request.Header.Peer
	detail, err := c.getRegionDetail(regionID, leader)
	if err != nil {
		return nil, errors.Trace(err)
	}

	changePeer := request.AdminRequest.ChangePeer
	found := false
	for _, peer := range detail.Region.Peers {
		if peer.GetPeerId() == changePeer.Peer.GetPeerId() {
			found = true
			break
		}
	}

	changeType := changePeer.GetChangeType()
	// For add peer, if change peer is already in raft server region, we can think the command has
	// been already applied, for remove peer, the peer is not in region now.
	if (changeType == raftpb.ConfChangeType_AddNode && found) ||
		(changeType == raftpb.ConfChangeType_RemoveNode && !found) {
		return &raft_cmdpb.ChangePeerResponse{
			Region: detail.Region,
		}, nil
	}

	// Here means the raft server doesn't execute this change peer command.
	return nil, nil
}

func (c *raftCluster) HandleAskSplit(request *pdpb.AskSplitRequest) error {
	newRegionID, err := c.s.idAlloc.Alloc()
	if err != nil {
		return errors.Trace(err)
	}

	peerIDs := make([]uint64, len(request.Region.Peers))
	for i := 0; i < len(peerIDs); i++ {
		if peerIDs[i], err = c.s.idAlloc.Alloc(); err != nil {
			return errors.Trace(err)
		}
	}

	split := &raft_cmdpb.AdminRequest{
		CmdType: raft_cmdpb.AdminCommandType_Split.Enum(),
		Split: &raft_cmdpb.SplitRequest{
			NewRegionId: proto.Uint64(newRegionID),
			NewPeerIds:  peerIDs,
			SplitKey:    request.SplitKey,
			Region:      request.Region,
		},
	}

	req := &raft_cmdpb.RaftCommandRequest{
		Header: &raft_cmdpb.RaftRequestHeader{
			RegionId: request.Region.RegionId,
			Peer:     request.Leader,
		},
		AdminRequest: split,
	}

	return c.postJob(req)
}

func (c *raftCluster) handleSplit(job *pd_jobpd.Job) error {
	request := job.Request
	response, err := c.sendRaftCommand(request, request.AdminRequest.Split.Region)
	if err != nil {
		return errors.Trace(err)
	}

	var split *raft_cmdpb.SplitResponse
	if response.Header != nil && response.Header.Error != nil {
		log.Errorf("handle %v but failed with response %v, check in raft server", job.Request, response.Header.Error)
		split, err = c.checkSplitOK(job.Request)
		if err != nil {
			return errors.Trace(err)
		} else if split == nil {
			log.Warnf("raft server doesn't execute %v, cancel it", job.Request)
			return nil
		}
	} else {
		// Must be split response here
		// TODO: check this error later.
		split = response.AdminResponse.Split
	}

	left := split.Left
	right := split.Right

	// Update region
	leftSearchPath := makeRegionSearchKey(c.clusterRoot, left.GetEndKey())
	rightSearchPath := makeRegionSearchKey(c.clusterRoot, right.GetEndKey())

	leftValue, err := proto.Marshal(left)
	if err != nil {
		return errors.Trace(err)
	}

	rightValue, err := proto.Marshal(right)
	if err != nil {
		return errors.Trace(err)
	}

	var ops []clientv3.Op

	leftPath := makeRegionKey(c.clusterRoot, left.GetRegionId())
	rightPath := makeRegionKey(c.clusterRoot, right.GetRegionId())

	ops = append(ops, clientv3.OpPut(leftPath, encodeRegionSearchKey(left.GetEndKey())))
	ops = append(ops, clientv3.OpPut(rightPath, encodeRegionSearchKey(right.GetEndKey())))
	ops = append(ops, clientv3.OpPut(leftSearchPath, string(leftValue)))
	ops = append(ops, clientv3.OpPut(rightSearchPath, string(rightValue)))

	var cmps []clientv3.Cmp
	cmps = append(cmps, c.s.leaderCmp())
	// new left search path must not exist
	cmps = append(cmps, clientv3.Compare(clientv3.CreatedRevision(leftSearchPath), "=", 0))
	// new right search path must exist, because it is the same as origin split path.
	cmps = append(cmps, clientv3.Compare(clientv3.CreatedRevision(rightSearchPath), ">", 0))
	cmps = append(cmps, clientv3.Compare(clientv3.CreatedRevision(rightPath), "=", 0))

	resp, err := c.s.client.Txn(context.TODO()).
		If(cmps...).
		Then(ops...).
		Commit()
	if err != nil {
		return errors.Trace(err)
	} else if !resp.Succeeded {
		return errors.New("update split region failed")
	}

	return nil
}

func (c *raftCluster) checkSplitOK(request *raft_cmdpb.RaftCommandRequest) (*raft_cmdpb.SplitResponse, error) {
	// TODO: check region version later.
	split := request.AdminRequest.Split
	region := split.Region
	leftRegionID := region.GetRegionId()
	rightRegionID := split.GetNewRegionId()
	leader := request.Header.Peer
	leftDetail, err := c.getRegionDetail(leftRegionID, leader)
	if err != nil {
		return nil, errors.Trace(err)
	}

	if !bytes.Equal(leftDetail.Region.GetEndKey(), split.SplitKey) {
		// The region is not split
		return nil, nil
	}

	rightDetail, err := c.getRegionDetail(rightRegionID, leader)
	if err != nil {
		return nil, errors.Trace(err)
	}

	resp := &raft_cmdpb.SplitResponse{
		Left:  leftDetail.Region,
		Right: rightDetail.Region,
	}

	return resp, nil
}

func (c *raftCluster) sendRaftCommand(request *raft_cmdpb.RaftCommandRequest, region *metapb.Region) (*raft_cmdpb.RaftCommandResponse, error) {
	originPeer := request.Header.Peer

RETRY:
	for i := 0; i < maxSendRetry; i++ {
		resp, err := c.callCommand(request)
		if err != nil {
			// We may meet some error, maybe network broken, node down, etc.
			// We can check later next time.
			return nil, errors.Trace(err)
		}

		if resp.Header.Error != nil && resp.Header.Error.NotLeader != nil {
			log.Warnf("peer %v is not leader, we got %v", request.Header.Peer, resp.Header.Error)

			leader := resp.Header.Error.NotLeader.Leader
			if leader != nil {
				// The origin peer is not leader and we get the new leader,
				// send this message to the new leader again.
				request.Header.Peer = leader
				continue
			}

			regionID := region.GetRegionId()
			// The origin peer is not leader, but we can't get the leader now,
			// so we try to get the leader in other region peers.
			for _, peer := range region.Peers {
				if peer.GetPeerId() == originPeer.GetPeerId() {
					continue
				}

				leader, err := c.getRegionLeader(regionID, peer)
				if err != nil {
					log.Errorf("get region %d leader err %v", regionID, err)
					continue
				} else if leader == nil {
					log.Infof("can not get leader for region %d in peer %v", regionID, peer)
					continue
				}

				// We get leader here.
				request.Header.Peer = leader
				continue RETRY
			}
		}

		return resp, nil
	}

	return nil, errors.Errorf("send raft command %v failed", request)
}

func (c *raftCluster) callCommand(request *raft_cmdpb.RaftCommandRequest) (*raft_cmdpb.RaftCommandResponse, error) {
	nodeID := request.Header.Peer.GetNodeId()

	node, err := c.GetNode(nodeID)
	if err != nil {
		return nil, errors.Trace(err)
	}

	// Connect the node.
	// TODO: use connection pool
	conn, err := net.DialTimeout("tcp", node.GetAddress(), connectTimeout)
	if err != nil {
		return nil, errors.Trace(err)
	}

	defer conn.Close()

	msg := &raft_serverpb.Message{
		MsgType: raft_serverpb.MessageType_Command.Enum(),
		CmdReq:  request,
	}

	msgID := atomic.AddUint64(&c.s.msgID, 1)
	if err = writeMessage(conn, msgID, msg); err != nil {
		return nil, errors.Trace(err)
	}

	msg.Reset()
	if _, err = readMessage(conn, msg); err != nil {
		return nil, errors.Trace(err)
	}

	if msg.CmdResp == nil {
		// This is a very serious bug, should we panic here?
		return nil, errors.Errorf("invalid command response message but %v", msg)
	}

	return msg.CmdResp, nil
}

func (c *raftCluster) getRegionLeader(regionID uint64, peer *metapb.Peer) (*metapb.Peer, error) {
	request := &raft_cmdpb.RaftCommandRequest{
		Header: &raft_cmdpb.RaftRequestHeader{
			Uuid:     uuid.NewV4().Bytes(),
			RegionId: proto.Uint64(regionID),
			Peer:     peer,
		},
		StatusRequest: &raft_cmdpb.StatusRequest{
			CmdType:      raft_cmdpb.StatusCommandType_RegionLeader.Enum(),
			RegionLeader: &raft_cmdpb.RegionLeaderRequest{},
		},
	}

	resp, err := c.callCommand(request)
	if err != nil {
		return nil, errors.Trace(err)
	}

	if resp.StatusResponse != nil && resp.StatusResponse.RegionLeader != nil {
		return resp.StatusResponse.RegionLeader.Leader, nil
	}

	return nil, errors.Errorf("get region %d leader failed, got resp %v", regionID, resp)
}

func (c *raftCluster) getRegionDetail(regionID uint64, peer *metapb.Peer) (*raft_cmdpb.RegionDetailResponse, error) {
	request := &raft_cmdpb.RaftCommandRequest{
		Header: &raft_cmdpb.RaftRequestHeader{
			Uuid:     uuid.NewV4().Bytes(),
			RegionId: proto.Uint64(regionID),
			Peer:     peer,
		},
		StatusRequest: &raft_cmdpb.StatusRequest{
			CmdType:      raft_cmdpb.StatusCommandType_RegionDetail.Enum(),
			RegionDetail: &raft_cmdpb.RegionDetailRequest{},
		},
	}

	resp, err := c.callCommand(request)
	if err != nil {
		return nil, errors.Trace(err)
	}

	if resp.StatusResponse != nil && resp.StatusResponse.RegionDetail != nil {
		return resp.StatusResponse.RegionDetail, nil
	}

	return nil, errors.Errorf("get region %d detail failed, got resp %v", regionID, resp)
}
