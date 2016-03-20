package pd

import (
	"bufio"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/juju/errors"
	"github.com/ngaut/deadline"
	"github.com/ngaut/log"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/pd/util"
	"github.com/twinj/uuid"
)

const (
	connectPDTimeout = time.Second * 3
	netIOTimeout     = time.Second
)

const (
	readBufferSize  = 8 * 1024
	writeBufferSize = 8 * 1024
)

const maxPipelineRequest = 10000

type tsoRequest struct {
	done     chan error
	physical int64
	logical  int64
}

type regionRequest struct {
	key    []byte
	done   chan error
	region *metapb.Region
}

type rpcWorker struct {
	addr      string
	clusterID uint64
	requests  chan interface{}
	wg        sync.WaitGroup
	quit      chan struct{}
}

func newRPCWorker(addr string, clusterID uint64) *rpcWorker {
	w := &rpcWorker{
		addr:      addr,
		clusterID: clusterID,
		requests:  make(chan interface{}, maxPipelineRequest),
		quit:      make(chan struct{}),
	}
	w.wg.Add(1)
	go w.work()
	return w
}

func (w *rpcWorker) stop(err error) {
	close(w.quit)
	w.wg.Wait()

	len := len(w.requests)
	for i := 0; i < len; i++ {
		req := <-w.requests
		switch r := req.(type) {
		case *tsoRequest:
			r.done <- err
		case *regionRequest:
			r.done <- err
		}
	}
}

func (w *rpcWorker) work() {
	defer w.wg.Done()

RECONNECT:
	log.Infof("[pd] connect to pd server %v", w.addr)
	conn, err := net.DialTimeout("tcp", w.addr, connectPDTimeout)
	if err != nil {
		log.Warnf("[pd] failed connect pd server: %v, will retry later", err)

		select {
		case <-time.After(time.Second):
			goto RECONNECT
		case <-w.quit:
			return
		}
	}

	reader := bufio.NewReaderSize(deadline.NewDeadlineReader(conn, netIOTimeout), readBufferSize)
	writer := bufio.NewWriterSize(deadline.NewDeadlineWriter(conn, netIOTimeout), writeBufferSize)
	readwriter := bufio.NewReadWriter(reader, writer)

	for {
		var pending []interface{}
		select {
		case req := <-w.requests:
			pending = append(pending, req)
		POP_ALL:
			for {
				select {
				case req := <-w.requests:
					pending = append(pending, req)
				default:
					break POP_ALL
				}
			}
			if ok := w.handleRequests(pending, readwriter); !ok {
				conn.Close()
				goto RECONNECT
			}
		case <-w.quit:
			conn.Close()
			return
		}
	}
}

func (w *rpcWorker) handleRequests(requests []interface{}, conn *bufio.ReadWriter) bool {
	var tsoRequests []*tsoRequest
	ok := true
	for _, req := range requests {
		switch r := req.(type) {
		case *tsoRequest:
			tsoRequests = append(tsoRequests, r)
		case *regionRequest:
			region, err := w.getRegionFromRemote(conn, r.key)
			if err != nil {
				ok = false
				log.Error(err)
				r.done <- err
			} else {
				r.region = region
				r.done <- nil
			}
		default:
			log.Errorf("[pd] invalid request %v", r)
		}
	}
	ts, err := w.getTSFromRemote(conn, len(tsoRequests))
	if err != nil {
		ok = false
		log.Error(err)
	}
	for i, req := range tsoRequests {
		if err != nil {
			req.done <- err
		} else {
			req.physical = ts[i].GetPhysical()
			req.logical = ts[i].GetLogical()
			req.done <- nil
		}
	}
	return ok
}

var msgID uint64

func newMsgID() uint64 {
	return atomic.AddUint64(&msgID, 1)
}

func (w *rpcWorker) getTSFromRemote(conn *bufio.ReadWriter, n int) ([]*pdpb.Timestamp, error) {
	req := pdpb.Request{
		Header: &pdpb.RequestHeader{
			Uuid:      uuid.NewV4().Bytes(),
			ClusterId: proto.Uint64(w.clusterID),
		},
		CmdType: pdpb.CommandType_Tso.Enum(),
		Tso: &pdpb.TsoRequest{
			Number: proto.Uint32(uint32(n)),
		},
	}
	if err := util.WriteMessage(conn, newMsgID(), &req); err != nil {
		return nil, errors.Errorf("[pd] rpc failed: %v", err)
	}
	conn.Flush()
	var rsp pdpb.Response
	if _, err := util.ReadMessage(conn, &rsp); err != nil {
		return nil, errors.Errorf("[pd] rpc failed: %v", err)
	}
	if rsp.GetTso() == nil {
		return nil, errors.New("[pd] tso filed in rpc response not set")
	}
	timestamps := rsp.GetTso().GetTimestamps()
	if len(timestamps) != n {
		return nil, errors.New("[pd] tso length in rpc response is incorrect")
	}
	return timestamps, nil
}

func (w *rpcWorker) getRegionFromRemote(conn *bufio.ReadWriter, key []byte) (*metapb.Region, error) {
	req := pdpb.Request{
		Header: &pdpb.RequestHeader{
			Uuid:      uuid.NewV4().Bytes(),
			ClusterId: proto.Uint64(w.clusterID),
		},
		CmdType: pdpb.CommandType_GetMeta.Enum(),
		GetMeta: &pdpb.GetMetaRequest{
			MetaType:  pdpb.MetaType_RegionType.Enum(),
			RegionKey: key,
		},
	}
	if err := util.WriteMessage(conn, newMsgID(), &req); err != nil {
		return nil, errors.Errorf("[pd] rpc failed: %v", err)
	}
	conn.Flush()
	var rsp pdpb.Response
	if _, err := util.ReadMessage(conn, &rsp); err != nil {
		return nil, errors.Errorf("[pd] rpc failed: %v", err)
	}
	if rsp.GetGetMeta() == nil {
		return nil, errors.New("[pd] GetMeta filed in rpc response not set")
	}
	region := rsp.GetGetMeta().GetRegion()
	if region == nil {
		return nil, errors.New("[pd] Region filed in rpc response not set")
	}
	return region, nil
}
