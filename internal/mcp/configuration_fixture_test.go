package mcp

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"
)

type staticResolver map[string][]string

func (r staticResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	return r[host], nil
}

// Synthetic configuration supports catalog tests without database state or DNS.
type testConfiguration struct {
	servers []ServerConfig
	closed  bool
}

func testStore(t *testing.T) *testConfiguration                    { t.Helper(); return &testConfiguration{} }
func addTestUsers(t *testing.T, _ *testConfiguration, _ ...string) { t.Helper() }
func (s *testConfiguration) Save(ctx context.Context, cfg ServerConfig) (ServerConfig, error) {
	if err := ctx.Err(); err != nil {
		return ServerConfig{}, err
	}
	cfg.ID = rand.Text()
	s.servers = append(s.servers, cfg)
	return cfg, nil
}
func (s *testConfiguration) ListForUser(ctx context.Context, owner string) ([]ServerConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.closed {
		return nil, errors.New("synthetic configuration failure")
	}
	var entries []ServerConfig
	for _, entry := range s.servers {
		if entry.Scope == ScopeGlobal || entry.OwnerUserID == owner {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}
func (s *testConfiguration) Get(ctx context.Context, scope, owner, name string) (ServerConfig, bool, error) {
	entries, err := s.ListForUser(ctx, owner)
	if err != nil {
		return ServerConfig{}, false, err
	}
	for _, entry := range entries {
		if entry.Scope == scope && entry.OwnerUserID == owner && entry.Name == name {
			return entry, true, nil
		}
	}
	return ServerConfig{}, false, nil
}
