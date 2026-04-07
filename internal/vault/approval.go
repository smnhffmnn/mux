package vault

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// ApprovalState tracks the lifecycle of an approval request.
type ApprovalState string

const (
	ApprovalPending ApprovalState = "pending"
	ApprovalGranted ApprovalState = "granted"
	ApprovalDenied  ApprovalState = "denied"
	ApprovalExpired ApprovalState = "expired"
)

const (
	DefaultApprovalTTL = 10 * time.Minute
	approvalIDBytes    = 16
)

// ApprovalRequest represents a pending action that needs human authorization.
type ApprovalRequest struct {
	ID        string        `json:"id"`
	Action    string        `json:"action"`    // e.g. "git push origin main"
	Context   string        `json:"context"`   // e.g. "nas-erp" (repo or session)
	Requester string        `json:"requester"` // e.g. "claude-code-session-1"
	State     ApprovalState `json:"state"`
	CreatedAt time.Time     `json:"created_at"`
	ExpiresAt time.Time     `json:"expires_at"`
	DecidedAt *time.Time    `json:"decided_at,omitempty"`
}

// IsExpired reports whether the request has exceeded its TTL.
func (r *ApprovalRequest) IsExpired() bool {
	return time.Now().After(r.ExpiresAt)
}

// EffectiveState returns the state, accounting for expiration.
func (r *ApprovalRequest) EffectiveState() ApprovalState {
	if r.State == ApprovalPending && r.IsExpired() {
		return ApprovalExpired
	}
	return r.State
}

// ApprovalQueue manages pending approval requests.
// This is the transport-agnostic core — both browser-based WebAuthn
// and future native iOS flows use the same queue.
type ApprovalQueue struct {
	mu       sync.RWMutex
	requests map[string]*ApprovalRequest
	notifier Notifier // abstracted: Discord webhook now, APNs later
	baseURL  string   // e.g. "https://mux.example.com:7701"
	done     chan struct{}
}

// NewApprovalQueue creates a queue with the given notifier.
func NewApprovalQueue(notifier Notifier, baseURL string) *ApprovalQueue {
	q := &ApprovalQueue{
		requests: make(map[string]*ApprovalRequest),
		notifier: notifier,
		baseURL:  baseURL,
		done:     make(chan struct{}),
	}
	go q.cleanupLoop()
	return q
}

// Close stops the background cleanup goroutine.
func (q *ApprovalQueue) Close() {
	close(q.done)
}

// Create adds a new approval request and notifies the user.
func (q *ApprovalQueue) Create(action, context, requester string) (*ApprovalRequest, error) {
	id, err := generateApprovalID()
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	req := &ApprovalRequest{
		ID:        id,
		Action:    action,
		Context:   context,
		Requester: requester,
		State:     ApprovalPending,
		CreatedAt: now,
		ExpiresAt: now.Add(DefaultApprovalTTL),
	}

	q.mu.Lock()
	q.requests[id] = req
	q.mu.Unlock()

	// Notify user (non-blocking — notification failure shouldn't block the queue)
	if q.notifier != nil {
		approvalURL := fmt.Sprintf("%s/vault/approve/%s", q.baseURL, id)
		go q.notifier.SendApprovalRequest(*req, approvalURL)
	}

	return req, nil
}

// Get returns the current state of an approval request.
func (q *ApprovalQueue) Get(id string) (*ApprovalRequest, error) {
	q.mu.RLock()
	defer q.mu.RUnlock()

	req, ok := q.requests[id]
	if !ok {
		return nil, fmt.Errorf("approval %q not found", id)
	}

	// Return a copy with effective state
	copy := *req
	copy.State = req.EffectiveState()
	return &copy, nil
}

// Grant marks an approval as granted. Called after successful WebAuthn authentication.
func (q *ApprovalQueue) Grant(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	req, ok := q.requests[id]
	if !ok {
		return fmt.Errorf("approval %q not found", id)
	}
	if req.EffectiveState() != ApprovalPending {
		return fmt.Errorf("approval %q is %s, not pending", id, req.EffectiveState())
	}

	now := time.Now().UTC()
	req.State = ApprovalGranted
	req.DecidedAt = &now
	return nil
}

// Deny marks an approval as denied.
func (q *ApprovalQueue) Deny(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	req, ok := q.requests[id]
	if !ok {
		return fmt.Errorf("approval %q not found", id)
	}
	if req.EffectiveState() != ApprovalPending {
		return fmt.Errorf("approval %q is %s, not pending", id, req.EffectiveState())
	}

	now := time.Now().UTC()
	req.State = ApprovalDenied
	req.DecidedAt = &now
	return nil
}

// Pending returns all currently pending (non-expired) requests.
func (q *ApprovalQueue) Pending() []ApprovalRequest {
	q.mu.RLock()
	defer q.mu.RUnlock()

	var result []ApprovalRequest
	for _, req := range q.requests {
		if req.EffectiveState() == ApprovalPending {
			result = append(result, *req)
		}
	}
	return result
}

// cleanupLoop removes expired/decided requests older than 1 hour.
func (q *ApprovalQueue) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-q.done:
			return
		case <-ticker.C:
			q.mu.Lock()
			for id, req := range q.requests {
				if time.Since(req.CreatedAt) > time.Hour {
					delete(q.requests, id)
				}
			}
			q.mu.Unlock()
		}
	}
}

func generateApprovalID() (string, error) {
	b := make([]byte, approvalIDBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate approval ID: %w", err)
	}
	return hex.EncodeToString(b), nil
}
