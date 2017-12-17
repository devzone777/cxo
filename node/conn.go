package node

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/skycoin/skycoin/src/cipher"

	"github.com/skycoin/net/factory"

	"github.com/skycoin/cxo/data"
	"github.com/skycoin/cxo/node/msg"
	"github.com/skycoin/cxo/skyobject"
	"github.com/skycoin/cxo/skyobject/registry"
)

// A Conn represent connection of the Node
type Conn struct {
	*factory.Connection

	// lock
	mx sync.Mutex

	incoming bool // is incoming or not

	n      *Node         // back reference
	peerID cipher.PubKey // peer id

	// request - response
	seq  uint32                    // messege seq number (for request-response)
	reqs map[uint32]chan<- msg.Msg // requests

	// # stat
	//
	// TOOD (kostyarin): stat without mutexes to do not slow down the connection
	//
	// ------

	sendq chan<- []byte // channel from factory.Connection

	await  sync.WaitGroup  // wait for receiving loop
	closeq <-chan struct{} //
	closeo sync.Once       // close once
}

func (n *Node) newConnection(
	fc *factory.Connection,
	isIncoming bool,
	isTCP bool,
) (
	c *Conn,
) {

	c = new(Conn)

	c.Connection = fc
	c.incoming = isIncoming
	c.tcp = isTCP

	c.n = n

	c.reqs = make(map[uint32]chan<- msg.Msg)

	c.sendq = fc.GetChanOut()
	c.closeq = make(chan struct{})

	n.addPendingConn(c)

	//
	// the next step is c.handshake() and c.run()
	//

	return
}

// start handling
func (c *Conn) run() {
	c.await.Add(2)
	go c.receiving()
}

func (c *Conn) decodeRaw(raw []byte) (seq, rseq uint32, m msg.Msg, err error) {

	if len(raw) < 9 {
		err = errors.New("invlaid messege received: too short")
		return
	}

	seq = binary.LittleEndian.Uint32(raw)
	raw = raw[4:]

	rseq = binary.LittleEndian.Uint32(raw)
	raw = raw[4:]

	m, err = msg.Decode(raw)
	return
}

//
// info
//

// PeerID is ID of remote peer that used
// for internals and unique
func (c *Conn) PeerID() (id NodeID) {
	return c.peerID
}

// IsIncoming returns true if this Conn is
// incoming and accepted by listener
func (c *Conn) IsIncomig() (ok bool) {
	return c.incoming
}

// IsOutgoing is inverse of the IsIncoming
func (c *Conn) IsOutgoing() (ok bool) {
	return c.incoming == false
}

// Node returns related Node
func (c *Conn) Node() (node *Node) {
	return c.n
}

// Address returns remote address
// represetned as string
func (c *Conn) Address() (address string) {
	return c.GetRemoteAddr().String()
}

// Feeds returns list of feeds this connection
// share with peer
func (c *Conn) Feeds() (feeds []cipher.PubKey) {
	return c.n.fs.feedsOfConnection(c)

}

func connString(isIncoming, isTCP bool, addr string) (s string) {

	if isIncoming == true {
		s = "-> "
	} else {
		s = "<- "
	}

	if isTCP == true {
		s += "tcp://"
	} else {
		s += "udp://"
	}

	return s + addr
}

// String returns string "-> network://remote_address"
// for example: "-> tcp://127.0.0.1:8887". Where the
// arrow is "->" for incoming connections and is "<-"
// for outgoing
func (c *Conn) String() (s string) {
	return connString(c.incoming, c.IsTCP(), c.Address())
}

//
// requests
//

// RemoteFeeds requests list of feeds that remote peer share.
// It's possible if the remote peer is public server, otherwise
// it returns "not a public server" error. The request has
// timeout configured by Config
func (c *Conn) RemoteFeeds() (feeds []cipher.PubKey, err error) {

	var reply msg.Msg

	if reply, err = c.sendRequest(&msg.RqList{}); err != nil {
		return
	}

	switch x := reply.(type) {

	case *msg.List:

		feeds = x.List

	case *msg.Err:

		err = errors.New(x.Err)

	default:

		err = fmt.Errorf("invalid response type %T", reply)

	}

	return
}

func (c *Conn) sendRoot(r *registry.Root) {
	c.sendMsg(c.nextSeq(), 0, &msg.Root{
		Feed:  r.Pub,
		Nonce: r.Nonce,
		Seq:   r.Seq,

		Value: r.Encode(),

		Sig: r.Sig,
	})
}

