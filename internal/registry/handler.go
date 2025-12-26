package registry

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Handler provides HTTP endpoints for the service registry.
type Handler struct {
	registry *Registry
	logger   *slog.Logger
}

// NewHandler creates a new HTTP handler for the registry.
func NewHandler(registry *Registry, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		registry: registry,
		logger:   logger,
	}
}

// RegisterRequest is the JSON body for service registration.
type RegisterRequest struct {
	ID       string            `json:"id,omitempty"`
	Name     string            `json:"name"`
	Address  string            `json:"address"`
	Tags     []string          `json:"tags,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Weight   int               `json:"weight,omitempty"`
	TTL      string            `json:"ttl,omitempty"` // duration string like "30s"
}

// RegisterResponse is returned after successful registration.
type RegisterResponse struct {
	ID string `json:"id"`
}

// ServiceResponse is the JSON representation of a service.
type ServiceResponse struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Address       string            `json:"address"`
	Tags          []string          `json:"tags,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	Weight        int               `json:"weight"`
	TTL           string            `json:"ttl"`
	RegisteredAt  time.Time         `json:"registered_at"`
	LastHeartbeat time.Time         `json:"last_heartbeat"`
}

// ErrorResponse is returned on errors.
type ErrorResponse struct {
	Error string `json:"error"`
}

// ServeHTTP implements http.Handler.
// Routes:
//
//	POST   /v1/services           - Register a service
//	GET    /v1/services           - List all services
//	GET    /v1/services/:id       - Get a service by ID
//	DELETE /v1/services/:id       - Deregister a service
//	PUT    /v1/services/:id/heartbeat - Send heartbeat
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Strip prefix if present
	path := strings.TrimPrefix(r.URL.Path, "/v1/services")
	if path == "" {
		path = "/"
	}

	switch {
	case path == "/" || path == "":
		switch r.Method {
		case http.MethodPost:
			h.handleRegister(w, r)
		case http.MethodGet:
			h.handleList(w, r)
		default:
			h.methodNotAllowed(w, "GET, POST")
		}

	case strings.HasSuffix(path, "/heartbeat"):
		// PUT /v1/services/:id/heartbeat
		if r.Method != http.MethodPut {
			h.methodNotAllowed(w, "PUT")
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/heartbeat")
		h.handleHeartbeat(w, r, id)

	default:
		// GET or DELETE /v1/services/:id
		id := strings.TrimPrefix(path, "/")
		switch r.Method {
		case http.MethodGet:
			h.handleGet(w, r, id)
		case http.MethodDelete:
			h.handleDeregister(w, r, id)
		default:
			h.methodNotAllowed(w, "GET, DELETE")
		}
	}
}

// handleRegister handles POST /v1/services
func (h *Handler) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// Validate required fields
	if req.Name == "" {
		h.writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Address == "" {
		h.writeError(w, http.StatusBadRequest, "address is required")
		return
	}

	// Parse TTL if provided
	var ttl time.Duration
	if req.TTL != "" {
		var err error
		ttl, err = time.ParseDuration(req.TTL)
		if err != nil {
			h.writeError(w, http.StatusBadRequest, "invalid TTL: "+err.Error())
			return
		}
	}

	svc := Service{
		ID:       req.ID,
		Name:     req.Name,
		Address:  req.Address,
		Tags:     req.Tags,
		Metadata: req.Metadata,
		Weight:   req.Weight,
		TTL:      ttl,
	}

	id, err := h.registry.Register(svc)
	if err != nil {
		h.writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	h.writeJSON(w, http.StatusCreated, RegisterResponse{ID: id})
}

// handleList handles GET /v1/services
func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	// Optional filters
	name := r.URL.Query().Get("name")
	tag := r.URL.Query().Get("tag")

	var services []Service
	switch {
	case name != "":
		services = h.registry.GetByName(name)
	case tag != "":
		services = h.registry.GetByTag(tag)
	default:
		services = h.registry.List()
	}

	// Convert to response format
	response := make([]ServiceResponse, len(services))
	for i, svc := range services {
		response[i] = serviceToResponse(svc)
	}

	h.writeJSON(w, http.StatusOK, response)
}

// handleGet handles GET /v1/services/:id
func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request, id string) {
	svc := h.registry.Get(id)
	if svc == nil {
		h.writeError(w, http.StatusNotFound, "service not found")
		return
	}

	h.writeJSON(w, http.StatusOK, serviceToResponse(*svc))
}

// handleDeregister handles DELETE /v1/services/:id
func (h *Handler) handleDeregister(w http.ResponseWriter, r *http.Request, id string) {
	if !h.registry.Deregister(id) {
		h.writeError(w, http.StatusNotFound, "service not found")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleHeartbeat handles PUT /v1/services/:id/heartbeat
func (h *Handler) handleHeartbeat(w http.ResponseWriter, r *http.Request, id string) {
	if !h.registry.Heartbeat(id) {
		h.writeError(w, http.StatusNotFound, "service not found")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// writeJSON writes a JSON response.
func (h *Handler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError writes an error response.
func (h *Handler) writeError(w http.ResponseWriter, status int, message string) {
	h.writeJSON(w, status, ErrorResponse{Error: message})
}

// methodNotAllowed writes a 405 response.
func (h *Handler) methodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	h.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

// serviceToResponse converts a Service to its JSON response format.
func serviceToResponse(svc Service) ServiceResponse {
	ttl := svc.TTL.String()
	if svc.TTL == 0 {
		ttl = "default"
	}

	return ServiceResponse{
		ID:            svc.ID,
		Name:          svc.Name,
		Address:       svc.Address,
		Tags:          svc.Tags,
		Metadata:      svc.Metadata,
		Weight:        svc.Weight,
		TTL:           ttl,
		RegisteredAt:  svc.RegisteredAt,
		LastHeartbeat: svc.LastHeartbeat,
	}
}
