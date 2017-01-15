package buffer

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	log "github.com/golang/glog"

	"github.com/youtube/vitess/go/sync2"
	"github.com/youtube/vitess/go/vt/logutil"
	"github.com/youtube/vitess/go/vt/topo/topoproto"
	"github.com/youtube/vitess/go/vt/vterrors"

	vtrpcpb "github.com/youtube/vitess/go/vt/proto/vtrpc"
)

// bufferState represents the different states a shardBuffer object can be in.
type bufferState string

const (
	// stateIdle means no failover is currently in progress.
	stateIdle bufferState = "IDLE"
	// stateBuffering is the phase when a failover is in progress.
	stateBuffering bufferState = "BUFFERING"
	// stateDraining is the phase when a failover ended and the queue is drained.
	stateDraining bufferState = "DRAINING"
)

// shardBuffer buffers requests during a failover for a particular shard.
// The object will be reused across failovers. If no failover is currently in
// progress, the state is "IDLE".
//
// Note that this object is accessed concurrently by multiple threads:
// - vtgate request threads
// - discovery.HealthCheck listener execution thread
// - timeout thread (timeout_thread.go) to evict too old buffered requests
// - drain() thread
type shardBuffer struct {
	// Immutable fields set at construction.
	keyspace       string
	shard          string
	bufferSizeSema *sync2.Semaphore
	// statsKey is used to update the stats variables.
	statsKey     []string
	logTooRecent *logutil.ThrottledLogger

	// mu guards the fields below.
	mu    sync.RWMutex
	state bufferState
	// queue is the list of buffered requests (ordered by arrival).
	queue []*entry
	// externallyReparented tracks the last time each shard was reparented.
	// The value is the seen maximum value of
	// "StreamHealthResponse.TabletexternallyReparentedTimestamp".
	externallyReparented int64
	// lastStart is the last time we saw the start of a failover.
	lastStart time.Time
	// lastEnd is the last time we saw the end of a failover.
	lastEnd time.Time
	// timeoutThread will be set while a failover is in progress and the object is
	// in the BUFFERING state.
	timeoutThread *timeoutThread
}

// entry is created per buffered request.
type entry struct {
	// done will be closed by shardBuffer when the failover is over and the
	// request can be retried.
	// Any Go routine closing this channel must also remove the entry from the
	// ShardBuffer queue such that nobody else tries to close it.
	done chan struct{}

	// deadline is the time when the entry is out of the buffering window and it
	// must be canceled.
	deadline time.Time

	// err is set if the buffering failed e.g. when the entry was evicted.
	err error

	// bufferCtx wraps the request ctx and is used to track the retry of a
	// request during the drain phase. Once the retry is done, the caller
	// must cancel this context (by calling bufferCancel).
	bufferCtx    context.Context
	bufferCancel func()
}

func newShardBuffer(keyspace, shard string, bufferSizeSema *sync2.Semaphore) *shardBuffer {
	return &shardBuffer{
		keyspace:       keyspace,
		shard:          shard,
		bufferSizeSema: bufferSizeSema,
		statsKey:       []string{keyspace, shard},
		logTooRecent:   logutil.NewThrottledLogger(fmt.Sprintf("FailoverTooRecent-%v", topoproto.KeyspaceShardString(keyspace, shard)), 5*time.Second),
		state:          stateIdle,
	}
}

func (sb *shardBuffer) waitForFailoverEnd(ctx context.Context, keyspace, shard string, err error) (RetryDoneFunc, error) {
	// We assume if err != nil then it's always caused by a failover.
	// Other errors must be filtered at higher layers.
	failoverDetected := err != nil

	// Fast path (read lock): Check if we should NOT buffer a request.
	sb.mu.RLock()
	if !sb.shouldBufferLocked(failoverDetected) {
		// No buffering required. Return early.
		sb.mu.RUnlock()
		return nil, nil
	}
	sb.mu.RUnlock()

	// Buffering required. Acquire write lock.
	sb.mu.Lock()
	// Re-check state because it could have changed in the meantime.
	if !sb.shouldBufferLocked(failoverDetected) {
		// Buffering no longer required. Return early.
		sb.mu.Unlock()
		return nil, nil
	}

	// Start buffering if failover is not detected yet.
	if sb.state == stateIdle {
		// Do not buffer if last failover is too recent.
		if d := time.Now().Sub(sb.lastEnd); d < *minTimeBetweenFailovers {
			sb.mu.Unlock()
			sb.logTooRecent.Infof("NOT starting buffering for shard: %s because the last failover is too recent (%v < %v)."+
				" (A failover was detected by this seen error: %v.)",
				topoproto.KeyspaceShardString(keyspace, shard), d, *minTimeBetweenFailovers, err)
			return nil, nil
		}

		sb.startBufferingLocked(err)
	}
	entry, err := sb.bufferRequestLocked(ctx)
	sb.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return entry.bufferCancel, sb.wait(ctx, entry)
}

