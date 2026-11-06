// operations_bind_test.go pins how operations' two optional uuid columns reach
// the wire.
//
// In-package (not db_test) because both binders are unexported — which is the
// point. The distinction between "bind NULL" and "bind the empty string" is
// invisible in a round-trip test against a stub and fatal against Postgres: an
// empty string bound to a uuid parameter raises SQLSTATE 22P02, which is
// precisely how every dashboard-consented auto-launch rule used to die (review
// A CR-02). The behavioural half — that a keyless row round-trips and stays
// invisible to the issuer-scoped read — lives in operations_test.go behind
// DATABASE_TEST_URL.
package db

import "testing"

// TestAPIKeyIDArgBindsNullForAKeylessOperation: the empty APIKeyID must reach
// the driver as nil, never as "".
//
// A launch authorized by an org role has no key id. `api_key_id` is a uuid
// column, so the empty string is not a "blank value" there — it is a malformed
// uuid, and Postgres rejects the whole INSERT rather than storing anything. The
// only encoding of "there was no key" a uuid column has is NULL.
func TestAPIKeyIDArgBindsNullForAKeylessOperation(t *testing.T) {
	keyless := &Operation{OrgID: "org-1", Kind: "instance.create"}
	if got := apiKeyIDArg(keyless); got != nil {
		t.Errorf("apiKeyIDArg for a keyless operation = %#v, want nil (SQL NULL); "+
			"binding %#v to a uuid column raises SQLSTATE 22P02 and fails the launch", got, got)
	}

	keyed := &Operation{APIKeyID: "8f1c2b3a-0000-4000-8000-000000000001", OrgID: "org-1"}
	got := apiKeyIDArg(keyed)
	if s, ok := got.(string); !ok || s != keyed.APIKeyID {
		t.Errorf("apiKeyIDArg for a keyed operation = %#v, want the id %q unchanged", got, keyed.APIKeyID)
	}
}

// TestOperationIDArgIsUnchangedByTheKeylessWork guards the sibling binder that
// plan 86-20 added: two optional uuid parameters on the same INSERT is exactly
// the shape in which one gets accidentally rewired to the other.
func TestOperationIDArgIsUnchangedByTheKeylessWork(t *testing.T) {
	if got := operationIDArg(&Operation{}); got != nil {
		t.Errorf("operationIDArg with no supplied id = %#v, want nil so the column default applies", got)
	}
	supplied := &Operation{OperationID: "8f1c2b3a-0000-4000-8000-000000000002"}
	if got := operationIDArg(supplied); got != supplied.OperationID {
		t.Errorf("operationIDArg = %#v, want the supplied id %q", got, supplied.OperationID)
	}
}

// TestOperationInsertBindsBothOptionalColumnsPositionally pins the argument
// ORDER of the INSERT against its parameter list.
//
// Both optional columns are uuids and both are bound through a helper returning
// `any`, so swapping $1 and $2 is a change the compiler cannot see: the insert
// would still run, and it would store the operation id in api_key_id.
func TestOperationInsertBindsBothOptionalColumnsPositionally(t *testing.T) {
	const wantColumns = "INSERT INTO operations (operation_id, api_key_id, org_id, kind, state, metadata)"
	if got := operationInsertSQL; len(got) < len(wantColumns) || got[:len(wantColumns)] != wantColumns {
		t.Fatalf("operationInsertSQL column list changed; the binder order in CreateOperation "+
			"(operationIDArg, apiKeyIDArg, OrgID, Kind, Metadata) is positional against it.\ngot: %s", got)
	}
}
