package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestRegistry_Register(t *testing.T) {
	logger := slog.Default()
	config := Config{
		DefaultTTL:      30 * time.Second,
		CleanupInterval: 1 * time.Hour, // long interval to avoid interference
	}

	r := New(config, logger)
	defer r.Stop()

	// Register a service
	svc := Service{
		Name:    "api",
		Address: "localhost:8080",
		Tags:    []string{"v1", "production"},
		Weight:  10,
	}

	id, err := r.Register(svc)
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	if id == "" {
		t.Error("expected generated ID, got empty string")
	}

	// Verify service is stored
	got := r.Get(id)
	if got == nil {
		t.Fatal("Get returned nil for registered service")
	}

	if got.Name != "api" {
		t.Errorf("Name = %q, want %q", got.Name, "api")
	}
	if got.Address != "localhost:8080" {
		t.Errorf("Address = %q, want %q", got.Address, "localhost:8080")
	}
	if got.Weight != 10 {
		t.Errorf("Weight = %d, want %d", got.Weight, 10)
	}
	if len(got.Tags) != 2 {
		t.Errorf("Tags = %v, want 2 tags", got.Tags)
	}
}

func TestRegistry_RegisterWithCustomID(t *testing.T) {
	r := New(DefaultConfig(), nil)
	defer r.Stop()

	svc := Service{
		ID:      "my-custom-id",
		Name:    "api",
		Address: "localhost:8080",
	}

	id, err := r.Register(svc)
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	if id != "my-custom-id" {
		t.Errorf("ID = %q, want %q", id, "my-custom-id")
	}
}

func TestRegistry_RegisterUpdate(t *testing.T) {
	r := New(DefaultConfig(), nil)
	defer r.Stop()

	// Register first version
	svc1 := Service{
		ID:      "svc-1",
		Name:    "api",
		Address: "localhost:8080",
		Weight:  1,
	}
	r.Register(svc1)

	// Update with same ID
	svc2 := Service{
		ID:      "svc-1",
		Name:    "api",
		Address: "localhost:9090", // different address
		Weight:  5,                // different weight
	}
	r.Register(svc2)

	// Should have only 1 service
	if r.Count() != 1 {
		t.Errorf("Count = %d, want 1", r.Count())
	}

	// Should have updated values
	got := r.Get("svc-1")
	if got.Address != "localhost:9090" {
		t.Errorf("Address = %q, want %q", got.Address, "localhost:9090")
	}
	if got.Weight != 5 {
		t.Errorf("Weight = %d, want %d", got.Weight, 5)
	}
}

func TestRegistry_Deregister(t *testing.T) {
	r := New(DefaultConfig(), nil)
	defer r.Stop()

	id, _ := r.Register(Service{Name: "api", Address: "localhost:8080"})

	// Deregister
	if !r.Deregister(id) {
		t.Error("Deregister returned false for existing service")
	}

	// Should be gone
	if r.Get(id) != nil {
		t.Error("Get returned non-nil after deregister")
	}

	// Deregister again should return false
	if r.Deregister(id) {
		t.Error("Deregister returned true for non-existent service")
	}
}

func TestRegistry_Heartbeat(t *testing.T) {
	r := New(DefaultConfig(), nil)
	defer r.Stop()

	id, _ := r.Register(Service{Name: "api", Address: "localhost:8080"})

	initial := r.Get(id).LastHeartbeat

	// Wait a bit and heartbeat
	time.Sleep(10 * time.Millisecond)

	if !r.Heartbeat(id) {
		t.Error("Heartbeat returned false for existing service")
	}

	updated := r.Get(id).LastHeartbeat
	if !updated.After(initial) {
		t.Error("LastHeartbeat was not updated")
	}

	// Heartbeat non-existent
	if r.Heartbeat("non-existent") {
		t.Error("Heartbeat returned true for non-existent service")
	}
}

func TestRegistry_GetByName(t *testing.T) {
	r := New(DefaultConfig(), nil)
	defer r.Stop()

	r.Register(Service{Name: "api", Address: "localhost:8080"})
	r.Register(Service{Name: "api", Address: "localhost:8081"})
	r.Register(Service{Name: "auth", Address: "localhost:9090"})

	apis := r.GetByName("api")
	if len(apis) != 2 {
		t.Errorf("GetByName('api') returned %d services, want 2", len(apis))
	}

	auths := r.GetByName("auth")
	if len(auths) != 1 {
		t.Errorf("GetByName('auth') returned %d services, want 1", len(auths))
	}

	none := r.GetByName("nonexistent")
	if len(none) != 0 {
		t.Errorf("GetByName('nonexistent') returned %d services, want 0", len(none))
	}
}

