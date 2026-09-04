package hostruntime

import (
	"bytes"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
)

// postgresCatalogProtocolExpected is an in-memory projection of the applied
// revision. PreviousRevision is supplied by the existing applied state; it is
// deliberately not a new persistence mechanism.
type postgresCatalogProtocolExpected struct {
	Binding                    postgresCatalogProtocolBinding
	Revision, PreviousRevision string
	Desired, Previous          []hostcontract.LocalDataClient
}
type postgresCatalogProtocolBinding struct{ Environment, Server, Ownership, Service string }
type postgresCatalogProtocolState uint8

const (
	postgresCatalogProtocolUnavailable postgresCatalogProtocolState = iota
	postgresCatalogProtocolPrior
	postgresCatalogProtocolTarget
	postgresCatalogProtocolPartial
	postgresCatalogProtocolForeign
	postgresCatalogProtocolMixed
	postgresCatalogProtocolAbsent
	postgresCatalogProtocolCreateOnly
	postgresCatalogProtocolExact
)

type postgresCatalogProtocolRoleObservation struct {
	Marker                                                                           postgresCatalogProtocolState
	Operation                                                                        string
	Admin, Owners, Clients, Creators, Memberships, Retired, OldSettings, InitialSafe bool
}
type postgresCatalogProtocolReferenceObservation struct {
	Key                       string
	State                     postgresCatalogProtocolState
	Operation                 string
	StableOwner, CreatorOwner bool
}
type postgresCatalogProtocolDetailObservation struct {
	Key                                                                                   string
	State                                                                                 postgresCatalogProtocolState
	Operation                                                                             string
	SchemaOwner, PublicCreateRevoked, OwnerUsageCreate, Settings, Connect, NoScopedExtras bool
	Fresh, Owner                                                                          bool
}
type postgresCatalogProtocolDatabaseObservation struct {
	Key       string
	Reference postgresCatalogProtocolReferenceObservation
	Detail    postgresCatalogProtocolDetailObservation
}
type postgresCatalogProtocolClassification struct {
	State   postgresCatalogProtocolState
	Pending bool
}
type postgresCatalogProtocolRoleMarkerBinding struct {
	Name    string
	Markers []string
}

func postgresCatalogProtocolRoleMarkerAllows(binding postgresCatalogProtocolRoleMarkerBinding, marker string) bool {
	for _, allowed := range binding.Markers {
		if marker == allowed {
			return true
		}
	}
	return false
}

func (o postgresCatalogProtocolRoleObservation) StructuralExact() bool {
	return o.Admin && o.Owners && o.Clients && o.Creators && o.Memberships && o.Retired && o.OldSettings
}
func (o postgresCatalogProtocolDetailObservation) StructuralExact() bool {
	return o.SchemaOwner && o.PublicCreateRevoked && o.OwnerUsageCreate && o.Settings && o.Connect && o.NoScopedExtras
}
func (o postgresCatalogProtocolDetailObservation) CreateOnlyExact() bool { return o.Fresh && o.Owner }

