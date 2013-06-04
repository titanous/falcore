package filter

import (
	"github.com/fitstar/falcore"
	"net/http"
	"regexp"
	"sync/atomic"
	"time"
)

// Throttles incomming requests at a maximum number of 
// requests per second.  
type Throttler struct {
	Condition func(req *falcore.Request) bool // If this is set, and returns false, the request will not be throttled
	count     int64

	ticker  *time.Ticker
	tickerM *sync.RWMutex
}

// Type check
var _ falcore.RequestFilter = new(Throttler)

func NewThrottler(RPS int) *Throttler {
	th := new(Throttler)
	atomic.StoreInt64(&th.count, 0)
	th.tickerM = new(sync.RWMutex)
	if RPS > 0 {
		th.ticker = time.NewTicker(time.Second / time.Duration(RPS))
	}
	return th
}

func (t *Throttler) FilterRequest(req *falcore.Request) *http.Response {
	req.CurrentStage.Status = 0

	if t.Condition != nil && t.Condition(req) == false {
		return nil
	}

	t.tickerM.RLock()
	tt := t.ticker
	t.tickerM.RUnlock()

	if tt != nil {
		req.CurrentStage.Status = 1
		atomic.AddInt64(&t.count, 1)
		for {
			// If the req completes because the channel was closed,
			// grab the new ticker and try again.
			_, ok := <-tt.C
			if ok {
				break
			}

			// Get new ticker
			t.tickerM.RLock()
			tt = t.ticker
			t.tickerM.RUnlock()

			// If throttling has been disabled, continue.
			if t.ticker == nil {
				break
			}
		}
		atomic.AddInt64(&t.count, -1)
	}
	return nil
}

// Change the throttling limit
func (t *Throttler) SetRPS(RPS int) {
	t.tickerM.Lock()
	defer t.tickerM.Unlock()

	oldTicker := t.ticker
	if RPS > 0 {
		t.ticker = time.NewTicker(time.Second / time.Duration(RPS))
	} else {
		t.ticker = nil
	}
	oldTicker.Stop()   // doesn't close C
	close(oldTicker.C) // signal to waiting requests they should look for a new ticker
}

// Returns the number of requests waiting on the throttler
func (t *Throttler) Pending() int64 {
	return atomic.LoadInt64(&t.count)
}

// Logs the number of pending requests at WARN level every :interval
// :name is included in log line
// Does not log if nothing is being throttled.
func (t *Throttler) StartReporter(name string, interval time.Duration) {
	go func() {
		var waiting int64
		for {
			time.Sleep(interval)
			waiting = t.Pending()
			if waiting > 0 {
				falcore.Warn("%v: %v requests waiting", name, waiting)
			}
		}
	}()
}
