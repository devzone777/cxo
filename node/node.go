package node

import (
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/skycoin/net/skycoin-messenger/factory"

	"github.com/skycoin/skycoin/src/cipher"

	"github.com/skycoin/cxo/data"
	"github.com/skycoin/cxo/data/cxds"
	"github.com/skycoin/cxo/data/idxdb"
	"github.com/skycoin/cxo/skyobject"

	"github.com/skycoin/cxo/node/gnet"
	"github.com/skycoin/cxo/node/log"
	"github.com/skycoin/cxo/node/msg"
)

// common errors
var (
	// ErrTimeout occurs when a request that waits response tooks too long
	ErrTimeout = errors.New("timeout")
	// ErrSubscriptionRejected means that remote peer rejects our subscription
	ErrSubscriptionRejected = errors.New("subscription rejected by remote peer")
	// ErrNilConnection means that you tries to subscribe or request list of
	// feeds from a nil-connection
	ErrNilConnection = errors.New("subscribe to nil connection")
	// ErrUnexpectedResponse occurs if a remote peer sends any unexpected
	// response for our request
	ErrUnexpectedResponse = errors.New("unexpected response")
	// ErrNonPublicPeer occurs if a remote peer can't give us list of
	// feeds because it is not public
	ErrNonPublicPeer = errors.New(
		"request list of feeds from non-public peer")
	// ErrConnClsoed occurs if coonection closed but an action requested
	ErrConnClsoed = errors.New("connection closed")
	// ErrUnsubscribed is a reason of dropping a filling Root
	ErrUnsubscribed = errors.New("unsubscribed")
)

// A Node represents CXO P2P node
// that includes RPC server if enabled
// by configs
type Node struct {
	log.Logger                      // logger of the server
	src        msg.Src              // msg source
	conf       Config               // configuratios
	db         *data.DB             // database
	so         *skyobject.Container // skyobject

	// feeds
	fmx    sync.RWMutex
	feeds  map[cipher.PubKey]map[*Conn]struct{}
	feedsl []cipher.PubKey // cow

	// connections
	cmx    sync.RWMutex
	conns  []*Conn
	connsl []*Conn // cow

	// waiting/wanted obejcts
	wmx sync.Mutex
	wos map[cipher.SHA256]map[*Conn]struct{}

	// connections
	pool *gnet.Pool
	rpc  *rpcServer // rpc server

	// closing
	quit  chan struct{}
	quito sync.Once

	done  chan struct{} // when quit done
	doneo sync.Once

	await sync.WaitGroup

	discovery *factory.MessengerFactory
}

