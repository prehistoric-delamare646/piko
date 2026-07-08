package piko

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type remoteInfo struct {
	size      int64
	rangeable bool
	suggested string
	finalURL  string
}

func (d *downloader) inspect(ctx context.Context) (remoteInfo, error) {
	info := remoteInfo{}
	headStatus := 0

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, d.url, nil)
	if err == nil {
		d.setCommonHeaders(req)
		resp, err := d.client.Do(req)
		if err == nil {
			resp.Body.Close()
			headStatus = resp.StatusCode
			info.finalURL = resp.Request.URL.String()
			if resp.StatusCode < 400 {
				info.size = resp.ContentLength
				info.suggested = filenameFromDisposition(resp.Header.Get("Content-Disposition"))
				info.rangeable = strings.EqualFold(resp.Header.Get("Accept-Ranges"), "bytes")
			}
		}
	}

	if info.size > 0 && info.rangeable {
		return info, nil
	}

	probed, probeErr := d.probeRange(ctx)
	if probeErr == nil {
		if probed.size > 0 {
			info.size = probed.size
		}
		if probed.suggested != "" {
			info.suggested = probed.suggested
		}
		if probed.finalURL != "" {
			info.finalURL = probed.finalURL
		}
		info.rangeable = probed.rangeable
	}

	if probeErr != nil && (headStatus == 0 || headStatus >= 400) {
		return info, probeErr
	}
	return info, nil
}

func (d *downloader) probeRange(ctx context.Context) (remoteInfo, error) {
	var lastErr error
	for attempt := 0; attempt <= d.retries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.url, nil)
		if err != nil {
			return remoteInfo{}, err
		}
		d.setCommonHeaders(req)
		req.Header.Set("Range", "bytes=0-0")

		resp, err := d.client.Do(req)
		if err != nil {
			lastErr = err
			if attempt < d.retries {
				if err := sleepWithContext(ctx, retryDelay(attempt)); err != nil {
					return remoteInfo{}, err
				}
			}
			continue
		}

		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1))

		if resp.StatusCode == http.StatusPartialContent {
			return remoteInfo{
				size:      parseContentRangeSize(resp.Header.Get("Content-Range")),
				rangeable: true,
				suggested: filenameFromDisposition(resp.Header.Get("Content-Disposition")),
				finalURL:  resp.Request.URL.String(),
			}, nil
		}
		if resp.StatusCode >= 400 {
			return remoteInfo{}, fmt.Errorf("range probe failed: %s", resp.Status)
		}
		return remoteInfo{
			size:      resp.ContentLength,
			rangeable: false,
			suggested: filenameFromDisposition(resp.Header.Get("Content-Disposition")),
			finalURL:  resp.Request.URL.String(),
		}, nil
	}
	return remoteInfo{}, lastErr
}
