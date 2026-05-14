// Package service contains the business-logic layer. user_service.go
// orchestrates the complete authentication and profile management flow,
// composing repository, cache, JWT, and SMS components.
//
// Boundary rules:
//   - Returns *errcode.AppError for all expected business failures
//     (handler converts to HTTP via response.FromError).
//   - Returns wrapped error for unexpected infrastructure failures
//     (handler maps to 500).
//   - All time-based logic uses time.Now() directly — no clock injection.
//     Tests at this layer use real Redis/MySQL via integration harness.
//
// Added in Step 3 (user module).
package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"cooking-platform/internal/cache"
	"cooking-platform/internal/model"
	"cooking-platform/internal/model/dto"
	"cooking-platform/internal/repository"
	"cooking-platform/pkg/config"
	"cooking-platform/pkg/errcode"
	"cooking-platform/pkg/jwt"
	"cooking-platform/pkg/sms"

	"go.uber.org/zap"
)

// UserService is the entry point for all user-related business operations.
type UserService struct {
	repo      repository.UserRepository
	userCache *cache.UserCache
	jwtMgr    *jwt.Manager
	smsSender sms.Sender
	smsCfg    config.SMSConfig
	rlCfg     config.RatelimitConfig
}

// NewUserService wires the service with its dependencies.
func NewUserService(
	repo repository.UserRepository,
	userCache *cache.UserCache,
	jwtMgr *jwt.Manager,
	smsSender sms.Sender,
	smsCfg config.SMSConfig,
	rlCfg config.RatelimitConfig,
) *UserService {
	return &UserService{
		repo:      repo,
		userCache: userCache,
		jwtMgr:    jwtMgr,
		smsSender: smsSender,
		smsCfg:    smsCfg,
		rlCfg:     rlCfg,
	}
}

// ── Public API ──────────────────────────────────────────────────────────────

// SendCode dispatches a verification code to the user's phone after passing
// three rate-limit checks: per-phone window, per-phone daily, per-IP daily.
//
// On success, the code is stored in Redis (TTL = cfg.SMS.CodeTTL) and the
// caller is informed of the remaining seconds.
func (s *UserService) SendCode(ctx context.Context, phone, clientIP string) (*dto.SendCodeResp, error) {
	phoneHash := hashPhone(phone)
	ipHash := hashIP(clientIP)

	// 1. Per-phone window — "no more than one send within the window".
	allowed, retryAfter, err := s.userCache.CheckAndConsumeSMSWindow(ctx, phoneHash, s.rlCfg.SMSPhoneWindow)
	if err != nil {
		return nil, fmt.Errorf("sms window check: %w", err)
	}
	if !allowed {
		zap.L().Info("sms window rejected",
			zap.String("phone_hash", phoneHash),
			zap.Duration("retry_after", retryAfter),
		)
		return nil, errcode.ErrSMSWindow
	}

	// 2. Per-phone daily — "max N sends to this phone in 24h".
	allowed, err = s.userCache.IncrementAndCheckSMSPhoneDaily(ctx, phoneHash, s.rlCfg.SMSPerPhonePerDay)
	if err != nil {
		return nil, fmt.Errorf("sms phone daily check: %w", err)
	}
	if !allowed {
		zap.L().Info("sms daily phone limit reached", zap.String("phone_hash", phoneHash))
		return nil, errcode.ErrSMSDailyPhone
	}

	// 3. Per-IP daily — "max N sends from this IP in 24h".
	allowed, err = s.userCache.IncrementAndCheckSMSIPDaily(ctx, ipHash, s.rlCfg.SMSPerIPPerDay)
	if err != nil {
		return nil, fmt.Errorf("sms ip daily check: %w", err)
	}
	if !allowed {
		zap.L().Info("sms daily ip limit reached", zap.String("ip_hash", ipHash))
		return nil, errcode.ErrSMSDailyIP
	}

	// All limits passed — generate, persist, and dispatch the code.
	code, err := generateNumericCode(s.smsCfg.CodeLength)
	if err != nil {
		return nil, fmt.Errorf("generate code: %w", err)
	}

	if err := s.userCache.SaveSMSCode(ctx, phoneHash, code, s.smsCfg.CodeTTL); err != nil {
		return nil, fmt.Errorf("save sms code: %w", err)
	}

	if err := s.smsSender.SendCode(ctx, phone, code); err != nil {
		// SMS dispatch failure: clean up the saved code so the user can retry
		// without burning a send-quota slot.
		_ = s.userCache.DeleteSMSCode(ctx, phoneHash)
		return nil, fmt.Errorf("send sms: %w", err)
	}

	return &dto.SendCodeResp{
		ExpiresIn: int(s.smsCfg.CodeTTL.Seconds()),
	}, nil
}

