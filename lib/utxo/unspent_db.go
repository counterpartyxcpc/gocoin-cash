package utxo

import (
	"os"
	"fmt"
	"sync"
	"time"
	"bufio"
	"bytes"
	"unsafe"
	"io/ioutil"
	"encoding/binary"
	"github.com/piotrnar/gocoin/lib/btc"
	"github.com/piotrnar/gocoin/lib/others/sys"
)


const (
	UTXO_RECORDS_PREALLOC = 35e6
)

var (
	UTXO_WRITING_TIME_TARGET = 5*time.Minute  // Take it easy with flushing UTXO.db onto disk
)

type FunctionWalkUnspent func(*UtxoRec)

type CallbackFunctions struct {
	// If NotifyTx is set, it will be called each time a new unspent
	// output is being added or removed. When being removed, btc.TxOut is nil.
	NotifyTxAdd func (*UtxoRec)
	NotifyTxDel func (*UtxoRec, []bool)

	// These two are used only during loading
	LoadWalk FunctionWalkUnspent // this one is called for each UTXO record that has just been loaded
}

// Used to pass block's changes to UnspentDB
type BlockChanges struct {
	Height uint32
	LastKnownHeight uint32  // put here zero to disable this feature
	AddList []*UtxoRec
	DeledTxs map[[32]byte] []bool
	UndoData map[[32]byte] *UtxoRec
}


type UnspentDB struct {
	HashMap map[UtxoKeyType] unsafe.Pointer
	sync.RWMutex // used to access HashMap

	LastBlockHash[]byte
	LastBlockHeight uint32
	dir_utxo, dir_undo string
	volatimemode bool
	UnwindBufLen uint32
	DirtyDB sys.SyncBool
	sync.Mutex

	abortwritingnow chan bool
	WritingInProgress sys.SyncBool
	writingDone sync.WaitGroup

	CurrentHeightOnDisk uint32
	hurryup chan bool
	DoNotWriteUndoFiles bool
	CB CallbackFunctions
}

type NewUnspentOpts struct {
	Dir string
	Rescan bool
	VolatimeMode bool
	UnwindBufferLen uint32
	CB CallbackFunctions
	AbortNow *bool
}

func NewUnspentDb(opts *NewUnspentOpts) (db *UnspentDB) {
	//var maxbl_fn string
	db = new(UnspentDB)
	db.dir_utxo = opts.Dir
	db.dir_undo = db.dir_utxo + "undo"+string(os.PathSeparator)
	db.volatimemode = opts.VolatimeMode
	db.UnwindBufLen = 256
	db.CB = opts.CB
	db.abortwritingnow = make(chan bool, 1)
	db.hurryup = make(chan bool, 1)

	os.MkdirAll(db.dir_undo, 0770)

	os.Remove(db.dir_undo+"tmp")
	os.Remove(db.dir_utxo+"UTXO.db.tmp")

	if opts.Rescan {
		db.HashMap = make(map[UtxoKeyType]unsafe.Pointer, UTXO_RECORDS_PREALLOC)
		return
	}

	// Load data form disk
	var k UtxoKeyType
	var cnt_dwn, cnt_dwn_from, perc int
	var le uint64
	var u64, tot_recs uint64
	var info string

	of, er := os.Open(db.dir_utxo + "UTXO.db")
	if er!=nil {
		of, er = os.Open(db.dir_utxo + "UTXO.old")
		if er!=nil {
			db.HashMap = make(map[UtxoKeyType]unsafe.Pointer, UTXO_RECORDS_PREALLOC)
			return
		}
	}

	defer of.Close()

	rd := bufio.NewReader(of)

	er = binary.Read(rd, binary.LittleEndian, &u64)
	if er != nil {
		goto fatal_error
	}
	db.LastBlockHeight = uint32(u64)

	db.LastBlockHash = make([]byte, 32)
	_, er = rd.Read(db.LastBlockHash)
	if er != nil {
		goto fatal_error
	}
	er = binary.Read(rd, binary.LittleEndian, &u64)
	if er != nil {
		goto fatal_error
	}

	//fmt.Println("Last block height", db.LastBlockHeight, "   Number of records", u64)
	cnt_dwn_from = int(u64/100)

	db.HashMap = make(map[UtxoKeyType]unsafe.Pointer, int(u64))
	info = fmt.Sprint("\rLoading ", u64, " transactions from UTXO.db - ")

	for tot_recs<u64 {
		if opts.AbortNow!=nil && *opts.AbortNow {
			break
		}
		le, er = btc.ReadVLen(rd)
		if er!=nil {
			goto fatal_error
		}

		er = btc.ReadAll(rd, k[:])
		if er!=nil {
			goto fatal_error
		}

		b := malloc(uint32(int(le)-UtxoIdxLen))
		er = btc.ReadAll(rd, Slice(b))
		if er!=nil {
			goto fatal_error
		}

		db.RWMutex.Lock()
		db.HashMap[k] = b
		db.RWMutex.Unlock()

		if db.CB.LoadWalk!=nil {
			db.CB.LoadWalk(NewUtxoRecStatic(k, Slice(b)))
		}

		tot_recs++
		if cnt_dwn==0 {
			fmt.Print(info, perc, "% complete ... ")
			perc++
			cnt_dwn = cnt_dwn_from
		} else {
			cnt_dwn--
		}
	}

	fmt.Print("\r                                                              \r")

	db.CurrentHeightOnDisk = db.LastBlockHeight

	return

fatal_error:
	println("Fatal error when opening UTXO.db:", er.Error(), tot_recs, u64)
	if opts.AbortNow!=nil {
		*opts.AbortNow = true
	}
	return
}


