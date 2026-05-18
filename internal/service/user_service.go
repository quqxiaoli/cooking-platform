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
// Added in Step 3 (user module). Step 9 added OSS whitelist enforcement
// on avatar_url updates.
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
	"cooking-platform/pkg/crypto"
	"cooking-platform/pkg/errcode"
	"cooking-platform/pkg/jwt"
	"cooking-platform/pkg/oss"
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
	ossCfg    config.OSSConfig    // [Step 9] for avatar_url whitelist
	encCfg    config.EncryptionConfig // [Step 11] phone AES-GCM key + pepper
	rlCfg     config.RatelimitConfig
}

// NewUserService wires the service with its dependencies.
//
// [Step 9] ossCfg is held so UpdateProfile can enforce the avatar_url
// whitelist via oss.IsAllowedURL — defence in depth on top of the upload
// callback flow.
// [Step 11] encCfg carries the AES-256-GCM key (PhoneKey) and pepper
// (PhonePepper) for phone field-level encryption. Empty values are safe
// in dev — encryption degrades to a no-op (see pkg/crypto/phone.go).
func NewUserService(
	repo repository.UserRepository,
	userCache *cache.UserCache,
	jwtMgr *jwt.Manager,
	smsSender sms.Sender,
	smsCfg config.SMSConfig,
	ossCfg config.OSSConfig,
	encCfg config.EncryptionConfig,
	rlCfg config.RatelimitConfig,
) *UserService {
	return &UserService{
		repo:      repo,
		userCache: userCache,
		jwtMgr:    jwtMgr,
		smsSender: smsSender,
		smsCfg:    smsCfg,
		ossCfg:    ossCfg,
		encCfg:    encCfg,
		rlCfg:     rlCfg,
	}
}

// ── Public API ──────────────────────────────────────────────────────────────

// SendCode dispatches a verification code to the user's phone after passing
// three rate-limit checks: per-phone window, per-phone daily, per-IP daily.
func (s *UserService) SendCode(ctx context.Context, phone, clientIP string) (*dto.SendCodeResp, error) {
	phoneHash := s.hashPhone(phone)
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

	// 2. Per-phone daily.
	allowed, err = s.userCache.IncrementAndCheckSMSPhoneDaily(ctx, phoneHash, s.rlCfg.SMSPerPhonePerDay)
	if err != nil {
		return nil, fmt.Errorf("sms phone daily check: %w", err)
	}
	if !allowed {
		zap.L().Info("sms daily phone limit reached", zap.String("phone_hash", phoneHash))
		return nil, errcode.ErrSMSDailyPhone
	}

	// 3. Per-IP daily.
	allowed, err = s.userCache.IncrementAndCheckSMSIPDaily(ctx, ipHash, s.rlCfg.SMSPerIPPerDay)
	if err != nil {
		return nil, fmt.Errorf("sms ip daily check: %w", err)
	}
	if !allowed {
		zap.L().Info("sms daily ip limit reached", zap.String("ip_hash", ipHash))
		return nil, errcode.ErrSMSDailyIP
	}

	code, err := generateNumericCode(s.smsCfg.CodeLength)
	if err != nil {
		return nil, fmt.Errorf("generate code: %w", err)
	}

	if err := s.userCache.SaveSMSCode(ctx, phoneHash, code, s.smsCfg.CodeTTL); err != nil {
		return nil, fmt.Errorf("save sms code: %w", err)
	}

	if err := s.smsSender.SendCode(ctx, phone, code); err != nil {
		_ = s.userCache.DeleteSMSCode(ctx, phoneHash)
		return nil, fmt.Errorf("send sms: %w", err)
	}

	return &dto.SendCodeResp{
		ExpiresIn: int(s.smsCfg.CodeTTL.Seconds()),
	}, nil
}

