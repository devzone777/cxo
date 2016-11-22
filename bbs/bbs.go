package bbs

import (
	"github.com/skycoin/cxo/schema"
	"github.com/skycoin/skycoin/src/cipher"
)

type Bbs struct {
	Boards schema.HArray `type:"Board"`
	Store  *schema.Store `enc:"-"`
}

func CreateBbs() *Bbs {
	store := schema.NewStore()
	return &Bbs{Store:store, Boards:schema.NewHArray()}
}

func (bbs *Bbs) AddBoard(name string) (cipher.SHA256, error) {
	b := Board{Name:name, Threads:schema.NewHArray()}
	key, _ := bbs.Store.Save(b)
	href := schema.HrefStatic{key}
	bbs.Boards = bbs.Boards.Append(href)
	bbs.Store.Save(bbs.Boards)
	return key, nil
}

func (bbs *Bbs) AllBoars() []Board {
	var boards []Board
	boards = bbs.Boards.ToObjects(bbs.Store, Board{}).([]Board)
	return boards
}

func (bbs *Bbs) AddThread(boardKey cipher.SHA256, name string) {
	newThread := Thread{Name:name}
	key, _:= bbs.Store.Save(newThread)

	var board Board
	bbs.Store.Load(boardKey, &board)
	board.Threads = board.Threads.Append(schema.HrefStatic{Hash:key})
	bbs.updateBoard(boardKey, board)
}

func (bbs *Bbs) GetThreads(board Board) []Thread {
	return board.Threads.ToObjects(bbs.Store, Thread{}).([]Thread)
}
//
func (bbs *Bbs) updateBoard(key cipher.SHA256, board Board) {
	newBoardKey, _ := bbs.Store.Save(board)
	boards := []schema.HrefStatic{{Hash:newBoardKey}}
	for i := 0; i < len(bbs.Boards); i++ {
		if bbs.Boards[i].Hash != key{
			boards = append(boards, bbs.Boards[i])
		}
	}
	bbs.Boards = boards
}
