// Package authz is the one place that decides what a role may do (ADR-0102). It is pure: the
// api resolves the actor (user and role of the session's membership) per request and asks this
// package; services ask it again for decisions that depend on the target (member management).
//
// Operations are the operation ids of api/openapi.yaml. An operation missing from the table is
// denied, and a test in internal/api fails when the spec has an operation the table does not list.
package authz

import (
	"errors"
	"strings"

	"github.com/google/uuid"
)

// Role is a membership role.
type Role string

// Roles, highest first.
const (
	Owner  Role = "owner"
	Admin  Role = "admin"
	Member Role = "member"
	Viewer Role = "viewer"
)

// ErrForbidden means the actor's role does not allow the action.
var ErrForbidden = errors.New("authz: forbidden")

// rank orders the roles; an unknown role ranks below viewer and is allowed nothing.
func rank(r Role) int {
	switch r {
	case Owner:
		return 4
	case Admin:
		return 3
	case Member:
		return 2
	case Viewer:
		return 1
	}
	return 0
}

// Valid reports whether r is one of the four roles.
func Valid(r Role) bool { return rank(r) > 0 }

// AtLeast reports whether r is min or higher.
func (r Role) AtLeast(min Role) bool { return rank(r) > 0 && rank(r) >= rank(min) }

// Actor is the user of a request and its role in the session's organization.
type Actor struct {
	UserID uuid.UUID
	Role   Role
	// Projects is the effective role in each project the actor belongs to (ADR-0102, T-02).
	// Owners and admins see every project and need no entries.
	Projects map[uuid.UUID]Role
}

// ErrProjectForbidden means the actor sees the project but its role there does not allow the
// change (a member with the viewer override).
var ErrProjectForbidden = errors.New("authz: forbidden in this project")

// ProjectRole is the role of a member or viewer inside a project: the organization role, lowered
// by the project's override when there is one (an override never raises the organization role).
func ProjectRole(org Role, override *Role) Role {
	if override != nil && Valid(*override) && rank(*override) < rank(org) {
		return *override
	}
	return org
}

// AllProjects reports whether the actor sees every project of the organization.
func (a Actor) AllProjects() bool { return a.Role.AtLeast(Admin) }

// RoleIn returns the actor's effective role in a project and whether it sees the project at all.
func (a Actor) RoleIn(project uuid.UUID) (Role, bool) {
	if a.AllProjects() {
		return a.Role, true
	}
	r, ok := a.Projects[project]
	if !ok || !Valid(r) || !a.Role.AtLeast(Viewer) {
		return "", false
	}
	// Defence in depth: a project role never exceeds the organization role.
	return ProjectRole(a.Role, &r), true
}

// Sees reports whether the actor may read a project and what belongs to it.
func (a Actor) Sees(project uuid.UUID) bool {
	_, ok := a.RoleIn(project)
	return ok
}

// CanEditIn reports whether the actor may create, change, pause and delete monitors in a project.
func (a Actor) CanEditIn(project uuid.UUID) bool {
	r, ok := a.RoleIn(project)
	return ok && r.AtLeast(Member)
}

// Scope is the set of projects whose data a list may show: every project, or the listed ones.
type Scope struct {
	All bool
	IDs []uuid.UUID
}

// Scope returns the projects the actor sees.
func (a Actor) Scope() Scope {
	if a.AllProjects() {
		return Scope{All: true}
	}
	ids := make([]uuid.UUID, 0, len(a.Projects))
	for id := range a.Projects {
		ids = append(ids, id)
	}
	return Scope{IDs: ids}
}

// Includes reports whether the scope covers a project.
func (s Scope) Includes(project uuid.UUID) bool {
	if s.All {
		return true
	}
	for _, id := range s.IDs {
		if id == project {
			return true
		}
	}
	return false
}

// Narrow limits the scope to one project (a list filter); the result is empty when the scope
// does not include it.
func (s Scope) Narrow(project uuid.UUID) Scope {
	if s.Includes(project) {
		return Scope{IDs: []uuid.UUID{project}}
	}
	return Scope{IDs: []uuid.UUID{}}
}

// Public marks operations that need no session; their entry in the table is Public.
const Public Role = "public"

