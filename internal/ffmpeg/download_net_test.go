//go:build nettest

// This file is excluded from normal runs: it reaches the internet, so it only
// runs with -tags nettest. It exists to check that the configured download
// sources still answer, and to measure them, which is how the gyan.dev
// throttling was found in the first place.
package ffmpeg

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestRealSourcesRespond(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const sample = 4 << 20 // 4 MB is enough to judge throughput

	for _, src := range Sources() {
		url, err := src.address(ctx)
		if err != nil {
			t.Errorf("%s: resolve: %v", src.Name, err)
			continue
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Errorf("%s: %v", src.Name, err)
			continue
		}
		req.Header.Set("Range", "bytes=0-4194303")

		started := time.Now()
		resp, err := downloadClient.Do(req)
		if err != nil {
			t.Errorf("%s: %v", src.Name, err)
			continue
		}
		n, copyErr := io.Copy(io.Discard, io.LimitReader(resp.Body, sample))
		resp.Body.Close()
		elapsed := time.Since(started)

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			t.Errorf("%s: HTTP %s", src.Name, resp.Status)
			continue
		}
		if copyErr != nil {
			t.Errorf("%s: read: %v (after %s)", src.Name, copyErr, HumanBytes(n))
			continue
		}
		rate := int64(float64(n) / elapsed.Seconds())
		t.Logf("%-34s %-8s %s in %s (%s)", src.Name, resp.Status[:3], HumanBytes(n), elapsed.Round(time.Millisecond), HumanRate(rate))
	}
}
