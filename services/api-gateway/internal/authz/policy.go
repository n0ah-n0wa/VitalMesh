// Package authz makes authorization decisions. The rules are data: a policy
// maps each role to the permissions it holds, and a route table maps each
// API operation to the permission it requires. Transports enforce the
// decision; nothing here depends on HTTP.
package authz

import (
	"context"
	"log/slog"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

// Permission names a class of operation. Two reserved values describe
// routes that need no permission: Public needs no token at all and
// Authenticated needs a valid token of any role.
type Permission string

// Permissions of the public API. Each is held by the roles listed in
// Default.
const (
	Public        Permission = "public"
	Authenticated Permission = "authenticated"

	UsersManage       Permission = "users:manage"
	PatientsRead      Permission = "patients:read"
	PatientsWrite     Permission = "patients:write"
	MeasurementsRead  Permission = "measurements:read"
	MeasurementsWrite Permission = "measurements:write"
	JobsRead          Permission = "jobs:read"
	JobsWrite         Permission = "jobs:write"
	ResultsRead       Permission = "results:read"
)

// All lists every permission a policy can grant, for exhaustive tests.
var All = []Permission{
	UsersManage,
	PatientsRead, PatientsWrite,
	MeasurementsRead, MeasurementsWrite,
	JobsRead, JobsWrite,
	ResultsRead,
}

// CodePermissionDenied is returned when an authenticated account's role
// does not hold the permission an operation requires. 403.
const CodePermissionDenied = "PERMISSION_DENIED"

// PermissionDenied is the client-safe error for a denied operation. It
// names neither the permission nor the role.
func PermissionDenied() *domain.Error {
	return domain.New(domain.KindForbidden, CodePermissionDenied, "The account is not permitted to perform this operation.")
}

// Policy grants permissions to roles. It is immutable once built.
type Policy struct {
	grants map[domain.Role]map[Permission]bool
}

// NewPolicy builds a Policy from a role -> permissions table.
func NewPolicy(grants map[domain.Role][]Permission) *Policy {
	p := &Policy{grants: make(map[domain.Role]map[Permission]bool, len(grants))}
	for role, perms := range grants {
		set := make(map[Permission]bool, len(perms))
		for _, perm := range perms {
			set[perm] = true
		}
		p.grants[role] = set
	}
	return p
}

// Default is the platform's role policy (SPECIFICATIONS.md sections 9.1 and
// 30, docs/API.md "Authorization"):
//
//   - ADMIN manages users and can do everything an OPERATOR can;
//   - OPERATOR creates and deletes patients and measurements and drives
//     processing jobs;
//   - USER reads.
func Default() *Policy {
	read := []Permission{PatientsRead, MeasurementsRead, JobsRead, ResultsRead}
	operate := append([]Permission{PatientsWrite, MeasurementsWrite, JobsWrite}, read...)
	admin := append([]Permission{UsersManage}, operate...)
	return NewPolicy(map[domain.Role][]Permission{
		domain.RoleAdmin:    admin,
		domain.RoleOperator: operate,
		domain.RoleUser:     read,
	})
}

// Allows reports whether role holds permission. Public is allowed to
// everyone, Authenticated to every role the policy knows, and anything else
// only when granted explicitly. Unknown roles hold nothing.
func (p *Policy) Allows(role domain.Role, permission Permission) bool {
	if permission == Public {
		return true
	}
	grants, known := p.grants[role]
	if !known {
		return false
	}
	if permission == Authenticated {
		return true
	}
	return grants[permission]
}

// Permissions returns the permissions granted to role, in the order of All.
func (p *Policy) Permissions(role domain.Role) []Permission {
	var out []Permission
	for _, perm := range All {
		if p.grants[role][perm] {
			out = append(out, perm)
		}
	}
	return out
}

// Authorize decides whether the principal carried by ctx may perform an
// operation needing permission. It returns nil when allowed, an
// authentication error when ctx carries no principal (unless the permission
// is Public), and PermissionDenied otherwise. Denials are logged with the
// account, its role and the permission; nothing else.
func (p *Policy) Authorize(ctx context.Context, logger *slog.Logger, permission Permission) error {
	if permission == Public {
		return nil
	}
	principal, ok := auth.FromContext(ctx)
	if !ok {
		return auth.AuthenticationRequired()
	}
	if !p.Allows(principal.Role, permission) {
		logger.InfoContext(ctx, "authorization denied",
			"user_id", principal.UserID,
			"role", principal.Role,
			"permission", permission,
		)
		return PermissionDenied()
	}
	return nil
}