// NewNode creates new Node instnace using given
// configurations. The functions creates database and
// Container of skyobject instances internally. Use
// Config.Skyobject to provide appropriate configuration
// for skyobject.Container such as skyobject.Registry,
// etc. For example
//
//     conf := NewConfig()
//     conf.Skyobject.Registry = skyobject.NewRegistry(blah)
//
//     node, err := NewNode(conf)
//
func NewNode(sc Config) (s *Node, err error) {

	// data dir

	if sc.DataDir != "" {
		if err = initDataDir(sc.DataDir); err != nil {
			return
		}
	}

	// database

	var db *data.DB
	var cxPath, idxPath string

	if sc.DB != nil {
		cxPath, idxPath = "<used provided DB>", "<used provided DB>"
		db = sc.DB
	} else if sc.InMemoryDB {
		cxPath, idxPath = "<in memory>", "<in memory>"
		db = data.NewDB(cxds.NewMemoryCXDS(), idxdb.NewMemeoryDB())
	} else {
		if sc.DBPath == "" {
			cxPath = filepath.Join(sc.DataDir, "cxds.db")
			idxPath = filepath.Join(sc.DataDir, "idx.db")
		} else {
			cxPath = sc.DBPath + ".cxds"
			idxPath = sc.DBPath + ".idx"
		}
		var cx data.CXDS
		var idx data.IdxDB
		if cx, err = cxds.NewDriveCXDS(cxPath); err != nil {
			return
		}
		if idx, err = idxdb.NewDriveIdxDB(idxPath); err != nil {
			cx.Close()
			return
		}
		db = data.NewDB(cx, idx)
	}

	// container

	var so *skyobject.Container
	so = skyobject.NewContainer(db, sc.Skyobject)

	// node instance

	s = new(Node)

	s.Logger = log.NewLogger(sc.Log)
	s.conf = sc

	s.db = db

	s.so = so
	s.feeds = make(map[cipher.PubKey]map[*Conn]struct{})

	s.wos = make(map[cipher.SHA256]map[*Conn]struct{})

	// fill up feeds from database
	err = s.db.IdxDB().Tx(func(feeds data.Feeds) (err error) {
		return feeds.Iterate(func(pk cipher.PubKey) (err error) {
			s.feeds[pk] = make(map[*Conn]struct{})
			return
		})
	})
	if err != nil {
		db.Close() // close DB
		s = nil    // GC
		return
	}

	if sc.Config.Logger == nil {
		sc.Config.Logger = s.Logger // use the same logger
	}

	// gnet related callbacks
	if ch := sc.Config.OnCreateConnection; ch == nil {
		sc.Config.OnCreateConnection = s.onConnect
	} else {
		sc.Config.OnCreateConnection = func(c *gnet.Conn) {
			s.onConnect(c)
			ch(c)
		}
	}
	if dh := sc.Config.OnCloseConnection; dh == nil {
		sc.Config.OnCloseConnection = s.onDisconnect
	} else {
		sc.Config.OnCloseConnection = func(c *gnet.Conn) {
			s.onDisconnect(c)
			dh(c)
		}
	}
	if dc := sc.Config.OnDial; dc == nil {
		sc.Config.OnDial = s.onDial
	} else {
		sc.Config.OnDial = func(c *gnet.Conn, err error) error {
			if err = dc(c, err); err != nil {
				return err
			}
			return s.onDial(c, err)
		}
	}

	if s.pool, err = gnet.NewPool(sc.Config); err != nil {
		db.Close() // close DB
		s = nil
		return
	}

	if sc.EnableRPC {
		s.rpc = newRPC(s)
	}

	s.quit = make(chan struct{})
	s.done = make(chan struct{})

	if err = s.start(cxPath, idxPath); err != nil {
		s.Close()
		s = nil
	}
	return
}

func (s *Node) start(cxPath, idxPath string) (err error) {
	s.Debugf(log.All, `starting node:
    data dir:             %s

    max connections:      %d
    max message size:     %d

    dial timeout:         %v
    read timeout:         %v
    write timeout:        %v

    ping interval:        %v

    read queue:           %d
    write queue:          %d

    redial timeout:       %d
    max redial timeout:   %d
    dials limit:          %d

    read buffer:          %d
    write buffer:         %d

    TLS:                  %v

    enable RPC:           %v
    RPC address:          %s
    listening address:    %s
    enable listening:     %v
    remote close:         %t

    in-memory DB:         %v
    CXDS path:            %s
    index DB path:        %s

    discovery:            %s

    debug:                %#v
`,
		s.conf.DataDir,
		s.conf.MaxConnections,
		s.conf.MaxMessageSize,

		s.conf.DialTimeout,
		s.conf.ReadTimeout,
		s.conf.WriteTimeout,

		s.conf.PingInterval,

		s.conf.ReadQueueLen,
		s.conf.WriteQueueLen,

		s.conf.RedialTimeout,
		s.conf.MaxRedialTimeout,
		s.conf.DialsLimit,

		s.conf.ReadBufferSize,
		s.conf.WriteBufferSize,

		s.conf.TLSConfig != nil,

		s.conf.EnableRPC,
		s.conf.RPCAddress,
		s.conf.Listen,
		s.conf.EnableListener,
		s.conf.RemoteClose,

		s.conf.InMemoryDB,
		cxPath,
		idxPath,

		s.conf.DiscoveryAddresses.String(),

		s.conf.Log.Debug,
	)

	if len(s.conf.DiscoveryAddresses) > 0 {
		f := factory.NewMessengerFactory()
		for _, addr := range s.conf.DiscoveryAddresses {
			f.ConnectWithConfig(addr, &factory.ConnConfig{
				Reconnect:                      true,
				ReconnectWait:                  time.Second * 30,
				FindServiceNodesByKeysCallback: s.findServiceNodesCallback,
				OnConnected: s.
					updateServiceDiscoveryCallback,
			})
		}
		s.discovery = f
	}

	// start listener
	if s.conf.EnableListener == true {
		if err = s.pool.Listen(s.conf.Listen); err != nil {
			return
		}
		s.Print("listen on ", s.pool.Address())
	}

	// start rpc listener if need
	if s.conf.EnableRPC == true {
		if err = s.rpc.Start(s.conf.RPCAddress); err != nil {
			s.pool.Close()
			return
		}
		s.Print("rpc listen on ", s.rpc.Address())
	}

	if s.conf.PingInterval > 0 {
		s.await.Add(1)
		go s.pingsLoop()
	}

	return
}

