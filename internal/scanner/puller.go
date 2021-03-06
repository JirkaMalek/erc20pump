// Package scanner performs the scanning task.
package scanner

import (
	"bytes"
	"erc20pump/internal/cfg"
	"erc20pump/internal/scanner/cache"
	"erc20pump/internal/scanner/rpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"log"
	"sync"
	"time"
)

// logBufferCapacity represents the capacity of collected log records.
const logBufferCapacity = 100

// defaultLogsWindowSize represents the maximal number of blocks we try to pull at once.
const defaultLogsWindowSize = 5

// logPuller represents log record pulling service
type logPuller struct {
	output        chan types.Log
	topBlock      uint64
	currentBlock  uint64
	sigStop       chan bool
	wg            *sync.WaitGroup
	rpc           *rpc.Adapter
	cache         *cache.MemCache
	topics        [][]common.Hash
	contractMatch func(rc *common.Address) bool
}

// newPuller creates a new puller service.
func newPuller(cfg *cfg.Config, rpc *rpc.Adapter, cache *cache.MemCache) *logPuller {
	// build a list of topics we want to scan for
	topics := [][]common.Hash{make([]common.Hash, 0, len(LogTopicProcessor))}
	for t := range LogTopicProcessor {
		topics[0] = append(topics[0], t)
	}

	// make the puller
	return &logPuller{
		output:       make(chan types.Log, logBufferCapacity),
		topBlock:     0,
		currentBlock: cfg.StartBlock,
		sigStop:      make(chan bool, 1),
		topics:       topics,
		rpc:          rpc,
		cache:        cache,
		contractMatch: func(rc *common.Address) bool {
			return bytes.Compare(rc.Bytes(), cfg.ScanContract.Bytes()) == 0
		},
	}
}

// run the log puller service.
func (lp *logPuller) run(wg *sync.WaitGroup) {
	lp.wg = wg

	wg.Add(1)
	go lp.scan()
}

// stop signals the log puller thread to terminate.
func (lp *logPuller) stop() {
	lp.sigStop <- true
}

// scan the blockchain for log records of interest.
func (lp *logPuller) scan() {
	tick := time.NewTicker(500 * time.Millisecond)
	info := time.NewTicker(5 * time.Second)

	defer func() {
		tick.Stop()
		info.Stop()
		close(lp.output)

		log.Println("log puller terminated")
		lp.wg.Done()
	}()

	var logs []types.Log
	var record types.Log
	for {
		// terminate if requested
		select {
		case <-lp.sigStop:
			return
		case <-tick.C:
			lp.fetchHead()
		case <-info.C:
			log.Println("scanner at #", lp.currentBlock, "head at #", lp.topBlock)
		default:
		}

		// do we have a log record to process?
		if logs == nil || len(logs) == 0 {
			logs = lp.nextLogs()
			continue
		}

		// get the next record and process
		record, logs = logs[0], logs[1:]
		lp.process(record)
	}
}

// fetchHead updates the current known head block index.
func (lp *logPuller) fetchHead() {
	var err error

	lp.topBlock, err = lp.rpc.TopBlock()
	if err != nil {
		log.Println("error pulling the current head", err.Error())
	}
}

// nextLogs pulls the next set of log records from the backend server.
func (lp *logPuller) nextLogs() []types.Log {
	// do we even have anything to pull?
	if lp.currentBlock > lp.topBlock {
		return nil
	}

	// what is our current target?
	target := lp.currentBlock + defaultLogsWindowSize
	if target > lp.topBlock {
		target = lp.topBlock
	}

	// pull the data from remote server
	logs, err := lp.rpc.GetLogs(lp.topics, lp.currentBlock, target)
	if err != nil {
		log.Println("failed to pull logs", err.Error())
		return nil
	}

	// advance current block
	lp.currentBlock = target + 1
	return logs
}

// process given event log record.
func (lp *logPuller) process(ev types.Log) {
	// do we know the transaction recipient?
	rec, err := lp.cache.TrxRecipient(ev.TxHash, lp.rpc.TrxRecipient)
	if err != nil {
		log.Println("recipient not available", err.Error())
		return
	}

	// is the recipient interesting?
	if !lp.contractMatch(&rec) {
		return
	}

	log.Println("match", rec.String(), "on", ev.TxHash.String())

	// this one is what we're looking for
	lp.output <- ev
}
