package orpheus

import (
	"encoding/json"
	"fmt"
)

// APIError is a non-2xx response mapped from the API's RFC 7807 problem+json
// body. Callers can errors.As to inspect Status/Type/RequestID.
type APIError struct {
	Status    int    `json:"status"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Detail    string `json:"detail"`
	RequestID string `json:"request_id,omitempty"`
	// Raw is the response body when it wasn't valid problem+json.
	Raw string `json:"-"`
}

func (e *APIError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("orpheus: %d %s: %s", e.Status, e.Title, e.Detail)
	}
	if e.Title != "" {
		return fmt.Sprintf("orpheus: %d %s", e.Status, e.Title)
	}
	return fmt.Sprintf("orpheus: %d: %s", e.Status, e.Raw)
}

// IsNotFound reports whether the error is a 404.
func (e *APIError) IsNotFound() bool { return e.Status == 404 }

// IsForbidden reports whether the error is a 403.
func (e *APIError) IsForbidden() bool { return e.Status == 403 }

func parseAPIError(status int, body []byte) *APIError {
	e := &APIError{Status: status, Raw: string(body)}
	// Best-effort: overlay any problem+json fields. On malformed bodies the
	// Status + Raw still make the error actionable.
	_ = json.Unmarshal(body, e)
	e.Status = status // ensure the transport status wins over any body value
	return e
}
