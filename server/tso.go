package server

import (
	"encoding/binary"
	"path"
	"sync/atomic"
	"time"

	"golang.org/x/net/context"

	"github.com/coreos/etcd/clientv3"
	"github.com/golang/protobuf/proto"
	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/pd/protopb"
)

const (
	// update timestamp every updateTimestampStep milliseconds.
	updateTimestampStep = int64(10)
	maxLogical          = int64(1 << 18)
)

type atomicObject struct {
	physical time.Time
	logical  int64
}

func getTimestampPath(rootPath string) string {
	return path.Join(rootPath, "timestamp")
}

func (s *Server) loadTimestamp() (int64, error) {
	kv := clientv3.NewKV(s.client)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	resp, err := kv.Get(ctx, getTimestampPath(s.cfg.RootPath))
	cancel()

	if err != nil {
		return 0, errors.Trace(err)
	}

	if n := len(resp.Kvs); n == 0 {
		// no timestamp key
		return 0, nil
	} else if n > 1 {
		return 0, errors.Errorf("invalid get leader resp %v, must only one", resp.Kvs)
	}
	data := resp.Kvs[0].Value

	if len(data) != 8 {
		return 0, errors.Errorf("invalid timestamp data, must 8 bytes, but %d", len(data))
	}

	return int64(binary.BigEndian.Uint64(data)), nil
}

// save timestamp, if lastTs is 0, we think the timestamp doesn't exist, so create it,
// otherwise, update it.
func (s *Server) saveTimestamp(now time.Time) error {
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, uint64(now.UnixNano()))

	key := getTimestampPath(s.cfg.RootPath)

	resp, err := s.client.Txn(context.TODO()).
		If(s.leaderCmp()).
		Then(clientv3.OpPut(key, string(data))).
		Commit()
	if err != nil {
		return errors.Trace(err)
	} else if !resp.Succeeded {
		return errors.New("save timestamp failed, maybe we lost leader")
	}

	s.lastSavedTime = now

	return nil
}

func (s *Server) syncTimestamp() error {
	last, err := s.loadTimestamp()

	if err != nil {
		return errors.Trace(err)
	}

	var now time.Time

	for {
		now = time.Now()

		since := (now.UnixNano() - last) / 1e6
		if since <= 0 {
			return errors.Errorf("%s <= last saved time %s", now, time.Unix(0, last))
		}

		// TODO: can we speed up this?
		if wait := 2*s.cfg.LeaderLease*1e3 - since; wait > 0 {
			log.Warnf("wait %d milliseconds to guarantee valid generated timestamp", wait)
			time.Sleep(time.Duration(wait) * time.Millisecond)
			continue
		}

		break
	}

	if err = s.saveTimestamp(now); err != nil {
		return errors.Trace(err)
	}

	log.Debug("sync and save timestamp ok")

	current := &atomicObject{
		physical: now,
	}
	s.ts.Store(current)

	return nil
}

func (s *Server) updateTimestamp() error {
	prev := s.ts.Load().(*atomicObject)
	now := time.Now()

	// ms
	since := now.Sub(prev.physical).Nanoseconds() / 1e6
	if since > 2*updateTimestampStep {
		log.Warnf("clock offset: %v, prev: %v, now %v", since, prev.physical, now)
	}
	// Avoid the same physical time stamp
	if since <= 0 {
		log.Warn("invalid physical time stamp, re-update later again")
		return nil
	}

	if now.Sub(s.lastSavedTime).Nanoseconds()/1e6 > s.cfg.LeaderLease*1e3 {
		if err := s.saveTimestamp(now); err != nil {
			return errors.Trace(err)
		}
	}

	current := &atomicObject{
		physical: now,
	}
	s.ts.Store(current)

	return nil
}

const maxRetryNum = 100

func (s *Server) getRespTS() *protopb.Timestamp {
	resp := &protopb.Timestamp{}
	for i := 0; i < maxRetryNum; i++ {
		current, ok := s.ts.Load().(*atomicObject)
		if !ok {
			log.Errorf("we haven't synced timestamp ok, wait  and retry")
			time.Sleep(200 * time.Millisecond)
			continue
		}

		resp.Physical = proto.Int64(int64(current.physical.UnixNano()) / 1e6)
		resp.Logical = proto.Int64(atomic.AddInt64(&current.logical, 1))
		if *resp.Logical >= maxLogical {
			log.Errorf("logical part outside of max logical interval %v, please check ntp time", resp)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		return resp
	}
	return nil
}
