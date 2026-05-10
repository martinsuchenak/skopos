package ctxkeys

import "testing"

func TestContextKeys(t *testing.T) {
	if UserIDKey == "" {
		t.Error("UserIDKey should not be empty")
	}
	if RequestIDKey == "" {
		t.Error("RequestIDKey should not be empty")
	}
	if APIKeyKey == "" {
		t.Error("APIKeyKey should not be empty")
	}
}
