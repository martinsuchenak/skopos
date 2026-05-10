package build

import "testing"

func TestVersionDefined(t *testing.T) {
	if Version == "" {
		t.Error("Version should not be empty")
	}
}

func TestDateDefined(t *testing.T) {
	if Date == "" {
		t.Error("Date should not be empty")
	}
}