// Login verifies the SMS code and either logs in an existing user or creates
// a new one. Returns a fully-populated LoginResp including both tokens.
func (s *UserService) Login(ctx context.Context, phone, code string) (*dto.LoginResp, error) {
	phoneHash := hashPhone(phone)

	// 1. Verify the code.
	storedCode, err := s.userCache.GetSMSCode(ctx, phoneHash)
	if err != nil {
		if errors.Is(err, cache.ErrCacheNotFound) {
			return nil, errcode.ErrCodeNotFound
		}
		return nil, fmt.Errorf("get sms code: %w", err)
	}
	if storedCode != code {
		return nil, errcode.ErrCodeMismatch
	}
	// One-time use — delete after successful verification.
	if err := s.userCache.DeleteSMSCode(ctx, phoneHash); err != nil {
		// Non-fatal: log and continue. Worst case the code can be reused
		// once more before TTL expiry.
		zap.L().Warn("delete sms code after verification failed",
			zap.String("phone_hash", phoneHash),
			zap.Error(err),
		)
	}

	// 2. Find or create the user.
	user, err := s.repo.FindByPhoneHash(ctx, phoneHash)
	if err != nil {
		if !errors.Is(err, repository.ErrUserNotFound) {
			return nil, fmt.Errorf("find user: %w", err)
		}
		// First-time login → auto-register.
		//
		// CreatedAt/UpdatedAt are set explicitly rather than left to GORM's
		// autoCreateTime. The model field carries a `default:CURRENT_TIMESTAMP(3)`
		// tag, which makes GORM v2 treat the column as DB-generated: it skips
		// both Go-side fillup AND the post-insert read-back. The DB row gets
		// the right timestamp, but the in-memory `user` handed to toPublicResp
		// keeps time.Time's zero value (serialises as -62135596800000). This is
		// bug R-1 from the Step 6 verification report. Setting time.Now() here
		// makes the login response reliable without a SELECT after INSERT —
		// the same fix post_service.Create already applies for posts.
		now := time.Now()
		user = &model.User{
			PhoneHash:      phoneHash,
			PhoneEncrypted: phone, // Step 3: plaintext. Step 11: AES-GCM ciphertext.
			Nickname:       defaultNickname(phone),
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if err := s.repo.Create(ctx, user); err != nil {
			return nil, fmt.Errorf("create user: %w", err)
		}
		zap.L().Info("new user auto-registered", zap.Int64("user_id", user.ID))
	}

	// 3. Banned-user check (status field is the source of truth).
	if user.IsBanned() {
		return nil, errcode.ErrUserBanned
	}

	// 4. Issue token pair.
	pair, err := s.issueTokenPair(user.ID)
	if err != nil {
		return nil, fmt.Errorf("issue tokens: %w", err)
	}

	return &dto.LoginResp{
		TokenPair: *pair,
		User:      toPublicResp(user),
	}, nil
}

// Refresh exchanges a valid refresh token for a new access+refresh pair.
// The old refresh token remains valid until natural expiry (no rotation).
func (s *UserService) Refresh(ctx context.Context, refreshToken string) (*dto.TokenPair, error) {
	claims, err := s.jwtMgr.Parse(refreshToken)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, errcode.ErrTokenExpired
		}
		return nil, errcode.ErrTokenInvalid
	}

	// Optional safety: reject if user is banned (handle hot-revocation case).
	user, err := s.repo.FindByID(ctx, claims.UserID)
	if err != nil {
		if errors.Is(err, repository.ErrUserNotFound) {
			return nil, errcode.ErrUserNotFound
		}
		return nil, fmt.Errorf("find user: %w", err)
	}
	if user.IsBanned() {
		return nil, errcode.ErrUserBanned
	}

	pair, err := s.issueTokenPair(claims.UserID)
	if err != nil {
		return nil, fmt.Errorf("issue tokens: %w", err)
	}
	return pair, nil
}