func (s *Node) addConn(c *Conn) {
	s.cmx.Lock()
	defer s.cmx.Unlock()

	c.gc.SetValue(c) // for resubscriptions

	s.conns = append(s.conns, c)
	s.connsl = nil // clear cow copy
}

func (s *Node) updateServiceDiscoveryCallback(conn *factory.Connection) {
	feeds := s.Feeds()
	services := make([]*factory.Service, len(feeds))
	for i, feed := range feeds {
		services[i] = &factory.Service{Key: feed}
	}
	s.updateServiceDiscovery(conn, feeds, services)
}

func (s *Node) updateServiceDiscovery(conn *factory.Connection,
	feeds []cipher.PubKey, services []*factory.Service) {

	conn.FindServiceNodesByKeys(feeds)
	if s.conf.PublicServer {
		conn.UpdateServices(&factory.NodeServices{
			ServiceAddress: s.conf.Listen,
			Services:       services,
		})
	}
}

func (s *Node) findServiceNodesCallback(resp *factory.QueryResp) {
	if len(resp.Result) < 1 {
		return
	}
	for k, v := range resp.Result {
		key, err := cipher.PubKeyFromHex(k)
		if err != nil {
			continue
		}
		for _, addr := range v {
			c, err := s.Connect(addr)
			if err != nil {
				continue
			}
			c.Subscribe(key)
		}
	}
}

func (s *Node) delConn(c *Conn) {
	s.cmx.Lock()
	defer s.cmx.Unlock()

	s.connsl = nil // clear cow copy

	for i, x := range s.conns {
		if x == c {
			s.conns[i] = s.conns[len(s.conns)-1]
			s.conns[len(s.conns)-1] = nil
			s.conns = s.conns[:len(s.conns)-1]
			return
		}
	}

}

func (s *Node) gotObject(key cipher.SHA256, obj *msg.Object) {
	s.wmx.Lock()
	defer s.wmx.Unlock()

	for c := range s.wos[key] {
		c.Send(obj)
	}
	delete(s.wos, key)
}

func (s *Node) wantObejct(key cipher.SHA256, c *Conn) {
	s.wmx.Lock()
	defer s.wmx.Unlock()

	if cs, ok := s.wos[key]; ok {
		cs[c] = struct{}{}
		return
	}
	s.wos[key] = map[*Conn]struct{}{c: {}}
}

func (s *Node) delConnFromWantedObjects(c *Conn) {
	s.wmx.Lock()
	defer s.wmx.Unlock()

	for _, cs := range s.wos {
		delete(cs, c)
	}
}

// Connections of the Node. It returns shared
// slice and you must not modify it
func (s *Node) Connections() (cs []*Conn) {
	s.cmx.RLock()
	defer s.cmx.RUnlock()

	if s.connsl != nil {
		return s.connsl
	}

	s.connsl = make([]*Conn, len(s.conns))
	copy(s.connsl, s.conns)
	return
}

// Connection by address. It returns nil if
// connection not found or not established yet
func (s *Node) Connection(address string) (c *Conn) {
	if gc := s.pool.Connection(address); gc != nil {
		c, _ = gc.Value().(*Conn)
	}
	return
}

// Close the Node
func (s *Node) Close() (err error) {
	s.quito.Do(func() {
		close(s.quit)
	})
	err = s.pool.Close()
	if s.conf.EnableRPC {
		s.rpc.Close()
	}
	s.await.Wait()
	// we have to close boltdb once
	s.doneo.Do(func() {
		// close Container
		s.so.Close()
		// close database after all, otherwise, it panics
		s.db.Close()
		// close the Quiting channel
		close(s.done)
	})

	return
}

