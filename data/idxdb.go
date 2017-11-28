package data

import (
	"github.com/skycoin/skycoin/src/cipher"
)

// An IterateFeedsFunc represents function for
// iterating over all feeds IdxDB contains
type IterateFeedsFunc func(cipher.PubKey) error

// A Feeds represents bucket of feeds
type Feeds interface {
	// Add feed. Adding a feed twice or
	// more times does nothing
	Add(pk cipher.PubKey) (err error)
	// Del feed if its empty. It's impossible to
	// delete non-empty feed. This restriction required
	// for related objects. We need to decrement refs count
	// of all related objects. If feed doesn't exist
	// then the Del returns ErrNotFound
	Del(pk cipher.PubKey) (err error)

	// Iterate all feeds. Use ErrStopRange to break
	// it iteration. The Iterate passes any error
	// returned from given function through. Except
	// ErrStopIteration that turns nil. It's possible
	// to mutate the IdxDB inside the Iterate
	Iterate(iterateFunc IterateFeedsFunc) (err error)
	// Has returns true if the IdxDB contains
	// feed with given public key
	Has(pk cipher.PubKey) (ok bool, err error)

	// Heads of feed. It returns ErrNoSuchFeed
	// if given feed doesn't exist
	Heads(pk cipher.PubKey) (hs Heads, err error)
}

// An IterateHeadsFunc used to iterate over
// heads of a feed
type IterateHeadsFunc func(nonce uint64) (err error)

// A Heads represens all heads of a feed
type Heads interface {
	// Roots of head with given nonce. If given
	// head doesn't exists then, this method
	// returns ErrNoSuchHead
	Roots(nonce uint64) (rs Roots, err error)
	// Add new head with given nonce.
	// If a head with given nonce already
	// exists, then this method does nothing
	Add(nonce uint64) (rs Roots, err error)
	// Del deletes head with given nonce and
	// all its Root objects. The method returns
	// ErrNotFound if a head with given nonce
	// doesn't exist
	Del(nonce uint64) (err error)
	// Has returns true if a head with given
	// nonce exits in the CXDS
	Has(nonce uint64) (ok bool, err error)
	// Iterate over all heads
	Iterate(iterateFunc IterateHeadsFunc) (err error)
}

// An IterateRootsFunc represents function for
// iterating over all Root objects of a feed
type IterateRootsFunc func(r *Root) (err error)

// A Roots represents bucket of Root objects.
// All Root objects ordered by seq number
// from small to big
type Roots interface {
	// Ascend iterates all Root object ascending order.
	// Use ErrStopIteration to stop iteration. Any error
	// (except the ErrStopIteration) returned by given
	// IterateRootsFunc will be passed through
	Ascend(iterateFunc IterateRootsFunc) (err error)
	// Descend is the same as the Ascend, but it iterates
	// decending order. From lates Root objects to
	// oldes
	Descend(iterateFunc IterateRootsFunc) (err error)

	// Set or update Root. Method modifies given Root
	// setting AccessTime and CreateTime to appropriate
	// values
	Set(r *Root) (err error)

	// Del Root by seq number
	Del(seq uint64) (err error)

	// Get Root by seq number
	Get(seq uint64) (r *Root, err error)

	// Has the Roots Root with given seq?
	Has(seq uint64) (ok bool, err error)
}

// An IdxDB repesents database that contains
// meta information: feeds meta information
// about Root objects. There is data/idxdb
// package that implements the IdxDB. The
// IdxDB returns and uses errors ErrNotFound,
// ErrNoSuchFeed, ErrNoSuchHead,
// ErrStopIteration, ErrFeedIsNotEmpty and
// ErrHeadIsNotEmpty from this package.
//
// Also, the IdxDB contains safe-closing flag.
// Since, the skyobejct package uses cache for
// the CXDS, it keeps some information in memory
// wihtout syncing to speed up the CXO. And while
// a CXO application closes safely, the flag set
// to true. And using this flag on next start the
// CXO (the skyobejct package), can detemine state
// of the last closing. And if it has been closed
// using any unsafe way (panic or similar), then
// the skyobject walks through all feeds, heads and
// root objects to make CXDS values actual. This
// way we can rid out of this initialization (this
// walking) every time. But if something is wrong,
// then we can fix it automatically. This is
// shadowed from end-user protection from unexpected
// power off. But keep in mind, that in this cases
// a CXO application can starts slower then usual
// if DB is big. The flag set by the Close method
//
type IdxDB interface {
	Tx(func(Feeds) error) error // transaction
	Close() error               // close the IdxDB

	IsClosedSafely() bool // true if DB is ok

	// TODO: stat
}
