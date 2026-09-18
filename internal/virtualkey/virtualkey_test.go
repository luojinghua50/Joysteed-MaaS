package virtualkey

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/configbus"
	"github.com/luojinghua50/Joysteed-MaaS/internal/controlplane"
	tenant "github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

const testEncryptionMaterial = "0123456789abcdef0123456789abcdef"

func TestCipherRoundTripAndBinding(t *testing.T) {
	cipher, err := NewCipher(testEncryptionMaterial)
	require.NoError(t, err)
	ciphertext, err := cipher.Encrypt("sk-bf-secret", associatedData("tenant-a", "key-a"))
	require.NoError(t, err)
	require.NotContains(t, ciphertext, "sk-bf-secret")
	plaintext, err := cipher.Decrypt(ciphertext, associatedData("tenant-a", "key-a"))
	require.NoError(t, err)
	require.Equal(t, "sk-bf-secret", plaintext)
	_, err = cipher.Decrypt(ciphertext, associatedData("tenant-b", "key-a"))
	require.Error(t, err)
}

func TestCreateWritesKeyAndOutboxAtomicallyWithoutSerializingSecret(t *testing.T) {
	store, db := testKeyStore(t)
	var created *Mutation
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		created, err = store.CreateTx(tx, "tenant-a", "Production")
		return err
	}))
	require.NotEmpty(t, created.Secret)
	require.NotEqual(t, created.Secret, created.Key.SecretCiphertext)
	require.Equal(t, StatusPending, created.Key.Status)
	require.Equal(t, uint64(1), created.Key.DesiredGeneration)

	encoded, err := json.Marshal(created.Key)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), created.Secret)
	require.NotContains(t, string(encoded), created.Key.SecretCiphertext)

	changes, err := configbus.NewStore(db).ListChanges(context.Background(), 0, 10)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	require.NotContains(t, changes[0].Entity, created.Secret)

	rollbackErr := db.Transaction(func(tx *gorm.DB) error {
		_, err := store.CreateTx(tx, "tenant-a", "Rolled back")
		return errors.Join(err, errors.New("force rollback"))
	})
	require.Error(t, rollbackErr)
	keys, err := store.List(context.Background(), "tenant-a", 10)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	changes, err = configbus.NewStore(db).ListChanges(context.Background(), 0, 10)
	require.NoError(t, err)
	require.Len(t, changes, 1)
}

func TestRevealTxIsTenantScopedAndRejectsRevokedKeys(t *testing.T) {
	store, db := testKeyStore(t)
	created := createKey(t, store, db)
	var secret string
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		_, varSecret, err := store.RevealTx(tx, "tenant-a", created.Key.ID)
		secret = varSecret
		return err
	}))
	require.Equal(t, created.Secret, secret)

	err := db.Transaction(func(tx *gorm.DB) error {
		_, _, revealErr := store.RevealTx(tx, "tenant-b", created.Key.ID)
		return revealErr
	})
	require.ErrorIs(t, err, ErrKeyNotFound)

	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		_, revokeErr := store.RevokeTx(tx, "tenant-a", created.Key.ID)
		return revokeErr
	}))
	err = db.Transaction(func(tx *gorm.DB) error {
		_, _, revealErr := store.RevealTx(tx, "tenant-a", created.Key.ID)
		return revealErr
	})
	require.ErrorIs(t, err, ErrKeyUnavailable)
}

func TestProjectorIsIdempotentAndRevokes(t *testing.T) {
	store, db := testKeyStore(t)
	created := createKey(t, store, db)
	target := &fakeTarget{}
	projector, err := NewProjector(db, store.cipher, target)
	require.NoError(t, err)

	require.NoError(t, projector.ProjectTenant(context.Background(), "tenant-a", created.Change.Generation))
	require.Equal(t, 1, target.upserts)
	require.Equal(t, created.Secret, target.last.Secret)
	row := loadKey(t, db, created.Key.ID)
	require.Equal(t, StatusActive, row.Status)
	require.Equal(t, row.DesiredGeneration, row.AppliedGeneration)

	require.NoError(t, projector.ProjectTenant(context.Background(), "tenant-a", created.Change.Generation))
	require.Equal(t, 1, target.upserts, "duplicate generation must not rewrite the data plane")

	var revoked *Mutation
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		revoked, err = store.RevokeTx(tx, "tenant-a", created.Key.ID)
		return err
	}))
	require.Equal(t, StatusRevoking, revoked.Key.Status)
	require.NoError(t, projector.ProjectTenant(context.Background(), "tenant-a", revoked.Change.Generation))
	require.Equal(t, []string{created.Key.ID}, target.deletes)
	row = loadKey(t, db, created.Key.ID)
	require.Equal(t, StatusRevoked, row.Status)
	require.NotNil(t, row.RevokedAt)
}

