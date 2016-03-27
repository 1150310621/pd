package server

import (
	"net"
	"sync"
	"time"

	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/ngaut/sync2"
)

const (
	connectTimeout = 3 * time.Second
)

type nodeConn struct {
	conn        net.Conn
	touchedTime time.Time
}

func (nc *nodeConn) close() error {
	return errors.Trace(nc.conn.Close())
}

func newNodeConn(addr string) (*nodeConn, error) {
	conn, err := net.DialTimeout("tcp", addr, connectTimeout)
	if err != nil {
		return nil, errors.Trace(err)
	}

	return &nodeConn{
		conn:        conn,
		touchedTime: time.Now()}, nil
}

type nodeConns struct {
	m           sync.Mutex
	conns       map[string]*nodeConn
	idleTimeout sync2.AtomicDuration
}

// newNodeConns creates a new node conns.
func newNodeConns() *nodeConns {
	ncs := new(nodeConns)
	ncs.conns = make(map[string]*nodeConn)
	return ncs
}

// This function is not thread-safed.
func (ncs *nodeConns) createNewConn(addr string) (*nodeConn, error) {
	conn, err := newNodeConn(addr)
	if err != nil {
		return nil, errors.Trace(err)
	}

	ncs.conns[addr] = conn
	return conn, nil
}

// SetIdleTimeout sets idleTimeout of each conn.
func (ncs *nodeConns) SetIdleTimeout(idleTimeout time.Duration) {
	ncs.idleTimeout.Set(idleTimeout)
}

// GetConn gets the conn by addr.
func (ncs *nodeConns) GetConn(addr string) (*nodeConn, error) {
	ncs.m.Lock()
	defer ncs.m.Unlock()

	conn, ok := ncs.conns[addr]
	if !ok {
		return ncs.createNewConn(addr)
	}

	timeout := ncs.idleTimeout.Get()
	if timeout > 0 && conn.touchedTime.Add(timeout).Sub(time.Now()) < 0 {
		err := conn.close()
		if err != nil {
			return nil, errors.Trace(err)
		}

		return ncs.createNewConn(addr)
	}

	conn.touchedTime = time.Now()
	return conn, nil
}

// Close closes the conns.
func (ncs *nodeConns) Close() {
	ncs.m.Lock()
	defer ncs.m.Unlock()

	for _, conn := range ncs.conns {
		err := conn.close()
		if err != nil {
			log.Warnf("Close node conn failed - %v", err)
		}
	}

	ncs.conns = map[string]*nodeConn{}
}
