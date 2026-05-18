// Package service — search_service.go orchestrates full-text content search:
// sanitise the keyword, run the FULLTEXT query, assemble author snapshots,
// and translate between the opaque wire cursor and the internal offset.
//
// Service rules here are consistent with user_service.go / post_service.go:
//  1. Handlers stay thin: parse → call Search → respond.
//  2. Business invariants (keyword rules, offset math, page-size clamping)
//     live in this layer, not the handler and not the repository.
//  3. Repository does exactly one SQL statement; no caching, no events.
//  4. Search results are NOT cached: search keywords are highly dispersed,
//     so a result cache would carry a poor hit-rate for its memory cost.
//     Same reasoning as ListByUser's "not cached" decision.
//
// ── Keyword handling (PRD §7 F-S01 AC-2 / AC-7) ────────────────────────────
//
//   - AC-2: empty or whitespace-only keyword → ErrSearchKeywordEmpty,
//     the search is NOT executed.
//   - AC-7: keywords are clamped to maxKeywordLen runes (truncated, NOT
//     rejected). We count runes, not bytes — a 50-character limit must mean
//     50 CJK characters, not 50 bytes (~16 Chinese characters).
//
// ── BOOLEAN MODE sanitisation ──────────────────────────────────────────────
//
// MySQL FULLTEXT BOOLEAN MODE treats + - > < ( ) ~ * " @ as operators.
// A raw user keyword like "C++" or "a-b" would be parsed as operators and
// either error or return nonsense. sanitiseBoolean strips these operator
// characters so the keyword is matched literally. This is the conservative
// MVP choice: we lose power-user boolean syntax but gain predictable, safe
// behaviour for the 99% case (plain keyword search).
//
// ── Pagination ─────────────────────────────────────────────────────────────
//
// The wire cursor is an opaque decimal string carrying the OFFSET. First
// page = "" (offset 0). We fetch size+1 rows: if we get more than `size`
// back, there is a next page and next_cursor = offset+size. This mirrors
// post_service.go's has_more detection without a separate COUNT query.
// See search_repository.go's header for why offset (not keyset) here.
//
// Added in Step 7 (search module).
package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"cooking-platform/internal/model/dto"
	"cooking-platform/internal/repository"
	"cooking-platform/pkg/config"
	"cooking-platform/pkg/errcode"

	"go.uber.org/zap"
)

// SearchService is the business orchestrator for the search module.
//
// maxKeywordLen / booleanOperators were promoted from package consts to
// cfg.Search in Step 18 (SEARCH-01, Config-First). Stored on the struct so
// helper functions stay pure (rune counting / Map filtering close over the
// config value via method receivers).
type SearchService struct {
	searchRepo       repository.SearchRepository
	assembler        *AuthorAssembler
	maxKeywordLen    int
	booleanOperators string
}

// NewSearchService wires the service with its dependencies.
//
// It depends on the shared AuthorAssembler (not userRepo directly) so the
// author-snapshot logic is identical to the feed path — single source of
// truth, see author_assembler.go.
func NewSearchService(
	searchRepo repository.SearchRepository,
	assembler *AuthorAssembler,
	cfg config.SearchConfig,
) *SearchService {
	return &SearchService{
		searchRepo:       searchRepo,
		assembler:        assembler,
		maxKeywordLen:    cfg.MaxKeywordLen,
		booleanOperators: cfg.BooleanOperators,
	}
}

