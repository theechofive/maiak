package authz

import (
	"testing"
)

var roles = []Role{Owner, Admin, Member, Viewer}

func TestCanDeniesUnknown(t *testing.T) {
	for _, r := range roles {
		if Can(Actor{Role: r}, "noSuchOperation") {
			t.Errorf("%s may call an unknown operation", r)
		}
	}
	if Can(Actor{Role: "superuser"}, "listMonitors") {
		t.Error("unknown role may read monitors")
	}
	if Can(Actor{}, "getMe") {
		t.Error("empty role may call getMe")
	}
	if !Can(Actor{}, "getHealth") {
		t.Error("public operation refused")
	}
}

func TestCanMatrix(t *testing.T) {
	// One representative operation per minimum role, case as the generated server passes it.
	cases := map[string]Role{
		"ListMonitors":             Viewer,
		"CreateMonitor":            Member,
		"RegenerateHeartbeatToken": Member,
		"CreateChannel":            Admin,
		"TestChannel":              Admin,
		"CreateInvitation":         Admin,
		"UpdateMember":             Admin,
		"DeleteOrganization":       Owner,
	}
	for op, min := range cases {
		for _, r := range roles {
			want := rank(r) >= rank(min)
			if got := Can(Actor{Role: r}, op); got != want {
				t.Errorf("Can(%s, %s) = %v, want %v", r, op, got, want)
			}
		}
	}
}

func TestCanManage(t *testing.T) {
	cases := []struct {
		actor, target, to Role
		want              bool
	}{
		{Owner, Owner, Admin, true},
		{Owner, Viewer, Owner, true},
		{Owner, Admin, "", true},
		{Admin, Owner, Admin, false},  // admins cannot touch owners
		{Admin, Owner, "", false},     // nor remove them
		{Admin, Member, Owner, false}, // nor grant owner
		{Admin, Member, Admin, true},
		{Admin, Admin, Viewer, true},
		{Admin, Viewer, "", true},
		{Member, Viewer, Member, false},
		{Viewer, Viewer, "", false},
		{Owner, Member, "root", false},
		{Owner, "root", Member, false},
	}
	for _, c := range cases {
		if got := CanManage(Actor{Role: c.actor}, c.target, c.to); got != c.want {
			t.Errorf("CanManage(%s, %s -> %q) = %v, want %v", c.actor, c.target, c.to, got, c.want)
		}
	}
}

func TestCanInvite(t *testing.T) {
	cases := []struct {
		actor, role Role
		want        bool
	}{
		{Owner, Admin, true},
		{Owner, Owner, false},
		{Admin, Admin, true},
		{Admin, Viewer, true},
		{Member, Viewer, false},
		{Viewer, Viewer, false},
		{Owner, "root", false},
	}
	for _, c := range cases {
		if got := CanInvite(Actor{Role: c.actor}, c.role); got != c.want {
			t.Errorf("CanInvite(%s, %s) = %v, want %v", c.actor, c.role, got, c.want)
		}
	}
}

func TestRoleAtLeast(t *testing.T) {
	if !Owner.AtLeast(Viewer) || Viewer.AtLeast(Member) || Role("").AtLeast(Viewer) {
		t.Fatal("AtLeast ordering")
	}
}
