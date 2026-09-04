package hostruntime

import (
	"regexp"
	"strings"
	"testing"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
)

func TestPostgresCatalogProtocolExpectedAllowsOverlapHyphensAndSharedUsers(t *testing.T) {
	e := postgresCatalogProtocolFixture(t)
	if err := e.valid(); err != nil {
		t.Fatal(err)
	}
	if postgresCatalogProtocolClientMarker(e, "shared_user") != postgresCatalogProtocolClientMarker(e, "shared_user") {
		t.Fatal("shared username marker is not stable")
	}
	reordered := e
	reordered.Desired = []hostcontract.LocalDataClient{e.Desired[2], e.Desired[1], e.Desired[0]}
	if postgresCatalogProtocolClientMarker(e, "shared_user") != postgresCatalogProtocolClientMarker(reordered, "shared_user") {
		t.Fatal("shared username marker changed when apps were reordered")
	}
	prior := e
	prior.Revision, prior.Desired = e.PreviousRevision, e.Previous
	if postgresCatalogProtocolClientMarker(e, "shared_user") == postgresCatalogProtocolClientMarker(prior, "shared_user") {
		t.Fatal("changed shared username app set did not produce prior marker")
	}
	bad := e
	bad.Desired = append(bad.Desired, hostcontract.LocalDataClient{AppID: "api-one", Username: "other_user", Database: "other_db"})
	if err := bad.valid(); err == nil {
		t.Fatal("duplicate desired App ID accepted")
	}
}

func TestPostgresCatalogProtocolSQLUsesExactCatalogObjectsAndBooleanBits(t *testing.T) {
	e := postgresCatalogProtocolFixture(t)
	queries := []string{postgresCatalogProtocolRolesSQL(e), postgresCatalogProtocolDatabaseReferenceSQL(e, "app_db"), postgresCatalogProtocolDatabaseDetailSQL(e, "app_db")}
	for _, query := range queries {
		if query == "" || !strings.Contains(query, "s2hpg2") {
			t.Fatalf("missing protocol query: %q", query)
		}
		for _, forbidden := range []string{"verifier", "password", "marker table", "pg_shadow", "description LIKE 's2hpg2:%'"} {
			if strings.Contains(strings.ToLower(query), forbidden) {
				t.Fatalf("query contains %q: %s", forbidden, query)
			}
		}
		if strings.Contains(query, "::int") || !strings.Contains(query, "CASE WHEN") {
			t.Fatalf("boolean output is not explicitly 0/1: %s", query)
		}
		if !strings.Contains(query, "pg_shdescription") || strings.Contains(query, "shobj_description") || strings.Contains(query, "E'\\t\\t'") {
			t.Fatalf("query does not use exact catalog framing: %s", query)
		}
		allowed := map[string]bool{"pg_roles": true, "pg_auth_members": true, "pg_db_role_setting": true, "pg_database": true, "pg_database_owner": true, "pg_namespace": true, "pg_authid": true, "pg_shdescription": true, "has_database_privilege": true, "has_schema_privilege": true}
		for _, name := range regexp.MustCompile(`\b(?:pg_[a-z_]+|has_[a-z_]+)\b`).FindAllString(query, -1) {
			if !allowed[name] {
				t.Fatalf("query uses catalog outside allowlist %q: %s", name, query)
			}
		}
	}
	if !strings.Contains(queries[0], "classoid='pg_authid'::regclass") || !strings.Contains(queries[1], "classoid='pg_database'::regclass") {
		t.Fatal("comments are not bound to their catalog class")
	}
	if !strings.Contains(queries[0], "NOT r.rolinherit") || !strings.Contains(queries[0], "NOT m.admin_option AND NOT m.inherit_option AND m.set_option") {
		t.Fatal("role predicates are not exact")
	}
	if !strings.Contains(queries[2], "setconfig") || !strings.Contains(queries[2], "ARRAY[") || !strings.Contains(queries[2], "aclexplode") {
		t.Fatal("database detail does not use exact settings and grants")
	}
}

func TestPostgresCatalogProtocolParsesGeneratedShapeAndRejectsImpossiblePairs(t *testing.T) {
	e := postgresCatalogProtocolFixture(t)
	op := postgresCatalogProtocolOperation(e.Revision)
	key := postgresCatalogProtocolDatabaseToken("app_db")
	roles, err := parsePostgresCatalogProtocolRoles([]byte("s2hpg2\t1\troles\ttarget\t" + op + "\t1\t1\t1\t1\t1\t1\t1\t0\n"))
	if err != nil || !roles.StructuralExact() {
		t.Fatalf("roles = %#v, %v", roles, err)
	}
	ref, err := parsePostgresCatalogProtocolReference([]byte("s2hpg2\t1\tdbref\t" + key + "\ttarget\t" + op + "\t1\t0\n"))
	if err != nil || ref.Key != key {
		t.Fatalf("reference = %#v, %v", ref, err)
	}
	detail, err := parsePostgresCatalogProtocolDetail([]byte("s2hpg2\t1\tdbdetail\t" + key + "\tcreate-only\t-\t1\t1\n"))
	if err != nil || !detail.CreateOnlyExact() {
		t.Fatalf("detail = %#v, %v", detail, err)
	}
	detail, err = parsePostgresCatalogProtocolDetail([]byte("s2hpg2\t1\tdbdetail\t" + key + "\ttarget\t" + op + "\t1\t1\t1\t1\t1\t1\n"))
	if err != nil || !detail.StructuralExact() {
		t.Fatalf("full detail = %#v, %v", detail, err)
	}
	for _, record := range [][]byte{
		[]byte("s2hpg2\t1\troles\ttarget\t-\t1\t1\t1\t1\t1\t1\t1\t0\n"),
		[]byte("s2hpg2\t1\tdbref\t" + key + "\tprior\t-\t1\t0\n"),
		[]byte("s2hpg2\t1\tdbdetail\t" + key + "\tcreate-only\t" + op + "\t1\t1\n"),
		[]byte("s2hpg2\t1\tdbdetail\t" + key + "\tabsent\t-\t1\t1\n"),
	} {
		if strings.Contains(string(record), "roles") {
			_, err = parsePostgresCatalogProtocolRoles(record)
		} else if strings.Contains(string(record), "dbref") {
			_, err = parsePostgresCatalogProtocolReference(record)
		} else {
			_, err = parsePostgresCatalogProtocolDetail(record)
		}
		if err == nil {
			t.Fatalf("accepted impossible record %q", record)
		}
	}
}