// operations maps every operation id (lowercase) to the minimum role. Viewer means any member.
// The platform admin operations need the platform-admin flag on top (requirePlatformAdmin in
// internal/api); they are listed with Viewer because the flag, not a role, decides.
var operations = map[string]Role{
	// No session.
	"gethealth":         Public,
	"getpublicconfig":   Public,
	"login":             Public,
	"register":          Public,
	"pingheartbeat":     Public,
	"pingheartbeatpost": Public,
	"lookupinvitation":  Public,

	// The session itself and the user's own organizations: any role.
	"logout":             Viewer,
	"getme":              Viewer,
	"listorganizations":  Viewer,
	"switchorganization": Viewer,
	"acceptinvitation":   Viewer,
	"leaveorganization":  Viewer,

	// Reading the organization's data.
	"listmonitors":         Viewer,
	"getmonitor":           Viewer,
	"getmonitorstats":      Viewer,
	"listmonitorchecks":    Viewer,
	"listmonitorincidents": Viewer,
	"getusage":             Viewer,
	"listchannels":         Viewer,
	"getchannel":           Viewer,
	"listmembers":          Viewer,
	// Projects: the list and reads show only the projects the actor sees (T-02).
	"listprojects":       Viewer,
	"getproject":         Viewer,
	"listprojectmembers": Viewer,

	// Monitors and heartbeats.
	"createmonitor":            Member,
	"updatemonitor":            Member,
	"deletemonitor":            Member,
	"pausemonitor":             Member,
	"resumemonitor":            Member,
	"regenerateheartbeattoken": Member,

	// Notification channels, members and invitations.
	"createchannel":    Admin,
	"updatechannel":    Admin,
	"deletechannel":    Admin,
	"testchannel":      Admin,
	"listinvitations":  Admin,
	"createinvitation": Admin,
	"revokeinvitation": Admin,
	"updatemember":     Admin,
	"removemember":     Admin,

	// Projects and their members (T-02).
	"createproject":       Admin,
	"updateproject":       Admin,
	"deleteproject":       Admin,
	"putprojectmember":    Admin,
	"removeprojectmember": Admin,

	// The status page (ADR-0103): every role sees its settings and link; owners and admins
	// configure and publish it. Components and the preview name monitors of every project.
	"liststatuspages":          Viewer,
	"getstatuspage":            Viewer,
	"createstatuspage":         Admin,
	"updatestatuspage":         Admin,
	"deletestatuspage":         Admin,
	"setstatuspagepassword":    Admin,
	"clearstatuspagepassword":  Admin,
	"liststatuspagecomponents": Admin,
	"putstatuspagecomponents":  Admin,
	"previewstatuspage":        Admin,

	// The organization itself.
	"deleteorganization": Owner,

	// Platform admin panel (ADR-0097).
	"admingetstats":          Viewer,
	"adminlisttenants":       Viewer,
	"admingettenant":         Viewer,
	"adminupdatetenant":      Viewer,
	"admindeletetenant":      Viewer,
	"adminlisttenantusers":   Viewer,
	"adminresetuserpassword": Viewer,
}

// MinRole returns the minimum role of an operation (Public for operations without a session)
// and whether the operation is in the table. Operation ids are compared case-insensitively:
// the generated server passes them as Go names.
func MinRole(operationID string) (Role, bool) {
	r, ok := operations[strings.ToLower(operationID)]
	return r, ok
}

// Operations returns every operation id in the table (lowercase).
func Operations() []string {
	out := make([]string, 0, len(operations))
	for op := range operations {
		out = append(out, op)
	}
	return out
}

// Can reports whether the actor may call the operation. Unknown operations and unknown roles
// are denied; Public operations are allowed to anyone.
func Can(a Actor, operationID string) bool {
	min, ok := MinRole(operationID)
	if !ok {
		return false
	}
	if min == Public {
		return true
	}
	return a.Role.AtLeast(min)
}

// CanManage reports whether the actor may change or remove a member whose role is target, and
// (for a role change) give it the role to (pass "" for a removal). Only admins and owners manage
// members; an admin never touches an owner and never grants owner (ADR-0102): nobody grants a
// role above their own. Guard rails that need the database (the last owner, the platform admin)
// are checked by the caller.
func CanManage(a Actor, target, to Role) bool {
	if !a.Role.AtLeast(Admin) || !Valid(target) {
		return false
	}
	if rank(target) > rank(a.Role) {
		return false
	}
	if to != "" && (!Valid(to) || rank(to) > rank(a.Role)) {
		return false
	}
	return true
}

// CanInvite reports whether the actor may create an invitation with the role: admins and owners,
// for admin, member or viewer (owner is granted by a role change, never by a link).
func CanInvite(a Actor, role Role) bool {
	return a.Role.AtLeast(Admin) && role != Owner && Valid(role) && rank(role) <= rank(a.Role)
}