// Logout blacklists the access token's JTI for its remaining lifetime.
// The refresh token is NOT blacklisted: clients are expected to discard it
// locally on logout. Server-side refresh blacklisting requires storing every
// active refresh token, which we explicitly avoid for MVP simplicity.
func (s *UserService) Logout(ctx context.Context, accessToken string) error {
	claims, err := s.jwtMgr.Parse(accessToken)
	if err != nil {
		// Already invalid token — logout is idempotent, succeed silently.
		return nil
	}
	remaining := time.Until(claims.ExpiresAt.Time)
	if remaining <= 0 {
		return nil
	}
	if err := s.userCache.BlacklistJWT(ctx, claims.JTI, remaining); err != nil {
		return fmt.Errorf("blacklist jti: %w", err)
	}
	return nil
}

// GetMyProfile returns the authenticated user's private profile (with masked phone).
func (s *UserService) GetMyProfile(ctx context.Context, userID int64) (*dto.UserPrivateResp, error) {
	user, err := s.repo.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, repository.ErrUserNotFound) {
			return nil, errcode.ErrUserNotFound
		}
		return nil, fmt.Errorf("find user: %w", err)
	}
	resp := dto.UserPrivateResp{
		UserPublicResp: toPublicResp(user),
		PhoneMasked:    maskPhone(user.PhoneEncrypted),
	}
	return &resp, nil
}

// GetPublicProfile returns any user's public profile (no PII).
func (s *UserService) GetPublicProfile(ctx context.Context, userID int64) (*dto.UserPublicResp, error) {
	user, err := s.repo.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, repository.ErrUserNotFound) {
			return nil, errcode.ErrUserNotFound
		}
		return nil, fmt.Errorf("find user: %w", err)
	}
	resp := toPublicResp(user)
	return &resp, nil
}

// UpdateProfile applies partial updates to the authenticated user's profile.
//
// Whitelist semantics: only nickname/avatar_url/bio are mutable here.
// Counters, status, phone, and timestamps cannot be changed via this path.
func (s *UserService) UpdateProfile(ctx context.Context, userID int64, req dto.UpdateProfileReq) error {
	updates := make(map[string]interface{}, 3)

	if req.Nickname != nil {
		nickname := strings.TrimSpace(*req.Nickname)
		if nickname == "" {
			return errcode.ErrNicknameInvalid
		}
		updates["nickname"] = nickname
	}
	if req.Bio != nil {
		// Empty bio is allowed (means "clear my bio").
		updates["bio"] = strings.TrimSpace(*req.Bio)
	}
	if req.AvatarURL != nil {
		// Whitelist: only OSS URLs from our domain. Empty string = clear avatar.
		// MVP: accept any string up to 500 chars; tighten in Step 9 when OSS lands.
		updates["avatar_url"] = strings.TrimSpace(*req.AvatarURL)
	}

	if len(updates) == 0 {
		return nil // No-op update is fine.
	}

	if err := s.repo.UpdateProfile(ctx, userID, updates); err != nil {
		return fmt.Errorf("update profile: %w", err)
	}

	// Invalidate cache so the next read sees fresh data.
	if err := s.userCache.DeleteUserInfo(ctx, userID); err != nil {
		// Non-fatal: TTL will eventually clear the stale entry.
		zap.L().Warn("invalidate user cache failed",
			zap.Int64("user_id", userID),
			zap.Error(err),
		)
	}
	return nil
}