// Login verifies the SMS code and either logs in an existing user or creates
// a new one.
func (s *UserService) Login(ctx context.Context, phone, code string) (*dto.LoginResp, error) {
	phoneHash := s.hashPhone(phone)

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
	if err := s.userCache.DeleteSMSCode(ctx, phoneHash); err != nil {
		zap.L().Warn("delete sms code after verification failed",
			zap.String("phone_hash", phoneHash),
			zap.Error(err),
		)
	}

	user, err := s.repo.FindByPhoneHash(ctx, phoneHash)
	if err != nil {
		if !errors.Is(err, repository.ErrUserNotFound) {
			return nil, fmt.Errorf("find user: %w", err)
		}
		// First-time login → auto-register. CreatedAt/UpdatedAt set
		// explicitly per the Step 6 R-1 fix (default:CURRENT_TIMESTAMP(3)
		// makes GORM skip Go-side fillup, so the returned struct's
		// timestamps would otherwise be zero).
		encrypted, encErr := crypto.EncryptPhone(phone, s.encCfg.PhoneKey)
		if encErr != nil {
			zap.L().Error("encrypt phone failed on register",
				zap.String("phone_hash", phoneHash),
				zap.Error(encErr),
			)
			if errors.Is(encErr, crypto.ErrEmptyKey) {
				return nil, errcode.ErrPhoneKeyMissing
			}
			return nil, errcode.ErrEncryptPhone
		}
		now := time.Now()
		user = &model.User{
			PhoneHash:      phoneHash,
			PhoneEncrypted: encrypted, // [Step 11] AES-GCM ciphertext; plaintext in dev (key="")
			Nickname:       defaultNickname(phone),
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if err := s.repo.Create(ctx, user); err != nil {
			return nil, fmt.Errorf("create user: %w", err)
		}
		zap.L().Info("new user auto-registered", zap.Int64("user_id", user.ID))
	}

	if user.IsBanned() {
		return nil, errcode.ErrUserBanned
	}

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
func (s *UserService) Refresh(ctx context.Context, refreshToken string) (*dto.TokenPair, error) {
	claims, err := s.jwtMgr.Parse(refreshToken)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, errcode.ErrTokenExpired
		}
		return nil, errcode.ErrTokenInvalid
	}

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
func (s *UserService) Logout(ctx context.Context, accessToken string) error {
	claims, err := s.jwtMgr.Parse(accessToken)
	if err != nil {
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

// GetMyProfile returns the authenticated user's private profile.
func (s *UserService) GetMyProfile(ctx context.Context, userID int64) (*dto.UserPrivateResp, error) {
	user, err := s.repo.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, repository.ErrUserNotFound) {
			return nil, errcode.ErrUserNotFound
		}
		return nil, fmt.Errorf("find user: %w", err)
	}
	plain, decErr := crypto.DecryptPhone(user.PhoneEncrypted, s.encCfg.PhoneKey)
	if decErr != nil {
		// Decryption failure is non-fatal for the profile read — log and
		// return a safe placeholder rather than a 500. This can only happen
		// if the key was rotated without re-running the migration script.
		zap.L().Warn("decrypt phone failed on profile read",
			zap.Int64("user_id", userID),
			zap.Int("errcode", errcode.ErrDecryptPhone.Code),
			zap.Error(decErr),
		)
		plain = ""
	}
	resp := dto.UserPrivateResp{
		UserPublicResp: toPublicResp(user),
		PhoneMasked:    crypto.MaskPhone(plain),
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
// [Step 9] avatar_url is now whitelisted against cfg.OSS.URLPrefix via
// oss.IsAllowedURL. Empty string is allowed (means "clear avatar").
// Non-empty URLs that don't begin with our OSS prefix are rejected with
// errcode.ErrUploadURLNotAllowed (460105) — protects against malicious
// clients pointing avatars at hostile third-party hosts (tracking pixels,
// malware, etc.) even if they somehow bypass the upload flow.
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
		avatarURL := strings.TrimSpace(*req.AvatarURL)
		if !oss.IsAllowedURL(avatarURL, s.ossCfg.URLPrefix) {
			return errcode.ErrUploadURLNotAllowed
		}
		updates["avatar_url"] = avatarURL
	}

	if len(updates) == 0 {
		return nil
	}

	if err := s.repo.UpdateProfile(ctx, userID, updates); err != nil {
		return fmt.Errorf("update profile: %w", err)
	}

	if err := s.userCache.DeleteUserInfo(ctx, userID); err != nil {
		zap.L().Warn("invalidate user cache failed",
			zap.Int64("user_id", userID),
			zap.Error(err),
		)
	}
	return nil
}

// VerifyAccessToken is called by the Auth middleware on every protected
// request.
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

// hashPhone delegates to crypto.HashPhone so the pepper from encCfg is applied.
// When PhonePepper is empty (dev mode), result equals plain SHA256(phone) —
// backward compatible with phone_hash rows written in Step 3–10.
func (s *UserService) hashPhone(phone string) string {
	return crypto.HashPhone(phone, s.encCfg.PhonePepper)
}

func hashIP(ip string) string {
	sum := sha256.Sum256([]byte(ip))
	return hex.EncodeToString(sum[:])
}

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

func defaultNickname(phone string) string {
	if len(phone) < 4 {
		return "厨友"
	}
	return "厨友" + phone[len(phone)-4:]
}

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