// shouldBufferLocked returns true if the current request should be buffered
// (based on the current state and whether the request detected a failover).
func (sb *shardBuffer) shouldBufferLocked(failoverDetected bool) bool {
	switch s := sb.state; {
	case s == stateIdle && !failoverDetected:
		// No failover in progress.
		return false
	case s == stateIdle && failoverDetected:
		// Not buffering yet, but new failover detected.
		return true
	case s == stateBuffering:
		// Failover in progress.
		return true
	case s == stateDraining && !failoverDetected:
		// Draining. Non-failover related requests can pass through.
		return false
	case s == stateDraining && failoverDetected:
		// Possible race between request which saw failover-related error and the
		// end of the failover. Do not buffer and let vtgate retry immediately.
		return false
	}
	panic("BUG: All possible states must be covered by the switch expression above.")
}

func (sb *shardBuffer) startBufferingLocked(err error) {
	// Reset monitoring data from previous failover.
	requestsInFlightMax.Set(sb.statsKey, 0)
	failoverDurationMs.Set(sb.statsKey, 0)

	sb.lastStart = time.Now()
	sb.logErrorIfStateNotLocked(stateIdle)
	sb.state = stateBuffering
	sb.queue = make([]*entry, 0)

	sb.timeoutThread = newTimeoutThread(sb)
	sb.timeoutThread.start()
	log.Infof("Starting buffering for shard: %s (window: %v, size: %v, max failover duration: %v) (A failover was detected by this seen error: %v.)", topoproto.KeyspaceShardString(sb.keyspace, sb.shard), *window, *size, *maxFailoverDuration, err)
}

// logErrorIfStateNotLocked logs an error if the current state is not "state".
// We do not panic/crash the process here because it is expected that a wrong
// state is less severe than (potentially) crash-looping all vtgates.
// Note: The prefix "Locked" is not related to the state. Instead, it stresses
// that "sb.mu" must be locked before calling the method.
func (sb *shardBuffer) logErrorIfStateNotLocked(state bufferState) {
	if sb.state != state {
		log.Errorf("BUG: Buffer state should be '%v' and not '%v'. Full state of buffer object: %#v Stacktrace:\n%s", state, sb.state, sb, debug.Stack())
	}
}

// bufferRequest creates a new entry in the queue for a request which
// should be buffered.
// It returns *entry which can be used as input for shardBuffer.cancel(). This
// is useful for canceled RPCs (e.g. due to deadline exceeded) which want to
// give up their spot in the buffer. It also holds the "bufferCancel" function.
// If buffering fails e.g. due to a full buffer, an error is returned.
func (sb *shardBuffer) bufferRequestLocked(ctx context.Context) (*entry, error) {
	if !sb.bufferSizeSema.TryAcquire() {
		// Buffer is full. Evict the oldest entry and buffer this request instead.
		if len(sb.queue) == 0 {
			// Overall buffer is full, but this shard's queue is empty. That means
			// there is at least one other shard failing over as well which consumes
			// the whole buffer.
			return nil, bufferFullError
		}

		e := sb.queue[0]
		// Evict the entry. Do not release its slot in the buffer and reuse it for
		// this new request.
		// NOTE: We keep the lock to avoid racing with drain().
		sb.unblockAndWait(e, entryEvictedError, false /* releaseSlot */)
		sb.queue = sb.queue[1:]
	}

	e := &entry{
		done:     make(chan struct{}),
		deadline: time.Now().Add(*window),
	}
	e.bufferCtx, e.bufferCancel = context.WithCancel(ctx)
	sb.queue = append(sb.queue, e)
	requestsInFlightMax.Add(sb.statsKey, 1)
	if len(sb.queue) == 1 {
		sb.timeoutThread.notifyQueueNotEmpty()
	}
	return e, nil
}

// unblockAndWait unblocks a blocked request and waits until it reported its end.
func (sb *shardBuffer) unblockAndWait(e *entry, err error, releaseSlot bool) {
	// Set error such that the request will see it.
	e.err = err
	// Tell blocked request to stop waiting.
	close(e.done)
	// Wait for unblocked request to end.
	<-e.bufferCtx.Done()
	if releaseSlot {
		sb.bufferSizeSema.Release()
	}
}

// wait blocks while the request is buffered during the failover.
func (sb *shardBuffer) wait(ctx context.Context, e *entry) error {
	select {
	case <-ctx.Done():
		sb.remove(e)
		return vterrors.FromError(vtrpcpb.ErrorCode_TRANSIENT_ERROR, fmt.Errorf("context was canceled before failover finished (%v)", ctx.Err()))
	case <-e.done:
		return e.err
	}
}