func TestRegistry_GetByTag(t *testing.T) {
	r := New(DefaultConfig(), nil)
	defer r.Stop()

	r.Register(Service{Name: "api", Address: "localhost:8080", Tags: []string{"canary", "v2"}})
	r.Register(Service{Name: "api", Address: "localhost:8081", Tags: []string{"stable", "v1"}})
	r.Register(Service{Name: "api", Address: "localhost:8082", Tags: []string{"canary", "v2"}})

	canary := r.GetByTag("canary")
	if len(canary) != 2 {
		t.Errorf("GetByTag('canary') returned %d services, want 2", len(canary))
	}

	stable := r.GetByTag("stable")
	if len(stable) != 1 {
		t.Errorf("GetByTag('stable') returned %d services, want 1", len(stable))
	}
}

func TestRegistry_List(t *testing.T) {
	r := New(DefaultConfig(), nil)
	defer r.Stop()

	r.Register(Service{Name: "api", Address: "localhost:8080"})
	r.Register(Service{Name: "auth", Address: "localhost:9090"})

	list := r.List()
	if len(list) != 2 {
		t.Errorf("List returned %d services, want 2", len(list))
	}
}

func TestRegistry_MaxServices(t *testing.T) {
	config := Config{
		DefaultTTL:      30 * time.Second,
		CleanupInterval: 1 * time.Hour,
		MaxServices:     2,
	}
	r := New(config, nil)
	defer r.Stop()

	r.Register(Service{Name: "api1", Address: "localhost:8080"})
	r.Register(Service{Name: "api2", Address: "localhost:8081"})

	// Third should fail
	_, err := r.Register(Service{Name: "api3", Address: "localhost:8082"})
	if err == nil {
		t.Error("expected error when exceeding MaxServices, got nil")
	}

	// Updating existing should work
	_, err = r.Register(Service{ID: r.List()[0].ID, Name: "api1-updated", Address: "localhost:8080"})
	if err != nil {
		t.Errorf("update existing should work, got error: %v", err)
	}
}

func TestRegistry_TTLExpiration(t *testing.T) {
	config := Config{
		DefaultTTL:      50 * time.Millisecond,
		CleanupInterval: 20 * time.Millisecond,
	}
	r := New(config, nil)
	defer r.Stop()

	id, _ := r.Register(Service{Name: "api", Address: "localhost:8080"})

	// Should exist initially
	if r.Get(id) == nil {
		t.Fatal("service should exist immediately after registration")
	}

	// Wait for expiration + cleanup
	time.Sleep(100 * time.Millisecond)

	// Should be gone
	if r.Get(id) != nil {
		t.Error("service should have expired")
	}
}

func TestRegistry_TTLExpirationWithHeartbeat(t *testing.T) {
	config := Config{
		DefaultTTL:      50 * time.Millisecond,
		CleanupInterval: 20 * time.Millisecond,
	}
	r := New(config, nil)
	defer r.Stop()

	id, _ := r.Register(Service{Name: "api", Address: "localhost:8080"})

	// Keep alive with heartbeats
	for i := 0; i < 5; i++ {
		time.Sleep(30 * time.Millisecond)
		r.Heartbeat(id)
	}

	// Should still exist
	if r.Get(id) == nil {
		t.Error("service should still exist with heartbeats")
	}

	// Stop heartbeating and wait for expiration
	time.Sleep(100 * time.Millisecond)

	// Now should be gone
	if r.Get(id) != nil {
		t.Error("service should have expired after heartbeats stopped")
	}
}

