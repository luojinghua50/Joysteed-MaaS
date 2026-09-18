// Package authn persists control-plane sessions without storing bearer or CSRF
// tokens in plaintext.
package authn

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/rbac"
	tenant "github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
	"gorm.io/gorm"
)

var ErrInvalidSession = errors.New("authn: invalid or expired session")

type Session struct {
	ID                string             `gorm:"column:id;primaryKey;type:text"`
	AccessTokenHash   string             `gorm:"column:access_token_hash;type:text;not null;uniqueIndex"`
	CSRFTokenHash     string             `gorm:"column:csrf_token_hash;type:text;not null"`
	PrincipalType     rbac.PrincipalType `gorm:"column:principal_type;type:text;not null;index:idx_control_sessions_principal"`
	PrincipalID       string             `gorm:"column:principal_id;type:text;not null;index:idx_control_sessions_principal"`
	TenantID          *tenant.ID         `gorm:"column:tenant_id;type:text;index:idx_control_sessions_principal"`
	ExpiresAt         time.Time          `gorm:"column:expires_at;not null;index"`
	CreatedAt         time.Time          `gorm:"column:created_at;not null"`
	LastAuthenticated time.Time          `gorm:"column:last_authenticated_at;not null"`
}

func (Session) TableName() string { return "control_sessions" }

type Store struct{ db *gorm.DB }

func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

func (s *Store) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("authn: database is required")
	}
	if err := s.db.WithContext(ctx).AutoMigrate(&Session{}); err != nil {
		return fmt.Errorf("authn: migrate sessions: %w", err)
	}
	return nil
}

func (s *Store) Create(ctx context.Context, principal rbac.Principal, lifetime time.Duration) (access, csrf string, claims rbac.SessionClaims, err error) {
	if s == nil || s.db == nil || !principal.Valid() || lifetime <= 0 {
		return "", "", rbac.SessionClaims{}, ErrInvalidSession
	}
	id, err := randomToken(18)
	if err != nil {
		return "", "", rbac.SessionClaims{}, err
	}
	access, err = randomToken(32)
	if err != nil {
		return "", "", rbac.SessionClaims{}, err
	}
	claims, csrf, err = rbac.NewSessionClaims(id, principal, lifetime)
	if err != nil {
		return "", "", rbac.SessionClaims{}, err
	}
	now := time.Now().UTC()
	row := Session{
		ID: id, AccessTokenHash: tokenHash(access), CSRFTokenHash: claims.CSRFTokenHash,
		PrincipalType: principal.Type, PrincipalID: principal.ID, TenantID: tenantPtr(principal.TenantID),
		ExpiresAt: claims.ExpiresAt, CreatedAt: now, LastAuthenticated: now,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return "", "", rbac.SessionClaims{}, fmt.Errorf("authn: create session: %w", err)
	}
	return access, csrf, claims, nil
}

func (s *Store) Authenticate(ctx context.Context, access string, now time.Time) (rbac.SessionClaims, error) {
	if s == nil || s.db == nil || strings.TrimSpace(access) == "" {
		return rbac.SessionClaims{}, ErrInvalidSession
	}
	var row Session
	err := s.db.WithContext(ctx).Where("access_token_hash = ? AND expires_at > ?", tokenHash(access), now.UTC()).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return rbac.SessionClaims{}, ErrInvalidSession
	}
	if err != nil {
		return rbac.SessionClaims{}, fmt.Errorf("authn: authenticate session: %w", err)
	}
	principal := rbac.Principal{Type: row.PrincipalType, ID: row.PrincipalID}
	if row.TenantID != nil {
		principal.TenantID = *row.TenantID
	}
	claims := rbac.SessionClaims{SessionID: row.ID, Principal: principal, CSRFTokenHash: row.CSRFTokenHash, ExpiresAt: row.ExpiresAt}
	if !claims.Valid(now.UTC()) {
		return rbac.SessionClaims{}, ErrInvalidSession
	}
	_ = s.db.WithContext(ctx).Model(&Session{}).Where("id = ?", row.ID).Update("last_authenticated_at", now.UTC()).Error
	return claims, nil
}

func (s *Store) Revoke(ctx context.Context, access string) error {
	if s == nil || s.db == nil || strings.TrimSpace(access) == "" {
		return ErrInvalidSession
	}
	return s.db.WithContext(ctx).Where("access_token_hash = ?", tokenHash(access)).Delete(&Session{}).Error
}

// RevokePrincipalTx removes every active session for a principal inside the
// caller's transaction. This keeps credential changes and session revocation
// atomic.
func (s *Store) RevokePrincipalTx(tx *gorm.DB, principal rbac.Principal) error {
	if s == nil || tx == nil || !principal.Valid() {
		return ErrInvalidSession
	}
	query := tx.Where("principal_type = ? AND principal_id = ?", principal.Type, principal.ID)
	if principal.TenantID == "" {
		query = query.Where("tenant_id IS NULL")
	} else {
		query = query.Where("tenant_id = ?", principal.TenantID)
	}
	return query.Delete(&Session{}).Error
}

func (s *Store) DeleteExpired(ctx context.Context, now time.Time) error {
	if s == nil || s.db == nil {
		return ErrInvalidSession
	}
	return s.db.WithContext(ctx).Where("expires_at <= ?", now.UTC()).Delete(&Session{}).Error
}

func tokenHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func randomToken(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("authn: random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func tenantPtr(id tenant.ID) *tenant.ID {
	if id == "" {
		return nil
	}
	return &id
}
