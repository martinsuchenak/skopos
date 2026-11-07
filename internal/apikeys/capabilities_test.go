package apikeys

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/martinsuchenak/skopos/internal/auth"
)

// A key minted with a preset (plus extras) stores the normalised capability
// list, lookups hand it to the principal, an update can change or clear it,
// and keys without capabilities stay full access (nil).
func TestKeyCapabilities(t *testing.T) {
	st := testStorage(t)
	svc := NewService(st)
	ctx := context.Background()
	seedWorkspace(t, st, "ws-a")

	agent, err := svc.Create(ctx, CreateInput{Name: "agent", Workspaces: []string{"ws-a"}, Preset: "agent", Capabilities: []string{"code:read", "workspaces:read"}})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := auth.ParseCapabilities(append(append([]string{}, auth.Presets["agent"]...), "workspaces:read"))
	if !reflect.DeepEqual(agent.Key.Capabilities, want) {
		t.Fatalf("created capabilities %v, want %v", agent.Key.Capabilities, want)
	}
	info, err := st.LookupKey(ctx, auth.HashKey(agent.Secret))
	if err != nil || !reflect.DeepEqual(info.Capabilities, want) {
		t.Fatalf("lookup: %+v, %v", info, err)
	}
	full, err := svc.Create(ctx, CreateInput{Name: "full", Workspaces: []string{"ws-a"}})
	if err != nil || full.Key.Capabilities != nil {
		t.Fatalf("a key without capabilities is full access (nil): %+v, %v", full, err)
	}
	if info, _ := st.LookupKey(ctx, auth.HashKey(full.Secret)); info.Capabilities != nil {
		t.Fatalf("full key lookup capabilities: %v", info.Capabilities)
	}

	caps := []string{"rest", "runs:write"}
	updated, err := svc.Update(ctx, agent.Key.ID, UpdateInput{Capabilities: &caps})
	if err != nil || !reflect.DeepEqual(updated.Capabilities, []string{"rest", "runs:write"}) {
		t.Fatalf("update: %+v, %v", updated, err)
	}
	cleared := []string{}
	updated, err = svc.Update(ctx, agent.Key.ID, UpdateInput{Capabilities: &cleared})
	if err != nil || updated.Capabilities != nil {
		t.Fatalf("[] clears to full access: %+v, %v", updated, err)
	}
	// Changing only the name leaves capabilities alone.
	updated, _ = svc.Update(ctx, agent.Key.ID, UpdateInput{Preset: "agent"})
	name := "renamed"
	if again, err := svc.Update(ctx, agent.Key.ID, UpdateInput{Name: &name}); err != nil || !reflect.DeepEqual(again.Capabilities, updated.Capabilities) {
		t.Fatalf("rename kept capabilities? %+v, %v", again, err)
	}

	for _, bad := range []CreateInput{
		{Name: "x", Workspaces: []string{"ws-a"}, Preset: "superuser"},
		{Name: "x", Workspaces: []string{"ws-a"}, Capabilities: []string{"keys:write"}},
	} {
		if _, err := svc.Create(ctx, bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%+v: want ErrInvalidInput, got %v", bad, err)
		}
	}
}

// Sending a key's current capabilities back is not a change: its streams
// stay open.
func TestKeyCapabilitiesNoOpUpdate(t *testing.T) {
	st := testStorage(t)
	svc := NewService(st)
	ctx := context.Background()
	seedWorkspace(t, st, "ws-a")
	var dropped int
	svc.SetScopeNotifier(func([]string) { dropped++ })
	k, err := svc.Create(ctx, CreateInput{Name: "agent", Workspaces: []string{"ws-a"}, Preset: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, k.Key.ID, UpdateInput{Preset: "agent"}); err != nil || dropped != 0 {
		t.Fatalf("same preset again: dropped %d, %v", dropped, err)
	}
	full, _ := svc.Create(ctx, CreateInput{Name: "full", Workspaces: []string{"ws-a"}})
	none := []string{}
	if _, err := svc.Update(ctx, full.Key.ID, UpdateInput{Capabilities: &none}); err != nil || dropped != 0 {
		t.Fatalf("full stays full: dropped %d, %v", dropped, err)
	}
	if _, err := svc.Update(ctx, full.Key.ID, UpdateInput{Preset: "agent"}); err != nil || dropped != 1 {
		t.Fatalf("a real change drops the streams once: dropped %d, %v", dropped, err)
	}
}
