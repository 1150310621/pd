package server

import (
	"encoding/json"
	"os"
	"path"
	"sync/atomic"
	"time"

	"golang.org/x/net/context"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/lease"
	storagepb "github.com/coreos/etcd/storage/storagepb"
	"github.com/juju/errors"
	"github.com/ngaut/log"
)

type Leader struct {
	Addr string `json:"addr"`
	PID  int    `json:"pid"`
}

// IsLeader returns whether server is leader or not.
func (s *Server) IsLeader() bool {
	return atomic.LoadInt64(&s.isLeader) == 1
}

func (s *Server) enableLeader(b bool) {
	value := int64(0)
	if b {
		value = 1
	}

	atomic.StoreInt64(&s.isLeader, value)
}

// GetLeaderPath returns the leader path.
func GetLeaderPath(rootPath string) string {
	return path.Join(rootPath, "leader")
}

func (s *Server) getLeaderPath() string {
	return GetLeaderPath(s.cfg.RootPath)
}

func (s *Server) leaderLoop() {
	defer func() {
		s.wg.Done()
		s.enableLeader(false)
	}()

	for {
		if s.IsClosed() {
			return
		}

		s.enableLeader(false)

		leader, err := s.getLeader()
		if err != nil {
			log.Errorf("get leader err %v", err)
			continue
		} else if leader != nil {
			log.Debugf("leader is %#v, watch it", leader)

			s.watchLeader()

			log.Debugf("leader changed, try to campaign leader")
		}

		err = s.campaignLeader()
		if err != nil {
			log.Errorf("campaign leader err %s", err)
		}

		// here means we are not leader, close all connections
		s.closeAllConnections()
	}
}

// GetLeader gets server leader from etcd.
func GetLeader(c *clientv3.Client, leaderPath string) (*Leader, error) {
	kv := clientv3.NewKV(c)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	resp, err := kv.Get(ctx, leaderPath)
	cancel()

	if err != nil {
		return nil, errors.Trace(err)
	}

	if n := len(resp.Kvs); n == 0 {
		// no leader key
		return nil, nil
	} else if n > 1 {
		return nil, errors.Errorf("invalid get leader resp %v, must only one", resp.Kvs)
	}

	leader := Leader{}
	err = json.Unmarshal(resp.Kvs[0].Value, &leader)
	if err != nil {
		return nil, errors.Trace(err)
	}

	return &leader, nil
}

func (s *Server) getLeader() (*Leader, error) {
	return GetLeader(s.client, s.getLeaderPath())
}

func (s *Server) campaignLeader() error {
	log.Debugf("begin to campaign leader")

	leader := Leader{
		Addr: s.cfg.Addr,
		PID:  os.Getpid(),
	}

	data, err := json.Marshal(leader)
	if err != nil {
		return errors.Trace(err)
	}

	lessor := clientv3.NewLease(s.client)
	defer lessor.Close()

	leaseResp, err := lessor.Create(context.TODO(), s.cfg.LeaderLease)
	if err != nil {
		return errors.Trace(err)
	}

	leaderKey := s.getLeaderPath()
	// The leader key must not exist, so the CreatedRevision is 0.
	resp, err := s.client.Txn(context.TODO()).
		If(clientv3.Compare(clientv3.CreatedRevision(leaderKey), "=", 0)).
		Then(clientv3.OpPut(leaderKey, string(data), clientv3.WithLease(lease.LeaseID(leaseResp.ID)))).
		Commit()
	if err != nil {
		return errors.Trace(err)
	} else if !resp.Succeeded {
		log.Infof("campaign leader failed, other server may campaign ok")
		return nil
	}

	// keeps the leader
	ch, err := lessor.KeepAlive(s.client.Ctx(), lease.LeaseID(leaseResp.ID))
	if err != nil {
		return errors.Trace(err)
	}

	log.Debugf("become leader")

	s.enableLeader(true)

	for {
		_, ok := <-ch
		if !ok {
			log.Infof("keep alive channel is closed")
			break
		}
	}

	s.enableLeader(false)

	return nil
}

func (s *Server) watchLeader() {
	watcher := clientv3.NewWatcher(s.client)
	defer watcher.Close()

	for {
		rch := watcher.Watch(s.client.Ctx(), s.getLeaderPath())
		for wresp := range rch {
			if wresp.Canceled {
				return
			}

			for _, ev := range wresp.Events {
				if ev.Type == storagepb.EXPIRE || ev.Type == storagepb.DELETE {
					log.Infof("leader is expired or deleted")
					return
				}
			}
		}
	}
}