// VerifyAccessToken is called by the Auth middleware on every protected
// request. It performs three checks:
//  1. JWT signature/expiry valid (jwtMgr.Parse)
//  2. JTI not blacklisted (cache.IsJWTBlacklisted)
//  3. User not banned (cache.IsUserBanned — fast path; falls back to DB if cache miss)
//
// Returns the user_id and JTI on success.
func (s *UserService) VerifyAccessToken(ctx context.Context, token string) (userID int64, jti string, err error) {
	claims, err := s.jwtMgr.Parse(token)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return 0, "", errcode.ErrTokenExpired
		}
		return 0, "", errcode.ErrTokenInvalid
	}

	blacklisted, bErr := s.userCache.IsJWTBlacklisted(ctx, claims.JTI)
	if bErr != nil {
		// Fail open: log and continue. Risk window bounded by access TTL.
		zap.L().Warn("jwt blacklist check failed", zap.Error(bErr))
	} else if blacklisted {
		return 0, "", errcode.ErrTokenInvalid
	}

	banned, banErr := s.userCache.IsUserBanned(ctx, claims.UserID)
	if banErr != nil {
		zap.L().Warn("user ban check failed", zap.Error(banErr))
	} else if banned {
		return 0, "", errcode.ErrUserBanned
	}

	return claims.UserID, claims.JTI, nil
}

// ── Private helpers ─────────────────────────────────────────────────────────

func (s *UserService) issueTokenPair(userID int64) (*dto.TokenPair, error) {
	access, _, accessExp, err := s.jwtMgr.IssueAccessToken(userID)
	if err != nil {
		return nil, err
	}
	refresh, _, _, err := s.jwtMgr.IssueRefreshToken(userID)
	if err != nil {
		return nil, err
	}
	return &dto.TokenPair{
		AccessToken:          access,
		RefreshToken:         refresh,
		AccessTokenExpiresAt: accessExp.UnixMilli(),
		TokenType:            "Bearer",
	}, nil
}

// hashPhone produces the SHA-256 hex digest used as the canonical user identity key.
//
// Step 3: plain SHA-256 of the phone number.
// Step 11: will be salted with a deployment-wide pepper to defeat rainbow tables.
// The output length (64 chars) is unchanged across the migration, so callers
// don't need to handle versioning.
func hashPhone(phone string) string {
	sum := sha256.Sum256([]byte(phone))
	return hex.EncodeToString(sum[:])
}

// hashIP produces the SHA-256 hex digest of the client IP for use in
// rate-limit keys. Hashing IPs avoids storing PII in Redis (GDPR-aligned habit).
func hashIP(ip string) string {
	sum := sha256.Sum256([]byte(ip))
	return hex.EncodeToString(sum[:])
}

// generateNumericCode produces a random N-digit numeric verification code
// using crypto/rand for unpredictability. math/rand is unsuitable: predictable
// from a small seed and an adversary could brute-force the next code.
func generateNumericCode(length int) (string, error) {
	const digits = "0123456789"
	max := big.NewInt(int64(len(digits)))

	var b strings.Builder
	b.Grow(length)
	for i := 0; i < length; i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b.WriteByte(digits[n.Int64()])
	}
	return b.String(), nil
}

// defaultNickname is generated from the last 4 digits of the phone number
// for first-time auto-registration. Users can change it via PATCH /users/me.
//
// Example: phone="13800138000" → nickname="厨友8000"
//
// Localised "厨友" prefix matches the product's warm, casual tone (PRD-Phase1).
func defaultNickname(phone string) string {
	if len(phone) < 4 {
		return "厨友"
	}
	return "厨友" + phone[len(phone)-4:]
}

// maskPhone displays "138****8000" given "13800138000". Used in private
// profile responses so the user sees a recognisable but partially-redacted
// version of their number.
//
// Step 11: when phone_encrypted holds AES-GCM ciphertext, this function will
// be updated to decrypt first. For Step 3 (plaintext), it operates directly
// on the stored value.
func maskPhone(phone string) string {
	if len(phone) < 7 {
		return phone
	}
	return phone[:3] + "****" + phone[len(phone)-4:]
}

// toPublicResp converts a model.User to its public DTO form.
func toPublicResp(u *model.User) dto.UserPublicResp {
	return dto.UserPublicResp{
		ID:             u.ID,
		Nickname:       u.Nickname,
		AvatarURL:      u.AvatarURL,
		Bio:            u.Bio,
		PostCount:      u.PostCount,
		TotalLikes:     u.TotalLikes,
		FollowerCount:  u.FollowerCount,
		FollowingCount: u.FollowingCount,
		CreatedAt:      u.CreatedAt.UnixMilli(),
	}
}
