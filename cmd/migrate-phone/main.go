// cmd/migrate-phone — one-time data migration for Step 11.
//
// Converts users.phone_encrypted from plaintext to AES-256-GCM ciphertext
// and re-hashes users.phone_hash with the deployment-wide pepper.
//
// Idempotent: rows already encrypted are detected via GCM auth-tag failure
// on decrypt and skipped (or re-hashed only if the pepper changed).
//
// Usage:
//
//	go run ./cmd/migrate-phone [--dry-run]
//
// The command reads configs/config.yaml (same as the server) plus
// APP_ENCRYPTION_PHONE_KEY / APP_ENCRYPTION_PHONE_PEPPER env vars.
// Run with --dry-run first to preview changes without touching the DB.
//
// Safety:
//   - Processes rows in batches of 200 using cursor pagination (id > last).
//   - Each row is updated in its own UPDATE statement — no transaction
//     wraps the entire table; a crash mid-run is safe to resume.
//   - Soft-deleted rows (deleted_at IS NOT NULL) are skipped — they will
//     never be read by the application again.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"cooking-platform/internal/model"
	"cooking-platform/pkg/config"
	"cooking-platform/pkg/crypto"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

const batchSize = 200

func main() {
	dryRun := flag.Bool("dry-run", false, "print planned changes without writing to DB")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("FATAL: load config: %v", err)
	}

	db, err := initDB(cfg.Database)
	if err != nil {
		log.Fatalf("FATAL: connect db: %v", err)
	}

	keyHex := cfg.Encryption.PhoneKey
	pepper := cfg.Encryption.PhonePepper

	if keyHex == "" && pepper == "" {
		// Dev mode: nothing to do. Exit cleanly so CI doesn't break.
		log.Println("encryption.phone_key and phone_pepper are both empty — nothing to migrate (dev mode)")
		os.Exit(0)
	}

	log.Printf("starting phone encryption migration: dry_run=%v key_set=%v pepper_set=%v",
		*dryRun, keyHex != "", pepper != "")

	stats, err := migrate(db, keyHex, pepper, *dryRun)
	if err != nil {
		log.Fatalf("FATAL: migration: %v", err)
	}

	log.Printf("done: processed=%d encrypted=%d rehashed=%d skipped=%d errors=%d",
		stats.processed, stats.encrypted, stats.rehashed, stats.skipped, stats.errors)

	if stats.errors > 0 {
		os.Exit(1)
	}
}

type stats struct {
	processed int
	encrypted int
	rehashed  int
	skipped   int
	errors    int
}

// migrate iterates all non-deleted users in ascending id order, batch by batch.
// For each user it determines whether phone_encrypted and/or phone_hash need
// updating and applies the changes atomically per-row.
func migrate(db *gorm.DB, keyHex, pepper string, dryRun bool) (stats, error) {
	var s stats
	var lastID int64 = 0

	for {
		var users []model.User

		// Cursor pagination on primary key — stable across concurrent writes.
		res := db.Where("id > ? AND deleted_at IS NULL", lastID).
			Order("id ASC").
			Limit(batchSize).
			Find(&users)
		if res.Error != nil {
			return s, fmt.Errorf("fetch batch after id=%d: %w", lastID, res.Error)
		}
		if len(users) == 0 {
			break
		}

		for _, u := range users {
			s.processed++

			updates, err := computeUpdates(u, keyHex, pepper)
			if err != nil {
				log.Printf("ERROR user_id=%d: %v", u.ID, err)
				s.errors++
				continue
			}

			if len(updates) == 0 {
				s.skipped++
				continue
			}

			_, encUpdated := updates["phone_encrypted"]
			_, hashUpdated := updates["phone_hash"]

			if dryRun {
				log.Printf("DRY user_id=%-10d encrypt=%-5v rehash=%-5v", u.ID, encUpdated, hashUpdated)
				if encUpdated {
					s.encrypted++
				}
				if hashUpdated {
					s.rehashed++
				}
				continue
			}

			if err := db.Model(&model.User{}).
				Where("id = ?", u.ID).
				Updates(updates).Error; err != nil {
				log.Printf("ERROR user_id=%d update: %v", u.ID, err)
				s.errors++
				continue
			}

			if encUpdated {
				s.encrypted++
			}
			if hashUpdated {
				s.rehashed++
			}
		}

		lastID = users[len(users)-1].ID
		if len(users) < batchSize {
			break
		}
	}

	return s, nil
}

// computeUpdates decides what needs changing for a single user row.
//
// Detection heuristic for "is phone_encrypted already ciphertext?":
//   - Call DecryptPhone with the current key.
//   - If it succeeds → the value is already valid ciphertext; extract plain.
//   - If it fails → assume plaintext (valid for Step 3-10 rows where
//     phone_encrypted was written as a raw digit string by user_service).
//     AES-GCM auth-tag authentication makes it impossible for a valid phone
//     number (all digits, ≤15 chars) to pass GCM Open, so false-positives
//     (treating real ciphertext as plaintext) cannot occur when the key is correct.
//
// Returns an empty map when no update is needed (row already up-to-date).
func computeUpdates(u model.User, keyHex, pepper string) (map[string]interface{}, error) {
	updates := make(map[string]interface{}, 2)

	var (
		plain        string
		needsEncrypt bool
	)

	if keyHex != "" {
		decrypted, err := crypto.DecryptPhone(u.PhoneEncrypted, keyHex)
		if err != nil {
			// Decrypt failed → stored value is plaintext. Encrypt it.
			plain = u.PhoneEncrypted
			needsEncrypt = true
		} else {
			plain = decrypted
		}
	} else {
		// No key → phone_encrypted is plaintext by definition (dev mode
		// with a non-empty pepper still needs hash update below).
		plain = u.PhoneEncrypted
	}

	if needsEncrypt {
		ciphertext, err := crypto.EncryptPhone(plain, keyHex)
		if err != nil {
			return nil, fmt.Errorf("encrypt: %w", err)
		}
		updates["phone_encrypted"] = ciphertext
	}

	newHash := crypto.HashPhone(plain, pepper)
	if newHash != u.PhoneHash {
		updates["phone_hash"] = newHash
	}

	return updates, nil
}

func initDB(cfg config.DatabaseConfig) (*gorm.DB, error) {
	db, err := gorm.Open(mysql.Open(cfg.DSN), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("gorm open: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get sql.DB: %w", err)
	}

	sqlDB.SetMaxOpenConns(5)
	sqlDB.SetMaxIdleConns(2)
	sqlDB.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}

	return db, nil
}
