package sqlite

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// The two-way drift gate for the owned identity set. It walks this package's
// own schema (sqlite_master, PRAGMA table_info, PRAGMA foreign_key_list) and
// fails when a member carries no identity declaration, or when a declared
// identity reference is absent from the set. It also fails on any *_id-named
// TEXT column in neither the owned set nor the recorded preserved-opaque set, so
// a new column is red by default instead of silently inherited.
//
// The classification rule it enforces:
//
//	IN  an owner-declared identity type: a foreign key into an identity-keyed
//	    relation, or a codec-parsed structured identity field.
//	OUT an opaque, equality-only label preserved byte-exact whatever its bytes.
//
// Membership is a citation, never name intuition: a name that looks
// identity-shaped is not a declaration.

type schemaColumn struct {
	table, column, declaredType string
}

type schemaReference struct {
	childTable, childColumn, parentTable, parentColumn string
}

func readSchema(t *testing.T, ctx context.Context, db *DB) ([]schemaColumn, []schemaReference) {
	t.Helper()
	rows, err := db.db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if strings.HasPrefix(name, "sqlite_") {
			continue
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()

	var columns []schemaColumn
	var references []schemaReference
	for _, table := range tables {
		info, err := db.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%q)", table))
		if err != nil {
			t.Fatal(err)
		}
		for info.Next() {
			var cid, notNull, primaryKey int
			var name, declaredType string
			var defaultValue any
			if err := info.Scan(&cid, &name, &declaredType, &notNull, &defaultValue, &primaryKey); err != nil {
				info.Close()
				t.Fatal(err)
			}
			columns = append(columns, schemaColumn{table, name, strings.ToUpper(declaredType)})
		}
		if err := info.Err(); err != nil {
			info.Close()
			t.Fatal(err)
		}
		info.Close()

		fks, err := db.db.QueryContext(ctx, fmt.Sprintf("PRAGMA foreign_key_list(%q)", table))
		if err != nil {
			t.Fatal(err)
		}
		for fks.Next() {
			var id, seq int
			var parentTable, childColumn, parentColumn, onUpdate, onDelete, match string
			if err := fks.Scan(&id, &seq, &parentTable, &childColumn, &parentColumn, &onUpdate, &onDelete, &match); err != nil {
				fks.Close()
				t.Fatal(err)
			}
			references = append(references, schemaReference{table, childColumn, parentTable, parentColumn})
		}
		if err := fks.Err(); err != nil {
			fks.Close()
			t.Fatal(err)
		}
		fks.Close()
	}
	return columns, references
}

func declaredReferenceTarget(t *testing.T, declaration identityDeclaration) (string, string) {
	t.Helper()
	parentTable, parentColumn, ok := strings.Cut(declaration.reference, ".")
	if !ok || parentTable == "" || parentColumn == "" {
		t.Fatalf("identity declaration reference %q is not table.column", declaration.reference)
	}
	return parentTable, parentColumn
}

func TestOwnedIdentityColumnsMatchDeclaredSchema(t *testing.T) {
	ctx := context.Background()
	db := newJournalDB(t)
	columns, references := readSchema(t, ctx, db)

	columnSet := map[string]schemaColumn{}
	for _, column := range columns {
		columnSet[column.table+"."+column.column] = column
	}
	referenceSet := map[string]schemaReference{}
	for _, reference := range references {
		referenceSet[reference.childTable+"."+reference.childColumn] = reference
	}

	memberSet := map[string]identityColumn{}
	for _, member := range append(append([]identityColumn{}, ownedIdentityColumns...), ownedContextIdentityColumns...) {
		key := member.table + "." + member.column
		if _, duplicate := memberSet[key]; duplicate {
			t.Fatalf("owned identity set lists %s twice", key)
		}
		memberSet[key] = member
	}
	keyedRelations := map[string]string{} // relation -> its identity key column
	for _, member := range ownedIdentityColumns {
		if member.declaration.kind == identityFromRelationKey {
			keyedRelations[member.table] = member.column
		}
	}
	opaqueSet := map[string]string{}
	for _, opaque := range preservedOpaqueColumns {
		key := opaque.table + "." + opaque.column
		if opaque.reason == "" {
			t.Fatalf("preserved-opaque column %s has no recorded judgment", key)
		}
		if _, duplicate := opaqueSet[key]; duplicate {
			t.Fatalf("preserved-opaque set lists %s twice", key)
		}
		opaqueSet[key] = opaque.reason
	}

	// Anti-vacuity: both halves of the classification must be visibly populated,
	// and the schema must carry the tables the set claims.
	if len(memberSet) < 20 || len(opaqueSet) == 0 {
		t.Fatalf("classification sets too small to be meaningful: %d members, %d opaque", len(memberSet), len(opaqueSet))
	}
	for _, relation := range []string{"tasks", "agents", "activities", "comments"} {
		if keyedRelations[relation] == "" {
			t.Fatalf("identity-keyed relation %q has no relation-key member", relation)
		}
	}
	if _, ok := opaqueSet["actor_namespace_claims.claimant_id"]; !ok {
		t.Fatal("actor_namespace_claims.claimant_id is not recorded as a preserved-opaque label")
	}

	// Direction A: every member carries a satisfied identity declaration. An
	// FK-declared member must have the exact declared foreign key; a relation-key
	// or model-typed member must at least exist in the schema it cites.
	for key, member := range memberSet {
		if _, exists := columnSet[key]; !exists {
			t.Errorf("owned identity member %s has no column in the schema", key)
			continue
		}
		switch member.declaration.kind {
		case identityFromForeignKey:
			wantTable, wantColumn := declaredReferenceTarget(t, member.declaration)
			got, ok := referenceSet[key]
			if !ok {
				t.Errorf("member %s is declared a foreign key but the schema declares no reference from it", key)
				continue
			}
			if got.parentTable != wantTable || got.parentColumn != wantColumn {
				t.Errorf("member %s declares reference %s.%s, schema declares %s.%s", key, wantTable, wantColumn, got.parentTable, got.parentColumn)
			}
		case identityFromRelationKey, identityFromModelType:
			if member.declaration.ownerType == "" {
				t.Errorf("member %s is declared %d but names no owner identity type", key, member.declaration.kind)
			}
		default:
			t.Errorf("member %s carries no identity declaration", key)
		}
	}

	// Direction B: a declared identity reference is never missing from the set.
	// Any FK whose parent is an identity-keyed relation must own that reference.
	for _, reference := range references {
		identityKey, keyed := keyedRelations[reference.parentTable]
		if !keyed || reference.parentColumn != identityKey {
			continue
		}
		key := reference.childTable + "." + reference.childColumn
		if _, member := memberSet[key]; !member {
			t.Errorf("schema reference %s → %s.%s is identity-bearing but absent from the owned identity set", key, reference.parentTable, reference.parentColumn)
		}
	}

	// Red by default: every *_id-named TEXT column is classified on purpose.
	var unclassified []string
	for _, column := range columns {
		if column.declaredType != "TEXT" || !strings.HasSuffix(strings.ToLower(column.column), "_id") {
			continue
		}
		key := column.table + "." + column.column
		if _, member := memberSet[key]; member {
			continue
		}
		if _, opaque := opaqueSet[key]; opaque {
			continue
		}
		unclassified = append(unclassified, key)
	}
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Fatalf("unclassified identity-looking TEXT columns (declare each an owned identity or record it preserved-opaque): %s", strings.Join(unclassified, ", "))
	}
}