func TestPostgresCatalogProtocolRecordAssemblyHasExactFieldCounts(t *testing.T) {
	e := postgresCatalogProtocolFixture(t)
	op, key := postgresCatalogProtocolOperation(e.Revision), postgresCatalogProtocolDatabaseToken("app_db")
	compact := postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "dbdetail", key, "create-only", "-", "1", "1"})
	full := postgresCatalogProtocolRecord([]string{"s2hpg2", "1", "dbdetail", key, "target", op, "1", "1", "1", "1", "1", "1"})
	if _, err := parsePostgresCatalogProtocolDetail(compact); err != nil {
		t.Fatalf("compact generated record: %v", err)
	}
	if _, err := parsePostgresCatalogProtocolDetail(full); err != nil {
		t.Fatalf("full generated record: %v", err)
	}
	if strings.Count(string(compact), "\t") != 7 || strings.Count(string(full), "\t") != 11 {
		t.Fatalf("record separators compact=%q full=%q", compact, full)
	}
}

func TestPostgresCatalogProtocolStrictFramingAndAbsentDetail(t *testing.T) {
	e := postgresCatalogProtocolFixture(t)
	op, key := postgresCatalogProtocolOperation(e.Revision), postgresCatalogProtocolDatabaseToken("app_db")
	good := []byte("s2hpg2\t1\troles\ttarget\t" + op + "\t1\t1\t1\t1\t1\t1\t1\t0\n")
	for _, bad := range [][]byte{
		append(append([]byte{}, good...), '\n'), append([]byte(" "), good...), []byte(strings.Repeat("x", 513)), []byte("s2hpg2\t1\troles\ttarget\t" + op + "\t1\t1\t1\t1\t1\t1\t1\t0\r\n"), []byte("s2hpg2\t1\troles\ttarget\t" + op + "\t1\t1\t1\t1\t1\t1\t1\t0\x00\n"), []byte("s2hpg2\t1\troles\ttarget\t" + op + "\t1\t1\t1\t1\t1\t1\t1\t0\t0\n"), []byte("s2hpg2\t1\troles\ttarget\t\t1\t1\t1\t1\t1\t1\t1\t0\n"), []byte("s2hpg2\t1\tdbref\t\ttarget\t" + op + "\t1\t0\n"),
	} {
		if strings.Contains(string(bad), "\troles\t") {
			if _, err := parsePostgresCatalogProtocolRoles(bad); err == nil {
				t.Fatalf("accepted %q", bad)
			}
		} else if _, err := parsePostgresCatalogProtocolReference(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	ref := postgresCatalogProtocolReferenceObservation{Key: key, State: postgresCatalogProtocolAbsent, Operation: "-"}
	detail, err := postgresCatalogProtocolAbsentDetail(ref)
	if err != nil || detail.Key != key || detail.State != postgresCatalogProtocolAbsent || detail.Operation != "-" {
		t.Fatalf("absent detail = %#v, %v", detail, err)
	}
	if _, err := postgresCatalogProtocolAbsentDetail(postgresCatalogProtocolReferenceObservation{Key: key, State: postgresCatalogProtocolTarget, Operation: op}); err == nil {
		t.Fatal("synthesized detail without absent reference")
	}
}

func TestPostgresCatalogProtocolClassifierRequiresOrderedKeyedPrefix(t *testing.T) {
	e := postgresCatalogProtocolFixture(t)
	op, old := postgresCatalogProtocolOperation(e.Revision), postgresCatalogProtocolOperation(e.PreviousRevision)
	exactRoles := postgresCatalogProtocolRoleObservation{Marker: postgresCatalogProtocolTarget, Operation: op, Admin: true, Owners: true, Clients: true, Creators: true, Memberships: true, Retired: true, OldSettings: true}
	priorRoles := exactRoles
	priorRoles.Marker, priorRoles.Operation = postgresCatalogProtocolPrior, old
	exact := func(db string) postgresCatalogProtocolDatabaseObservation {
		k := postgresCatalogProtocolDatabaseToken(db)
		return postgresCatalogProtocolDatabaseObservation{Key: k, Reference: postgresCatalogProtocolReferenceObservation{Key: k, State: postgresCatalogProtocolTarget, Operation: op, StableOwner: true}, Detail: postgresCatalogProtocolDetailObservation{Key: k, State: postgresCatalogProtocolTarget, Operation: op, SchemaOwner: true, PublicCreateRevoked: true, OwnerUsageCreate: true, Settings: true, Connect: true, NoScopedExtras: true}}
	}
	prior := func(db string) postgresCatalogProtocolDatabaseObservation {
		v := exact(db)
		v.Reference.State, v.Reference.Operation = postgresCatalogProtocolPrior, old
		v.Detail.State, v.Detail.Operation = postgresCatalogProtocolPrior, old
		return v
	}
	create := func(db string) postgresCatalogProtocolDatabaseObservation {
		v := exact(db)
		v.Reference.State, v.Reference.Operation = postgresCatalogProtocolCreateOnly, "-"
		v.Reference.StableOwner, v.Reference.CreatorOwner = false, true
		v.Detail.State, v.Detail.Operation = postgresCatalogProtocolCreateOnly, "-"
		v.Detail = postgresCatalogProtocolDetailObservation{Key: v.Key, State: postgresCatalogProtocolCreateOnly, Operation: "-", Fresh: true, Owner: true}
		return v
	}
	dbs := []postgresCatalogProtocolDatabaseObservation{exact("app_db"), exact("jobs_db")}
	if got := classifyPostgresCatalogProtocol(e, exactRoles, dbs, false); got.State != postgresCatalogProtocolExact || got.Pending {
		t.Fatalf("exact = %#v", got)
	}
	if got := classifyPostgresCatalogProtocol(e, priorRoles, []postgresCatalogProtocolDatabaseObservation{prior("app_db"), prior("jobs_db")}, false); got.State != postgresCatalogProtocolPrior {
		t.Fatalf("prior = %#v", got)
	}
	initial := postgresCatalogProtocolRoleObservation{Marker: postgresCatalogProtocolAbsent, Operation: "-", InitialSafe: true}
	emptyPrevious := e
	emptyPrevious.Previous, emptyPrevious.PreviousRevision = nil, ""
	if got := classifyPostgresCatalogProtocol(emptyPrevious, initial, []postgresCatalogProtocolDatabaseObservation{{Key: postgresCatalogProtocolDatabaseToken("app_db"), Reference: postgresCatalogProtocolReferenceObservation{Key: postgresCatalogProtocolDatabaseToken("app_db"), State: postgresCatalogProtocolAbsent, Operation: "-"}, Detail: postgresCatalogProtocolDetailObservation{Key: postgresCatalogProtocolDatabaseToken("app_db"), State: postgresCatalogProtocolAbsent, Operation: "-"}}, {Key: postgresCatalogProtocolDatabaseToken("jobs_db"), Reference: postgresCatalogProtocolReferenceObservation{Key: postgresCatalogProtocolDatabaseToken("jobs_db"), State: postgresCatalogProtocolAbsent, Operation: "-"}, Detail: postgresCatalogProtocolDetailObservation{Key: postgresCatalogProtocolDatabaseToken("jobs_db"), State: postgresCatalogProtocolAbsent, Operation: "-"}}}, false); got.State != postgresCatalogProtocolPrior {
		t.Fatalf("initial = %#v", got)
	}
	if got := classifyPostgresCatalogProtocol(e, exactRoles, []postgresCatalogProtocolDatabaseObservation{prior("app_db"), prior("jobs_db")}, true); got.State != postgresCatalogProtocolPartial || !got.Pending {
		t.Fatalf("two old suffix = %#v", got)
	}
	if got := classifyPostgresCatalogProtocol(e, exactRoles, []postgresCatalogProtocolDatabaseObservation{exact("app_db"), create("jobs_db")}, true); got.State != postgresCatalogProtocolPartial || !got.Pending {
		t.Fatalf("partial = %#v", got)
	}
	three := e
	three.Desired = append(append([]hostcontract.LocalDataClient{}, e.Desired...), hostcontract.LocalDataClient{AppID: "archive-app", Username: "archive_user", Database: "z_archive_db"})
	if got := classifyPostgresCatalogProtocol(three, exactRoles, []postgresCatalogProtocolDatabaseObservation{exact("app_db"), create("jobs_db"), prior("z_archive_db")}, true); got.State != postgresCatalogProtocolPartial || !got.Pending {
		t.Fatalf("exact/create-only/prior suffix = %#v", got)
	}
	if got := classifyPostgresCatalogProtocol(three, exactRoles, []postgresCatalogProtocolDatabaseObservation{exact("app_db"), create("jobs_db"), exact("z_archive_db")}, true); got.State != postgresCatalogProtocolMixed {
		t.Fatalf("exact after create-only = %#v", got)
	}
	for _, dbs := range [][]postgresCatalogProtocolDatabaseObservation{{exact("jobs_db"), exact("app_db")}, {exact("app_db"), exact("app_db")}, {prior("app_db"), create("jobs_db")}, {create("app_db"), create("jobs_db")}, {create("app_db"), exact("jobs_db")}} {
		if got := classifyPostgresCatalogProtocol(e, exactRoles, dbs, true); got.State != postgresCatalogProtocolMixed {
			t.Fatalf("bad sequence = %#v", got)
		}
	}
	foreign := exact("app_db")
	foreign.Detail.State = postgresCatalogProtocolForeign
	if got := classifyPostgresCatalogProtocol(e, exactRoles, []postgresCatalogProtocolDatabaseObservation{foreign, exact("jobs_db")}, false); got.State != postgresCatalogProtocolForeign {
		t.Fatalf("foreign = %#v", got)
	}
	unavailable := exact("app_db")
	unavailable.Reference.State = postgresCatalogProtocolUnavailable
	if got := classifyPostgresCatalogProtocol(e, exactRoles, []postgresCatalogProtocolDatabaseObservation{unavailable, exact("jobs_db")}, false); got.State != postgresCatalogProtocolUnavailable {
		t.Fatalf("unavailable = %#v", got)
	}
	foreignRoles := exactRoles
	foreignRoles.Marker = postgresCatalogProtocolForeign
	if got := classifyPostgresCatalogProtocol(e, foreignRoles, dbs, false); got.State != postgresCatalogProtocolForeign {
		t.Fatalf("role foreign = %#v", got)
	}
	unavailableRoles := exactRoles
	unavailableRoles.Marker = postgresCatalogProtocolUnavailable
	if got := classifyPostgresCatalogProtocol(e, unavailableRoles, dbs, false); got.State != postgresCatalogProtocolUnavailable {
		t.Fatalf("role unavailable = %#v", got)
	}
}

func TestPostgresCatalogProtocolSQLBindsEveryDescriptionAndProjectsTopologies(t *testing.T) {
	e := postgresCatalogProtocolFixture(t)
	for _, query := range []string{postgresCatalogProtocolRolesSQL(e), postgresCatalogProtocolDatabaseReferenceSQL(e, "app_db"), postgresCatalogProtocolDatabaseDetailSQL(e, "app_db")} {
		for _, access := range strings.Split(query, "pg_shdescription")[1:] {
			if !strings.Contains(access, "classoid=") {
				t.Fatalf("unbound pg_shdescription access: %s", query)
			}
		}
	}
	roles := postgresCatalogProtocolRolesSQL(e)
	for _, required := range []string{"NOT m.admin_option AND NOT m.inherit_option AND m.set_option", "NOT EXISTS (SELECT 1 FROM pg_auth_members", "r.rolname='s2h_admin'", "InitialSafe"} {
		if required == "InitialSafe" {
			continue
		}
		if !strings.Contains(roles, required) {
			t.Fatalf("roles missing %q", required)
		}
	}
	detail := postgresCatalogProtocolDatabaseDetailSQL(e, "app_db")
	for _, required := range []string{"aclexplode", "nspacl", "old_user", "shared_user", "CASE WHEN " + dbComment("d.oid", postgresCatalogProtocolPreviousDatabaseMarker(e, "app_db"))} {
		if !strings.Contains(detail, required) {
			t.Fatalf("detail missing %q", required)
		}
	}
}

func TestPostgresCatalogProtocolSQLModelsBootstrapAndExactScopedTopology(t *testing.T) {
	e := postgresCatalogProtocolFixture(t)
	roles := postgresCatalogProtocolRolesSQL(e)
	for _, required := range []string{
		"r.rolname='s2h_admin' AND r.rolcanlogin AND r.rolinherit AND r.rolsuper AND r.rolcreatedb AND r.rolcreaterole AND NOT r.rolreplication AND NOT r.rolbypassrls",
		"m.member=(SELECT oid FROM pg_roles WHERE rolname='s2h_admin')",
		"m.roleid=(SELECT oid FROM pg_roles WHERE rolname='s2h_admin')",
		"NOT EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.member=(SELECT oid FROM pg_roles WHERE rolname='s2h_admin') OR m.roleid=(SELECT oid FROM pg_roles WHERE rolname='s2h_admin'))",
		"r.rolname='old_user' AND NOT r.rolcanlogin",
		"NOT EXISTS (SELECT 1 FROM pg_db_role_setting s JOIN pg_roles r ON r.oid=s.setrole WHERE r.rolname='old_user')",
		"d.datname='app_db')",
		"'jobs_user'",
	} {
		if !strings.Contains(roles, required) {
			t.Fatalf("roles missing %q: %s", required, roles)
		}
	}
	detail := postgresCatalogProtocolDatabaseDetailSQL(e, "app_db")
	for _, required := range []string{
		"r.rolname='shared_user' AND d.datname='app_db'",
		"r.rolname='old_user'",
		"rolname='s2h_owner_",
		"a.privilege_type='CONNECT' AND NOT a.is_grantable",
		"r.rolname=" + sqlQuote(postgresOwner(e.Binding.Service, "app_db")) + " AND s.setdatabase=(SELECT oid FROM pg_database WHERE datname='app_db')",
	} {
		if !strings.Contains(detail, required) {
			t.Fatalf("detail missing %q: %s", required, detail)
		}
	}
	if !strings.Contains(detail, "NOT EXISTS (SELECT 1 FROM pg_db_role_setting s JOIN pg_roles r ON r.oid=s.setrole WHERE r.rolname="+sqlQuote(postgresOwner(e.Binding.Service, "app_db"))+" AND s.setdatabase=(SELECT oid FROM pg_database WHERE datname='app_db'))") {
		t.Fatalf("owner setting reset is missing: %s", detail)
	}
}

func TestPostgresCatalogProtocolWriterHandoffIsCompleteAndDataDerived(t *testing.T) {
	e := postgresCatalogProtocolFixture(t)
	got := postgresCatalogProtocolWriterHandoff(e)
	prior := postgresCatalogProtocolPreviousExpected(e)
	sharedUser := postgresCatalogProtocolSharedUsername(t, e.Desired)
	want := []string{
		"BEGIN",
		"ENSURE ROLE s2h_admin WHEN ABSENT",
		"ALTER ROLE s2h_admin SUPERUSER CREATEDB CREATEROLE LOGIN INHERIT NOREPLICATION NOBYPASSRLS",
		"ENSURE ROLE " + postgresOwner(e.Binding.Service, "app_db") + " WHEN ABSENT",
		"ALTER ROLE " + postgresOwner(e.Binding.Service, "app_db") + " NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS",
		"COMMENT ON ROLE " + postgresOwner(e.Binding.Service, "app_db") + " IS " + postgresCatalogProtocolOwnerMarker(e, "app_db"),
		"ENSURE ROLE " + postgresOwner(e.Binding.Service, "jobs_db") + " WHEN ABSENT",
		"ALTER ROLE " + postgresOwner(e.Binding.Service, "jobs_db") + " NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS",
		"COMMENT ON ROLE " + postgresOwner(e.Binding.Service, "jobs_db") + " IS " + postgresCatalogProtocolOwnerMarker(e, "jobs_db"),
		"ENSURE ROLE " + postgresCatalogProtocolCreator(e, "app_db") + " WHEN ABSENT",
		"ALTER ROLE " + postgresCatalogProtocolCreator(e, "app_db") + " NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS",
		"COMMENT ON ROLE " + postgresCatalogProtocolCreator(e, "app_db") + " IS " + postgresCatalogProtocolCreatorMarker(e, "app_db"),
		"ENSURE ROLE " + postgresCatalogProtocolCreator(e, "jobs_db") + " WHEN ABSENT",
		"ALTER ROLE " + postgresCatalogProtocolCreator(e, "jobs_db") + " NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS",
		"COMMENT ON ROLE " + postgresCatalogProtocolCreator(e, "jobs_db") + " IS " + postgresCatalogProtocolCreatorMarker(e, "jobs_db"),
		"ENSURE ROLE " + postgresCatalogProtocolCreator(prior, "app_db") + " WHEN ABSENT",
		"ALTER ROLE " + postgresCatalogProtocolCreator(prior, "app_db") + " NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS",
		"COMMENT ON ROLE " + postgresCatalogProtocolCreator(prior, "app_db") + " IS " + postgresCatalogProtocolCreatorMarker(prior, "app_db"),
		"ENSURE ROLE jobs_user WHEN ABSENT",
		"ALTER ROLE jobs_user LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS",
		"COMMENT ON ROLE jobs_user IS " + postgresCatalogProtocolClientMarker(e, "jobs_user"),
		"ENSURE ROLE old_user WHEN ABSENT",
		"ALTER ROLE old_user LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS",
		"COMMENT ON ROLE old_user IS " + postgresCatalogProtocolClientMarker(prior, "old_user"),
		"ENSURE ROLE " + sharedUser + " WHEN ABSENT",
		"ALTER ROLE " + sharedUser + " LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS",
		"COMMENT ON ROLE " + sharedUser + " IS " + postgresCatalogProtocolClientMarker(e, sharedUser),
		"GRANT <desired owner memberships> WITH ADMIN FALSE INHERIT FALSE SET TRUE",
		"REVOKE <removed or moved owner> FROM <client>",
		"RESET obsolete client role settings",
		"ALTER ROLE <removed client> NOLOGIN",
		"COMMENT ON ROLE s2h_admin IS " + postgresCatalogProtocolRolesMarker(e),
		"COMMIT",
		"CREATE DATABASE app_db OWNER " + postgresCatalogProtocolCreator(e, "app_db") + " WHEN ABSENT",
		"BEGIN",
		"ALTER DATABASE app_db OWNER TO " + postgresOwner(e.Binding.Service, "app_db"),
		"ALTER SCHEMA public OWNER TO " + postgresOwner(e.Binding.Service, "app_db"),
		"REVOKE CREATE ON SCHEMA public FROM PUBLIC",
		"GRANT USAGE, CREATE ON SCHEMA public TO " + postgresOwner(e.Binding.Service, "app_db"),
		"GRANT owner and desired clients CONNECT ON DATABASE app_db",
		"REVOKE unintended managed CONNECT ON DATABASE app_db",
		"REVOKE unintended managed schema grants ON SCHEMA public",
		"SET ROLE for each desired client IN DATABASE app_db TO " + postgresOwner(e.Binding.Service, "app_db"),
		"COMMENT ON DATABASE app_db IS " + postgresCatalogProtocolDatabaseMarker(e, "app_db"),
		"COMMIT",
		"CREATE DATABASE jobs_db OWNER " + postgresCatalogProtocolCreator(e, "jobs_db") + " WHEN ABSENT",
		"BEGIN",
		"ALTER DATABASE jobs_db OWNER TO " + postgresOwner(e.Binding.Service, "jobs_db"),
		"ALTER SCHEMA public OWNER TO " + postgresOwner(e.Binding.Service, "jobs_db"),
		"REVOKE CREATE ON SCHEMA public FROM PUBLIC",
		"GRANT USAGE, CREATE ON SCHEMA public TO " + postgresOwner(e.Binding.Service, "jobs_db"),
		"GRANT owner and desired clients CONNECT ON DATABASE jobs_db",
		"REVOKE unintended managed CONNECT ON DATABASE jobs_db",
		"REVOKE unintended managed schema grants ON SCHEMA public",
		"SET ROLE for each desired client IN DATABASE jobs_db TO " + postgresOwner(e.Binding.Service, "jobs_db"),
		"COMMENT ON DATABASE jobs_db IS " + postgresCatalogProtocolDatabaseMarker(e, "jobs_db"),
		"COMMIT",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("writer expectations = %#v", got)
	}
	adminMarker := indexPostgresCatalogExpectation(got, "COMMENT ON ROLE s2h_admin IS "+postgresCatalogProtocolRolesMarker(e))
	appMarker := indexPostgresCatalogExpectation(got, "COMMENT ON DATABASE app_db IS "+postgresCatalogProtocolDatabaseMarker(e, "app_db"))
	jobsMarker := indexPostgresCatalogExpectation(got, "COMMENT ON DATABASE jobs_db IS "+postgresCatalogProtocolDatabaseMarker(e, "jobs_db"))
	if adminMarker < 0 || appMarker < 0 || jobsMarker < 0 || got[adminMarker+1] != "COMMIT" || got[appMarker+1] != "COMMIT" || got[jobsMarker+1] != "COMMIT" {
		t.Fatalf("markers are not immediately before their commits: %#v", got)
	}
	for _, db := range []string{"app_db", "jobs_db"} {
		create := indexPostgresCatalogExpectation(got, "CREATE DATABASE "+db+" OWNER "+postgresCatalogProtocolCreator(e, db)+" WHEN ABSENT")
		marker := indexPostgresCatalogExpectation(got, "COMMENT ON DATABASE "+db+" IS "+postgresCatalogProtocolDatabaseMarker(e, db))
		if create < 0 || marker < create || marker+1 >= len(got) || got[create+1] != "BEGIN" || got[marker+1] != "COMMIT" {
			t.Fatalf("%s recoverable create/finalize boundary is not atomic: %#v", db, got)
		}
	}
	if strings.Contains(strings.Join(got, "\n"), "GRANT desired clients USAGE ON SCHEMA public") {
		t.Fatalf("writer grants clients schema usage: %#v", got)
	}
	createOnly := false
	for _, phase := range got {
		if strings.HasPrefix(phase, "CREATE DATABASE ") {
			if createOnly {
				t.Fatalf("reachable handoff sequence has multiple create-only databases: %#v", got)
			}
			createOnly = true
		}
		if phase == "COMMIT" {
			createOnly = false
		}
	}
}

func TestPostgresCatalogProtocolObserverRejectsClientSchemaACLsOutsideWriterHandoff(t *testing.T) {
	e := postgresCatalogProtocolFixture(t)
	detail := postgresCatalogProtocolDatabaseDetailSQL(e, "app_db")
	owner := postgresOwner(e.Binding.Service, "app_db")
	users := postgresCatalogProtocolUsernames(append(append([]hostcontract.LocalDataClient{}, e.Desired...), e.Previous...))
	quotedUsers := make([]string, 0, len(users))
	for _, user := range users {
		quotedUsers = append(quotedUsers, sqlQuote(user))
	}
	for _, required := range []string{
		"a.grantee=(SELECT oid FROM pg_roles WHERE rolname=" + sqlQuote(owner) + ") AND a.privilege_type IN ('USAGE','CREATE')",
		"r.rolname IN (" + strings.Join(quotedUsers, ",") + ")",
		"FROM pg_namespace n CROSS JOIN LATERAL aclexplode(n.nspacl) a JOIN pg_roles r ON r.oid=a.grantee",
	} {
		if !strings.Contains(detail, required) {
			t.Fatalf("observer does not require exact stable-owner-only schema ACL compatibility %q: %s", required, detail)
		}
	}
	if strings.Contains(strings.Join(postgresCatalogProtocolWriterHandoff(e), "\n"), "clients USAGE ON SCHEMA public") {
		t.Fatal("writer reintroduces client schema ACL rejected by observer")
	}
}

func indexPostgresCatalogExpectation(values []string, want string) int {
	for i, value := range values {
		if value == want {
			return i
		}
	}
	return -1
}

func TestPostgresCatalogProtocolPriorUsesRevisionEvenWithZeroClients(t *testing.T) {
	e := postgresCatalogProtocolFixture(t)
	e.Previous = nil
	if err := e.valid(); err != nil {
		t.Fatalf("zero-client prior rejected: %v", err)
	}
	prior := postgresCatalogProtocolPreviousExpected(e)
	if got, want := postgresCatalogProtocolPreviousDatabaseMarker(e, "app_db"), postgresCatalogProtocolDatabaseMarker(prior, "app_db"); got != want {
		t.Fatalf("previous marker = %q, want %q", got, want)
	}
	roles := postgresCatalogProtocolRolesSQL(e)
	if !strings.Contains(roles, postgresCatalogProtocolRolesMarker(prior)) {
		t.Fatalf("roles SQL omitted zero-client prior marker: %s", roles)
	}
	old := postgresCatalogProtocolOperation(e.PreviousRevision)
	priorRoles := postgresCatalogProtocolRoleObservation{Marker: postgresCatalogProtocolPrior, Operation: old, Admin: true, Owners: true, Clients: true, Creators: true, Memberships: true, Retired: true, OldSettings: true}
	key := postgresCatalogProtocolDatabaseToken("app_db")
	priorDB := postgresCatalogProtocolDatabaseObservation{Key: key, Reference: postgresCatalogProtocolReferenceObservation{Key: key, State: postgresCatalogProtocolAbsent, Operation: "-"}, Detail: postgresCatalogProtocolDetailObservation{Key: key, State: postgresCatalogProtocolAbsent, Operation: "-"}}
	if got := classifyPostgresCatalogProtocol(e, priorRoles, []postgresCatalogProtocolDatabaseObservation{priorDB, {Key: postgresCatalogProtocolDatabaseToken("jobs_db"), Reference: postgresCatalogProtocolReferenceObservation{Key: postgresCatalogProtocolDatabaseToken("jobs_db"), State: postgresCatalogProtocolAbsent, Operation: "-"}, Detail: postgresCatalogProtocolDetailObservation{Key: postgresCatalogProtocolDatabaseToken("jobs_db"), State: postgresCatalogProtocolAbsent, Operation: "-"}}}, false); got.State != postgresCatalogProtocolPrior {
		t.Fatalf("zero-client prior = %#v", got)
	}
}

func TestPostgresCatalogProtocolRejectsPreviousWithoutRevision(t *testing.T) {
	e := postgresCatalogProtocolFixture(t)
	e.PreviousRevision = ""
	if err := e.valid(); err == nil {
		t.Fatal("previous clients accepted without a previous revision")
	}
	e.Previous = nil
	if err := e.valid(); err != nil {
		t.Fatalf("zero-client previous revision rejected: %v", err)
	}
}

func TestPostgresCatalogProtocolCreatorBindsTargetOperation(t *testing.T) {
	e := postgresCatalogProtocolFixture(t)
	creator := postgresCatalogProtocolCreator(e, "app_db")
	if !strings.HasPrefix(creator, "s2h_create_") || len(creator) != len("s2h_create_")+24 {
		t.Fatalf("creator = %q", creator)
	}
	if postgresCatalogProtocolCreatorMarker(e, "app_db") == postgresCatalogProtocolCreatorMarker(e, "jobs_db") {
		t.Fatal("creator marker does not bind database operation")
	}
	changed := e
	changed.Revision = revisionB()
	if postgresCatalogProtocolCreator(e, "app_db") == postgresCatalogProtocolCreator(changed, "app_db") {
		t.Fatal("creator role does not bind revision")
	}
	changed = e
	changed.Binding.Service = "other-db"
	if postgresCatalogProtocolCreator(e, "app_db") == postgresCatalogProtocolCreator(changed, "app_db") {
		t.Fatal("creator role does not bind service")
	}
	changed = e
	changed.Binding.Environment = "other-host-binding"
	if postgresCatalogProtocolCreator(e, "app_db") == postgresCatalogProtocolCreator(changed, "app_db") {
		t.Fatal("creator role does not bind Host")
	}
	for _, query := range []string{postgresCatalogProtocolRolesSQL(e), postgresCatalogProtocolDatabaseReferenceSQL(e, "app_db"), postgresCatalogProtocolDatabaseDetailSQL(e, "app_db")} {
		if !strings.Contains(query, creator) || !strings.Contains(query, postgresCatalogProtocolCreatorMarker(e, "app_db")) {
			t.Fatalf("creator provenance omitted: %s", query)
		}
	}
}

func TestPostgresCatalogProtocolRejectsUnmarkedStableOwnerCreateOnlyAndWrongCreator(t *testing.T) {
	e := postgresCatalogProtocolFixture(t)
	ref := postgresCatalogProtocolDatabaseReferenceSQL(e, "app_db")
	detail := postgresCatalogProtocolDatabaseDetailSQL(e, "app_db")
	owner := postgresOwner(e.Binding.Service, "app_db")
	creator := postgresCatalogProtocolCreator(e, "app_db")
	for _, query := range []string{ref, detail} {
		if strings.Contains(query, "d.datdba=(SELECT oid FROM pg_roles WHERE rolname="+sqlQuote(owner)+") THEN 'create-only'") {
			t.Fatalf("stable owner accepted as create-only: %s", query)
		}
		if !strings.Contains(query, "d.datdba=(SELECT oid FROM pg_roles WHERE rolname="+sqlQuote(creator)+")") {
			t.Fatalf("creator owner missing: %s", query)
		}
	}
}

func TestPostgresCatalogProtocolUsesSplitDatabaseOwnersAndRejectsCreatorOwnedTarget(t *testing.T) {
	e := postgresCatalogProtocolFixture(t)
	op := postgresCatalogProtocolOperation(e.Revision)
	key := postgresCatalogProtocolDatabaseToken("app_db")
	ref, err := parsePostgresCatalogProtocolReference([]byte("s2hpg2\t1\tdbref\t" + key + "\ttarget\t" + op + "\t1\t0\n"))
	if err != nil || !ref.StableOwner || ref.CreatorOwner {
		t.Fatalf("target reference = %#v, %v", ref, err)
	}
	create, err := parsePostgresCatalogProtocolReference([]byte("s2hpg2\t1\tdbref\t" + key + "\tcreate-only\t-\t0\t1\n"))
	if err != nil || create.StableOwner || !create.CreatorOwner {
		t.Fatalf("create-only reference = %#v, %v", create, err)
	}
	roles := postgresCatalogProtocolRoleObservation{Marker: postgresCatalogProtocolTarget, Operation: op, Admin: true, Owners: true, Clients: true, Creators: true, Memberships: true, Retired: true, OldSettings: true}
	detail := postgresCatalogProtocolDetailObservation{Key: key, State: postgresCatalogProtocolTarget, Operation: op, SchemaOwner: true, PublicCreateRevoked: true, OwnerUsageCreate: true, Settings: true, Connect: true, NoScopedExtras: true}
	creatorOwned := postgresCatalogProtocolDatabaseObservation{Key: key, Reference: postgresCatalogProtocolReferenceObservation{Key: key, State: postgresCatalogProtocolTarget, Operation: op, CreatorOwner: true}, Detail: detail}
	jobs := postgresCatalogProtocolDatabaseToken("jobs_db")
	exactJobs := postgresCatalogProtocolDatabaseObservation{Key: jobs, Reference: postgresCatalogProtocolReferenceObservation{Key: jobs, State: postgresCatalogProtocolTarget, Operation: op, StableOwner: true}, Detail: postgresCatalogProtocolDetailObservation{Key: jobs, State: postgresCatalogProtocolTarget, Operation: op, SchemaOwner: true, PublicCreateRevoked: true, OwnerUsageCreate: true, Settings: true, Connect: true, NoScopedExtras: true}}
	if got := classifyPostgresCatalogProtocol(e, roles, []postgresCatalogProtocolDatabaseObservation{creatorOwned, exactJobs}, false); got.State != postgresCatalogProtocolMixed {
		t.Fatalf("creator-owned target = %#v", got)
	}
	wrongCreator := creatorOwned
	wrongCreator.Reference.State, wrongCreator.Reference.Operation = postgresCatalogProtocolForeign, "-"
	if got := classifyPostgresCatalogProtocol(e, roles, []postgresCatalogProtocolDatabaseObservation{wrongCreator, exactJobs}, false); got.State != postgresCatalogProtocolForeign {
		t.Fatalf("wrong creator = %#v", got)
	}
	createOnly := postgresCatalogProtocolDatabaseObservation{Key: key, Reference: create, Detail: postgresCatalogProtocolDetailObservation{Key: key, State: postgresCatalogProtocolCreateOnly, Operation: "-", Fresh: true, Owner: true}}
	if got := classifyPostgresCatalogProtocol(e, roles, []postgresCatalogProtocolDatabaseObservation{createOnly, exactJobs}, true); got.State != postgresCatalogProtocolMixed {
		t.Fatalf("out-of-order legitimate create-only = %#v", got)
	}
}

func TestPostgresCatalogProtocolPriorStructuralFalseAndUnmarkedCollisionAreMixed(t *testing.T) {
	e := postgresCatalogProtocolFixture(t)
	prior := postgresCatalogProtocolRoleObservation{Marker: postgresCatalogProtocolPrior, Operation: postgresCatalogProtocolOperation(e.PreviousRevision), Admin: true, Owners: true, Clients: true, Creators: true, Memberships: true, Retired: true, OldSettings: true}
	prior.Clients = false
	absent := func(db string) postgresCatalogProtocolDatabaseObservation {
		key := postgresCatalogProtocolDatabaseToken(db)
		return postgresCatalogProtocolDatabaseObservation{Key: key, Reference: postgresCatalogProtocolReferenceObservation{Key: key, State: postgresCatalogProtocolAbsent, Operation: "-"}, Detail: postgresCatalogProtocolDetailObservation{Key: key, State: postgresCatalogProtocolAbsent, Operation: "-"}}
	}
	if got := classifyPostgresCatalogProtocol(e, prior, []postgresCatalogProtocolDatabaseObservation{absent("app_db"), absent("jobs_db")}, false); got.State != postgresCatalogProtocolMixed {
		t.Fatalf("false prior structural bit = %#v", got)
	}
	roles := postgresCatalogProtocolRolesSQL(e)
	priorExpected := postgresCatalogProtocolPreviousExpected(e)
	for _, principal := range []string{"shared_user", "jobs_user", "old_user", postgresOwner(e.Binding.Service, "app_db"), postgresCatalogProtocolCreator(e, "app_db"), postgresCatalogProtocolCreator(priorExpected, "app_db"), "s2h_admin"} {
		if !strings.Contains(roles, "x.rolname="+sqlQuote(principal)) || !strings.Contains(roles, "d.description NOT IN") {
			t.Fatalf("scoped marker collision is not principal-bound: %s", roles)
		}
	}
	for _, principal := range []string{postgresCatalogProtocolCreator(e, "app_db"), postgresCatalogProtocolCreator(priorExpected, "app_db"), "s2h_admin"} {
		if !strings.Contains(roles, sqlQuote(principal)) {
			t.Fatalf("creator/admin missing from membership scope %q: %s", principal, roles)
		}
	}
}

func TestPostgresCatalogProtocolRoleMarkerBindingsRejectSwappedMarkers(t *testing.T) {
	e := postgresCatalogProtocolFixture(t)
	e.Previous = append(e.Previous, hostcontract.LocalDataClient{AppID: "previous-only-app", Username: "previous_only_user", Database: "previous_only_db"})
	prior := postgresCatalogProtocolPreviousExpected(e)
	bindings := postgresCatalogProtocolRoleMarkerBindings(e)
	allowed := func(role, marker string) bool {
		for _, binding := range bindings {
			if binding.Name == role {
				return postgresCatalogProtocolRoleMarkerAllows(binding, marker)
			}
		}
		return false
	}
	previousOwner := postgresOwner(e.Binding.Service, "previous_only_db")
	creator := postgresCatalogProtocolCreator(e, "app_db")
	client := "shared_user"
	markers := []string{
		postgresCatalogProtocolOwnerMarker(prior, "previous_only_db"),
		postgresCatalogProtocolCreatorMarker(e, "app_db"),
		postgresCatalogProtocolClientMarker(e, client),
	}
	roles := []string{previousOwner, creator, client}
	for i, role := range roles {
		if !allowed(role, markers[i]) {
			t.Fatalf("%s does not permit its marker", role)
		}
		if allowed(role, markers[(i+1)%len(markers)]) {
			t.Fatalf("%s accepted %s marker", role, roles[(i+1)%len(roles)])
		}
	}
	rolesSQL := postgresCatalogProtocolRolesSQL(e)
	for _, binding := range bindings {
		if !strings.Contains(rolesSQL, "x.rolname="+sqlQuote(binding.Name)) {
			t.Fatalf("roles SQL does not bind marker to %q: %s", binding.Name, rolesSQL)
		}
	}
}

func TestPostgresCatalogProtocolZeroClientsAndCreateOnlySQL(t *testing.T) {
	e := postgresCatalogProtocolFixture(t)
	e.Desired, e.Previous, e.PreviousRevision = nil, nil, ""
	roles := postgresCatalogProtocolRolesSQL(e)
	if strings.Contains(roles, "IN ()") {
		t.Fatalf("zero-client roles SQL has empty IN list: %s", roles)
	}
	createOnly := postgresCatalogProtocolDatabaseDetailSQL(postgresCatalogProtocolFixture(t), "app_db")
	createBranch := strings.Split(createOnly, " ELSE ")[0]
	for _, forbidden := range []string{"bootstrapPublic", "a.is_grantable)=2", "nspowner=(SELECT oid FROM pg_roles WHERE rolname='s2h_owner"} {
		if strings.Contains(createBranch, forbidden) {
			t.Fatalf("create-only branch assumes final schema state %q: %s", forbidden, createBranch)
		}
	}
	if !strings.Contains(createOnly, "NOT EXISTS (SELECT 1 FROM pg_shdescription x WHERE x.objoid=d.oid AND x.classoid='pg_database'::regclass)") || !strings.Contains(createOnly, "FROM pg_namespace n WHERE n.nspname='public'") {
		t.Fatalf("create-only branch lacks fresh identity/schema checks: %s", createOnly)
	}
}

func TestPostgresCatalogProtocolClassifierTreatsFalseStructuralBitsAsMixed(t *testing.T) {
	e := postgresCatalogProtocolFixture(t)
	op := postgresCatalogProtocolOperation(e.Revision)
	roles := postgresCatalogProtocolRoleObservation{Marker: postgresCatalogProtocolTarget, Operation: op, Admin: true, Owners: true, Clients: true, Creators: true, Memberships: true, Retired: true, OldSettings: true}
	key := postgresCatalogProtocolDatabaseToken("app_db")
	detail := postgresCatalogProtocolDetailObservation{Key: key, State: postgresCatalogProtocolTarget, Operation: op, SchemaOwner: true, PublicCreateRevoked: true, OwnerUsageCreate: true, Settings: true, Connect: true, NoScopedExtras: true}
	dbs := []postgresCatalogProtocolDatabaseObservation{{Key: key, Reference: postgresCatalogProtocolReferenceObservation{Key: key, State: postgresCatalogProtocolTarget, Operation: op, StableOwner: true}, Detail: detail}, {Key: postgresCatalogProtocolDatabaseToken("jobs_db"), Reference: postgresCatalogProtocolReferenceObservation{Key: postgresCatalogProtocolDatabaseToken("jobs_db"), State: postgresCatalogProtocolTarget, Operation: op, StableOwner: true}, Detail: postgresCatalogProtocolDetailObservation{Key: postgresCatalogProtocolDatabaseToken("jobs_db"), State: postgresCatalogProtocolTarget, Operation: op, SchemaOwner: true, PublicCreateRevoked: true, OwnerUsageCreate: true, Settings: true, Connect: true, NoScopedExtras: true}}}
	roles.Memberships = false
	if got := classifyPostgresCatalogProtocol(e, roles, dbs, false); got.State != postgresCatalogProtocolMixed {
		t.Fatalf("false role structural bit = %#v", got)
	}
	roles.Memberships = true
	dbs[0].Detail.Connect = false
	if got := classifyPostgresCatalogProtocol(e, roles, dbs, false); got.State != postgresCatalogProtocolMixed {
		t.Fatalf("false detail structural bit = %#v", got)
	}
}

func postgresCatalogProtocolFixture(t *testing.T) postgresCatalogProtocolExpected {
	t.Helper()
	e := postgresCatalogProtocolExpected{Binding: postgresCatalogProtocolBinding{Environment: "prod-east", Server: "edge-1", Ownership: "owner-1", Service: "primary-db"}, Revision: revision(), PreviousRevision: revisionB(), Desired: []hostcontract.LocalDataClient{{AppID: "api-one", Username: "shared_user", Database: "app_db"}, {AppID: "worker-two", Username: "shared_user", Database: "app_db"}, {AppID: "jobs-app", Username: "jobs_user", Database: "jobs_db"}}, Previous: []hostcontract.LocalDataClient{{AppID: "api-one", Username: "shared_user", Database: "app_db"}, {AppID: "old-app", Username: "old_user", Database: "app_db"}}}
	if err := e.valid(); err != nil {
		t.Fatal(err)
	}
	return e
}

func postgresCatalogProtocolSharedUsername(t *testing.T, clients []hostcontract.LocalDataClient) string {
	t.Helper()
	counts := map[string]int{}
	for _, client := range clients {
		counts[client.Username]++
	}
	for username, count := range counts {
		if count > 1 {
			return username
		}
	}
	t.Fatal("fixture has no shared username")
	return ""
}
