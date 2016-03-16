package server

import (
	"net"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/juju/errors"
	. "github.com/pingcap/check"
	"github.com/pingcap/kvproto/pkg/errorpb"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/kvproto/pkg/raft_cmdpb"
	"github.com/pingcap/kvproto/pkg/raft_serverpb"
	"github.com/pingcap/kvproto/pkg/raftpb"
)

var _ = Suite(&testClusterWorkerSuite{})

type testClusterWorkerSuite struct {
	testClusterBaseSuite

	clusterID uint64

	nodeLock sync.Mutex
	nodes    map[uint64]*mockRaftNode

	regionLeaderLock sync.Mutex
	regionLeaders    map[uint64]metapb.Peer

	quitCh chan struct{}
}

func (s *testClusterWorkerSuite) getRootPath() string {
	return "test_cluster_worker"
}

type mockRaftPeer struct {
	peer   metapb.Peer
	region metapb.Region
}

type mockRaftStore struct {
	sync.Mutex

	s *testClusterWorkerSuite

	storeIdent raft_serverpb.StoreIdent

	peers map[uint64]*mockRaftPeer
}

type mockRaftMsg struct {
	peer   metapb.Peer
	region metapb.Region
	req    *raft_cmdpb.RaftCommandRequest
}

type mockRaftNode struct {
	sync.Mutex

	s *testClusterWorkerSuite

	node metapb.Node

	listener net.Listener

	stores map[uint64]*mockRaftStore

	raftMsgCh chan *mockRaftMsg
}

func (s *testClusterWorkerSuite) bootstrap(c *C) *mockRaftNode {
	req := s.newBootstrapRequest(c, s.clusterID, "127.0.0.1:0")
	node := req.Bootstrap.Node
	store := req.Bootstrap.Stores[0]
	region := req.Bootstrap.Region

	err := s.svr.bootstrapCluster(s.clusterID, req.Bootstrap)
	c.Assert(err, IsNil)

	raftNode := s.newMockRaftNode(c, node)
	raftStore := raftNode.addStore(c, store)
	raftStore.addRegion(c, region)
	return raftNode
}