// Search runs a full-text search and returns one page of results.
//
// Flow:
//  1. Normalise + validate the keyword (AC-2 reject empty, AC-7 clamp length).
//  2. Sanitise it for BOOLEAN MODE (strip operator characters).
//  3. Decode the opaque cursor into an offset.
//  4. Query size+1 rows from the FULLTEXT repository.
//  5. Assemble author snapshots and the next cursor.
func (s *SearchService) Search(ctx context.Context, q dto.SearchQuery) (*dto.SearchResp, error) {
	// 1. Keyword normalisation & validation.
	keyword := s.normaliseKeyword(q.Keyword)
	if keyword == "" {
		// AC-2: empty / whitespace-only — do not execute the search.
		return nil, errcode.ErrSearchKeywordEmpty
	}

	// 2. BOOLEAN MODE sanitisation. If stripping operators leaves nothing
	//    (e.g. the user typed only "+++"), treat it as an empty keyword —
	//    there is no literal term left to match.
	safe := s.sanitiseBoolean(keyword)
	if safe == "" {
		return nil, errcode.ErrSearchKeywordEmpty
	}

	// 3. Cursor → offset.
	offset, err := parseSearchCursor(q.Cursor)
	if err != nil {
		return nil, errcode.ErrSearchCursorInvalid
	}

	// 4. Page sizing. Reuse the feed's [1,50]-with-default-20 policy, then
	//    fetch one extra row to detect has_more without a COUNT query.
	size := normaliseSize(q.Size)
	rows, err := s.searchRepo.SearchByTitle(ctx, safe, q.SceneTag, offset, size+1)
	if err != nil {
		return nil, fmt.Errorf("search by title: %w", err)
	}

	// 5. has_more detection: more than `size` rows came back → there is a
	//    next page. Trim the probe row before assembling the response.
	hasMore := len(rows) > size
	if hasMore {
		rows = rows[:size]
	}

	authorMap, err := s.assembler.LoadMap(ctx, rows)
	if err != nil {
		return nil, err
	}

	resp := &dto.SearchResp{
		Posts:      BuildListItems(rows, authorMap),
		NextCursor: nextSearchCursor(offset, size, hasMore),
		HasMore:    hasMore,
	}

	zap.L().Debug("search executed",
		zap.String("keyword", safe),
		zap.Int8("scene_tag", q.SceneTag),
		zap.Int("offset", offset),
		zap.Int("results", len(rows)),
		zap.Bool("has_more", hasMore),
	)
	return resp, nil
}

// ── Private helpers ─────────────────────────────────────────────────────────

// normaliseKeyword trims surrounding whitespace and clamps the keyword to
// s.maxKeywordLen RUNES (AC-7 — truncate, do not reject). Rune-based slicing
// avoids cutting a multi-byte CJK character in half.
func (s *SearchService) normaliseKeyword(raw string) string {
	trimmed := strings.TrimSpace(raw)
	runes := []rune(trimmed)
	if len(runes) > s.maxKeywordLen {
		runes = runes[:s.maxKeywordLen]
	}
	return string(runes)
}

// sanitiseBoolean removes MySQL FULLTEXT BOOLEAN MODE operator characters so
// the keyword is matched as a literal term. Inner whitespace is collapsed so
// "  red   pork " becomes "red pork" (two terms, both required by AGAINST's
// default behaviour without operators). See file header for why we strip
// rather than support boolean syntax.
func (s *SearchService) sanitiseBoolean(keyword string) string {
	cleaned := strings.Map(func(r rune) rune {
		if strings.ContainsRune(s.booleanOperators, r) {
			return -1 // drop the operator character
		}
		return r
	}, keyword)
	// Collapse any run of whitespace to single spaces, drop leading/trailing.
	return strings.Join(strings.Fields(cleaned), " ")
}

// parseSearchCursor decodes the opaque wire cursor into an OFFSET.
//
//	""      → 0 (first page)
//	"<n>"   → n, where n is a non-negative decimal integer
//	other   → error (handler maps to ErrSearchCursorInvalid)
//
// Negative offsets are rejected — they have no meaning and would surface as
// a MySQL error deeper down. The cursor is deliberately opaque to clients
// (see search_repository.go header): they pass back whatever next_cursor
// gave them, nothing more.
func parseSearchCursor(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("search cursor must be non-negative, got %d", n)
	}
	return n, nil
}

// nextSearchCursor builds the next page's opaque cursor, or "" when this is
// the last page. The next offset is simply current offset + page size.
func nextSearchCursor(offset, size int, hasMore bool) string {
	if !hasMore {
		return ""
	}
	return strconv.Itoa(offset + size)
}
