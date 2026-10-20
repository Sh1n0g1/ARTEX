package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// releaseCache is the layer that protects the GitHub quota: the unauthenticated API
// allows only 60 requests/hour/IP, and the top bar's "new version available" notice
// queries once per full page load. Once the cache stops working, opening a few extra
// tabs would burn through the quota, leaving it unable to check when an update is
// actually wanted.

func newTestCache(fetch func(context.Context, *http.Client) (*selfupdate.Release, error)) *releaseCache {
	return &releaseCache{fetch: fetch}
}

func TestReleaseCacheServesFromCache(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	for range 5 {
		rel, err := c.get(t.Context(), nil, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if rel.TagName != "v0.3.8" {
			t.Fatalf("TagName = %q", rel.TagName)
		}
	}
	if calls != 1 {
		t.Errorf("5 queries should only fetch once, got %d", calls)
	}
}

func TestReleaseCacheForceBypasses(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// Clicking "check for updates" must get a live result, otherwise a just-published
	// version would stay invisible until the cache expires.
	if _, err := c.get(t.Context(), nil, true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force should bypass the cache, expected 2 fetches, got %d", calls)
	}
}

func TestReleaseCacheExpiresAfterTTL(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// Wind the stored timestamp back to just past expiry, simulating the TTL elapsing.
	c.at = time.Now().Add(-releaseTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("should fetch again after TTL expiry, expected 2, got %d", calls)
	}
}

func TestReleaseCacheUsesShorterTTLForErrors(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return nil, errors.New("github unreachable")
	})

	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("expected an error to be returned")
	}
	// A failure result is also cached briefly, otherwise every page load would wait
	// out a full timeout whenever GitHub is unreachable.
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("expected an error to be returned")
	}
	if calls != 1 {
		t.Errorf("errors should be cached briefly, expected 1 fetch, got %d", calls)
	}

	// But the error TTL must be clearly shorter than the success TTL, so it can
	// self-heal quickly once the network recovers.
	if releaseErrTTL >= releaseTTL {
		t.Fatalf("error TTL(%v) must be shorter than success TTL(%v)", releaseErrTTL, releaseTTL)
	}
	c.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("expected an error to be returned")
	}
	if calls != 2 {
		t.Errorf("should retry after the error TTL expires, expected 2, got %d", calls)
	}
}

func TestReleaseCacheDoesNotPoisonOnCallerCancel(t *testing.T) {
	good := &selfupdate.Release{TagName: "v0.3.8"}
	c := newTestCache(func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return good, nil
	})
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}

	// A visitor closing the tab cancels the request. That doesn't mean GitHub has a
	// problem, so a "cancelled" error must never be written into the cache —
	// otherwise every visitor for the next 30 minutes would get a baffling error.
	c.fetch = func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return nil, ctx.Err()
	}
	c.at = time.Now().Add(-releaseTTL - time.Second) // expire the cache to force a fetch

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, nil, false); err == nil {
		t.Fatal("should pass the error through to the caller when it already cancelled")
	}

	// Key invariant: the cancelled call leaves no trace — the cache has neither the
	// "cancelled" error nor lost its previous good result.
	if c.err != nil {
		t.Fatalf("a cancellation error must not be written to the cache, got %v", c.err)
	}
	if c.rel == nil || c.rel.TagName != "v0.3.8" {
		t.Fatalf("the cache should retain the previous good result, got %+v", c.rel)
	}

	// That cancellation yielded no new data, so the next visitor should fetch fresh —
	// and get a normal result, unaffected by the previous cancellation.
	c.fetch = func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return good, nil
	}
	rel, err := c.get(t.Context(), nil, false)
	if err != nil {
		t.Fatalf("a normal request after a cancellation should not error: %v", err)
	}
	if rel == nil || rel.TagName != "v0.3.8" {
		t.Fatalf("expected a normal result, got %+v", rel)
	}
}
