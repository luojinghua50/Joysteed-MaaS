// Package virtualkey owns MaaS-managed tenant credentials. MaaS is the source
// of truth; Bifrost receives an eventually consistent, idempotent projection.
package virtualkey

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

	"github.com/luojinghua50/Joysteed-MaaS/internal/configbus"
	"github.com/luojinghua50/Joysteed-MaaS/internal/controlplane"
	tenant "github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Status string

const (
	StatusPending  Status = "pending"
	StatusActive   Status = "active"
	StatusFailed   Status = "failed"
	StatusRevoking Status = "revoking"
	StatusRevoked  Status = "revoked"
)

type DesiredState string

const (
	DesiredActive  DesiredState = "active"
	DesiredRevoked DesiredState = "revoked"
)

var (
	ErrDatabaseRequired  = errors.New("virtualkey: database is required")
	ErrCipherRequired    = errors.New("virtualkey: cipher is required")
	ErrTenantRequired    = errors.New("virtualkey: tenant id is required")
	ErrNameRequired      = errors.New("virtualkey: name is required")
	ErrKeyNotFound       = errors.New("virtualkey: key not found")
	ErrKeyUnavailable    = errors.New("virtualkey: key is unavailable")
	ErrInvalidTransition = errors.New("virtualkey: invalid state transition")
)

const entityPrefix = "virtual_key:"

// Key is safe to serialize: the encrypted recoverable credential is excluded
// from JSON so list responses and audit snapshots cannot disclose it.
type Key struct {
	ID                string       `gorm:"column:id;type:text;primaryKey" json:"id"`
	TenantID          tenant.ID    `gorm:"column:tenant_id;type:text;not null;uniqueIndex:idx_tenant_virtual_key_name,priority:1;index" json:"tenant_id"`
	Name              string       `gorm:"column:name;type:text;not null;uniqueIndex:idx_tenant_virtual_key_name,priority:2" json:"name"`
	SecretCiphertext  string       `gorm:"column:secret_ciphertext;type:text;not null" json:"-"`
	SecretFingerprint string       `gorm:"column:secret_fingerprint;type:text;not null;index" json:"secret_fingerprint"`
	Status            Status       `gorm:"column:status;type:text;not null;index" json:"status"`
	DesiredState      DesiredState `gorm:"column:desired_state;type:text;not null" json:"-"`
	DesiredGeneration uint64       `gorm:"column:desired_generation;not null" json:"desired_generation"`
	AppliedGeneration uint64       `gorm:"column:applied_generation;not null;default:0" json:"applied_generation"`
	LastError         string       `gorm:"column:last_error;type:text" json:"last_error,omitempty"`
	ActivatedAt       *time.Time   `gorm:"column:activated_at" json:"activated_at,omitempty"`
	RevokedAt         *time.Time   `gorm:"column:revoked_at" json:"revoked_at,omitempty"`
	CreatedAt         time.Time    `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt         time.Time    `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (Key) TableName() string { return "tenant_virtual_keys" }

type Mutation struct {
	Key    *Key
	Before *Key
	Secret string
	Change *configbus.Change
}

type Store struct {
	db     *gorm.DB
	cipher *Cipher
	bus    *configbus.Store
}

func NewStore(db *gorm.DB, cipher *Cipher) (*Store, error) {
	if db == nil {
		return nil, ErrDatabaseRequired
	}
	if cipher == nil {
		return nil, ErrCipherRequired
	}
	return &Store{db: db, cipher: cipher, bus: configbus.NewStore(db)}, nil
}

func (s *Store) DB() *gorm.DB { return s.db }

func (s *Store) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return ErrDatabaseRequired
	}
	if err := s.db.WithContext(ctx).AutoMigrate(&Key{}); err != nil {
		return fmt.Errorf("virtualkey: migrate keys: %w", err)
	}
	if err := s.bus.Migrate(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Store) List(ctx context.Context, tenantID tenant.ID, limit int) ([]Key, error) {
	if tenantID == "" {
		return nil, ErrTenantRequired
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var rows []Key
	if err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).
		Order("created_at DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("virtualkey: list tenant keys: %w", err)
	}
	return rows, nil
}

// RevealTx decrypts one tenant-owned credential for an explicitly authorized
// portal request. List responses continue to expose metadata only.
func (s *Store) RevealTx(tx *gorm.DB, tenantID tenant.ID, keyID string) (*Key, string, error) {
	if tx == nil {
		return nil, "", ErrDatabaseRequired
	}
	if tenantID == "" {
		return nil, "", ErrTenantRequired
	}
	var row Key
	err := tx.Where("tenant_id = ? AND id = ?", tenantID, strings.TrimSpace(keyID)).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, "", ErrKeyNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("virtualkey: load key for reveal: %w", err)
	}
	if row.DesiredState != DesiredActive || row.Status == StatusRevoking || row.Status == StatusRevoked {
		return nil, "", ErrKeyUnavailable
	}
	secret, err := s.cipher.Decrypt(row.SecretCiphertext, associatedData(string(row.TenantID), row.ID))
	if err != nil {
		return nil, "", fmt.Errorf("virtualkey: decrypt key for reveal: %w", err)
	}
	return &row, secret, nil
}

