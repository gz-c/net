package factory

import (
	"sync"

	"net"

	"io"

	"sync/atomic"

	"encoding/binary"

	log "github.com/sirupsen/logrus"
	cn "github.com/skycoin/net/conn"
	"github.com/skycoin/skycoin/src/cipher"
)

type transport struct {
	creator *MessengerFactory
	// node
	factory *MessengerFactory
	// conn between nodes
	conn *Connection
	// app
	appNet net.Listener

	fromNode, toNode cipher.PubKey
	fromApp, toApp   cipher.PubKey

	conns      map[uint32]net.Conn
	connsMutex sync.RWMutex

	fieldsMutex sync.RWMutex
}

func NewTransport(creator *MessengerFactory, fromNode, toNode, fromApp, toApp cipher.PubKey) *transport {
	t := &transport{
		creator:  creator,
		fromNode: fromNode,
		toNode:   toNode,
		fromApp:  fromApp,
		toApp:    toApp,
		factory:  NewMessengerFactory(),
		conns:    make(map[uint32]net.Conn),
	}
	return t
}

func (t *transport) ListenAndConnect(address string) (conn *Connection, err error) {
	conn, err = t.factory.connectUDPWithConfig(address, &ConnConfig{
		OnConnected: func(connection *Connection) {
			connection.Reg()
		},
		Creator: t.creator,
	})
	return
}

func (t *transport) Connect(address, appAddress string) (err error) {
	conn, err := t.factory.connectUDPWithConfig(address, &ConnConfig{
		OnConnected: func(connection *Connection) {
			connection.writeOP(OP_BUILD_APP_CONN_OK,
				&buildConnResp{
					FromNode: t.fromNode,
					Node:     t.toNode,
					FromApp:  t.fromApp,
					App:      t.toApp,
				})
		},
		Creator: t.creator,
	})
	if err != nil {
		return
	}
	t.fieldsMutex.Lock()
	t.conn = conn
	t.fieldsMutex.Unlock()

	go t.nodeReadLoop(conn, func(id uint32) net.Conn {
		t.connsMutex.Lock()
		defer t.connsMutex.Unlock()
		appConn, ok := t.conns[id]
		if !ok {
			appConn, err = net.Dial("tcp", appAddress)
			if err != nil {
				log.Debugf("app conn dial err %v", err)
				return nil
			}
			t.conns[id] = appConn
			go t.appReadLoop(id, appConn, conn, false)
		}
		return appConn
	})

	return
}

func (t *transport) nodeReadLoop(conn *Connection, getAppConn func(id uint32) net.Conn) {
	var err error
	for {
		select {
		case m, ok := <-conn.GetChanIn():
			if !ok {
				log.Debugf("node conn read err %v", err)
				return
			}
			id := binary.BigEndian.Uint32(m[PKG_HEADER_ID_BEGIN:PKG_HEADER_ID_END])
			appConn := getAppConn(id)
			if appConn == nil {
				continue
			}
			op := m[PKG_HEADER_OP_BEGIN]
			if op == OP_CLOSE {
				t.connsMutex.Lock()
				t.conns[id] = nil
				t.connsMutex.Unlock()
				appConn.Close()
				continue
			}
			body := m[PKG_HEADER_END:]
			if len(body) < 1 {
				continue
			}
			err = writeAll(appConn, body)
			log.Debugf("send to tcp")
			if err != nil {
				log.Debugf("app conn write err %v", err)
				continue
			}
		}
	}
}

func (t *transport) appReadLoop(id uint32, appConn net.Conn, conn *Connection, create bool) {
	buf := make([]byte, cn.MAX_UDP_PACKAGE_SIZE-100)
	binary.BigEndian.PutUint32(buf[PKG_HEADER_ID_BEGIN:PKG_HEADER_ID_END], id)
	if create {
		conn.GetChanOut() <- buf[:PKG_HEADER_END]
	}

	defer func() {
		t.connsMutex.Lock()
		defer t.connsMutex.Unlock()
		// exited by err
		if t.conns[id] != nil {
			buf[PKG_HEADER_OP_BEGIN] = OP_CLOSE
			conn.GetChanOut() <- buf[:PKG_HEADER_END]
			if create {
				delete(t.conns, id)
			} else {
				t.conns[id] = nil
			}
			return
		}
		if create {
			delete(t.conns, id)
		}
	}()
	for {
		n, err := appConn.Read(buf[PKG_HEADER_END:])
		if err != nil {
			log.Debugf("app conn read err %v, %d", err, n)
			return
		}
		pkg := make([]byte, PKG_HEADER_END+n)
		copy(pkg, buf[:PKG_HEADER_END+n])
		conn.GetChanOut() <- pkg
		log.Debugf("send to node udp")
	}
}

func (t *transport) setUDPConn(conn *Connection) {
	t.fieldsMutex.Lock()
	t.conn = conn
	t.fieldsMutex.Unlock()
}

func (t *transport) ListenForApp(address string, fn func()) (err error) {
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	t.fieldsMutex.Lock()
	t.appNet = ln
	t.fieldsMutex.Unlock()

	fn()

	go t.accept()
	return
}

const (
	PKG_HEADER_ID_SIZE = 4
	PKG_HEADER_OP_SIZE = 1

	PKG_HEADER_BEGIN    = 0
	PKG_HEADER_OP_BEGIN
	PKG_HEADER_OP_END   = PKG_HEADER_OP_BEGIN + PKG_HEADER_OP_SIZE
	PKG_HEADER_ID_BEGIN
	PKG_HEADER_ID_END   = PKG_HEADER_ID_BEGIN + PKG_HEADER_ID_SIZE
	PKG_HEADER_END
)

const (
	OP_TRANSPORT = iota
	OP_CLOSE
	OP_SHUTDOWN
)

func (t *transport) accept() {
	t.fieldsMutex.RLock()
	tConn := t.conn
	t.fieldsMutex.RUnlock()


	var idSeq uint32
	for {
		conn, err := t.appNet.Accept()
		if err != nil {
			return
		}
		go t.nodeReadLoop(tConn, func(id uint32) net.Conn {
			t.connsMutex.RLock()
			conn := t.conns[id]
			t.connsMutex.RUnlock()
			return conn
		})
		id := atomic.AddUint32(&idSeq, 1)
		t.connsMutex.Lock()
		t.conns[id] = conn
		t.connsMutex.Unlock()
		go t.appReadLoop(id, conn, tConn, true)
	}
}

func (t *transport) Close() {
	t.fieldsMutex.Lock()
	defer t.fieldsMutex.Unlock()

	if t.factory == nil {
		return
	}

	t.connsMutex.RLock()
	for _, v := range t.conns {
		if v == nil {
			continue
		}
		v.Close()
	}
	t.connsMutex.RUnlock()
	if t.appNet != nil {
		t.appNet.Close()
		t.appNet = nil
	}
	if t.conn != nil {
		t.conn.Close()
		t.conn = nil
	}
	t.factory.Close()
	t.factory = nil
}

func writeAll(conn io.Writer, m []byte) error {
	for i := 0; i < len(m); {
		n, err := conn.Write(m[i:])
		if err != nil {
			return err
		}
		i += n
	}
	return nil
}