func postgresCatalogProtocolHostValue(v string) bool { return v != "" && utf8.ValidString(v) }
func (e postgresCatalogProtocolExpected) valid() error {
	if !postgresCatalogProtocolHostValue(e.Binding.Environment) || !postgresCatalogProtocolHostValue(e.Binding.Server) || !postgresCatalogProtocolHostValue(e.Binding.Ownership) || !postgresCatalogProtocolHostValue(e.Binding.Service) || e.Revision == "" {
		return errors.New("postgres catalog expected")
	}
	if _, err := hostcontract.ParseRevision(e.Revision); err != nil {
		return errors.New("postgres catalog expected")
	}
	if e.PreviousRevision != "" {
		if _, err := hostcontract.ParseRevision(e.PreviousRevision); err != nil {
			return errors.New("postgres catalog expected")
		}
	}
	if e.PreviousRevision == "" && len(e.Previous) != 0 {
		return errors.New("postgres catalog previous")
	}
	for _, clients := range [][]hostcontract.LocalDataClient{e.Desired, e.Previous} {
		seen := map[string]bool{}
		for _, c := range clients {
			if !validPostgresClient(c) || seen[c.AppID] {
				return errors.New("postgres catalog client")
			}
			seen[c.AppID] = true
		}
	}
	return nil
}
func postgresCatalogProtocolBindingToken(e postgresCatalogProtocolExpected) string {
	return token(e.Binding.Environment, e.Binding.Server, e.Binding.Ownership, e.Binding.Service)
}
func postgresCatalogProtocolOperation(r string) string { return token(r) }
func postgresCatalogProtocolRolesMarker(e postgresCatalogProtocolExpected) string {
	return "s2hpg2:" + postgresCatalogProtocolBindingToken(e) + ":roles:" + postgresCatalogProtocolOperation(e.Revision) + ":admin"
}
func postgresCatalogProtocolOwnerMarker(e postgresCatalogProtocolExpected, db string) string {
	return "s2hpg2:" + postgresCatalogProtocolBindingToken(e) + ":roles:" + postgresCatalogProtocolOperation(e.Revision) + ":owner:" + token(db)
}
func postgresCatalogProtocolClientMarker(e postgresCatalogProtocolExpected, username string) string {
	apps := []string{}
	for _, c := range e.Desired {
		if c.Username == username {
			apps = append(apps, c.AppID)
		}
	}
	sort.Strings(apps)
	return "s2hpg2:" + postgresCatalogProtocolBindingToken(e) + ":roles:" + postgresCatalogProtocolOperation(e.Revision) + ":client:" + token(append([]string{username}, apps...)...)
}
func postgresCatalogProtocolDatabaseMarker(e postgresCatalogProtocolExpected, db string) string {
	return "s2hpg2:" + postgresCatalogProtocolBindingToken(e) + ":database:" + postgresCatalogProtocolOperation(e.Revision) + ":" + token(db)
}
func postgresCatalogProtocolCreator(e postgresCatalogProtocolExpected, db string) string {
	return "s2h_create_" + token(postgresCatalogProtocolBindingToken(e), e.Binding.Service, postgresCatalogProtocolOperation(e.Revision), postgresCatalogProtocolDatabaseToken(db))
}
func postgresCatalogProtocolCreatorMarker(e postgresCatalogProtocolExpected, db string) string {
	return "s2hpg2:" + postgresCatalogProtocolBindingToken(e) + ":create:" + postgresCatalogProtocolOperation(e.Revision) + ":" + postgresCatalogProtocolDatabaseToken(db)
}
func postgresCatalogProtocolPreviousDatabaseMarker(e postgresCatalogProtocolExpected, db string) string {
	if e.PreviousRevision == "" {
		return ""
	}
	p := postgresCatalogProtocolPreviousExpected(e)
	return postgresCatalogProtocolDatabaseMarker(p, db)
}
func postgresCatalogProtocolPreviousExpected(e postgresCatalogProtocolExpected) postgresCatalogProtocolExpected {
	p := e
	p.Desired, p.Revision = e.Previous, e.PreviousRevision
	return p
}
func postgresCatalogProtocolRoleMarkerBindings(e postgresCatalogProtocolExpected) []postgresCatalogProtocolRoleMarkerBinding {
	markers := map[string]map[string]bool{}
	add := func(name, marker string) {
		if markers[name] == nil {
			markers[name] = map[string]bool{}
		}
		markers[name][marker] = true
	}
	add("s2h_admin", postgresCatalogProtocolRolesMarker(e))
	for _, db := range postgresCatalogProtocolDatabases(e.Desired) {
		add(postgresOwner(e.Binding.Service, db), postgresCatalogProtocolOwnerMarker(e, db))
		add(postgresCatalogProtocolCreator(e, db), postgresCatalogProtocolCreatorMarker(e, db))
	}
	for _, username := range postgresCatalogProtocolUsernames(e.Desired) {
		add(username, postgresCatalogProtocolClientMarker(e, username))
	}
	if e.PreviousRevision != "" {
		p := postgresCatalogProtocolPreviousExpected(e)
		add("s2h_admin", postgresCatalogProtocolRolesMarker(p))
		for _, db := range postgresCatalogProtocolDatabases(e.Previous) {
			add(postgresOwner(e.Binding.Service, db), postgresCatalogProtocolOwnerMarker(p, db))
			add(postgresCatalogProtocolCreator(p, db), postgresCatalogProtocolCreatorMarker(p, db))
		}
		for _, username := range postgresCatalogProtocolUsernames(e.Previous) {
			add(username, postgresCatalogProtocolClientMarker(p, username))
		}
	}
	names := make([]string, 0, len(markers))
	for name := range markers {
		names = append(names, name)
	}
	sort.Strings(names)
	bindings := make([]postgresCatalogProtocolRoleMarkerBinding, 0, len(names))
	for _, name := range names {
		values := make([]string, 0, len(markers[name]))
		for marker := range markers[name] {
			values = append(values, marker)
		}
		sort.Strings(values)
		bindings = append(bindings, postgresCatalogProtocolRoleMarkerBinding{Name: name, Markers: values})
	}
	return bindings
}
func postgresCatalogProtocolDatabaseToken(db string) string { return token(db) }
func sqlQuote(v string) string                              { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }
func bit(sql string) string                                 { return "CASE WHEN " + sql + " THEN '1' ELSE '0' END" }
func postgresCatalogProtocolRecord(fields []string) []byte {
	return []byte(strings.Join(fields, "\t") + "\n")
}
func postgresCatalogProtocolSQLRecord(fields []string) string {
	return strings.Join(fields, " || E'\\t' || ")
}
func roleComment(oid, marker string) string {
	return "EXISTS (SELECT 1 FROM pg_shdescription sd WHERE sd.objoid=" + oid + " AND sd.classoid='pg_authid'::regclass AND sd.description=" + sqlQuote(marker) + ")"
}
func dbComment(oid, marker string) string {
	return "EXISTS (SELECT 1 FROM pg_shdescription sd WHERE sd.objoid=" + oid + " AND sd.classoid='pg_database'::regclass AND sd.description=" + sqlQuote(marker) + ")"
}