func TestRegistry_Subscribe(t *testing.T) {
	r := New(DefaultConfig(), nil)
	defer r.Stop()

	ch := r.Subscribe()
	defer r.Unsubscribe(ch)

	// Register should trigger event
	id, _ := r.Register(Service{Name: "api", Address: "localhost:8080"})

	select {
	case event := <-ch:
		if event.Type != EventServiceRegistered {
			t.Errorf("event.Type = %v, want EventServiceRegistered", event.Type)
		}
		if event.Service.ID != id {
			t.Errorf("event.Service.ID = %q, want %q", event.Service.ID, id)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("timeout waiting for register event")
	}

	// Deregister should trigger event
	r.Deregister(id)

	select {
	case event := <-ch:
		if event.Type != EventServiceDeregistered {
			t.Errorf("event.Type = %v, want EventServiceDeregistered", event.Type)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("timeout waiting for deregister event")
	}
}

func TestRegistry_SubscribeExpiration(t *testing.T) {
	config := Config{
		DefaultTTL:      30 * time.Millisecond,
		CleanupInterval: 10 * time.Millisecond,
	}
	r := New(config, nil)
	defer r.Stop()

	ch := r.Subscribe()
	defer r.Unsubscribe(ch)

	r.Register(Service{Name: "api", Address: "localhost:8080"})

	// Drain the register event
	<-ch

	// Wait for expiration
	select {
	case event := <-ch:
		if event.Type != EventServiceExpired {
			t.Errorf("event.Type = %v, want EventServiceExpired", event.Type)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("timeout waiting for expiration event")
	}
}

func TestRegistry_ConcurrentAccess(t *testing.T) {
	r := New(DefaultConfig(), nil)
	defer r.Stop()

	var wg sync.WaitGroup
	const numGoroutines = 100
	const numOps = 100

	// Concurrent registrations
	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func(n int) {
			defer wg.Done()
			for j := 0; j < numOps; j++ {
				r.Register(Service{
					Name:    "api",
					Address: "localhost:8080",
				})
			}
		}(i)
	}

	// Concurrent reads
	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < numOps; j++ {
				r.List()
				r.GetByName("api")
				r.Count()
			}
		}()
	}

	wg.Wait()

	// Should not panic or deadlock
	t.Logf("Final count: %d", r.Count())
}

// Handler tests

