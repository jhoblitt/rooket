package cmd

import (
	"bytes"
	"errors"
	"io"
	"sync"
)

// runConcurrent runs every fn concurrently, each writing to its own buffer,
// then flushes the buffers to out in the order the fns were passed and returns
// their joined errors. Ordered, buffered flushing keeps concurrent output
// readable — the same discipline the per-node scripts use (cluster.forEachNode)
// — at the cost of showing a branch's output only once all branches finish.
//
// It is rooket's building block for the design goal that every command exploit
// all available concurrency (see docs/design/concurrency.md): independent steps
// that share no data dependency are handed to one runConcurrent call rather
// than run in sequence.
//
// Each goroutine writes only to its own buffer, so the buffers need no locking;
// out is written from a single goroutine after the join. A branch that must not
// abort its siblings (a best-effort optimization) should return nil and log its
// own failure as a warning.
func runConcurrent(out io.Writer, fns ...func(io.Writer) error) error {
	if len(fns) == 0 {
		return nil
	}
	bufs := make([]bytes.Buffer, len(fns))
	errs := make([]error, len(fns))
	var wg sync.WaitGroup
	for i, fn := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = fn(&bufs[i])
		}()
	}
	wg.Wait()
	for i := range fns {
		out.Write(bufs[i].Bytes())
	}
	return errors.Join(errs...)
}