func postgresCatalogProtocolRolesSQL(e postgresCatalogProtocolExpected) string {
	if e.valid() != nil {
		return ""
	}
	target := postgresCatalogProtocolRolesMarker(e)
	prior := ""
	if e.PreviousRevision != "" {
		p := postgresCatalogProtocolPreviousExpected(e)
		prior = postgresCatalogProtocolRolesMarker(p)
	}
	structure := func(x postgresCatalogProtocolExpected) (string, string, string, string, string) {
		owners, clients, creators, memberships, settings := "TRUE", "TRUE", "TRUE", "TRUE", "TRUE"
		for _, db := range postgresCatalogProtocolDatabases(x.Desired) {
			owner := postgresOwner(x.Binding.Service, db)
			owners += " AND EXISTS (SELECT 1 FROM pg_roles r WHERE r.rolname=" + sqlQuote(owner) + " AND NOT r.rolcanlogin AND NOT r.rolinherit AND NOT r.rolsuper AND NOT r.rolcreatedb AND NOT r.rolcreaterole AND NOT r.rolreplication AND NOT r.rolbypassrls AND " + roleComment("r.oid", postgresCatalogProtocolOwnerMarker(x, db)) + ")"
			creator := postgresCatalogProtocolCreator(x, db)
			creators += " AND EXISTS (SELECT 1 FROM pg_roles r WHERE r.rolname=" + sqlQuote(creator) + " AND NOT r.rolcanlogin AND NOT r.rolinherit AND NOT r.rolsuper AND NOT r.rolcreatedb AND NOT r.rolcreaterole AND NOT r.rolreplication AND NOT r.rolbypassrls AND " + roleComment("r.oid", postgresCatalogProtocolCreatorMarker(x, db)) + ")"
		}
		memberPairs, scopedRoles := []string{}, []string{}
		for _, db := range postgresCatalogProtocolDatabases(append(append([]hostcontract.LocalDataClient{}, e.Desired...), e.Previous...)) {
			scopedRoles = append(scopedRoles, sqlQuote(postgresOwner(e.Binding.Service, db)))
			scopedRoles = append(scopedRoles, sqlQuote(postgresCatalogProtocolCreator(e, db)))
			if e.PreviousRevision != "" {
				scopedRoles = append(scopedRoles, sqlQuote(postgresCatalogProtocolCreator(postgresCatalogProtocolPreviousExpected(e), db)))
			}
		}
		for _, u := range postgresCatalogProtocolUsernames(append(append([]hostcontract.LocalDataClient{}, e.Desired...), e.Previous...)) {
			scopedRoles = append(scopedRoles, sqlQuote(u))
		}
		for _, u := range postgresCatalogProtocolUsernames(x.Desired) {
			clients += " AND EXISTS (SELECT 1 FROM pg_roles r WHERE r.rolname=" + sqlQuote(u) + " AND r.rolcanlogin AND NOT r.rolinherit AND NOT r.rolsuper AND NOT r.rolcreatedb AND NOT r.rolcreaterole AND NOT r.rolreplication AND NOT r.rolbypassrls AND " + roleComment("r.oid", postgresCatalogProtocolClientMarker(x, u)) + ")"
			for _, db := range postgresCatalogProtocolUserDatabases(x.Desired, u) {
				memberships += " AND EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles o ON o.oid=m.roleid JOIN pg_roles c ON c.oid=m.member WHERE o.rolname=" + sqlQuote(postgresOwner(x.Binding.Service, db)) + " AND c.rolname=" + sqlQuote(u) + " AND NOT m.admin_option AND NOT m.inherit_option AND m.set_option)"
				memberPairs = append(memberPairs, "(o.rolname="+sqlQuote(postgresOwner(x.Binding.Service, db))+" AND c.rolname="+sqlQuote(u)+" AND NOT m.admin_option AND NOT m.inherit_option AND m.set_option)")
			}
		}
		if len(scopedRoles) != 0 {
			allowed := "FALSE"
			if len(memberPairs) != 0 {
				allowed = strings.Join(memberPairs, " OR ")
			}
			memberships += " AND NOT EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles o ON o.oid=m.roleid JOIN pg_roles c ON c.oid=m.member WHERE (o.rolname IN (" + strings.Join(scopedRoles, ",") + ") OR c.rolname IN (" + strings.Join(scopedRoles, ",") + ")) AND NOT (" + allowed + "))"
		}
		memberships += " AND NOT EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.member=(SELECT oid FROM pg_roles WHERE rolname='s2h_admin') OR m.roleid=(SELECT oid FROM pg_roles WHERE rolname='s2h_admin'))"
		return owners, clients, creators, memberships, settings
	}
	owners, clients, creators, memberships, old := structure(e)
	priorOwners, priorClients, priorCreators, priorMemberships, priorOld := structure(postgresCatalogProtocolPreviousExpected(e))
	retired, priorRetired := "TRUE", "TRUE"
	for _, u := range postgresCatalogProtocolPreviousOnlyUsers(e) {
		retired += " AND EXISTS (SELECT 1 FROM pg_roles r WHERE r.rolname=" + sqlQuote(u) + " AND NOT r.rolcanlogin)"
		for _, db := range postgresCatalogProtocolDatabases(e.Previous) {
			retired += " AND NOT EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles o ON o.oid=m.roleid JOIN pg_roles c ON c.oid=m.member WHERE o.rolname=" + sqlQuote(postgresOwner(e.Binding.Service, db)) + " AND c.rolname=" + sqlQuote(u) + ")"
		}
		old += " AND NOT EXISTS (SELECT 1 FROM pg_db_role_setting s JOIN pg_roles r ON r.oid=s.setrole WHERE r.rolname=" + sqlQuote(u) + ")"
	}
	for _, u := range postgresCatalogProtocolUsernames(e.Previous) {
		for _, db := range postgresCatalogProtocolUserDatabases(e.Previous, u) {
			if containsPostgresCatalogProtocolDatabase(postgresCatalogProtocolUserDatabases(e.Desired, u), db) {
				continue
			}
			owner := postgresOwner(e.Binding.Service, db)
			memberships += " AND NOT EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles o ON o.oid=m.roleid JOIN pg_roles c ON c.oid=m.member WHERE o.rolname=" + sqlQuote(owner) + " AND c.rolname=" + sqlQuote(u) + ")"
			old += " AND NOT EXISTS (SELECT 1 FROM pg_db_role_setting s JOIN pg_roles r ON r.oid=s.setrole JOIN pg_database d ON d.oid=s.setdatabase WHERE r.rolname=" + sqlQuote(u) + " AND d.datname=" + sqlQuote(db) + ")"
		}
	}
	bindings := postgresCatalogProtocolRoleMarkerBindings(e)
	roleNames := make([]string, 0, len(bindings))
	foreignClauses, managedNames := []string{}, []string{}
	for _, binding := range bindings {
		roleNames = append(roleNames, sqlQuote(binding.Name))
		if binding.Name != "s2h_admin" {
			managedNames = append(managedNames, sqlQuote(binding.Name))
		}
		markers := make([]string, len(binding.Markers))
		for i, marker := range binding.Markers {
			markers[i] = sqlQuote(marker)
		}
		unmarked := "FALSE"
		if binding.Name != "s2h_admin" {
			unmarked = "NOT EXISTS (SELECT 1 FROM pg_shdescription d WHERE d.objoid=x.oid AND d.classoid='pg_authid'::regclass)"
		}
		foreignClauses = append(foreignClauses, "EXISTS (SELECT 1 FROM pg_roles x WHERE x.rolname="+sqlQuote(binding.Name)+" AND (EXISTS (SELECT 1 FROM pg_shdescription d WHERE d.objoid=x.oid AND d.classoid='pg_authid'::regclass AND d.description NOT IN ("+strings.Join(markers, ",")+")) OR "+unmarked+"))")
	}
	foreign := strings.Join(foreignClauses, " OR ")
	admin := "EXISTS (SELECT 1 FROM pg_roles r WHERE r.rolname='s2h_admin' AND r.rolcanlogin AND r.rolinherit AND r.rolsuper AND r.rolcreatedb AND r.rolcreaterole AND NOT r.rolreplication AND NOT r.rolbypassrls)"
	managedAbsent := "TRUE"
	if len(managedNames) != 0 {
		managedAbsent = "NOT EXISTS (SELECT 1 FROM pg_roles r WHERE r.rolname IN (" + strings.Join(managedNames, ",") + "))"
	}
	initialSafe := admin + " AND " + managedAbsent + " AND NOT EXISTS (SELECT 1 FROM pg_shdescription d JOIN pg_roles r ON r.oid=d.objoid WHERE d.classoid='pg_authid'::regclass AND r.rolname IN (" + strings.Join(roleNames, ",") + ") AND left(d.description,7)='s2hpg2:') AND NOT EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.member=(SELECT oid FROM pg_roles WHERE rolname='s2h_admin') OR m.roleid=(SELECT oid FROM pg_roles WHERE rolname='s2h_admin')) AND NOT EXISTS (SELECT 1 FROM pg_db_role_setting s JOIN pg_roles r ON r.oid=s.setrole WHERE r.rolname='s2h_admin')"
	state := "CASE WHEN " + foreign + " THEN 'foreign' WHEN " + roleComment("r.oid", target) + " THEN 'target' WHEN " + func() string {
		if prior == "" {
			return "FALSE"
		}
		return roleComment("r.oid", prior)
	}() + " THEN 'prior' WHEN EXISTS (SELECT 1 FROM pg_shdescription d WHERE d.objoid=r.oid AND d.classoid='pg_authid'::regclass) THEN 'foreign' ELSE 'absent' END"
	return "SELECT " + postgresCatalogProtocolSQLRecord([]string{"'s2hpg2'", "'1'", "'roles'", "COALESCE((SELECT " + state + " FROM pg_roles r WHERE r.rolname='s2h_admin'),'unavailable')", "COALESCE((SELECT CASE WHEN " + roleComment("r.oid", target) + " THEN " + sqlQuote(postgresCatalogProtocolOperation(e.Revision)) + " WHEN " + func() string {
		if prior == "" {
			return "FALSE"
		}
		return roleComment("r.oid", prior)
	}() + " THEN " + sqlQuote(postgresCatalogProtocolOperation(e.PreviousRevision)) + " ELSE '-' END FROM pg_roles r WHERE r.rolname='s2h_admin'),'-')", bit(admin), bit("CASE WHEN " + func() string {
		if prior != "" {
			return roleComment("r.oid", prior)
		}
		return "FALSE"
	}() + " THEN (" + priorOwners + ") ELSE (" + owners + ") END"), bit("CASE WHEN " + func() string {
		if prior != "" {
			return roleComment("r.oid", prior)
		}
		return "FALSE"
	}() + " THEN (" + priorClients + ") ELSE (" + clients + ") END"), bit("CASE WHEN " + func() string {
		if prior != "" {
			return roleComment("r.oid", prior)
		}
		return "FALSE"
	}() + " THEN (" + priorCreators + ") ELSE (" + creators + ") END"), bit("CASE WHEN " + func() string {
		if prior != "" {
			return roleComment("r.oid", prior)
		}
		return "FALSE"
	}() + " THEN (" + priorMemberships + ") ELSE (" + memberships + ") END"), bit("CASE WHEN " + func() string {
		if prior != "" {
			return roleComment("r.oid", prior)
		}
		return "FALSE"
	}() + " THEN (" + priorRetired + ") ELSE (" + retired + ") END"), bit("CASE WHEN " + func() string {
		if prior != "" {
			return roleComment("r.oid", prior)
		}
		return "FALSE"
	}() + " THEN (" + priorOld + ") ELSE (" + old + ") END"), bit(initialSafe)}) + ";\n"
}
func postgresCatalogProtocolDatabaseReferenceSQL(e postgresCatalogProtocolExpected, db string) string {
	if e.valid() != nil || !validPrincipal(db) {
		return ""
	}
	marker := postgresCatalogProtocolDatabaseMarker(e, db)
	creator := postgresCatalogProtocolCreator(e, db)
	creatorExact := "EXISTS (SELECT 1 FROM pg_roles r WHERE r.rolname=" + sqlQuote(creator) + " AND NOT r.rolcanlogin AND NOT r.rolinherit AND NOT r.rolsuper AND NOT r.rolcreatedb AND NOT r.rolcreaterole AND NOT r.rolreplication AND NOT r.rolbypassrls AND " + roleComment("r.oid", postgresCatalogProtocolCreatorMarker(e, db)) + ")"
	prior := postgresCatalogProtocolPreviousDatabaseMarker(e, db)
	owner := postgresOwner(e.Binding.Service, db)
	priorState := "FALSE"
	priorOp := "'-'"
	if prior != "" {
		priorState = dbComment("d.oid", prior)
		priorOp = sqlQuote(postgresCatalogProtocolOperation(e.PreviousRevision))
	}
	return "SELECT " + postgresCatalogProtocolSQLRecord([]string{"'s2hpg2'", "'1'", "'dbref'", sqlQuote(postgresCatalogProtocolDatabaseToken(db)), "COALESCE((SELECT CASE WHEN " + dbComment("d.oid", marker) + " THEN 'target' WHEN " + priorState + " THEN 'prior' WHEN NOT EXISTS (SELECT 1 FROM pg_shdescription x WHERE x.objoid=d.oid AND x.classoid='pg_database'::regclass) AND d.datdba=(SELECT oid FROM pg_roles WHERE rolname=" + sqlQuote(creator) + ") AND " + creatorExact + " THEN 'create-only' ELSE 'foreign' END FROM pg_database d WHERE d.datname=" + sqlQuote(db) + "),'absent')", "COALESCE((SELECT CASE WHEN " + dbComment("d.oid", marker) + " THEN " + sqlQuote(postgresCatalogProtocolOperation(e.Revision)) + " WHEN " + priorState + " THEN " + priorOp + " ELSE '-' END FROM pg_database d WHERE d.datname=" + sqlQuote(db) + "),'-')", bit("EXISTS (SELECT 1 FROM pg_database d WHERE d.datname=" + sqlQuote(db) + " AND d.datdba=(SELECT oid FROM pg_roles WHERE rolname=" + sqlQuote(owner) + "))"), bit("EXISTS (SELECT 1 FROM pg_database d WHERE d.datname=" + sqlQuote(db) + " AND d.datdba=(SELECT oid FROM pg_roles WHERE rolname=" + sqlQuote(creator) + "))")}) + ";\n"
}
func postgresCatalogProtocolDatabaseDetailSQL(e postgresCatalogProtocolExpected, db string) string {
	if e.valid() != nil || !validPrincipal(db) {
		return ""
	}
	owner := postgresOwner(e.Binding.Service, db)
	creator := postgresCatalogProtocolCreator(e, db)
	creatorExact := "EXISTS (SELECT 1 FROM pg_roles r WHERE r.rolname=" + sqlQuote(creator) + " AND NOT r.rolcanlogin AND NOT r.rolinherit AND NOT r.rolsuper AND NOT r.rolcreatedb AND NOT r.rolcreaterole AND NOT r.rolreplication AND NOT r.rolbypassrls AND " + roleComment("r.oid", postgresCatalogProtocolCreatorMarker(e, db)) + ")"
	m, prior := postgresCatalogProtocolDatabaseMarker(e, db), postgresCatalogProtocolPreviousDatabaseMarker(e, db)
	priorState, priorOp := "FALSE", "'-'"
	if prior != "" {
		priorState, priorOp = dbComment("d.oid", prior), sqlQuote(postgresCatalogProtocolOperation(e.PreviousRevision))
	}
	state := "CASE WHEN " + dbComment("d.oid", m) + " THEN 'target' WHEN " + priorState + " THEN 'prior' WHEN NOT EXISTS (SELECT 1 FROM pg_shdescription x WHERE x.objoid=d.oid AND x.classoid='pg_database'::regclass) AND d.datdba=(SELECT oid FROM pg_roles WHERE rolname=" + sqlQuote(creator) + ") AND " + creatorExact + " THEN 'create-only' ELSE 'foreign' END"
	base := []string{"'s2hpg2'", "'1'", "'dbdetail'", sqlQuote(postgresCatalogProtocolDatabaseToken(db))}
	freshSchema := "EXISTS (SELECT 1 FROM pg_namespace n WHERE n.nspname='public')"
	create := postgresCatalogProtocolSQLRecord(append(base, "'create-only'", "'-'", bit(freshSchema), bit("EXISTS (SELECT 1 FROM pg_database d WHERE d.datname="+sqlQuote(db)+" AND d.datdba=(SELECT oid FROM pg_roles WHERE rolname="+sqlQuote(creator)+"))")))
	settings, connect := "TRUE", "TRUE"
	active := func(x postgresCatalogProtocolExpected) (string, string, string) {
		settings, connect := "TRUE", "TRUE"
		allowedRoles, settingRoles := []string{sqlQuote(owner)}, []string{}
		connect += " AND EXISTS (SELECT 1 FROM aclexplode((SELECT datacl FROM pg_database WHERE datname=" + sqlQuote(db) + ")) a WHERE a.grantee=(SELECT oid FROM pg_roles WHERE rolname=" + sqlQuote(owner) + ") AND a.privilege_type='CONNECT' AND NOT a.is_grantable)"
		for _, username := range postgresCatalogProtocolUsernames(x.Desired) {
			if !containsPostgresCatalogProtocolDatabase(postgresCatalogProtocolUserDatabases(x.Desired, username), db) {
				continue
			}
			allowedRoles = append(allowedRoles, sqlQuote(username))
			settingRoles = append(settingRoles, sqlQuote(username))
			settings += " AND (SELECT setconfig FROM pg_db_role_setting s JOIN pg_roles r ON r.oid=s.setrole JOIN pg_database d ON d.oid=s.setdatabase WHERE r.rolname=" + sqlQuote(username) + " AND d.datname=" + sqlQuote(db) + ")=ARRAY[" + sqlQuote("role="+owner) + "]"
			connect += " AND EXISTS (SELECT 1 FROM aclexplode((SELECT datacl FROM pg_database WHERE datname=" + sqlQuote(db) + ")) a WHERE a.grantee=(SELECT oid FROM pg_roles WHERE rolname=" + sqlQuote(username) + ") AND a.privilege_type='CONNECT' AND NOT a.is_grantable)"
		}
		managedUsers := postgresCatalogProtocolUsernames(append(append([]hostcontract.LocalDataClient{}, e.Desired...), e.Previous...))
		managedRoles := append([]string{sqlQuote(owner)}, allowedRoles[1:]...)
		for _, ownerDB := range postgresCatalogProtocolDatabases(append(append([]hostcontract.LocalDataClient{}, e.Desired...), e.Previous...)) {
			managedRoles = append(managedRoles, sqlQuote(postgresOwner(e.Binding.Service, ownerDB)))
		}
		quotedUsers := make([]string, 0, len(managedUsers))
		for _, username := range managedUsers {
			managedRoles = append(managedRoles, sqlQuote(username))
			quotedUsers = append(quotedUsers, sqlQuote(username))
		}
		allowedSettings := "FALSE"
		if len(settingRoles) != 0 {
			allowedSettings = "r.rolname IN (" + strings.Join(settingRoles, ",") + ") AND s.setconfig=ARRAY[" + sqlQuote("role="+owner) + "]"
		}
		noOwnerSetting := "NOT EXISTS (SELECT 1 FROM pg_db_role_setting s JOIN pg_roles r ON r.oid=s.setrole WHERE r.rolname=" + sqlQuote(owner) + " AND s.setdatabase=(SELECT oid FROM pg_database WHERE datname=" + sqlQuote(db) + "))"
		noExtras := noOwnerSetting + " AND NOT EXISTS (SELECT 1 FROM pg_db_role_setting s JOIN pg_roles r ON r.oid=s.setrole WHERE s.setdatabase=(SELECT oid FROM pg_database WHERE datname=" + sqlQuote(db) + ") AND r.rolname IN (" + strings.Join(managedRoles, ",") + ") AND NOT (" + allowedSettings + ")) AND NOT EXISTS (SELECT 1 FROM aclexplode((SELECT datacl FROM pg_database WHERE datname=" + sqlQuote(db) + ")) a JOIN pg_roles r ON r.oid=a.grantee WHERE r.rolname IN (" + strings.Join(managedRoles, ",") + ") AND (r.rolname NOT IN (" + strings.Join(allowedRoles, ",") + ") OR a.privilege_type<>'CONNECT' OR a.is_grantable))"
		if len(quotedUsers) != 0 {
			noExtras += " AND NOT EXISTS (SELECT 1 FROM pg_namespace n CROSS JOIN LATERAL aclexplode(n.nspacl) a JOIN pg_roles r ON r.oid=a.grantee WHERE n.nspname='public' AND r.rolname IN (" + strings.Join(quotedUsers, ",") + "))"
		}
		return settings, connect, noExtras
	}
	settings, connect, noExtras := active(e)
	priorSettings, priorConnect, priorExtras := active(postgresCatalogProtocolPreviousExpected(e))
	which := func(target, prior string) string {
		return "CASE WHEN " + priorState + " THEN (" + prior + ") ELSE (" + target + ") END"
	}
	schemaOwner := "EXISTS (SELECT 1 FROM pg_namespace n WHERE n.nspname='public' AND n.nspowner=(SELECT oid FROM pg_roles WHERE rolname=" + sqlQuote(owner) + "))"
	publicRevoked := "NOT EXISTS (SELECT 1 FROM pg_namespace n CROSS JOIN LATERAL aclexplode(n.nspacl) a WHERE n.nspname='public' AND a.grantee=0 AND a.privilege_type='CREATE')"
	ownerPrivileges := "EXISTS (SELECT 1 FROM pg_namespace n CROSS JOIN LATERAL aclexplode(n.nspacl) a WHERE n.nspname='public' AND a.grantee=(SELECT oid FROM pg_roles WHERE rolname=" + sqlQuote(owner) + ") AND a.privilege_type IN ('USAGE','CREATE') AND NOT a.is_grantable GROUP BY n.oid HAVING count(*)=2)"
	full := postgresCatalogProtocolSQLRecord(append(base, "COALESCE((SELECT "+state+" FROM pg_database d WHERE d.datname="+sqlQuote(db)+"),'absent')", "COALESCE((SELECT CASE WHEN "+dbComment("d.oid", m)+" THEN "+sqlQuote(postgresCatalogProtocolOperation(e.Revision))+" WHEN "+priorState+" THEN "+priorOp+" ELSE '-' END FROM pg_database d WHERE d.datname="+sqlQuote(db)+"),'-')", bit(schemaOwner), bit(publicRevoked), bit(ownerPrivileges), bit(which(settings, priorSettings)), bit(which(connect, priorConnect)), bit(which(noExtras, priorExtras))))
	return "SELECT CASE WHEN EXISTS (SELECT 1 FROM pg_database d WHERE d.datname=" + sqlQuote(db) + " AND d.datdba=(SELECT oid FROM pg_roles WHERE rolname=" + sqlQuote(creator) + ") AND " + creatorExact + " AND NOT EXISTS (SELECT 1 FROM pg_shdescription x WHERE x.objoid=d.oid AND x.classoid='pg_database'::regclass) AND " + freshSchema + ") THEN " + create + " ELSE " + full + " END;\n"
}

