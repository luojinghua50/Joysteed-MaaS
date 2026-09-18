// Package audit stores control-plane audit events. Audit rows are append-only:
// this package deliberately exposes no update or delete operation.
package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/rbac"
	tenant "github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
	"gorm.io/gorm"
)

var (
	ErrDatabaseRequired = errors.New("audit: database is required")
	ErrInvalidEntry     = errors.New("audit: invalid entry")
)

// Entry is the immutable audit representation. BeforeJSON and AfterJSON are
// redacted snapshots, never credentials in plaintext.
type Entry struct {
	ID            string             `gorm:"column:id;primaryKey;type:text" json:"id"`
	PrincipalType rbac.PrincipalType `gorm:"column:principal_type;type:text;not null;index" json:"principal_type"`
	PrincipalID   string             `gorm:"column:principal_id;type:text;not null;index" json:"principal_id"`
	TenantID      *tenant.ID         `gorm:"column:tenant_id;type:text;index" json:"tenant_id,omitempty"`
	Action        string             `gorm:"column:action;type:text;not null;index" json:"action"`
	ResourceType  string             `gorm:"column:resource_type;type:text;not null" json:"resource_type"`
	ResourceID    string             `gorm:"column:resource_id;type:text;not null" json:"resource_id"`
	BeforeJSON    string             `gorm:"column:before_json;type:text" json:"before_json,omitempty"`
	AfterJSON     string             `gorm:"column:after_json;type:text" json:"after_json,omitempty"`
	SourceIP      string             `gorm:"column:source_ip;type:text" json:"source_ip,omitempty"`
	RequestID     string             `gorm:"column:request_id;type:text;index" json:"request_id,omitempty"`
	CreatedAt     time.Time          `gorm:"column:created_at;not null;index" json:"created_at"`
}

func (Entry) TableName() string { return "audit_entries" }

type Input struct {
	Principal    rbac.Principal
	TenantID     tenant.ID
	Action       string
	ResourceType string
	ResourceID   string
	Before       any
	After        any
	SourceIP     string
	RequestID    string
}

type Store struct{ db *gorm.DB }

func NewStore(db *gorm.DB) *Store { return &Store{db: db} }
func (s *Store) DB() *gorm.DB {
	if s == nil {
		return nil
	}
	return s.db
}

func (s *Store) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return ErrDatabaseRequired
	}
	if err := s.db.WithContext(ctx).AutoMigrate(&Entry{}); err != nil {
		return fmt.Errorf("audit: migrate: %w", err)
	}
	return nil
}

// Append writes one immutable event. Callers that mutate business state must
// use AppendTx with the same transaction to preserve the audit invariant.
func (s *Store) Append(ctx context.Context, in Input) (*Entry, error) {
	if s == nil || s.db == nil {
		return nil, ErrDatabaseRequired
	}
	var out *Entry
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		out, err = s.AppendTx(tx, in)
		return err
	})
	return out, err
}

func (s *Store) AppendTx(tx *gorm.DB, in Input) (*Entry, error) {
	if tx == nil {
		return nil, ErrDatabaseRequired
	}
	if !in.Principal.Valid() || strings.TrimSpace(in.Action) == "" ||
		strings.TrimSpace(in.ResourceType) == "" || strings.TrimSpace(in.ResourceID) == "" {
		return nil, ErrInvalidEntry
	}
	scopeTenantID := in.TenantID
	if in.Principal.Type == rbac.PrincipalTenantUser {
		if scopeTenantID != "" && scopeTenantID != in.Principal.TenantID {
			return nil, ErrInvalidEntry
		}
		scopeTenantID = in.Principal.TenantID
	}
	before, err := redactSnapshot(in.Before)
	if err != nil {
		return nil, fmt.Errorf("audit: redact before: %w", err)
	}
	after, err := redactSnapshot(in.After)
	if err != nil {
		return nil, fmt.Errorf("audit: redact after: %w", err)
	}
	id, err := newID()
	if err != nil {
		return nil, fmt.Errorf("audit: id: %w", err)
	}
	e := &Entry{
		ID: id, PrincipalType: in.Principal.Type, PrincipalID: in.Principal.ID,
		TenantID: tenantPtr(scopeTenantID), Action: in.Action,
		ResourceType: in.ResourceType, ResourceID: in.ResourceID,
		BeforeJSON: before, AfterJSON: after, SourceIP: in.SourceIP,
		RequestID: in.RequestID, CreatedAt: time.Now().UTC(),
	}
	if err := tx.Create(e).Error; err != nil {
		return nil, fmt.Errorf("audit: append: %w", err)
	}
	return e, nil
}

func (s *Store) ListTenant(ctx context.Context, tenantID tenant.ID, limit int) ([]Entry, error) {
	if s == nil || s.db == nil {
		return nil, ErrDatabaseRequired
	}
	if tenantID == "" {
		return nil, ErrInvalidEntry
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var rows []Entry
	err := s.db.WithContext(ctx).Where("tenant_id = ?", string(tenantID)).Order("created_at DESC").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("audit: list tenant: %w", err)
	}
	return rows, nil
}

func (s *Store) ListPlatform(ctx context.Context, limit int) ([]Entry, error) {
	if s == nil || s.db == nil {
		return nil, ErrDatabaseRequired
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var rows []Entry
	err := s.db.WithContext(ctx).Order("created_at DESC").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("audit: list platform: %w", err)
	}
	return rows, nil
}

func tenantPtr(id tenant.ID) *tenant.ID {
	if id == "" {
		return nil
	}
	return &id
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

var sensitiveFragments = []string{
	"key", "token", "secret", "password", "passwd", "authorization", "credential", "private",
}

func sensitiveKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	if lower == "key" || strings.HasSuffix(lower, "_key") || strings.HasSuffix(lower, "-key") {
		return true
	}
	// Preserve ordinary words such as "monkey" while covering the common
	// snake/kebab/camel-case credential names used by provider configs.
	normalized := strings.NewReplacer("-", "", "_", "", " ", "").Replace(lower)
	for _, fragment := range sensitiveFragments {
		if fragment != "key" && (normalized == fragment || strings.HasSuffix(normalized, fragment)) {
			return true
		}
	}
	for _, exact := range []string{"apikey", "accesskey", "privatekey", "secretkey", "clientsecret", "authorization", "credential"} {
		if normalized == exact {
			return true
		}
	}
	return false
}

// RedactJSON recursively replaces values below credential-like keys. It is
// exported so request-log and audit code can share exactly the same policy.
func RedactJSON(raw []byte) ([]byte, error) {
	var value any
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return json.Marshal(redactValue(value, false))
}

func redactSnapshot(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	var raw []byte
	var err error
	switch x := v.(type) {
	case []byte:
		raw = x
	case json.RawMessage:
		raw = x
	default:
		raw, err = json.Marshal(x)
		if err != nil {
			return "", err
		}
	}
	redacted, err := RedactJSON(raw)
	if err != nil {
		return "", err
	}
	return string(redacted), nil
}

func redactValue(v any, force bool) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for key, value := range x {
			out[key] = redactValue(value, force || sensitiveKey(key))
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, value := range x {
			out[i] = redactValue(value, force)
		}
		return out
	default:
		if force {
			return "[REDACTED]"
		}
		return v
	}
}