func TestHandler_Register(t *testing.T) {
	r := New(DefaultConfig(), nil)
	defer r.Stop()
	h := NewHandler(r, nil)

	body := `{"name": "api", "address": "localhost:8080", "tags": ["v1"], "weight": 5, "ttl": "60s"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/services", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusCreated)
	}

	var resp RegisterResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.ID == "" {
		t.Error("expected ID in response")
	}

	// Verify in registry
	svc := r.Get(resp.ID)
	if svc == nil {
		t.Fatal("service not found in registry")
	}
	if svc.Weight != 5 {
		t.Errorf("Weight = %d, want 5", svc.Weight)
	}
	if svc.TTL != 60*time.Second {
		t.Errorf("TTL = %v, want 60s", svc.TTL)
	}
}

func TestHandler_RegisterValidation(t *testing.T) {
	r := New(DefaultConfig(), nil)
	defer r.Stop()
	h := NewHandler(r, nil)

	tests := []struct {
		name string
		body string
		want int
	}{
		{"missing name", `{"address": "localhost:8080"}`, http.StatusBadRequest},
		{"missing address", `{"name": "api"}`, http.StatusBadRequest},
		{"invalid JSON", `{invalid}`, http.StatusBadRequest},
		{"invalid TTL", `{"name": "api", "address": "localhost:8080", "ttl": "invalid"}`, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/services", bytes.NewBufferString(tt.body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestHandler_List(t *testing.T) {
	r := New(DefaultConfig(), nil)
	defer r.Stop()
	h := NewHandler(r, nil)

	r.Register(Service{Name: "api", Address: "localhost:8080", Tags: []string{"v1"}})
	r.Register(Service{Name: "api", Address: "localhost:8081", Tags: []string{"v2"}})
	r.Register(Service{Name: "auth", Address: "localhost:9090"})

	tests := []struct {
		name  string
		query string
		want  int
	}{
		{"list all", "", 3},
		{"filter by name", "?name=api", 2},
		{"filter by tag", "?tag=v1", 1},
		{"no matches", "?name=nonexistent", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/services"+tt.query, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
			}

			var services []ServiceResponse
			json.NewDecoder(rec.Body).Decode(&services)
			if len(services) != tt.want {
				t.Errorf("got %d services, want %d", len(services), tt.want)
			}
		})
	}
}

func TestHandler_Get(t *testing.T) {
	r := New(DefaultConfig(), nil)
	defer r.Stop()
	h := NewHandler(r, nil)

	id, _ := r.Register(Service{Name: "api", Address: "localhost:8080"})

	// Get existing
	req := httptest.NewRequest(http.MethodGet, "/v1/services/"+id, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var svc ServiceResponse
	json.NewDecoder(rec.Body).Decode(&svc)
	if svc.ID != id {
		t.Errorf("ID = %q, want %q", svc.ID, id)
	}

	// Get non-existent
	req = httptest.NewRequest(http.MethodGet, "/v1/services/nonexistent", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandler_Deregister(t *testing.T) {
	r := New(DefaultConfig(), nil)
	defer r.Stop()
	h := NewHandler(r, nil)

	id, _ := r.Register(Service{Name: "api", Address: "localhost:8080"})

	// Delete existing
	req := httptest.NewRequest(http.MethodDelete, "/v1/services/"+id, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}

	// Should be gone
	if r.Get(id) != nil {
		t.Error("service should be deregistered")
	}

	// Delete non-existent
	req = httptest.NewRequest(http.MethodDelete, "/v1/services/"+id, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandler_Heartbeat(t *testing.T) {
	r := New(DefaultConfig(), nil)
	defer r.Stop()
	h := NewHandler(r, nil)

	id, _ := r.Register(Service{Name: "api", Address: "localhost:8080"})
	initial := r.Get(id).LastHeartbeat

	time.Sleep(10 * time.Millisecond)

	// Send heartbeat
	req := httptest.NewRequest(http.MethodPut, "/v1/services/"+id+"/heartbeat", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}

	updated := r.Get(id).LastHeartbeat
	if !updated.After(initial) {
		t.Error("LastHeartbeat was not updated")
	}

	// Heartbeat non-existent
	req = httptest.NewRequest(http.MethodPut, "/v1/services/nonexistent/heartbeat", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandler_MethodNotAllowed(t *testing.T) {
	r := New(DefaultConfig(), nil)
	defer r.Stop()
	h := NewHandler(r, nil)

	tests := []struct {
		method string
		path   string
	}{
		{http.MethodPut, "/v1/services"},
		{http.MethodPatch, "/v1/services"},
		{http.MethodPost, "/v1/services/someid"},
		{http.MethodGet, "/v1/services/someid/heartbeat"},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
			}
		})
	}
}

// Benchmark

func BenchmarkRegistry_Register(b *testing.B) {
	r := New(DefaultConfig(), nil)
	defer r.Stop()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Register(Service{Name: "api", Address: "localhost:8080"})
	}
}

func BenchmarkRegistry_Get(b *testing.B) {
	r := New(DefaultConfig(), nil)
	defer r.Stop()

	id, _ := r.Register(Service{Name: "api", Address: "localhost:8080"})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Get(id)
	}
}

func BenchmarkRegistry_List(b *testing.B) {
	r := New(DefaultConfig(), nil)
	defer r.Stop()

	// Pre-populate with 100 services
	for i := 0; i < 100; i++ {
		r.Register(Service{Name: "api", Address: "localhost:8080"})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.List()
	}
}

// Integration test: full lifecycle
func TestRegistry_FullLifecycle(t *testing.T) {
	config := Config{
		DefaultTTL:      100 * time.Millisecond,
		CleanupInterval: 20 * time.Millisecond,
	}
	r := New(config, nil)
	defer r.Stop()
	h := NewHandler(r, nil)

	// Subscribe to events
	events := r.Subscribe()
	defer r.Unsubscribe(events)

	// 1. Register via API
	body := `{"name": "api", "address": "localhost:8080"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/services", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("register failed: %d", rec.Code)
	}

	var resp RegisterResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	id := resp.ID

	// Check registered event
	event := <-events
	if event.Type != EventServiceRegistered {
		t.Errorf("expected registered event, got %v", event.Type)
	}

	// 2. Heartbeat to keep alive
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(30 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				req := httptest.NewRequest(http.MethodPut, "/v1/services/"+id+"/heartbeat", nil)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
			}
		}
	}()

	// Service should stay alive
	time.Sleep(150 * time.Millisecond)
	if r.Get(id) == nil {
		t.Error("service should still be alive with heartbeats")
	}

	// 3. Stop heartbeats
	cancel()

	// 4. Wait for expiration
	time.Sleep(150 * time.Millisecond)

	// Check expired event
	select {
	case event := <-events:
		if event.Type != EventServiceExpired {
			t.Errorf("expected expired event, got %v", event.Type)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("timeout waiting for expired event")
	}

	// Service should be gone
	if r.Get(id) != nil {
		t.Error("service should have expired")
	}
}