func postgresCatalogProtocolFields(b []byte, kind string, count int) ([]string, error) {
	if len(b) == 0 || len(b) > 512 || b[len(b)-1] != '\n' || bytes.Count(b, []byte{'\n'}) != 1 {
		return nil, errors.New("catalog record")
	}
	line := b[:len(b)-1]
	for _, c := range line {
		if c != '\t' && (c < 0x21 || c > 0x7e) {
			return nil, errors.New("catalog record")
		}
	}
	f := strings.Split(string(line), "\t")
	if len(f) != count || f[0] != "s2hpg2" || f[1] != "1" || f[2] != kind {
		return nil, errors.New("catalog record")
	}
	return f, nil
}
func postgresCatalogProtocolOperationField(v string) (string, error) {
	if v == "-" {
		return v, nil
	}
	if len(v) == 24 && lowerHex(v) {
		return v, nil
	}
	return "", errors.New("operation")
}
func postgresCatalogProtocolBit(v string) (bool, error) {
	if v == "0" {
		return false, nil
	}
	if v == "1" {
		return true, nil
	}
	return false, errors.New("bit")
}
func postgresCatalogProtocolRoleState(v string) (postgresCatalogProtocolState, error) {
	switch v {
	case "target":
		return postgresCatalogProtocolTarget, nil
	case "prior":
		return postgresCatalogProtocolPrior, nil
	case "unavailable":
		return postgresCatalogProtocolUnavailable, nil
	case "foreign":
		return postgresCatalogProtocolForeign, nil
	case "absent":
		return postgresCatalogProtocolAbsent, nil
	}
	return 0, errors.New("role state")
}
func postgresCatalogProtocolDatabaseState(v string) (postgresCatalogProtocolState, error) {
	switch v {
	case "absent":
		return postgresCatalogProtocolAbsent, nil
	case "prior":
		return postgresCatalogProtocolPrior, nil
	case "target":
		return postgresCatalogProtocolTarget, nil
	case "create-only":
		return postgresCatalogProtocolCreateOnly, nil
	case "foreign":
		return postgresCatalogProtocolForeign, nil
	case "unavailable":
		return postgresCatalogProtocolUnavailable, nil
	}
	return 0, errors.New("database state")
}
func validPair(s postgresCatalogProtocolState, op string, prior string) bool {
	switch s {
	case postgresCatalogProtocolTarget:
		return op != "-"
	case postgresCatalogProtocolPrior:
		return op != "-"
	case postgresCatalogProtocolAbsent, postgresCatalogProtocolCreateOnly, postgresCatalogProtocolForeign, postgresCatalogProtocolUnavailable:
		return op == "-"
	}
	return false
}
func parsePostgresCatalogProtocolRoles(b []byte) (postgresCatalogProtocolRoleObservation, error) {
	f, err := postgresCatalogProtocolFields(b, "roles", 13)
	if err != nil {
		return postgresCatalogProtocolRoleObservation{}, err
	}
	s, err := postgresCatalogProtocolRoleState(f[3])
	if err != nil {
		return postgresCatalogProtocolRoleObservation{}, err
	}
	op, err := postgresCatalogProtocolOperationField(f[4])
	if err != nil || !validPair(s, op, "") {
		return postgresCatalogProtocolRoleObservation{}, errors.New("roles pair")
	}
	o := postgresCatalogProtocolRoleObservation{Marker: s, Operation: op}
	bits := []*bool{&o.Admin, &o.Owners, &o.Clients, &o.Creators, &o.Memberships, &o.Retired, &o.OldSettings}
	for i, p := range bits {
		*p, err = postgresCatalogProtocolBit(f[i+5])
		if err != nil {
			return postgresCatalogProtocolRoleObservation{}, err
		}
	}
	o.InitialSafe, err = postgresCatalogProtocolBit(f[12])
	if err != nil {
		return postgresCatalogProtocolRoleObservation{}, err
	}
	return o, nil
}
func parsePostgresCatalogProtocolReference(b []byte) (postgresCatalogProtocolReferenceObservation, error) {
	f, err := postgresCatalogProtocolFields(b, "dbref", 8)
	if err != nil {
		return postgresCatalogProtocolReferenceObservation{}, err
	}
	s, err := postgresCatalogProtocolDatabaseState(f[4])
	if err != nil {
		return postgresCatalogProtocolReferenceObservation{}, err
	}
	op, err := postgresCatalogProtocolOperationField(f[5])
	if err != nil || !validPair(s, op, "") {
		return postgresCatalogProtocolReferenceObservation{}, errors.New("reference pair")
	}
	stableOwner, err := postgresCatalogProtocolBit(f[6])
	creatorOwner, creatorErr := postgresCatalogProtocolBit(f[7])
	if err != nil || creatorErr != nil || len(f[3]) != 24 || !lowerHex(f[3]) {
		return postgresCatalogProtocolReferenceObservation{}, errors.New("reference")
	}
	return postgresCatalogProtocolReferenceObservation{Key: f[3], State: s, Operation: op, StableOwner: stableOwner, CreatorOwner: creatorOwner}, nil
}
func parsePostgresCatalogProtocolDetail(b []byte) (postgresCatalogProtocolDetailObservation, error) {
	f0, err := postgresCatalogProtocolFields(b, "dbdetail", 8)
	if err == nil && f0[4] == "create-only" {
		op, _ := postgresCatalogProtocolOperationField(f0[5])
		fresh, e1 := postgresCatalogProtocolBit(f0[6])
		owner, e2 := postgresCatalogProtocolBit(f0[7])
		if e1 != nil || e2 != nil || op != "-" || len(f0[3]) != 24 || !lowerHex(f0[3]) {
			return postgresCatalogProtocolDetailObservation{}, errors.New("create detail")
		}
		return postgresCatalogProtocolDetailObservation{Key: f0[3], State: postgresCatalogProtocolCreateOnly, Operation: op, Fresh: fresh, Owner: owner}, nil
	}
	f, err := postgresCatalogProtocolFields(b, "dbdetail", 12)
	if err != nil {
		return postgresCatalogProtocolDetailObservation{}, err
	}
	s, err := postgresCatalogProtocolDatabaseState(f[4])
	if err != nil {
		return postgresCatalogProtocolDetailObservation{}, err
	}
	op, err := postgresCatalogProtocolOperationField(f[5])
	if err != nil || !validPair(s, op, "") || s == postgresCatalogProtocolCreateOnly || len(f[3]) != 24 || !lowerHex(f[3]) {
		return postgresCatalogProtocolDetailObservation{}, errors.New("detail pair")
	}
	o := postgresCatalogProtocolDetailObservation{Key: f[3], State: s, Operation: op}
	bits := []*bool{&o.SchemaOwner, &o.PublicCreateRevoked, &o.OwnerUsageCreate, &o.Settings, &o.Connect, &o.NoScopedExtras}
	for i, p := range bits {
		*p, err = postgresCatalogProtocolBit(f[i+6])
		if err != nil {
			return postgresCatalogProtocolDetailObservation{}, err
		}
	}
	return o, nil
}
func postgresCatalogProtocolAbsentDetail(r postgresCatalogProtocolReferenceObservation) (postgresCatalogProtocolDetailObservation, error) {
	if r.State != postgresCatalogProtocolAbsent || r.Operation != "-" || len(r.Key) != 24 || !lowerHex(r.Key) {
		return postgresCatalogProtocolDetailObservation{}, errors.New("absent detail")
	}
	return postgresCatalogProtocolDetailObservation{Key: r.Key, State: postgresCatalogProtocolAbsent, Operation: "-"}, nil
}

