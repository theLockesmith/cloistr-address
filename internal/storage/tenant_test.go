package storage

import (
	"context"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func TestCreateTenant(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	s := NewWithDB(db)

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO users").
		WithArgs("aaaa000000000000000000000000000000000000000000000000000000000000").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("INSERT INTO tenants").
		WithArgs("arbiter-fleet", "aaaa000000000000000000000000000000000000000000000000000000000000").
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)))
	mock.ExpectCommit()

	tenant, err := s.CreateTenant(context.Background(), "arbiter-fleet", "aaaa000000000000000000000000000000000000000000000000000000000000")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if tenant.ID != "arbiter-fleet" {
		t.Fatalf("got id=%q, want arbiter-fleet", tenant.ID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAddTenantMember_EnsuresUser(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	s := NewWithDB(db)

	pk := "bbbb000000000000000000000000000000000000000000000000000000000000"

	mock.ExpectBegin()
	// First: ensure users row
	mock.ExpectExec("INSERT INTO users").
		WithArgs(pk).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Then: insert tenant_members (with role and joined_at matching prod schema)
	mock.ExpectExec("INSERT INTO tenant_members").
		WithArgs("arbiter-fleet", pk).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := s.AddTenantMember(context.Background(), "arbiter-fleet", pk); err != nil {
		t.Fatalf("AddTenantMember: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveTenantMember(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	s := NewWithDB(db)

	pk := "cccc000000000000000000000000000000000000000000000000000000000000"

	mock.ExpectExec("DELETE FROM tenant_members").
		WithArgs("arbiter-fleet", pk).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.RemoveTenantMember(context.Background(), "arbiter-fleet", pk); err != nil {
		t.Fatalf("RemoveTenantMember: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSetTenantQuota(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	s := NewWithDB(db)

	mock.ExpectExec("INSERT INTO tenant_quotas").
		WithArgs("arbiter-fleet", "storage_bytes", int64(10737418240)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.SetTenantQuota(context.Background(), "arbiter-fleet", "storage_bytes", 10737418240); err != nil {
		t.Fatalf("SetTenantQuota: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetTenantForPubkey_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	s := NewWithDB(db)

	pk := "dddd000000000000000000000000000000000000000000000000000000000000"

	mock.ExpectQuery("SELECT t.id").
		WithArgs(pk).
		WillReturnRows(sqlmock.NewRows([]string{"id", "owner_pubkey", "created_at"}))

	tenant, err := s.GetTenantForPubkey(context.Background(), pk)
	if err != nil {
		t.Fatalf("GetTenantForPubkey: %v", err)
	}
	if tenant != nil {
		t.Fatalf("expected nil tenant for non-member, got %+v", tenant)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