// DB of the Node
func (s *Node) DB() *data.DB { return s.db }

// Container of the Node
func (s *Node) Container() *skyobject.Container {
	return s.so
}

//
// Public methods of the Node
//

// Pool returns underlying *gnet.Pool.
// It returns nil if the Node is not started
// yet. Use methods of this Pool to manipulate
// connections: Dial, Connection, Connections,
// Address, etc
func (s *Node) Pool() *gnet.Pool {
	return s.pool
}

// Feeds the server share. It returns shared
// slice and you must not modify it
func (s *Node) Feeds() []cipher.PubKey {

	// locks: s.fmx RLock/RUnlock

	s.fmx.RLock()
	defer s.fmx.RUnlock()

	if len(s.feedsl) != 0 {
		return s.feedsl
	}
	s.feedsl = make([]cipher.PubKey, 0, len(s.feeds))
	for f := range s.feeds {
		s.feedsl = append(s.feedsl, f)
	}
	return s.feedsl
}

// HasFeed or has not
func (s *Node) HasFeed(pk cipher.PubKey) (ok bool) {
	s.fmx.RLock()
	defer s.fmx.RUnlock()
	_, ok = s.feeds[pk]
	return
}

// send Root to subscribers
func (s *Node) broadcastRoot(r *skyobject.Root, e *Conn) {
	s.fmx.RLock()
	defer s.fmx.RUnlock()

	for c := range s.feeds[r.Pub] {
		if c == e {
			continue // except
		}
		c.SendRoot(r)
	}
}

func (s *Node) addConnToFeed(c *Conn, pk cipher.PubKey) (added bool) {
	s.fmx.Lock()
	defer s.fmx.Unlock()

	if cs, ok := s.feeds[pk]; ok {
		cs[c], added = struct{}{}, true
	}
	return
}

func (s *Node) delConnFromFeed(c *Conn, pk cipher.PubKey) (deleted bool) {
	s.fmx.Lock()
	defer s.fmx.Unlock()

	if cs, ok := s.feeds[pk]; ok {
		if _, deleted = cs[c]; deleted {
			delete(cs, c)
		}
	}
	return
}

func (s *Node) onConnect(gc *gnet.Conn) {
	s.Debugf(ConnPin, "[%s] new connection %t", gc.Address(), gc.IsIncoming())

	if gc.IsIncoming() {

		c := s.newConn(gc)

		s.await.Add(1)
		go c.handle(nil)

	}

	// all outgoing connections processed by s.Connect()
}

func (s *Node) onDisconnect(gc *gnet.Conn) {
	s.Debugf(ConnPin, "[%s] close connection %t", gc.Address(), gc.IsIncoming())
}

func (s *Node) onDial(gc *gnet.Conn, _ error) (_ error) {
	if c, ok := gc.Value().(*Conn); ok {
		c.enqueueEvent(resubscribeEvent{})
	}
	return
}

// Quiting returns cahnnel that closed
// when the Node closed
func (s *Node) Quiting() <-chan struct{} {
	return s.done // when quit done
}

// RPCAddress returns address of RPC listener or an empty
// stirng if disabled
func (s *Node) RPCAddress() (address string) {
	if s.rpc != nil {
		address = s.rpc.Address()
	}
	return
}

// Publish given Root (send to feed). Given Root
// must be holded and not chagned during this call
// (holded during this call only)
func (s *Node) Publish(r *skyobject.Root) {

	// make sterile copy first

	root := new(skyobject.Root)

	root.Reg = r.Reg
	root.Pub = r.Pub
	root.Seq = r.Seq
	root.Time = r.Time
	root.Sig = r.Sig
	root.Hash = r.Hash
	root.Prev = r.Prev
	root.IsFull = r.IsFull

	root.Refs = make([]skyobject.Dynamic, 0, len(r.Refs))

	for _, dr := range r.Refs {
		root.Refs = append(root.Refs, skyobject.Dynamic{
			SchemaRef: dr.SchemaRef,
			Object:    dr.Object,
		})
	}

	s.broadcastRoot(root, nil)
}

