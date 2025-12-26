// Package registry provides service discovery with TTL-based expiration.
// Services register themselves and must send periodic heartbeats to stay active.
// The registry notifies subscribers of changes via watch channels.
package registry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// EventType represents the type of registry change event.
type EventType int

const (
	// EventServiceRegistered is emitted when a new service registers.
	EventServiceRegistered EventType = iota
	// EventServiceDeregistered is emitted when a service explicitly deregisters.
	EventServiceDeregistered
	// EventServiceExpired is emitted when a service TTL expires without heartbeat.
	EventServiceExpired
	// EventServiceUpdated is emitted when service metadata is updated.
	EventServiceUpdated
)

func (e EventType) String() string {
	switch e {
	case EventServiceRegistered:
		return "registered"
	case EventServiceDeregistered:
		return "deregistered"
	case EventServiceExpired:
		return "expired"
	case EventServiceUpdated:
		return "updated"
	default:
		return "unknown"
	}
}

// Event represents a change in the service registry.
type Event struct {
	Type      EventType
	Service   Service
	Timestamp time.Time
}

// Service represents a registered backend service.
type Service struct {
	// ID is the unique identifier for this service instance.
	// Auto-generated if not provided during registration.
	ID string `json:"id"`

	// Name is the logical service name (e.g., "api", "auth").
	// Multiple instances can share the same name.
	Name string `json:"name"`

	// Address is the host:port to connect to.
	Address string `json:"address"`

	// Tags are optional labels for filtering (e.g., "canary", "v2").
	Tags []string `json:"tags,omitempty"`

	// Metadata is arbitrary key-value data about the service.
	Metadata map[string]string `json:"metadata,omitempty"`

	// Weight is the load balancing weight (default 1).
	Weight int `json:"weight,omitempty"`

	// TTL is how long this registration lives without heartbeat.
	// Zero means use registry default.
	TTL time.Duration `json:"ttl,omitempty"`

	// RegisteredAt is when the service first registered.
	RegisteredAt time.Time `json:"registered_at"`

	// LastHeartbeat is the most recent heartbeat time.
	LastHeartbeat time.Time `json:"last_heartbeat"`
}

// IsExpired returns true if the service TTL has passed since last heartbeat.
func (s *Service) IsExpired(defaultTTL time.Duration) bool {
	ttl := s.TTL
	if ttl == 0 {
		ttl = defaultTTL
	}
	return time.Since(s.LastHeartbeat) > ttl
}

// Config holds registry configuration.
type Config struct {
	// DefaultTTL is the TTL used when services don't specify one.
	DefaultTTL time.Duration

	// CleanupInterval is how often to scan for expired services.
	CleanupInterval time.Duration

	// MaxServices is the maximum number of services allowed (0 = unlimited).
	MaxServices int
}

// DefaultConfig returns sensible defaults for the registry.
func DefaultConfig() Config {
	return Config{
		DefaultTTL:      30 * time.Second,
		CleanupInterval: 10 * time.Second,
		MaxServices:     0,
	}
}

// Registry is an in-memory service registry with TTL-based expiration.
// It is safe for concurrent use.
type Registry struct {
	config Config
	logger *slog.Logger

	mu       sync.RWMutex
	services map[string]*Service // keyed by service ID

	// subscribers receive change events
	subMu       sync.RWMutex
	subscribers map[chan Event]struct{}

	// lifecycle
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a new service registry.
func New(config Config, logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())

	r := &Registry{
		config:      config,
		logger:      logger,
		services:    make(map[string]*Service),
		subscribers: make(map[chan Event]struct{}),
		ctx:         ctx,
		cancel:      cancel,
	}

	// Start cleanup goroutine
	r.wg.Add(1)
	go r.cleanupLoop()

	return r
}

// Register adds a new service to the registry.
// If the service ID already exists, it updates the existing entry.
// Returns the service ID (generated if not provided).
func (r *Registry) Register(svc Service) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Generate ID if not provided
	if svc.ID == "" {
		svc.ID = generateID()
	}

	// Check max services limit
	if r.config.MaxServices > 0 && len(r.services) >= r.config.MaxServices {
		if _, exists := r.services[svc.ID]; !exists {
			return "", fmt.Errorf("registry full: max %d services", r.config.MaxServices)
		}
	}

	// Set defaults
	if svc.Weight <= 0 {
		svc.Weight = 1
	}

	now := time.Now()
	eventType := EventServiceRegistered

	// Check if this is an update
	if existing, exists := r.services[svc.ID]; exists {
		eventType = EventServiceUpdated
		svc.RegisteredAt = existing.RegisteredAt
	} else {
		svc.RegisteredAt = now
	}
	svc.LastHeartbeat = now

	r.services[svc.ID] = &svc

	r.logger.Info("service registered",
		"id", svc.ID,
		"name", svc.Name,
		"address", svc.Address,
		"ttl", svc.TTL,
	)

	// Notify subscribers (non-blocking)
	r.notify(Event{
		Type:      eventType,
		Service:   svc,
		Timestamp: now,
	})

	return svc.ID, nil
}

