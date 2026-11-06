package authz

import (
	"testing"

	"github.com/google/uuid"
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

func TestProjectAccess(t *testing.T) {
	p1, p2, p3 := uuid.New(), uuid.New(), uuid.New()
	viewer := Viewer
	member := Member
	if got := ProjectRole(Member, &viewer); got != Viewer {
		t.Errorf("member with viewer override = %s", got)
	}
	if got := ProjectRole(Viewer, &member); got != Viewer {
		t.Errorf("an override raised a viewer to %s", got)
	}
	if got := ProjectRole(Member, nil); got != Member {
		t.Errorf("no override = %s", got)
	}

	m := Actor{Role: Member, Projects: map[uuid.UUID]Role{p1: Member, p2: Viewer}}
	for _, c := range []struct {
		project    uuid.UUID
		sees, edit bool
	}{{p1, true, true}, {p2, true, false}, {p3, false, false}} {
		if m.Sees(c.project) != c.sees || m.CanEditIn(c.project) != c.edit {
			t.Errorf("member in %v: sees=%v edit=%v", c.project, m.Sees(c.project), m.CanEditIn(c.project))
		}
	}
	// An entry above the organization role is capped.
	v := Actor{Role: Viewer, Projects: map[uuid.UUID]Role{p1: Owner}}
	if r, ok := v.RoleIn(p1); !ok || r != Viewer || v.CanEditIn(p1) {
		t.Errorf("viewer capped: %s %v", r, ok)
	}
	if (Actor{Role: "nobody", Projects: map[uuid.UUID]Role{p1: Member}}).Sees(p1) {
		t.Error("unknown role sees a project")
	}
	for _, r := range []Role{Owner, Admin} {
		a := Actor{Role: r}
		if !a.Sees(p3) || !a.CanEditIn(p3) || !a.Scope().All {
			t.Errorf("%s does not see every project", r)
		}
	}
	sc := m.Scope()
	if sc.All || !sc.Includes(p1) || !sc.Includes(p2) || sc.Includes(p3) {
		t.Errorf("member scope %+v", sc)
	}
	if n := sc.Narrow(p3); n.All || len(n.IDs) != 0 {
		t.Errorf("narrow to an unseen project = %+v", n)
	}
	if n := (Scope{All: true}).Narrow(p3); n.All || len(n.IDs) != 1 {
		t.Errorf("narrow all = %+v", n)
	}
}
