package common

import (
	"testing"
)

// TestAPIErrorIsNotFound tests the IsNotFound helper.
func TestAPIErrorIsNotFound(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		want       bool
	}{
		{"404 is NotFound", 404, true},
		{"400 is not NotFound", 400, false},
		{"500 is not NotFound", 500, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &APIError{StatusCode: tt.statusCode}
			if got := err.IsNotFound(); got != tt.want {
				t.Errorf("IsNotFound() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAPIErrorIsConflict tests the IsConflict helper.
func TestAPIErrorIsConflict(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		want       bool
	}{
		{"409 is Conflict", 409, true},
		{"400 is not Conflict", 400, false},
		{"404 is not Conflict", 404, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &APIError{StatusCode: tt.statusCode}
			if got := err.IsConflict(); got != tt.want {
				t.Errorf("IsConflict() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAPIErrorError tests error message formatting.
func TestAPIErrorError(t *testing.T) {
	err := &APIError{
		Method:     "POST",
		Path:       "/api/v1/apps",
		StatusCode: 409,
		Body:       `{"error":"app already exists"}`,
	}

	want := "API error: POST /api/v1/apps → HTTP 409: {\"error\":\"app already exists\"}"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
