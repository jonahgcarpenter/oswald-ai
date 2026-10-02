package config

import (
	"testing"
	"time"
)

func TestDefaultRetentionPolicy(t *testing.T) {
	policy := DefaultRetentionPolicy()
	if policy.SessionInactivity != 24*time.Hour || policy.PendingDeliveryTimeout != 15*time.Minute || policy.MaintenanceInterval != time.Hour || policy.BatchSize != 100 {
		t.Fatal("unexpected retention bounds")
	}
}