// send last Root to peer
func (c *Conn) sendLastRoot(pk cipher.PubKey) {

	// ignore error
	if r, _ := c.n.c.LastRoot(pk, c.n.c.ActiveHead(pk)); r != nil {
		c.sendRoot(r)
	}

}

// Subscribe to gievn feed of remote peer. The Subscribe adds
// feed to the Node if the Node doesn't have the feed calling
// the (*Node).Share method. If request fails, then the feed
// is not removed. E.g. if the Subscribe method returns error
// then it probably adds given feed to the Node, but request
// fails. Or it can returns error of the (*Node).Share
func (c *Conn) Subscribe(feed cipher.PubKey) (err error) {

	// add the feed to node

	if err = c.n.Share(feed); err != nil {
		return
	}

	var reply msg.Msg

	if reply, err = c.sendRequest(&msg.Sub{Feed: feed}); err != nil {
		return
	}

	switch x := reply.(type) {

	case *msg.Ok:
		// success

	case *msg.Err:
		err = errors.New(x.Err)

	default:
		err = fmt.Errorf("invalid response type %T", reply)

	}

	if err != nil {
		reutrn
	}

	c.n.fs.addConnFeed(c, feed)
	c.sendLastRoot(pk)
	return
}

// just send the messege
func (c *Conn) unsubscribe(pk cipher.PubKey) {
	c.sendMsg(c.nextSeq(), 0, &msg.Unsub{
		Feed: feed,
	})
}

// Unsubscribe from given feed of remote peer
func (c *Conn) Unsubscribe(feed cipher.PubKey) {
	c.n.fs.delConnFeed(c, feed)
	c.unsubscribe(feed) // notify peer
	return
}

// PreviewFunc used by (*Conn).Preview method. The function
// receive registry.Pack and lates Root object. The Pack
// used to get obejcts from DB or from remote peer. If the
// function returns true, then the Node and the Connection
// will be subscribed to the feed. Given Pack and given Root
// can't be used outside the function.
type PreviewFunc func(pack registry.Pack, r *registry.Root) (subscribe bool)

// Preview a feed of remote peer. The request is blocking.
// The Preview gets latest Root of given feed from remote
// peer and uses the peer to obtain objects this node does
// not have.
func (c *Conn) Preview(
	feed cipher.PubKey, //      : feed to preview
	previewFunc PreviewFunc, // : the function
) (
	err error, //               : first error
) {

	var reply msg.Msg
	if reply, err = c.sendRequest(&msg.RqPreview{feed}); err != nil {
		return
	}

	var r *registry.Root

	switch x := reply.(type) {
	case *msg.Err:
		return errors.New("error: " + x.Err)
	case *msg.Root:
		if r, err = c.n.c.ReceivedRoot(x.Sig, x.Value); err != nil {
			return
		}
	default:
		return fmt.Errorf("invalid msg type received: %T", reply)
	}

	var p *skyobject.Preview
	if p, err = c.n.c.Preview(r, c.getter()); err != nil {
		return
	}

	if previewFunc(p, r) == true {
		err = c.Subscribe(feed)
	}

	return
}

// implements skyobject.Getter wrapping
// the Conn
type cget struct {
	c *Conn
}

func (c *cget) Get(key cipher.SHA256) (val []byte, err error) {

	var reply msg.Msg
	if reply, err = c.c.sendRequest(m); err != nil {
		return
	}

	switch x := reply.(type) {
	case *msg.Object:
		if cipher.SumSHA256(x.Value) != key {
			return errors.New("wrong object received (different hash)")
		}
		val = x.Value
	case *msg.Err:
		return errors.New("error: " + x.Err)
	default:
		return fmt.Errorf("invalid msg type received: %T", reply)
	}

	return
}

func (c *Conn) getter() (cg skyobject.Getter) {
	return &cget{c}
}

//
// terminate
//

// close and release
func (c *Conn) close(reason error) error {
	c.closeo.Do(func() {
		c.n.delConnection(c)
		close(c.closeq)      // close the channel
		c.Connection.Close() // close
		c.await.Wait()       // wait for goroutines

		c.n.onDisconenct(c, reason) // callback
	})
	return reason
}

// Close the Conn
func (c *Conn) Close() (err error) {
	return c.close(nil)
}

func (c *Conn) nextSeq() uint32 {
	return atomic.AddUint32(&c.seq, 1)
}

func (c *Conn) encodeMsg(seq, rseq uint32, m msg.Msg) (raw []byte) {

	var em = m.Encode()

	raw = make([]byte, 8, 8+len(em))

	binary.LittleEndian.PutUint32(raw, seq)
	binary.LittleEndian.PutUint32(raw[:4], rseq)

	raw = append(raw, em...)

	return

}