func TestFreshProjectorRestoresAlreadyAppliedActiveKeys(t *testing.T) {
	store, db := testKeyStore(t)
	created := createKey(t, store, db)
	firstTarget := &fakeTarget{}
	first, err := NewProjector(db, store.cipher, firstTarget)
	require.NoError(t, err)
	require.NoError(t, first.ProjectTenant(context.Background(), "tenant-a", created.Change.Generation))
	require.Equal(t, 1, firstTarget.upserts)

	// A new Projector represents a restarted gateway with an empty governance
	// cache. The control-plane row is already fenced as applied, but must still
	// be replayed once into this process and its Bifrost target.
	freshTarget := &fakeTarget{}
	fresh, err := NewProjector(db, store.cipher, freshTarget)
	require.NoError(t, err)
	require.NoError(t, fresh.ProjectTenant(context.Background(), "tenant-a", created.Change.Generation))
	require.Equal(t, 1, freshTarget.upserts)
	require.Equal(t, created.Secret, freshTarget.last.Secret)

	require.NoError(t, fresh.ProjectTenant(context.Background(), "tenant-a", created.Change.Generation))
	require.Equal(t, 1, freshTarget.upserts, "one process restores each tenant snapshot once")
}

func TestProjectionFailureIsSanitizedAndRetryable(t *testing.T) {
	store, db := testKeyStore(t)
	created := createKey(t, store, db)
	target := &fakeTarget{upsertErr: fmt.Errorf("backend rejected %s", created.Secret)}
	projector, err := NewProjector(db, store.cipher, target)
	require.NoError(t, err)

	err = projector.ProjectTenant(context.Background(), "tenant-a", created.Change.Generation)
	require.Error(t, err)
	row := loadKey(t, db, created.Key.ID)
	require.Equal(t, StatusFailed, row.Status)
	require.NotContains(t, row.LastError, created.Secret)
	require.Contains(t, row.LastError, "[redacted]")

	target.upsertErr = nil
	var retry *Mutation
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		retry, err = store.RetryTx(tx, "tenant-a", created.Key.ID)
		return err
	}))
	require.Equal(t, StatusPending, retry.Key.Status)
	require.Greater(t, retry.Change.Generation, created.Change.Generation)
	require.NoError(t, projector.ProjectTenant(context.Background(), "tenant-a", retry.Change.Generation))
	row = loadKey(t, db, created.Key.ID)
	require.Equal(t, StatusActive, row.Status)
	require.Empty(t, row.LastError)
}

type fakeTarget struct {
	mu        sync.Mutex
	upserts   int
	last      Projection
	deletes   []string
	upsertErr error
}

func (f *fakeTarget) Upsert(_ context.Context, projection Projection) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts++
	f.last = projection
	return f.upsertErr
}

func (f *fakeTarget) Delete(_ context.Context, _ tenant.ID, keyID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, keyID)
	return nil
}

func testKeyStore(t *testing.T) (*Store, *gorm.DB) {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&controlplane.Tenant{}))
	require.NoError(t, db.Create(&controlplane.Tenant{ID: "tenant-a", Slug: "tenant-a", Name: "Tenant A", Status: controlplane.StatusActive}).Error)
	cipher, err := NewCipher(testEncryptionMaterial)
	require.NoError(t, err)
	store, err := NewStore(db, cipher)
	require.NoError(t, err)
	require.NoError(t, store.Migrate(context.Background()))
	t.Cleanup(func() { sqlDB, _ := db.DB(); _ = sqlDB.Close() })
	return store, db
}

func createKey(t *testing.T, store *Store, db *gorm.DB) *Mutation {
	t.Helper()
	var mutation *Mutation
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		mutation, err = store.CreateTx(tx, "tenant-a", "Production")
		return err
	}))
	return mutation
}

func loadKey(t *testing.T, db *gorm.DB, id string) Key {
	t.Helper()
	var row Key
	require.NoError(t, db.Where("id = ?", id).Take(&row).Error)
	return row
}