func (db *UnspentDB) save() {
	//var cnt_dwn, cnt_dwn_from, perc int
	var abort, hurryup bool

	os.Rename(db.dir_utxo + "UTXO.db", db.dir_utxo + "UTXO.old")

	of, er := os.Create(db.dir_utxo + "UTXO.db.tmp")
	if er!=nil {
		println("Create file:", er.Error())
		return
	}

	start_time := time.Now()
	db.RWMutex.RLock()
	total_records := int64(len(db.HashMap))
	var current_record, data_progress, time_progress int64

	wr := bufio.NewWriter(of)
	binary.Write(wr, binary.LittleEndian, uint64(db.LastBlockHeight))
	wr.Write(db.LastBlockHash)
	binary.Write(wr, binary.LittleEndian, uint64(total_records))
	for k, v := range db.HashMap {
		if !hurryup {
			current_record++
			if (current_record&0xf)==0 {
				data_progress = int64((current_record<<20)/total_records)
				time_progress = int64((time.Now().Sub(start_time)<<20) / UTXO_WRITING_TIME_TARGET)
				if data_progress > time_progress {
					select {
					case <- db.abortwritingnow:
						abort = true
					case <- db.hurryup:
						hurryup = true
					case <- time.After(1e6):
					}
				}
			}
		}
		if abort {
			break
		}
		btc.WriteVlen(wr, uint64(UtxoIdxLen+_len(v)))
		wr.Write(k[:])
		_, er = wr.Write(_slice(v))
		if er != nil {
			println("\n\007Fatal error saving UTXO:", er.Error())
			abort = true
			break
		}
	}
	db.RWMutex.RUnlock()

	if abort {
		of.Close()
		os.Remove(db.dir_utxo + "UTXO.db.tmp")
	} else {
		db.DirtyDB.Clr()
		wr.Flush()
		of.Close()
		os.Rename(db.dir_utxo + "UTXO.db.tmp", db.dir_utxo + "UTXO.db")
		//println("utxo written OK in", time.Now().Sub(start_time).String(), timewaits)
		db.CurrentHeightOnDisk = db.LastBlockHeight
	}

	db.writingDone.Done()
}


// Commit the given add/del transactions to UTXO and Unwind DBs
func (db *UnspentDB) CommitBlockTxs(changes *BlockChanges, blhash []byte) (e error) {
	undo_fn := fmt.Sprint(db.dir_undo, changes.Height)

	db.Mutex.Lock()
	defer db.Mutex.Unlock()
	db.abortWriting()

	if changes.UndoData!=nil {
		bu := new(bytes.Buffer)
		bu.Write(blhash)
		if changes.UndoData != nil {
			for _, xx := range changes.UndoData {
				bin := xx.Serialize(true)
				btc.WriteVlen(bu, uint64(len(bin)))
				bu.Write(bin)
			}
		}
		ioutil.WriteFile(db.dir_undo+"tmp", bu.Bytes(), 0666)
		os.Rename(db.dir_undo+"tmp", undo_fn)
	}

	db.commit(changes)

	if db.LastBlockHash==nil {
		db.LastBlockHash = make([]byte, 32)
	}
	copy(db.LastBlockHash, blhash)
	db.LastBlockHeight = changes.Height

	if changes.Height>db.UnwindBufLen {
		os.Remove(fmt.Sprint(db.dir_undo, changes.Height-db.UnwindBufLen))
	}

	db.DirtyDB.Set()
	return
}