func classifyPostgresCatalogProtocol(e postgresCatalogProtocolExpected, roles postgresCatalogProtocolRoleObservation, dbs []postgresCatalogProtocolDatabaseObservation, pending bool) postgresCatalogProtocolClassification {
	if e.valid() != nil || roles.Marker == postgresCatalogProtocolUnavailable {
		return postgresCatalogProtocolClassification{State: postgresCatalogProtocolUnavailable}
	}
	for _, d := range dbs {
		if d.Reference.State == postgresCatalogProtocolUnavailable || d.Detail.State == postgresCatalogProtocolUnavailable {
			return postgresCatalogProtocolClassification{State: postgresCatalogProtocolUnavailable}
		}
		if d.Reference.State == postgresCatalogProtocolForeign || d.Detail.State == postgresCatalogProtocolForeign {
			return postgresCatalogProtocolClassification{State: postgresCatalogProtocolForeign}
		}
	}
	if roles.Marker == postgresCatalogProtocolForeign {
		return postgresCatalogProtocolClassification{State: postgresCatalogProtocolForeign}
	}
	keys := postgresCatalogProtocolDatabaseTokens(e.Desired)
	if len(dbs) != len(keys) {
		return postgresCatalogProtocolClassification{State: postgresCatalogProtocolUnavailable}
	}
	target, prior := postgresCatalogProtocolOperation(e.Revision), postgresCatalogProtocolOperation(e.PreviousRevision)
	roleTarget := roles.Marker == postgresCatalogProtocolTarget && roles.Operation == target && roles.StructuralExact()
	rolePrior := roles.Marker == postgresCatalogProtocolPrior && roles.Operation == prior && roles.StructuralExact()
	initial := e.PreviousRevision == "" && roles.Marker == postgresCatalogProtocolAbsent && roles.Operation == "-" && roles.InitialSafe
	if !roleTarget && !rolePrior && !initial {
		return postgresCatalogProtocolClassification{State: postgresCatalogProtocolMixed}
	}
	phase := 0
	allTarget, allPrior := roleTarget, rolePrior || initial
	for i, d := range dbs {
		if d.Key != keys[i] || d.Reference.Key != d.Key || d.Detail.Key != d.Key {
			return postgresCatalogProtocolClassification{State: postgresCatalogProtocolMixed}
		}
		exact := d.Reference.State == postgresCatalogProtocolTarget && d.Reference.Operation == target && d.Reference.StableOwner && !d.Reference.CreatorOwner && d.Detail.State == postgresCatalogProtocolTarget && d.Detail.Operation == target && d.Detail.StructuralExact()
		old := d.Reference.State == postgresCatalogProtocolPrior && d.Reference.Operation == prior && d.Reference.StableOwner && !d.Reference.CreatorOwner && d.Detail.State == postgresCatalogProtocolPrior && d.Detail.Operation == prior && d.Detail.StructuralExact() || d.Reference.State == postgresCatalogProtocolAbsent && d.Detail.State == postgresCatalogProtocolAbsent
		create := d.Reference.State == postgresCatalogProtocolCreateOnly && d.Reference.Operation == "-" && !d.Reference.StableOwner && d.Reference.CreatorOwner && d.Detail.State == postgresCatalogProtocolCreateOnly && d.Detail.Operation == "-" && d.Detail.CreateOnlyExact()
		allTarget = allTarget && exact
		allPrior = allPrior && old
		if (rolePrior || initial) && old {
			continue
		}
		if phase == 0 && exact {
			continue
		}
		if phase == 0 && create && pending {
			phase = 1
			continue
		}
		if (phase == 0 || phase == 1 || phase == 2) && old && roleTarget {
			phase = 2
			continue
		}
		return postgresCatalogProtocolClassification{State: postgresCatalogProtocolMixed}
	}
	if allTarget {
		return postgresCatalogProtocolClassification{State: postgresCatalogProtocolExact}
	}
	if allPrior {
		return postgresCatalogProtocolClassification{State: postgresCatalogProtocolPrior}
	}
	if phase > 0 && roleTarget && pending {
		return postgresCatalogProtocolClassification{State: postgresCatalogProtocolPartial, Pending: true}
	}
	return postgresCatalogProtocolClassification{State: postgresCatalogProtocolMixed}
}
func postgresCatalogProtocolDatabases(cs []hostcontract.LocalDataClient) []string {
	m := map[string]bool{}
	for _, c := range cs {
		m[c.Database] = true
	}
	r := []string{}
	for v := range m {
		r = append(r, v)
	}
	sort.Strings(r)
	return r
}
func postgresCatalogProtocolDatabaseTokens(cs []hostcontract.LocalDataClient) []string {
	values := postgresCatalogProtocolDatabases(cs)
	for i := range values {
		values[i] = postgresCatalogProtocolDatabaseToken(values[i])
	}
	return values
}
func postgresCatalogProtocolUsernames(cs []hostcontract.LocalDataClient) []string {
	m := map[string]bool{}
	for _, c := range cs {
		m[c.Username] = true
	}
	r := []string{}
	for v := range m {
		r = append(r, v)
	}
	sort.Strings(r)
	return r
}
func postgresCatalogProtocolUserDatabases(cs []hostcontract.LocalDataClient, u string) []string {
	m := map[string]bool{}
	for _, c := range cs {
		if c.Username == u {
			m[c.Database] = true
		}
	}
	r := []string{}
	for v := range m {
		r = append(r, v)
	}
	sort.Strings(r)
	return r
}
func containsPostgresCatalogProtocolDatabase(dbs []string, db string) bool {
	for _, candidate := range dbs {
		if candidate == db {
			return true
		}
	}
	return false
}
func postgresCatalogProtocolPreviousOnlyUsers(e postgresCatalogProtocolExpected) []string {
	desired := map[string]bool{}
	for _, c := range e.Desired {
		desired[c.Username] = true
	}
	m := map[string]bool{}
	for _, c := range e.Previous {
		if !desired[c.Username] {
			m[c.Username] = true
		}
	}
	r := []string{}
	for u := range m {
		r = append(r, u)
	}
	sort.Strings(r)
	return r
}

