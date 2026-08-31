package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"claracore-server/internal/config"
	externaldomain "claracore-server/internal/domain/external"
	"claracore-server/internal/platform"
)

type fakeAtlasSyncService struct {
	requestInput externaldomain.RequestSyncInput
	requestOK    bool
	requestErr   error
	claim        externaldomain.SyncRequestClaim
	claimOK      bool
	claimErr     error
	syncErr      error
	syncs        int
	resolved     int
	retry        bool
	resolveCh    chan struct{}
}

type fakeAtlasSource struct{}

func (fakeAtlasSource) ListDocuments(context.Context, int, int) (externaldomain.Page, error) {
	return externaldomain.Page{}, nil
}

func (fakeAtlasSource) GetDocument(context.Context, string, string) (externaldomain.Document, error) {
	return externaldomain.Document{}, nil
}

func (f *fakeAtlasSyncService) RequestSync(_ context.Context, in externaldomain.RequestSyncInput) (bool, error) {
	f.requestInput = in
	return f.requestOK, f.requestErr
}

func (f *fakeAtlasSyncService) Sync(context.Context, externaldomain.SyncInput) (externaldomain.SyncReport, error) {
	f.syncs++
	return externaldomain.SyncReport{}, f.syncErr
}

func (f *fakeAtlasSyncService) ClaimSyncRequest(context.Context, platform.ID, string, time.Duration) (externaldomain.SyncRequestClaim, bool, error) {
	return f.claim, f.claimOK, f.claimErr
}

func (f *fakeAtlasSyncService) ResolveSyncRequest(_ context.Context, _ externaldomain.SyncRequestClaim, retry bool) error {
	f.resolved++
	f.retry = retry
	if f.resolveCh != nil {
		select {
		case f.resolveCh <- struct{}{}:
		default:
		}
	}
	return nil
}

func TestNotifyExternalSyncIsBounded(t *testing.T) {
	queue := make(chan struct{}, 1)
	notifyExternalSync(queue)
	notifyExternalSync(queue)
	if len(queue) != 1 {
		t.Fatalf("local wake-up hints must coalesce: len=%d", len(queue))
	}
}

func TestQueueConfiguredAtlasSyncPersistsBeforeWakeAndChecksTenant(t *testing.T) {
	svc := &fakeAtlasSyncService{requestOK: true}
	wake := make(chan struct{}, 1)
	cfg := config.AtlasExternal{SpaceID: "space", Name: "atlas-docs", System: "shared-docs"}
	accepted, err := queueConfiguredAtlasSync(context.Background(), svc, wake, "tenant", "tenant", cfg)
	if err != nil || !accepted || len(wake) != 1 || svc.requestInput.TenantID != "tenant" || svc.requestInput.SpaceID != "space" || svc.requestInput.Name != cfg.Name {
		t.Fatalf("configured request was not durably queued before wake: accepted=%v input=%+v wake=%d err=%v", accepted, svc.requestInput, len(wake), err)
	}
	foreign := &fakeAtlasSyncService{requestOK: true}
	if accepted, err = queueConfiguredAtlasSync(context.Background(), foreign, make(chan struct{}, 1), "tenant", "other", cfg); err == nil || accepted || foreign.requestInput.TenantID != "" {
		t.Fatalf("foreign tenant must be rejected before the domain call: accepted=%v input=%+v err=%v", accepted, foreign.requestInput, err)
	}
}

func TestRunClaimedAtlasSyncRetriesOnlyUnattemptedWork(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.AtlasExternal{Name: "atlas-docs", SpaceID: "space", System: "shared-docs", SyncTimeout: 10 * time.Minute, PageSize: 20}
	claim := externaldomain.SyncRequestClaim{RequestID: "request", TenantID: "tenant", ExternalSourceID: "source", ClaimToken: "claim"}

	busy := &fakeAtlasSyncService{claim: claim, claimOK: true, syncErr: externaldomain.ErrSyncBusy}
	if !runClaimedAtlasSync(context.Background(), busy, nil, "tenant", cfg, logger) || busy.syncs != 1 || busy.resolved != 1 || !busy.retry {
		t.Fatalf("busy lease must release the durable request for retry: %+v", busy)
	}

	failed := &fakeAtlasSyncService{claim: claim, claimOK: true, syncErr: errors.New("upstream unavailable")}
	if !runClaimedAtlasSync(context.Background(), failed, nil, "tenant", cfg, logger) || failed.syncs != 1 || failed.resolved != 1 || failed.retry {
		t.Fatalf("an attempted upstream failure must complete the request and rely on its failure receipt: %+v", failed)
	}
}

func TestRunAtlasSyncConsumesDurableRequestBeforeStartupRefresh(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	claim := externaldomain.SyncRequestClaim{RequestID: "request", TenantID: "tenant", ExternalSourceID: "source", ClaimToken: "claim"}
	svc := &fakeAtlasSyncService{claim: claim, claimOK: true, resolveCh: make(chan struct{}, 1)}
	cfg := config.AtlasExternal{
		Endpoint: "https://atlas.example/mcp", Name: "atlas-docs", System: "shared-docs", SpaceID: "space",
		SyncInterval: time.Hour, SyncTimeout: 10 * time.Minute, PageSize: 20,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go runAtlasSync(ctx, svc, fakeAtlasSource{}, "tenant", cfg, make(chan struct{}, 1), logger, done)
	select {
	case <-svc.resolveCh:
	case <-time.After(time.Second):
		t.Fatal("external worker did not resolve the startup request")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("external worker did not stop")
	}
	if svc.syncs != 1 || svc.resolved != 1 {
		t.Fatalf("startup must consume the durable request without an extra scheduled refresh: syncs=%d resolved=%d", svc.syncs, svc.resolved)
	}
}