// Deregister removes a service from the registry.
// Returns true if the service was found and removed.
func (r *Registry) Deregister(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	svc, exists := r.services[id]
	if !exists {
		return false
	}

	delete(r.services, id)

	r.logger.Info("service deregistered",
		"id", svc.ID,
		"name", svc.Name,
		"address", svc.Address,
	)

	r.notify(Event{
		Type:      EventServiceDeregistered,
		Service:   *svc,
		Timestamp: time.Now(),
	})

	return true
}

// Heartbeat updates the last heartbeat time for a service.
// Returns false if the service is not found.
func (r *Registry) Heartbeat(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	svc, exists := r.services[id]
	if !exists {
		return false
	}

	svc.LastHeartbeat = time.Now()
	return true
}

// Get returns a service by ID.
// Returns nil if not found.
func (r *Registry) Get(id string) *Service {
	r.mu.RLock()
	defer r.mu.RUnlock()

	svc, exists := r.services[id]
	if !exists {
		return nil
	}

	// Return a copy to prevent mutation
	copy := *svc
	return &copy
}

// GetByName returns all services with the given name.
func (r *Registry) GetByName(name string) []Service {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []Service
	for _, svc := range r.services {
		if svc.Name == name {
			result = append(result, *svc)
		}
	}
	return result
}

// GetByTag returns all services with the given tag.
func (r *Registry) GetByTag(tag string) []Service {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []Service
	for _, svc := range r.services {
		for _, t := range svc.Tags {
			if t == tag {
				result = append(result, *svc)
				break
			}
		}
	}
	return result
}

// List returns all registered services.
func (r *Registry) List() []Service {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]Service, 0, len(r.services))
	for _, svc := range r.services {
		result = append(result, *svc)
	}
	return result
}

// Count returns the number of registered services.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.services)
}

// Subscribe returns a channel that receives registry change events.
// The channel has a buffer of 16 events; slow subscribers will miss events.
// Call Unsubscribe to clean up when done.
func (r *Registry) Subscribe() chan Event {
	ch := make(chan Event, 16)

	r.subMu.Lock()
	r.subscribers[ch] = struct{}{}
	r.subMu.Unlock()

	r.logger.Debug("subscriber added", "total", len(r.subscribers))
	return ch
}

// Unsubscribe removes a subscriber channel.
func (r *Registry) Unsubscribe(ch chan Event) {
	r.subMu.Lock()
	delete(r.subscribers, ch)
	r.subMu.Unlock()
	close(ch)

	r.logger.Debug("subscriber removed")
}

// Stop shuts down the registry, stopping the cleanup goroutine.
func (r *Registry) Stop() {
	r.cancel()
	r.wg.Wait()

	// Close all subscriber channels
	r.subMu.Lock()
	for ch := range r.subscribers {
		close(ch)
	}
	r.subscribers = make(map[chan Event]struct{})
	r.subMu.Unlock()

	r.logger.Info("registry stopped")
}

// notify sends an event to all subscribers (non-blocking).
func (r *Registry) notify(event Event) {
	r.subMu.RLock()
	defer r.subMu.RUnlock()

	for ch := range r.subscribers {
		select {
		case ch <- event:
		default:
			// Subscriber is slow, drop the event
			r.logger.Warn("dropping event for slow subscriber",
				"event", event.Type.String(),
				"service", event.Service.ID,
			)
		}
	}
}

// cleanupLoop periodically removes expired services.
func (r *Registry) cleanupLoop() {
	defer r.wg.Done()

	ticker := time.NewTicker(r.config.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.cleanupExpired()
		}
	}
}

// cleanupExpired removes all services that have exceeded their TTL.
func (r *Registry) cleanupExpired() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	var expired []Service

	for id, svc := range r.services {
		if svc.IsExpired(r.config.DefaultTTL) {
			expired = append(expired, *svc)
			delete(r.services, id)
		}
	}

	// Notify outside the loop to avoid holding lock during notification
	for _, svc := range expired {
		r.logger.Info("service expired",
			"id", svc.ID,
			"name", svc.Name,
			"address", svc.Address,
			"last_heartbeat", svc.LastHeartbeat,
		)

		r.notify(Event{
			Type:      EventServiceExpired,
			Service:   svc,
			Timestamp: now,
		})
	}
}

// generateID creates a random 8-character hex ID.
func generateID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}
