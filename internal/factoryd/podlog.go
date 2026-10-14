package factoryd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"
)

// maxLogLine bounds one worker log line; a longer one is dropped (and
// counted, so a reopened stream skips it too), never fatal to the stream.
const maxLogLine = 1 << 20

// maxLogBuffer bounds the log read ahead of the session loop.
const maxLogBuffer = 64 << 20

// podLog is a followed pod log: lines delivers every line in order, and
// err, once lines is closed, says whether the follower gave up.
type podLog struct {
	lines chan []byte

	mu     sync.Mutex
	cond   *sync.Cond
	queue  [][]byte
	size   int
	cap    int
	done   bool
	failed error
}

func (p *podLog) err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failed
}

// push queues a line; false when the read-ahead cap is exceeded.
func (p *podLog) push(line []byte) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.size+len(line) > p.cap {
		p.failed = fmt.Errorf("worker log: more than %d bytes read ahead of the session", p.cap)
		return false
	}
	p.queue = append(p.queue, line)
	p.size += len(line)
	p.cond.Signal()
	return true
}

func (p *podLog) finish() {
	p.mu.Lock()
	p.done = true
	p.cond.Signal()
	p.mu.Unlock()
}

// pump hands queued lines to the consumer, then closes lines.
func (p *podLog) pump(ctx context.Context) {
	defer close(p.lines)
	for {
		p.mu.Lock()
		for len(p.queue) == 0 && !p.done {
			p.cond.Wait()
		}
		if len(p.queue) == 0 {
			p.mu.Unlock()
			return
		}
		line := p.queue[0]
		p.queue[0] = nil
		p.queue = p.queue[1:]
		p.size -= len(line)
		p.mu.Unlock()
		select {
		case p.lines <- line:
		case <-ctx.Done():
			return
		}
	}
}

// followPodLog delivers a pod's log lines until the pod has exited and its
// log is drained, or is gone. It reads ahead without waiting for the
// consumer (a supervisor call may block the session loop while the pod is
// deleted), up to maxLogBuffer, and hands each line to tap (nil: none)
// once, as it reads it. A follow stream can end while the container still
// runs (the API server or kubelet drops it, a read fails); the log is then
// reopened, and the lines already read, which a reopened stream replays
// from the start, are skipped. This assumes the kubelet has not rotated
// the container log in between. ctx ends it early.
func followPodLog(ctx context.Context, kube Kube, ns, pod, taskID string, tap func([]byte)) *podLog {
	return followPodLogCap(ctx, kube, ns, pod, taskID, 0, maxLogBuffer, tap)
}

// followPodLogFrom follows the log past its first seen lines: a
// reattached session reads on from where the stopped factoryd's loop was.
func followPodLogFrom(ctx context.Context, kube Kube, ns, pod, taskID string, seen int, tap func([]byte)) *podLog {
	return followPodLogCap(ctx, kube, ns, pod, taskID, seen, maxLogBuffer, tap)
}

func followPodLogCap(ctx context.Context, kube Kube, ns, pod, taskID string, seen, capBytes int, tap func([]byte)) *podLog {
	p := &podLog{lines: make(chan []byte), cap: capBytes}
	p.cond = sync.NewCond(&p.mu)
	go p.pump(ctx)
	go func() {
		defer p.finish()
		drainedAfterExit := false
		for {
			stream, err := kube.PodLog(ctx, ns, pod)
			if err == nil {
				seen, err = readLogLines(stream, seen, func(line []byte) bool {
					if tap != nil {
						tap(line)
					}
					return p.push(line)
				})
				stream.Close() // #nosec G104 -- a read-only log stream; its close error changes nothing
			}
			if p.err() != nil {
				log.Printf("task %s: pod %s: %v", taskID, pod, p.err())
				return
			}
			if ctx.Err() != nil {
				return
			}
			if err != nil && !errors.Is(err, io.EOF) {
				log.Printf("task %s: pod %s log: %v", taskID, pod, err)
			}
			phase, _, perr := kube.PodStatus(ctx, ns, pod)
			switch {
			case errors.Is(perr, ErrNotFound):
				return
			case perr == nil && (phase == "Succeeded" || phase == "Failed"):
				if drainedAfterExit {
					return
				}
				drainedAfterExit = true
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
	return p
}

// errLogFull stops a read whose line the consumer refused.
var errLogFull = errors.New("log buffer full")

// readLogLines hands the stream's lines after the first skip ones to emit
// and returns how many lines the stream has had in total.
func readLogLines(stream io.Reader, skip int, emit func([]byte) bool) (int, error) {
	br := bufio.NewReaderSize(stream, 64<<10)
	n := 0
	var line []byte
	tooLong := false
	for {
		part, err := br.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(part) > maxLogLine {
				tooLong, line = true, nil
			} else {
				line = append(line, part...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			// an unterminated tail is where the stream broke; a reopened
			// stream replays it whole (the worker ends every line)
			return n, err
		}
		n++
		if n > skip && !tooLong && !emit(bytes.TrimRight(line, "\r\n")) {
			return n, errLogFull
		}
		line, tooLong = nil, false
	}
}
