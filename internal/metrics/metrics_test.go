// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package metrics

import (
	"reflect"
	"testing"
)

func TestCollector(t *testing.T) {
	c := New(100)

	// Since everything is a no-op, we just verify they don't panic or do anything weird
	c.SetStart(12345)
	c.StartRequest()
	c.EndRequest(true, 1.23)
	c.IncUpstream429()
	c.IncUpstreamEmpty()
	c.IncUpstreamAuth()
	c.RecordRequest("/test", true, 0.5, "now")
	c.Reset()

	snap := c.Snapshot()
	expectedSnap := Snapshot{} //nolint:exhaustruct
	if !reflect.DeepEqual(snap, expectedSnap) {
		t.Errorf("Snapshot() = %v, want %v", snap, expectedSnap)
	}

	recent := c.RecentRequests()
	if recent != nil {
		t.Errorf("RecentRequests() = %v, want nil", recent)
	}
}

func TestDefault(t *testing.T) {
	if Default == nil {
		t.Fatal("Default collector should not be nil")
	}

	// Just a smoke test
	Default.StartRequest()
	Default.EndRequest(true, 0.1)
}
