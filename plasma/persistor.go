package plasma

import (
	"encoding/binary"
	"math/rand"
	"unsafe"
)

var maxPageEncodedSize = 1024 * 1024 * 10

type lssBlockType uint16

var lssBlockTypeSize = int(unsafe.Sizeof(*(new(lssBlockType))))

const (
	lssPageData lssBlockType = iota
	lssPageReloc
	lssPageSplit
	lssPageMerge
	lssDiscard
)

func discardLSSBlock(wbuf []byte) {
	binary.BigEndian.PutUint16(wbuf[:lssBlockTypeSize], uint16(lssDiscard))
}

func writeLSSBlock(wbuf []byte, typ lssBlockType, bs []byte) {
	copy(wbuf[lssBlockTypeSize:], bs)
	binary.BigEndian.PutUint16(wbuf[:lssBlockTypeSize], uint16(typ))
}

func getLSSBlockType(bs []byte) lssBlockType {
	return lssBlockType(binary.BigEndian.Uint16(bs))
}

func (s *Plasma) Persist(pid PageId, evict bool, w *Writer) Page {
	buf := w.wCtx.GetBuffer(0)
retry:

	// Never read from lss
	pg, _ := s.ReadPage(pid, nil, false)
	if pg.NeedsFlush() {
		bs, dataSz := pg.Marshal(buf)
		offset, wbuf, res := s.lss.ReserveSpace(lssBlockTypeSize + len(bs))
		writeLSSBlock(wbuf, lssPageData, bs)

		var ok bool
		if evict {
			ok = s.EvictPage(pid, pg, offset)
		} else {
			pg.AddFlushRecord(offset, dataSz, false)
			ok = s.UpdateMapping(pid, pg)
		}

		if ok {
			s.lss.FinalizeWrite(res)
			w.sts.FlushDataSz += int64(dataSz)
		} else {
			discardLSSBlock(wbuf)
			s.lss.FinalizeWrite(res)
			goto retry
		}
	} else if evict && pg.IsEvictable() {
		offset := pg.GetLSSOffset()
		if !s.EvictPage(pid, pg, offset) {
			goto retry
		}
	}

	return pg
}

func (s *Plasma) PersistAll() {
	callb := func(pid PageId, partn RangePartition) error {
		s.Persist(pid, false, s.persistWriters[partn.Shard])
		return nil
	}

	s.PageVisitor(callb, s.NumPersistorThreads)
	s.lss.Sync()
}

func (s *Plasma) EvictAll() {
	callb := func(pid PageId, partn RangePartition) error {
		s.Persist(pid, true, s.evictWriters[partn.Shard])
		return nil
	}

	s.PageVisitor(callb, s.NumPersistorThreads)
}

func (s *Plasma) RunSwapper(proceed func() bool) {
	random := make([]*rand.Rand, s.NumEvictorThreads)
	for i, _ := range random {
		random[i] = rand.New(rand.NewSource(rand.Int63()))
	}

	callb := func(pid PageId, partn RangePartition) error {
		if proceed() && random[partn.Shard].Float32() <= 0.3 {
			s.Persist(pid, true, s.evictWriters[partn.Shard])
		}
		return nil
	}

	s.PageVisitor(callb, s.NumEvictorThreads)
}