// oldestEntry returns the head of the queue or nil if the queue is empty.
func (sb *shardBuffer) oldestEntry() *entry {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	if len(sb.queue) > 0 {
		return sb.queue[0]
	}
	return nil
}

// evictOldestEntry is used by timeoutThread to evict the head entry of the
// queue if it exceeded its buffering window.
func (sb *shardBuffer) evictOldestEntry(e *entry) {
	sb.mu.Lock()
	if len(sb.queue) == 0 || e != sb.queue[0] {
		// Entry is already removed e.g. by remove(). Ignore it.
		return
	}
	sb.unblockAndWait(e, nil /* err */, true /* releaseSlot */)
	sb.queue = sb.queue[1:]
	sb.mu.Unlock()
}

// remove must be called when the request was canceled from outside and not
// internally.
func (sb *shardBuffer) remove(toRemove *entry) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	if sb.queue == nil {
		// Queue is cleared because we're already in the DRAIN phase.
		return
	}

	// If entry is still in the queue, delete it and cancel it internally.
	for i, e := range sb.queue {
		if e == toRemove {
			// Delete entry at index "i" from slice.
			sb.queue = append(sb.queue[:i], sb.queue[i+1:]...)
			// Entry was not canceled internally yet. Finish it explicitly. This way,
			// timeoutThread will find out about it as well.
			close(toRemove.done)
			return
		}
	}

	// Entry was already removed. Keep the queue as it is.
}

func (sb *shardBuffer) recordExternallyReparentedTimestamp(timestamp int64) {
	// Fast path (read lock): Check if new timestamp is higher.
	sb.mu.RLock()
	if timestamp <= sb.externallyReparented {
		// Do nothing. Equal values are reported if the MASTER has not changed.
		// Smaller values can be reported during the failover by the old master
		// after the new master already took over.
		sb.mu.RUnlock()
		return
	}
	sb.mu.RUnlock()

	// New timestamp is higher. Stop buffering if running.
	sb.mu.Lock()
	defer sb.mu.Unlock()

	// Re-check value after acquiring write lock.
	if timestamp <= sb.externallyReparented {
		return
	}

	sb.externallyReparented = timestamp
	sb.stopBufferingLocked("failover end detected")
}

func (sb *shardBuffer) stopBufferingDueToMaxDuration() {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	sb.stopBufferingLocked(fmt.Sprintf("stopping buffering because failover did not finish in time (%v)", *maxFailoverDuration))
}

func (sb *shardBuffer) stopBufferingLocked(reason string) {
	if sb.state != stateBuffering {
		return
	}

	// Stop buffering.
	sb.lastEnd = time.Now()
	d := time.Since(sb.lastStart)
	failoverDurationMs.Set(sb.statsKey, int64(d/time.Millisecond))

	sb.logErrorIfStateNotLocked(stateBuffering)
	sb.state = stateDraining
	q := sb.queue
	// Clear the queue such that remove(), oldestEntry() and evictOldestEntry()
	// will not work on obsolete data.
	sb.queue = nil

	log.Infof("Stopping buffering for shard: %s after: %.1f seconds due to: %v. Draining %d buffered requests now.", topoproto.KeyspaceShardString(sb.keyspace, sb.shard), d.Seconds(), reason, len(q))

	// Start the drain. (Use a new Go routine to release the lock.)
	go sb.drain(q)
}

func (sb *shardBuffer) drain(q []*entry) {
	// stop must be called outside of the lock because the thread may access
	// shardBuffer as well e.g. to get the current oldest entry.
	sb.timeoutThread.stop()

	start := time.Now()
	// TODO(mberlin): Parallelize the drain by pumping the data through a channel.
	for _, e := range q {
		sb.unblockAndWait(e, nil /* err */, true /* releaseSlot */)
	}
	d := time.Since(start)
	log.Infof("Draining finished for shard: %s Took: %v for: %d requests.", topoproto.KeyspaceShardString(sb.keyspace, sb.shard), d, len(q))

	// Draining is done. Change state from "draining" to "idle".
	sb.mu.Lock()
	defer sb.mu.Unlock()
	sb.logErrorIfStateNotLocked(stateDraining)
	sb.state = stateIdle
	sb.timeoutThread = nil
}

// sizeForTesting is used by the unit test only to find out the current number
// of buffered requests.
// TODO(mberlin): Remove this if we add a more general statistics reporting.
func (sb *shardBuffer) sizeForTesting() int {
	sb.mu.RLock()
	defer sb.mu.RUnlock()
	return len(sb.queue)
}

// stateForTesting is used by unit tests only to probe the current state.
func (sb *shardBuffer) stateForTesting() bufferState {
	sb.mu.RLock()
	defer sb.mu.RUnlock()
	return sb.state
}