// Connect to peer. Use callback to handle the Conn
func (s *Node) Connect(address string) (c *Conn, err error) {

	var gc *gnet.Conn
	if gc, err = s.pool.Dial(address); err != nil {
		return
	}

	hs := make(chan error)

	c = s.newConn(gc)

	s.await.Add(1)
	go c.handle(hs)

	err = <-hs
	return
}

// AddFeed to list of feed the Node shares.
// This method adds feed to undrlying skyobject.Container
// and database. But it doesn't starts exchanging
// the feed with peers. Use following code to
// subscribe al connections to the feed
//
//     if err := s.AddFeed(pk); err != nil {
//         // database failure
//     }
//     for _, c := range s.Connections() {
//         // blocking call
//         if err := c.Subscribe(pk); err != nil {
//             // handle the err
//         }
//     }
//
func (s *Node) AddFeed(pk cipher.PubKey) (err error) {
	s.fmx.Lock()
	defer s.fmx.Unlock()

	if _, ok := s.feeds[pk]; !ok {
		if err = s.so.AddFeed(pk); err != nil {
			return
		}
		s.feeds[pk] = make(map[*Conn]struct{})
		s.feedsl = nil // clear cow copy
		updateServiceDiscovery(s)
	}
	return
}

// del feed from share-list
func (s *Node) delFeed(pk cipher.PubKey) (ok bool) {
	s.fmx.Lock()
	defer s.fmx.Unlock()

	if _, ok = s.feeds[pk]; ok {
		delete(s.feeds, pk)
		s.feedsl = nil // clear cow copy
		updateServiceDiscovery(s)
	}
	return
}

func updateServiceDiscovery(n *Node) {
	if n.discovery != nil {
		feeds := make([]cipher.PubKey, 0, len(n.feeds))
		services := make([]*factory.Service, 0, len(feeds))

		for pk := range n.feeds {
			feeds = append(feeds, pk)
			services = append(services, &factory.Service{Key: pk})
		}
		go n.discovery.ForEachConn(func(connection *factory.Connection) {
			n.updateServiceDiscovery(connection, feeds, services)
		})
	}
}

// del feed from connections, every connection must
// reply when it done, because we have to know
// the moment after which our DB doesn't contains
// non-full Root object; thus, every connections
// terminates fillers of the feed and removes non-full
// root objects
func (s *Node) delFeedConns(pk cipher.PubKey) (dones []delFeedConnsReply) {
	s.cmx.RLock()
	defer s.cmx.RUnlock()

	dones = make([]delFeedConnsReply, 0, len(s.conns))

	for _, c := range s.conns {

		done := make(chan struct{})

		select {
		case c.events <- &unsubscribeFromDeletedFeedEvent{pk, done}:
		case <-c.gc.Closed():
		}

		dones = append(dones, delFeedConnsReply{done, c.done})
	}
	return
}

type delFeedConnsReply struct {
	done   <-chan struct{} // filler closed
	closed <-chan struct{} // connections closed and done
}

// DelFeed stops sharing given feed. It unsubscribes
// from all connections
func (s *Node) DelFeed(pk cipher.PubKey) (err error) {

	if false == s.delFeed(pk) {
		return // not deleted (we haven't the feed)
	}

	dones := s.delFeedConns(pk)

	// wait
	for _, dfcr := range dones {
		select {
		case <-dfcr.done:
		case <-dfcr.closed: // connection's done
		}
	}

	// now, we can remove the feed if there
	// are not holded Root objects
	err = s.so.DelFeed(pk)
	return
}

/*
// Stat of underlying DB and Container
func (s *Node) Stat() (st Stat) {
	st.Data = s.DB().Stat()
	st.CXO = s.Container().Stat()
	return
}
*/

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func (s *Node) pingsLoop() {
	defer s.await.Done()

	tk := time.NewTicker(s.conf.PingInterval)
	defer tk.Stop()

	for {
		select {
		case <-tk.C:
			now := time.Now()
			for _, c := range s.Connections() {
				md := maxDuration(now.Sub(c.gc.LastRead()),
					now.Sub(c.gc.LastWrite()))
				if md < s.conf.PingInterval {
					continue
				}
				c.SendPing()
			}
		case <-s.quit:
			return
		}
	}
}