func (c *Conn) sendMsg(seq, rseq uint32, m msg.Msg) {
	c.sendRaw(c.encodeMsg(seq, rseq, m))
}

func (c *Conn) sendRaw(raw []byte) {

	select {
	case c.sendq <- raw:
	case <-c.closeq:
	}

}

func (c *Conn) fatality(args ...interface{}) {

	var err = errors.New(fmt.Sprint(args...))

	c.n.Print("[ERR] ", err)
	c.close(err)
}

func (c *Conn) receiving() {

	defer c.await.Done()

	var (
		receiveq = c.GetChanIn()
		closeq   = c.closeq

		seq, rseq uint32
		m         msg.Msg
		err       error

		raw []byte
		ok  bool
	)

	for {

		select {

		case raw, ok = <-receiveq:

			if ok == false {
				return
			}

			// [ 4 seq ][ 4 rseq ][ 1 msg type ]

			if len(raw) < 9 {
				c.fatality("invalid messege received: samll size")
				return
			}

			// seq of the Msg
			seq = binary.LittleEndian.Uint32(raw)
			raw = raw[4:]

			// response for a seq or zero
			rseq = binary.LittleEndian.Uint32(raw)
			raw = raw[4:]

			if m, err = msg.Decode(raw); err != nil {
				c.fatality("can't decode received messege: ", err)
				return
			}

			// the messege can be a response for a request
			if rq, ok := c.isResponse(rseq); ok == true {
				rq <- m
				continue
			}

			if err = c.handle(seq, m); err != nil {
				c.fatality("error handling messege: ", err)
				return
			}

		case <-closeq:
			return

		}

	}

}

func (c *Conn) isResponse(rseq uint32) (rq chan<- msg.Msg, ok bool) {
	c.mx.Lock()
	defer c.mx.Unlock()

	rq, ok = c.reqs[rseq]
	return
}

func (c *Conn) addRequest(seq uint32, rq chan<- msg.Msg) {
	c.mx.Lock()
	defer c.mx.Unlock()

	c.reqs[seq] = rq
}

func (c *Conn) delRequest(seq uint32) {
	c.mx.Lock()
	defer c.mx.Unlock()

	delete(c.reqs, seq)
}

func (c *Conn) sendRequest(m msg.Msg) (reply msg.Msg, err error) {

	var (
		tr *time.Timer
		tc <-chan time.Time
	)

	if rt := c.n.config.ResponseTimeout; rt > 0 {
		tr = time.NewTimer(rt)
		tc = tr.C

		defer tr.Stop()
	}

	var (
		rq  = make(chan msg.Msg)
		seq = c.nextSeq()
	)

	c.addRequest(seq, rq)
	defer c.delRequest(seq)

	c.sendMsg(seq, 0, m)

	select {
	case rq <- reply:
		return

	case <-tc:
		return nil, ErrTimeout

	case <-c.closeq:
		return nil, ErrClosed
	}

}

func (c *Conn) sendErr(rseq uint32, err error) {
	c.sendMsg(c.nextSeq(), rseq, &msg.Err{err.Error()})
}

func (c *Conn) sendOk(rseq uint32) {
	c.sendMsg(c.nextSeq(), rseq, &msg.Ok{})
}

// handle messeges except responses and handshakes
func (c *Conn) handle(seq uint32, m msg.Msg) (err error) {

	switch x := m.(type) {

	// subscriptions

	case *msg.Sub: // <- Sub (feed)
		return c.handleSub(seq, x)

	case *msg.Unsub: // <- Unsub (feed)
		return c.handleUnsub(seq, x)

	// public server features

	case *msg.RqList: // <- RqList ()
		return c.handleRqList(seq, x)

	// the *List is response and handled outside the handle()

	// root (push)

	case *msg.Root: // <- Root (feed, nonce, seq, sig, val)
		return c.handleRoot(x)

	// obejcts

	case *msg.RqObject: // <- RqO (key, prefetch)
		c.await.Add(1)
		go c.handleRqObject(x)
		return

	// preview

	case *msg.RqPreview: // -> RqPreview (feed)
		return c.handleRqPreview(seq, x)

	//
	// delayed messeges (ignore them)
	//
	// the delayed messeges are responses that received
	// after timeout, e.g. the requst is closed with
	// ErrTimeout and noone waits them

	case *msg.Object: // -> O (delayed)
	case *msg.Err: // -> Err (delayed)
	case *msg.Ok: // -> Ok (delayed)
	case *msg.List: // -> List (delayed)

	default:

		return fmt.Errorf("invalid messege type %T", m)

	}

}