func (db *UnspentDB) UndoBlockTxs(bl *btc.Block, newhash []byte) {
	db.Mutex.Lock()
	defer db.Mutex.Unlock()
	db.abortWriting()

	for _, tx := range bl.Txs {
		lst := make([]bool, len(tx.TxOut))
		for i := range lst {
			lst[i] = true
		}
		db.del(tx.Hash.Hash[:], lst)
	}

	fn := fmt.Sprint(db.dir_undo, db.LastBlockHeight)
	var addback []*UtxoRec

	if _, er := os.Stat(fn); er != nil {
		fn += ".tmp"
	}

	dat, er := ioutil.ReadFile(fn)
	if er!=nil {
		panic(er.Error())
	}

	off := 32  // ship the block hash
	for off < len(dat) {
		le, n := btc.VLen(dat[off:])
		off += n
		qr := FullUtxoRec(dat[off:off+le])
		off += le
		addback = append(addback, qr)
	}

	for _, tx := range addback {
		if db.CB.NotifyTxAdd!=nil {
			db.CB.NotifyTxAdd(tx)
		}

		var ind UtxoKeyType
		copy(ind[:], tx.TxID[:])
		db.RWMutex.RLock()
		v := db.HashMap[ind]
		db.RWMutex.RUnlock()
		if v != nil {
			oldrec := NewUtxoRec(ind, _slice(v))
			for a := range tx.Outs {
				if tx.Outs[a]==nil {
					tx.Outs[a] = oldrec.Outs[a]
				}
			}
		}
		db.RWMutex.Lock()
		db.HashMap[ind] = malloc_and_copy(tx.Bytes())
		db.RWMutex.Unlock()
	}

	os.Remove(fn)
	db.LastBlockHeight--
	copy(db.LastBlockHash, newhash)
	db.DirtyDB.Set()
}


// Call it when the main thread is idle
func (db *UnspentDB) Idle() bool {
	if db.volatimemode {
		return false
	}

	db.Mutex.Lock()
	defer db.Mutex.Unlock()

	if db.DirtyDB.Get() && !db.WritingInProgress.Get() {
		db.writingDone.Add(1)
		db.WritingInProgress.Set()
		//println("save", db.LastBlockHeight, "now")
		go db.save() // this one will call db.writingDone.Done()
		return true
	}

	return false
}


func (db *UnspentDB) HurryUp() {
	select {
		case db.hurryup <- true:
		default:
	}
}

// Flush the data and close all the files
func (db *UnspentDB) Close() {
	db.volatimemode = false
	db.Idle()
	if db.WritingInProgress.Get() {
		db.writingDone.Wait()
		db.WritingInProgress.Clr()
	}
}


// Get given unspent output
func (db *UnspentDB) UnspentGet(po *btc.TxPrevOut) (res *btc.TxOut) {
	var ind UtxoKeyType
	var v unsafe.Pointer
	copy(ind[:], po.Hash[:])

	db.RWMutex.RLock()
	v = db.HashMap[ind]
	db.RWMutex.RUnlock()
	if v != nil {
		res = OneUtxoRec(ind, _slice(v), po.Vout)
	}

	return
}


// Returns true if gived TXID is in UTXO
func (db *UnspentDB) TxPresent(id *btc.Uint256) (res bool) {
	var ind UtxoKeyType
	copy(ind[:], id.Hash[:])
	db.RWMutex.RLock()
	_, res = db.HashMap[ind]
	db.RWMutex.RUnlock()
	return
}


func (db *UnspentDB) del(hash []byte, outs []bool) {
	var ind UtxoKeyType
	copy(ind[:], hash)
	db.RWMutex.RLock()
	v := db.HashMap[ind]
	db.RWMutex.RUnlock()
	if v==nil {
		return // no such txid in UTXO (just ignorde delete request)
	}
	rec := NewUtxoRec(ind, _slice(v))
	if db.CB.NotifyTxDel!=nil {
		db.CB.NotifyTxDel(rec, outs)
	}
	var anyout bool
	for i, rm := range outs {
		if rm {
			rec.Outs[i] = nil
		} else if rec.Outs[i] != nil {
			anyout = true
		}
	}
	db.RWMutex.Lock()
	if anyout {
		db.HashMap[ind] = malloc_and_copy(rec.Bytes())
	} else {
		delete(db.HashMap, ind)
	}
	db.RWMutex.Unlock()
	free(v)
}