func (s *Store) CreateTx(tx *gorm.DB, tenantID tenant.ID, name string) (*Mutation, error) {
	if tx == nil {
		return nil, ErrDatabaseRequired
	}
	name = strings.TrimSpace(name)
	if tenantID == "" {
		return nil, ErrTenantRequired
	}
	if name == "" {
		return nil, ErrNameRequired
	}
	if len(name) > 120 {
		return nil, fmt.Errorf("%w: maximum length is 120", ErrNameRequired)
	}
	var tenantCount int64
	if err := tx.Model(&controlplane.Tenant{}).Where("id = ?", tenantID).Count(&tenantCount).Error; err != nil {
		return nil, fmt.Errorf("virtualkey: verify tenant: %w", err)
	}
	if tenantCount != 1 {
		return nil, fmt.Errorf("%w: %s", controlplane.ErrTenantNotFound, tenantID)
	}

	id, err := randomID("vk_")
	if err != nil {
		return nil, err
	}
	secret, err := randomSecret()
	if err != nil {
		return nil, err
	}
	ciphertext, err := s.cipher.Encrypt(secret, associatedData(string(tenantID), id))
	if err != nil {
		return nil, err
	}
	change, err := s.bus.PublishTx(tx, tenantID, entityPrefix+id)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	row := &Key{
		ID: id, TenantID: tenantID, Name: name, SecretCiphertext: ciphertext,
		SecretFingerprint: fingerprint(secret), Status: StatusPending,
		DesiredState: DesiredActive, DesiredGeneration: change.Generation,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := tx.Create(row).Error; err != nil {
		return nil, fmt.Errorf("virtualkey: create key: %w", err)
	}
	return &Mutation{Key: row, Secret: secret, Change: change}, nil
}

func (s *Store) RevokeTx(tx *gorm.DB, tenantID tenant.ID, keyID string) (*Mutation, error) {
	row, err := lockKey(tx, tenantID, keyID)
	if err != nil {
		return nil, err
	}
	if row.Status == StatusRevoked || row.Status == StatusRevoking {
		return &Mutation{Key: row}, nil
	}
	before := *row
	change, err := s.bus.PublishTx(tx, tenantID, entityPrefix+keyID)
	if err != nil {
		return nil, err
	}
	row.Status = StatusRevoking
	row.DesiredState = DesiredRevoked
	row.DesiredGeneration = change.Generation
	row.LastError = ""
	row.UpdatedAt = time.Now().UTC()
	if err := tx.Save(row).Error; err != nil {
		return nil, fmt.Errorf("virtualkey: mark key revoking: %w", err)
	}
	return &Mutation{Key: row, Before: &before, Change: change}, nil
}

func (s *Store) RetryTx(tx *gorm.DB, tenantID tenant.ID, keyID string) (*Mutation, error) {
	row, err := lockKey(tx, tenantID, keyID)
	if err != nil {
		return nil, err
	}
	if row.Status != StatusFailed {
		return nil, fmt.Errorf("%w: only failed keys can be retried", ErrInvalidTransition)
	}
	before := *row
	change, err := s.bus.PublishTx(tx, tenantID, entityPrefix+keyID)
	if err != nil {
		return nil, err
	}
	if row.DesiredState == DesiredRevoked {
		row.Status = StatusRevoking
	} else {
		row.Status = StatusPending
	}
	row.DesiredGeneration = change.Generation
	row.LastError = ""
	row.UpdatedAt = time.Now().UTC()
	if err := tx.Save(row).Error; err != nil {
		return nil, fmt.Errorf("virtualkey: retry key: %w", err)
	}
	return &Mutation{Key: row, Before: &before, Change: change}, nil
}

func lockKey(tx *gorm.DB, tenantID tenant.ID, keyID string) (*Key, error) {
	if tx == nil {
		return nil, ErrDatabaseRequired
	}
	if tenantID == "" {
		return nil, ErrTenantRequired
	}
	var row Key
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("tenant_id = ? AND id = ?", tenantID, strings.TrimSpace(keyID)).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("virtualkey: lock key: %w", err)
	}
	return &row, nil
}

func randomID(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("virtualkey: generate id: %w", err)
	}
	return prefix + hex.EncodeToString(b), nil
}

func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("virtualkey: generate secret: %w", err)
	}
	return "sk-bf-" + base64.RawURLEncoding.EncodeToString(b), nil
}

func fingerprint(secret string) string {
	hash := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(hash[:8])
}
