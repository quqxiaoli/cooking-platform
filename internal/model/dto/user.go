// Package dto — user.go defines request and response DTOs for the user module.
//
// DTOs are the public contract between handler and service layers. They are
// also the JSON wire format for HTTP requests/responses. Keep them stable:
// breaking changes here break clients.
//
// Validation tags use go-playground/validator. The "phone" tag is registered
// in pkg/validator at startup.
//
// Added in Step 3 (user module).
package dto

// ── Auth requests ───────────────────────────────────────────────────────────

// SendCodeReq is the body of POST /api/v1/auth/send-code.
type SendCodeReq struct {
	Phone string `json:"phone" binding:"required,phone"`
}

// SendCodeResp is the response payload after a code is dispatched.
// ExpiresIn is communicated to the client so it can show a countdown.
type SendCodeResp struct {
	ExpiresIn int `json:"expires_in"` // seconds
}

// LoginReq is the body of POST /api/v1/auth/login.
//
// First-time login auto-registers the user — there is no separate "register"
// endpoint (PRD-Phase2: phone+code is the only authentication method).
type LoginReq struct {
	Phone string `json:"phone" binding:"required,phone"`
	Code  string `json:"code" binding:"required,len=6,numeric"`
}

// RefreshReq is the body of POST /api/v1/auth/refresh.
//
// Refresh tokens travel in the body (not Authorization header) because they
// are NOT bearer tokens — they only authorise issuing a new access token,
// not arbitrary API calls.
type RefreshReq struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

// ── Auth responses ──────────────────────────────────────────────────────────

// TokenPair carries both tokens after successful login or refresh.
//
// AccessTokenExpiresAt is provided as a UnixMilli timestamp so clients can
// pre-emptively refresh before expiry without parsing JWT internals.
type TokenPair struct {
	AccessToken          string `json:"access_token"`
	RefreshToken         string `json:"refresh_token"`
	AccessTokenExpiresAt int64  `json:"access_token_expires_at"` // UnixMilli
	TokenType            string `json:"token_type"`              // always "Bearer"
}

// LoginResp wraps the token pair plus a snapshot of the user profile so the
// client can render the home screen without an extra round-trip to /users/me.
type LoginResp struct {
	TokenPair
	User UserPublicResp `json:"user"`
}

// ── User profile ────────────────────────────────────────────────────────────

// UserPublicResp is the public view of a user — no phone number, no email,
// no anything PII. Returned by GET /api/v1/users/:id and embedded in feed/post
// responses (Step 4+).
//
// IsFollowing reflects whether the authenticated caller (if any) currently
// follows this user. Anonymous callers and self-views always see false.
type UserPublicResp struct {
	ID             int64  `json:"id"`
	Nickname       string `json:"nickname"`
	AvatarURL      string `json:"avatar_url"`
	Bio            string `json:"bio"`
	PostCount      uint32 `json:"post_count"`
	TotalLikes     uint32 `json:"total_likes"`
	FollowerCount  uint32 `json:"follower_count"`
	FollowingCount uint32 `json:"following_count"`
	CreatedAt      int64  `json:"created_at"`   // UnixMilli
	IsFollowing    bool   `json:"is_following"` // viewer follows this user
}

// UserPrivateResp is the self-view: includes the masked phone number.
// Returned only by GET /api/v1/users/me.
//
// PhoneMasked example: "138****8000". The full phone is never exposed even
// to the user themselves over HTTP — they already know their own number.
type UserPrivateResp struct {
	UserPublicResp
	PhoneMasked string `json:"phone_masked"`
}

// UpdateProfileReq is the body of PATCH /api/v1/users/me.
//
// All fields are optional pointers — nil means "do not change". Empty string
// means "clear the field". This distinguishes "I want to remove my bio" from
// "I'm only updating my nickname".
//
// AvatarURL is NOT validated as a URL here: the value is produced by our own
// OSS upload flow (Step 9) and the service layer applies whitelist checks.
// Front-end-supplied arbitrary URLs are rejected by service-layer validation,
// not by the binding tag.
type UpdateProfileReq struct {
	Nickname  *string `json:"nickname,omitempty" binding:"omitempty,min=1,max=50"`
	AvatarURL *string `json:"avatar_url,omitempty" binding:"omitempty,max=500"`
	Bio       *string `json:"bio,omitempty" binding:"omitempty,max=200"`
}
