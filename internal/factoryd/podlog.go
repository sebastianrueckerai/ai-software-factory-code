package factoryd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"time"
)

// maxLogLine bounds one worker log line; a longer one is dropped (and
// counted, so a reopened stream skips it too), never fatal to the stream.
const maxLogLine = 1 << 20

// followPodLog delivers a pod's log lines until the pod has exited and its
// log is drained, or is gone. A follow stream can end while the container
// still runs (the API server or kubelet drops it, a read fails); the log is
// then reopened, and the lines already delivered, which a reopened stream
// replays from the start, are skipped. The channel closes at the end; ctx
// ends it early.
func followPodLog(ctx context.Context, kube Kube, ns, pod, taskID string) <-chan []byte {
	lines := make(chan []byte, 1024)
	go func() {
		defer close(lines)
		seen, drainedAfterExit := 0, false
		for {
			stream, err := kube.PodLog(ctx, ns, pod)
			if err == nil {
				seen, err = readLogLines(ctx, stream, seen, lines)
				stream.Close() // #nosec G104 -- a read-only log stream; its close error changes nothing
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
	return lines
}

// readLogLines sends the stream's lines after the first skip ones and
// returns how many lines the stream has had in total.
func readLogLines(ctx context.Context, stream io.Reader, skip int, out chan<- []byte) (int, error) {
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
		if n > skip && !tooLong {
			select {
			case out <- bytes.TrimRight(line, "\r\n"):
			case <-ctx.Done():
				return n, ctx.Err()
			}
		}
		line, tooLong = nil, false
	}
}