func (s *testClusterWorkerSuite) newMockRaftNode(c *C, n *metapb.Node) *mockRaftNode {
	if n == nil {
		n = s.newNode(c, 0, "127.0.0.1:0")
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	c.Assert(err, IsNil)

	addr := l.Addr().String()
	n.Address = proto.String(addr)
	node := &mockRaftNode{
		s:         s,
		node:      *n,
		listener:  l,
		stores:    make(map[uint64]*mockRaftStore),
		raftMsgCh: make(chan *mockRaftMsg, 1024),
	}

	go node.runCmd(c)
	go node.runRaft(c)

	cluster, err := s.svr.getCluster(s.clusterID)
	c.Assert(err, IsNil)

	cluster.PutNode(&node.node)

	s.nodeLock.Lock()
	defer s.nodeLock.Unlock()

	s.nodes[n.GetNodeId()] = node

	return node
}

func (s *testClusterWorkerSuite) sendRaftMsg(c *C, msg *mockRaftMsg) {
	nodeID := msg.peer.GetNodeId()

	s.nodeLock.Lock()
	defer s.nodeLock.Unlock()

	node, ok := s.nodes[nodeID]
	if !ok {
		return
	}

	select {
	case node.raftMsgCh <- msg:
	default:
		c.Logf("can not send msg to %v", msg.peer)
	}
}

func (s *testClusterWorkerSuite) broadcastRaftMsg(c *C, leader *mockRaftPeer,
	req *raft_cmdpb.RaftCommandRequest) {
	region := leader.region
	for _, peer := range region.Peers {
		if peer.GetPeerId() != leader.peer.GetPeerId() {
			msg := &mockRaftMsg{
				peer:   *peer,
				region: *proto.Clone(&region).(*metapb.Region),
				req:    req,
			}
			s.sendRaftMsg(c, msg)
		}
	}

	// We should handle ConfChangeType_AddNode specially, because here the leader's
	// region doesn't contain this peer.
	if req.AdminRequest != nil && req.AdminRequest.ChangePeer != nil {
		changePeer := req.AdminRequest.ChangePeer
		if changePeer.GetChangeType() == raftpb.ConfChangeType_AddNode {
			c.Assert(changePeer.Peer.GetPeerId(), Not(Equals), leader.peer.GetPeerId())
			msg := &mockRaftMsg{
				peer:   *changePeer.Peer,
				region: *proto.Clone(&region).(*metapb.Region),
				req:    req,
			}
			s.sendRaftMsg(c, msg)
		}
	}
}

func (n *mockRaftNode) addStore(c *C, s *metapb.Store) *mockRaftStore {
	n.Lock()
	defer n.Unlock()

	if s == nil {
		s = n.s.newStore(c, n.node.GetNodeId(), 0)
	} else {
		c.Assert(s.GetNodeId(), Equals, n.node.GetNodeId())
	}

	store := &mockRaftStore{
		s: n.s,
		storeIdent: raft_serverpb.StoreIdent{
			ClusterId: proto.Uint64(n.s.clusterID),
			NodeId:    proto.Uint64(n.node.GetNodeId()),
			StoreId:   proto.Uint64(s.GetStoreId()),
		},
		peers: make(map[uint64]*mockRaftPeer),
	}

	n.stores[s.GetStoreId()] = store

	cluster, err := n.s.svr.getCluster(n.s.clusterID)
	c.Assert(err, IsNil)
	cluster.PutStore(s)

	return store
}

func (s *mockRaftStore) addRegion(c *C, region *metapb.Region) {
	s.Lock()
	defer s.Unlock()

	storeID := s.storeIdent.GetStoreId()
	var (
		peer  metapb.Peer
		found = false
	)

	for _, p := range region.Peers {
		if p.GetStoreId() == storeID {
			peer = *p
			found = true
			break
		}
	}
	c.Assert(found, IsTrue)
	s.peers[region.GetRegionId()] = &mockRaftPeer{
		peer:   peer,
		region: *proto.Clone(region).(*metapb.Region),
	}
}

func (n *mockRaftNode) runCmd(c *C) {
	for {
		conn, err := n.listener.Accept()
		if err != nil {
			c.Logf("accept err %v", err)
			return
		}

		msg := &raft_serverpb.Message{}
		msgID, err := readMessage(conn, msg)
		c.Assert(err, IsNil)

		req := msg.GetCmdReq()
		c.Assert(req, NotNil)

		resp := n.proposeCommand(c, req)
		if resp.Header == nil {
			resp.Header = &raft_cmdpb.RaftResponseHeader{}
		}
		resp.Header.Uuid = req.Header.Uuid

		respMsg := &raft_serverpb.Message{
			MsgType: raft_serverpb.MessageType_CommandResp.Enum(),
			CmdResp: resp,
		}

		err = writeMessage(conn, msgID, respMsg)
		c.Assert(err, IsNil)
	}
}

func (n *mockRaftNode) runRaft(c *C) {
	for {
		select {
		case msg := <-n.raftMsgCh:
			n.handleRaftMsg(c, msg)
		case <-n.s.quitCh:
			return
		}
	}
}

func (n *mockRaftNode) handleRaftMsg(c *C, msg *mockRaftMsg) {
	storeID := msg.peer.GetStoreId()
	n.Lock()
	store, ok := n.stores[storeID]
	n.Unlock()
	if !ok {
		return
	}

	store.Lock()
	defer store.Unlock()

	regionID := msg.region.GetRegionId()
	_, ok = store.peers[regionID]
	if !ok {
		// No peer, create it.
		store.peers[regionID] = &mockRaftPeer{
			peer:   msg.peer,
			region: msg.region,
		}
	}

	// TODO: all nodes must have same response, check later.
	store.handleWriteCommand(c, msg.req)
}

func newErrorCmdResponse(err error) *raft_cmdpb.RaftCommandResponse {
	resp := &raft_cmdpb.RaftCommandResponse{
		Header: &raft_cmdpb.RaftResponseHeader{
			Error: &errorpb.Error{
				Message: proto.String(err.Error()),
			},
		},
	}
	return resp
}

func (n *mockRaftNode) proposeCommand(c *C, req *raft_cmdpb.RaftCommandRequest) *raft_cmdpb.RaftCommandResponse {
	storeID := req.Header.Peer.GetStoreId()
	n.Lock()
	store, ok := n.stores[storeID]
	n.Unlock()
	if !ok {
		return newErrorCmdResponse(errors.Errorf("store %d is not found", storeID))
	}

	store.Lock()
	defer store.Unlock()

	regionID := req.Header.GetRegionId()
	peer, ok := store.peers[regionID]
	if !ok {
		resp := newErrorCmdResponse(errors.New("region not found"))
		resp.Header.Error.RegionNotFound = &errorpb.RegionNotFoundError{
			RegionId: proto.Uint64(req.Header.GetRegionId()),
		}
		return resp
	}

	if req.StatusRequest != nil {
		return store.handleStatusRequest(c, req)
	}

	// lock leader to prevent outer test change it.
	n.s.regionLeaderLock.Lock()
	defer n.s.regionLeaderLock.Unlock()

	leader, ok := n.s.regionLeaders[regionID]
	if ok && leader.GetPeerId() != peer.peer.GetPeerId() {
		resp := newErrorCmdResponse(errors.New("peer not leader"))
		resp.Header.Error.NotLeader = &errorpb.NotLeaderError{
			RegionId: proto.Uint64(regionID),
			Leader:   &leader,
		}
		return resp
	}

	// send the request to other peers.
	n.s.broadcastRaftMsg(c, peer, req)
	resp := store.handleWriteCommand(c, req)

	// update the region leader.
	n.s.regionLeaders[regionID] = peer.peer

	return resp
}

func (s *mockRaftStore) handleWriteCommand(c *C, req *raft_cmdpb.RaftCommandRequest) *raft_cmdpb.RaftCommandResponse {
	if req.AdminRequest != nil {
		return s.handleAdminRequest(c, req)
	}
	return newErrorCmdResponse(errors.Errorf("unsupported request %v", req))
}

func (s *mockRaftStore) handleStatusRequest(c *C, req *raft_cmdpb.RaftCommandRequest) *raft_cmdpb.RaftCommandResponse {
	// TODO later
	return newErrorCmdResponse(errors.Errorf("unsupported request %v", req))
}

func (s *mockRaftStore) handleAdminRequest(c *C, req *raft_cmdpb.RaftCommandRequest) *raft_cmdpb.RaftCommandResponse {
	var resp *raft_cmdpb.RaftCommandResponse
	switch req.AdminRequest.GetCmdType() {
	case raft_cmdpb.AdminCommandType_ChangePeer:
		resp = s.handleChangePeer(c, req)
	case raft_cmdpb.AdminCommandType_Split:
		resp = s.handleSplit(c, req)
	}

	if resp.AdminResponse != nil {
		resp.AdminResponse.CmdType = req.AdminRequest.CmdType
	}
	return resp
}

func (s *mockRaftStore) handleChangePeer(c *C, req *raft_cmdpb.RaftCommandRequest) *raft_cmdpb.RaftCommandResponse {
	changePeer := req.AdminRequest.ChangePeer
	confType := changePeer.GetChangeType()
	peer := changePeer.Peer

	raftPeer := s.peers[req.Header.GetRegionId()]
	region := raftPeer.region
	c.Assert(region.GetRegionId(), Equals, req.Header.GetRegionId())

	if confType == raftpb.ConfChangeType_AddNode {
		for _, p := range region.Peers {
			if p.GetPeerId() == peer.GetPeerId() || p.GetStoreId() == peer.GetStoreId() {
				return newErrorCmdResponse(errors.Errorf("add duplicated peer %v for region %v", peer, region))
			}
		}
		c.Assert(peer.GetPeerId(), Greater, region.GetMaxPeerId())
		region.Peers = append(region.Peers, peer)
		region.MaxPeerId = proto.Uint64(peer.GetPeerId())
		raftPeer.region = region
	} else {
		foundIndex := -1
		for i, p := range region.Peers {
			if p.GetPeerId() == peer.GetPeerId() {
				foundIndex = i
				break
			}
		}

		if foundIndex == -1 {
			return newErrorCmdResponse(errors.Errorf("remove missing peer %v for region %v", peer, region))
		}

		region.Peers = append(region.Peers[:foundIndex], region.Peers[foundIndex+1:]...)
		raftPeer.region = region

		// remove itself
		if peer.GetStoreId() == s.storeIdent.GetStoreId() {
			delete(s.peers, region.GetRegionId())
		}
	}

	resp := &raft_cmdpb.RaftCommandResponse{
		AdminResponse: &raft_cmdpb.AdminResponse{
			ChangePeer: &raft_cmdpb.ChangePeerResponse{
				Region: &region,
			},
		},
	}
	return resp
}

func (s *mockRaftStore) handleSplit(c *C, req *raft_cmdpb.RaftCommandRequest) *raft_cmdpb.RaftCommandResponse {
	split := req.AdminRequest.Split
	raftPeer := s.peers[req.Header.GetRegionId()]
	splitKey := split.SplitKey
	newRegionID := split.GetNewRegionId()
	newPeerIDs := split.GetNewPeerIds()

	region := raftPeer.region

	c.Assert(newPeerIDs, HasLen, len(region.Peers))

	c.Assert(string(splitKey), Greater, string(region.GetStartKey()))
	if len(region.GetEndKey()) > 0 {
		c.Assert(string(splitKey), Less, string(region.GetEndKey()))
	}

	newRegion := &metapb.Region{
		RegionId: proto.Uint64(newRegionID),
		Peers:    make([]*metapb.Peer, len(newPeerIDs)),
		StartKey: splitKey,
		EndKey:   append([]byte(nil), region.GetEndKey()...),
	}

	var newPeer metapb.Peer

	maxPeerID := uint64(0)
	for i, id := range newPeerIDs {
		peer := *region.Peers[i]
		peer.PeerId = proto.Uint64(id)

		if peer.GetStoreId() == s.storeIdent.GetStoreId() {
			newPeer = peer
		}

		if id > maxPeerID {
			maxPeerID = id
		}
		newRegion.Peers[i] = &peer
	}

	newRegion.MaxPeerId = proto.Uint64(maxPeerID)
	region.EndKey = append([]byte(nil), splitKey...)

	raftPeer.region = region
	s.peers[newRegionID] = &mockRaftPeer{
		peer:   newPeer,
		region: *newRegion,
	}

	resp := &raft_cmdpb.RaftCommandResponse{
		AdminResponse: &raft_cmdpb.AdminResponse{
			Split: &raft_cmdpb.SplitResponse{
				Left:  &region,
				Right: newRegion,
			},
		},
	}
	return resp
}

func (s *testClusterWorkerSuite) SetUpSuite(c *C) {
	s.clusterID = 1

	s.nodes = make(map[uint64]*mockRaftNode)

	s.svr = newTestServer(c, s.getRootPath())

	s.client = newEtcdClient(c)

	s.regionLeaders = make(map[uint64]metapb.Peer)

	s.quitCh = make(chan struct{})

	deleteRoot(c, s.client, s.getRootPath())

	go s.svr.Run()

	mustGetLeader(c, s.client, s.getRootPath())

	// Construct the raft cluster, 3 nodes, n1, n2, and n3
	// and 5 stores, s11, s12 in n1, s21, s22 in n2 and s31 in n3.
	raftNode1 := s.bootstrap(c)
	raftNode1.addStore(c, nil)

	raftNode2 := s.newMockRaftNode(c, nil)
	raftNode2.addStore(c, nil)
	raftNode2.addStore(c, nil)

	raftNode3 := s.newMockRaftNode(c, nil)
	raftNode3.addStore(c, nil)

	cluster, err := s.svr.getCluster(s.clusterID)
	c.Assert(err, IsNil)
	cluster.PutMeta(&metapb.Cluster{
		ClusterId:     proto.Uint64(s.clusterID),
		MaxPeerNumber: proto.Uint32(5),
	})

	nodes, err := cluster.GetAllNodes()
	c.Assert(err, IsNil)
	c.Assert(nodes, HasLen, 3)

	stores, err := cluster.GetAllStores()
	c.Assert(err, IsNil)
	c.Assert(stores, HasLen, 5)
}

func (s *testClusterWorkerSuite) TearDownSuite(c *C) {
	s.svr.Close()
	s.client.Close()

	close(s.quitCh)
}

func (s *testClusterWorkerSuite) checkRegionPeerNumber(c *C, regionKey []byte, expectNumber int) *metapb.Region {
	cluster, err := s.svr.getCluster(s.clusterID)
	c.Assert(err, IsNil)

	for i := 0; i < 10; i++ {
		region, err1 := cluster.GetRegion(regionKey)
		c.Assert(err1, IsNil)
		if len(region.Peers) == expectNumber {
			return region
		}
		time.Sleep(100 * time.Millisecond)
	}
	region, err := cluster.GetRegion(regionKey)
	c.Assert(err, IsNil)
	c.Assert(region.Peers, HasLen, expectNumber)
	return region
}

func (s *testClusterWorkerSuite) regionPeerExisted(c *C, regionID uint64, peer *metapb.Peer) bool {
	s.nodeLock.Lock()
	defer s.nodeLock.Unlock()

	node, ok := s.nodes[peer.GetNodeId()]
	if !ok {
		return false
	}

	node.Lock()
	defer node.Unlock()
	store, ok := node.stores[peer.GetStoreId()]
	if !ok {
		return false
	}

	store.Lock()
	defer store.Unlock()
	p, ok := store.peers[regionID]
	if !ok {
		return false
	}

	c.Assert(p.peer.GetPeerId(), Equals, peer.GetPeerId())
	return true
}

func (s *testClusterWorkerSuite) TestChangePeer(c *C) {
	cluster, err := s.svr.getCluster(s.clusterID)
	c.Assert(err, IsNil)

	meta, err := cluster.GetMeta()
	c.Assert(err, IsNil)
	c.Assert(meta.GetMaxPeerNumber(), Equals, uint32(5))

	regionKey := []byte("a")
	region, err := cluster.GetRegion(regionKey)
	c.Assert(err, IsNil)

	c.Assert(region.Peers, HasLen, 1)

	// Now we treat the first peer in region as leader.
	leaderPeer := *region.Peers[0]
	leaderPd := mustGetLeader(c, s.client, s.getRootPath())

	conn, err := net.Dial("tcp", leaderPd.GetAddr())
	c.Assert(err, IsNil)
	defer conn.Close()

	// add another 4 peers.
	for i := 0; i < 4; i++ {
		askChangePeer := &pdpb.Request{
			Header:  newRequestHeader(s.clusterID),
			CmdType: pdpb.CommandType_AskChangePeer.Enum(),
			AskChangePeer: &pdpb.AskChangePeerRequest{
				Leader: &leaderPeer,
				Region: region,
			},
		}
		sendRequest(c, conn, 0, askChangePeer)
		_, resp := recvResponse(c, conn)
		c.Assert(resp.GetCmdType(), Equals, pdpb.CommandType_AskChangePeer)

		region = s.checkRegionPeerNumber(c, regionKey, i+2)
	}

	region = s.checkRegionPeerNumber(c, regionKey, 5)

	regionID := region.GetRegionId()
	for _, peer := range region.Peers {
		ok := s.regionPeerExisted(c, regionID, peer)
		c.Assert(ok, IsTrue)
	}

	err = cluster.PutMeta(&metapb.Cluster{
		ClusterId:     proto.Uint64(s.clusterID),
		MaxPeerNumber: proto.Uint32(3),
	})
	c.Assert(err, IsNil)

	oldRegion := proto.Clone(region).(*metapb.Region)

	// remove 2 peers
	for i := 0; i < 2; i++ {
		askChangePeer := &pdpb.Request{
			Header:  newRequestHeader(s.clusterID),
			CmdType: pdpb.CommandType_AskChangePeer.Enum(),
			AskChangePeer: &pdpb.AskChangePeerRequest{
				Leader: &leaderPeer,
				Region: region,
			},
		}
		sendRequest(c, conn, 0, askChangePeer)
		_, resp := recvResponse(c, conn)
		c.Assert(resp.GetCmdType(), Equals, pdpb.CommandType_AskChangePeer)

		region = s.checkRegionPeerNumber(c, regionKey, 4-i)
	}

	region = s.checkRegionPeerNumber(c, regionKey, 3)

	for _, peer := range region.Peers {
		ok := s.regionPeerExisted(c, regionID, peer)
		c.Assert(ok, IsTrue)
	}

	// check removed peer
	for _, oldPeer := range oldRegion.Peers {
		found := false
		for _, peer := range region.Peers {
			if oldPeer.GetPeerId() == peer.GetPeerId() {
				found = true
				break
			}
		}

		if found {
			continue
		}

		ok := s.regionPeerExisted(c, regionID, oldPeer)
		c.Assert(ok, IsFalse)
	}
}

func (s *testClusterWorkerSuite) TestSplit(c *C) {
	cluster, err := s.svr.getCluster(s.clusterID)
	c.Assert(err, IsNil)

	regionKey := []byte("a")
	region, err := cluster.GetRegion(regionKey)
	c.Assert(err, IsNil)
	c.Assert(region.GetStartKey(), BytesEquals, []byte(""))
	c.Assert(region.GetEndKey(), BytesEquals, []byte(""))

	// Now we treat the first peer in region as leader.
	leaderPeer := *region.Peers[0]
	leaderPd := mustGetLeader(c, s.client, s.getRootPath())

	conn, err := net.Dial("tcp", leaderPd.GetAddr())
	c.Assert(err, IsNil)
	defer conn.Close()

	askSplit := &pdpb.Request{
		Header:  newRequestHeader(s.clusterID),
		CmdType: pdpb.CommandType_AskSplit.Enum(),
		AskSplit: &pdpb.AskSplitRequest{
			Region:   region,
			Leader:   &leaderPeer,
			SplitKey: []byte("b"),
		},
	}

	sendRequest(c, conn, 0, askSplit)
	_, resp := recvResponse(c, conn)
	c.Assert(resp.GetCmdType(), Equals, pdpb.CommandType_AskSplit)

	time.Sleep(500 * time.Millisecond)
	left, err := cluster.GetRegion([]byte("a"))
	c.Assert(err, IsNil)
	c.Assert(left.GetStartKey(), BytesEquals, []byte(""))
	c.Assert(left.GetEndKey(), BytesEquals, []byte("b"))
	c.Assert(left.GetRegionId(), Equals, region.GetRegionId())

	for _, peer := range left.Peers {
		ok := s.regionPeerExisted(c, left.GetRegionId(), peer)
		c.Assert(ok, IsTrue)
	}

	right, err := cluster.GetRegion([]byte("b"))
	c.Assert(err, IsNil)
	c.Assert(right.GetStartKey(), BytesEquals, []byte("b"))
	c.Assert(right.GetEndKey(), BytesEquals, []byte(""))

	region, err = cluster.GetRegion([]byte("c"))
	c.Assert(err, IsNil)
	c.Assert(region.GetRegionId(), Equals, right.GetRegionId())

	for _, peer := range right.Peers {
		ok := s.regionPeerExisted(c, right.GetRegionId(), peer)
		c.Assert(ok, IsTrue)
	}
}
