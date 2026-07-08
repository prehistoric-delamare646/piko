package piko

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
)

type part struct {
	index int
	start int64
	end   int64
}

func (d *downloader) downloadParts(ctx context.Context, output string, size int64, partSize int64, concurrency int, force bool) error {
	discard := IsNullOutput(output)
	partPath := ""
	var writer io.WriterAt = discardWriterAt{}
	var file *os.File
	if !discard {
		partPath = output + ".part"
		if err := prepareTemp(partPath); err != nil {
			return err
		}

		var err error
		file, err = os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		if err := file.Truncate(size); err != nil {
			file.Close()
			_ = os.Remove(partPath)
			return err
		}
		writer = file
	}

	err := d.downloadPartsToWriter(ctx, writer, size, partSize, concurrency)
	if closeErr := closeFile(file); err == nil {
		err = closeErr
	}
	if err != nil {
		if !discard {
			_ = os.Remove(partPath)
		}
		return err
	}

	if discard {
		return nil
	}
	return finishOutput(partPath, output, force)
}

func (d *downloader) downloadPartsToWriter(ctx context.Context, writer io.WriterAt, size int64, partSize int64, concurrency int) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	d.done.Store(0)
	d.emitProgress(size, false)
	parts := make(chan part, concurrency*2)
	errCh := make(chan error, 1)
	var wg sync.WaitGroup

	wg.Go(func() {
		defer close(parts)
		index := 0
		for start := int64(0); start < size; start += partSize {
			end := min(start+partSize-1, size-1)
			index++
			p := part{index: index, start: start, end: end}
			select {
			case parts <- p:
			case <-ctx.Done():
				return
			}
		}
	})

	for workerID := range concurrency {
		client := d.clients[workerID%len(d.clients)]
		wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case p, ok := <-parts:
					if !ok {
						return
					}
					if err := d.downloadRange(ctx, client, writer, p); err != nil {
						select {
						case errCh <- err:
							cancel()
						default:
						}
						return
					}
				}
			}
		})
	}
	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	d.emitProgress(size, true)
	return nil
}

func (d *downloader) downloadRange(ctx context.Context, client *http.Client, writer io.WriterAt, p part) error {
	offset := p.start
	var lastErr error
	for attempt := 0; attempt <= d.retries; attempt++ {
		if offset > p.end {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		attemptCtx, cancel := context.WithCancel(ctx)
		req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, d.url, nil)
		if err != nil {
			cancel()
			return err
		}
		d.setCommonHeaders(req)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, p.end))

		resp, err := client.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			if !isRetryableDownloadError(err) {
				return err
			}
		} else {
			err = d.copyRange(attemptCtx, cancel, writer, resp, p.index, offset, p.end, &offset)
			resp.Body.Close()
			cancel()
			if err == nil {
				return nil
			}
			lastErr = err
			if !isRetryableDownloadError(err) {
				return err
			}
		}

		if attempt < d.retries {
			if err := sleepWithContext(ctx, retryDelay(attempt)); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("part %d failed at byte %d: %w", p.index, offset, lastErr)
}

func (d *downloader) copyRange(ctx context.Context, cancel context.CancelFunc, writer io.WriterAt, resp *http.Response, partIndex int, requestStart int64, requestEnd int64, offset *int64) error {
	if resp.StatusCode != http.StatusPartialContent {
		return httpStatusError{partIndex: partIndex, code: resp.StatusCode, status: resp.Status}
	}
	if err := validateContentRange(partIndex, resp.Header.Get("Content-Range"), requestStart, requestEnd); err != nil {
		return err
	}

	buf := make([]byte, copyBufferSize)
	progress := d.startStallMonitor(cancel)
	if progress != nil {
		defer close(progress)
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		readSize := min(int64(len(buf)), requestEnd-*offset+1)
		n, readErr := resp.Body.Read(buf[:int(readSize)])
		if n > 0 {
			writeSize := min(int64(n), requestEnd-*offset+1)
			written, writeErr := writer.WriteAt(buf[:int(writeSize)], *offset)
			if writeErr != nil {
				return writeErr
			}
			if int64(written) != writeSize {
				return io.ErrShortWrite
			}
			*offset += writeSize
			d.addProgress(writeSize, 0)
			if progress != nil {
				select {
				case progress <- struct{}{}:
				default:
				}
			}
			if writeSize < int64(n) || *offset > requestEnd {
				return nil
			}
		}

		if readErr == io.EOF {
			if *offset > requestEnd {
				return nil
			}
			return io.ErrUnexpectedEOF
		}
		if readErr != nil {
			return readErr
		}
	}
}

type discardWriterAt struct{}

func (discardWriterAt) WriteAt(p []byte, _ int64) (int, error) {
	return len(p), nil
}