// subscribe (with reply)
func (c *Conn) handleSub(seq uint32, sub *msg.Sub) (_ error) {

	// don't allow blank

	if sub.Feed == (cipher.PubKey{}) {
		return errors.New("blank public key") // fatal (invalid request)
	}

	// check first
	if c.n.fs.hasConnFeed(c, sub.Feed) == true {
		c.sendOk(seq) // already subscribed
		return
	}

	// callback
	var reject = c.n.onSubscribeRemote(c, feed)

	// reject subscription by callback
	if reject != nil {
		c.sendErr(seq, reject)
		return
	}

	// the callback can subscibe the node to the feed,
	// and anyway we can't subscribe to a feed we don't
	// share

	if c.n.fs.hasFeed(sub.Feed) == false {
		c.sendErr(seq, errors.New("do not share the feed"))
		return
	}

	// ok

	c.n.fs.addConnFeed(c, sub.Feed)
	c.sendOk(seq)

	return
}

// unsubscribe (no reply)
func (c *Conn) handleUnsub(seq uint32, unsub *msg.Unsub) (err error) {

	if unsub.Feed == (cipher.PubKey{}) {
		return errors.New("invalid request Unsub blank feed") // fatal
	}

	c.n.delConnFeed(c, unsub.Feed) // delete
	return
}

// request list of feeds
func (c *Conn) handleRqList(seq uint32, rq *msg.RqList) (_ error) {

	if c.n.config.Public == false {
		c.sendErr(seq, ErrNotPublic)
		return
	}

	c.sendMsg(c.nextSeq(), seq, &msg.List{
		Feeds: c.n.Feeds(),
	})

	return
}

// got Root (preview Root objects are handled by request-responnse, not here)
func (c *Conn) handleRoot(root *msg.Root) (_ error) {

	// check seq first (avoid verify-signature for old unwanted Root obejcts)

	var last, err = c.n.c.LastRootSeq(root.Feed, root.Nonce) // last is full

	switch err {
	case data.ErrNoSuchFeed:

		return // unexpected Root

	case data.ErrNoSuchHead, data.ErrNotFound:

		// go dow

	default: // nil (found)

		if last >= root.Seq {
			return // we have newer one
		}

	}

	var r *registry.Root

	if r, err = c.n.c.ReceivedRoot(root.Sig, root.Value); err != nil {
		c.n.Printf("[ERR] [%s] received Root error: %s", c.String(), err)
		return // keep connection ?
	}

	// do nothing, because the Node already have this Root
	if r.IsFull == true {
		return
	}

	// fill the Root only if the node and the connection
	// subscribed to feed of the Root
	c.n.fs.receivedRoot(c, r)
	return
}

// async
func (c *Conn) handleRqObject(seq uint32, rq *msg.RqObject) {
	defer c.await.Done()

	var (
		val []byte
		err error

		gc = make(chan skyobject.Object, 1)

		tm *time.Timer
		tc <-chan time.C
	)

	// TODO (kostyarin): the request holds resources and in good case
	//                   it's ok, but it's possible to DDoS the Node
	//                   perfoкming many trash request

	// TODO (kostyarin): get the object or subscribe for the object
	//                   only if it is wanted (to think)

	c.n.c.Want(rq.Key, gc)
	defer c.n.c.Unwant(rq.Key, gc) // to be memory safe

	select {
	case obj := <-gc:
		// got
		c.sendMsg(c.nextSeq(), seq, &msg.Object{Value: obj.Val})
		return
	default:
		// wait
	}

	if rt := c.n.config.ResponseTimeout; rt > 0 {
		tm = time.NewTimer(rt)
		tc = tm.C

		defer tm.Stop()
	}

	select {
	case obj := <-gc:
		c.sendMsg(c.nextSeq(), seq, &msg.Object{Value: obj.Val})
	case <-tc:
		c.sendMsg(c.nextSeq(), seq, &msg.Err{}) // timeout
	case <-c.closeq:
		// closed
	}

	return
}

func (c *Conn) handleRqPreview(seq uint32, rqp *msg.RqPreview) (_ error) {

	var r, err = c.n.c.LastRoot(rqp.Feed, c.n.c.ActiveHead(rqp.Feed))

	if err != nil {
		c.sendMsg(c.nextSeq(), seq, &msg.Err{Err: err.Error()})
		return
	}

	c.sendMsg(c.nextSeq(), seq, &msg.Root{
		Feed:  r.Pub,
		Nonce: r.Nonce,
		Seq:   r.Seq,

		Value: r.Encode(),
		Sig:   r.Sig,
	})

	return
}