// postgresCatalogProtocolWriterHandoff is the non-secret mutation contract that
// reconcile must preserve when it consumes this pure catalog protocol.
func postgresCatalogProtocolWriterHandoff(e postgresCatalogProtocolExpected) []string {
	if e.valid() != nil {
		return nil
	}
	prior := postgresCatalogProtocolPreviousExpected(e)
	desiredDBs, previousDBs := postgresCatalogProtocolDatabases(e.Desired), postgresCatalogProtocolDatabases(e.Previous)
	dbs := postgresCatalogProtocolDatabases(append(append([]hostcontract.LocalDataClient{}, e.Desired...), e.Previous...))
	users := postgresCatalogProtocolUsernames(append(append([]hostcontract.LocalDataClient{}, e.Desired...), e.Previous...))
	values := []string{
		"BEGIN",
		"ENSURE ROLE s2h_admin WHEN ABSENT",
		"ALTER ROLE s2h_admin SUPERUSER CREATEDB CREATEROLE LOGIN INHERIT NOREPLICATION NOBYPASSRLS",
	}
	for _, db := range dbs {
		owner := postgresOwner(e.Binding.Service, db)
		marker := postgresCatalogProtocolOwnerMarker(e, db)
		if !containsPostgresCatalogProtocolDatabase(desiredDBs, db) {
			marker = postgresCatalogProtocolOwnerMarker(prior, db)
		}
		values = append(values, "ENSURE ROLE "+owner+" WHEN ABSENT", "ALTER ROLE "+owner+" NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS", "COMMENT ON ROLE "+owner+" IS "+marker)
	}
	for _, db := range desiredDBs {
		creator := postgresCatalogProtocolCreator(e, db)
		values = append(values, "ENSURE ROLE "+creator+" WHEN ABSENT", "ALTER ROLE "+creator+" NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS", "COMMENT ON ROLE "+creator+" IS "+postgresCatalogProtocolCreatorMarker(e, db))
	}
	for _, db := range previousDBs {
		creator := postgresCatalogProtocolCreator(prior, db)
		values = append(values, "ENSURE ROLE "+creator+" WHEN ABSENT", "ALTER ROLE "+creator+" NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS", "COMMENT ON ROLE "+creator+" IS "+postgresCatalogProtocolCreatorMarker(prior, db))
	}
	for _, username := range users {
		marker := postgresCatalogProtocolClientMarker(e, username)
		if len(postgresCatalogProtocolUserDatabases(e.Desired, username)) == 0 {
			marker = postgresCatalogProtocolClientMarker(prior, username)
		}
		values = append(values, "ENSURE ROLE "+username+" WHEN ABSENT", "ALTER ROLE "+username+" LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS", "COMMENT ON ROLE "+username+" IS "+marker)
	}
	values = append(values,
		"GRANT <desired owner memberships> WITH ADMIN FALSE INHERIT FALSE SET TRUE",
		"REVOKE <removed or moved owner> FROM <client>",
		"RESET obsolete client role settings",
		"ALTER ROLE <removed client> NOLOGIN",
		"COMMENT ON ROLE s2h_admin IS "+postgresCatalogProtocolRolesMarker(e),
		"COMMIT",
	)
	for _, db := range desiredDBs {
		owner := postgresOwner(e.Binding.Service, db)
		values = append(values,
			"CREATE DATABASE "+db+" OWNER "+postgresCatalogProtocolCreator(e, db)+" WHEN ABSENT",
			"BEGIN",
			"ALTER DATABASE "+db+" OWNER TO "+owner,
			"ALTER SCHEMA public OWNER TO "+owner,
			"REVOKE CREATE ON SCHEMA public FROM PUBLIC",
			"GRANT USAGE, CREATE ON SCHEMA public TO "+owner,
			"GRANT owner and desired clients CONNECT ON DATABASE "+db,
			"REVOKE unintended managed CONNECT ON DATABASE "+db,
			"REVOKE unintended managed schema grants ON SCHEMA public",
			"SET ROLE for each desired client IN DATABASE "+db+" TO "+owner,
			"COMMENT ON DATABASE "+db+" IS "+postgresCatalogProtocolDatabaseMarker(e, db),
			"COMMIT",
		)
	}
	return values
}