func (db *UnspentDB) commit(changes *BlockChanges) {
	// Now aplly the unspent changes
	for _, rec := range changes.AddList {
		var ind UtxoKeyType
		copy(ind[:], rec.TxID[:])
		if db.CB.NotifyTxAdd!=nil {
			db.CB.NotifyTxAdd(rec)
		}
		db.RWMutex.Lock()
		db.HashMap[ind] = malloc_and_copy(rec.Bytes())
		db.RWMutex.Unlock()
	}
	for k, v := range changes.DeledTxs {
		db.del(k[:], v)
	}
}


func (db *UnspentDB) AbortWriting() {
	db.Mutex.Lock()
	db.abortWriting()
	db.Mutex.Unlock()
}

func (db *UnspentDB) abortWriting() {
	if db.WritingInProgress.Get() {
		db.abortwritingnow <- true
		db.writingDone.Wait()
		select {
		case <- db.abortwritingnow:
		default:
		}
		db.WritingInProgress.Clr() // empty the channel
	}
}

func (db *UnspentDB) UTXOStats() (s string) {
	var outcnt, sum, sumcb uint64
	var totdatasize, unspendable, unspendable_recs, unspendable_bytes uint64

	db.RWMutex.RLock()

	lele := len(db.HashMap)

	for k, v := range db.HashMap {
		totdatasize += uint64(_len(v)+8)
		rec := NewUtxoRecStatic(k, _slice(v))
		var spendable_found bool
		for _, r := range rec.Outs {
			if r!=nil {
				outcnt++
				sum += r.Value
				if rec.Coinbase {
					sumcb += r.Value
				}
				if len(r.PKScr)>0 && r.PKScr[0]==0x6a {
					unspendable++
					unspendable_bytes += uint64(8+len(r.PKScr))
				} else {
					spendable_found = true
				}
			}
		}
		if !spendable_found {
			unspendable_recs++
		}
	}

	db.RWMutex.RUnlock()

	s = fmt.Sprintf("UNSPENT: %.8f BTC in %d outs from %d txs. %.8f BTC in coinbase.\n",
		float64(sum)/1e8, outcnt, lele, float64(sumcb)/1e8)
	s += fmt.Sprintf(" TotalData:%.1fMB  MaxTxOutCnt:%d  DirtyDB:%t  Writing:%t  Abort:%t\n",
		float64(totdatasize)/1e6, len(rec_outs), db.DirtyDB.Get(), db.WritingInProgress.Get(), len(db.abortwritingnow) > 0)
	s += fmt.Sprintf(" Last Block : %s @ %d\n", btc.NewUint256(db.LastBlockHash).String(),
		db.LastBlockHeight)
	s += fmt.Sprintf(" Unspendable outputs: %d (%dKB)  txs:%d\n",
		unspendable, unspendable_bytes>>10, unspendable_recs)

	return
}


// Return DB statistics
func (db *UnspentDB) GetStats() (s string) {
	db.RWMutex.RLock()
	hml := len(db.HashMap)
	db.RWMutex.RUnlock()

	s = fmt.Sprintf("UNSPENT: %d records. MaxTxOutCnt:%d  DirtyDB:%t  Writing:%t  Abort:%t\n",
		hml, len(rec_outs), db.DirtyDB.Get(), db.WritingInProgress.Get(), len(db.abortwritingnow) > 0)
	s += fmt.Sprintf(" Last Block : %s @ %d\n", btc.NewUint256(db.LastBlockHash).String(),
		db.LastBlockHeight)
	return
}

func (db *UnspentDB) PurgeUnspendable(all bool) {
	var unspendable_txs, unspendable_recs uint64
	db.Mutex.Lock()
	db.abortWriting()

	db.RWMutex.Lock()

	for k, v := range db.HashMap {
		rec := NewUtxoRecStatic(k, _slice(v))
		var spendable_found bool
		var record_removed uint64
		for idx, r := range rec.Outs {
			if r!=nil {
				if len(r.PKScr)>0 && r.PKScr[0]==0x6a {
					unspendable_recs++
					if all {
						rec.Outs[idx] = nil
						record_removed++
					}
				} else {
					spendable_found = true
				}
			}
		}
		if !spendable_found {
			free(v)
			delete(db.HashMap, k)
			unspendable_txs++
		} else if record_removed>0 {
			free(v)
			db.HashMap[k] = malloc_and_copy(rec.Serialize(false))
			unspendable_recs += record_removed
		}
	}
	db.RWMutex.Unlock()

	db.Mutex.Unlock()

	fmt.Println("Purged", unspendable_txs, "transactions and", unspendable_recs, "extra records")
}
