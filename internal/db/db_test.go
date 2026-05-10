package db

import "testing"

func TestSchemaFSExists(t *testing.T) {
	data, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		t.Fatalf("failed to read embedded schema: %v", err)
	}
	if len(data) == 0 {
		t.Error("schema.sql should not be empty")
	}
}
